//go:build unix

package git

import (
	"os/exec"
	"syscall"
)

// isolate puts the command in its own process group and kills the whole group
// when its context ends, so helpers it spawned cannot outlive the deadline.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
