// Command stepbin is the actual `kodama step` entrypoint (internal/entrypoint)
// built as a static binary for the T-031 acceptance composition. The production
// binary registers the same command through internal/app (T-027).
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
	os.Exit(entrypoint.Run(context.Background(), os.Args[2:], os.Getenv, os.Stdout, os.Stderr))
}
