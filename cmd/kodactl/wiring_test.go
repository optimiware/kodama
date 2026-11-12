package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// isolateUserConfig points the default kodactl config directory at an empty
// temporary directory so a developer's real config never affects a test.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// writeSessionFile writes one current version 2 durable human session with the
// exact mode, bound to apiURL. A version 1 access-token-only file is not
// accepted by the renewing credential source, so the real-entry fixtures seed
// the v2 shape.
func writeSessionFile(t *testing.T, path, apiURL, token string, expires time.Time, mode os.FileMode) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"version":             2,
		"kind":                "session",
		"token":               token,
		"expires_at":          expires,
		"refresh_token":       "refresh-" + token,
		"issuer":              "https://issuer.example",
		"client_id":           "kodactl",
		"audience":            "kodama-api",
		"api_url":             apiURL,
		"token_endpoint":      "https://issuer.example/oauth/token",
		"revocation_endpoint": "https://issuer.example/oauth/revoke",
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("WriteFile session: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod session: %v", err)
	}
}

// resultRecorder records the Authorization header of every public operation
// request. Discovery requests are counted separately because a session
// credential source performs the API's own discovery to verify binding.
type resultRecorder struct {
	mu             sync.Mutex
	authorizations []string
	discoveries    int
}

func (recorder *resultRecorder) record(request *http.Request) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.authorizations = append(recorder.authorizations, request.Header.Get("Authorization"))
}

func (recorder *resultRecorder) recordDiscovery() {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.discoveries++
}

func (recorder *resultRecorder) count() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.authorizations)
}

func (recorder *resultRecorder) discoveryCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.discoveries
}

func (recorder *resultRecorder) authorization(index int) string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if index < 0 || index >= len(recorder.authorizations) {
		return ""
	}
	return recorder.authorizations[index]
}

// newResultServer answers every public operation request with one successful
// result envelope for the operation and serves a matching discovery document
// bound to server.URL at api.DiscoveryPath.
func newResultServer(t *testing.T, operation string, recorder *resultRecorder) *httptest.Server {
	t.Helper()
	return newTestResultServer(t, operation, recorder, true)
}

// newResultServerWithoutDiscovery returns 404 at api.DiscoveryPath so discovery
// failure paths are deterministic without external network dependencies.
func newResultServerWithoutDiscovery(t *testing.T, operation string, recorder *resultRecorder) *httptest.Server {
	t.Helper()
	return newTestResultServer(t, operation, recorder, false)
}

