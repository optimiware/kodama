package up

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	// DefaultInstanceName is the default stack instance name.
	DefaultInstanceName = "default"
	instanceMarkerName  = ".kodama-up"
	maxInstanceNameLen  = 48
)

var instanceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// StateDirOptions configures state directory resolution and platform safety checks.
type StateDirOptions struct {
	GOOS                  string
	HomeDir               string
	InstanceName          string
	ExplicitStateDir      string
	AllowUnsharedStateDir bool
}

// ValidateInstanceName verifies that name is a valid lowercase DNS label.
func ValidateInstanceName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("instance name is empty")
	}
	if len(trimmed) > maxInstanceNameLen || !instanceNamePattern.MatchString(trimmed) {
		return fmt.Errorf("instance name %q must be a lowercase DNS label (a-z, 0-9, '-') of at most %d characters", name, maxInstanceNameLen)
	}
	return nil
}

// DefaultStateDir returns $HOME/.kodama/instances/<name>, which is shared into
// macOS Linux container VMs via virtiofs without extra configuration.
func DefaultStateDir(homeDir, instanceName string) (string, error) {
	if err := ValidateInstanceName(instanceName); err != nil {
		return "", err
	}
	home := strings.TrimSpace(homeDir)
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("an absolute HOME directory is required to resolve the default state directory")
	}
	return filepath.Join(filepath.Clean(home), ".kodama", "instances", instanceName), nil
}

// ResolveStateDir resolves and validates the stack state directory. On darwin,
// paths outside $HOME (such as /tmp or /var/tmp, which are not shared into
// Podman Machine, OrbStack, Docker Desktop or Colima Linux VMs via virtiofs and
// fail container bind-mounts with ENOENT) are refused unless
// AllowUnsharedStateDir is true.
func ResolveStateDir(opts StateDirOptions) (string, error) {
	goos := strings.TrimSpace(opts.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	instance := strings.TrimSpace(opts.InstanceName)
	if instance == "" {
		instance = DefaultInstanceName
	}
	if err := ValidateInstanceName(instance); err != nil {
		return "", err
	}

	home := strings.TrimSpace(opts.HomeDir)
	if home == "" {
		detected, err := os.UserHomeDir()
		if err == nil {
			home = detected
		}
	}

	raw := strings.TrimSpace(opts.ExplicitStateDir)
	if raw == "" {
		def, err := DefaultStateDir(home, instance)
		if err != nil {
			return "", err
		}
		raw = def
	} else if raw == "~" && home != "" {
		raw = home
	} else if after, ok := strings.CutPrefix(raw, "~/"); ok && home != "" {
		raw = filepath.Join(home, after)
	}

	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("state directory %q must be an absolute path", raw)
	}
	cleaned := evalExistingAncestors(filepath.Clean(raw))
	if cleaned == "/" || strings.HasPrefix(raw, "//") {
		return "", fmt.Errorf("refusing degenerate state directory %q", raw)
	}

	if home != "" && filepath.IsAbs(home) {
		cleanHome := evalExistingAncestors(filepath.Clean(home))
		parentHome := filepath.Dir(cleanHome)
		if cleaned == cleanHome || cleaned == parentHome {
			return "", fmt.Errorf("refusing state directory %q: must not be HOME or its parent directory", raw)
		}
		if goos == "darwin" && !opts.AllowUnsharedStateDir {
			if !isStrictSubpath(cleaned, cleanHome) {
				return "", fmt.Errorf(
					"refusing state directory %q outside %q on macOS: Linux container VMs share $HOME via virtiofs and bind-mounts under /tmp or /var/tmp fail with ENOENT; use %q or pass --allow-unshared-state-dir",
					raw, cleanHome, filepath.Join(cleanHome, ".kodama", "instances", instance),
				)
			}
		}
	} else if goos == "darwin" && !opts.AllowUnsharedStateDir {
		return "", errors.New("HOME is not set on macOS; set HOME or pass --allow-unshared-state-dir")
	}

	return cleaned, nil
}

// EnsureInstanceStateDir creates stateDir (mode 0700) and records or verifies
// the .kodama-up ownership marker so another instance or an unrelated directory
// is never overwritten.
func EnsureInstanceStateDir(stateDir, instanceName string) error {
	if err := ValidateInstanceName(instanceName); err != nil {
		return err
	}
	markerPath := filepath.Join(stateDir, instanceMarkerName)
	expected := "instance=" + instanceName

	if info, err := os.Stat(stateDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("state path %q exists and is not a directory", stateDir)
		}
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			return fmt.Errorf("read state directory %q: %w", stateDir, err)
		}
		if len(entries) > 0 {
			recorded, ok := readInstanceMarker(markerPath)
			if !ok || recorded != expected {
				return fmt.Errorf("refusing state directory %q: non-empty directory does not belong to kodama instance %q", stateDir, instanceName)
			}
		}
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return fmt.Errorf("create state directory %q: %w", stateDir, err)
		}
	} else {
		return fmt.Errorf("stat state directory %q: %w", stateDir, err)
	}
	_ = os.Chmod(stateDir, 0o700)

	if err := writeFileAtomic(markerPath, []byte(expected+"\n"), 0o600); err != nil {
		return fmt.Errorf("write instance marker: %w", err)
	}
	return nil
}

// PurgeInstanceStateDir deletes only known Kodama instance files and directories
// inside stateDir after verifying the .kodama-up marker. If unknown files
// remain, the marker is kept intact and an actionable error is returned.
func PurgeInstanceStateDir(stateDir, instanceName string) error {
	if err := ValidateInstanceName(instanceName); err != nil {
		return err
	}
	info, err := os.Stat(stateDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path %q is not a directory", stateDir)
	}

	markerPath := filepath.Join(stateDir, instanceMarkerName)
	expected := "instance=" + instanceName
	recorded, ok := readInstanceMarker(markerPath)
	if !ok || recorded != expected {
		return fmt.Errorf("refusing to purge %q: directory does not carry marker for instance %q", stateDir, instanceName)
	}

	knownDirs := []string{"tls", "secrets", "data", "git", "logs", "run"}
	for _, sub := range knownDirs {
		_ = os.RemoveAll(filepath.Join(stateDir, sub))
	}
	knownFiles := []string{
		"stack.json",
		"env.sh",
		"admin-token",
		"oidc-signing.key",
		"gateway.pid",
		"gateway.log",
		"bootstrap.done",
		"project.done",
		"personas.done",
	}
	for _, file := range knownFiles {
		_ = os.Remove(filepath.Join(stateDir, file))
	}

	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	var unknown []string
	for _, entry := range entries {
		if entry.Name() != instanceMarkerName {
			unknown = append(unknown, entry.Name())
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf(
			"refusing to delete unknown files in %q (%s); move or remove them, then run `kodama down --purge` again",
			stateDir, strings.Join(unknown, ", "),
		)
	}

	if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(stateDir)
}

func readInstanceMarker(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	trimmed := strings.TrimSuffix(string(data), "\n")
	if strings.Contains(trimmed, "\n") || !strings.HasPrefix(trimmed, "instance=") {
		return "", false
	}
	return trimmed, len(strings.TrimPrefix(trimmed, "instance=")) > 0
}

func isStrictSubpath(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func evalExistingAncestors(absPath string) string {
	current := absPath
	var tail []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absPath
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}
