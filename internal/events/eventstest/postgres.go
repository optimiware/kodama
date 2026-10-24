package eventstest

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// pgBus adapts the PostgreSQL store to the contract's Bus. Update is
// storage.WithTenant and the Unit's Append joins its pgx.Tx.
type pgBus struct {
	*postgres.Store
	env *Env
}

var _ Bus = (*pgBus)(nil)

type pgUnit struct {
	store *postgres.Store
	tx    pgx.Tx
}

func (u pgUnit) Append(ctx context.Context, env eventsv1.Envelope) (AppendResult, error) {
	r, err := u.store.Append(ctx, u.tx, env)
	return AppendResult{Scope: r.Scope, Sequence: r.Sequence, Digest: r.Digest, Duplicate: r.Duplicate}, err
}

func (u pgUnit) Note(ctx context.Context, key string) error {
	_, err := u.tx.Exec(ctx, `INSERT INTO t088_effects (tenant_id, id) VALUES (kodama_tenant_id(), $1) ON CONFLICT DO NOTHING`, key)
	return err
}

func (u pgUnit) Watermark(ctx context.Context, scope events.Scope) (int64, error) {
	w, err := u.store.Watermark(ctx, u.tx, scope)
	return w.HighWater, err
}

func (u pgUnit) Observations(ctx context.Context, scope events.Scope, after int64, limit int) ([][]byte, error) {
	obs, _, err := u.store.ReadObservations(ctx, u.tx, scope, after, limit)
	out := make([][]byte, len(obs))
	for i, o := range obs {
		out[i] = o.Payload
	}
	return out, err
}

// Update implements Bus.
func (b *pgBus) Update(ctx context.Context, tenant domain.TenantID, fn func(Unit) error) error {
	return storage.WithTenant(ctx, b.env.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return fn(pgUnit{store: b.Store, tx: tx})
	})
}

// Notes implements Bus.
func (b *pgBus) Notes(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	var out []string
	err := storage.WithTenant(ctx, b.env.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM t088_effects WHERE tenant_id = $1 ORDER BY id`, string(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	return out, err
}

// Replay implements Bus: request the replay, then run it until it completes.
func (b *pgBus) Replay(ctx context.Context, req events.ReplayRequest) (ReplayResult, error) {
	rec, err := b.Store.RequestReplay(ctx, req)
	if err != nil {
		return ReplayResult{}, err
	}
	for i := 0; rec.State != "completed" && i < 100; i++ {
		if rec, err = b.Store.RunReplay(ctx, req.Scope.TenantID, rec.ID); err != nil {
			return ReplayResult{}, err
		}
	}
	if rec.State != "completed" {
		return ReplayResult{}, fmt.Errorf("replay %s did not complete: %s", rec.ID, rec.State)
	}
	return ReplayResult{ID: rec.ID, State: rec.State, Classification: rec.Classification, Processed: rec.Processed, Skipped: rec.Skipped, Failed: rec.Failed}, nil
}

// OpenPostgres is the Open of the PostgreSQL adapter: an isolated migrated
// database with TenantA/ProjectA and TenantB/ProjectB seeded, the consumers
// bound, OperatorID authorized and clock as the store's time source. It skips
// without PostgreSQL, and always in the unit layer.
func OpenPostgres(t testing.TB, clock *Clock, consumers ...Consumer) Bus {
	t.Helper()
	env := NewDB(t, 20)
	env.SeedTenant(t, string(TenantA), string(ProjectA))
	env.SeedTenant(t, string(TenantB), string(ProjectB))
	if _, err := env.Migration.Exec(context.Background(), fmt.Sprintf(`CREATE TABLE t088_effects (tenant_id kodama_id NOT NULL, id text NOT NULL, PRIMARY KEY (tenant_id, id));
		ALTER TABLE t088_effects ENABLE ROW LEVEL SECURITY; ALTER TABLE t088_effects FORCE ROW LEVEL SECURITY;
		CREATE POLICY tenant_all ON t088_effects USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
		GRANT SELECT, INSERT, UPDATE ON t088_effects TO %s`, pgx.Identifier{env.DB.RuntimeRole}.Sanitize())); err != nil {
		t.Fatalf("create the contract's effect table: %v", err)
	}
	opts := DefaultOptions()
	opts.Clock = clock.Now
	opts.Authorizer = Operators{OperatorID}
	bus := &pgBus{env: env}
	bindings := make([]postgres.Binding, len(consumers))
	for i, c := range consumers {
		c := c
		bindings[i] = postgres.Binding{Spec: c.Spec, Handle: func(ctx context.Context, tx pgx.Tx, e eventsv1.Envelope) error {
			return c.Handle(ctx, e, pgUnit{store: bus.Store, tx: tx})
		}}
	}
	bus.Store = env.NewStore(t, opts, bindings...)
	return bus
}
