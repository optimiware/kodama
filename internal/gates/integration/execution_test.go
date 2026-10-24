package integration_test

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
)

// TestGateRestartNoJob (PX-13, PX-09, PX-A08): a ready gate step waits in
// durable records and consumes no compute. Recreating the services over the
// same records recovers exactly one gate, preserves the pending state, creates
// no attempt, reservation or Job for the gate, and after an applicable approval
// the run progresses once.
func TestGateRestartNoJob(t *testing.T) {
	e := gatestest.New(t)
	e.GrantAll()
	p, run := reviewed(t, e)
	tenant := p.Scope.TenantID

	finishTest(t, e, p)
	// The gate step is never claimable as a Job.
	if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("claim with a ready gate: %v", err)
	}
	attemptsBefore := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID))
	reservationsBefore := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
	dispatchedBefore := len(e.X.Workloads.Starts())

	if rep := reconcile(t, e, tenant); rep.Requested != 1 {
		t.Fatalf("first reconciliation: %+v", rep)
	}
	g := gateOf(t, e, run, "review")
	if g.Status != gates.StatusPending || g.Version != 1 {
		t.Fatalf("gate %+v", g)
	}
	agg := e.Get(run)
	review := agg.Step("review")
	if review.Status != execution.StepPending || review.GateRef != string(g.ID) || review.AttemptsStarted != 0 {
		t.Fatalf("a ready gate step must stay pending with its gate reference and no attempt: %+v", review)
	}

	// Restart: new services over the same durable records.
	for i := 0; i < 2; i++ {
		e.Restart()
		if rep := reconcile(t, e, tenant); rep.Requested != 0 || rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("reconciliation after restart %d created or moved something: %+v", i, rep)
		}
	}
	if n := count(e, `SELECT count(*) FROM gates WHERE run_id = $1`, string(run.ID)); n != 1 {
		t.Fatalf("%d gates after restarts", n)
	}
	if got := stepStatus(t, e, run, "review"); got != execution.StepPending {
		t.Fatalf("gate step %s while waiting for its decision", got)
	}
	if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("a pending gate let its successor run: %v", err)
	}

	// An applicable approval moves the step once, from the durable record.
	if _, err := approve(t, e, g, gatestest.Reviewer); err != nil {
		t.Fatal(err)
	}
	e.Restart()
	if rep := reconcile(t, e, tenant); rep.Completed != 1 {
		t.Fatalf("after approval: %+v", rep)
	}
	if rep := reconcile(t, e, tenant); rep.Completed != 0 || rep.Failed != 0 {
		t.Fatalf("repeated processing moved the step again: %+v", rep)
	}
	review = e.Get(run).Step("review")
	if review.Status != execution.StepSucceeded || review.AttemptsStarted != 0 || review.GateRef != string(g.ID) {
		t.Fatalf("gate step after approval: %+v", review)
	}
	if n := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'review'`, string(run.ID)); n != 0 {
		t.Fatalf("%d attempts for the gate step", n)
	}
	if n := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)); n != attemptsBefore {
		t.Fatalf("attempts %d -> %d: the gate allocated one", attemptsBefore, n)
	}
	if n := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID)); n != reservationsBefore {
		t.Fatalf("reservations %d -> %d: the gate held compute", reservationsBefore, n)
	}
	if d := len(e.X.Workloads.Starts()); d != dispatchedBefore {
		t.Fatalf("a workload was started for the gate: %d -> %d", dispatchedBefore, d)
	}

	// The successor runs exactly once.
	in := e.MustClaim(tenant)
	if in.StepID != "deploy" || in.Ordinal != 1 {
		t.Fatalf("claimed %+v", in)
	}
	if _, err := e.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("a second claim: %v", err)
	}
	if n := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'deploy'`, string(run.ID)); n != 1 {
		t.Fatalf("%d deploy attempts", n)
	}
	// Audit and events exist once for the request and the decision.
	if n := count(e, `SELECT count(*) FROM audit WHERE resource_type = 'gate' AND resource_id = $1`, string(g.ID)); n != 2 {
		t.Fatalf("%d gate audit rows, want requested + decided", n)
	}
}
