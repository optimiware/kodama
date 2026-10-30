// Command hookmain is the smallest binary that serves `git-hook <name>`: what
// the `kodama` binary's `git-hook` command does (T-132 registers that command).
// The package's tests build it as the hook binary, because a freshly built,
// race-instrumented test binary starts slowly on some systems.
package main

import (
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/gitservice"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "git-hook" {
		fmt.Fprintln(os.Stderr, "usage: hookmain git-hook <pre-receive|reference-transaction|post-receive> [args...]")
		os.Exit(2)
	}
	os.Exit(gitservice.HookMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Environ()))
}
