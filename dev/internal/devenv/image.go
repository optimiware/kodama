package devenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ManifestTypes are the media types the registry answers for a tag.
const ManifestTypes = "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json"

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// buildImage builds the fixture image with the real Linux `kodama`, the
// dependency probe and the Git server, publishes it to the scope registry and
// records its digest.
func (e *Environment) buildImage() error {
	lock, err := Versions()
	if err != nil {
		return err
	}
	version, err := output("go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	if strings.TrimSpace(version) != "go"+lock.Go {
		return fmt.Errorf("expected Go %s, found %s", lock.Go, strings.TrimSpace(version))
	}
	machine, apkArch, err := goarch(runtime.GOARCH)
	if err != nil {
		return err
	}
	context := filepath.Join(e.Dir, "image")
	if err := os.MkdirAll(context, 0o700); err != nil {
		return err
	}
	packageLock := lock.GitDaemon
	apk := filepath.Join(context, "git-daemon.apk")
	expected := packageLock.SHA256[machine]
	if !checksumMatches(apk, expected) {
		url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/%s/main/%s/git-daemon-%s.apk",
			packageLock.Alpine, apkArch, packageLock.Version)
		if err := download(url, apk, expected); err != nil {
			return err
		}
	}
	env := append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+machine)
	for name, source := range map[string]string{"kodama": "./cmd/kodama", "probe": "./dev/probe", "gitserver": "./dev/gitserver"} {
		if _, err := runCmd(Cmd{Args: []string{"go", "build", "-mod=readonly", "-trimpath", "-o", filepath.Join(context, name), source},
			Env: env, Timeout: 10 * time.Minute}); err != nil {
			return err
		}
	}
	repository := fmt.Sprintf("localhost:%d/%s/scaffold", e.State.Port, e.Scope)
	if _, err := runCmd(Cmd{Args: []string{e.Provider, "build", "-f", filepath.Join(Root(), "dev", "images", "Containerfile"),
		"-t", repository + ":dev", context}, Timeout: 10 * time.Minute}); err != nil {
		return err
	}
	push := []string{e.Provider, "push"}
	if e.Provider == "podman" {
		push = append(push, "--tls-verify=false")
	}
	push = append(push, repository+":dev")
	if _, err := runCmd(Cmd{Args: push, Timeout: 10 * time.Minute}); err != nil {
		return err
	}
	digest, err := e.registryDigest(repository)
	if err != nil {
		return err
	}
	e.State.LocalImage = repository + ":dev"
	e.State.Image = repository + "@" + digest
	return e.Save()
}

func goarch(arch string) (machine, apkArch string, err error) {
	switch arch {
	case "arm64", "aarch64":
		return "arm64", "aarch64", nil
	case "amd64", "x86_64":
		return "amd64", "x86_64", nil
	default:
		return "", "", fmt.Errorf("unsupported host architecture %s", arch)
	}
}

func (e *Environment) registryDigest(repository string) (string, error) {
	request, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://localhost:%d/v2/%s/scaffold/manifests/dev", e.State.Port, e.Scope), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", ManifestTypes)
	client := http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	digest := response.Header.Get("Docker-Content-Digest")
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry returned an invalid image digest %q", digest)
	}
	return digest, nil
}

func checksumMatches(path, expected string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

func download(url, path, expected string) error {
	client := http.Client{Timeout: 60 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: HTTP %d", url, response.StatusCode)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != expected {
		return fmt.Errorf("git-daemon package checksum mismatch")
	}
	return os.WriteFile(path, data, 0o644)
}
