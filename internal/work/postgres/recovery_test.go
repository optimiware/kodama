package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/work"
)

func (e *env) snapshot(it work.Item) (history, audit, outbox int, version int64) {
	h, a, o := e.records(it)
	return h, a, o, int64(e.get(it).Version)
}

// Test_mutation_rollback_and_unknown_commit: a failing history, audit or event
// append, backlog rejection and a connection lost at commit roll back or leave an
// unknown outcome that is reported as unavailability, never as success and never
// replayed automatically. Real PostgreSQL. WM-08.
func Test_mutation_rollback_and_unknown_commit(t *testing.T) {
	ctx := context.Background()

	for name, tc := range map[string]struct{ table, event string }{
		"history insert fails": {"work_item_history", "INSERT"},
		"audit insert fails":   {"audit", "INSERT"},
		"state update fails":   {"work_items", "UPDATE"},
		"outbox insert fails":  {"outbox", "INSERT"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			it := e.create(scopeA, work.KindStory, "rolls back")
			h0, a0, o0, v0 := e.snapshot(it)
			e.ownerExec(`CREATE FUNCTION t011_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't011 injected failure'; END $$`)
			e.ownerExec(`CREATE TRIGGER t011_fail BEFORE ` + tc.event + ` ON ` + tc.table + ` FOR EACH ROW EXECUTE FUNCTION t011_fail()`)
			_, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("never stored")}})
			if err == nil || is(err, work.ErrNoChange) {
				t.Fatalf("the mutation reported success: %v", err)
			}
			if is(err, work.ErrUnavailable) {
				t.Fatalf("a failure before commit is not an unknown outcome: %v", err)
			}
			e.ownerExec(`DROP TRIGGER t011_fail ON ` + tc.table)
			h, a, o, v := e.snapshot(it)
			if h != h0 || a != a0 || o != o0 || v != v0 || e.get(it).Title != it.Title {
				t.Fatalf("partial write: %d/%d/%d v%d -> %d/%d/%d v%d", h0, a0, o0, v0, h, a, o, v)
			}
			// Nothing was replayed: the same request now applies exactly once.
			next, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("stored once")}})
			if err != nil || int64(next.Version) != v0+1 {
				t.Fatalf("retry: %v", err)
			}
			if h2, _, _ := e.records(it); h2 != h0+1 {
				t.Fatalf("history after one successful retry = %d", h2)
			}
		})
	}

	t.Run("backlog_rejection_rolls_back_the_business_change", func(t *testing.T) {
		e := newEnvWith(t, envOptions{mutate: func(o *eventspg.Options) { o.Backlog = events.BacklogLimits{MaxEvents: 3, MaxBytes: 1 << 20} }})
		for i := 0; i < 3; i++ {
			e.create(scopeA, work.KindTask, "fits")
		}
		_, err := e.service.Create(ctx, work.CreateCommand{Scope: scopeA, Caller: e.caller(engineer), Kind: work.KindTask, Title: "rejected"})
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("backlog full: %v", err)
		}
		if n := e.count(tenantA, `SELECT count(*) FROM work_items`); n != 3 {
			t.Fatalf("items = %d", n)
		}
		if n := e.count(tenantA, `SELECT count(*) FROM work_item_history`); n != 3 {
			t.Fatalf("history = %d", n)
		}
		if n := e.count(tenantA, `SELECT count(*) FROM audit WHERE resource_type = 'work_item'`); n != 3 {
			t.Fatalf("audit = %d", n)
		}
		// An existing item can still be read while the backlog is full.
		if items, err := e.service.List(ctx, scopeA, e.caller(analyst), work.ListQuery{Limit: 10}); err != nil || len(items) != 3 {
			t.Fatalf("reads during backlog pressure: %v %d", err, len(items))
		}
	})

	t.Run("a_connection_lost_at_commit_is_reported_unknown_and_never_replayed", func(t *testing.T) {
		e := newEnv(t)
		it := e.create(scopeA, work.KindStory, "commit is cut")
		h0, a0, o0, v0 := e.snapshot(it)
		// The constraint trigger runs at commit time, after every statement of the
		// mutation succeeded, and kills the session.
		e.ownerExec(`CREATE FUNCTION t011_kill() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_terminate_backend(pg_backend_pid()); PERFORM pg_sleep(1); RETURN NULL; END $$`)
		e.ownerExec(`CREATE CONSTRAINT TRIGGER t011_kill AFTER UPDATE ON work_items DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION t011_kill()`)
		_, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("maybe stored")}})
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("an interrupted commit must be unavailability, got %v", err)
		}
		e.ownerExec(`DROP TRIGGER t011_kill ON work_items`)
		// The caller rereads: here the commit did not happen, so state is unchanged
		// and no automatic replay occurred.
		h, a, o, v := e.snapshot(it)
		if h != h0 || a != a0 || o != o0 || v != v0 {
			t.Fatalf("records changed by an interrupted commit: %d/%d/%d v%d", h, a, o, v)
		}
		// Rereading the version makes a manual retry safe and exactly-once.
		if _, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("maybe stored")}}); err != nil {
			t.Fatal(err)
		}
		if n, _, _ := e.records(it); n != h0+1 {
			t.Fatalf("history = %d", n)
		}
	})

	t.Run("a_cancelled_request_context_rolls_back", func(t *testing.T) {
		e := newEnv(t)
		it := e.create(scopeA, work.KindStory, "ctx")
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := e.service.Edit(cctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("x")}})
		if err == nil {
			t.Fatal("a cancelled request reported success")
		}
		if e.get(it).Version != it.Version {
			t.Fatal("a cancelled request changed the item")
		}
	})

	t.Run("an_expired_authority_check_never_writes", func(t *testing.T) {
		e := newEnv(t)
		it := e.create(scopeA, work.KindStory, "authority")
		e.ownerExec(`ALTER TABLE t011_grants RENAME TO t011_grants_gone`)
		_, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Title: str("x")}})
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("an unavailable authority must fail closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		e.ownerExec(`ALTER TABLE t011_grants_gone RENAME TO t011_grants`)
		if e.get(it).Version != it.Version {
			t.Fatal("changed")
		}
	})
}