func newTestResultServer(t *testing.T, operation string, recorder *resultRecorder, serveDiscovery bool) *httptest.Server {
	t.Helper()
	payload, err := json.Marshal(api.Result{Operation: operation, Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == api.DiscoveryPath {
			if recorder != nil {
				recorder.recordDiscovery()
			}
			if !serveDiscovery {
				http.NotFound(writer, request)
				return
			}
			document, marshalErr := json.Marshal(api.DiscoveryDocument{
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
			if marshalErr != nil {
				http.Error(writer, marshalErr.Error(), http.StatusInternalServerError)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(document)
			return
		}
		if recorder != nil {
			recorder.record(request)
		}
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(api.HTTPStatusSuccess)
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestKodactlUsesSeededHumanSessionCredential pins that the real kodactl entry
// reads a current version 2 human session file on every command, verifies its
// server binding, authenticates with a `session` credential and never renders
// the token in arguments or diagnostics.
func TestKodactlUsesSeededHumanSessionCredential(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "human-session-token-abc123"

	recorder := &resultRecorder{}
	server := newResultServer(t, "task.read", recorder)
	writeSessionFile(t, sessionPath, server.URL, token, time.Now().Add(time.Hour), 0o600)
	args := []string{
		"-server-url", server.URL,
		"-session-file", sessionPath,
		"-output", "json",
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, defaultCLIDeps()); code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if recorder.count() != 1 {
		t.Fatalf("requests = %d; want 1", recorder.count())
	}
	if got := recorder.authorization(0); got != "Bearer "+token {
		t.Fatalf("authorization = %q; want the seeded human session token", got)
	}
	for _, rendered := range []string{stdout.String(), stderr.String(), strings.Join(args, " ")} {
		if strings.Contains(rendered, token) {
			t.Fatalf("the session token appeared in output or arguments: %q", rendered)
		}
	}
}

// TestKodactlRefusesSessionBoundToAnotherServer pins that a non-expiring (+1h)
// session minted for server A is refused before any operation request is sent
// to --server B, without leaking the session token.
func TestKodactlRefusesSessionBoundToAnotherServer(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "server-a-bound-session-token"

	recorderA := &resultRecorder{}
	serverA := newResultServer(t, "task.read", recorderA)
	recorderB := &resultRecorder{}
	serverB := newResultServer(t, "task.read", recorderB)
	writeSessionFile(t, sessionPath, serverA.URL, token, time.Now().Add(time.Hour), 0o600)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-server", serverB.URL,
		"-session-file", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 1 {
		t.Fatalf("exit code = %d; want failure (stderr=%q)", code, stderr.String())
	}
	if recorderA.count() != 0 || recorderB.count() != 0 {
		t.Fatalf("operation requests A=%d B=%d; want 0/0", recorderA.count(), recorderB.count())
	}
	if recorderB.discoveryCount() != 1 {
		t.Fatalf("server B discovery requests = %d; want 1", recorderB.discoveryCount())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "login required") {
		t.Fatalf("stderr = %q; want login-required refusal", stderr.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatalf("stderr rendered the session token: %q", stderr.String())
	}
}

// TestKodactlRefusesExpiredHumanSession pins that an expired version 2 session
// never authenticates a request and never renders its token. The renewing
// source tries the configured discovery first; the result server's discovery
// route is a 404, so the refusal happens before any public operation.
func TestKodactlRefusesExpiredHumanSession(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "expired-session-token-abc123"

	recorder := &resultRecorder{}
	server := newResultServerWithoutDiscovery(t, "task.read", recorder)
	writeSessionFile(t, sessionPath, server.URL, token, time.Now().Add(-time.Minute), 0o600)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-server-url", server.URL,
		"-session-file", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 1 {
		t.Fatalf("exit code = %d; want failure", code)
	}
	if recorder.count() != 0 {
		t.Fatalf("an expired session sent %d requests", recorder.count())
	}
	if recorder.discoveryCount() != 1 {
		t.Fatalf("discovery requests = %d; want the renewing source's one attempt", recorder.discoveryCount())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatalf("the expired-session diagnostic rendered the token: %q", stderr.String())
	}
}

// TestKodactlEnforcesSessionFileAndDirectoryPermissions pins the 0600 file and
// 0700 directory rule for the human session source.
func TestKodactlEnforcesSessionFileAndDirectoryPermissions(t *testing.T) {
	cases := []struct {
		name         string
		fileMode     os.FileMode
		directory    os.FileMode
		wantCode     int
		wantFragment string
		wantRequests int
	}{
		{name: "restrictive", fileMode: 0o600, directory: 0o700, wantCode: 0, wantRequests: 1},
		{name: "file too permissive", fileMode: 0o644, directory: 0o700, wantCode: 1, wantFragment: "0600"},
		{name: "directory too permissive", fileMode: 0o600, directory: 0o755, wantCode: 1, wantFragment: "0700"},
		{name: "file too restrictive", fileMode: 0o400, directory: 0o700, wantCode: 1, wantFragment: "0600"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateUserConfig(t)
			directory := t.TempDir()
			if err := os.Chmod(directory, testCase.directory); err != nil {
				t.Fatalf("Chmod directory: %v", err)
			}
			sessionPath := filepath.Join(directory, "session.json")
			token := "permission-session-token-abc123"

			recorder := &resultRecorder{}
			server := newResultServer(t, "task.read", recorder)
			writeSessionFile(t, sessionPath, server.URL, token, time.Now().Add(time.Hour), testCase.fileMode)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{
				"-server-url", server.URL,
				"-session-file", sessionPath,
				"call", "task.read", "--input={\"task_id\":\"task-1\"}",
			}, &stdout, &stderr, defaultCLIDeps())
			if code != testCase.wantCode {
				t.Fatalf("exit code = %d; want %d (stderr %q)", code, testCase.wantCode, stderr.String())
			}
			if recorder.count() != testCase.wantRequests {
				t.Fatalf("requests = %d; want %d", recorder.count(), testCase.wantRequests)
			}
			if testCase.wantFragment != "" && !strings.Contains(stderr.String(), testCase.wantFragment) {
				t.Fatalf("stderr = %q; want the %s permission refusal", stderr.String(), testCase.wantFragment)
			}
			if strings.Contains(stderr.String(), token) {
				t.Fatalf("the permission diagnostic rendered the token: %q", stderr.String())
			}
		})
	}
}

// TestKodactlRereadsTheSessionOnEveryCommand pins that the session credential
// source reads the file again for every command instead of caching it.
func TestKodactlRereadsTheSessionOnEveryCommand(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	firstToken := "first-human-session-token"

	recorder := &resultRecorder{}
	server := newResultServer(t, "task.read", recorder)
	writeSessionFile(t, sessionPath, server.URL, firstToken, time.Now().Add(time.Hour), 0o600)
	args := []string{
		"-server-url", server.URL,
		"-session-file", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr, defaultCLIDeps()); code != 0 {
		t.Fatalf("first exit code = %d; stderr=%q", code, stderr.String())
	}

	secondToken := "rotated-human-session-token"
	writeSessionFile(t, sessionPath, server.URL, secondToken, time.Now().Add(time.Hour), 0o600)
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), args, &stdout, &stderr, defaultCLIDeps()); code != 0 {
		t.Fatalf("second exit code = %d; stderr=%q", code, stderr.String())
	}

	if recorder.count() != 2 {
		t.Fatalf("requests = %d; want 2", recorder.count())
	}
	if got := recorder.authorization(0); got != "Bearer "+firstToken {
		t.Fatalf("first authorization = %q; want the first session token", got)
	}
	if got := recorder.authorization(1); got != "Bearer "+secondToken {
		t.Fatalf("second authorization = %q; want the rotated session token", got)
	}
}

// TestKodactlRefusesOversizedHumanSessionToken pins the public credential bound
// for the session source.
func TestKodactlRefusesOversizedHumanSessionToken(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := strings.Repeat("x", api.MaxCredentialBytes+1)

	recorder := &resultRecorder{}
	server := newResultServer(t, "task.read", recorder)
	writeSessionFile(t, sessionPath, server.URL, token, time.Now().Add(time.Hour), 0o600)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-server-url", server.URL,
		"-session-file", sessionPath,
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 1 {
		t.Fatalf("exit code = %d; want failure", code)
	}
	if recorder.count() != 0 {
		t.Fatalf("an oversized token sent %d requests", recorder.count())
	}
	if strings.Contains(stderr.String(), token) {
		t.Fatal("the oversized-token diagnostic rendered the token")
	}
}

// TestKodactlLoginCannotReuseASeededSession pins that the real entry has no
// seeded-session login path: `login` always runs the configured OAuth runtime,
// so a seeded session without a reachable discovery document is a visible
// failure and is never re-persisted as a login.
func TestKodactlLoginCannotReuseASeededSession(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod directory: %v", err)
	}
	sessionPath := filepath.Join(directory, "session.json")
	token := "login-human-session-token-abc123"
	writeSessionFile(t, sessionPath, "https://factory.example", token, time.Now().Add(time.Hour), 0o600)
	before, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}

	recorder := &resultRecorder{}
	server := newResultServerWithoutDiscovery(t, "task.read", recorder)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-server-url", server.URL,
		"-session-file", sessionPath,
		"login",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 1 {
		t.Fatalf("login exit code = %d; want a discovery failure", code)
	}
	if recorder.count() != 0 {
		t.Fatalf("login sent %d public operations", recorder.count())
	}
	if recorder.discoveryCount() != 1 {
		t.Fatalf("login discovery requests = %d; want exactly the runtime discovery", recorder.discoveryCount())
	}
	if strings.Contains(stdout.String(), "logged in") || strings.Contains(stderr.String(), "logged in") {
		t.Fatalf("the legacy seeded-session login path is still reachable: %q/%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("login rendered the session token")
	}
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session after login: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed login rewrote the seeded session")
	}
	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("Stat session: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %o; want 600", info.Mode().Perm())
	}
}

