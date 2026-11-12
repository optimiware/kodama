//go:build integration

package identity_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// buildKodactl builds the real CLI entrypoint. Tests drive the binary so the
// flag/config wiring, the human-session source and the filesystem driver are
// exercised together rather than through injected ports.
func buildKodactl(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("KODAMA_REQUIRE_INFRA") == "1" {
			t.Fatalf("go toolchain is not available (KODAMA_REQUIRE_INFRA=1): %v", err)
		}
		t.Skip("go toolchain is not available")
	}
	binary := filepath.Join(t.TempDir(), "kodactl")
	command := exec.Command("go", "build", "-o", binary, "./cmd/kodactl")
	command.Dir = repositoryRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build kodactl: %v\n%s", err, output)
	}
	return binary
}

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
			t.Fatal("repository root not found")
		}
		directory = parent
	}
}

// isolateKodactlConfig keeps the developer's real ~/.config/kodama out of the
// test and gives the default config/session paths an empty root.
func isolateKodactlConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// writeRecord seeds one version 2 human session with the restrictive modes the
// product requires. The helper reads it without a refresh because the expiry is
// an hour away; the binding is complete so a later refresh would be possible.
func writeRecord(t *testing.T, path, apiURL, token string, expires time.Time) {
	t.Helper()
	record := cli.SessionRecord{
		Version:            cli.SessionFileVersion,
		Kind:               "session",
		Token:              token,
		ExpiresAt:          expires,
		RefreshToken:       token + "-refresh",
		Issuer:             "https://issuer.example",
		ClientID:           "kodactl",
		Audience:           "kodama-api",
		APIURL:             apiURL,
		TokenEndpoint:      "https://issuer.example/oauth/token",
		RevocationEndpoint: "https://issuer.example/oauth/revoke",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile session: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod session: %v", err)
	}
}

// startDiscoveryServer serves the API discovery document the session source
// verifies the stored binding against on every credential read, bound to the
// server's own URL, issuer, client and audience as seeded by writeRecord.
func startDiscoveryServer(t *testing.T) string {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != api.DiscoveryPath {
			http.NotFound(writer, request)
			return
		}
		document, err := json.Marshal(api.DiscoveryDocument{
			Issuer:                      "https://issuer.example",
			ClientID:                    "kodactl",
			Audience:                    "kodama-api",
			AuthorizationEndpoint:       "https://issuer.example/oauth/authorize",
			TokenEndpoint:               "https://issuer.example/oauth/token",
			DeviceAuthorizationEndpoint: "https://issuer.example/oauth/device",
			RevocationEndpoint:          "https://issuer.example/oauth/revoke",
			Scopes:                      []string{"openid", "profile"},
			APIURL:                      server.URL,
		})
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(document)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// writeLegacySession seeds a version 1 access-token-only session.
func writeLegacySession(t *testing.T, path, token string, expires time.Time) {
	t.Helper()
	data, err := json.Marshal(cli.SessionRecord{Kind: "session", Token: token, ExpiresAt: expires})
	if err != nil {
		t.Fatalf("marshal legacy session: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile legacy session: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod legacy session: %v", err)
	}
}

func writeConfigFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
}

// runKodactl executes the built binary and returns its exit code and streams.
func runKodactl(t *testing.T, binary string, args ...string) (int, string, string) {
	t.Helper()
	command := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run kodactl: %v", err)
	}
	return exitErr.ExitCode(), stdout.String(), stderr.String()
}

// TestKodactlLoginNeverRepersistsASeededSession pins that the real entrypoint
// always runs the OAuth runtime, so a seeded session is not a login and a
// failed discovery leaves the file untouched.
func TestKodactlLoginNeverRepersistsASeededSession(t *testing.T) {
	isolateKodactlConfig(t)
	binary := buildKodactl(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "human-session-token-abc123"
	writeLegacySession(t, sessionPath, token, time.Now().Add(time.Hour))
	before, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}

	// Port 1 on loopback refuses immediately, so the discovery fails closed
	// without any network fixture.
	code, stdout, stderr := runKodactl(t, binary,
		"-server-url", "https://127.0.0.1:1",
		"-session-file", sessionPath,
		"login",
	)
	if code != 1 {
		t.Fatalf("login exit code = %d; want a discovery failure", code)
	}
	if strings.Contains(stdout, "logged in") || strings.Contains(stderr, "logged in") {
		t.Fatal("the legacy seeded-session login path is still reachable")
	}
	if !strings.Contains(stderr, "login configuration") {
		t.Fatalf("stderr = %q; want the discovery failure", stderr)
	}
	if strings.Contains(stdout, token) || strings.Contains(stderr, token) {
		t.Fatal("login rendered the seeded session token")
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session after login: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("a failed login rewrote the seeded session")
	}
}

