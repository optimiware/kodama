// Command stepbin is the actual entrypoint (`kodama step`, including the T-078
// container-log mirror) built as a standalone static binary for the T-064
// cluster fixtures. The production binary registers the same command through
// internal/app (T-027).
//
// One fixture adaptation, recorded as blocker B-4 in test/integration/logs/README.md:
// the entrypoint chmods its secrets directory, which fails for a non-root user
// on the Pod's memory-backed volume root (owned by root, group-writable through
// fsGroup). The fixture therefore points the entrypoint at a subdirectory of
// that volume with its documented flag; everything else is untouched.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/entrypoint"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != entrypoint.CommandName {
		fmt.Fprintln(os.Stderr, "usage: kodama step [flags]")
		os.Exit(2)
	}
	args := append([]string(nil), os.Args[2:]...)
	for i, a := range args {
		if a == "--secrets-dir=/kodama/secrets" {
			args[i] = "--secrets-dir=/kodama/secrets/t064"
		}
	}
	os.Exit(entrypoint.Run(context.Background(), args, os.Getenv, os.Stdout, os.Stderr))
}
