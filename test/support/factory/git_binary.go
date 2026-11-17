package factory

// Hook binary construction for the host Git composition.
//
// The installed native hooks exec an absolute `kodema` binary. The Go test
// binary is not that binary, so the fixture builds the real `./cmd/kodema` from
// this checkout into a fixture-private directory and passes its absolute path
// as gitapp.Config.HookBinary.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// buildHookBinary builds the real kodema command from the assigned checkout.
func buildHookBinary(ctx context.Context, directory string) (string, error) {
	if strings.TrimSpace(directory) == "" {
		return "", fmt.Errorf("hook binary directory is empty")
	}
	if err := os.MkdirAll(directory, gitRepositoryDirectory); err != nil {
		return "", fmt.Errorf("create hook binary directory: %w", err)
	}
	root, err := findModuleRoot()
	if err != nil {
		return "", err
	}
	binary := filepath.Join(directory, "kodema")
	command := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/kodema")
	command.Dir = root
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build kodema hook binary: %w: %s", err, strings.TrimSpace(string(output)))
	}
	info, err := os.Stat(binary)
	if err != nil {
		return "", fmt.Errorf("stat built hook binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("built hook binary %q is not a regular file", binary)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("built hook binary %q is not executable", binary)
	}
	return binary, nil
}

// findModuleRoot walks up from the current directory to the module root.
func findModuleRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		info, err := os.Stat(filepath.Join(directory, "go.mod"))
		if err == nil && !info.IsDir() {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no go.mod above the test working directory")
		}
		directory = parent
	}
}
