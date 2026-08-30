//go:build !unix

package browser

import (
	"os"
	"os/exec"
)

// setProcessGroup is a no-op on platforms without POSIX process groups.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree terminates the Chromium process.
func killProcessTree(proc *os.Process) {
	if proc == nil {
		return
	}
	_ = proc.Kill()
}
