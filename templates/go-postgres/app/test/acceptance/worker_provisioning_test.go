package acceptance

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"example.invalid/inventory/internal/app"
	"example.invalid/inventory/internal/config"
	"example.invalid/inventory/internal/tenancy"
)

// TestWorkerAwaitsProvisioning covers a fresh environment: the delivery step
// migrates it and provisioning follows, so a worker deployed with the
// application starts before its service identity holds a membership. It must
// wait (and stay observable) rather than crash-loop, work as soon as the
// membership exists, and still refuse identities that are never provisioned.
func TestWorkerAwaitsProvisioning(t *testing.T) {
	s := newSystem(t)
	id := s.createException(tenantA, "rita", "A/LATE")
	if code, _ := s.resolve(tenantA, "rita", id, "key-late-0001", 1, 7); code != 202 {
		t.Fatalf("resolve: %d", code)
	}
	tokenFile := writeFile(t, s.dir, "late-erp-token", "erp-token-a\n")
	cfg := config.Worker{
		Database: config.Database{URLFile: s.runtimeURLFile(), MaxConns: 4},
		Tenant:   tenantA, ServiceID: "svc-late-worker",
		ERP:          config.ERP{BaseURL: s.erp.URL(), TimeoutSeconds: 5, AllowInsecure: true, Scopes: config.RequiredERPScopes, CredentialFiles: map[string]string{tenantA: tokenFile}},
		PollInterval: config.Duration(100 * time.Millisecond), Lease: config.Duration(time.Second), MaxAttempts: 6,
		RetryBase: config.Duration(200 * time.Millisecond), RetryMax: config.Duration(time.Second),
	}

	// Strict construction still refuses the unprovisioned identity.
	if _, err := app.NewWorker(context.Background(), cfg, s.log); !errors.Is(err, tenancy.ErrNotMember) {
		t.Fatalf("NewWorker must refuse an unprovisioned identity: %v", err)
	}
	// A wait that ends without provisioning returns the refusal: nothing runs.
	if _, err := app.AwaitWorker(context.Background(), cfg, s.log, 300*time.Millisecond, 50*time.Millisecond); !errors.Is(err, tenancy.ErrNotMember) {
		t.Fatalf("an identity that is never provisioned must not start: %v", err)
	}
	// A configuration problem is not retried.
	bad := cfg
	bad.ERP.CredentialFiles = map[string]string{tenantA: filepath.Join(s.dir, "missing-token")}
	begin := time.Now()
	if _, err := app.AwaitWorker(context.Background(), bad, s.log, time.Minute, time.Second); err == nil || time.Since(begin) > 500*time.Millisecond {
		t.Fatalf("a bad credential must fail immediately: %v after %s", err, time.Since(begin))
	}

	type result struct {
		w   *app.Worker
		err error
	}
	started := make(chan result, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		w, err := app.AwaitWorker(ctx, cfg, s.log, time.Minute, 50*time.Millisecond)
		started <- result{w, err}
	}()
	select {
	case r := <-started:
		t.Fatalf("the worker started without a membership: %v", r.err)
	case <-time.After(400 * time.Millisecond):
	}
	if st, _ := s.state(tenantA, "rita", id); st != "pending" {
		t.Fatalf("nothing may process the intent before provisioning: %s", st)
	}

	// Provisioning grants the worker membership (an owner-identity operation).
	mig := config.Migrate{Database: config.Database{URLFile: filepath.Join(s.dir, "owner-url"), MaxConns: 2}, RuntimeRole: s.db.RuntimeRole}
	seed := app.SeedFile{TenantID: tenantA, Name: "Acme"}
	seed.Members = append(seed.Members, struct {
		Subject string `json:"subject"`
		Role    string `json:"role"`
	}{"svc-late-worker", "worker"})
	if err := app.Seed(context.Background(), mig, seed); err != nil {
		t.Fatal(err)
	}
	var w *app.Worker
	select {
	case r := <-started:
		if r.err != nil {
			t.Fatal(r.err)
		}
		w = r.w
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not start after provisioning")
	}
	go func() { _ = w.Run(ctx) }()
	s.eventually("the late worker to resolve the intent", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
}
