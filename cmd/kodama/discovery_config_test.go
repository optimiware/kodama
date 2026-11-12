// Tests for the Shell public-discovery settings on the `kodama server`
// entrypoint: the production configuration shape maps the public document to
// app.DiscoveryConfig, and every invalid document is refused before a
// composition is constructed.
package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	shellapp "github.com/optimiware/kodama/internal/shell/app"
)

// discoveryArguments are one complete valid public discovery document as flags.
func discoveryArguments() []string {
	return []string{
		"--discovery-issuer", "https://issuer.example",
		"--discovery-client-id", "kodactl",
		"--discovery-audience", "kodama-api",
		"--discovery-authorization-endpoint", "https://issuer.example/oauth/authorize",
		"--discovery-token-endpoint", "https://issuer.example/oauth/token",
		"--discovery-device-authorization-endpoint", "https://issuer.example/oauth/device",
		"--discovery-revocation-endpoint", "https://issuer.example/oauth/revoke",
		"--discovery-scope", "openid",
		"--discovery-scope", "profile",
		"--discovery-api-url", "https://api.example",
	}
}

// developmentArguments are the minimal valid development composition flags.
func developmentArguments(t *testing.T) []string {
	t.Helper()
	return []string{"--profile", "development", "--data-dir", t.TempDir(), "--dev-token", "dev-token"}
}

// runServerCapture runs the server entry with a capturing composition factory
// that serves immediately. It returns the exit code, captured config and stderr.
func runServerCapture(t *testing.T, args []string) (int, *shellapp.Config, string) {
	t.Helper()
	var captured *shellapp.Config
	deps := serverDeps{
		newApp: func(_ context.Context, config shellapp.Config) (serverApp, error) {
			captured = &config
			return &fakeServerApp{
				serve: func(context.Context, net.Listener) error { return nil },
			}, nil
		},
		listen: func(string, string) (net.Listener, error) { return nil, nil },
	}
	var stderr lockedBuffer
	code := runServer(context.Background(), args, &lockedBuffer{}, &stderr, deps)
	return code, captured, stderr.String()
}

// assertDiscoveryDocument compares the mapped Shell discovery input.
func assertDiscoveryDocument(t *testing.T, config *shellapp.Config) {
	t.Helper()
	if config == nil || config.Discovery == nil {
		t.Fatalf("the discovery configuration was not mapped: %+v", config)
	}
	discovery := config.Discovery
	if discovery.Issuer != "https://issuer.example" ||
		discovery.ClientID != "kodactl" ||
		discovery.Audience != "kodama-api" ||
		discovery.AuthorizationEndpoint != "https://issuer.example/oauth/authorize" ||
		discovery.TokenEndpoint != "https://issuer.example/oauth/token" ||
		discovery.DeviceAuthorizationEndpoint != "https://issuer.example/oauth/device" ||
		discovery.RevocationEndpoint != "https://issuer.example/oauth/revoke" ||
		discovery.APIURL != "https://api.example" {
		t.Fatalf("mapped discovery = %+v", discovery)
	}
	if len(discovery.Scopes) != 2 || discovery.Scopes[0] != "openid" || discovery.Scopes[1] != "profile" {
		t.Fatalf("mapped discovery scopes = %v", discovery.Scopes)
	}
}

func TestServerMapsDiscoveryFlags(t *testing.T) {
	args := append(developmentArguments(t), discoveryArguments()...)
	code, config, stderr := runServerCapture(t, args)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	assertDiscoveryDocument(t, config)
}

func TestServerMapsDiscoveryEnvironment(t *testing.T) {
	t.Setenv("KODAMA_DISCOVERY_ISSUER", "https://issuer.example")
	t.Setenv("KODAMA_DISCOVERY_CLIENT_ID", "kodactl")
	t.Setenv("KODAMA_DISCOVERY_AUDIENCE", "kodama-api")
	t.Setenv("KODAMA_DISCOVERY_AUTHORIZATION_ENDPOINT", "https://issuer.example/oauth/authorize")
	t.Setenv("KODAMA_DISCOVERY_TOKEN_ENDPOINT", "https://issuer.example/oauth/token")
	t.Setenv("KODAMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT", "https://issuer.example/oauth/device")
	t.Setenv("KODAMA_DISCOVERY_REVOCATION_ENDPOINT", "https://issuer.example/oauth/revoke")
	t.Setenv("KODAMA_DISCOVERY_SCOPES", "openid, profile")
	t.Setenv("KODAMA_DISCOVERY_API_URL", "https://api.example")
	code, config, stderr := runServerCapture(t, developmentArguments(t))
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	assertDiscoveryDocument(t, config)
}

