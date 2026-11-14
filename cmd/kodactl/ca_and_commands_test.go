package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
)

func writeCertBundlePEM(t *testing.T, servers ...*httptest.Server) string {
	t.Helper()
	var bundle bytes.Buffer
	for _, srv := range servers {
		cert := srv.Certificate()
		if cert == nil {
			t.Fatal("TLS server has no certificate")
		}
		if err := pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			t.Fatalf("pem.Encode: %v", err)
		}
	}
	caPath := filepath.Join(t.TempDir(), "ca-bundle.pem")
	if err := os.WriteFile(caPath, bundle.Bytes(), 0o600); err != nil {
		t.Fatalf("WriteFile CA bundle: %v", err)
	}
	return caPath
}

// TestKodactlCAFileTrustsBothOIDCAndAPITransports drives the real OIDC login,
// refresh, logout, REST call, and MCP stdio bridge over real TLS servers
// WITHOUT injecting fake HTTP transports, proving --ca-file and KODAMA_CA_FILE
// wire RootCAs into both transports while keeping InsecureSkipVerify false.
func TestKodactlCAFileTrustsBothOIDCAndAPITransports(t *testing.T) {
	isolateUserConfig(t)
	_, sessionPath := sessionPaths(t)
	provider := newFakeProvider(t)
	recorder := &resultRecorder{}
	document := api.DiscoveryDocument{}

	var mcpAuth string
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case api.DiscoveryPath:
			recorder.recordDiscovery()
			writeJSON(writer, document)
		case "/mcp":
			body, _ := io.ReadAll(request.Body)
			_ = request.Body.Close()
			mcpAuth = request.Header.Get("Authorization")
			if strings.Contains(string(body), `"notifications/`) {
				writer.WriteHeader(http.StatusAccepted)
				return
			}
			writer.Header().Set("Content-Type", api.MediaType)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
		default:
			recorder.record(request)
			payload, _ := json.Marshal(api.Result{Operation: "task.read", Body: json.RawMessage(`{"task_id":"task-1"}`)})
			writer.Header().Set("Content-Type", api.MediaType)
			writer.WriteHeader(api.HTTPStatusSuccess)
			_, _ = writer.Write(payload)
		}
	}))
	t.Cleanup(apiServer.Close)
	apiServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	provider.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	document = provider.document(apiServer.URL)

	caPath := writeCertBundlePEM(t, provider.server, apiServer)

	// Verify transport invariants: InsecureSkipVerify is never enabled.
	pool, err := loadCABundleFile(caPath)
	if err != nil || pool == nil {
		t.Fatalf("loadCABundleFile: %v", err)
	}
	for name, rt := range map[string]http.RoundTripper{
		"oidc": newOIDCTransportWithRootCAs(pool),
		"api":  newAPITransport(pool),
	} {
		tr, ok := rt.(*http.Transport)
		if !ok || tr.TLSClientConfig == nil {
			t.Fatalf("%s transport missing TLSClientConfig", name)
		}
		if tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("%s transport set InsecureSkipVerify = true", name)
		}
		if tr.TLSClientConfig.RootCAs == nil {
			t.Fatalf("%s transport did not wire RootCAs", name)
		}
	}

	base := time.Now().Truncate(time.Second)
	deps := defaultCLIDeps()
	deps.clock = fixedClock{now: base}
	deps.environment = mapEnvironment(nil)

	// 1. Without --ca-file, TLS login against self-signed server MUST fail.
	failCode, _, _ := runEntry(t, []string{
		"--server", apiServer.URL,
		"--session", sessionPath,
		"login", "--device",
	}, deps)
	if failCode == cli.ExitSuccess {
		t.Fatal("login succeeded against untrusted TLS server without --ca-file")
	}

	// 2. With --ca-file, device login trusts BOTH apiServer (discovery) and provider (OIDC).
	code, stdout, stderr := runEntry(t, []string{
		"--ca-file", caPath,
		"--url", apiServer.URL,
		"--session", sessionPath,
		"login", "--device",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("login with --ca-file exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged in") {
		t.Fatalf("login stdout = %q", stdout)
	}

	// 3. With KODAMA_CA_FILE env var, REST call trusts TLS server.
	t.Setenv("KODAMA_CA_FILE", caPath)
	code, _, stderr = runEntry(t, []string{
		"--url", apiServer.URL,
		"--session", sessionPath,
		"call", "task.read", `--input={"task_id":"task-1"}`,
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("REST call with KODAMA_CA_FILE exit = %d; stderr=%q", code, stderr)
	}
	if recorder.count() != 1 || recorder.authorization(0) != "Bearer "+loginAccess {
		t.Fatalf("REST recorder = %d / %q", recorder.count(), recorder.authorization(0))
	}

	// 4. Seed expiring session and run `mcp stdio`: auto-refreshes OIDC over TLS
	// and forwards MCP over TLS, ignoring 202 notification responses.
	seedRecord(t, sessionPath, recordFor(document, base.Add(20*time.Second)))
	mcpDeps := deps
	mcpDeps.stdin = strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n",
	)
	code, mcpStdout, mcpStderr := runEntry(t, []string{
		"--url", apiServer.URL,
		"--session", sessionPath,
		"mcp", "stdio",
	}, mcpDeps)
	if code != cli.ExitSuccess {
		t.Fatalf("mcp stdio exit = %d; stderr=%q", code, mcpStderr)
	}
	if mcpStdout != "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n" {
		t.Fatalf("mcp stdio stdout = %q", mcpStdout)
	}
	if mcpAuth != "Bearer "+freshAccess {
		t.Fatalf("mcp Authorization = %q; want refreshed token", mcpAuth)
	}

	// 5. Logout over TLS with KODAMA_CA_FILE.
	code, _, stderr = runEntry(t, []string{
		"--url", apiServer.URL,
		"--session", sessionPath,
		"logout",
	}, deps)
	if code != cli.ExitSuccess {
		t.Fatalf("logout exit = %d; stderr=%q", code, stderr)
	}
}

// TestKodactlRefusesInvalidCAFileBeforeNetwork pins ExitUsage on missing,
// directory, empty, or malformed --ca-file / KODAMA_CA_FILE.
func TestKodactlRefusesInvalidCAFileBeforeNetwork(t *testing.T) {
	isolateUserConfig(t)
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(emptyPath, nil, 0o600)
	badPath := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(badPath, []byte("SECRET-MATERIAL-NOT-A-CERT"), 0o600)

	cases := []struct {
		name string
		path string
	}{
		{"missing file", filepath.Join(dir, "missing.pem")},
		{"directory", dir},
		{"empty file", emptyPath},
		{"invalid PEM", badPath},
		{"blank flag", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			deps := cliDeps{runCLI: func(context.Context, cli.Config, []string, cli.Ports, cli.CommandDeps) int {
				called = true
				return 0
			}}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{
				"--ca-file", tc.path,
				"--server", "https://kodama.example.com",
				"call", "task.read",
			}, &stdout, &stderr, deps)
			if code != cli.ExitUsage {
				t.Fatalf("exit = %d; want %d (stderr=%q)", code, cli.ExitUsage, stderr.String())
			}
			if called {
				t.Fatal("invalid --ca-file reached CLI execution")
			}
			if strings.Contains(stderr.String(), "SECRET-MATERIAL-NOT-A-CERT") {
				t.Fatal("stderr leaked file contents")
			}
		})
	}
}