// TestKodactlHelperUsesConfiguredTrustedOrigins pins that the real entry loads
// the config file and returns the credential only for an exact registered
// origin and its path descendants.
func TestKodactlHelperUsesConfiguredTrustedOrigins(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	configFile := filepath.Join(directory, "config.json")
	config := `{"server_url":"https://factory.example","trusted_origins":["https://git.example.com/shop"]}`
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "exact origin", url: "https://git.example.com/shop", want: "kodex.execution-1.token"},
		{name: "subpath", url: "https://git.example.com/shop/repo.git", want: "kodex.execution-1.token"},
		{name: "lookalike path", url: "https://git.example.com/shopkeeper"},
		{name: "lookalike host", url: "https://git.example.com.evil/shop/repo.git"},
		{name: "other port", url: "https://git.example.com:8443/shop/repo.git"},
		{name: "other scheme", url: "http://git.example.com/shop/repo.git"},
		{name: "userinfo", url: "https://user@git.example.com/shop/repo.git"},
		{name: "query", url: "https://git.example.com/shop/repo.git?redirect=https://evil.example"},
		{name: "other host", url: "https://other.example/shop/repo.git"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{
				"-config", configFile,
				"-credential-file", tokenFile,
				"helper", "get", testCase.url,
			}, &stdout, &stderr, defaultCLIDeps())
			if code != 0 {
				t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
			}
			if testCase.want == "" {
				if stdout.Len() != 0 {
					t.Fatalf("stdout = %q; want an empty refusal", stdout.String())
				}
			} else if want := "username=kodama\npassword=" + testCase.want + "\n"; stdout.String() != want {
				t.Fatalf("stdout = %q; want %q", stdout.String(), want)
			}
			if strings.Contains(stderr.String(), "kodex.execution-1.token") {
				t.Fatalf("helper diagnostics rendered the token: %q", stderr.String())
			}
		})
	}
}

