package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestKodactlVersionNeedsNoConfiguration proves `kodactl version` and
// `kodactl --version` print the linked version and revision without reading a
// configuration file or reaching the CLI commands.
func TestKodactlVersionNeedsNoConfiguration(t *testing.T) {
	isolateUserConfig(t)
	previousVersion, previousRevision := version, revision
	t.Cleanup(func() { version, revision = previousVersion, previousRevision })
	version, revision = "1.2.3", "0123456789abcdef0123456789abcdef01234567"
	want := "kodactl 1.2.3 (revision 0123456789abcdef0123456789abcdef01234567, " +
		runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"

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
			deps := cliDeps{runCLI: func(context.Context, cli.Config, []string, cli.Ports, cli.CommandDeps) int {
				t.Error("version reached the CLI commands")
				return 0
			}}
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
