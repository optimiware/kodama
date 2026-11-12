package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	shellapp "github.com/optimiware/kodama/internal/shell/app"
)

type fakeServerApp struct {
	handler http.Handler
	serve   func(context.Context, net.Listener) error
	closed  int
}

func (app *fakeServerApp) Handler() http.Handler { return app.handler }
func (app *fakeServerApp) Serve(ctx context.Context, listener net.Listener) error {
	return app.serve(ctx, listener)
}
func (app *fakeServerApp) Close(context.Context) error {
	app.closed++
	return nil
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestServerRefusesProductionWithoutItsConfiguration(t *testing.T) {
	called := false
	deps := serverDeps{
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			called = true
			return nil, nil
		},
		listen: func(string, string) (net.Listener, error) { return nil, nil },
	}
	var stderr lockedBuffer
	code := runServer(context.Background(), []string{"--profile", "production"}, &lockedBuffer{}, &stderr, deps)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if called {
		t.Fatal("the production refusal constructed a weaker composition")
	}
	if !strings.Contains(stderr.String(), "production requires KODAMA_DATABASE_URL") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("production refusal printed the development banner: %q", stderr.String())
	}
}

func TestServerDevBannerAndGracefulJoin(t *testing.T) {
	joined := make(chan struct{})
	started := make(chan shellapp.Config, 1)
	deps := serverDeps{
		newApp: func(_ context.Context, config shellapp.Config) (serverApp, error) {
			started <- config
			return &fakeServerApp{
				handler: http.NewServeMux(),
				serve: func(ctx context.Context, listener net.Listener) error {
					defer listener.Close()
					<-ctx.Done()
					close(joined)
					return nil
				},
			}, nil
		},
		listen: net.Listen,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- runServer(ctx, []string{
			"--profile", "development",
			"--data-dir", t.TempDir(),
			"--dev-token", "dev-token",
			"--address", "127.0.0.1:0",
		}, &stdout, &stderr, deps)
	}()
	var config shellapp.Config
	select {
	case config = <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the server composition was never constructed")
	}
	if config.Profile != shellapp.ProfileDevelopment {
		t.Fatalf("profile = %q", config.Profile)
	}
	if len(config.Dev.Tokens) != 1 || config.Dev.Tokens[0].Credential != "dev-token" {
		t.Fatalf("dev tokens = %+v", config.Dev.Tokens)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not shut down")
	}
	select {
	case <-joined:
	default:
		t.Fatal("the server returned before its serving goroutine joined")
	}
	if !strings.Contains(stderr.String(), "WARNING: development composition only") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestServerGitStorageRootSources pins the operator-facing input for the
// retained-commit keeper: the environment, the configuration file and the flag
// all name the same setting, with flags winning over the file.
func TestServerGitStorageRootSources(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(configPath, []byte(`{"git_storage_root": "/from/file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	options, code := parseServerOptions([]string{"--config", configPath}, io.Discard)
	if code != 0 {
		t.Fatalf("parseServerOptions(config) exited %d", code)
	}
	if options.GitStorageRoot != "/from/file" {
		t.Fatalf("config-file GitStorageRoot = %q; want /from/file", options.GitStorageRoot)
	}

	options, code = parseServerOptions([]string{"--config", configPath, "--git-storage-root", "/from/flag"}, io.Discard)
	if code != 0 {
		t.Fatalf("parseServerOptions(flag) exited %d", code)
	}
	if options.GitStorageRoot != "/from/flag" {
		t.Fatalf("flag GitStorageRoot = %q; want /from/flag", options.GitStorageRoot)
	}

	// The environment is the production setting and wins over the file.
	root := t.TempDir()
	t.Setenv("KODAMA_GIT_STORAGE_ROOT", root)
	options, code = parseServerOptions([]string{"--config", configPath}, io.Discard)
	if code != 0 {
		t.Fatalf("parseServerOptions(environment) exited %d", code)
	}
	if options.GitStorageRoot != root {
		t.Fatalf("environment GitStorageRoot = %q; want %q", options.GitStorageRoot, root)
	}
}

// TestServerDevelopmentRefusesGitStorageRoot proves the development profile
// does not silently ignore a configured keeper root: it has no durable retained
// commit source, so the partial configuration is a named refusal.
func TestServerDevelopmentRefusesGitStorageRoot(t *testing.T) {
	options := defaultServerOptions()
	options.Profile = "development"
	options.DataDir = t.TempDir()
	options.DevTokens = []string{"dev-token"}
	options.GitStorageRoot = t.TempDir()
	_, err := options.config()
	if !errors.Is(err, shellapp.ErrKeepRefsWithoutRetainedCommits) {
		t.Fatalf("config error = %v; want ErrKeepRefsWithoutRetainedCommits", err)
	}
}

func TestServerRequiresDevToken(t *testing.T) {
	deps := serverDeps{
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			t.Fatal("constructed a composition without a dev token")
			return nil, nil
		},
		listen: func(string, string) (net.Listener, error) { return nil, nil },
	}
	var stderr lockedBuffer
	code := runServer(context.Background(), []string{"--profile", "development", "--data-dir", t.TempDir()}, &lockedBuffer{}, &stderr, deps)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "dev-token") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestServerReadsConfigFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "server.json")
	if err := os.WriteFile(path, []byte(`{
		"profile": "development",
		"data_dir": "`+directory+`",
		"dev_tokens": ["file-token"],
		"dev_principal": "admin-9",
		"dev_tenant": "tenant-9",
		"dev_project": "project-9",
		"dev_roles": "admin",
		"dev_expiry": "1h"
	}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	started := make(chan shellapp.Config, 1)
	deps := serverDeps{
		newApp: func(_ context.Context, config shellapp.Config) (serverApp, error) {
			started <- config
			return &fakeServerApp{
				handler: http.NewServeMux(),
				serve: func(ctx context.Context, listener net.Listener) error {
					listener.Close()
					<-ctx.Done()
					return nil
				},
			}, nil
		},
		listen: net.Listen,
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- runServer(ctx, []string{"--config", path, "--address", "127.0.0.1:0"}, &lockedBuffer{}, &stderr, deps)
	}()
	var config shellapp.Config
	select {
	case config = <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the config-file composition was never constructed")
	}
	if config.InstallationID != "installation-dev" {
		t.Fatalf("installation id = %q", config.InstallationID)
	}
	if len(config.Dev.Tokens) != 1 || config.Dev.Tokens[0].Credential != "file-token" {
		t.Fatalf("dev tokens = %+v", config.Dev.Tokens)
	}
	if config.Dev.Tokens[0].Principal.ID != "admin-9" {
		t.Fatalf("principal = %+v", config.Dev.Tokens[0].Principal)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not shut down")
	}
}

func TestServerRejectsUnknownProfileAndFlags(t *testing.T) {
	deps := serverDeps{
		newApp: func(context.Context, shellapp.Config) (serverApp, error) {
			t.Fatal("constructed an unknown profile")
			return nil, nil
		},
		listen: func(string, string) (net.Listener, error) { return nil, nil },
	}
	for _, args := range [][]string{
		{"--profile", "staging", "--data-dir", t.TempDir(), "--dev-token", "t"},
		{"--profile", "development", "--nope"},
	} {
		var stderr lockedBuffer
		if code := runServer(context.Background(), args, &lockedBuffer{}, &stderr, deps); code != 2 {
			t.Fatalf("args %v exit code = %d; want 2", args, code)
		}
	}
}

// TestApplyServerEnvMapsEverySettingToItsOwnField gives each server environment
// setting a distinct sentinel and asserts it reaches its own option. A swapped
// variable or a hard-coded default would otherwise make the production command
// read an unrelated operator value.
func TestApplyServerEnvMapsEverySettingToItsOwnField(t *testing.T) {
	values := map[string]string{
		"KODAMA_PROFILE":                                 "production",
		"KODAMA_ADDRESS":                                 "address-sentinel:8443",
		"KODAMA_DATA_DIR":                                "/data-dir-sentinel",
		"KODAMA_INSTALLATION_ID":                         "installation-sentinel",
		"KODAMA_PUBLIC_URL":                              "https://public-url-sentinel.example",
		"KODAMA_MCP_PATH":                                "/mcp-sentinel",
		"KODAMA_DEV_PRINCIPAL":                           "principal-sentinel",
		"KODAMA_DEV_TENANT":                              "tenant-sentinel",
		"KODAMA_DEV_PROJECT":                             "project-sentinel",
		"KODAMA_DEV_ROLES":                               "admin",
		"KODAMA_DEV_EXPIRY":                              "45m",
		"KODAMA_DISCOVERY_ISSUER":                        "https://discovery-issuer-sentinel.example",
		"KODAMA_DISCOVERY_CLIENT_ID":                     "client-id-sentinel",
		"KODAMA_DISCOVERY_AUDIENCE":                      "discovery-audience-sentinel",
		"KODAMA_DISCOVERY_AUTHORIZATION_ENDPOINT":        "https://discovery-authorize-sentinel.example/authorize",
		"KODAMA_DISCOVERY_TOKEN_ENDPOINT":                "https://discovery-token-sentinel.example/token",
		"KODAMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT": "https://discovery-device-sentinel.example/device",
		"KODAMA_DISCOVERY_REVOCATION_ENDPOINT":           "https://discovery-revoke-sentinel.example/revoke",
		"KODAMA_DISCOVERY_API_URL":                       "https://discovery-api-sentinel.example",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	t.Setenv("KODAMA_DISCOVERY_SCOPES", "scope-a scope-b")

	options, err := applyServerEnv(defaultServerOptions())
	if err != nil {
		t.Fatalf("applyServerEnv: %v", err)
	}
	for _, testCase := range []struct{ name, got, want string }{
		{"profile", options.Profile, "production"},
		{"address", options.Address, "address-sentinel:8443"},
		{"data directory", options.DataDir, "/data-dir-sentinel"},
		{"installation id", options.InstallationID, "installation-sentinel"},
		{"public URL", options.PublicURL, "https://public-url-sentinel.example"},
		{"MCP path", options.MCPPath, "/mcp-sentinel"},
		{"development principal", options.DevPrincipal, "principal-sentinel"},
		{"development tenant", options.DevTenant, "tenant-sentinel"},
		{"development project", options.DevProject, "project-sentinel"},
		{"development roles", options.DevRoles, "admin"},
		{"discovery issuer", options.Discovery.Issuer, "https://discovery-issuer-sentinel.example"},
		{"discovery client id", options.Discovery.ClientID, "client-id-sentinel"},
		{"discovery audience", options.Discovery.Audience, "discovery-audience-sentinel"},
		{"discovery authorization endpoint", options.Discovery.AuthorizationEndpoint, "https://discovery-authorize-sentinel.example/authorize"},
		{"discovery token endpoint", options.Discovery.TokenEndpoint, "https://discovery-token-sentinel.example/token"},
		{"discovery device endpoint", options.Discovery.DeviceAuthorizationEndpoint, "https://discovery-device-sentinel.example/device"},
		{"discovery revocation endpoint", options.Discovery.RevocationEndpoint, "https://discovery-revoke-sentinel.example/revoke"},
		{"discovery API URL", options.Discovery.APIURL, "https://discovery-api-sentinel.example"},
	} {
		if testCase.got != testCase.want {
			t.Fatalf("%s = %q; want %q", testCase.name, testCase.got, testCase.want)
		}
	}
	if options.DevExpiry != 45*time.Minute {
		t.Fatalf("development expiry = %s; want 45m", options.DevExpiry)
	}
	if len(options.Discovery.Scopes) != 2 || options.Discovery.Scopes[0] != "scope-a" || options.Discovery.Scopes[1] != "scope-b" {
		t.Fatalf("discovery scopes = %v; want the environment's two scopes", options.Discovery.Scopes)
	}
}
