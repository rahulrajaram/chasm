package guestexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func localWrapper(t *testing.T, argv ...string) (*exec.Cmd, string) {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(nonce[:])
	marker := "/tmp/.chasm-exec-" + token
	t.Cleanup(func() { os.Remove(marker) })
	client := &Client{grace: 200 * time.Millisecond}
	args := client.wrappedArgv(token, argv)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, args[0], args[1:]...), marker
}

func TestWrapperStatusesAndStdin(t *testing.T) {
	for _, status := range []int{0, 7, 129, 143} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			cmd, marker := localWrapper(t, "/bin/bash", "-c", fmt.Sprintf("cat; exit %d", status))
			cmd.Stdin = strings.NewReader("hello\x00world\n")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			_ = cmd.Run()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != status {
				t.Fatalf("state=%v stderr=%s", cmd.ProcessState, &stderr)
			}
			if stdout.String() != "hello\x00world\n" {
				t.Fatalf("stdin changed: %q", stdout.String())
			}
			info, err := os.Lstat(marker)
			if status == 129 {
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("marker=%v %v", info, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("unexpected marker: %v", err)
			}
		})
	}
}

func TestWrapperForwardsAndEscalates(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		status       int
	}{
		{"TERM", `trap 'exit 7' TERM; echo $$; while :; do /bin/sleep 1; done`, 7},
		{"KILL", `trap '' TERM; echo $$; exec /bin/sleep 30`, 137},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _ := localWrapper(t, "/bin/bash", "-c", tc.script)
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if cmd.ProcessState.ExitCode() != tc.status {
				t.Fatalf("state=%v stderr=%s", cmd.ProcessState, &stderr)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("child %d still exists: %v", pid, err)
			}
		})
	}
}

func TestWrapperDoesNotOverwriteMarker(t *testing.T) {
	cmd, marker := localWrapper(t, "/bin/bash", "-c", "exit 129")
	target := t.TempDir() + "/target"
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, marker); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Run()
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("marker overwrote target: %q %v", data, err)
	}
}

func TestWrapperKillsRemainingProcessGroup(t *testing.T) {
	// The direct child exits on TERM while its descendant ignores TERM.
	// The wrapper must retain its watchdog and kill that remaining group.
	cmd, _ := localWrapper(t, "/bin/bash", "-c", `trap 'exit 7' TERM; /bin/bash -c 'trap "" TERM; echo $$; exec /bin/sleep 30' & wait`)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() != 7 {
		t.Fatalf("state=%v", cmd.ProcessState)
	}
	// An orphan may remain a zombie until the container's init reaps it.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil && !strings.HasPrefix(string(data)[strings.LastIndex(string(data), ")")+1:], " Z ") {
		t.Fatalf("descendant survived: %s", data)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestWrapperLookupErrors(t *testing.T) {
	directory := t.TempDir()
	path := directory + "/not-executable"
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path    string
		status  int
		message string
	}{
		{path, 126, "Permission denied"}, {directory + "/missing", 127, "No such file or directory"},
	} {
		cmd, _ := localWrapper(t, tc.path)
		output, _ := cmd.CombinedOutput()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.status || !strings.Contains(string(output), tc.message) {
			t.Fatalf("state=%v output=%q", cmd.ProcessState, output)
		}
	}
}

func TestMarkerReadbackRemovesMarker(t *testing.T) {
	cmd, marker := localWrapper(t, "/bin/bash", "-c", "exit 129")
	_ = cmd.Run()
	token := strings.TrimPrefix(marker, "/tmp/.chasm-exec-")
	for _, want := range []int{0, 1} {
		readback := exec.Command("/bin/bash", "--noprofile", "--norc", "-p", "-c", markerReadback, "chasm-status", token)
		_ = readback.Run()
		if readback.ProcessState.ExitCode() != want {
			t.Fatalf("readback: %v", readback.ProcessState)
		}
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker not removed: %v", err)
	}
}

func TestWrapperPreservesInterruptDispositions(t *testing.T) {
	for _, tc := range []struct {
		signal syscall.Signal
		status int
	}{{syscall.SIGINT, 130}, {syscall.SIGQUIT, 131}} {
		t.Run(tc.signal.String(), func(t *testing.T) {
			cmd, _ := localWrapper(t, "/bin/bash", "-c", "ulimit -c 0; echo $$; exec /bin/sleep 30")
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			line, err := bufio.NewReader(output).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(pid, syscall.SIGKILL)
			// Wait for exec: Bash itself ignores QUIT, whereas an external command
			// must inherit the normal disposition.
			deadline := time.Now().Add(time.Second)
			for {
				data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
				if err == nil && strings.TrimSpace(string(data)) == "sleep" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child did not exec sleep")
				}
				time.Sleep(time.Millisecond)
			}
			if err := syscall.Kill(pid, tc.signal); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.status {
				t.Fatalf("signal ignored: %v", cmd.ProcessState)
			}
		})
	}
}
