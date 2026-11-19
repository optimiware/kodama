package clientcmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestVersionNeedsNoConfiguration proves `kodema version` and `kodema
// --version`, reached through the client flags, print the build identity
// without reading a configuration file or reaching the CLI commands.
func TestVersionNeedsNoConfiguration(t *testing.T) {
	isolateUserConfig(t)
	const want = "kodema 1.2.3\n"

	malformed := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(malformed, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}

	for _, args := range [][]string{
		{"version"},
		{"--version"},
		{"--config", malformed, "version"},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			deps := cliDeps{
				runCLI: func(context.Context, cli.Config, []string, cli.Ports, cli.CommandDeps) int {
					t.Error("version reached the CLI commands")
					return 0
				},
				printVersion: func(out io.Writer) { io.WriteString(out, want) },
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), args, &stdout, &stderr, deps); code != 0 {
				t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
			}
			if stdout.String() != want {
				t.Fatalf("stdout = %q; want %q", stdout.String(), want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q; want it empty", stderr.String())
			}
		})
	}
}
