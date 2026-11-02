//go:build unix

package gitservice

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command and every process it starts (git
// receive-pack, its hooks, index-pack) in a new process group, so the whole tree
// can be terminated at a receive deadline.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGKILL to the command's process group. A group that
// is already gone is not an error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return cmd.Process.Kill()
	}
	return nil
}

// processGroupAlive reports whether any process of the group still exists.
func processGroupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
