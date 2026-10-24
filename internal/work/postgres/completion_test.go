package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/work"
)

func (e *env) get(it work.Item) work.Item {
	e.t.Helper()
	got, err := e.service.Get(context.Background(), it.Scope, e.caller(engineer), it.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

// Test_human_completion: a current authorized human completes the observed
// revision and candidate; service and workload callers, admin-only authority,
// revoked access, stale subjects and missing evidence cannot; a passing run's
// notification path (a service principal) never completes work. Acceptance and
// findings are recorded atomically with history, audit and outbox. Real
// PostgreSQL with injected authority/evidence fixtures. WM-07, WM-A05.
func Test_human_completion(t *testing.T) {
	t.Run("a_current_human_completes_and_the_decision_is_recorded_atomically", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "completable"))
		h0, a0, o0 := e.records(it)
		done, err := e.move(it, reviewer, work.StateDone, completion(it))
		if err != nil {
			t.Fatal(err)
		}
		if done.State != work.StateDone || done.Version != it.Version+1 || done.ContentRevision != it.ContentRevision {
			t.Fatalf("done = %+v", done)
		}
		c := done.Completion
		if c == nil || c.CompletedBy.ID != reviewer || c.CompletedBy.Type != work.ActorUser || c.CandidateCommit != commitA ||
			c.ContentRevision != it.ContentRevision || len(c.AcceptedCriteria) != 1 || c.Reason != "accepted" {
			t.Fatalf("completion = %+v", c)
		}
		h, a, o := e.records(it)
		if h != h0+1 || a != a0+1 || o != o0+1 {
			t.Fatalf("records %d/%d/%d -> %d/%d/%d", h0, a0, o0, h, a, o)
		}
		hist, _ := e.service.History(context.Background(), scopeA, e.caller(reviewer), it.ID, work.HistoryQuery{AfterVersion: it.Version, Limit: 5})
		if len(hist) != 1 || hist[0].Completion == nil || hist[0].Review == nil || hist[0].Review.CandidateCommit != commitA {
			t.Fatalf("history = %+v", hist)
		}
		// The stored decision is read back identically.
		if got := e.get(it); got.Completion == nil || got.Completion.CompletedAt.IsZero() || got.State != work.StateDone {
			t.Fatalf("reread = %+v", got)
		}
	})

	t.Run("only_a_human_with_the_permission_completes", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "protected"))
		for name, tc := range map[string]struct {
			principal string
			want      error
		}{
			"service principal holding the permission (a passing run's notification)": {pipeline, work.ErrNotHuman},
			"admin without the reviewer permission":                                   {admin, work.ErrDenied},
			"engineer without the reviewer permission":                                {engineer, work.ErrDenied},
			"analyst":           {analyst, work.ErrDenied},
			"unknown principal": {"u_stranger", work.ErrHidden},
		} {
			if _, err := e.move(it, tc.principal, work.StateDone, completion(it)); !is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A workload identity is never human, whatever grants it holds.
		e.setActorType(tenantA, reviewer, work.ActorWorkload)
		if _, err := e.move(it, reviewer, work.StateDone, completion(it)); !is(err, work.ErrNotHuman) {
			t.Errorf("workload actor: %v", err)
		}
		e.setActorType(tenantA, reviewer, work.ActorUser)
		// Revocation is immediate.
		e.revoke(tenantA, reviewer, work.PermComplete)
		if _, err := e.move(it, reviewer, work.StateDone, completion(it)); !is(err, work.ErrDenied) {
			t.Errorf("revoked reviewer: %v", err)
		}
		if got := e.get(it); got.State != work.StateReview || got.Version != it.Version {
			t.Fatalf("refused attempts changed the item: %+v", got)
		}
		h, a, o := e.records(it)
		if h != 4 || a != 4 || o != 4 { // create, ready, active, review
			t.Fatalf("refused attempts wrote records: %d/%d/%d", h, a, o)
		}
	})

	t.Run("stale_subjects_and_missing_evidence_are_refused", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "stale"))

		if _, err := e.move(it, reviewer, work.StateDone, func(r *work.TransitionRequest) {}); !is(err, work.ErrPrerequisite) {
			t.Errorf("no completion claim: %v", err)
		}
		if _, err := e.move(it, reviewer, work.StateDone, func(r *work.TransitionRequest) {
			completion(it)(r)
			r.Completion.AcceptedCriteria = nil
		}); !is(err, work.ErrPrerequisite) {
			t.Errorf("no acceptance: %v", err)
		}
		if _, err := e.move(it, reviewer, work.StateDone, func(r *work.TransitionRequest) {
			completion(it)(r)
			r.Completion.CandidateCommit = commitB
		}); !is(err, work.ErrStaleSubject) {
			t.Errorf("another candidate: %v", err)
		}
		// The workspace moved past the reviewed candidate.
		e.acceptCommit(scopeA, it.ID, commitB)
		if _, err := e.move(it, reviewer, work.StateDone, completion(it)); !is(err, work.ErrStaleSubject) {
			t.Errorf("workspace advanced: %v", err)
		}
		e.acceptCommit(scopeA, it.ID, commitA)
		// An edit after review changes the content revision.
		edited, err := e.service.Edit(context.Background(), work.EditCommand{Scope: scopeA, Caller: e.caller(analyst), ID: it.ID, Expected: it.Version,
			Patch: work.Patch{Spec: str("# Plan\nChanged under review")}})
		if err != nil || edited.ContentRevision != it.ContentRevision+1 {
			t.Fatalf("edit: %v", err)
		}
		if _, err := e.move(edited, reviewer, work.StateDone, completion(it)); !is(err, work.ErrStaleSubject) {
			t.Errorf("old revision observed: %v", err)
		}
		if _, err := e.move(edited, reviewer, work.StateDone, completion(edited)); !is(err, work.ErrStaleSubject) {
			t.Errorf("subject recorded before the edit: %v", err)
		}
		// Rework and re-review binds a new subject; completion then works.
		e.setLimit(tenantA, projectA, 5)
		back := e.mustMove(edited, engineer, work.StateActive)
		again := e.toReview(back)
		if _, err := e.move(again, reviewer, work.StateDone, completion(again)); err != nil {
			t.Fatalf("completion of the re-reviewed subject: %v", err)
		}
	})

	t.Run("findings_are_acknowledged_by_name", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "findings"))
		e.ownerExec(`INSERT INTO t011_findings (tenant_id, work_item_id, id, summary) VALUES ($1, $2, 'F-7', 'export is slow')`, tenantA, string(it.ID))
		if _, err := e.move(it, reviewer, work.StateDone, completion(it)); !is(err, work.ErrPrerequisite) {
			t.Fatalf("open finding: %v", err)
		}
		done, err := e.move(it, reviewer, work.StateDone, func(r *work.TransitionRequest) {
			completion(it)(r)
			r.Reason = "waiving F-7: export speed is tracked separately"
			r.Completion.AcknowledgedFindings = []work.Finding{{ID: "F-7", Summary: "export is slow"}}
		})
		if err != nil || len(done.Completion.AcknowledgedFindings) != 1 || done.Completion.AcknowledgedFindings[0].ID != "F-7" {
			t.Fatalf("named acknowledgment: %v %+v", err, done.Completion)
		}
		reread := e.get(it)
		if reread.Completion == nil || len(reread.Completion.AcknowledgedFindings) != 1 {
			t.Fatalf("stored completion: %+v", reread.Completion)
		}
	})

	t.Run("nothing_but_the_review_state_completes", func(t *testing.T) {
		e := newEnv(t)
		it := e.activate(scopeA, "still active")
		if _, err := e.move(it, reviewer, work.StateDone, completion(it)); !is(err, work.ErrInvalidTransition) {
			t.Fatalf("active -> done: %v", err)
		}
	})
}
