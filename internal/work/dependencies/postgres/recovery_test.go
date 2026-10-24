package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
)

// Test_dependency_atomicity_and_recovery: injected history, audit, version,
// edge and event failures roll back every effect; a full event backlog fails
// safely; an unknown commit outcome is reported as unavailability and never
// replayed; a reread plus retry produces no duplicates; and nothing depends on
// a broker (none runs here). Real PostgreSQL. WM-08, IT-11.
func Test_dependency_atomicity_and_recovery(t *testing.T) {
	ctx := context.Background()

	for name, tc := range map[string]struct{ table, event string }{
		"history insert fails": {"work_item_history", "INSERT"},
		"audit insert fails":   {"audit", "INSERT"},
		"version update fails": {"work_items", "UPDATE"},
		"edge insert fails":    {"work_item_dependencies", "INSERT"},
		"outbox insert fails":  {"outbox", "INSERT"},
	} {
		t.Run(name, func(t *testing.T) {
			st := depstest.NewStack(t)
			dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
			before := snap(st, dep)
			st.OwnerExec(`CREATE FUNCTION t045_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't045 injected failure'; END $$`)
			st.OwnerExec(`CREATE TRIGGER t045_fail BEFORE ` + tc.event + ` ON ` + tc.table + ` FOR EACH ROW EXECUTE FUNCTION t045_fail()`)
			_, err := add(st, dep, pre, dep.Version)
			if err == nil {
				t.Fatal("the mutation reported success")
			}
			if is(err, work.ErrUnavailable) {
				t.Fatalf("a failure before commit is not an unknown outcome: %v", err)
			}
			st.OwnerExec(`DROP TRIGGER t045_fail ON ` + tc.table)
			if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
				t.Fatalf("partial write: %+v -> %+v, links %d", before, after, st.Links(tenantA))
			}
			// Nothing was replayed: the same request now applies exactly once.
			out, err := add(st, dep, pre, dep.Version)
			if err != nil || !out.Changed {
				t.Fatalf("retry: %+v %v", out, err)
			}
			if after := snap(st, dep); after.history != before.history+1 || after.version != before.version+1 || st.Links(tenantA) != 1 {
				t.Fatalf("after one retry: %+v", after)
			}
		})
	}

	t.Run("removal_failures_roll_back_too", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		st.MustLink(dep, pre)
		before := snap(st, dep)
		st.OwnerExec(`CREATE FUNCTION t045_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't045 injected failure'; END $$`)
		st.OwnerExec(`CREATE TRIGGER t045_fail BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t045_fail()`)
		if _, err := remove(st, dep, pre, domain.Version(before.version)); err == nil {
			t.Fatal("the removal reported success")
		}
		st.OwnerExec(`DROP TRIGGER t045_fail ON audit`)
		if after := snap(st, dep); after != before || st.Links(tenantA) != 1 {
			t.Fatalf("the failed removal left effects: %+v -> %+v", before, after)
		}
	})

	t.Run("a_full_event_backlog_rolls_the_change_back_and_reads_still_work", func(t *testing.T) {
		st := depstest.NewStack(t, depstest.Options{MutateEvents: func(o *eventspg.Options) {
			o.Backlog = events.BacklogLimits{MaxEvents: 5, MaxBytes: 1 << 20}
		}})
		n := items(st, scopeA, 3, "fits") // 3 events
		st.MustLink(n[0], n[1])           // 4
		st.MustLink(n[0], n[2])           // 5: the backlog is now full
		before := snap(st, n[1])
		_, err := st.Link(n[1], n[2])
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("backlog full: %v", err)
		}
		if after := snap(st, n[1]); after != before || st.Links(tenantA) != 2 {
			t.Fatalf("the rejected change left effects: %+v -> %+v, links %d", before, after, st.Links(tenantA))
		}
		if got := prerequisites(t, st, n[0]); len(got) != 2 {
			t.Fatalf("reads during backlog pressure: %v", got)
		}
		// A no-op writes no event, so it succeeds even though the backlog is full.
		if out, err := st.Link(n[0], n[1]); err != nil || out.Changed {
			t.Fatalf("no-op under backlog pressure: %+v %v", out, err)
		}
	})

	t.Run("a_connection_lost_at_commit_is_unknown_and_never_replayed", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		before := snap(st, dep)
		// The constraint trigger runs at commit time, after every statement of the
		// mutation succeeded, and kills the session.
		st.OwnerExec(`CREATE FUNCTION t045_kill() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_terminate_backend(pg_backend_pid()); PERFORM pg_sleep(1); RETURN NULL; END $$`)
		st.OwnerExec(`CREATE CONSTRAINT TRIGGER t045_kill AFTER UPDATE ON work_items DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION t045_kill()`)
		_, err := add(st, dep, pre, dep.Version)
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("an interrupted commit must be unavailability, got %v", err)
		}
		st.OwnerExec(`DROP TRIGGER t045_kill ON work_items`)
		// Reread: here the commit did not happen, and nothing was replayed.
		if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
			t.Fatalf("an interrupted commit changed state: %+v -> %+v", before, after)
		}
		// Rereading makes a manual retry safe and exactly-once.
		if _, err := add(st, dep, pre, st.Get(dep).Version); err != nil {
			t.Fatal(err)
		}
		if after := snap(st, dep); after.history != before.history+1 || st.Links(tenantA) != 1 {
			t.Fatalf("retry footprint: %+v", after)
		}
	})

	t.Run("a_cancelled_request_context_rolls_back", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		before := snap(st, dep)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := st.Deps.Add(cctx, dependencies.AddCommand{Scope: scopeA, Caller: depstest.Caller(depstest.Engineer), Dependent: dep.ID, Prerequisite: pre.ID, Expected: dep.Version})
		if err == nil {
			t.Fatal("a cancelled request reported success")
		}
		if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
			t.Fatal("a cancelled request changed state")
		}
	})

	t.Run("no_op_retries_produce_no_duplicate_effects", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		first, err := add(st, dep, pre, dep.Version)
		if err != nil || !first.Changed {
			t.Fatalf("first: %+v %v", first, err)
		}
		applied := snap(st, dep)
		// A blind replay of the original request carries the old version: the
		// resource moved on, so it is a conflict, never a second application.
		if _, err := add(st, dep, pre, dep.Version); !is(err, work.ErrVersionConflict) {
			t.Fatalf("replay with the stale version: %v", err)
		}
		// After rereading the version the retry is a documented no-op.
		for i := 0; i < 3; i++ {
			out, err := add(st, dep, pre, first.Owner.Version)
			if err != nil || out.Changed || out.Owner.Version != first.Owner.Version {
				t.Fatalf("no-op retry %d: %+v %v", i, out, err)
			}
		}
		if after := snap(st, dep); after != applied || st.Links(tenantA) != 1 {
			t.Fatalf("retries changed the footprint: %+v -> %+v", applied, after)
		}
		// The same holds for removal.
		rm, err := remove(st, dep, pre, first.Owner.Version)
		if err != nil || !rm.Changed {
			t.Fatalf("remove: %+v %v", rm, err)
		}
		removed := snap(st, dep)
		for i := 0; i < 3; i++ {
			if out, err := remove(st, dep, pre, rm.Owner.Version); err != nil || out.Changed {
				t.Fatalf("no-op removal %d: %+v %v", i, out, err)
			}
		}
		if after := snap(st, dep); after != removed {
			t.Fatalf("removal retries changed the footprint: %+v -> %+v", removed, after)
		}
	})

	t.Run("commits_never_depend_on_the_broker", func(t *testing.T) {
		// No broker exists in this test: the change commits and its event waits
		// in the outbox for the publisher.
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		out := st.MustLink(dep, pre)
		pending := st.Count(tenantA, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND state = 'pending' AND event_id = $2`, tenantA, "evt_"+latestHistoryID(t, st, dep))
		if !out.Changed || pending != 1 {
			t.Fatalf("outcome %+v, pending events for the change: %d", out, pending)
		}
	})
}

func latestHistoryID(t testing.TB, st *depstest.Stack, it work.Item) string {
	t.Helper()
	var id string
	err := st.Owner.QueryRow(context.Background(), `SELECT id FROM work_item_history WHERE tenant_id = $1 AND work_item_id = $2 ORDER BY version DESC LIMIT 1`,
		string(it.Scope.TenantID), string(it.ID)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
