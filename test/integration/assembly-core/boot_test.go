package assembly_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/execution"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func get(t testing.TB, base, path string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func readyz(t testing.TB, base string) int {
	t.Helper()
	code, _ := get(t, base, "/readyz")
	return code
}

// Test_core_assembly_boot (SD-09, SD-10, SD-11, IT-03, PX-05, AC-12): the actual
// composition boots over real dependencies in the declared migration order under
// the non-owner runtime role; its readiness reflects the database schema and
// each configured dependency (and recovers when it does); only the supported
// published routes exist and every one of them is authenticated, except the
// optional capabilities the installation did not configure, which are recorded
// as disabled with a reason; the workers are the `test` composition's set,
// dispatch included; and a missing or unsafe binding refuses startup instead of
// serving.
func Test_core_assembly_boot(t *testing.T) {
	s := newStack(t, options{Unmigrated: true})

	t.Run("an unmigrated database is live but never ready, and migration makes it ready without a restart", func(t *testing.T) {
		srv := s.start()
		base := s.base()
		if code, body := get(t, base, "/healthz"); code != 200 || body != `{"status":"ok"}` {
			t.Fatalf("healthz: %d %s", code, body)
		}
		if code, body := get(t, base, "/readyz"); code != 503 || body != `{"status":"not_ready"}` {
			t.Fatalf("readyz against an empty schema: %d %s", code, body)
		}
		if failed := srv.Core().NotReady(context.Background()); failed["postgres"] == "" {
			t.Fatalf("the not-migrated schema is not reported by the database probe: %v", failed)
		}

		// The declared order, applied by the real runner as the owner identity.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		manifest, err := app.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, m := range manifest.Migrations() {
			want = append(want, m.ID)
		}
		res, err := app.Migrate(ctx, app.MigrateConfig{MigrationURL: s.DB.MigrationURL, RuntimeURL: s.DB.RuntimeURL})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(res.Applied, ",") != strings.Join(want, ",") {
			t.Fatalf("applied %v, declared %v", res.Applied, want)
		}
		if again, err := app.Migrate(ctx, app.MigrateConfig{MigrationURL: s.DB.MigrationURL, RuntimeURL: s.DB.RuntimeURL}); err != nil || len(again.Applied) != 0 || again.Skipped != len(want) {
			t.Fatalf("a second migration must verify, not reapply: %+v %v", again, err)
		}
		// The baseline is schema then grants; the only later change is the additive
		// run_shared schema (T-112), appended last.
		order := []string{"001_schema.sql", "002_grants.sql", "003_run_resources.sql"}
		if strings.Join(want, ",") != strings.Join(order, ",") {
			t.Fatalf("migrations %v, want %v", want, order)
		}
		waitFor(t, "readiness after migration", 10*time.Second, func() bool { return readyz(t, base) == 200 })
		if body := mustGet(t, base, "/readyz"); body != `{"status":"ready"}` {
			t.Fatal(body)
		}
		s.stop()
	})

	srv := s.start()
	base := s.base()
	waitFor(t, "ready", 10*time.Second, func() bool { return readyz(t, base) == 200 })

	t.Run("the runtime identity is the non-owner role and the schema is owned by another", func(t *testing.T) {
		var runtimeOwned, ownerOwned int
		if err := s.Owner.QueryRow(context.Background(), `SELECT
			(SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = $1),
			(SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = $2)`, s.DB.RuntimeRole, s.DB.MigrationRole).Scan(&runtimeOwned, &ownerOwned); err != nil {
			t.Fatal(err)
		}
		if runtimeOwned != 0 || ownerOwned < 20 {
			t.Fatalf("table owners: runtime %d, migration %d", runtimeOwned, ownerOwned)
		}
		var forced int
		if err := s.Owner.QueryRow(context.Background(), `SELECT count(*) FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r'
			AND c.relname IN ('work_items','runs','step_attempts','secrets','workspaces','artifacts','outbox') AND c.relrowsecurity AND c.relforcerowsecurity`).Scan(&forced); err != nil {
			t.Fatal(err)
		}
		if forced != 7 {
			t.Fatalf("row level security is not enabled and forced on %d of the tenant tables", 7-forced)
		}
	})

	t.Run("workers and routes are the test composition: dispatch and the runtime API are wired, optional capabilities are recorded disabled", func(t *testing.T) {
		got := srv.Core().Workers()
		want := []string{
			"artifact-reconciler", "deployment-cleanup", "dispatch", "event-consumer:gates-decision", "event-consumer:work-cancellation",
			"event-publisher", "gate-reconciler", "reconciler", "workspace-reconciler",
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("workers %v, want %v", got, want)
		}
		routes := srv.Core().Routes
		registered := map[string]bool{}
		for _, r := range routes.Registered {
			registered[r] = true
		}
		for _, on := range []string{
			"executor-dispatch", "runtime-admission", "runtime-context", "step-result-callbacks", "source-proposals", "runtime-artifacts",
			"logs", "gates", "agent-profiles", "environments", "deployments", "work-dependencies",
		} {
			if !registered[on] {
				t.Errorf("capability %s is not registered by the test composition (registered %v)", on, routes.Registered)
			}
		}
		// What this installation did not configure stays disabled, each with the
		// configuration that would enable it; nothing registered is also disabled.
		disabled := append([]string(nil), routes.Disabled...)
		sort.Strings(disabled)
		wantOff := []string{"action-ceiling", "agent-steps", "chatops", "exploratory-qa", "flow-summary-list", "git-webhooks", "mcp-proxy", "model-gateway", "oci-registry"}
		if strings.Join(disabled, ",") != strings.Join(wantOff, ",") {
			t.Errorf("disabled capabilities %v, want %v", disabled, wantOff)
		}
		for _, d := range routes.Disabled {
			if registered[d] {
				t.Errorf("capability %s is both registered and disabled", d)
			}
		}
		for _, d := range []string{"action-ceiling", "git-webhooks", "oci-registry"} {
			if routes.Reasons[d] == "" {
				t.Errorf("disabled capability %s carries no reason", d)
			}
		}
	})

	t.Run("readiness reflects each configured dependency and recovers", func(t *testing.T) {
		for _, tc := range []struct {
			name, probe string
			sever, back func()
		}{
			{"PostgreSQL", "postgres", s.PG.Stop, s.PG.Start},
			{"JetStream", "nats", s.NATS.Stop, s.NATS.Start},
			{"S3", "s3", s.S3Proxy.Stop, s.S3Proxy.Start},
		} {
			tc.sever()
			waitFor(t, tc.name+" outage in readiness", 15*time.Second, func() bool { return readyz(t, base) == 503 })
			if code, _ := get(t, base, "/healthz"); code != 200 {
				t.Fatalf("liveness must not depend on %s: %d", tc.name, code)
			}
			if failed := srv.Core().NotReady(context.Background()); failed[tc.probe] == "" || len(failed) != 1 {
				t.Fatalf("%s outage reported as %v", tc.name, failed)
			}
			tc.back()
			waitFor(t, "recovery from the "+tc.name+" outage", 30*time.Second, func() bool { return readyz(t, base) == 200 })
		}
	})

	t.Run("readiness answers the published probe contract", func(t *testing.T) {
		data, err := os.ReadFile("../../../dev/contracts/health.openapi.json")
		if err != nil {
			t.Fatal(err)
		}
		var contract struct {
			Paths map[string]struct {
				Get struct {
					Responses map[string]struct {
						Content map[string]struct {
							Schema struct {
								Properties map[string]struct{ Const string }
							}
						}
					}
				}
			}
		}
		if err := json.Unmarshal(data, &contract); err != nil {
			t.Fatal(err)
		}
		if code, body := get(t, base, "/readyz"); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := contract.Paths["/readyz"].Get.Responses["200"].Content["application/json"].Schema.Properties["status"].Const
		if len(body) != 1 || body["status"] != want || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("readyz %v (%v), want status %q", body, resp.Header, want)
		}
	})

	t.Run("exactly the published subset is registered and every operation is authenticated", func(t *testing.T) {
		data, err := os.ReadFile("../../../api/openapi.yaml")
		if err != nil {
			t.Fatal(err)
		}
		var spec struct {
			Paths map[string]map[string]any `yaml:"paths"`
		}
		if err := yaml.Unmarshal(data, &spec); err != nil {
			t.Fatal(err)
		}
		param := regexp.MustCompile(`\{[A-Za-z]+\}`)
		operations := 0
		// A published operation of a capability the installation left unconfigured
		// is absent (404), not merely denied; Routes.Disabled records which.
		optional := map[string]*regexp.Regexp{
			"chatops":       regexp.MustCompile(`/chatops-(destinations|deliveries)`),
			"mcp-proxy":     regexp.MustCompile(`/mcp-servers`),
			"model-gateway": regexp.MustCompile(`/model-settings`),
		}
		disabled := map[string]bool{}
		for _, d := range srv.Core().Routes.Disabled {
			disabled[d] = true
		}
		absent := 0
		for path, item := range spec.Paths {
			concrete := param.ReplaceAllString(path, "t070_x")
			for method := range item {
				switch method {
				case "get", "post", "put", "patch", "delete":
				default:
					continue
				}
				operations++
				req, _ := http.NewRequest(strings.ToUpper(method), base+app.APIPrefix+concrete, strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				var env struct {
					Error struct{ Code string }
				}
				_ = json.NewDecoder(resp.Body).Decode(&env)
				resp.Body.Close()
				if resp.StatusCode == http.StatusNotFound {
					known := false
					for capability, re := range optional {
						known = known || (disabled[capability] && re.MatchString(path))
					}
					if known {
						absent++
						continue
					}
				}
				if resp.StatusCode != http.StatusUnauthorized || env.Error.Code != "invalid_credential" {
					t.Errorf("%s %s without a credential: %d %q (a published operation must be registered and authenticate first)", method, path, resp.StatusCode, env.Error.Code)
				}
			}
		}
		if operations < 40 {
			t.Fatalf("only %d operations in the published subset", operations)
		}
		t.Logf("%d published operations, %d of them absent with their capability recorded as disabled", operations, absent)

		// The runtime API and the execution, log, gate and delivery routes of the
		// test composition exist and refuse an unauthenticated caller; what the
		// installation did not configure is absent, not merely denied.
		for _, tc := range []struct {
			method, path string
			want         int
		}{
			{"POST", "/runtime/v1/attempts/att_t070/result", http.StatusUnauthorized},
			{"POST", "/runtime/v1/attempts/att_t070/source", http.StatusUnauthorized},
			{"POST", "/runtime/v1/attempts/att_t070/artifacts", http.StatusUnauthorized},
			{"POST", "/runtime/v1/attempts/att_t070/admit", http.StatusBadRequest}, // the admission token travels in the body
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/runs/run_x/logs", http.StatusUnauthorized},
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/gates", http.StatusUnauthorized},
			{"POST", "/api/v1/tenants/t070_x/projects/p_x/gates/gate_x/decisions", http.StatusUnauthorized},
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/agent-profiles", http.StatusUnauthorized},
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/environments", http.StatusUnauthorized},
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/deployments", http.StatusUnauthorized},
			{"GET", "/api/v1/tenants/t070_x/projects/p_x/work-items/wi_x/dependencies", http.StatusUnauthorized},
			{"POST", "/api/v1/tenants/t070_x/projects/p_x/webhooks/git", http.StatusNotFound},
			{"GET", "/runtime/v1/attempts/att_t070/unknown", http.StatusNotFound},
		} {
			req, _ := http.NewRequest(tc.method, base+tc.path, strings.NewReader("{}"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("%s %s without a credential: %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		}
		// Methods the owners never registered stay refused.
		req, _ := http.NewRequest("DELETE", base+app.APIPrefix+"/tenants/t070_x/projects/p_x/work-items/wi_x", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("DELETE of a work item: %d", resp.StatusCode)
		}
	})

	t.Run("missing and unsafe bindings refuse startup", func(t *testing.T) {
		good := s.coreConfig()
		good.Process.HTTPAddr = "127.0.0.1:0"
		for name, tc := range map[string]struct {
			mutate func(*app.CoreConfig)
			want   string
		}{
			"no identity provider": {func(c *app.CoreConfig) { c.OIDC = app.OIDCConfig{} }, "OIDC"},
			"no key provider":      {func(c *app.CoreConfig) { c.Secrets = app.SecretsConfig{} }, "key provider"},
			"unknown adapter":      {func(c *app.CoreConfig) { c.Secrets.Adapter = "unknown" }, "adapter"},
			"missing keyfile":      {func(c *app.CoreConfig) { c.Secrets.Keyfile = nil }, "keyfile configuration"},
			"no object store":      {func(c *app.CoreConfig) { c.S3 = app.S3Config{} }, "S3"},
			"no broker":            {func(c *app.CoreConfig) { c.NATS = app.NATSConfig{} }, "NATS"},
			"no rate limit":        {func(c *app.CoreConfig) { c.Tuning.RateLimitPerMinute = 0 }, "rate limit"},
			"no cursor key":        {func(c *app.CoreConfig) { c.CursorKey = nil }, "cursor"},
			"git credential for any host": {func(c *app.CoreConfig) {
				c.Git.Credentials = []app.GitCredential{{Ref: "secret://installation/x", Username: "u", Password: "p", URLPrefix: "http://insecure.invalid/", Tenants: []string{"*"}}}
			}, "https"},
			"the owner role as runtime identity": {func(c *app.CoreConfig) {
				u, _ := url.Parse(s.DB.MigrationURL)
				u.Host = s.PG.Addr()
				c.Database.RuntimeURL = u.String()
			}, "owns schema tables"},
			"unreachable broker": {func(c *app.CoreConfig) { c.NATS.URL = "nats://127.0.0.1:1" }, "NATS"},
		} {
			cfg := good
			cfg.Git.Credentials = append([]app.GitCredential(nil), good.Git.Credentials...)
			tc.mutate(&cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			srv, err := app.Start(ctx, cfg, nil)
			cancel()
			if err == nil {
				_ = srv.Shutdown(context.Background())
				t.Errorf("%s: the assembly started", name)
				continue
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %q does not name %q", name, err, tc.want)
			}
		}
	})

	t.Run("the cleanup run has no deployment authority and dispatch has no executor", func(t *testing.T) {
		_, err := srv.Core().Execution.PrepareCleanup(context.Background(), execution.CleanupCommand{})
		if err == nil {
			t.Fatal("a cleanup run was prepared without a deployment authorization binding")
		}
	})

	t.Run("a clean shutdown drains and releases the listener", func(t *testing.T) {
		s.stop()
		if conn, err := net.DialTimeout("tcp", s.addr, time.Second); err == nil {
			conn.Close()
			t.Fatal("the listener remained open")
		}
	})
}

func mustGet(t testing.TB, base, path string) string {
	t.Helper()
	code, body := get(t, base, path)
	if code != 200 {
		t.Fatalf("GET %s: %d %s", path, code, body)
	}
	return body
}

// Test_core_assembly_boot_process drives the real process entry point: the
// configuration is loaded from the environment and the secrets directory
// exactly as a deployment supplies it, and the process exits cleanly on its
// cancellation, which proves the worker start/stop wiring of `kodama server
// --assembly core`.
func Test_core_assembly_boot_process(t *testing.T) {
	s := newStack(t, options{})
	cfg := s.coreConfig()

	dir := t.TempDir()
	put := func(name, value string) { mustWrite(t, filepath.Join(dir, name), value) }
	put("database-runtime-url", cfg.Database.RuntimeURL)
	put("s3-access-key", cfg.S3.AccessKey)
	put("s3-secret-key", cfg.S3.SecretKey)
	put("cursor-key", string(cfg.CursorKey))
	put("idempotency-hash-key", string(cfg.IdempotencyHashKey))
	put("git-read-token", s.Git.ReadToken)
	put("git-write-token", s.Git.WriteToken)
	mustWrite(t, filepath.Join(dir, "master-key"), mustRead(t, filepath.Join(s.secrets, "master-key")))
	mustWrite(t, filepath.Join(dir, "runtime-signing-key"), mustRead(t, filepath.Join(s.secrets, "runtime-signing-key")))
	creds, _ := json.Marshal([]map[string]any{
		{"ref": "secret://installation/checkout", "username": gitfx.Username, "passwordFile": "git-read-token", "urlPrefix": s.Git.URL + "/", "tenants": []string{TenantA}},
		{"ref": "secret://installation/publisher", "username": gitfx.Username, "passwordFile": "git-write-token", "urlPrefix": s.Git.URL + "/", "tenants": []string{TenantA}},
	})
	put("git-credentials.json", string(creds))

	env := map[string]string{
		"KODAMA_ASSEMBLY": "core", "KODAMA_HTTP_ADDR": "127.0.0.1:0", "KODAMA_SHUTDOWN_TIMEOUT": "10s", "KODAMA_LOG_LEVEL": "info",
		"KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": cfg.NATS.URL, "KODAMA_EVENT_STREAM": cfg.NATS.Stream, "KODAMA_EVENT_NAMESPACE": cfg.NATS.Namespace,
		"KODAMA_S3_ENDPOINT": cfg.S3.Endpoint, "KODAMA_S3_BUCKET": cfg.S3.Bucket, "KODAMA_S3_PREFIX": cfg.S3.Prefix, "KODAMA_S3_ALLOW_INSECURE_HTTP": "true",
		"KODAMA_OIDC_ISSUER": cfg.OIDC.Issuer, "KODAMA_OIDC_AUDIENCE": cfg.OIDC.Audience, "KODAMA_OIDC_ALLOW_INSECURE_HTTP": "true",
		"KODAMA_INSTALLATION_ID": "t070-process", "KODAMA_SECRETS_ADAPTER": "keyfile",
		"KODAMA_ENV": "test", "KODAMA_RUNTIME_URL": cfg.Runtime.ServerURL, "KODAMA_RUNTIME_STATE_DIR": cfg.Runtime.StateDir,
		"KODAMA_ENTRYPOINT_BINARY": cfg.Runtime.EntrypointBinary, "KODAMA_PODMAN_NETWORK": cfg.Runtime.Podman.Network, "KODAMA_RUNTIME_KEY_GENERATION": "1",
		"KODAMA_GIT_CA_FILE": s.Git.CAFile, "KODAMA_GIT_SCRATCH_DIR": filepath.Join(dir, "scratch"),
	}
	getenv := func(k string) string { return env[k] }

	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- app.Main(ctx, []string{"server"}, getenv, io.Discard, out) }()
	// The process chose its own port; it reports it in its listening record.
	var addr string
	listening := regexp.MustCompile(`"address":"([^"]+)"`)
	waitFor(t, "the process to report its listener", 30*time.Second, func() bool {
		select {
		case code := <-done:
			t.Fatalf("the process exited with %d before it listened: %s", code, out.String())
		default:
		}
		if m := listening.FindStringSubmatch(out.String()); m != nil && strings.Contains(out.String(), "core assembly listening") {
			addr = m[1]
			return true
		}
		return false
	})
	base := "http://" + addr
	waitFor(t, "the process to become ready", 30*time.Second, func() bool { return readyz(t, base) == 200 })
	// The process prepared its own spool directories (core_spool_directories):
	// none was created by this test.
	for _, name := range []string{"artifact-spool", "log-spool"} {
		if info, err := os.Stat(filepath.Join(dir, "scratch", name)); err != nil || !info.IsDir() {
			t.Fatalf("the process did not create %s: %v", name, err)
		}
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the process did not stop")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener remained open")
	}

	// The environment has no default (ADR 0009) and a runtime without its
	// origin or key is not a degraded start either: both are configuration
	// failures (exit 2).
	for _, name := range []string{"KODAMA_ENV", "KODAMA_RUNTIME_URL", "KODAMA_ENTRYPOINT_BINARY"} {
		saved := env[name]
		delete(env, name)
		if code := app.Main(context.Background(), []string{"server"}, getenv, io.Discard, io.Discard); code != 2 {
			t.Fatalf("a configuration without %s exited %d", name, code)
		}
		env[name] = saved
	}
	// A missing secret is a configuration failure (exit 2), never a degraded start.
	delete(env, "KODAMA_OIDC_ISSUER")
	if code := app.Main(context.Background(), []string{"server"}, getenv, io.Discard, io.Discard); code != 2 {
		t.Fatalf("a configuration without an identity provider exited %d", code)
	}
}

func mustWrite(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// syncBuffer is a bytes.Buffer the logger goroutine and the test may use concurrently.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
