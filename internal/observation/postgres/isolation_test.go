package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
	"github.com/optimiware/kodama/internal/storage"
)

func scope(t testing.TB, tenant domain.TenantID, project domain.ProjectID, run string, types ...string) observation.Scope {
	t.Helper()
	s, err := observation.NewScope(tenant, project, domain.RunID(run), types)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// tenantContext reads the tenant setting on a fresh connection of the pool: a
// pooled connection must never carry the previous caller's tenant.
func tenantContext(t testing.TB, e *ot.Env) *string {
	t.Helper()
	var got *string
	if err := e.Runtime.QueryRow(context.Background(), `SELECT kodama_tenant_id()`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Test_tenant_pool_cleanup: RLS, explicit predicates, foreign run lookup,
// cancellation and repeated pooled-connection reuse preserve tenant isolation.
func Test_tenant_pool_cleanup(t *testing.T) {
	e := ot.New(t, ot.Options{Conns: 1}) // one connection: every call reuses it
	ctx := context.Background()
	// Tenant B owns a project with the very same ID as tenant A's.
	e.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, string(ot.TenantB), string(ot.ProjectA))

	e.Commit(ot.TenantA, ot.InsertWork(ot.ProjectA, "wi_a1", "backlog"), ot.WorkCreated(ot.TenantA, ot.ProjectA, "t024_ea1", "wi_a1"))
	e.Commit(ot.TenantB, ot.InsertWork(ot.ProjectA, "wi_b1", "active"), ot.WorkCreated(ot.TenantB, ot.ProjectA, "t024_eb1", "wi_b1"))
	e.Commit(ot.TenantB, ot.InsertWork(ot.ProjectA, "wi_b2", "ready"), ot.WorkCreated(ot.TenantB, ot.ProjectA, "t024_eb2", "wi_b2"))
	e.SeedRun(ot.TenantA, ot.ProjectA2, "run_a2", "running", 1) // same tenant, other project
	e.SeedRun(ot.TenantB, ot.ProjectB, "run_b", "running", 1)

	t.Run("reads and snapshots see only the requested tenant", func(t *testing.T) {
		snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 100)
		if err != nil || len(snap.Resources) != 1 || snap.Resources[0].ID != "wi_a1" || snap.Position != 1 {
			t.Fatalf("tenant A snapshot = %+v, %v", snap, err)
		}
		snap, err = e.Obs.Snapshot(ctx, scope(t, ot.TenantB, ot.ProjectA, ""), 100)
		if err != nil || len(snap.Resources) != 2 || snap.Position != 2 {
			t.Fatalf("tenant B snapshot = %+v, %v", snap, err)
		}
		recs, wm, err := e.Obs.Read(ctx, scope(t, ot.TenantA, ot.ProjectA, ""), 0, 10)
		if err != nil || len(recs) != 1 || recs[0].Envelope.TenantID != ot.TenantA || wm.HighWater != 1 {
			t.Fatalf("tenant A read = %+v %+v %v", recs, wm, err)
		}
		if got := tenantContext(t, e); got != nil {
			t.Fatalf("the pooled connection kept tenant context %q", *got)
		}
	})

	t.Run("a project of another tenant or a missing project is not found", func(t *testing.T) {
		for name, s := range map[string]observation.Scope{
			"foreign project": scope(t, ot.TenantA, ot.ProjectB, ""),
			"missing project": scope(t, ot.TenantA, "p024_missing", ""),
			"unknown tenant":  scope(t, "t024_nobody", ot.ProjectA, ""),
		} {
			if err := e.Obs.Verify(ctx, s); !errors.Is(err, observation.ErrNotFound) {
				t.Errorf("%s: Verify = %v", name, err)
			}
			if _, err := e.Obs.Snapshot(ctx, s, 10); !errors.Is(err, observation.ErrNotFound) {
				t.Errorf("%s: Snapshot = %v", name, err)
			}
		}
	})

	t.Run("run lookup is scoped by tenant and project", func(t *testing.T) {
		if err := e.Obs.Verify(ctx, scope(t, ot.TenantA, ot.ProjectA2, "run_a2")); err != nil {
			t.Fatalf("own run: %v", err)
		}
		for name, s := range map[string]observation.Scope{
			"another project's run": scope(t, ot.TenantA, ot.ProjectA, "run_a2"),
			"another tenant's run":  scope(t, ot.TenantA, ot.ProjectA2, "run_b"),
			"foreign tenant+run":    scope(t, ot.TenantB, ot.ProjectB, "run_a2"),
			"missing run":           scope(t, ot.TenantA, ot.ProjectA2, "run_missing"),
		} {
			if err := e.Obs.Verify(ctx, s); !errors.Is(err, observation.ErrNotFound) {
				t.Errorf("%s: Verify = %v, want ErrNotFound", name, err)
			}
			if _, err := e.Obs.Snapshot(ctx, s, 10); !errors.Is(err, observation.ErrNotFound) {
				t.Errorf("%s: Snapshot = %v", name, err)
			}
		}
		snap, err := e.Obs.Snapshot(ctx, scope(t, ot.TenantA, ot.ProjectA2, "run_a2"), 10)
		if err != nil || len(snap.Resources) != 1 || snap.Resources[0].ID != "run_a2" {
			t.Fatalf("run snapshot = %+v, %v", snap, err)
		}
	})

	t.Run("repeated reuse and concurrency never leak a tenant", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tenant, want := ot.TenantA, 1
				if i%2 == 1 {
					tenant, want = ot.TenantB, 2
				}
				snap, err := e.Obs.Snapshot(ctx, scope(t, tenant, ot.ProjectA, ""), 100)
				if err != nil || len(snap.Resources) != want {
					errs <- fmt.Errorf("tenant %s: %d resources, %v (want %d)", tenant, len(snap.Resources), err, want)
					return
				}
				for _, r := range snap.Resources {
					if (tenant == ot.TenantA) != (r.ID == "wi_a1") {
						errs <- fmt.Errorf("tenant %s saw %s", tenant, r.ID)
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if got := tenantContext(t, e); got != nil {
			t.Fatalf("the pooled connection kept tenant context %q", *got)
		}
	})

	t.Run("cancellation releases the connection clean", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := e.Obs.Read(cctx, scope(t, ot.TenantA, ot.ProjectA, ""), 0, 10); err == nil {
			t.Fatal("a cancelled read must fail")
		} else if errors.Is(err, observation.ErrUnavailable) && cctx.Err() == nil {
			t.Fatalf("err = %v", err)
		}
		if _, err := e.Obs.Snapshot(cctx, scope(t, ot.TenantA, ot.ProjectA, ""), 10); err == nil {
			t.Fatal("a cancelled snapshot must fail")
		}
		// A deadline that fires while a transaction holds a row lock the read
		// needs is not available here; a deadline during begin is.
		short, cancel2 := context.WithTimeout(ctx, time.Nanosecond)
		defer cancel2()
		time.Sleep(time.Millisecond)
		if _, _, err := e.Obs.Read(short, scope(t, ot.TenantA, ot.ProjectA, ""), 0, 10); err == nil {
			t.Fatal("an expired deadline must fail")
		}
		// The single connection is usable again and carries no context.
		if got := tenantContext(t, e); got != nil {
			t.Fatalf("tenant context %q after cancellation", *got)
		}
		recs, _, err := e.Obs.Read(ctx, scope(t, ot.TenantB, ot.ProjectA, ""), 0, 10)
		if err != nil || len(recs) != 2 {
			t.Fatalf("read after cancellation: %d records, %v", len(recs), err)
		}
	})

	t.Run("RLS alone confines a statement even without predicates", func(t *testing.T) {
		var n int
		err := storage.WithTenantOptions(ctx, e.Runtime, ot.TenantA, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM work_items`).Scan(&n)
		})
		if err != nil || n != 1 {
			t.Fatalf("a tenant-A transaction sees %d work items (%v), want 1", n, err)
		}
	})
}
