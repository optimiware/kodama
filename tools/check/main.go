package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/archtest"
)

func main() {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	findings, err := archtest.Run(*root, flags.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(findings) > 0 {
		for _, finding := range findings {
			fmt.Println(finding.String())
		}
		os.Exit(1)
	}
}
