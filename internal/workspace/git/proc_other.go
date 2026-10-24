//go:build !unix

package git

import "os/exec"

func isolate(cmd *exec.Cmd) {}
