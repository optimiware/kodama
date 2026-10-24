package postgres_test

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// infraFail reports an infrastructure failure the executor can prove never
// launched the command, and confirms the workload gone.
func infraFail(t *testing.T, e *executiontest.Env, in *execution.AttemptSpec) {
	t.Helper()
	if err := e.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}); err != nil {
		t.Fatal(err)
	}
	if err := e.Terminate(in, execution.LaunchNotStarted); err != nil {
		t.Fatal(err)
	}
}

// Test_retry_bounds: PX-07. Backoff is persisted, the attempt count is bounded,
// the action key is stable, and command or policy failures are never replayed.
func Test_retry_bounds(t *testing.T) {
	t.Run("bounded, backed off, stable action key", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("job", readRun, "retry: {maxAttempts: 3, backoffSeconds: 45, on: [infrastructure, transient]}")))
		run := e.MustAdmit(p, def, nil)
		var key string
		seen := map[string]bool{}
		for ordinal := 1; ordinal <= 3; ordinal++ {
			in := e.MustClaim(p.Scope.TenantID)
			if in.Ordinal != ordinal {
				t.Fatalf("ordinal %d, want %d", in.Ordinal, ordinal)
			}
			if key == "" {
				key = in.ActionKey
			}
			if in.ActionKey != key || seen[in.WorkloadName] {
				t.Fatalf("attempt %d: action key %q (want %q), job %q repeated=%v", ordinal, in.ActionKey, key, in.WorkloadName, seen[in.WorkloadName])
			}
			seen[in.WorkloadName] = true
			infraFail(t, e, in)
			agg := e.Get(run)
			st := agg.Step("job")
			if ordinal < 3 {
				if st.Status != execution.StepPending || st.NextAttemptAt == nil {
					t.Fatalf("after attempt %d: step %s next %v", ordinal, st.Status, st.NextAttemptAt)
				}
				// The backoff is a persisted fact, not an in-memory timer.
				var next time.Time
				e.Scalar(&next, `SELECT next_attempt_at FROM step_runs WHERE run_id = $1`, string(run.ID))
				if d := next.Sub(e.Clock.Now()); d < 40*time.Second || d > 46*time.Second {
					t.Fatalf("persisted backoff %s", d)
				}
				if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
					t.Fatalf("claimed during backoff: %v", err)
				}
				// A restarted controller sees the same backoff.
				if _, err := e.NewService(executiontest.Options{}).ClaimRunnable(t.Context(), p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
					t.Fatalf("a restarted controller ignored the backoff: %v", err)
				}
				e.Clock.Advance(46 * time.Second)
			}
		}
		agg := e.Get(run)
		if agg.Step("job").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
			t.Fatalf("step %s run %s after exhausting attempts", agg.Step("job").Status, agg.Run.Status)
		}
		e.Clock.Advance(time.Hour)
		if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) || attemptCount(e, run.ID) != 3 {
			t.Fatalf("exceeded the attempt bound: %v, %d attempts", err, attemptCount(e, run.ID))
		}
		if reservationsHeld(e) != 0 {
			t.Fatal("reservations leaked across retries")
		}
	})

	t.Run("default is one attempt", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("job", readRun)))
		run := e.MustAdmit(p, def, nil)
		infraFail(t, e, e.MustClaim(p.Scope.TenantID))
		if agg := e.Get(run); agg.Run.Status != execution.RunFailed || attemptCount(e, run.ID) != 1 {
			t.Fatalf("run %s, %d attempts", agg.Run.Status, attemptCount(e, run.ID))
		}
	})

	t.Run("explicit command and validation failures are not replayed", func(t *testing.T) {
		for name, finish := range map[string]func(*executiontest.Env, *execution.AttemptSpec){
			"command failure": func(e *executiontest.Env, in *execution.AttemptSpec) { _, _ = e.Fail(in) },
			"missing outputs": func(e *executiontest.Env, in *execution.AttemptSpec) { _, _ = e.Succeed(in, nil, "") },
		} {
			e := executiontest.New(t)
			e.GrantAll()
			p := e.NewProject(executiontest.ScopeA)
			def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("job", readRun,
				"outputs: {report: {type: string}}", "retry: {maxAttempts: 5, on: [infrastructure, transient]}")))
			run := e.MustAdmit(p, def, nil)
			in := e.MustClaim(p.Scope.TenantID)
			e.Start(in)
			finish(e, in)
			if agg := e.Get(run); agg.Run.FailureIntentAt == nil || agg.Run.Status == execution.RunSucceeded {
				t.Fatalf("%s: run %s without failure intent", name, agg.Run.Status)
			}
			if err := e.Terminate(in, execution.LaunchStarted); err != nil {
				t.Fatal(err)
			}
			if agg := e.Get(run); agg.Step("job").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
				t.Fatalf("%s: step %s run %s after settlement", name, agg.Step("job").Status, agg.Run.Status)
			}
			e.Clock.Advance(time.Hour)
			if _, err := e.Claim(p.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) || attemptCount(e, run.ID) != 1 {
				t.Fatalf("%s: replayed (%v, %d attempts)", name, err, attemptCount(e, run.ID))
			}
		}
	})

	t.Run("a transient failure with a retry budget but not its class fails", func(t *testing.T) {
		e := executiontest.New(t)
		e.GrantAll()
		p := e.NewProject(executiontest.ScopeA)
		def := e.Define(p.Scope, pipelinetest.Doc(pipelinetest.Container("job", readRun, "retry: {maxAttempts: 3, on: [infrastructure]}")))
		run := e.MustAdmit(p, def, nil)
		in := e.MustClaim(p.Scope.TenantID)
		if err := e.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassTransient, Reason: execution.ReasonTransient, Launch: execution.LaunchNotStarted}); err != nil {
			t.Fatal(err)
		}
		if agg := e.Get(run); agg.Run.FailureIntentAt == nil {
			t.Fatal("a failure whose class is not retryable did not record failure intent immediately")
		}
	})
	_ = domain.RunID("")
	_ = runtimev1.OutcomeCompleted
}
