package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/installation"
	shellapp "github.com/optimiware/kodama/internal/shell/app"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// testProfileFixture is one complete `kodama server --profile test`
// environment: TLS material, an owner-only keyfile, a listening Podman API
// socket and sentinel database credentials, so every refusal can be scanned.
type testProfileFixture struct {
	values  map[string]string
	options serverOptions
	keyfile string
	socket  string
	tlsDir  string
}

func (fixture *testProfileFixture) getenv(name string) string { return fixture.values[name] }

func newTestProfileFixture(t *testing.T) *testProfileFixture {
	t.Helper()
	tlsDir := t.TempDir()
	selfSigned(t, tlsDir)
	keyDir := t.TempDir()
	keyfile := filepath.Join(keyDir, "kek-v1.key")
	key := make([]byte, secret.DataKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyfile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "podman.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on the fake Podman socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return &testProfileFixture{
		keyfile: keyfile,
		socket:  socket,
		tlsDir:  tlsDir,
		values: map[string]string{
			"KODAMA_DATABASE_URL":            "postgres://kodama_runtime:SENTINEL-RUNTIME-PW@db.test:5432/kodama?sslmode=disable",
			"KODAMA_CONTROLLER_DATABASE_URL": "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@db.test:5432/kodama?sslmode=disable",
			"KODAMA_PUBLISHER_DATABASE_URL":  "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@db.test:5432/kodama?sslmode=disable",
			"KODAMA_AUTH_DATABASE_URL":       "postgres://kodama_auth:SENTINEL-AUTH-PW@db.test:5432/kodama?sslmode=disable",
			"KODAMA_NATS_URL":                "nats://nats.test:4222",
			"KODAMA_PODMAN_SOCKET":           socket,
			"KODAMA_KEY_ADAPTER":             "keyfile",
			"KODAMA_KEY_REF":                 "keyfile:kek-v1",
			"KODAMA_KEYFILE":                 keyfile,
			"KODAMA_TLS_DIR":                 tlsDir,
			"KODAMA_OIDC_ISSUER":             "https://idp.example.com",
			"KODAMA_OIDC_AUDIENCE":           "https://kodama.test",
			"KODAMA_OIDC_JWKS_URL":           "https://idp.example.com/jwks",
			"KODAMA_OIDC_TENANT_CLAIM":       "org_id",
		},
		options: serverOptions{
			Profile:        "test",
			Address:        "10.20.30.40:8443",
			DataDir:        t.TempDir(),
			InstallationID: "installation-test-1",
			PublicURL:      "https://kodama.test:8443",
			WorkloadURL:    "https://host.containers.internal:8443",
			MCPPath:        "/mcp",
		},
	}
}

// unexportedField reads one unexported field for inspection only.
func unexportedField(t *testing.T, value any, name string) reflect.Value {
	t.Helper()
	current := reflect.ValueOf(value)
	for current.Kind() == reflect.Pointer || current.Kind() == reflect.Interface {
		if current.IsNil() {
			t.Fatalf("nil value while reading %q", name)
		}
		current = current.Elem()
	}
	field := current.FieldByName(name)
	if !field.IsValid() {
		t.Fatalf("%s has no field %q", current.Type(), name)
	}
	return field
}

func assertTestProfileSecretFree(t *testing.T, text string) {
	t.Helper()
	for _, sentinel := range []string{"SENTINEL", "PW@"} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("diagnostic rendered a secret (%s): %q", sentinel, text)
		}
	}
}

// TestBuildTestProfileWiresGitStorageRoot proves the durable single-host test
// profile passes the same mounted keeper root as production.
func TestBuildTestProfileWiresGitStorageRoot(t *testing.T) {
	fixture := newTestProfileFixture(t)
	root := t.TempDir()
	fixture.values["KODAMA_GIT_STORAGE_ROOT"] = root
	build, err := buildTestProfile(fixture.options, fixture.getenv)
	if err != nil {
		t.Fatalf("buildTestProfile: %v", err)
	}
	if build.Config.GitStorageRoot != root {
		t.Fatalf("GitStorageRoot = %q; want %q", build.Config.GitStorageRoot, root)
	}
}

