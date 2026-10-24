package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const retryInfra = "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure]}"

// ambiguous drives a writer attempt to an infrastructure failure whose launch
// the executor cannot establish, then confirms the workload gone without
// evidence: the effect is unknown.
func ambiguous(t *testing.T, e *executiontest.Env, in *execution.AttemptSpec) {
	t.Helper()
	err := e.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Terminate(in, execution.LaunchUnknown); err != nil {
		t.Fatal(err)
	}
}

// Test_unknown_effect_no_retry: PX-07, PX-11, PX-14. An ambiguous external
// effect survives restart, blocks retry and writer reassignment, keeps its
// reservations, and needs affirmative reconciliation evidence. The failed
// attempt is never rewritten.
func Test_unknown_effect_no_retry(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*executiontest.Env, executiontest.Project, *execution.Run, *execution.AttemptSpec) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("deploy", writeRun, retryInfra)))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		ambiguous(t, e, in)
		return e, p, run, in
	}

	t.Run("blocks retry and writer reassignment and survives restart", func(t *testing.T) {
		e, p, run, in := setup(t)
		agg := e.Get(run)
		at := agg.Attempt(in.AttemptID)
		c := agg.Control(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutcomeUnknown || c.Effect != execution.EffectUnknown || c.Settled() {
			t.Fatalf("attempt %s/%s effect %s settled %v", at.Status, at.Reason, c.Effect, c.Settled())
		}
		if st := agg.Step("deploy"); st.Status != execution.StepRunning {
			t.Fatalf("step is %s, want blocked in running", st.Status)
		}
		e.Clock.Advance(time.Hour) // far beyond any backoff
		check := func(svc *execution.Service, when string) {
			t.Helper()
			if _, err := svc.ClaimRunnable(ctx, p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
				t.Fatalf("%s: a blocked step was claimed: %v", when, err)
			}
			if attemptCount(e, run.ID) != 1 {
				t.Fatalf("%s: %d attempts", when, attemptCount(e, run.ID))
			}
			if reservationsHeld(e) != 1 {
				t.Fatalf("%s: capacity reservation not retained: %d", when, reservationsHeld(e))
			}
			var writer string
			e.Scalar(&writer, `SELECT coalesce(writer_attempt_id, '') FROM workspaces WHERE id = $1`, string(p.Workspace))
			if writer != string(in.AttemptID) {
				t.Fatalf("%s: writer slot reassigned to %q", when, writer)
			}
		}
		check(e.Svc, "before restart")
		// Restart: a fresh service over the same database, with no effect adapter.
		restarted := e.NewService(executiontest.Options{NoEffects: true})
		if _, err := restarted.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		check(restarted, "after restart without an effect adapter")
		// Reconciliation that stays unknown changes nothing either.
		e.Effects.Set(execution.VerdictStillUnknown, nil)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		check(e.Svc, "after an inconclusive reconciliation")
		if n := e.Count(`SELECT count(*) FROM attempt_evidence WHERE kind = 'reconciliation'`); n < 1 {
			t.Fatal("the inconclusive reconciliation left no evidence")
		}
	})

	t.Run("affirmative replay-safe evidence settles it and allows one new attempt", func(t *testing.T) {
		e, p, run, in := setup(t)
		e.Effects.Set(execution.VerdictReplaySafe, nil)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		at := agg.Attempt(in.AttemptID)
		// The unknown attempt keeps its failed outcome; it was never "fixed".
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutcomeUnknown {
			t.Fatalf("attempt rewritten to %s/%s", at.Status, at.Reason)
		}
		if !agg.Control(in.AttemptID).Settled() || reservationsHeld(e) != 0 {
			t.Fatal("reconciliation did not settle and release exactly once")
		}
		if st := agg.Step("deploy"); st.Status != execution.StepPending || st.NextAttemptAt == nil {
			t.Fatalf("step %s next %v, want pending with persisted backoff", st.Status, st.NextAttemptAt)
		}
		if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("the backoff was ignored: %v", err)
		}
		e.Clock.Advance(31 * time.Second)
		next := e.MustClaim(p.Scope.TenantID)
		if next.Ordinal != 2 || next.ActionKey != in.ActionKey || next.AttemptID == in.AttemptID {
			t.Fatalf("retry %+v after %+v", next, in)
		}
	})

	t.Run("settled but not replay-safe evidence fails the step instead of retrying", func(t *testing.T) {
		e, p, run, _ := setup(t)
		e.Effects.Set(execution.VerdictSettled, nil)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		if agg.Step("deploy").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
			t.Fatalf("step %s run %s", agg.Step("deploy").Status, agg.Run.Status)
		}
		e.Clock.Advance(time.Hour)
		if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a failed run dispatched again: %v", err)
		}
		if attemptCount(e, run.ID) != 1 {
			t.Fatal("a second attempt was created")
		}
	})

	t.Run("an error class alone proves nothing: a started command is never replayed", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("deploy", readRun, retryInfra)))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		e.Start(in)
		if err := e.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchStarted}); err != nil {
			t.Fatal(err)
		}
		if err := e.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		if agg.Run.Status != execution.RunFailed || attemptCount(e, run.ID) != 1 {
			t.Fatalf("run %s attempts %d: an infrastructure failure after launch was replayed", agg.Run.Status, attemptCount(e, run.ID))
		}
	})

	t.Run("not-started evidence makes the retry safe without reconciliation", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("deploy", readRun, retryInfra)))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		if err := e.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}); err != nil {
			t.Fatal(err)
		}
		if err := e.Terminate(in, execution.LaunchNotStarted); err != nil {
			t.Fatal(err)
		}
		agg := e.Get(run)
		if st := agg.Step("deploy"); st.Status != execution.StepPending || agg.Run.Status != execution.RunRunning {
			t.Fatalf("step %s run %s", st.Status, agg.Run.Status)
		}
	})
}
