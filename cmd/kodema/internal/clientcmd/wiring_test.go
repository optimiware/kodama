package clientcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
	"github.com/optimiware/kodema/internal/userland/up"
)

// isolateUserConfig points the default kodema config directory and HOME at
// empty temporary directories so a developer's real config or running
// ~/.kodema/instances/default never affects a test.
func isolateUserConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
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
		"audience":            "kodema-api",
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
				Audience:                    "kodema-api",
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

// TestKodemaUsesSeededHumanSessionCredential pins that the real kodema entry
// reads a current version 2 human session file on every command, verifies its
// server binding, authenticates with a `session` credential and never renders
// the token in arguments or diagnostics.
func TestKodemaUsesSeededHumanSessionCredential(t *testing.T) {
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

// TestKodemaRefusesSessionBoundToAnotherServer pins that a non-expiring (+1h)
// session minted for server A is refused before any operation request is sent
// to --server B, without leaking the session token.
func TestKodemaRefusesSessionBoundToAnotherServer(t *testing.T) {
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

// TestKodemaRefusesExpiredHumanSession pins that an expired version 2 session
// never authenticates a request and never renders its token. The renewing
// source tries the configured discovery first; the result server's discovery
// route is a 404, so the refusal happens before any public operation.
func TestKodemaRefusesExpiredHumanSession(t *testing.T) {
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

// TestKodemaEnforcesSessionFileAndDirectoryPermissions pins the 0600 file and
// 0700 directory rule for the human session source.
func TestKodemaEnforcesSessionFileAndDirectoryPermissions(t *testing.T) {
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

// TestKodemaRereadsTheSessionOnEveryCommand pins that the session credential
// source reads the file again for every command instead of caching it.
func TestKodemaRereadsTheSessionOnEveryCommand(t *testing.T) {
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

// TestKodemaRefusesOversizedHumanSessionToken pins the public credential bound
// for the session source.
func TestKodemaRefusesOversizedHumanSessionToken(t *testing.T) {
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

// TestKodemaLoginCannotReuseASeededSession pins that the real entry has no
// seeded-session login path: `login` always runs the configured OAuth runtime,
// so a seeded session without a reachable discovery document is a visible
// failure and is never re-persisted as a login.
func TestKodemaLoginCannotReuseASeededSession(t *testing.T) {
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

// TestKodemaHelperUsesConfiguredTrustedOrigins pins that the real entry loads
// the config file and returns the credential only for an exact registered
// origin and its path descendants.
func TestKodemaHelperUsesConfiguredTrustedOrigins(t *testing.T) {
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
				"git", "credential", "get", testCase.url,
			}, &stdout, &stderr, defaultCLIDeps())
			if code != 0 {
				t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
			}
			if testCase.want == "" {
				if stdout.Len() != 0 {
					t.Fatalf("stdout = %q; want an empty refusal", stdout.String())
				}
			} else if want := "username=kodema\npassword=" + testCase.want + "\n"; stdout.String() != want {
				t.Fatalf("stdout = %q; want %q", stdout.String(), want)
			}
			if strings.Contains(stderr.String(), "kodex.execution-1.token") {
				t.Fatalf("helper diagnostics rendered the token: %q", stderr.String())
			}
		})
	}
}

// TestKodemaHelperWithoutRegisteredOriginsReturnsNothing pins that the entry
// never fabricates a trusted origin when no config or flag registers one.
func TestKodemaHelperWithoutRegisteredOriginsReturnsNothing(t *testing.T) {
	isolateUserConfig(t)
	directory := t.TempDir()
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte("kodex.execution-1.token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile token: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-credential-file", tokenFile,
		"git", "credential", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want an empty refusal", stdout.String())
	}
}

// TestKodemaHelperUsesTrustedOriginFlag pins the explicit flag path.
func TestKodemaHelperUsesTrustedOriginFlag(t *testing.T) {
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
		"git", "credential", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if want := "username=kodema\npassword=kodex.execution-1.token\n"; stdout.String() != want {
		t.Fatalf("stdout = %q; want %q", stdout.String(), want)
	}

	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"-credential-file", tokenFile,
		"-trusted-origin", "https://git.example.com/shop",
		"git", "credential", "get", "https://git.example.com/shopkeeper",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 || stdout.Len() != 0 {
		t.Fatalf("lookalike helper = %d/%q; want an empty success", code, stdout.String())
	}
}

// TestKodemaReadsConfigFromEnvironment pins the KODEMA_CONFIG environment
// path.
func TestKodemaReadsConfigFromEnvironment(t *testing.T) {
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
	t.Setenv("KODEMA_CONFIG", configFile)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-credential-file", tokenFile,
		"git", "credential", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	if want := "username=kodema\npassword=kodex.execution-1.token\n"; stdout.String() != want {
		t.Fatalf("stdout = %q; want %q", stdout.String(), want)
	}
}

// TestKodemaRefusesInvalidConfigFile pins that an unknown config member or an
// invalid trusted origin is a usage refusal that never renders a token.
func TestKodemaRefusesInvalidConfigFile(t *testing.T) {
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
		"git", "credential", "get", "https://git.example.com/shop/repo.git",
	}, &stdout, &stderr, defaultCLIDeps())
	if code != 2 {
		t.Fatalf("exit code = %d; want usage", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid kodema config") {
		t.Fatalf("stderr = %q; want an invalid-config diagnostic", stderr.String())
	}
}

// setupDefaultInstance writes a live ~/.kodema/instances/default state dir
// under home pointing at publicURL, caPath and projectID.
func setupDefaultInstance(t *testing.T, home, publicURL, caPath, projectID string) string {
	t.Helper()
	stateDir, err := up.DefaultStateDir(home, up.DefaultInstanceName)
	if err != nil {
		t.Fatalf("DefaultStateDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "tls"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	desc := up.StackDescriptor{
		Instance:   up.DefaultInstanceName,
		StateDir:   stateDir,
		PublicURL:  publicURL,
		TenantID:   up.DefaultTenantID,
		ProjectID:  projectID,
		CACertPath: caPath,
		GatewayPID: os.Getpid(),
	}
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		t.Fatalf("marshal stack descriptor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "stack.json"), data, 0o600); err != nil {
		t.Fatalf("WriteFile stack.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "gateway.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile gateway.pid: %v", err)
	}
	return stateDir
}

// TestKodemaAutoDiscoversDefaultInstanceForClientCommands pins that in a fresh
// shell without env.sh or --server/--ca-file/--project, `kodema login`,
// `kodema whoami`, `kodema inbox`, and `kodema git credential get` auto-discover
// the running ~/.kodema/instances/default stack, and explicit flags still win.
func TestKodemaAutoDiscoversDefaultInstanceForClientCommands(t *testing.T) {
	isolateUserConfig(t)
	home := os.Getenv("HOME")
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	var gotInboxProject string
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case api.DiscoveryPath:
			writeJSON(writer, document)
		case "/api/v1/whoami":
			payload, _ := json.Marshal(api.Result{
				Operation: "principal.whoami",
				Body:      json.RawMessage(`{"principal_id":"oidc-00112233445566778899aabbccddeeff","kind":"human","tenant_id":"tenant-local","project_id":"project-local","roles":["engineer"]}`),
			})
			writer.Header().Set("Content-Type", api.MediaType)
			writer.WriteHeader(api.HTTPStatusSuccess)
			_, _ = writer.Write(payload)
		default:
			if strings.Contains(request.URL.Path, "/projects/") && strings.HasSuffix(request.URL.Path, "/inbox") {
				parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
				for i := 0; i < len(parts)-1; i++ {
					if parts[i] == "projects" {
						gotInboxProject = parts[i+1]
						break
					}
				}
				payload, _ := json.Marshal(api.Result{
					Operation: "task.inbox",
					Body:      json.RawMessage(`{"items":[]}`),
				})
				writer.Header().Set("Content-Type", api.MediaType)
				writer.WriteHeader(api.HTTPStatusSuccess)
				_, _ = writer.Write(payload)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(apiServer.Close)
	apiServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	provider.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	document = provider.document(apiServer.URL)

	caPath := writeCertBundlePEM(t, provider.server, apiServer)
	setupDefaultInstance(t, home, apiServer.URL, caPath, "project-local")

	deps := defaultCLIDeps()
	deps.clock = fixedClock{now: time.Now().Truncate(time.Second)}
	deps.environment = mapEnvironment(nil)

	// 1. `kodema login --device` auto-discovers server + CA from default instance.
	code, stdout, stderr := runEntry(t, []string{"login", "--device"}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("auto-discovered login exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged in") {
		t.Fatalf("login stdout = %q", stdout)
	}

	// 2. `kodema whoami` works without any flags or env vars and prints explicit scope.
	code, stdout, stderr = runEntry(t, []string{"whoami"}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("auto-discovered whoami exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "scope\tproject (project-local)") {
		t.Fatalf("whoami stdout = %q; want explicit project scope", stdout)
	}

	// 3. `kodema inbox` auto-discovers project-local without --project or KODEMA_PROJECT.
	code, _, stderr = runEntry(t, []string{"inbox"}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("auto-discovered inbox exit = %d; stderr=%q", code, stderr)
	}
	if gotInboxProject != "project-local" {
		t.Fatalf("inbox project = %q; want project-local", gotInboxProject)
	}

	// 4. Explicit --project overrides auto-discovered project.
	code, _, stderr = runEntry(t, []string{"--project", "project-override", "inbox"}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("override inbox exit = %d; stderr=%q", code, stderr)
	}
	if gotInboxProject != "project-override" {
		t.Fatalf("override inbox project = %q; want project-override", gotInboxProject)
	}

	// 5. `kodema --trusted-origin <url> git credential get` works without --server or --ca-file.
	code, stdout, stderr = runEntry(t, []string{
		"--trusted-origin", apiServer.URL,
		"git", "credential", "get", apiServer.URL + "/git/demo.git",
	}, deps)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("auto-discovered git credential = %d / stderr=%q", code, stderr)
	}
	if want := "username=kodema\npassword=" + loginAccess + "\n"; stdout != want {
		t.Fatalf("git credential stdout = %q; want %q", stdout, want)
	}
}

// TestKodemaGitCredentialSessionAPIURLFallbackAndTLSDiagnostic pins that
// `kodema --trusted-origin <url> --session <file> git credential get` works
// without --server (falling back to the session's api_url) and writes an
// actionable TLS/CA diagnostic to stderr instead of failing silently when CA
// trust is missing.
func TestKodemaGitCredentialSessionAPIURLFallbackAndTLSDiagnostic(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	document := api.DiscoveryDocument{}
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == api.DiscoveryPath {
			writeJSON(writer, document)
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(apiServer.Close)
	apiServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	provider.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	document = provider.document(apiServer.URL)
	seedRecord(t, sessionPath, recordFor(document, time.Now().Add(time.Hour)))
	caPath := writeCertBundlePEM(t, provider.server, apiServer)

	// Without --server (and no default instance), --ca-file + --session works via session api_url.
	code, stdout, stderr := runEntry(t, []string{
		"--ca-file", caPath,
		"--trusted-origin", apiServer.URL,
		"--session", sessionPath,
		"git", "credential", "get", apiServer.URL + "/git/repo.git",
	}, defaultCLIDeps())
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("git credential with session api_url = %d / stderr=%q", code, stderr)
	}
	if want := "username=kodema\npassword=" + loginAccess + "\n"; stdout != want {
		t.Fatalf("stdout = %q; want %q", stdout, want)
	}

	// Without --ca-file against self-signed TLS, exits 0 with empty stdout and logs CA hint to stderr.
	code, stdout, stderr = runEntry(t, []string{
		"--trusted-origin", apiServer.URL,
		"--session", sessionPath,
		"git", "credential", "get", apiServer.URL + "/git/repo.git",
	}, defaultCLIDeps())
	if code != cli.ExitSuccess {
		t.Fatalf("missing-CA git credential exit = %d; want 0", code)
	}
	if stdout != "" {
		t.Fatalf("missing-CA git credential stdout = %q; want empty", stdout)
	}
	for _, want := range []string{"kodema git credential:", "TLS", "--ca-file"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("missing-CA git credential stderr = %q; want %q", stderr, want)
		}
	}
	assertNoSecrets(t, []string{stdout, stderr}, loginAccess, loginRefresh)
}

// TestKodemaLoginTightensSessionDirPermissionsAndWarnsOnShadowingEnv pins that
// `kodema login --session <0755-dir>/persona.json` tightens the parent dir to
// 0700 so subsequent commands immediately succeed, refuses shared sticky dirs
// up front, and warns on stderr if KODEMA_CREDENTIAL_FILE shadows the session.
func TestKodemaLoginTightensSessionDirPermissionsAndWarnsOnShadowingEnv(t *testing.T) {
	isolateUserConfig(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}
	apiServer := newPublicServer(t, "task.read", &document, recorder)
	document = provider.document(apiServer.URL)

	deps := defaultCLIDeps()
	deps.oidc = &cli.HTTPOIDCClient{RoundTripper: provider.client().Transport}
	deps.clock = fixedClock{now: time.Now()}

	// Existing 0755 parent directory: login tightens to 0700 so next command succeeds.
	looseDir := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(looseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(looseDir, "eng.json")

	deps.environment = mapEnvironment(map[string]string{
		"KODEMA_CREDENTIAL_FILE": "/tmp/admin-token",
	})
	code, stdout, stderr := runEntry(t, []string{
		"--server", apiServer.URL,
		"--session", sessionPath,
		"login", "--device",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("login into 0755 dir exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged in") {
		t.Fatalf("login stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "KODEMA_CREDENTIAL_FILE") {
		t.Fatalf("login stderr = %q; want KODEMA_CREDENTIAL_FILE shadowing warning", stderr)
	}
	info, err := os.Lstat(looseDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("session parent dir mode after login = %v, err=%v; want 0700", info.Mode().Perm(), err)
	}

	// Next command reads the session immediately without any 0700 failure.
	deps.environment = mapEnvironment(nil)
	code, _, stderr = runEntry(t, []string{
		"--server", apiServer.URL,
		"--session", sessionPath,
		"call", "task.read", `--input={"task_id":"task-1"}`,
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("subsequent call after login exit = %d; stderr=%q", code, stderr)
	}
	if recorder.count() != 1 || recorder.authorization(0) != "Bearer "+loginAccess {
		t.Fatalf("recorder = %d / %q", recorder.count(), recorder.authorization(0))
	}

	// Direct session in a shared sticky directory (e.g. /tmp or sticky temp dir) is refused up front.
	stickyDir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(stickyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stickyDir, 0o755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runEntry(t, []string{
		"--server", apiServer.URL,
		"--session", filepath.Join(stickyDir, "eng.json"),
		"login", "--device",
	}, deps)
	if code != cli.ExitFailure || stdout != "" {
		t.Fatalf("login into sticky dir = %d / stdout=%q; want failure", code, stdout)
	}
	if !strings.Contains(stderr, "0700") {
		t.Fatalf("sticky dir login stderr = %q; want 0700 hint", stderr)
	}
}