func TestBuildTestProfileConfiguration(t *testing.T) {
	t.Run("assembles the durable test composition", func(t *testing.T) {
		fixture := newTestProfileFixture(t)
		build, err := buildTestProfile(fixture.options, fixture.getenv)
		if err != nil {
			t.Fatalf("buildTestProfile: %v", err)
		}
		config := build.Config
		if config.Profile != shellapp.ProfileTest || config.Dev != nil || config.Production != nil || config.Adapters != nil {
			t.Fatalf("composition = profile %q dev %v production %v adapters %v; want the durable test form only",
				config.Profile, config.Dev != nil, config.Production != nil, config.Adapters != nil)
		}
		want := installation.Config{
			Environment: installation.EnvironmentTest,
			State:       installation.StatePostgres,
			Messaging:   installation.MessagingNATS,
			Runtime:     installation.RuntimePodman,
			Keys:        installation.KeyKeyfile,
			DatabaseURL: fixture.values["KODAMA_DATABASE_URL"],
			NATSURL:     fixture.values["KODAMA_NATS_URL"],
			KeyRef:      "keyfile:kek-v1",
			PublicURL:   "https://kodama.test:8443",
		}
		if !reflect.DeepEqual(config.Installation, want) {
			t.Fatalf("installation = %+v; want %+v", config.Installation, want)
		}
		if config.InstallationID != "installation-test-1" || config.DataDir != fixture.options.DataDir || config.MCPPath != "/mcp" {
			t.Fatalf("identity/data/mcp = %q %q %q", config.InstallationID, config.DataDir, config.MCPPath)
		}
		if config.LoopInterval != time.Second || config.DefaultActionTimeout != 5*time.Minute {
			t.Fatalf("loop/action timeout = %s/%s; want 1s/5m", config.LoopInterval, config.DefaultActionTimeout)
		}
		test := config.Test
		if test == nil {
			t.Fatal("no durable test configuration")
		}
		if test.ControllerURL != fixture.values["KODAMA_CONTROLLER_DATABASE_URL"] ||
			test.PublisherURL != fixture.values["KODAMA_PUBLISHER_DATABASE_URL"] ||
			test.AuthURL != fixture.values["KODAMA_AUTH_DATABASE_URL"] {
			t.Fatal("a role database URL is not mapped to its own field")
		}
		if test.KeyfilePath != fixture.keyfile {
			t.Fatalf("keyfile = %q; want %q", test.KeyfilePath, fixture.keyfile)
		}
		if test.OIDC.Issuer != "https://idp.example.com" || test.OIDC.Audience != "https://kodama.test" ||
			test.OIDC.JWKSURL != "https://idp.example.com/jwks" || test.OIDC.TenantClaim != "org_id" ||
			test.OIDC.ClockSkew != time.Minute || test.OIDC.CacheTTL != 10*time.Minute {
			t.Fatalf("OIDC = %+v", test.OIDC)
		}
		if test.NATSTLS != nil {
			t.Fatalf("broker TLS = %+v; want none without configured files", test.NATSTLS)
		}
		if test.Registry.Transport == nil || test.RuntimeReady == nil || test.RuntimeCloser == nil {
			t.Fatal("registry transport, runtime readiness or runtime closer is missing")
		}
		adapter, ok := test.Runtime.(*runtimepodman.Adapter)
		if !ok || any(test.Workspace) != any(adapter) {
			t.Fatalf("runtime/workspace = %T/%T; want one Podman adapter", test.Runtime, test.Workspace)
		}
		if got := unexportedField(t, adapter, "kodamaURL").String(); got != fixture.options.WorkloadURL {
			t.Fatalf("workload KODAMA_URL = %q; want the explicit workload URL %q", got, fixture.options.WorkloadURL)
		}
		if got := unexportedField(t, adapter, "root").String(); got != filepath.Join(fixture.options.DataDir, "runtime") {
			t.Fatalf("runtime root = %q; want <data-dir>/runtime", got)
		}
		if _, ok := test.VolumeProvisioner.(*podmandriver.Libpod); !ok {
			t.Fatalf("volume provisioner = %T; want the Podman API client", test.VolumeProvisioner)
		}
		if build.TLS == nil || build.TLS.MinVersion != tls.VersionTLS12 || len(build.TLS.Certificates) != 1 {
			t.Fatalf("server TLS = %+v; want the configured certificate at TLS 1.2+", build.TLS)
		}
	})

	t.Run("broker TLS files are optional but complete", func(t *testing.T) {
		fixture := newTestProfileFixture(t)
		fixture.values["KODAMA_NATS_URL"] = "tls://nats.test:4222"
		fixture.values["KODAMA_NATS_CA_FILE"] = filepath.Join(fixture.tlsDir, "ca.crt")
		build, err := buildTestProfile(fixture.options, fixture.getenv)
		if err != nil {
			t.Fatalf("CA-only broker TLS: %v", err)
		}
		if build.Config.Test.NATSTLS == nil || build.Config.Test.NATSTLS.CAFile != fixture.values["KODAMA_NATS_CA_FILE"] {
			t.Fatalf("broker TLS = %+v", build.Config.Test.NATSTLS)
		}
		fixture.values["KODAMA_NATS_CERT_FILE"] = filepath.Join(fixture.tlsDir, "tls.crt")
		fixture.values["KODAMA_NATS_KEY_FILE"] = filepath.Join(fixture.tlsDir, "tls.key")
		if _, err := buildTestProfile(fixture.options, fixture.getenv); err != nil {
			t.Fatalf("complete broker TLS: %v", err)
		}
	})

	t.Run("discovery is matched against the OIDC verifier", func(t *testing.T) {
		fixture := newTestProfileFixture(t)
		fixture.options.Discovery = discoveryOptions{
			Issuer: "https://idp.example.com", ClientID: "kodactl", Audience: "https://kodama.test",
			AuthorizationEndpoint: "https://idp.example.com/authorize", TokenEndpoint: "https://idp.example.com/token",
			DeviceAuthorizationEndpoint: "https://idp.example.com/device", RevocationEndpoint: "https://idp.example.com/revoke",
			Scopes: []string{"openid"}, APIURL: "https://kodama.test:8443",
		}
		build, err := buildTestProfile(fixture.options, fixture.getenv)
		if err != nil {
			t.Fatalf("matching discovery: %v", err)
		}
		if build.Config.Discovery == nil || build.Config.Discovery.ClientID != "kodactl" {
			t.Fatalf("discovery = %+v", build.Config.Discovery)
		}
		fixture.options.Discovery.Issuer = "https://other.example.com"
		if _, err := buildTestProfile(fixture.options, fixture.getenv); err == nil || !strings.Contains(err.Error(), "issuer") {
			t.Fatalf("mismatched discovery issuer = %v; want a refusal", err)
		}
	})

	for _, name := range []string{
		"KODAMA_DATABASE_URL", "KODAMA_CONTROLLER_DATABASE_URL", "KODAMA_PUBLISHER_DATABASE_URL", "KODAMA_AUTH_DATABASE_URL",
		"KODAMA_NATS_URL", "KODAMA_PODMAN_SOCKET", "KODAMA_KEY_ADAPTER", "KODAMA_KEY_REF", "KODAMA_KEYFILE",
		"KODAMA_TLS_DIR", "KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM",
	} {
		t.Run("requires "+name, func(t *testing.T) {
			fixture := newTestProfileFixture(t)
			delete(fixture.values, name)
			_, err := buildTestProfile(fixture.options, fixture.getenv)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s = %v; want a refusal naming it", name, err)
			}
			assertTestProfileSecretFree(t, err.Error())
		})
	}

	refusals := []struct {
		name   string
		mutate func(*testing.T, *testProfileFixture)
		want   string
	}{
		{"development token", func(_ *testing.T, f *testProfileFixture) { f.options.DevTokens = []string{"dev"} }, "development token"},
		{"default installation id", func(_ *testing.T, f *testProfileFixture) { f.options.InstallationID = "installation-dev" }, "installation id"},
		{"empty installation id", func(_ *testing.T, f *testProfileFixture) { f.options.InstallationID = " " }, "installation id"},
		{"no data dir", func(_ *testing.T, f *testProfileFixture) { f.options.DataDir = "" }, "data directory"},
		{"relative data dir", func(_ *testing.T, f *testProfileFixture) { f.options.DataDir = "data" }, "data directory"},
		{"http public URL", func(_ *testing.T, f *testProfileFixture) { f.options.PublicURL = "http://127.0.0.1:8080" }, "public URL"},
		{"no workload URL", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "" }, "workload URL"},
		{"http workload URL", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "http://host.containers.internal:8443"
		}, "workload URL"},
		{"workload userinfo", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "https://user:SENTINEL@host.containers.internal:8443"
		}, "workload URL"},
		{"workload query", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "https://host.containers.internal:8443?x=1"
		}, "workload URL"},
		{"workload fragment", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "https://host.containers.internal:8443#x"
		}, "workload URL"},
		{"workload path", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "https://host.containers.internal:8443/api"
		}, "workload URL"},
		{"workload bad port", func(_ *testing.T, f *testProfileFixture) {
			f.options.WorkloadURL = "https://host.containers.internal:99999"
		}, "workload URL"},
		{"workload no host", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "https://:8443" }, "workload URL"},
		{"workload loopback", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "https://127.0.0.1:8443" }, "workload URL"},
		{"workload localhost", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "https://localhost:8443" }, "workload URL"},
		{"workload IPv6 loopback", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "https://[::1]:8443" }, "workload URL"},
		{"workload unspecified", func(_ *testing.T, f *testProfileFixture) { f.options.WorkloadURL = "https://0.0.0.0:8443" }, "workload URL"},
		{"host alias with loopback listener", func(_ *testing.T, f *testProfileFixture) { f.options.Address = "127.0.0.1:8443" }, "loopback"},
		{"cloud key adapter", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_KEY_ADAPTER"] = "scw-kms" }, "KODAMA_KEY_ADAPTER"},
		{"foreign key reference", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_KEY_REF"] = "scw-kms:fr-par/key" }, "key"},
		{"empty key name", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_KEY_REF"] = "keyfile:" }, "key"},
		{"key version 2", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_KEY_CURRENT_VERSION"] = "2" }, "KODAMA_KEY_CURRENT_VERSION"},
		{"relative keyfile", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_KEYFILE"] = "kek-v1.key" }, "KODAMA_KEYFILE"},
		{"missing keyfile", func(_ *testing.T, f *testProfileFixture) { _ = os.Remove(f.keyfile) }, "keyfile"},
		{"group-readable keyfile", func(_ *testing.T, f *testProfileFixture) { _ = os.Chmod(f.keyfile, 0o640) }, "keyfile"},
		{"short keyfile", func(t *testing.T, f *testProfileFixture) {
			if err := os.WriteFile(f.keyfile, []byte("short"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "keyfile"},
		{"relative Podman socket", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_PODMAN_SOCKET"] = "podman.sock" }, "KODAMA_PODMAN_SOCKET"},
		{"missing Podman socket", func(t *testing.T, f *testProfileFixture) {
			f.values["KODAMA_PODMAN_SOCKET"] = filepath.Join(t.TempDir(), "absent.sock")
		}, "KODAMA_PODMAN_SOCKET"},
		{"Podman socket is a file", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_PODMAN_SOCKET"] = f.keyfile }, "KODAMA_PODMAN_SOCKET"},
		{"no server certificate", func(t *testing.T, f *testProfileFixture) { f.values["KODAMA_TLS_DIR"] = t.TempDir() }, "certificate"},
		{"broker certificate without key", func(_ *testing.T, f *testProfileFixture) {
			f.values["KODAMA_NATS_CERT_FILE"] = filepath.Join(f.tlsDir, "tls.crt")
		}, "broker"},
		{"unreadable broker CA", func(t *testing.T, f *testProfileFixture) {
			f.values["KODAMA_NATS_CA_FILE"] = filepath.Join(t.TempDir(), "absent.crt")
		}, "broker"},
		{"http NATS", func(_ *testing.T, f *testProfileFixture) { f.values["KODAMA_NATS_URL"] = "http://nats.test:4222" }, "NATS"},
		{"runtime database scheme", func(_ *testing.T, f *testProfileFixture) {
			f.values["KODAMA_DATABASE_URL"] = "mysql://kodama_runtime:SENTINEL-RUNTIME-PW@db.test:5432/kodama"
		}, "KODAMA_DATABASE_URL"},
		{"controller database host list", func(_ *testing.T, f *testProfileFixture) {
			f.values["KODAMA_CONTROLLER_DATABASE_URL"] = "postgres://kodama_controller:SENTINEL-CONTROLLER-PW@a,b:5432/kodama"
		}, "KODAMA_CONTROLLER_DATABASE_URL"},
		{"publisher database elsewhere", func(_ *testing.T, f *testProfileFixture) {
			f.values["KODAMA_PUBLISHER_DATABASE_URL"] = "postgres://kodama_publisher:SENTINEL-PUBLISHER-PW@other.test:5432/kodama?sslmode=disable"
		}, "KODAMA_PUBLISHER_DATABASE_URL"},
		{"auth database other name", func(_ *testing.T, f *testProfileFixture) {
			f.values["KODAMA_AUTH_DATABASE_URL"] = "postgres://kodama_auth:SENTINEL-AUTH-PW@db.test:5432/other?sslmode=disable"
		}, "KODAMA_AUTH_DATABASE_URL"},
		{"unreadable OIDC CA", func(t *testing.T, f *testProfileFixture) {
			f.values["KODAMA_OIDC_CA_FILE"] = filepath.Join(t.TempDir(), "absent.crt")
		}, "OIDC"},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			fixture := newTestProfileFixture(t)
			refusal.mutate(t, fixture)
			before, _ := os.ReadFile(fixture.keyfile)
			_, err := buildTestProfile(fixture.options, fixture.getenv)
			if err == nil {
				t.Fatalf("buildTestProfile accepted %s", refusal.name)
			}
			if !strings.Contains(err.Error(), refusal.want) {
				t.Fatalf("refusal = %q; want it to mention %q", err, refusal.want)
			}
			assertTestProfileSecretFree(t, err.Error())
			after, _ := os.ReadFile(fixture.keyfile)
			if string(before) != string(after) {
				t.Fatal("a refused server configuration changed the keyfile")
			}
		})
	}

	t.Run("a missing keyfile is never initialized by the server", func(t *testing.T) {
		fixture := newTestProfileFixture(t)
		if err := os.Remove(fixture.keyfile); err != nil {
			t.Fatal(err)
		}
		if _, err := buildTestProfile(fixture.options, fixture.getenv); err == nil {
			t.Fatal("the server accepted a missing keyfile")
		}
		if _, err := os.Lstat(fixture.keyfile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the server created a keyfile: %v", err)
		}
	})

	t.Run("workload URL precedence is flags > environment > file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "server.json")
		if err := os.WriteFile(path, []byte(`{"profile":"test","workload_url":"https://file.example:1"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		options, code := parseServerOptions([]string{"--config", path}, io.Discard)
		if code != 0 || options.WorkloadURL != "https://file.example:1" || options.Profile != "test" {
			t.Fatalf("file workload URL = %q (code %d)", options.WorkloadURL, code)
		}
		t.Setenv("KODAMA_WORKLOAD_URL", "https://env.example:2")
		options, _ = parseServerOptions([]string{"--config", path}, io.Discard)
		if options.WorkloadURL != "https://env.example:2" {
			t.Fatalf("environment workload URL = %q", options.WorkloadURL)
		}
		options, _ = parseServerOptions([]string{"--config", path, "--workload-url", "https://flag.example:3"}, io.Discard)
		if options.WorkloadURL != "https://flag.example:3" {
			t.Fatalf("flag workload URL = %q", options.WorkloadURL)
		}
	})
}

// TestRunTestProfileServesTLSAndDrains drives the test profile through the
// real server dispatch: the build seam receives the parsed options, the app
// receives exactly the built configuration, the listener is wrapped with the
// built TLS configuration, cancellation drains with exit 0, and every failure
// exits with its fixed, secret-free diagnostic.
func TestRunTestProfileServesTLSAndDrains(t *testing.T) {
	build, pool := productionServeFixture(t)
	build.Config = shellapp.Config{Profile: shellapp.ProfileTest, InstallationID: "installation-test-1"}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	})

	t.Run("serves TLS and drains", func(t *testing.T) {
		listeners := make(chan net.Listener, 1)
		var configured shellapp.Config
		var built serverOptions
		deps := serverDeps{
			build: func(serverOptions) (productionBuild, error) {
				t.Error("the test profile reached the production build")
				return productionBuild{}, errors.New("wrong build")
			},
			buildTest: func(options serverOptions) (productionBuild, error) { built = options; return build, nil },
			newApp: func(_ context.Context, config shellapp.Config) (serverApp, error) {
				configured = config
				app := servingApp(handler)
				inner := app.serve
				app.serve = func(ctx context.Context, listener net.Listener) error {
					listeners <- listener
					return inner(ctx, listener)
				}
				return app, nil
			},
			listen: func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
		}
		ctx, cancel := context.WithCancel(context.Background())
		var stderr lockedBuffer
		done := make(chan int, 1)
		go func() {
			done <- runServer(ctx, []string{"--profile", "test", "--address", "127.0.0.1:0",
				"--workload-url", "https://host.containers.internal:8443"}, io.Discard, &stderr, deps)
		}()
		var listener net.Listener
		select {
		case listener = <-listeners:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("the test profile never served; stderr=%q", stderr.String())
		}
		if built.WorkloadURL != "https://host.containers.internal:8443" || built.Profile != "test" {
			t.Fatalf("build options = %+v", built)
		}
		if configured.Profile != shellapp.ProfileTest || configured.InstallationID != "installation-test-1" {
			t.Fatalf("app configuration = %+v; want exactly the built configuration", configured)
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "kodama.test", MinVersion: tls.VersionTLS12}}}
		response, err := client.Get("https://" + listener.Addr().String() + "/")
		if err != nil {
			cancel()
			t.Fatalf("TLS request: %v", err)
		}
		_ = response.Body.Close()
		if response.TLS == nil || response.TLS.Version < tls.VersionTLS12 {
			t.Fatalf("TLS state = %+v; want TLS 1.2+", response.TLS)
		}
		client.CloseIdleConnections()
		plain := &http.Client{Timeout: 2 * time.Second}
		if response, err := plain.Get("http://" + listener.Addr().String() + "/"); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				t.Fatal("a plaintext client was served")
			}
		}
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the test profile did not drain")
		}
		if strings.Contains(stderr.String(), "development composition") {
			t.Fatalf("the test profile printed the development banner: %q", stderr.String())
		}
	})

	failures := []struct {
		name   string
		deps   func(*fakeServerApp) serverDeps
		code   int
		want   string
		closed int
	}{
		{"configuration refusal", func(*fakeServerApp) serverDeps {
			return serverDeps{buildTest: func(serverOptions) (productionBuild, error) {
				return productionBuild{}, errors.New("test profile requires KODAMA_KEYFILE")
			}}
		}, 2, "requires KODAMA_KEYFILE", 0},
		{"construction failure", func(*fakeServerApp) serverDeps {
			return serverDeps{
				buildTest: func(serverOptions) (productionBuild, error) { return build, nil },
				newApp: func(context.Context, shellapp.Config) (serverApp, error) {
					return nil, errors.New("dial postgres://kodama_runtime:SENTINEL-PW@db failed")
				},
			}
		}, 1, "composition failed to start", 0},
		{"listen failure", func(app *fakeServerApp) serverDeps {
			return serverDeps{
				buildTest: func(serverOptions) (productionBuild, error) { return build, nil },
				newApp:    func(context.Context, shellapp.Config) (serverApp, error) { return app, nil },
				listen:    func(string, string) (net.Listener, error) { return nil, errors.New("SENTINEL bind") },
			}
		}, 1, "listen", 1},
		{"serve failure", func(app *fakeServerApp) serverDeps {
			app.serve = func(context.Context, net.Listener) error { return errors.New("SENTINEL serve") }
			return serverDeps{
				buildTest: func(serverOptions) (productionBuild, error) { return build, nil },
				newApp:    func(context.Context, shellapp.Config) (serverApp, error) { return app, nil },
				listen:    func(network, _ string) (net.Listener, error) { return net.Listen(network, "127.0.0.1:0") },
			}
		}, 1, "server failed", 0},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			app := &fakeServerApp{handler: handler}
			var stderr lockedBuffer
			code := runServer(context.Background(), []string{"--profile", "test", "--address", "127.0.0.1:0"}, io.Discard, &stderr, failure.deps(app))
			if code != failure.code {
				t.Fatalf("exit code = %d; want %d (stderr=%q)", code, failure.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), failure.want) {
				t.Fatalf("stderr = %q; want %q", stderr.String(), failure.want)
			}
			assertTestProfileSecretFree(t, stderr.String())
			if app.closed != failure.closed {
				t.Fatalf("app closed %d times; want %d", app.closed, failure.closed)
			}
		})
	}
}

// TestProductionRefusesTestEntrySettings pins that the test entry never
// weakens production: test-only settings never route an explicitly production
// server to the test builder, and the production key loader keeps refusing the
// keyfile adapter.
func TestProductionRefusesTestEntrySettings(t *testing.T) {
	t.Setenv("KODAMA_WORKLOAD_URL", "https://host.containers.internal:8443")
	t.Setenv("KODAMA_KEYFILE", "/var/lib/kodama/keys/kek-v1.key")
	t.Setenv("KODAMA_PODMAN_SOCKET", "/run/podman/podman.sock")
	t.Setenv("KODAMA_KEY_ADAPTER", "keyfile")
	productionBuilt := false
	deps := serverDeps{
		build: func(serverOptions) (productionBuild, error) {
			productionBuilt = true
			return productionBuild{}, errors.New("production supports KODAMA_KEY_ADAPTER=scw-kms only")
		},
		buildTest: func(serverOptions) (productionBuild, error) {
			t.Error("an explicitly production server reached the test builder")
			return productionBuild{}, errors.New("wrong build")
		},
	}
	var stderr lockedBuffer
	if code := runServer(context.Background(), []string{"--profile", "production"}, io.Discard, &stderr, deps); code != 2 || !productionBuilt {
		t.Fatalf("production with test settings: code %d, production build %v", code, productionBuilt)
	}

	if _, err := loadProductionKeys(func(name string) string {
		return map[string]string{"KODAMA_KEY_ADAPTER": "keyfile", "KODAMA_KEY_REF": "keyfile:kek-v1", "KODAMA_KEYFILE": "/k"}[name]
	}); err == nil || !strings.Contains(err.Error(), "scw-kms") {
		t.Fatalf("production key loader with the keyfile adapter = %v; want the scw-kms refusal", err)
	}

	for _, args := range [][]string{
		{"--profile", "production", "--init-keyfile", "--tenant", "tenant-a", "--tenant-name", "A", "--token-file", "/tmp/t"},
		{"--init-keyfile", "--tenant", "tenant-a", "--tenant-name", "A", "--token-file", "/tmp/t"},
		{"--profile", "development", "--tenant", "tenant-a", "--tenant-name", "A", "--token-file", "/tmp/t"},
		{"--profile", "staging", "--tenant", "tenant-a", "--tenant-name", "A", "--token-file", "/tmp/t"},
	} {
		t.Setenv("KODAMA_PROFILE", "")
		var stdout, stderr lockedBuffer
		if code := runBootstrapAdminEntry(context.Background(), args, &stdout, &stderr); code != 2 {
			t.Fatalf("bootstrap %v exit = %d; want 2 (stderr=%q)", args, code, stderr.String())
		}
		if stdout.String() != "" {
			t.Fatalf("a refused bootstrap wrote stdout: %q", stdout.String())
		}
	}
}

func TestBuildTestProfileWiresWorkloadCAAndWorkloadURL(t *testing.T) {
	fixture := newTestProfileFixture(t)
	build, err := buildTestProfile(fixture.options, fixture.getenv)
	if err != nil {
		t.Fatalf("buildTestProfile: %v", err)
	}
	if build.Config.WorkloadURL != fixture.options.WorkloadURL {
		t.Fatalf("WorkloadURL = %q; want %q", build.Config.WorkloadURL, fixture.options.WorkloadURL)
	}
	adapter, ok := build.Config.Test.Runtime.(*runtimepodman.Adapter)
	if !ok {
		t.Fatalf("Runtime = %T; want *runtimepodman.Adapter", build.Config.Test.Runtime)
	}
	if got := unexportedField(t, adapter, "workloadCAPEM").Len(); got == 0 {
		t.Fatal("buildTestProfile did not wire <KODAMA_TLS_DIR>/ca.crt into the Podman runtime adapter")
	}

	badFixture := newTestProfileFixture(t)
	badFixture.options.WorkloadCAFile = "relative/ca.crt"
	if _, err := buildTestProfile(badFixture.options, badFixture.getenv); err == nil || !strings.Contains(err.Error(), "workload CA") {
		t.Fatalf("relative WorkloadCAFile = %v; want workload CA refusal", err)
	}
}
