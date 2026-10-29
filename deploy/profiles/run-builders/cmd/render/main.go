// Command render prints the node-profile installer manifests.
//
//	go run ./deploy/profiles/run-builders/cmd/render [--uninstall] [--image REF@sha256:DIGEST]
package main

import (
	"flag"
	"fmt"
	"os"

	runbuilders "github.com/optimiware/kodama/deploy/profiles/run-builders"
)

func main() {
	uninstall := flag.Bool("uninstall", false, "render the uninstall DaemonSet instead of the installer")
	image := flag.String("image", "", "digest-pinned installer image (default: the pinned Ubuntu 24.04 index)")
	flag.Parse()
	options := runbuilders.Options{Image: *image}
	render := runbuilders.Render
	if *uninstall {
		render = runbuilders.RenderUninstall
	}
	out, err := render(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
}
