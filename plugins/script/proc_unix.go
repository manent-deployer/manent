//go:build unix

package script

import (
	"os/exec"
	"syscall"
)

// processGroup starts the command in its own process group and, on cancel, ends the whole
// group with SIGTERM (then SIGKILL after WaitDelay).
func processGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
}

// endGroup kills what is left of the group once the script has ended: a process left in the
// background must not outlive the run on the worker.
func endGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
