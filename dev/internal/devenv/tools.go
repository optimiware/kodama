package devenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// InstallTool downloads a checksum-locked tool into this worktree only and
// returns a stable executable path.
func InstallTool(name string) (string, error) {
	lock, err := Versions()
	if err != nil {
		return "", err
	}
	var spec Tool
	switch name {
	case "kind":
		spec = lock.Kind
	case "kubectl":
		spec = lock.Kubectl
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
	key, err := PlatformKey(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	expected, ok := spec.SHA256[key]
	if !ok {
		return "", fmt.Errorf("%s has no pin for %s", name, key)
	}
	target := filepath.Join(ToolsDir(), name+"-"+spec.Version)
	if checksumMatches(target, expected) {
		return commandPath(name, target)
	}
	url := fmt.Sprintf("https://github.com/kubernetes-sigs/kind/releases/download/v%s/kind-%s-%s",
		spec.Version, runtime.GOOS, runtime.GOARCH)
	if name == "kubectl" {
		url = fmt.Sprintf("https://dl.k8s.io/release/v%s/bin/%s/%s/kubectl", spec.Version, runtime.GOOS, runtime.GOARCH)
	}
	fmt.Println("Downloading " + url)
	client := http.Client{Timeout: 120 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		return "", fmt.Errorf("%s checksum mismatch", name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	temporary := fmt.Sprintf("%s.tmp-%d", target, os.Getpid())
	if err := os.WriteFile(temporary, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", err
	}
	return commandPath(name, target)
}

// commandPath exposes a pinned tool under its stable name without replacing
// an unmanaged file.
func commandPath(name, target string) (string, error) {
	directory := filepath.Join(filepath.Dir(target), "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	alias := filepath.Join(directory, name)
	if info, err := os.Lstat(alias); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(alias)
			if err != nil || resolved == target {
				return alias, err
			}
			if err := os.Remove(alias); err != nil {
				return "", err
			}
		} else {
			return "", fmt.Errorf("refusing to replace unmanaged tool %s", alias)
		}
	}
	if err := os.Symlink(target, alias); err != nil {
		return "", err
	}
	return alias, nil
}
