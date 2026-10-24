package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const gateStep = "{id: approve, kind: gate, needs: [build]}"

// Test_deadlines_and_failure_drain: PX-09, PX-08, PX-11. Startup and execution
// deadlines are distinct, a timeout fails the attempt, siblings are drained, and
// capacity is retained until settlement. A gate wait consumes no Job.
func Test_deadlines_and_failure_drain(t *testing.T) {
	ctx := context.Background()

	t.Run("startup deadline", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("a", readRun, "timeoutSeconds: 3600"), job("b", readRun, "timeoutSeconds: 600")))
		run := e.MustAdmit(p, def, nil)
		won := claimAll(t, e, p.Scope.TenantID, 4)
		if len(won) != 2 {
			t.Fatalf("%d claims", len(won))
		}
		var a, b *execution.AttemptSpec
		for _, in := range won {
			if in.StepID == "a" {
				a = in
			} else {
				b = in
			}
		}
		e.Start(b) // b starts; a never does
		// Past the startup budget but far inside a's execution timeout.
		e.Clock.Advance(2 * time.Minute)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		aa, bb := agg.Attempt(a.AttemptID), agg.Attempt(b.AttemptID)
		if aa.Status != execution.AttemptFailed || aa.Reason != execution.ReasonStartupTimeout || aa.Class != execution.ClassTimeout {
			t.Fatalf("a: %s/%s/%s", aa.Status, aa.Reason, aa.Class)
		}
		// The started sibling was drained, not failed: it lost its authority.
		if bb.Status != execution.AttemptCancelled || bb.Reason != execution.ReasonRunFailed || agg.Control(b.AttemptID).Live() {
			t.Fatalf("b: %s/%s live %v", bb.Status, bb.Reason, agg.Control(b.AttemptID).Live())
		}
		if agg.Run.FailureIntentAt == nil || agg.Run.FailureReason != execution.ReasonStartupTimeout {
			t.Fatalf("failure intent %v/%s", agg.Run.FailureIntentAt, agg.Run.FailureReason)
		}
		// Capacity stays held until the workloads are verified gone.
		if agg.Run.Status.Terminal() || reservationsHeld(e) != 2 {
			t.Fatalf("run %s with %d reservations held before settlement", agg.Run.Status, reservationsHeld(e))
		}
		if _, err := e.Claim(p.Scope.TenantID); err == nil {
			t.Fatal("dispatch continued after failure intent")
		}
		if err := e.Terminate(a, execution.LaunchNotStarted); err != nil {
			t.Fatal(err)
		}
		if got := e.Get(run).Run.Status; got.Terminal() {
			t.Fatalf("run %s before every workload was settled", got)
		}
		if err := e.Terminate(b, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		final := e.Get(run)
		if final.Run.Status != execution.RunFailed || final.Run.Reason != execution.ReasonStartupTimeout || reservationsHeld(e) != 0 {
			t.Fatalf("run %s/%s held %d", final.Run.Status, final.Run.Reason, reservationsHeld(e))
		}
	})

	t.Run("execution deadline starts at the observed start", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("job", readRun, "timeoutSeconds: 600")))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Clock.Advance(30 * time.Second) // inside the startup budget
		e.Start(in)
		var deadline, startedAt time.Time
		e.Scalar(&deadline, `SELECT execution_deadline_at FROM step_attempts WHERE id = $1`, string(in.AttemptID))
		e.Scalar(&startedAt, `SELECT started_at FROM step_attempts WHERE id = $1`, string(in.AttemptID))
		if d := deadline.Sub(startedAt); d != 600*time.Second {
			t.Fatalf("execution deadline is %s after the start", d)
		}
		e.Clock.Advance(5 * time.Minute) // past the startup budget, inside the execution timeout
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.Get(run).Attempt(in.AttemptID); got.Status != execution.AttemptRunning {
			t.Fatalf("the startup budget failed a started attempt: %s", got.Status)
		}
		e.Clock.Advance(10 * time.Minute)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		if at := agg.Attempt(in.AttemptID); at.Status != execution.AttemptFailed || at.Reason != execution.ReasonTimeout {
			t.Fatalf("%s/%s", at.Status, at.Reason)
		}
		if agg.Run.Status != execution.RunRunning && agg.Run.FailureReason != execution.ReasonTimeout {
			t.Fatalf("run %s/%s", agg.Run.Status, agg.Run.FailureReason)
		}
		// A late result for the timed-out attempt cannot rescue it.
		if _, err := e.Succeed(in, nil, ""); err == nil {
			t.Fatal("a result for a timed-out attempt was accepted")
		}
	})

	t.Run("a late result is rejected without reconciliation", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(job("job", readRun, "timeoutSeconds: 600")))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		e.Clock.Advance(11 * time.Minute) // past the execution deadline; no reconcile
		if _, err := e.Succeed(in, nil, ""); err == nil {
			t.Fatal("a result after the execution deadline was accepted")
		}
		agg := e.Get(run)
		at := agg.Attempt(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonTimeout || at.ResultHash != "" {
			t.Fatalf("%s/%s hash %q", at.Status, at.Reason, at.ResultHash)
		}
		if agg.Run.FailureIntentAt == nil || agg.Run.FailureReason != execution.ReasonTimeout || agg.Step("job").Status == execution.StepSucceeded {
			t.Fatalf("failure intent %v/%s, step %s", agg.Run.FailureIntentAt, agg.Run.FailureReason, agg.Step("job").Status)
		}
	})

	t.Run("a gate waits in durable records without compute", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("build", readRun), gateStep, pipelinetest.Container("ship", "needs: [approve]", readRun)))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if _, err := e.Succeed(in, nil, ""); err != nil {
			t.Fatal(err)
		}
		// Nothing else is runnable: the gate is not a Job and ship needs it.
		e.Clock.Advance(24 * time.Hour)
		if _, err := e.Claim(p.Scope.TenantID); err == nil {
			t.Fatal("a gate or its successor was dispatched")
		}
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		if agg.Step("approve").Status != execution.StepPending || agg.Step("ship").Status != execution.StepPending {
			t.Fatalf("gate %s, successor %s", agg.Step("approve").Status, agg.Step("ship").Status)
		}
		if attemptCount(e, run.ID) != 1 || reservationsHeld(e) != 0 || agg.Run.Status != execution.RunRunning {
			t.Fatalf("%d attempts, %d reservations, run %s", attemptCount(e, run.ID), reservationsHeld(e), agg.Run.Status)
		}
		// The wait survives a restart.
		if _, err := e.NewService(executiontest.Options{}).Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if e.Get(run).Step("approve").Status != execution.StepPending {
			t.Fatal("the gate wait did not survive a restart")
		}
	})
}
