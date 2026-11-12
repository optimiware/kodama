//go:build integration

package composition

// Private-registry composition evidence: the real production entry
// refuses a configured docker-config credential file that is missing or
// malformed, accepts a valid file, and the configured credential is what the
// immutable tag-to-digest resolution presents to a registry. The entry's
// configuration assignment itself is pinned by the cmd package unit test; this
// suite drives the built binary for the startup refusal and resolves through
// the same file-backed source the entry installs.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

const (
	registryUser     = "kodama"
	registryPassword = "registry-password-do-not-leak"
)

// registryBuildKodama builds the real command binary once per test.
func registryBuildKodama(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "kodama")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kodama")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kodama: %v\n%s", err, output)
	}
	return binary
}

// registryRun runs the built server entry with an isolated environment.
func registryRun(t *testing.T, binary string, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		command.Env = append(command.Env, name+"="+value)
	}
	var out, errOut bytes.Buffer
	command.Stdout, command.Stderr = &out, &errOut
	err := command.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run kodama: %v\nstderr: %s", err, errOut.String())
		}
		code = exit.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// registryWriteConfig writes one docker config.json file.
func registryWriteConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry credential file: %v", err)
	}
	return path
}

// TestProductionEntryRegistryCredentialFile proves the production entry's
// private-registry contract end to end.
func TestProductionEntryRegistryCredentialFile(t *testing.T) {
	binary := registryBuildKodama(t)
	serverArgs := []string{"server", "--profile", "production"}

	t.Run("missing file is refused by name", func(t *testing.T) {
		_, stderr, code := registryRun(t, binary, map[string]string{
			"KODAMA_REGISTRY_CONFIG_FILE": filepath.Join(t.TempDir(), "absent.json"),
		}, serverArgs...)
		if code != 2 {
			t.Fatalf("missing credential file exit = %d, want configuration refusal 2; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "KODAMA_REGISTRY_CONFIG_FILE") {
			t.Fatalf("refusal does not name KODAMA_REGISTRY_CONFIG_FILE: %s", stderr)
		}
	})

	t.Run("malformed file is refused", func(t *testing.T) {
		path := registryWriteConfig(t, `{"auths":{"registry.example.com":{"auth":"`+registryPassword+`"}}}`)
		_, stderr, code := registryRun(t, binary, map[string]string{
			"KODAMA_REGISTRY_CONFIG_FILE": path,
		}, serverArgs...)
		if code != 2 {
			t.Fatalf("malformed credential file exit = %d, want configuration refusal 2; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "KODAMA_REGISTRY_CONFIG_FILE") {
			t.Fatalf("refusal does not name KODAMA_REGISTRY_CONFIG_FILE: %s", stderr)
		}
		if strings.Contains(stderr, registryPassword) {
			t.Fatalf("refusal rendered the configured credential: %s", stderr)
		}
	})

	t.Run("valid file is accepted before the next required setting", func(t *testing.T) {
		path := registryWriteConfig(t, fmt.Sprintf(
			`{"auths":{"https://registry.example.com":{"auth":%q}}}`,
			base64.StdEncoding.EncodeToString([]byte(registryUser+":"+registryPassword))))
		_, stderr, code := registryRun(t, binary, map[string]string{
			"KODAMA_REGISTRY_CONFIG_FILE": path,
		}, serverArgs...)
		if code != 2 {
			t.Fatalf("valid credential file run exit = %d, want the next configuration refusal 2; stderr=%s", code, stderr)
		}
		if strings.Contains(stderr, "KODAMA_REGISTRY_CONFIG_FILE") {
			t.Fatalf("the entry refused a valid credential file: %s", stderr)
		}
		if !strings.Contains(stderr, "KODAMA_DATABASE_URL") {
			t.Fatalf("the entry did not reach the expected next requirement: %s", stderr)
		}
	})

	t.Run("configured credential resolves an immutable digest", func(t *testing.T) {
		body := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":1234,"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`
		sum := sha256.Sum256([]byte(body))
		digest := "sha256:" + hex.EncodeToString(sum[:])
		var authorization string
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			authorization = request.Header.Get("Authorization")
			path = request.URL.Path
			response.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			response.Header().Set("Docker-Content-Digest", digest)
			_, _ = io.WriteString(response, body)
		}))
		defer server.Close()
		host := strings.TrimPrefix(server.URL, "http://")
		credentialPath := registryWriteConfig(t, fmt.Sprintf(
			`{"auths":{"http://%s":{"auth":%q}}}`,
			host, base64.StdEncoding.EncodeToString([]byte(registryUser+":"+registryPassword))))

		source, err := registry.LoadDockerConfig(credentialPath)
		if err != nil {
			t.Fatalf("load configured credential file: %v", err)
		}
		resolver, err := registry.New(registry.Config{
			Transport:         http.DefaultTransport,
			Credentials:       source,
			AllowInsecureHTTP: true,
		})
		if err != nil {
			t.Fatalf("registry.New: %v", err)
		}
		reference := host + "/team/image:1.0"
		image, err := resolver.Resolve(context.Background(), pipeline.ResolveRequest{Reference: reference})
		if err != nil {
			t.Fatalf("resolve %s: %v", reference, err)
		}
		if image.Reference != host+"/team/image" || image.Digest != digest {
			t.Fatalf("resolved image = %#v, want %s@%s", image, host+"/team/image", digest)
		}
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(registryUser+":"+registryPassword))
		if authorization != want {
			t.Fatalf("registry Authorization = %q, want the configured Basic credential", authorization)
		}
		if path != "/v2/team/image/manifests/1.0" {
			t.Fatalf("manifest path = %q", path)
		}
		if strings.Contains(image.Digest+image.Reference, registryPassword) {
			t.Fatal("the resolved image carries the registry credential")
		}
	})
}
