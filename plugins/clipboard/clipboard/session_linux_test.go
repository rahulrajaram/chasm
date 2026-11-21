package clipboard

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestSessionCursorOnExit(t *testing.T) {
	for _, test := range []struct {
		name, script string
		code         int
		hidesCursor  bool
	}{
		{"reported-printf", "printf '%s\\n' 'Updated all three skill files.'", 0, false},
		{"application-leaves-cursor-hidden", "printf '\\033[?25l'; printf '%s\\n' 'Updated all three skill files.'", 0, true},
		{"application-terminated", "printf '\\033[?25l'; kill -TERM $$", 143, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, slave, err := openPTY()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionProcess$")
			cmd.Env = append(os.Environ(), "CHASM_CLIPBOARD_TEST_CHILD=1", "CHASM_CLIPBOARD_TEST_SCRIPT="+test.script)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			slave.Close()
			_ = cmd.Wait()
			if ctx.Err() != nil || cmd.ProcessState.ExitCode() != test.code {
				t.Fatalf("exit=%v timeout=%v", cmd.ProcessState, ctx.Err())
			}
			output, _ := io.ReadAll(master)
			if test.hidesCursor {
				hide := bytes.LastIndex(output, []byte("\x1b[?25l"))
				show := bytes.LastIndex(output, []byte("\x1b[?25h"))
				if hide < 0 || show <= hide {
					t.Fatalf("cursor not restored after child exit: %q", output)
				}
			} else if !bytes.Contains(output, []byte("Updated all three skill files.")) || bytes.Contains(output, []byte("\x1b[?25l")) {
				t.Fatalf("printf output changed or hid cursor: %q", output)
			}
		})
	}
}

// Run Session in a subprocess, just as the CLI does, so its terminal input
// goroutine and signal handlers cannot interfere with the Go test runner.
func TestSessionProcess(t *testing.T) {
	if os.Getenv("CHASM_CLIPBOARD_TEST_CHILD") != "1" {
		return
	}
	code, err := Session("/bin/sh", []string{"-c", os.Getenv("CHASM_CLIPBOARD_TEST_SCRIPT")}, os.Environ(), os.Stdin, os.Stdout, os.Stderr,
		func(selection string, text []byte) error {
			return os.WriteFile(os.Getenv("CHASM_CLIPBOARD_TEST_CAPTURE"), append([]byte(selection+":"), text...), 0600)
		})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	os.Exit(code)
}

func TestSessionPasteCopySizeAndExit(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	size := [4]uint16{31, 97, 0, 0}
	if err := ioctl(slave, syscall.TIOCSWINSZ, unsafe.Pointer(&size)); err != nil {
		t.Fatal(err)
	}
	original, err := termState(slave)
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "clipboard")
	paste := "\x1b[200~pasted text\x1b[201~"
	script := fmt.Sprintf("stty size; stty raw -echo; printf '\\033]52;c;aGVsbG8=\\007READY'; dd bs=1 count=%d 2>/dev/null; for i in 1 2 3 4 5 6 7 8 9 10; do dimensions=$(stty size); [ \"$dimensions\" = '45 120' ] && break; sleep 0.05; done; printf '\\nRESIZED:%%s:DONE' \"$dimensions\"; exit 7", len(paste))
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionProcess$")
	cmd.Env = append(os.Environ(), "CHASM_CLIPBOARD_TEST_CHILD=1", "CHASM_CLIPBOARD_TEST_SCRIPT="+script, "CHASM_CLIPBOARD_TEST_CAPTURE="+capture)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	output := make(chan []byte, 1)
	go func() {
		var got bytes.Buffer
		buffer := make([]byte, 1024)
		sent := false
		for {
			n, err := master.Read(buffer)
			got.Write(buffer[:n])
			if !sent && strings.Contains(got.String(), "READY") {
				sent = true
				resized := [4]uint16{45, 120, 0, 0}
				_ = ioctl(master, syscall.TIOCSWINSZ, unsafe.Pointer(&resized))
				_ = cmd.Process.Signal(syscall.SIGWINCH)
				_, _ = io.WriteString(master, paste)
			}
			if sent && strings.Contains(got.String(), ":DONE") {
				output <- got.Bytes()
				return
			}
			if err != nil {
				output <- got.Bytes()
				return
			}
		}
	}()
	select {
	case err := <-finished:
		if err == nil || cmd.ProcessState.ExitCode() != 7 {
			t.Fatalf("exit=%v state=%v", err, cmd.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not exit")
	}
	select {
	case got := <-output:
		if !bytes.Contains(got, []byte("31 97")) || !bytes.Contains(got, []byte("RESIZED:45 120:DONE")) || !bytes.Contains(got, []byte(paste)) || bytes.Contains(got, []byte("]52;")) {
			t.Fatalf("output=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("output not delivered")
	}
	data, err := os.ReadFile(capture)
	if err != nil || string(data) != "c:hello" {
		t.Fatalf("clipboard=%q err=%v", data, err)
	}
	restored, err := termState(slave)
	if err != nil || restored != original {
		t.Fatalf("terminal was not restored: %v", err)
	}
}
