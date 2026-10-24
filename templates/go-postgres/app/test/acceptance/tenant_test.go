package acceptance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"example.invalid/inventory/internal/app"
	"example.invalid/inventory/internal/config"
	"example.invalid/inventory/test/support/oidcfake"
)

// TestBlueprintGoTenant proves application tenancy: verified identity plus
// current application membership, cross-tenant denial, worker isolation, and
// operation with every factory endpoint unreachable.
func TestBlueprintGoTenant(t *testing.T) {
	s := newSystem(t)
	pathA := "/api/v1/tenants/" + tenantA + "/exceptions"
	pathB := "/api/v1/tenants/" + tenantB + "/exceptions"

	t.Run("identity is verified and membership decides access", func(t *testing.T) {
		create := map[string]any{"erpRef": "WH1/SKU-1", "erpQuantity": 10, "countedQuantity": 7}
		if code, _, _ := s.do("POST", pathA, "", create, nil); code != 401 {
			t.Errorf("no token: %d", code)
		}
		if code, _, _ := s.do("POST", pathA, s.idp.Token("rita", "https://other-audience.example.invalid"), create, nil); code != 401 {
			t.Errorf("wrong audience: %d", code)
		}
		// A token carrying tenant and admin claims, plus spoofed headers, is still just "mallory".
		forged := s.idp.Sign(oidcfake.Claims{Subject: "mallory", Audience: audience, Extra: map[string]any{"tenant_id": tenantA, "roles": []string{"admin"}, "kodama_tenant": tenantA}})
		if code, _, _ := s.do("POST", pathA, forged, create, map[string]string{"X-Tenant-ID": tenantA, "X-Role": "admin"}); code != 404 {
			t.Errorf("claims and headers must not grant tenant access: %d", code)
		}
		// The same person is a reviewer in A and only a viewer in B.
		if code, _, _ := s.do("POST", pathA, s.token("rita"), create, nil); code != 201 {
			t.Errorf("reviewer create in A: %d", code)
		}
		if code, _, _ := s.do("POST", pathB, s.token("rita"), create, nil); code != 403 {
			t.Errorf("viewer create in B: %d", code)
		}
		if code, _, _ := s.do("GET", pathB, s.token("rita"), nil, nil); code != 200 {
			t.Errorf("viewer read in B: %d", code)
		}
	})

	t.Run("a tenant selector is only a request for scope", func(t *testing.T) {
		for _, sel := range []string{"33333333-3333-4333-8333-333333333333", "not-a-uuid", "..%2f"} {
			if code, _, _ := s.do("GET", "/api/v1/tenants/"+sel+"/exceptions", s.token("rita"), nil, nil); code != 404 {
				t.Errorf("selector %q: %d", sel, code)
			}
		}
		if code, _, _ := s.do("GET", pathB, s.token("vic"), nil, nil); code != 404 {
			t.Errorf("vic is a member of A only: %d", code)
		}
	})

	t.Run("cross-tenant reads and writes are denied", func(t *testing.T) {
		idA := s.createException(tenantA, "rita", "A/ONLY")
		idB := s.createException(tenantB, "bea", "B/ONLY")
		// bea administers B only: A's record does not exist for her, by id or by list.
		if code, _, _ := s.do("GET", pathA+"/"+idA, s.token("bea"), nil, nil); code != 404 {
			t.Errorf("bea reading A: %d", code)
		}
		// rita holds roles in both tenants, but each id resolves only inside the requested tenant.
		if code, _, _ := s.do("GET", pathB+"/"+idA, s.token("rita"), nil, nil); code != 404 {
			t.Errorf("A's id under B: %d", code)
		}
		if code, _, _ := s.do("GET", pathA+"/"+idB, s.token("rita"), nil, nil); code != 404 {
			t.Errorf("B's id under A: %d", code)
		}
		if code, _ := s.resolve(tenantA, "bea", idA, "key-cross-0001", 1, 7); code != 404 {
			t.Errorf("bea resolving A's exception: %d", code)
		}
		if code, _ := s.resolve(tenantB, "rita", idA, "key-cross-0002", 1, 7); code != 403 && code != 404 {
			t.Errorf("rita resolving A's exception through B: %d", code)
		}
		if st, _ := s.state(tenantA, "rita", idA); st != "open" {
			t.Errorf("a denied write changed state: %s", st)
		}
		for _, p := range []string{pathA, pathB} {
			_, body, _ := s.do("GET", p, s.token("rita"), nil, nil)
			for _, it := range body["items"].([]any) {
				want := tenantA
				if p == pathB {
					want = tenantB
				}
				if it.(map[string]any)["tenantId"] != want {
					t.Errorf("foreign row in the list for %s: %v", p, it)
				}
			}
		}
	})

	t.Run("revoking membership takes effect on the next request", func(t *testing.T) {
		if code, _, _ := s.do("GET", pathA, s.token("vic"), nil, nil); code != 200 {
			t.Fatalf("vic before revocation: %d", code)
		}
		revoke(t, s, tenantA, "vic")
		if code, _, _ := s.do("GET", pathA, s.token("vic"), nil, nil); code != 404 {
			t.Errorf("vic after revocation: %d", code)
		}
	})

	t.Run("workers are scoped to one tenant and one service identity", func(t *testing.T) {
		idA := s.createException(tenantA, "rita", "A/WORK")
		idB := s.createException(tenantB, "bea", "B/WORK")
		if code, _ := s.resolve(tenantA, "rita", idA, "key-work-a-0001", 1, 7); code != 202 {
			t.Fatalf("resolve A: %d", code)
		}
		if code, _ := s.resolve(tenantB, "bea", idB, "key-work-b-0001", 1, 7); code != 202 {
			t.Fatalf("resolve B: %d", code)
		}
		// A human membership cannot be used as a worker identity, nor a worker in the wrong tenant.
		if _, err := s.worker(tenantA, "rita"); err == nil {
			t.Error("a human membership must not start a worker")
		}
		if _, err := s.worker(tenantA, "svc-worker-b"); err == nil {
			t.Error("a worker identity of another tenant must not start")
		}
		if _, err := s.worker(tenantA, "svc-not-provisioned"); err == nil {
			t.Error("an unprovisioned identity must not start")
		}
		stop, err := s.worker(tenantA, "svc-worker-a")
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		s.eventually("tenant A's exception to resolve", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", idA); return st == "resolved" })
		time.Sleep(600 * time.Millisecond) // give a misbehaving worker the chance to touch B
		if st, _ := s.state(tenantB, "bea", idB); st != "pending" {
			t.Fatalf("worker A must not process tenant B's work: %s", st)
		}
		if _, ok := s.erp.Effect(tenantB, "inv-adj-"+idB); ok {
			t.Fatal("tenant B has an ERP effect from tenant A's worker")
		}
		// Revoking the service identity stops its work: no impersonation, no cached authority.
		revoke(t, s, tenantA, "svc-worker-a")
		idA2 := s.createException(tenantA, "rita", "A/WORK2")
		if code, _ := s.resolve(tenantA, "rita", idA2, "key-work-a-0002", 1, 7); code != 202 {
			t.Fatal(code)
		}
		time.Sleep(800 * time.Millisecond)
		if st, _ := s.state(tenantA, "rita", idA2); st != "pending" {
			t.Fatalf("a revoked worker identity must not process work: %s", st)
		}
	})

	t.Run("the application works with every factory endpoint unreachable", func(t *testing.T) {
		// newSystem pointed the factory variables at a closed port for the whole run.
		if code, _, _ := s.do("GET", "/readyz", "", nil, nil); code != 200 {
			t.Errorf("readiness depends only on the application's own dependencies: %d", code)
		}
		if code, _, _ := s.do("GET", "/healthz", "", nil, nil); code != 200 {
			t.Errorf("liveness: %d", code)
		}
		s.createException(tenantA, "rita", "A/OFFLINE")
	})

	t.Run("readiness detects identity provider outages after initialization and recovers", func(t *testing.T) {
		if code, _, _ := s.do("GET", pathA, s.token("rita"), nil, nil); code != 200 {
			t.Fatalf("initialize token verification: %d", code)
		}
		if code, _, _ := s.do("GET", "/readyz", "", nil, nil); code != 200 {
			t.Fatalf("initial readiness: %d", code)
		}
		s.idp.SetDown(true)
		defer s.idp.SetDown(false)
		if code, _, _ := s.do("GET", "/readyz", "", nil, nil); code != 503 {
			t.Errorf("initialized API must report provider outage: %d", code)
		}
		if code, _, _ := s.do("GET", "/healthz", "", nil, nil); code != 200 {
			t.Errorf("liveness during outage: %d", code)
		}
		s.idp.SetDown(false)
		if code, _, _ := s.do("GET", "/readyz", "", nil, nil); code != 200 {
			t.Errorf("the same API must recover readiness: %d", code)
		}
	})

	t.Run("an unreachable identity provider fails requests closed and readiness reports it", func(t *testing.T) {
		s.idp.SetDown(true)
		defer s.idp.SetDown(false)
		// Cached keys may keep verifying; a fresh API must not authenticate anyone without its provider.
		dir := t.TempDir()
		_ = dir
		cfg := config.API{HTTPAddr: "127.0.0.1:0", Database: config.Database{URLFile: s.runtimeURLFile(), MaxConns: 2},
			OIDC: config.OIDC{Issuer: s.idp.Issuer(), Audience: audience}, CursorKeyFile: writeFile(t, s.dir, "ck2", "0123456789abcdef0123456789abcdef-cursor"), ShutdownTimeout: config.Duration(time.Second)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api, err := app.NewAPI(ctx, cfg, s.log)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = api.Run(ctx) }()
		fresh := &system{t: t, base: "http://" + api.Addr(), idp: s.idp}
		if code, _, _ := fresh.do("GET", "/readyz", "", nil, nil); code != 503 {
			t.Errorf("readiness: %d", code)
		}
		if code, _, _ := fresh.do("GET", "/healthz", "", nil, nil); code != 200 {
			t.Errorf("liveness: %d", code)
		}
		if code, _, _ := fresh.do("GET", pathA, s.token("rita"), nil, nil); code != 503 {
			t.Errorf("a provider outage is unavailable, never access: %d", code)
		}
	})
}

// revoke removes a membership using the owner identity (provisioning authority).
func revoke(t *testing.T, s *system, tenant, subject string) {
	t.Helper()
	ctx := context.Background()
	pool := s.db.OwnerPool(ctx, 1)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM memberships WHERE tenant_id = $1 AND subject = $2", tenant, subject); err != nil {
		t.Fatal(fmt.Errorf("revoke: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
