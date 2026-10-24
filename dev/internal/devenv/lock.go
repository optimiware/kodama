// Package devenv manages one scoped development environment: the dependency
// containers of dev/compose.yaml, an optional kind cluster, the generated
// fixtures, and the endpoint files Go tests consume.
//
// It replaces the Python harness that used to live in scripts/dev. Compose
// still owns the dependency containers; this package decides what a scope
// contains and keeps every external resource bound to its ownership token.
package devenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Tool is a checksum-locked binary distribution.
type Tool struct {
	Version string            `json:"version"`
	SHA256  map[string]string `json:"sha256"`
}

// GitDaemon is the Alpine package that provides git-http-backend in the
// fixture image.
type GitDaemon struct {
	Version string            `json:"version"`
	Alpine  string            `json:"alpine"`
	SHA256  map[string]string `json:"sha256"`
}

// Lock is the checked-in pin file dev/versions.lock.json.
type Lock struct {
	Go      string `json:"go"`
	Kind    Tool   `json:"kind"`
	Kubectl Tool   `json:"kubectl"`
	Images  struct {
		Node string `json:"node"`
	} `json:"images"`
	GitDaemon GitDaemon `json:"git_daemon"`
}

var (
	lockOnce sync.Once
	lockFile Lock
	lockErr  error
	rootOnce sync.Once
	rootDir  string
)

// Root is the repository root, derived from this source file so commands work
// from any working directory.
func Root() string {
	rootOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			panic("dev/internal/devenv: cannot locate the repository root")
		}
		rootDir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	})
	return rootDir
}

// StateDir is dev/.state in this checkout. Tests replace it with a temporary
// directory.
func StateDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	return filepath.Join(Root(), "dev", ".state")
}

var stateDirOverride string

// ToolsDir is dev/.tools in this checkout.
func ToolsDir() string { return filepath.Join(Root(), "dev", ".tools") }

// Versions returns the parsed pin file.
func Versions() (Lock, error) {
	lockOnce.Do(func() {
		data, err := os.ReadFile(filepath.Join(Root(), "dev", "versions.lock.json"))
		if err != nil {
			lockErr = err
			return
		}
		lockErr = json.Unmarshal(data, &lockFile)
	})
	return lockFile, lockErr
}

// ComposeFile is the dependency stack definition.
func ComposeFile() string { return filepath.Join(Root(), "dev", "compose.yaml") }

// PlatformKey names one architecture-specific pin, for example darwin-arm64.
func PlatformKey(system, arch string) (string, error) {
	switch arch {
	case "arm64", "aarch64":
		arch = "arm64"
	case "x86_64", "amd64":
		arch = "amd64"
	default:
		return "", fmt.Errorf("unsupported host architecture %s", arch)
	}
	return system + "-" + arch, nil
}
