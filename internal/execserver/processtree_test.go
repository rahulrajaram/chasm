package execserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRunScriptDeadlineReclaimsPipesAndLock pins the process-tree teardown:
// a timed-out script must not leave a backgrounded grandchild holding the
// collector pipes (which blocked cmd.Run() past every deadline) or the
// lifecycle flock it inherited.
func TestRunScriptDeadlineReclaimsPipesAndLock(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock is unavailable on this host")
	}
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "lifecycle.lock")
	script := fmt.Sprintf(`#!/bin/sh
exec 9>%q
flock -x 9
# A grandchild inherits the lock descriptor and both collector pipes.
sleep 300 &
echo ready
# The direct child itself blocks far past the test timeout.
sleep 300
`, lockPath)
	scriptPath := filepath.Join(dir, "stub-launcher.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	timeout := 2 * time.Second
	started := time.Now()
	stdout, _, code, err := runScript([]string{scriptPath}, nil, timeout)
	elapsed := time.Since(started)

	// The deadline must surface as the runScript failure, not as a blocked
	// Run: allow the deadline plus the pipe-collection backstop plus slack.
	if elapsed > timeout+waitDelayBackstop+10*time.Second {
		t.Fatalf("runScript returned after %v; descendants kept it blocked", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || code != -1 {
		t.Fatalf("expected deadline failure, got code %d err %v (stdout %q)", code, err, stdout)
	}
	if string(stdout) != "ready\n" {
		t.Fatalf("stdout %q", stdout)
	}

	// The grandchild held the lock through its inherited descriptor; the
	// group kill must have released it.
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if probeErr := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); probeErr != nil {
		t.Fatalf("lifecycle lock is still held after the deadline kill: %v", probeErr)
	}
}