// TestKodactlLoginIgnoresExecutionCredential pins that an execution credential
// file is not a human session and is not a login mechanism: the real OAuth
// runtime still discovers the provider and fails closed when it is down.
func TestKodactlLoginIgnoresExecutionCredential(t *testing.T) {
	isolateKodactlConfig(t)
	binary := buildKodactl(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")

	code, stdout, stderr := runKodactl(t, binary,
		"-server-url", "https://127.0.0.1:1",
		"-credential-file", tokenFile,
		"-session-file", sessionPath,
		"login",
	)
	if code != 1 {
		t.Fatalf("login exit code = %d; want a discovery failure", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q; want empty", stdout)
	}
	if strings.Contains(stderr, "login requires a human session credential") {
		t.Fatal("login still uses the seeded-session credential path")
	}
	if !strings.Contains(stderr, "login configuration") {
		t.Fatalf("stderr = %q; want the discovery failure", stderr)
	}
	if strings.Contains(stdout, "kodex.execution-1.token") || strings.Contains(stderr, "kodex.execution-1.token") {
		t.Fatal("login rendered the execution token")
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatalf("login wrote a session file for an execution credential (stat err %v)", err)
	}
}

// TestKodactlHelperUsesConfiguredTrustedOrigins proves the real entrypoint
// loads the config file and returns the seeded session only for an exact
// registered origin and its path descendants.
func TestKodactlHelperUsesConfiguredTrustedOrigins(t *testing.T) {
	isolateKodactlConfig(t)
	binary := buildKodactl(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "helper-human-session-token-abc123"
	serverURL := startDiscoveryServer(t)
	writeRecord(t, sessionPath, serverURL, token, time.Now().Add(time.Hour))
	configPath := filepath.Join(directory, "config.json")
	writeConfigFile(t, configPath, `{"server_url":"`+serverURL+`","trusted_origins":["https://git.example.com/shop"]}`)

	cases := []struct {
		name string
		url  string
		want string
	}{
		{"exact origin", "https://git.example.com/shop", token},
		{"subpath", "https://git.example.com/shop/repo.git", token},
		{"lookalike path", "https://git.example.com/shopkeeper", ""},
		{"lookalike host", "https://git.example.com.evil/shop/repo.git", ""},
		{"other port", "https://git.example.com:8443/shop/repo.git", ""},
		{"other scheme", "http://git.example.com/shop/repo.git", ""},
		{"userinfo", "https://user@git.example.com/shop/repo.git", ""},
		{"query", "https://git.example.com/shop/repo.git?redirect=https://evil.example", ""},
		{"other host", "https://other.example/shop/repo.git", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, stdout, stderr := runKodactl(t, binary,
				"-config", configPath,
				"-session-file", sessionPath,
				"helper", "get", testCase.url,
			)
			if code != 0 {
				t.Fatalf("helper exit code = %d; stderr=%q", code, stderr)
			}
			if testCase.want == "" {
				if stdout != "" {
					t.Fatalf("stdout = %q; want an empty refusal", stdout)
				}
			} else if want := "username=kodama\npassword=" + testCase.want + "\n"; stdout != want {
				t.Fatalf("stdout = %q; want %q", stdout, want)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q; want empty", stderr)
			}
			if strings.Contains(stderr, token) {
				t.Fatal("helper diagnostics rendered the session token")
			}
		})
	}
}

// TestKodactlHelperWithoutRegisteredOriginsReturnsNothing pins that the real
// entrypoint never fabricates a trusted origin when none is registered.
func TestKodactlHelperWithoutRegisteredOriginsReturnsNothing(t *testing.T) {
	isolateKodactlConfig(t)
	binary := buildKodactl(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	writeRecord(t, sessionPath, "https://factory.example", "helper-human-session-token-abc123", time.Now().Add(time.Hour))

	code, stdout, stderr := runKodactl(t, binary,
		"-session-file", sessionPath,
		"helper", "get", "https://git.example.com/shop/repo.git",
	)
	if code != 0 {
		t.Fatalf("helper exit code = %d; stderr=%q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q; want an empty refusal", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}
}

// TestKodactlHelperUsesTrustedOriginFlag pins the repeatable explicit flag path
// on the real entrypoint.
func TestKodactlHelperUsesTrustedOriginFlag(t *testing.T) {
	isolateKodactlConfig(t)
	binary := buildKodactl(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "flag-human-session-token-abc123"
	serverURL := startDiscoveryServer(t)
	writeRecord(t, sessionPath, serverURL, token, time.Now().Add(time.Hour))

	code, stdout, stderr := runKodactl(t, binary,
		"-server-url", serverURL,
		"-session-file", sessionPath,
		"-trusted-origin", "https://git.example.com/shop",
		"-trusted-origin", "https://git.example.com/other",
		"helper", "get", "https://git.example.com/shop/repo.git",
	)
	if code != 0 {
		t.Fatalf("helper exit code = %d; stderr=%q", code, stderr)
	}
	if want := "username=kodama\npassword=" + token + "\n"; stdout != want {
		t.Fatalf("stdout = %q; want %q", stdout, want)
	}

	code, stdout, stderr = runKodactl(t, binary,
		"-server-url", serverURL,
		"-session-file", sessionPath,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shopkeeper",
	)
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("lookalike helper = %d/%q/%q; want an empty success", code, stdout, stderr)
	}
}
