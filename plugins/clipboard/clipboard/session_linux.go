package clipboard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func ioctl(file *os.File, request uintptr, value unsafe.Pointer) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	err = conn.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(value))
		if errno != 0 {
			ioctlErr = errno
		}
	})
	if err != nil {
		return err
	}
	return ioctlErr
}

func termState(file *os.File) (syscall.Termios, error) {
	var state syscall.Termios
	err := ioctl(file, syscall.TCGETS, unsafe.Pointer(&state))
	return state, err
}

// IsTerminal tests the descriptor without reading terminal input.
func IsTerminal(file *os.File) bool {
	_, err := termState(file)
	return err == nil
}

func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock, number uint32
	if err = ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err == nil {
		err = ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&number))
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(number), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func copySize(from, to *os.File) {
	var size [4]uint16
	if ioctl(from, syscall.TIOCGWINSZ, unsafe.Pointer(&size)) == nil {
		_ = ioctl(to, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
	}
}

// Session runs one command behind a PTY, preserving terminal size, raw input,
// job control, and the command's exit code. It is intended for a CLI's final
// operation: the input copier can remain blocked until that CLI exits.
func Session(path string, args, environment []string, stdin, stdout, stderr *os.File, copyText CopyText) (int, error) {
	state, err := termState(stdin)
	if err != nil {
		return 1, fmt.Errorf("clipboard bridge needs a terminal: %w", err)
	}
	master, slave, err := openPTY()
	if err != nil {
		return 1, err
	}
	defer master.Close()
	defer slave.Close()
	if err := ioctl(slave, syscall.TCSETS, unsafe.Pointer(&state)); err != nil {
		return 1, err
	}
	copySize(stdout, slave)
	cmd := exec.Command(path, args...)
	cmd.Env = environment
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	// Cursor visibility is terminal-emulator state, not part of termios. A
	// child TUI can exit (or be killed) while it is hidden. Restore it after
	// draining child output so its final bytes cannot hide the host prompt.
	defer func() { _, _ = io.WriteString(stdout, "\x1b[?25h") }()
	slave.Close()

	raw := state
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := ioctl(stdin, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 1, err
	}
	defer ioctl(stdin, syscall.TCSETS, unsafe.Pointer(&state))

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGWINCH, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	done := make(chan struct{})
	var signalWorker sync.WaitGroup
	signalWorker.Add(1)
	go func() {
		defer signalWorker.Done()
		for {
			select {
			case <-done:
				return
			case sig := <-signals:
				if sig == syscall.SIGWINCH {
					copySize(stdout, master)
				} else {
					_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
				}
			}
		}
	}()
	defer func() { close(done); signalWorker.Wait() }()
	go func() { _, _ = io.Copy(master, stdin) }()
	outputDone := make(chan error, 1)
	drainExpired := make(chan struct{})
	go func() {
		warned := false
		outputDone <- Filter(stdout, master, func(selection string, text []byte) error {
			select {
			case <-drainExpired:
				return errors.New("clipboard session ended")
			default:
			}
			err := copyText(selection, text)
			if err != nil && !warned {
				warned = true
				fmt.Fprint(stderr, "\r\nchasm: desktop clipboard write failed; falling back to terminal clipboard support\r\n")
			}
			return err
		})
	}()
	waitErr := cmd.Wait()
	// A descendant may retain the slave after the command exits. Bound draining
	// so such a process cannot strand the desktop terminal in raw mode.
	var outputErr error
	select {
	case outputErr = <-outputDone:
	case <-time.After(3 * time.Second):
		close(drainExpired)
		master.Close()
		outputErr = <-outputDone
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			status := exitErr.Sys().(syscall.WaitStatus)
			if status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return exitErr.ExitCode(), nil
		}
		return 1, waitErr
	}
	// Linux PTYs report EIO when the last slave closes.
	if outputErr != nil && !errors.Is(outputErr, syscall.EIO) && !errors.Is(outputErr, os.ErrClosed) {
		return 1, outputErr
	}
	return 0, nil
}
