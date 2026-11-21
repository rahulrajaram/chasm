package execserver

import (
	"os/exec"
	"syscall"
	"time"
)

// waitDelayBackstop bounds how long Run keeps collecting inherited output
// pipes after the deadline kill. The group kill closes those pipes for every
// descendant that stayed in the group; the backstop only matters for a
// descendant that deliberately escaped (setsid) and still holds a write end.
const waitDelayBackstop = 30 * time.Second

// ownProcessGroup makes cmd the leader of its own process group so a deadline
// can tear down the whole tree, not just the direct child.
//
// Context cancellation on a plain CommandContext kills only the direct child.
// The launcher and workflow scripts start unbounded guests-side waits (incus
// exec, cloud-init status --wait) whose descendants inherit the collector
// pipes and the lifecycle flock descriptor: an orphaned grandchild kept
// cmd.Run() blocked forever past the deadline and held the flock across
// executor restarts. With Setpgid the child leads a fresh group; Cancel kills
// the negative process group, and WaitDelay stops Run from waiting on pipes
// a survivor may still hold.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelayBackstop
}
