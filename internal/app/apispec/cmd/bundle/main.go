// Command bundle writes the aggregate OpenAPI document of the core slice.
//
//	go run ./internal/app/apispec/cmd/bundle -api api -out api/openapi.yaml
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/app/apispec"
)

func main() {
	dir := flag.String("api", "../../api", "directory of the module fragments")
	out := flag.String("out", "../../api/openapi.yaml", "aggregate document to write")
	flag.Parse()
	data, err := apispec.Bundle(*dir, apispec.CoreFragments, apispec.CoreInfo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bundle:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "bundle:", err)
		os.Exit(1)
	}
}
