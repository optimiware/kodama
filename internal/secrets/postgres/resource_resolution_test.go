package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
)

// Test_resource_resolution_audit (VS-14): a run service resolves its pinned
// version through the same decryption, pin check and audit as an attempt, and
// the audit names the resource as the receiver, never an attempt.
func Test_resource_resolution_audit(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const v1 = "service-SENTINEL-value"
	e.createSecret(scopeA1, "db-password", v1)
	if err := e.pin(scopeA1, "run_1", "db-password", 1); err != nil {
		t.Fatal(err)
	}
	r := e.resolver(newAuthority())
	grant := secrets.ResourceGrant{Scope: scopeA1, RunID: "run_1", Key: "service.db", Bindings: []runtimeBinding{binding("db-password", 1)}}
	current := func(context.Context) (secrets.ResourceGrant, error) { return grant, nil }

	resp, err := r.ResolveResource(ctx, current, ref("db-password", 1), "req_svc")
	if err != nil || decode(t, resp) != v1 {
		t.Fatalf("resolution: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve' AND actor_type = 'workload' AND actor_id = 'resource:run_1/service.db'
		AND metadata->>'resourceKey' = 'service.db' AND metadata->>'runId' = 'run_1' AND request_id = 'req_svc'`); n != 1 {
		t.Fatalf("%d audit entries name the service as the receiver", n)
	}
	// The audit never carries the value.
	if n := e.count(`SELECT count(*) FROM audit WHERE metadata::text LIKE '%SENTINEL%'`); n != 0 {
		t.Fatal("a value reached the audit trail")
	}
	// A reference the service did not pin, and another run's pin, are refused before any read.
	if _, err := r.ResolveResource(ctx, current, ref("other", 1), ""); !errors.Is(err, secrets.ErrUndeclared) {
		t.Fatalf("an unpinned reference: %v", err)
	}
	foreign := grant
	foreign.RunID = "run_2"
	if _, err := r.ResolveResource(ctx, func(context.Context) (secrets.ResourceGrant, error) { return foreign, nil }, ref("db-password", 1), ""); !errors.Is(err, secrets.ErrNotPinned) {
		t.Fatalf("another run that never pinned the version: %v", err)
	}
	// After the run releases its pins the service resolves nothing.
	if n := e.releasePins(scopeA1, "run_1"); n != 1 {
		t.Fatalf("released %d pins", n)
	}
	if _, err := r.ResolveResource(ctx, current, ref("db-password", 1), ""); !errors.Is(err, secrets.ErrAttemptInactive) {
		t.Fatalf("a released run: %v", err)
	}
}

// Review finding 6, real SQL: a version an earlier step received stays readable
// for comparison after revocation and after the run released its pins, the read
// is audited as a scan and never as a delivery, and the use path stays closed.
func Test_scan_of_revoked_disclosed_values(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const v1 = "disclosed-then-revoked-SENTINEL"
	sec := e.createSecret(scopeA1, "erp-token", v1)
	if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	src, err := secretspg.NewVersionSource(e.store, e.wrapper, installation)
	if err != nil {
		t.Fatal(err)
	}
	req := secrets.VersionRequest{Scope: scopeA1, RunID: "run_1", SecretName: "erp-token", Version: 1}
	var _ secrets.ScanSource = src

	e.revoke(scopeA1, sec.ID(), 1)
	if _, err := src.Open(ctx, req); !errors.Is(err, secrets.ErrRevoked) {
		t.Fatalf("use of a revoked version: %v", err)
	}
	p, err := src.OpenForScan(ctx, req)
	if err != nil || string(p.Value()) != v1 {
		t.Fatalf("scan of a revoked version: %v", err)
	}
	p.Discard()
	if e.releasePins(scopeA1, "run_1") != 1 {
		t.Fatal("pins not released")
	}
	if _, err := src.Open(ctx, req); !errors.Is(err, secrets.ErrRevoked) && !errors.Is(err, secrets.ErrAttemptInactive) {
		t.Fatalf("use after release: %v", err)
	}
	if p, err := src.OpenForScan(ctx, req); err != nil || string(p.Value()) != v1 {
		t.Fatalf("scan after the run released its pins: %v", err)
	} else {
		p.Discard()
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE action = 'secret.scan' AND actor_type = 'system' AND metadata->>'runId' = 'run_1' AND metadata->>'name' = 'erp-token'`); n != 2 {
		t.Fatalf("%d scan audit entries, want 2", n)
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE action = 'secret.resolve'`); n != 0 {
		t.Fatal("a scan was audited as a delivery")
	}
	if n := e.count(`SELECT count(*) FROM audit WHERE metadata::text LIKE '%SENTINEL%'`); n != 0 {
		t.Fatal("a value reached the audit trail")
	}
	// A run that never pinned the version reads nothing, scan or not.
	other := req
	other.RunID = "run_9"
	if _, err := src.OpenForScan(ctx, other); !errors.Is(err, secrets.ErrNotPinned) {
		t.Fatalf("a run that never pinned it: %v", err)
	}
	// Another tenant's scope reads nothing.
	foreign := req
	foreign.Scope = scopeB1
	if _, err := src.OpenForScan(ctx, foreign); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
}
