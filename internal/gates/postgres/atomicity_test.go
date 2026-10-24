package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/httpapi"
)

func outbox(e *gatestest.Env, typ string, id gates.ID) int {
	return e.X.CountTenant(gatestest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $2`, typ, string(id))
}

// stepTransitions counts the recorded transitions of a step out of pending.
func stepTransitions(e *gatestest.Env, run *execution.Run, step string) int {
	return count(e, `SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'step' AND step_id = $2 AND to_status <> 'pending'`, string(run.ID), step)
}

func audits(e *gatestest.Env, action string, id gates.ID) int {
	return count(e, `SELECT count(*) FROM audit WHERE action = $1 AND resource_id = $2`, action, string(id))
}

// TestAtomicGateEffects (PX-05, SD-04, SD-A03): a gate's state, audit row,
// outbox event and idempotency record commit together or not at all. An
// injected audit/outbox/idempotency failure rolls back the decision, the gate's
// attachment to its step, an invalidation and a protected claim; no attempt
// ordinal or compute reservation leaks, and the same operation succeeds once
// the fault is gone.
func TestAtomicGateEffects(t *testing.T) {
	faults, execFaults := &gatestest.Faults{}, &gatestest.Faults{}
	e := gatestest.New(t, gatestest.Options{WrapEvents: faults.Appender, WrapExecEvents: execFaults.Appender})
	e.GrantAll()
	ctx := context.Background()
	scope := gatestest.ScopeA
	boom := errors.New("injected failure")

	t.Run("a failed outbox append rolls back the decision", func(t *testing.T) {
		_, _, g := pendingGate(t, e, scope)
		faults.FailEvents(boom, "gate.decided")
		defer faults.FailEvents(nil)
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err == nil {
			t.Fatalf("the decision succeeded without its event")
		}
		cur, _ := e.Store.Get(ctx, scope, g.ID)
		if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Version != g.Version {
			t.Fatalf("the decision survived the failed append: %+v", cur)
		}
		if audits(e, "gate.decided", g.ID) != 0 || outbox(e, "gate.decided", g.ID) != 0 {
			t.Fatalf("audit or outbox rows survived the rollback")
		}
		faults.FailEvents(nil)
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatalf("after the fault: %v", err)
		}
		if audits(e, "gate.decided", g.ID) != 1 || outbox(e, "gate.decided", g.ID) != 1 {
			t.Fatalf("a decision must leave exactly one audit row and one event")
		}
	})

	t.Run("a failed idempotency record rolls back the decision", func(t *testing.T) {
		_, _, g := pendingGate(t, e, scope)
		plan, err := e.Gates.PrepareDecision(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("body"))
		id := httpapi.RequestIdentity{
			Principal: httpapi.Principal{ID: gatestest.Reviewer}, TenantID: scope.TenantID, ProjectID: scope.ProjectID, Method: "POST",
			Route: "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/gates/" + string(g.ID) + "/decisions",
			Key:   "atomic-key", Hash: httpapi.RequestHash(sum),
		}
		allow := func(context.Context, httpapi.Reference) error { return nil }
		// A response the idempotency table refuses (not 2xx) is a failure of the
		// same transaction that recorded the decision.
		_, _, _, err = e.Keyed.DecideKeyed(ctx, id, allow, plan, func(*gates.Gate) (httpapi.Outcome, error) {
			return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 500, Body: []byte(`{}`)}}, nil
		})
		if err == nil {
			t.Fatalf("a decision committed although its stored response was refused")
		}
		cur, _ := e.Store.Get(ctx, scope, g.ID)
		if cur.Status != gates.StatusPending || cur.Version != g.Version || audits(e, "gate.decided", g.ID) != 0 || outbox(e, "gate.decided", g.ID) != 0 {
			t.Fatalf("effects survived the idempotency failure: %+v", cur)
		}
		if n := count(e, `SELECT count(*) FROM idempotency WHERE key = 'atomic-key'`); n != 0 {
			t.Fatalf("an idempotency record survived")
		}
		// The key is not consumed: the retry decides once.
		_, replayed, decided, err := e.Keyed.DecideKeyed(ctx, id, allow, plan, func(g *gates.Gate) (httpapi.Outcome, error) {
			return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 200, Body: []byte(`{"ok":true}`)}}, nil
		})
		if err != nil || replayed || decided == nil {
			t.Fatalf("retry: %v %v", err, replayed)
		}
	})

	t.Run("a failed event rolls back the gate and its attachment to the step", func(t *testing.T) {
		faults.FailEvents(boom, "gate.requested")
		defer faults.FailEvents(nil)
		run2 := mustRun(t, e, scope)
		rep, err := e.Gates.Reconcile(ctx, scope.TenantID)
		if err != nil || rep.Requested != 0 || rep.Failed == 0 {
			t.Fatalf("reconcile during the fault: %+v %v", rep, err)
		}
		if n := count(e, `SELECT count(*) FROM gates WHERE run_id = $1`, string(run2.ID)); n != 0 {
			t.Fatalf("a gate survived the failed event")
		}
		if n := count(e, `SELECT count(*) FROM step_runs WHERE run_id = $1 AND gate_ref IS NOT NULL`, string(run2.ID)); n != 0 {
			t.Fatalf("the step stayed bound to a gate that was rolled back")
		}
		if n := count(e, `SELECT count(*) FROM audit WHERE action = 'gate.requested' AND metadata->>'runId' = $1`, string(run2.ID)); n != 0 {
			t.Fatalf("an audit row survived")
		}
		faults.FailEvents(nil)
		rep, err = e.Gates.Reconcile(ctx, scope.TenantID)
		if err != nil || rep.Requested != 1 {
			t.Fatalf("after the fault: %+v %v", rep, err)
		}
	})

	t.Run("a failed event rolls back an invalidation", func(t *testing.T) {
		p, _, g := pendingGate(t, e, scope)
		e.X.EditSpec(scope, p.Item, "# Plan\nChanged.")
		faults.FailEvents(boom, "gate.invalidated")
		defer faults.FailEvents(nil)
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err == nil {
			t.Fatalf("a stale gate was decided")
		}
		cur, _ := e.Store.Get(ctx, scope, g.ID)
		if cur.Invalidation != nil || audits(e, "gate.invalidated", g.ID) != 0 {
			t.Fatalf("an invalidation survived its failed event: %+v", cur.Invalidation)
		}
		faults.FailEvents(nil)
		rep, err := e.Gates.Reconcile(ctx, scope.TenantID)
		if err != nil || rep.Invalidated == 0 {
			t.Fatalf("after the fault: %+v %v", rep, err)
		}
		if cur, _ := e.Store.Get(ctx, scope, g.ID); cur.Invalidation == nil || outbox(e, "gate.invalidated", g.ID) != 1 {
			t.Fatalf("the invalidation was not recorded once")
		}
	})

	t.Run("a failed event rolls back a protected claim without leaking an ordinal or a reservation", func(t *testing.T) {
		p, run, g := pendingGate(t, e, scope)
		_ = p
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatal(err)
		}
		if rep, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Completed != 1 {
			t.Fatalf("consume: %+v %v", rep, err)
		}
		reservations := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
		attempts := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID))
		execFaults.FailEvents(boom)
		defer execFaults.FailEvents(nil)
		if _, err := e.Claim(scope.TenantID); err == nil {
			t.Fatalf("a claim committed without its events")
		}
		if n := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID)); n != reservations {
			t.Fatalf("a reservation leaked: %d -> %d", reservations, n)
		}
		if n := count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)); n != attempts {
			t.Fatalf("an attempt leaked: %d -> %d", attempts, n)
		}
		var started int
		e.X.Scalar(&started, `SELECT attempts_started FROM step_runs WHERE run_id = $1 AND step_id = 'deploy'`, string(run.ID))
		if started != 0 {
			t.Fatalf("an attempt ordinal was consumed: %d", started)
		}
		execFaults.FailEvents(nil)
		in, err := e.Claim(scope.TenantID)
		if err != nil || in.Ordinal != 1 {
			t.Fatalf("claim after the fault: %v %+v", err, in)
		}
	})

	t.Run("a refused protected claim leaks nothing either", func(t *testing.T) {
		p, run, g := pendingGate(t, e, scope)
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Gates.Reconcile(ctx, scope.TenantID); err != nil {
			t.Fatal(err)
		}
		e.X.EditSpec(scope, p.Item, "# Plan\nChanged after the gate step succeeded.")
		reservations := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
		if _, err := e.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claim: %v", err)
		}
		var started int
		e.X.Scalar(&started, `SELECT attempts_started FROM step_runs WHERE run_id = $1 AND step_id = 'deploy'`, string(run.ID))
		if n := count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID)); n != reservations || started != 0 {
			t.Fatalf("a refused claim leaked: reservations %d -> %d, ordinal %d", reservations, n, started)
		}
	})

	// progressionState captures every durable effect a gate step transition
	// could leave: step and run records, history, audit, the execution outbox,
	// and the compute a gate must never create.
	type progression struct {
		stepStatus, runStatus                                       string
		history, audit, execOutbox, attempts, reservations, started int
	}
	snapshot := func(run *execution.Run) progression {
		var p progression
		e.X.Scalar(&p.stepStatus, `SELECT status FROM step_runs WHERE run_id = $1 AND step_id = 'review'`, string(run.ID))
		e.X.Scalar(&p.runStatus, `SELECT status FROM runs WHERE id = $1`, string(run.ID))
		e.X.Scalar(&p.started, `SELECT attempts_started FROM step_runs WHERE run_id = $1 AND step_id = 'review'`, string(run.ID))
		p.history = count(e, `SELECT count(*) FROM run_history WHERE run_id = $1`, string(run.ID))
		p.audit = count(e, `SELECT count(*) FROM audit WHERE resource_id = $1 OR metadata->>'runId' = $1`, string(run.ID))
		p.execOutbox = e.X.CountTenant(scope.TenantID, `SELECT count(*) FROM outbox WHERE convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $1`, string(run.ID))
		p.attempts = count(e, `SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = 'review'`, string(run.ID))
		p.reservations = count(e, `SELECT count(*) FROM execution_reservations WHERE run_id = $1`, string(run.ID))
		return p
	}

	t.Run("a failed execution event rolls back step completion but not the approval", func(t *testing.T) {
		// The gate is the last step: completing it settles the run, which
		// appends a run event after the step and run mutations are written.
		run, g := finalGate(t, e, scope)
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, approveCmd(g)); err != nil {
			t.Fatal(err)
		}
		before := snapshot(run)
		execFaults.FailEvents(boom)
		defer execFaults.FailEvents(nil)
		if _, err := e.Store.CompleteStep(ctx, scope, g.ID); err == nil {
			t.Fatalf("the step completed without its execution events")
		}
		if after := snapshot(run); after != before {
			t.Fatalf("a failed completion left effects: %+v -> %+v", before, after)
		}
		if cur, _ := e.Store.Get(ctx, scope, g.ID); cur.Status != gates.StatusApproved || cur.Decision == nil {
			t.Fatalf("the immutable approval did not survive: %+v", cur)
		}
		execFaults.FailEvents(nil)
		out, err := e.Store.CompleteStep(ctx, scope, g.ID)
		if err != nil || !out.Completed {
			t.Fatalf("retry: %v %+v", err, out)
		}
		done := snapshot(run)
		if done.stepStatus != string(execution.StepSucceeded) || done.runStatus != string(execution.RunSucceeded) || done.attempts != 0 || done.reservations != before.reservations || done.started != 0 {
			t.Fatalf("completion left compute or the wrong state: %+v", done)
		}
		if n := stepTransitions(e, run, "review"); n != 1 {
			t.Fatalf("%d step transitions after the retry, want one", n)
		}
		// Duplicate consumption is a no-op.
		_, _ = e.Store.CompleteStep(ctx, scope, g.ID)
		if again := snapshot(run); again != done {
			t.Fatalf("a duplicate completion changed state: %+v -> %+v", done, again)
		}
		if n := stepTransitions(e, run, "review"); n != 1 {
			t.Fatalf("%d step transitions after a duplicate, want one", n)
		}
	})

	t.Run("a failed execution event rolls back step rejection but not the rejection", func(t *testing.T) {
		_, run, g := pendingGate(t, e, scope)
		cmd := approveCmd(g)
		cmd.Decision = gates.DecisionReject
		cmd.Reason = "The identified evidence does not support delivery."
		if _, err := e.Gates.Decide(ctx, gatestest.Caller(gatestest.Reviewer), scope, g.ID, cmd); err != nil {
			t.Fatal(err)
		}
		before := snapshot(run)
		execFaults.FailEvents(boom)
		defer execFaults.FailEvents(nil)
		if _, err := e.Store.RejectStep(ctx, scope, g.ID); err == nil {
			t.Fatalf("the step was rejected without its execution events")
		}
		if after := snapshot(run); after != before {
			t.Fatalf("a failed rejection left effects: %+v -> %+v", before, after)
		}
		if cur, _ := e.Store.Get(ctx, scope, g.ID); cur.Status != gates.StatusRejected || cur.Decision == nil {
			t.Fatalf("the immutable rejection did not survive: %+v", cur)
		}
		execFaults.FailEvents(nil)
		out, err := e.Store.RejectStep(ctx, scope, g.ID)
		if err != nil || !out.Rejected {
			t.Fatalf("retry: %v %+v", err, out)
		}
		done := snapshot(run)
		if done.stepStatus != string(execution.StepFailed) || done.attempts != 0 || done.reservations != before.reservations {
			t.Fatalf("rejection left compute or the wrong state: %+v", done)
		}
		if n := stepTransitions(e, run, "review"); n != 1 {
			t.Fatalf("%d step transitions after the retry, want one", n)
		}
		_, _ = e.Store.RejectStep(ctx, scope, g.ID)
		if again := snapshot(run); again != done {
			t.Fatalf("a duplicate rejection changed state: %+v -> %+v", done, again)
		}
	})
}
