// Command render-config prints the static managed OpenCode configuration that
// the image bakes into /etc/opencode/opencode.json.
package main

import (
	"os"

	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
)

func main() { _, _ = os.Stdout.Write(spec.ManagedConfig()) }
