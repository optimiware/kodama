package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

func (e *env) setLimit(tenant, project string, limit int) {
	e.t.Helper()
	e.ownerExec(`UPDATE projects SET active_wip_limit = $3 WHERE tenant_id = $1 AND id = $2`, tenant, project, limit)
}

func (e *env) ready(scope domain.Scope, title string) work.Item {
	e.t.Helper()
	return e.mustMove(e.create(scope, work.KindStory, title), engineer, work.StateReady)
}

func (e *env) activeCount(tenant string) int {
	return e.count(tenant, `SELECT count(*) FROM work_items WHERE state = 'active'`)
}

// Test_last_wip_slot: two distinct admissions contend for the last slot;
// exactly one succeeds, blocked active items still count, and the loser leaves
// no mutation records. Real PostgreSQL. WM-03, WM-04, WM-A03.
func Test_last_wip_slot(t *testing.T) {
	e := newEnv(t)
	e.setLimit(tenantA, projectA, 2)
	holder := e.activate(scopeA, "holds one slot")
	a, b := e.ready(scopeA, "contender a"), e.ready(scopeA, "contender b")
	beforeA, _, _ := e.records(a)
	beforeB, _, _ := e.records(b)

	errs := parallel(
		func() error { _, err := e.move(a, engineer, work.StateActive); return err },
		func() error { _, err := e.move(b, engineer, work.StateActive); return err },
	)
	winners, losers := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case is(err, work.ErrWIPLimit):
			losers++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners %d losers %d: %v", winners, losers, errs)
	}
	if n := e.activeCount(tenantA); n != 2 {
		t.Fatalf("active items = %d, want the limit 2", n)
	}
	// The loser has no new history, audit or outbox record and is still ready.
	for _, it := range []work.Item{a, b} {
		got, err := e.service.Get(context.Background(), scopeA, e.caller(engineer), it.ID)
		if err != nil {
			t.Fatal(err)
		}
		before := beforeA
		if it.ID == b.ID {
			before = beforeB
		}
		h, au, ob := e.records(it)
		switch got.State {
		case work.StateActive:
			if h != before+1 || au != before+1 || ob != before+1 {
				t.Errorf("winner records %d/%d/%d, want %d each", h, au, ob, before+1)
			}
		case work.StateReady:
			if got.Version != it.Version || h != before || au != before || ob != before {
				t.Errorf("loser changed: version %d records %d/%d/%d", got.Version, h, au, ob)
			}
		default:
			t.Errorf("unexpected state %s", got.State)
		}
	}
	_ = holder
}

// Test_blocked_items_count_toward_wip: a blocked_reason does not release a slot.
func Test_blocked_items_count_toward_wip(t *testing.T) {
	e := newEnv(t)
	e.setLimit(tenantA, projectA, 1)
	held := e.activate(scopeA, "blocked but active")
	if _, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: held.ID, Expected: held.Version,
		Patch: work.Patch{BlockedReason: work.Some("waiting for ERP access")}}); err != nil {
		t.Fatal(err)
	}
	next := e.ready(scopeA, "needs the slot")
	if _, err := e.move(next, engineer, work.StateActive); !is(err, work.ErrWIPLimit) {
		t.Fatalf("a blocked active item must keep its slot: %v", err)
	}
	// Other projects and tenants have their own limits.
	other := e.ready(scopeB, "other tenant")
	if _, err := e.move(other, engineer, work.StateActive); err != nil {
		t.Fatalf("another tenant's limit is independent: %v", err)
	}
}

// Test_reopen_wip: review -> active (rework) and done -> active (reopen) use
// the same guard as first admission, require reasons, preserve the content
// revision and cannot exceed the limit under contention. Real PostgreSQL.
// WM-03, WM-04, WM-A03.
func Test_reopen_wip(t *testing.T) {
	e := newEnv(t)
	e.setLimit(tenantA, projectA, 3)

	inReview := e.toReview(e.activate(scopeA, "review item"))
	toComplete := e.toReview(e.activate(scopeA, "done item"))
	done := e.mustMove(toComplete, reviewer, work.StateDone, completion(toComplete))
	if done.State != work.StateDone {
		t.Fatal("setup")
	}
	e.activate(scopeA, "holder one")
	e.activate(scopeA, "holder two") // 2 of 3 slots used; review and done items hold none
	if n := e.activeCount(tenantA); n != 2 {
		t.Fatalf("setup active = %d", n)
	}

	// Reasons are mandatory for rework and reopening.
	if _, err := e.move(inReview, engineer, work.StateActive, func(r *work.TransitionRequest) { r.Reason = "" }); !is(err, work.ErrPrerequisite) {
		t.Fatalf("rework without a reason: %v", err)
	}
	if _, err := e.move(done, reviewer, work.StateActive, func(r *work.TransitionRequest) { r.Reason = "" }); !is(err, work.ErrPrerequisite) {
		t.Fatalf("reopen without a reason: %v", err)
	}

	errs := parallel(
		func() error {
			_, err := e.move(inReview, engineer, work.StateActive, func(r *work.TransitionRequest) { r.Reason = "rework: missing audit record" })
			return err
		},
		func() error {
			_, err := e.move(done, reviewer, work.StateActive, func(r *work.TransitionRequest) { r.Reason = "reopen: regression in ERP sync" })
			return err
		},
	)
	winners, losers := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case is(err, work.ErrWIPLimit):
			losers++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners %d losers %d", winners, losers)
	}
	if n := e.activeCount(tenantA); n != 3 {
		t.Fatalf("active = %d, want exactly the limit", n)
	}

	// The admitted item kept its content revision and lost its current decision.
	for _, it := range []work.Item{inReview, done} {
		got, _ := e.service.Get(context.Background(), scopeA, e.caller(engineer), it.ID)
		if got.State != work.StateActive {
			continue
		}
		if got.ContentRevision != it.ContentRevision || got.Version != it.Version+1 || got.Completion != nil || got.ReviewSubject != nil {
			t.Fatalf("admitted item = %+v", got)
		}
	}
	// The earlier completion stays in history after reopening.
	hist, err := e.service.History(context.Background(), scopeA, e.caller(engineer), done.ID, work.HistoryQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hist {
		found = found || (h.To == work.StateDone && h.Completion != nil && h.Completion.CompletedBy.ID == reviewer)
	}
	if !found {
		t.Fatal("the completion decision is missing from history")
	}

	// A lowered limit is honored by the next entry.
	e.setLimit(tenantA, projectA, 1)
	loser := e.ready(scopeA, "after the limit dropped")
	if _, err := e.move(loser, engineer, work.StateActive); !is(err, work.ErrWIPLimit) {
		t.Fatalf("lowered limit: %v", err)
	}
}