func TestServerMapsDiscoveryConfigFile(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "server.json")
	contents := `{
		"profile": "development",
		"data_dir": "` + directory + `",
		"dev_tokens": ["file-token"],
		"discovery": {
			"issuer": "https://issuer.example",
			"client_id": "kodactl",
			"audience": "kodama-api",
			"authorization_endpoint": "https://issuer.example/oauth/authorize",
			"token_endpoint": "https://issuer.example/oauth/token",
			"device_authorization_endpoint": "https://issuer.example/oauth/device",
			"revocation_endpoint": "https://issuer.example/oauth/revoke",
			"scopes": ["openid", "profile"],
			"api_url": "https://api.example"
		}
	}`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	code, config, stderr := runServerCapture(t, []string{"--config", configPath})
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if config == nil || len(config.Dev.Tokens) != 1 || config.Dev.Tokens[0].Credential != "file-token" {
		t.Fatalf("the config file was not loaded: %+v", config)
	}
	assertDiscoveryDocument(t, config)
}

func TestServerWithoutDiscoveryLeavesItUnset(t *testing.T) {
	code, config, stderr := runServerCapture(t, developmentArguments(t))
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr)
	}
	if config == nil || config.Discovery != nil {
		t.Fatalf("discovery = %+v; want nil", config)
	}
}

func TestServerRejectsInvalidDiscovery(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func([]string) []string
		wantErr string
	}{
		{
			name: "missing issuer",
			mutate: func(args []string) []string {
				return removeFlag(args, "--discovery-issuer")
			},
			wantErr: "discovery issuer is empty",
		},
		{
			name: "missing client id",
			mutate: func(args []string) []string {
				return removeFlag(args, "--discovery-client-id")
			},
			wantErr: "discovery client_id is empty",
		},
		{
			name: "missing scopes",
			mutate: func(args []string) []string {
				return removeFlag(removeFlag(args, "--discovery-scope"), "--discovery-scope")
			},
			wantErr: "discovery scopes are empty",
		},
		{
			name: "foreign token origin",
			mutate: func(args []string) []string {
				return replaceFlag(args, "--discovery-token-endpoint", "https://other.example/oauth/token")
			},
			wantErr: "does not share the issuer origin",
		},
		{
			name: "insecure non-loopback api url",
			mutate: func(args []string) []string {
				return replaceFlag(args, "--discovery-api-url", "http://api.example")
			},
			wantErr: "api_url",
		},
		{
			name: "malformed endpoint",
			mutate: func(args []string) []string {
				return replaceFlag(args, "--discovery-authorization-endpoint", "javascript:alert(1)")
			},
			wantErr: "authorization_endpoint",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			args := append(developmentArguments(t), testCase.mutate(discoveryArguments())...)
			called := false
			deps := serverDeps{
				newApp: func(context.Context, shellapp.Config) (serverApp, error) {
					called = true
					return &fakeServerApp{}, nil
				},
				listen: func(string, string) (net.Listener, error) { return nil, nil },
			}
			var stderr lockedBuffer
			code := runServer(context.Background(), args, &lockedBuffer{}, &stderr, deps)
			if code != 2 {
				t.Fatalf("exit = %d; want a configuration refusal", code)
			}
			if called {
				t.Fatal("an invalid discovery document constructed a composition")
			}
			if !strings.Contains(stderr.String(), "discovery configuration is invalid") ||
				!strings.Contains(stderr.String(), testCase.wantErr) {
				t.Fatalf("stderr = %q; want %q", stderr.String(), testCase.wantErr)
			}
		})
	}
}

// removeFlag removes the first occurrence of one flag and its value.
func removeFlag(args []string, name string) []string {
	for index := range args {
		if args[index] == name {
			return append(append([]string(nil), args[:index]...), args[index+2:]...)
		}
	}
	return args
}

// replaceFlag replaces one flag's value.
func replaceFlag(args []string, name, value string) []string {
	for index := range args {
		if args[index] == name && index+1 < len(args) {
			replaced := append([]string(nil), args...)
			replaced[index+1] = value
			return replaced
		}
	}
	return args
}
