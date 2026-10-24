// Command repoctl runs the repository checks that do not need a container:
// formatting, pinned configuration, documentation links and the scoped Go
// gate that writes FORGE_RESULTS.
package main

import (
	"fmt"
	"os"

	"github.com/optimiware/kodama/dev/internal/devenv"
	"github.com/optimiware/kodama/dev/internal/repochk"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: repoctl <fmt|config|docs|go> [arguments]")
		os.Exit(2)
	}
	root := devenv.Root()
	var err error
	switch os.Args[1] {
	case "fmt":
		err = repochk.Fmt(root, os.Args[2:])
	case "config":
		err = repochk.Config(root)
	case "docs":
		err = repochk.Docs(root)
	case "go":
		os.Exit(repochk.GoCheck(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
