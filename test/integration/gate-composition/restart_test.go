package gatecomposition_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
)

// decidedEnvelope is a gate.decided notification as a decision commits it. The
// consumer takes nothing from it but the gate's identity.
func decidedEnvelope(id string, g *gates.Gate, state string, at time.Time) eventsv1.Envelope {
	data, _ := json.Marshal(eventsv1.GateDecided{DecidedBy: eventsv1.Actor{Type: eventsv1.ActorUser, ID: gatestest.Reviewer}, Decision: state, State: state, SubjectDigest: g.SubjectDigest()})
	return eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeGateDecided, OccurredAt: at.UTC().Truncate(time.Microsecond),
		TenantID: g.Scope.TenantID, ProjectID: g.Scope.ProjectID, Resource: eventsv1.Resource{Type: "gate", ID: string(g.ID), Version: int64(g.Version) + 1},
		Actor: eventsv1.Actor{Type: eventsv1.ActorUser, ID: gatestest.Reviewer}, CorrelationID: "req_" + id, Data: data,
	}
}

// Test_gate_join_restart: PX-05, PX-13, SD-04, SD-10, SD-11, IT-03, DQ-04
// (acceptance `gate_join_restart`). A decision is durable before any
// progression; whatever interrupts the progression (a failed or lost commit, an
// outage of an event, a restart of every service, a redelivered or reordered
// wakeup, a missed wakeup) is repaired from the durable records by
// reconciliation, which re-checks current authorization and the current subject
// before it moves the step, moves it once, and allocates no attempt, ordinal,
// capacity reservation, writer slot or workload for the gate.
func Test_gate_join_restart(t *testing.T) {
	e := New(t, Options{})
	e.GrantAll()
	ctx := context.Background()
	scope := ScopeA
	tenant := scope.TenantID

	startsBefore := len(e.X.Workloads.Starts())
	noWorkload := func(t *testing.T, what string) {
		t.Helper()
		if d := len(e.X.Workloads.Starts()); d != startsBefore {
			t.Fatalf("%s: a workload was started for a gate: %d -> %d", what, startsBefore, d)
		}
	}

	t.Run("a decision survives restarts and progresses once from the durable record", func(t *testing.T) {
		s := e.Reviewed(scope)
		before := e.Snapshot(s, "review")
		// Restarts before the decision change nothing and request nothing again.
		for i := 0; i < 2; i++ {
			e.Restart()
			if rep := e.Reconcile(tenant); rep.Requested != 0 || rep.Completed != 0 || rep.Failed != 0 {
				t.Fatalf("reconciliation after restart %d created or moved something: %+v", i, rep)
			}
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("restarts changed the pending gate:\n before %+v\n after  %+v", before, after)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatalf("a pending gate let its successor run: %v", err)
		}
		g := e.GateOf(s.Run, "review")
		if g.ID != s.Gate.ID {
			t.Fatalf("a restart produced another gate %s, want %s", g.ID, s.Gate.ID)
		}

		e.MustApprove(g)
		e.Restart() // the decision is committed; the process died before progressing
		if rep := e.Reconcile(tenant); rep.Completed != 1 {
			t.Fatalf("after the restart: %+v", rep)
		}
		if rep := e.Reconcile(tenant); rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("repeated processing moved the step again: %+v", rep)
		}
		done := e.Snapshot(s, "review")
		if done.StepStatus != string(execution.StepSucceeded) || done.Attempts != 0 || done.Started != 0 || done.Reservations != before.Reservations || done.WriterSlots != 0 {
			t.Fatalf("the gate step after approval: %+v", done)
		}
		if n := e.Transitions(s.Run, "review"); n != 1 {
			t.Fatalf("%d step transitions", n)
		}
		if n := e.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'review'`, string(s.Run.ID)); n != 0 {
			t.Fatalf("%d attempts for the gate step", n)
		}
		// The successor runs exactly once, after another restart.
		e.Restart()
		in, err := e.ClaimErr(tenant)
		if err != nil || in.StepID != "deploy" || in.Ordinal != 1 {
			t.Fatalf("claim: %v %+v", err, in)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatalf("a second claim: %v", err)
		}
		// Audit and events exist once for the request and the decision.
		if after := e.Snapshot(s, "review"); after.GateAudit != 2 || after.GateOutbox != 2 {
			t.Fatalf("gate audit/outbox rows: %+v", after)
		}
		if n := e.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'deploy'`, string(s.Run.ID)); n != 1 {
			t.Fatalf("%d deploy attempts", n)
		}
		noWorkload(t, "the restarts")
	})

	t.Run("a failed progression commit is repaired after a restart", func(t *testing.T) {
		s := e.Reviewed(scope)
		e.MustApprove(s.Gate)
		before := e.Snapshot(s, "review")
		e.F.Commit(2)
		rep, err := e.Gates.Reconcile(ctx, tenant)
		e.F.Clear()
		if err != nil || rep.Completed != 0 || rep.Failed == 0 {
			t.Fatalf("reconcile during the fault: %+v %v", rep, err)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a failed progression left effects:\n before %+v\n after  %+v", before, after)
		}
		e.Restart()
		if rep := e.Reconcile(tenant); rep.Completed != 1 {
			t.Fatalf("after the restart: %+v", rep)
		}
		if n := e.Transitions(s.Run, "review"); n != 1 {
			t.Fatalf("%d step transitions", n)
		}
	})

	t.Run("a lost progression acknowledgment leaves one transition after a restart", func(t *testing.T) {
		s := e.FinalGate(scope)
		e.MustApprove(s.Gate)
		e.F.Commit(1)
		rep, err := e.Gates.Reconcile(ctx, tenant)
		e.F.Clear()
		if err != nil || rep.Failed == 0 {
			t.Fatalf("reconcile with a lost acknowledgment: %+v %v", rep, err)
		}
		e.Restart()
		done := e.Snapshot(s, "review")
		if rep := e.Reconcile(tenant); rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("the committed progression was applied again: %+v", rep)
		}
		if again := e.Snapshot(s, "review"); again != done {
			t.Fatalf("the restart changed the committed progression:\n %+v\n %+v", done, again)
		}
		if n := e.Transitions(s.Run, "review"); n != 1 || done.StepStatus != string(execution.StepSucceeded) || done.Attempts != 0 {
			t.Fatalf("%d transitions, state %+v", n, done)
		}
	})

	t.Run("an execution event outage never wedges the run and cancellation stays possible", func(t *testing.T) {
		s := e.FinalGate(scope)
		e.MustApprove(s.Gate)
		before := e.Snapshot(s, "review")
		e.F.Fail(PointEventAppend)
		for i := 0; i < 3; i++ {
			rep, err := e.Gates.Reconcile(ctx, tenant)
			if err != nil || rep.Failed == 0 || rep.Completed != 0 {
				t.Fatalf("pass %d: %+v %v", i, rep, err)
			}
			e.Restart()
		}
		e.F.Clear()
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("repeated failed passes left effects:\n before %+v\n after  %+v", before, after)
		}
		if cur := e.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Invalidation != nil {
			t.Fatalf("a failing progression disturbed the decision: %+v", cur)
		}
		// Cancellation is never blocked by an approval waiting for its progression.
		if _, err := e.Svc.Cancel(ctx, execution.Caller{PrincipalID: gatestest.Engineer, RequestID: "req_t073"}, scope, s.Run.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if rep := e.Reconcile(tenant); rep.Completed != 0 {
			t.Fatalf("an approval completed a cancelled run: %+v", rep)
		}
		if got := e.Snapshot(s, "review").StepStatus; got == string(execution.StepSucceeded) {
			t.Fatalf("the gate step succeeded in a cancelled run: %s", got)
		}
	})

	t.Run("a failed request is repaired after a restart with the same gate identity", func(t *testing.T) {
		s := e.ReadyRun(scope)
		id := gates.GateIDFor(s.Run.Scope, s.Run.ID, "review")
		e.F.Fail(PointEventAppend + ":gate.requested")
		rep, err := e.Gates.Reconcile(ctx, tenant)
		e.F.Clear()
		if err != nil || rep.Failed == 0 {
			t.Fatalf("reconcile during the fault: %+v %v", rep, err)
		}
		e.Restart()
		if rep := e.Reconcile(tenant); rep.Requested != 1 {
			t.Fatalf("after the restart: %+v", rep)
		}
		g := e.GateOf(s.Run, "review")
		after := e.Snapshot(s, "review")
		if g.ID != id || after.GateRows != 1 || after.Pins != 1 || after.GateAudit != 1 || after.GateOutbox != 1 || after.Attempts != 0 {
			t.Fatalf("gate %s: %+v", g.ID, after)
		}
	})

	t.Run("the decision wakeup runs in the consumer's transaction and a redelivery changes nothing", func(t *testing.T) {
		binding, err := e.Store.DecisionBinding("")
		if err != nil {
			t.Fatal(err)
		}
		deliver := func(g *gates.Gate, state, id string) error {
			return storage.WithTenant(ctx, e.F.DB(), g.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return binding.Handle(ctx, tx, decidedEnvelope(id, g, state, e.X.Clock.Now()))
			})
		}
		s := e.FinalGate(scope)

		// A notification for a gate nobody decided grants nothing.
		before := e.Snapshot(s, "review")
		if err := deliver(s.Gate, "approved", "evt_t073_forged"); err != nil {
			t.Fatalf("a notification for a pending gate: %v", err)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a notification granted something:\n before %+v\n after  %+v", before, after)
		}

		e.MustApprove(s.Gate)
		approved := e.Snapshot(s, "review")
		// A failure inside the consumer's transaction rolls back the step
		// progression it made and surfaces for redelivery.
		for _, f := range []fault{afterAuthority, afterEvent, atCommit} {
			f.arm(e.F)
			if err := deliver(s.Gate, "approved", "evt_t073_1"); err == nil {
				t.Fatalf("%s: the wakeup reported success", f.name)
			}
			e.F.Clear()
			if after := e.Snapshot(s, "review"); after != approved {
				t.Fatalf("%s: a failed wakeup left effects:\n before %+v\n after  %+v", f.name, approved, after)
			}
		}
		// The redelivery completes the step once, duplicates and a late
		// notification of an earlier state change nothing.
		e.F.Reset()
		if err := deliver(s.Gate, "approved", "evt_t073_1"); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		sameXid(t, "the wakeup transaction", e.F.Trace())
		done := e.Snapshot(s, "review")
		if done.StepStatus != string(execution.StepSucceeded) || done.Attempts != 0 || done.Reservations != approved.Reservations {
			t.Fatalf("the wakeup left compute or the wrong state: %+v", done)
		}
		for _, id := range []string{"evt_t073_1", "evt_t073_0", "evt_t073_2"} {
			if err := deliver(s.Gate, "approved", id); err != nil {
				t.Fatalf("duplicate or reordered wakeup %s: %v", id, err)
			}
		}
		if again := e.Snapshot(s, "review"); again != done {
			t.Fatalf("a duplicate wakeup changed state:\n %+v\n %+v", done, again)
		}
		if n := e.Transitions(s.Run, "review"); n != 1 {
			t.Fatalf("%d step transitions", n)
		}

		// A stale approval is not an error for the consumer and moves nothing; the
		// scan, not the wakeup, records the definitive staleness.
		stale := e.FinalGate(scope)
		e.MustApprove(stale.Gate)
		e.X.EditSpec(scope, stale.P.Item, "# Plan\nChanged after the approval.")
		before = e.Snapshot(stale, "review")
		if err := deliver(stale.Gate, "approved", "evt_t073_stale"); err != nil {
			t.Fatalf("a wakeup for a stale approval: %v", err)
		}
		if after := e.Snapshot(stale, "review"); after != before {
			t.Fatalf("a wakeup acted on a stale approval:\n before %+v\n after  %+v", before, after)
		}
		if rep := e.Reconcile(tenant); rep.Invalidated != 1 || rep.Completed != 0 {
			t.Fatalf("the scan did not record the staleness: %+v", rep)
		}
		// A rejection wakeup fails the step once, with no attempt.
		rejected := e.FinalGate(scope)
		if _, err := e.Decide(rejected.Gate, gatestest.Reviewer, gates.DecisionReject); err != nil {
			t.Fatal(err)
		}
		if err := deliver(e.Current(rejected.Gate), "rejected", "evt_t073_rej"); err != nil {
			t.Fatal(err)
		}
		if err := deliver(e.Current(rejected.Gate), "rejected", "evt_t073_rej"); err != nil {
			t.Fatal(err)
		}
		if got := e.Snapshot(rejected, "review"); got.StepStatus != string(execution.StepFailed) || got.Attempts != 0 || e.Transitions(rejected.Run, "review") != 1 {
			t.Fatalf("rejection wakeup: %+v", got)
		}
	})

	t.Run("a change made while the services were down is found after the restart", func(t *testing.T) {
		s := e.Reviewed(scope)
		e.MustApprove(s.Gate)
		before := e.Snapshot(s, "review")
		e.X.EditSpec(scope, s.P.Item, "# Plan\nChanged while the services were down.")
		e.Restart()
		rep := e.Reconcile(tenant)
		if rep.Completed != 0 || rep.Invalidated != 1 {
			t.Fatalf("reconcile: %+v", rep)
		}
		after := e.Snapshot(s, "review")
		if after.StepStatus != string(execution.StepPending) || after.Invalidations != 1 {
			t.Fatalf("a stale approval was consumed: %+v", after)
		}
		noCompute(t, "the stale approval", before, after)
		e.Restart()
		if rep := e.Reconcile(tenant); rep.Invalidated != 0 || rep.Completed != 0 {
			t.Fatalf("the staleness was recorded again: %+v", rep)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a reviewer revoked while the services were down waits for restored authority", func(t *testing.T) {
		s := e.Reviewed(scope)
		e.X.Grant(scope, "u073_leaving", identity.RoleReviewer)
		if _, err := e.Decide(s.Gate, "u073_leaving", gates.DecisionApprove); err != nil {
			t.Fatal(err)
		}
		before := e.Snapshot(s, "review")
		e.X.Revoke(scope, "u073_leaving")
		e.Restart()
		if rep := e.Reconcile(tenant); rep.Completed != 0 || rep.Invalidated != 0 {
			t.Fatalf("a revoked reviewer's approval was consumed or invalidated: %+v", rep)
		}
		if after := e.Snapshot(s, "review"); after != before {
			t.Fatalf("a refused progression left effects:\n before %+v\n after  %+v", before, after)
		}
		if err := e.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		e.X.Grant(scope, "u073_leaving", identity.RoleReviewer)
		e.Restart()
		if rep := e.Reconcile(tenant); rep.Completed != 1 {
			t.Fatalf("after the reviewer was restored: %+v", rep)
		}
		if in, err := e.ClaimErr(tenant); err != nil || in.Ordinal != 1 {
			t.Fatalf("claim: %v %+v", err, in)
		}
	})

	t.Run("an approval that expired while the services were down is never renewed", func(t *testing.T) {
		f := New(t, Options{GateTTL: time.Minute})
		f.GrantAll()
		s := f.Reviewed(scope)
		f.MustApprove(s.Gate)
		before := f.Snapshot(s, "review")
		f.X.Clock.Advance(2 * time.Minute)
		for i := 0; i < 2; i++ {
			f.Restart()
			if rep := f.Reconcile(tenant); rep.Completed != 0 {
				t.Fatalf("restart %d: an expired approval was consumed: %+v", i, rep)
			}
		}
		if err := f.NothingRunnable(tenant); err != nil {
			t.Fatal(err)
		}
		after := f.Snapshot(s, "review")
		if after.StepStatus != string(execution.StepPending) || after.GateAudit != before.GateAudit || after.GateOutbox != before.GateOutbox || after.Invalidations != 0 {
			t.Fatalf("expiry rewrote history or progressed:\n before %+v\n after  %+v", before, after)
		}
		noCompute(t, "the expired approval", before, after)
		if cur := f.Current(s.Gate); cur.Status != gates.StatusApproved || cur.Decision == nil {
			t.Fatalf("the decision of an expired approval changed: %+v", cur)
		}
	})

	t.Run("recovery allocates no job, ordinal or capacity for gates", func(t *testing.T) {
		e.Quiesce()
		var runs []Scenario
		e.Keep = true
		defer func() { e.Keep = false }()
		for i := 0; i < 3; i++ {
			runs = append(runs, e.FinalGate(scope))
		}
		reservations := e.X.Count(`SELECT count(*) FROM execution_reservations`)
		attempts := e.X.Count(`SELECT count(*) FROM step_attempts WHERE step_id = 'review'`)
		for i, s := range runs {
			if i%2 == 0 {
				e.MustApprove(s.Gate)
			}
			e.Restart()
		}
		for i := 0; i < 2; i++ {
			e.Restart()
			e.Reconcile(tenant)
		}
		if n := e.X.Count(`SELECT count(*) FROM execution_reservations`); n != reservations {
			t.Fatalf("reservations %d -> %d: a gate held capacity", reservations, n)
		}
		if n := e.X.Count(`SELECT count(*) FROM step_attempts WHERE step_id = 'review'`); n != attempts {
			t.Fatalf("attempts %d -> %d: a gate allocated one", attempts, n)
		}
		if e.X.Count(`SELECT count(*) FROM step_runs WHERE step_id = 'review' AND attempts_started <> 0`) != 0 {
			t.Fatal("an attempt ordinal was consumed by a gate step")
		}
		for i, s := range runs {
			want := execution.StepPending
			if i%2 == 0 {
				want = execution.StepSucceeded
			}
			if got := e.Snapshot(s, "review").StepStatus; got != string(want) {
				t.Fatalf("run %d gate step %s, want %s", i, got, want)
			}
		}
		noWorkload(t, "recovery")
		if _, err := e.ClaimErr(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a gate was claimed as a Job: %v", err)
		}
	})
}
