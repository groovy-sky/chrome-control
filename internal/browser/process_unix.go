//go:build unix

package browser

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts Chromium into its own process group so that the whole
// process tree can be signalled at once, and makes the kernel kill it if the
// worker dies unexpectedly.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = new(syscall.SysProcAttr)
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// killProcessTree terminates the entire Chromium process tree.
func killProcessTree(proc *os.Process) {
	if proc == nil || proc.Pid <= 0 {
		return
	}
	pgid, err := syscall.Getpgid(proc.Pid)
	if err != nil || pgid <= 0 {
		pgid = proc.Pid
	}
	// Give the group a brief chance to exit cleanly, then force-kill it.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = proc.Kill()
}