// TestKodactlHelpFlagsAndZeroArgSummary pins Requirement 1 at the binary entrypoint.
func TestKodactlHelpFlagsAndZeroArgSummary(t *testing.T) {
	isolateUserConfig(t)
	for _, argv := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"help", "logs"},
		{"logs", "--help"},
		{"mcp", "install", "-h"},
		{"version", "--help"},
	} {
		code, stdout, stderr := runEntry(t, argv, defaultCLIDeps())
		if code != cli.ExitSuccess {
			t.Fatalf("%v exit = %d; stderr=%q", argv, code, stderr)
		}
		if !strings.Contains(stdout, "Usage:") || stderr != "" {
			t.Fatalf("%v stdout=%q stderr=%q", argv, stdout, stderr)
		}
	}

	code, stdout, stderr := runEntry(t, nil, defaultCLIDeps())
	if code != cli.ExitUsage {
		t.Fatalf("zero-arg exit = %d; want %d", code, cli.ExitUsage)
	}
	if stdout != "" {
		t.Fatalf("zero-arg stdout = %q; want empty", stdout)
	}
	if !strings.Contains(stderr, "Commands:") || !strings.Contains(stderr, "mcp") {
		t.Fatalf("zero-arg stderr = %q; want command summary", stderr)
	}
}
