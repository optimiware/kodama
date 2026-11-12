// Package script generates the fail-fast entrypoint script required by S07 §2
// and KODAMA.md "Container actions".
//
// The adapter replaces the image entrypoint with the rendered script. Commands
// are opaque bytes: the renderer never trims, splits, escapes, deduplicates or
// reorders them, adds no tracing and injects no Kodama runner. It also never
// starts a shell or a process; real exit behavior belongs to the runtime
// adapters.
package script

import (
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// ShellPath is the shell used to run the fail-fast entrypoint script.
const ShellPath = "/bin/sh"

// preamble is the exact fail-fast header: the declared shell plus errexit.
const preamble = "#!" + ShellPath + "\nset -e\n"

// Render returns the fail-fast entrypoint script for the declared commands.
//
// The result is the preamble followed by every command joined with a single
// newline and terminated by one final newline. An empty command list or a
// command containing a NUL byte is refused with invalid_input.
func Render(commands []string) (string, error) {
	if len(commands) == 0 {
		return "", fault.New(fault.InvalidInput, "at least one command is required")
	}
	for index, command := range commands {
		if strings.ContainsRune(command, 0) {
			return "", fault.New(fault.InvalidInput, fmt.Sprintf("command %d contains a NUL byte", index))
		}
	}
	return preamble + strings.Join(commands, "\n") + "\n", nil
}
