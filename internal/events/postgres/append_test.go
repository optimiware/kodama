package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
)

// TestAppendAtomicity: state, audit, outbox and observation commit or roll back
// together; repeated IDs are idempotent and changed content conflicts.
func TestAppendAtomicity(t *testing.T) {
	f := newFixture(t, nil)
	f.businessTable("t005_state")
	ctx := context.Background()

	business := func(ctx context.Context, tx pgx.Tx, id string) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t005_state (tenant_id, id) VALUES ($1, $2)`, tenantA, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, actor_type, actor_id, action, resource_type, resource_id)
			VALUES ($1, $2, $3, 'user', 'usr_engineer', 'work_item.transition', 'work_item', $4)`, tenantA, "aud_"+id, projectA, "wi_"+id)
		return err
	}
	counts := func() (state, audit, outbox, obs, backlog int) {
		return f.count(tenantA, `SELECT count(*) FROM t005_state`), f.count(tenantA, `SELECT count(*) FROM audit`),
			f.count(tenantA, `SELECT count(*) FROM outbox`), f.count(tenantA, `SELECT count(*) FROM event_observations`),
			f.count(tenantA, `SELECT COALESCE(sum(pending_count), 0)::int FROM event_backlog`)
	}

	t.Run("commit", func(t *testing.T) {
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := business(ctx, tx, "c1"); err != nil {
				return err
			}
			res, err := f.store.Append(ctx, tx, eventA("c1", 1))
			if err == nil && (res.Sequence != 1 || res.Duplicate) {
				t.Errorf("result: %+v", res)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if s, a, o, ob, bl := counts(); s != 1 || a != 1 || o != 1 || ob != 1 || bl != 1 {
			t.Fatalf("state=%d audit=%d outbox=%d observations=%d backlog=%d", s, a, o, ob, bl)
		}
	})

	t.Run("rollback after append leaves nothing", func(t *testing.T) {
		boom := errors.New("business rule failed after the event was appended")
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := business(ctx, tx, "r1"); err != nil {
				return err
			}
			if _, err := f.store.Append(ctx, tx, eventA("r1", 1)); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if s, a, o, ob, bl := counts(); s != 1 || a != 1 || o != 1 || ob != 1 || bl != 1 {
			t.Fatalf("rollback leaked: state=%d audit=%d outbox=%d observations=%d backlog=%d", s, a, o, ob, bl)
		}
		// The sequence is not consumed by the rolled-back append.
		if res := f.append(eventA("r2", 1)); res.Sequence != 2 {
			t.Fatalf("sequence after a rollback = %d, want 2 (no gap)", res.Sequence)
		}
	})

	t.Run("repeated identical event is idempotent", func(t *testing.T) {
		before := f.count(tenantA, `SELECT count(*) FROM outbox`)
		res := f.append(eventA("c1", 1))
		if !res.Duplicate || res.Sequence != 1 {
			t.Fatalf("repeat: %+v", res)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox`); got != before {
			t.Fatalf("outbox grew from %d to %d", before, got)
		}
		// Within a single transaction too.
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			first, err := f.store.Append(ctx, tx, eventA("same-tx", 1))
			if err != nil {
				return err
			}
			second, err := f.store.Append(ctx, tx, eventA("same-tx", 1))
			if err != nil {
				return err
			}
			if first.Duplicate || !second.Duplicate || first.Sequence != second.Sequence {
				t.Errorf("same-transaction repeat: %+v %+v", first, second)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("changed content with the same ID conflicts", func(t *testing.T) {
		changed := eventA("c1", 2) // a different resource version under the same event ID
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.store.Append(ctx, tx, changed)
			return err
		})
		if !errors.Is(err, events.ErrConflict) {
			t.Fatalf("conflict: %v", err)
		}
	})

	t.Run("concurrent identical appends write once", func(t *testing.T) {
		const n = 10
		var wg sync.WaitGroup
		results := make(chan bool, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var dup bool
				err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
					res, err := f.store.Append(ctx, tx, eventA("race", 1))
					dup = res.Duplicate
					return err
				})
				if err != nil {
					t.Errorf("append: %v", err)
				}
				results <- dup
			}()
		}
		wg.Wait()
		close(results)
		fresh := 0
		for dup := range results {
			if !dup {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("%d appends wrote the event, want exactly 1", fresh)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = 'race'`); got != 1 {
			t.Fatalf("outbox rows = %d", got)
		}
	})

	t.Run("invalid input is refused before anything is written", func(t *testing.T) {
		before := f.count(tenantA, `SELECT count(*) FROM outbox`)
		bad := eventA("bad", 1)
		bad.Resource.Type = "gate"
		err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.store.Append(ctx, tx, bad)
			return err
		})
		if !errors.Is(err, eventsv1.ErrInvalid) {
			t.Fatalf("invalid event: %v", err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox`); got != before {
			t.Fatalf("outbox changed: %d -> %d", before, got)
		}
	})

	t.Run("tenant-wide events use their own scope", func(t *testing.T) {
		res := f.append(eventstestMembership(tenantA, "tw1", 1))
		if res.Scope.ProjectID != "" || res.Sequence != 1 {
			t.Fatalf("tenant-wide scope: %+v", res)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM event_scopes WHERE scope_key = '' AND project_id IS NULL`); got != 1 {
			t.Fatalf("tenant-wide scope rows = %d", got)
		}
	})

	_ = ctx
}