// TestKodactlHelperWithoutRegisteredOriginsReturnsNothing pins that the entry
// never fabricates a trusted origin when no config or flag registers one.
func TestKodactlHelperWithoutRegisteredOriginsReturnsNothing(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-credential-file", tokenFile,
		"helper", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want an empty refusal", stdout.String())
	}
}

// TestKodactlHelperUsesTrustedOriginFlag pins the explicit flag path.
func TestKodactlHelperUsesTrustedOriginFlag(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-credential-file", tokenFile,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if want := "username=kodama\npassword=kodex.execution-1.token\n"; stdout.String() != want {
		t.Fatalf("stdout = %q; want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"-credential-file", tokenFile,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shopkeeper",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 || stdout.Len() != 0 {
		t.Fatalf("lookalike helper = %d/%q; want an empty success", code, stdout.String())
	}
}

// TestKodactlReadsConfigFromEnvironment pins the KODAMA_CONFIG environment
// path.
func TestKodactlReadsConfigFromEnvironment(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	configFile := filepath.Join(directory, "config.json")
	config := `{"server_url":"https://factory.example","trusted_origins":["https://git.example.com/shop"]}`
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	t.Setenv("KODAMA_CONFIG", configFile)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-credential-file", tokenFile,
		"helper", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if want := "username=kodama\npassword=kodex.execution-1.token\n"; stdout.String() != want {
		t.Fatalf("stdout = %q; want %q", stdout.String(), want)
	}
}

// TestKodactlRefusesInvalidConfigFile pins that an unknown config member or an
// invalid trusted origin is a usage refusal that never renders a token.
func TestKodactlRefusesInvalidConfigFile(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.json")
	config := `{"server_url":"https://factory.example","trusted_origins":["https://git.example.com/shop"],"unexpected_member":true}`
	if err := os.WriteFile(configFile, []byte(config), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-config", configFile,
		"helper", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 2 {
		t.Fatalf("exit code = %d; want usage", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid kodactl config") {
		t.Fatalf("stderr = %q; want an invalid-config diagnostic", stderr.String())
	}
}
