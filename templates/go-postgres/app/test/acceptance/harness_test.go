// Package acceptance runs the application's scenario tests against its real
// constructors: real PostgreSQL (migrations, RLS, runtime role), the real HTTP
// server and worker built by internal/app, the controlled ERP peer and an
// in-process OIDC provider. Nothing here reaches a software factory.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.invalid/inventory/internal/app"
	"example.invalid/inventory/internal/config"
	"example.invalid/inventory/test/support/erpfake"
	"example.invalid/inventory/test/support/oidcfake"
	"example.invalid/inventory/test/support/pgtest"
)

const (
	audience = "https://inventory.example.invalid/api"
	tenantA  = "11111111-1111-4111-8111-111111111111"
	tenantB  = "22222222-2222-4222-8222-222222222222"
)

type system struct {
	t      *testing.T
	dir    string
	db     *pgtest.Database
	idp    *oidcfake.Provider
	erp    *erpfake.Server
	api    *app.API
	base   string
	log    *slog.Logger
	cancel context.CancelFunc
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeJSON(t *testing.T, dir, name string, v any) string {
	t.Helper()
	b, _ := json.Marshal(v)
	return writeFile(t, dir, name, string(b))
}

// newSystem migrates a fresh database, seeds two tenants and starts the API.
func newSystem(t *testing.T) *system {
	t.Helper()
	// The application never needs a factory; make any accidental dependency fail loudly.
	t.Setenv("KODAMA_SERVER_URL", "http://127.0.0.1:1")
	t.Setenv("KODAMA_RUNTIME_URL", "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	s := &system{t: t, dir: t.TempDir(), cancel: cancel, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	t.Cleanup(cancel)
	s.db = pgtest.New(t)
	s.idp = oidcfake.New(t)
	s.erp = erpfake.New(t, map[string]string{tenantA: "erp-token-a", tenantB: "erp-token-b"})

	ownerURL := writeFile(t, s.dir, "owner-url", s.db.MigrationURL)
	mig := config.Migrate{Database: config.Database{URLFile: ownerURL, MaxConns: 2}, RuntimeRole: s.db.RuntimeRole}
	if err := app.Migrate(ctx, mig, s.log); err != nil {
		t.Fatal(err)
	}
	seed := func(tenant, name string, members ...[2]string) {
		f := app.SeedFile{TenantID: tenant, Name: name}
		for _, m := range members {
			f.Members = append(f.Members, struct {
				Subject string `json:"subject"`
				Role    string `json:"role"`
			}{m[0], m[1]})
		}
		if err := app.Seed(ctx, mig, f); err != nil {
			t.Fatal(err)
		}
	}
	seed(tenantA, "Acme", [2]string{"rita", "reviewer"}, [2]string{"vic", "viewer"}, [2]string{"svc-worker-a", "worker"})
	seed(tenantB, "Globex", [2]string{"rita", "viewer"}, [2]string{"bea", "admin"}, [2]string{"svc-worker-b", "worker"})

	s.startAPI(ctx)
	return s
}

func (s *system) runtimeURLFile() string {
	return writeFile(s.t, s.dir, "runtime-url", s.db.RuntimeURL)
}

func (s *system) startAPI(ctx context.Context) {
	s.t.Helper()
	cfg := config.API{
		HTTPAddr: "127.0.0.1:0", Database: config.Database{URLFile: s.runtimeURLFile(), MaxConns: 8},
		OIDC:            config.OIDC{Issuer: s.idp.Issuer(), Audience: audience},
		CursorKeyFile:   writeFile(s.t, s.dir, "cursor-key", "0123456789abcdef0123456789abcdef-cursor"),
		ShutdownTimeout: config.Duration(2 * time.Second),
	}
	api, err := app.NewAPI(ctx, cfg, s.log)
	if err != nil {
		s.t.Fatal(err)
	}
	s.api, s.base = api, "http://"+api.Addr()
	go func() { _ = api.Run(ctx) }()
}

// worker starts a worker for one tenant and returns its stop function.
func (s *system) worker(tenant, serviceID string) (stop func(), err error) {
	s.t.Helper()
	tokenFile := writeFile(s.t, s.dir, "erp-token-"+tenant, map[string]string{tenantA: "erp-token-a", tenantB: "erp-token-b"}[tenant]+"\n")
	cfg := config.Worker{
		Database: config.Database{URLFile: s.runtimeURLFile(), MaxConns: 4},
		Tenant:   tenant, ServiceID: serviceID,
		ERP:          config.ERP{BaseURL: s.erp.URL(), TimeoutSeconds: 5, AllowInsecure: true, Scopes: config.RequiredERPScopes, CredentialFiles: map[string]string{tenant: tokenFile}},
		PollInterval: config.Duration(100 * time.Millisecond), Lease: config.Duration(time.Second), MaxAttempts: 6,
		RetryBase: config.Duration(200 * time.Millisecond), RetryMax: config.Duration(time.Second),
	}
	ctx, cancel := context.WithCancel(context.Background())
	w, err := app.NewWorker(ctx, cfg, s.log)
	if err != nil {
		cancel()
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = w.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	s.t.Cleanup(stop)
	return stop, nil
}

func (s *system) token(subject string) string { return s.idp.Token(subject, audience) }

func (s *system) do(method, path, token string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func (s *system) createException(tenant, subject, ref string) string {
	s.t.Helper()
	code, body, _ := s.do("POST", fmt.Sprintf("/api/v1/tenants/%s/exceptions", tenant), s.token(subject), map[string]any{"erpRef": ref, "erpQuantity": 10, "countedQuantity": 7}, nil)
	if code != 201 {
		s.t.Fatalf("create: %d %v", code, body)
	}
	return body["id"].(string)
}

func (s *system) resolve(tenant, subject, id, key string, version int, qty int) (int, map[string]any) {
	s.t.Helper()
	code, body, _ := s.do("POST", fmt.Sprintf("/api/v1/tenants/%s/exceptions/%s/resolution", tenant, id), s.token(subject),
		map[string]any{"adjustedQuantity": qty, "reason": "recount confirmed"}, map[string]string{"Idempotency-Key": key, "If-Match": fmt.Sprintf(`"%d"`, version)})
	return code, body
}

func (s *system) state(tenant, subject, id string) (string, map[string]any) {
	s.t.Helper()
	code, body, _ := s.do("GET", fmt.Sprintf("/api/v1/tenants/%s/exceptions/%s", tenant, id), s.token(subject), nil, nil)
	if code != 200 {
		s.t.Fatalf("get: %d %v", code, body)
	}
	return body["state"].(string), body
}

func (s *system) eventually(what string, d time.Duration, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %s", what)
}
