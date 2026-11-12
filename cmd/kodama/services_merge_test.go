package main

// The `kodama services --kind merge_coordinator` entry shape.
//
// A merge coordinator needs its durable state directory, the Git base URL and
// the mounted service credential. Every service takes the credential
// flag; the state directory and Git URL are merge-specific.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServicesMergeCoordinatorRequiresStateAndGitURL pins the merge-coordinator
// flag contract: the adapter cannot start without durable state and a Git URL.
func TestServicesMergeCoordinatorRequiresStateAndGitURL(t *testing.T) {
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("token"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	base := []string{
		"--kind", "merge_coordinator",
		"--service", "merge-svc",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--base-url", "http://127.0.0.1:1",
		"--credential-file", credential,
	}
	cases := map[string][]string{
		"no state directory": append(append([]string(nil), base...), "--git-url", "file:///srv/git"),
		"no Git URL":         append(append([]string(nil), base...), "--state-dir", t.TempDir()),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr lockedBuffer
			code := runServicesEntry(context.Background(), args, &lockedBuffer{}, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "merge coordinator requires --state-dir and --git-url") {
				t.Fatalf("stderr = %q; want the merge coordinator requirement", stderr.String())
			}
		})
	}
}
