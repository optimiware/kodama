package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUserlandAppImportGraphIsPublicOnly proves that the Git, agent and
// service app packages use only neutral packages, the public API/client
// contracts and other Userland packages. Even when linked into the same
// `kodema` binary as the Shell composition, they never import Shell, Kernel or
// installation code.
func TestUserlandAppImportGraphIsPublicOnly(t *testing.T) {
	root := repositoryRoot(t)
	command := exec.Command("go", "list", "-deps",
		"./internal/userland/app/auth",
		"./internal/userland/git/app",
		"./internal/userland/agent/app",
		"./internal/userland/services/app",
	)
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, output)
	}
	forbidden := []string{
		"github.com/optimiware/kodema/internal/shell",
		"github.com/optimiware/kodema/internal/kernel",
		"github.com/optimiware/kodema/internal/installation",
	}
	found := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		found[line] = true
		for _, prefix := range forbidden {
			if line == prefix || strings.HasPrefix(line, prefix+"/") {
				t.Errorf("userland app import graph contains forbidden package %q", line)
			}
		}
	}
	for _, expected := range []string{
		"github.com/optimiware/kodema/internal/userland/app/auth",
		"github.com/optimiware/kodema/internal/userland/git/app",
		"github.com/optimiware/kodema/internal/userland/agent/app",
		"github.com/optimiware/kodema/internal/userland/services/app",
		"github.com/optimiware/kodema/client",
	} {
		if !found[expected] {
			t.Errorf("go list did not report expected package %q", expected)
		}
	}
}

// TestKodemaProductDoesNotImportTestSupport proves the product binary,
// including the durable test profile, never links a test harness package:
// the test entry is composed from product adapters only.
func TestKodemaProductDoesNotImportTestSupport(t *testing.T) {
	root := repositoryRoot(t)
	command := exec.Command("go", "list", "-deps", "./cmd/kodema")
	command.Dir = root
	command.Env = os.Environ()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, output)
	}
	found := false
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line == "github.com/optimiware/kodema/test" || strings.HasPrefix(line, "github.com/optimiware/kodema/test/") {
			t.Errorf("kodema imports test package %q", line)
		}
		if line == "github.com/optimiware/kodema/internal/shell/app/drivers/podman" {
			found = true
		}
	}
	if !found {
		t.Error("go list did not report the Podman driver the test profile composes")
	}
}

// repositoryRoot walks up from the test working directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("could not find the repository root")
		}
		directory = parent
	}
}
