package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	tenantA, projectA = "t_acme", "p_inventory"
	tenantB, projectB = "t_globex", "p_billing"
)

type fixture struct {
	t     testing.TB
	env   *eventstest.Env
	store *postgres.Store
	clock *eventstest.Clock
}

func newFixture(t testing.TB, mutate func(*postgres.Options), bindings ...postgres.Binding) *fixture {
	t.Helper()
	env := eventstest.NewDB(t, 20)
	env.SeedTenant(t, tenantA, projectA)
	env.SeedTenant(t, tenantB, projectB)
	f := &fixture{t: t, env: env, clock: &eventstest.Clock{}}
	opts := eventstest.DefaultOptions()
	opts.Clock = f.clock.Now
	opts.Authorizer = eventstest.Operators{"ops_1"}
	if mutate != nil {
		mutate(&opts)
	}
	f.store = env.NewStore(t, opts, bindings...)
	return f
}

func (f *fixture) ownerExec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.env.Migration.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// businessTable creates a tenant-owned table following the module conventions.
func (f *fixture) businessTable(name string) {
	f.t.Helper()
	f.ownerExec(fmt.Sprintf(`CREATE TABLE %[1]s (tenant_id kodama_id NOT NULL, id text NOT NULL, PRIMARY KEY (tenant_id, id));
		ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY; ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_all ON %[1]s USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
		GRANT SELECT, INSERT, UPDATE ON %[1]s TO %[2]s`, name, pgx.Identifier{f.env.DB.RuntimeRole}.Sanitize()))
}

func (f *fixture) tx(tenant string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), f.env.Runtime, domain.TenantID(tenant), fn)
}

func (f *fixture) count(tenant, sql string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.tx(tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&n) }); err != nil {
		f.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

func (f *fixture) append(env eventsv1.Envelope) postgres.AppendResult {
	f.t.Helper()
	var res postgres.AppendResult
	err := f.tx(string(env.TenantID), func(ctx context.Context, tx pgx.Tx) (err error) {
		res, err = f.store.Append(ctx, tx, env)
		return err
	})
	if err != nil {
		f.t.Fatalf("append %s: %v", env.ID, err)
	}
	return res
}

func eventA(id string, version int64) eventsv1.Envelope {
	return eventstest.WorkItemTransition(tenantA, projectA, id, "wi_"+id, version)
}

// fresh stamps env as occurring now on the fixture's clock. Fixture events carry
// a fixed past occurrence time; one produced after the test advanced the clock
// past a prune must look like what a live producer emits, or append rightly
// refuses it as older than the deduplication boundary.
func (f *fixture) fresh(env eventsv1.Envelope) eventsv1.Envelope {
	env.OccurredAt = f.clock.Now()
	return env
}

func eventB(id string, version int64) eventsv1.Envelope {
	return eventstest.WorkItemTransition(tenantB, projectB, id, "wi_"+id, version)
}

func eventstestMembership(tenant, id string, version int64) eventsv1.Envelope {
	return eventstest.MembershipChange(tenant, id, version)
}

type eventsv1Envelope = eventsv1.Envelope

type pgxTx = pgx.Tx

func eventstestDefaults(f *fixture) postgres.Options {
	o := eventstest.DefaultOptions()
	o.Clock = f.clock.Now
	o.Authorizer = eventstest.Operators{"ops_1"}
	return o
}

func readAll(t testing.TB, f *fixture) []postgres.Observation {
	t.Helper()
	var out []postgres.Observation
	err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, _, err = f.store.ReadObservations(ctx, tx, events.Scope{TenantID: tenantA, ProjectID: projectA}, 0, 100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readPageRaw(f *fixture) (struct{}, struct{ High, Floor int64 }, error) {
	var w struct{ High, Floor int64 }
	err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
		wm, err := f.store.Watermark(ctx, tx, events.Scope{TenantID: tenantA, ProjectID: projectA})
		w.High, w.Floor = wm.HighWater, wm.Floor
		return err
	})
	return struct{}{}, w, err
}
