// Command summarize writes the client-compatibility summary of an OpenAPI
// document (apispec.Summarize) as the JSON baseline the composition tests commit
// (internal/app/testdata). Regenerate a baseline only when a deliberate,
// versioned break of the public API is accepted.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/app/apispec"
)

func main() {
	in := flag.String("in", "", "OpenAPI document to summarize (default: stdin)")
	out := flag.String("out", "", "file to write (default: stdout)")
	flag.Parse()
	var data []byte
	var err error
	if *in == "" {
		data, err = os.ReadFile("/dev/stdin")
	} else {
		data, err = os.ReadFile(*in)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s, err := apispec.Summarize(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, err := s.JSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b = append(b, '\n')
	if *out == "" {
		_, err = os.Stdout.Write(b)
	} else {
		err = os.WriteFile(*out, b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
