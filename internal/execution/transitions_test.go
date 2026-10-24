package execution

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/pipeline"
)

func wantTransitionErr(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("%s: got %v, want an invalid transition", what, err)
	}
}

// Test_execution_transitions: SD-02/SD-A02, PX-05, PX-07, PX-08, PX-12. Accepted
// and rejected run, step, attempt, cancellation, drain and retry transitions,
// without PostgreSQL, Kubernetes or an HTTP server.
func Test_execution_transitions(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"))
		r := agg.Run
		if !r.AcceptsDispatch() {
			t.Fatal("a queued, retained run refuses dispatch")
		}
		r.SourceRetention = RetentionPending
		r.RetainedRefID = "pin_1"
		if r.AcceptsDispatch() {
			t.Fatal("a run dispatches before its retained source is confirmed")
		}
		if _, err := r.ConfirmRetention("pin_other", t0); err == nil {
			t.Fatal("confirmation accepted another reference than the recorded intent")
		}
		if changed, err := r.ConfirmRetention("pin_1", t0); err != nil || !changed {
			t.Fatalf("confirm: %v %v", changed, err)
		}
		if changed, err := r.ConfirmRetention("pin_1", t0); err != nil || changed {
			t.Fatalf("a repeated confirmation must be a no-op: %v %v", changed, err)
		}
		if _, err := r.ConfirmRetention("pin_2", t0); err == nil {
			t.Fatal("a confirmed reference was replaced")
		}
		if err := r.finish(RunFailed, ReasonStepFailed, t0); err == nil {
			t.Error("a run failed without recording failure intent")
		}
		if err := r.finish(RunCancelled, ReasonCancelled, t0); err == nil {
			t.Error("a run was cancelled without a cancellation request")
		}
		if _, err := r.markRunning(t0); err != nil {
			t.Fatal(err)
		}
		if r.Status != RunRunning || r.StartedAt == nil {
			t.Fatalf("status %s", r.Status)
		}
		if !r.requestCancel(ReasonCancelled, t0) || r.requestCancel(ReasonWorkCancelled, t0) {
			t.Fatal("the first cancellation request records, repeats are no-ops")
		}
		if r.CancelReason != ReasonCancelled {
			t.Fatalf("cancel reason %s was overwritten", r.CancelReason)
		}
		if r.AcceptsDispatch() {
			t.Fatal("a cancelling run accepts dispatch")
		}
		if err := r.finish(RunSucceeded, ReasonCompleted, t0); err == nil {
			t.Error("a draining run succeeded")
		}
		if err := r.finish(RunCancelled, ReasonCancelled, t0); err != nil {
			t.Fatal(err)
		}
		for _, to := range []RunStatus{RunRunning, RunQueued, RunSucceeded, RunFailed, RunCancelled} {
			if err := r.finish(to, ReasonCompleted, t0); err == nil {
				t.Errorf("a terminal run accepted finish(%s)", to)
			}
		}
		if r.requestCancel(ReasonCancelled, t0) || r.recordFailure(ReasonStepFailed, t0) {
			t.Fatal("a terminal run accepted new intent")
		}
		if _, err := r.markRunning(t0); err == nil {
			t.Fatal("a terminal run restarted")
		}
	})

	t.Run("failure intent keeps its first reason", func(t *testing.T) {
		r := newAgg(t, nil, container("a")).Run
		if !r.recordFailure(ReasonTimeout, t0) || r.recordFailure(ReasonCommandFailed, t0) || r.FailureReason != ReasonTimeout {
			t.Fatalf("failure reason %s", r.FailureReason)
		}
	})

	t.Run("step", func(t *testing.T) {
		now := t0
		s := NewStepRun("run_1", 0, container("a", retry(2, 30, pipeline.RetryInfrastructure)), now)
		if s.ActionKey != ActionKey("run_1", "a") || s.ActionKey != NewStepRun("run_1", 0, container("a"), now).ActionKey {
			t.Fatal("the action key is not a stable function of run and step")
		}
		if err := s.succeed("", nil, now); err == nil {
			t.Error("a pending step succeeded")
		}
		if err := s.scheduleRetry(now); err == nil {
			t.Error("a pending step scheduled a retry")
		}
		if err := s.start(now); err != nil {
			t.Fatal(err)
		}
		if err := s.start(now); err == nil {
			t.Error("a running step started again")
		}
		if err := s.skip(ReasonCondition, now); err == nil {
			t.Error("a running step was skipped")
		}
		if err := s.scheduleRetry(now); err != nil {
			t.Fatal(err)
		}
		if s.Status != StepPending || s.NextAttemptAt == nil || !s.NextAttemptAt.Equal(now.Add(30*time.Second)) {
			t.Fatalf("retry scheduled to %v", s.NextAttemptAt)
		}
		if err := s.start(now); err == nil {
			t.Error("a step started inside its backoff")
		}
		later := now.Add(30 * time.Second)
		if err := s.start(later); err != nil {
			t.Fatal(err)
		}
		if err := s.scheduleRetry(later); err == nil {
			t.Error("a retry was scheduled with no attempts left")
		}
		if err := s.succeed(commitA, map[string]runtimev1.Value{"x": runtimev1.StringValue("1")}, later); err != nil {
			t.Fatal(err)
		}
		for name, op := range map[string]func() error{
			"start":   func() error { return s.start(later) },
			"succeed": func() error { return s.succeed("", nil, later) },
			"fail":    func() error { return s.fail(ReasonStepFailed, later) },
			"skip":    func() error { return s.skip(ReasonCondition, later) },
			"cancel":  func() error { return s.cancel(ReasonCancelled, later) },
			"retry":   func() error { return s.scheduleRetry(later) },
		} {
			wantTransitionErr(t, "terminal step "+name, op())
		}
	})

	t.Run("retry classes and bounds", func(t *testing.T) {
		s := NewStepRun("run_1", 0, container("a", retry(3, 0, pipeline.RetryInfrastructure)), t0)
		for class, want := range map[FailureClass]bool{ClassInfrastructure: true, ClassTransient: false, ClassCommand: false, ClassValidation: false,
			ClassPolicy: false, ClassTimeout: false, ClassCancelled: false} {
			if got := s.Retryable(class); got != want {
				t.Errorf("Retryable(%s) = %v, want %v", class, got, want)
			}
		}
		both := NewStepRun("run_1", 0, container("a", retry(2, 0, pipeline.RetryInfrastructure, pipeline.RetryTransient)), t0)
		if !both.Retryable(ClassTransient) {
			t.Error("a listed class is not retryable")
		}
		// A command failure is never retryable even if (impossibly) listed.
		bad := NewStepRun("run_1", 0, container("a", retry(5, 0)), t0)
		bad.RetryOn = []pipeline.RetryClass{"command"}
		if bad.Retryable(ClassCommand) {
			t.Error("an explicit command failure was classified retryable")
		}
		both.AttemptsStarted = 2
		if both.Retryable(ClassTransient) {
			t.Error("the attempt bound was ignored")
		}
		if def := NewStepRun("run_1", 0, container("a", func(s *pipeline.Step) { s.Retry = nil }), t0); def.MaxAttempts != 1 || def.Retryable(ClassInfrastructure) {
			t.Error("the default is one attempt and no retries")
		}
	})

	t.Run("attempt", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"))
		at := claim(t, agg, t0, "att_1")
		if at.Status != AttemptRunning || at.Reason != ReasonStarting || at.Ordinal != 1 {
			t.Fatalf("dispatch intent %s/%s #%d", at.Status, at.Reason, at.Ordinal)
		}
		if changed, err := at.observeStart("11111111-2222-3333-4444-555555555555", t0); err != nil || !changed {
			t.Fatalf("%v %v", changed, err)
		}
		if changed, err := at.observeStart("11111111-2222-3333-4444-555555555555", t0.Add(time.Second)); err != nil || changed {
			t.Fatalf("a repeated observation changed state: %v %v", changed, err)
		}
		if _, err := at.observeStart("99999999-2222-3333-4444-555555555555", t0); !errors.Is(err, ErrConflict) {
			t.Fatalf("another Job's UID: %v", err)
		}
		if at.ExecutionDeadline == nil || !at.ExecutionDeadline.Equal(t0.Add(time.Minute)) {
			t.Fatalf("execution deadline %v", at.ExecutionDeadline)
		}
		if err := at.succeed("sha256:"+hex64("a"), okResult(nil, ""), t0); err != nil {
			t.Fatal(err)
		}
		for name, op := range map[string]func() error{
			"fail":    func() error { return at.fail(ReasonCommandFailed, ClassCommand, t0) },
			"cancel":  func() error { return at.cancel(ReasonCancelled, t0) },
			"succeed": func() error { return at.succeed("sha256:"+hex64("b"), okResult(nil, ""), t0) },
			"observe": func() error { _, err := at.observeStart("11111111-2222-3333-4444-555555555555", t0); return err },
		} {
			wantTransitionErr(t, "terminal attempt "+name, op())
		}
	})

	t.Run("observation validation", func(t *testing.T) {
		for name, o := range map[string]Observation{
			"unknown kind":            {Kind: "paused"},
			"failure without class":   {Kind: ObservedFailed, Reason: ReasonInfrastructure},
			"command class observed":  {Kind: ObservedFailed, Class: ClassCommand, Reason: ReasonCommandFailed},
			"failure without reason":  {Kind: ObservedFailed, Class: ClassInfrastructure},
			"started without UID":     {Kind: ObservedStarted},
			"unknown launch evidence": {Kind: ObservedTerminated, Launch: "maybe"},
		} {
			if err := o.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("cancellation and draining", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"), container("b"), container("c", needs("a")))
		a := claim(t, agg, t0, "att_a")
		b := claim(t, agg, t0, "att_b")
		agg.Transitions = nil
		if !agg.RequestCancel(ReasonCancelled, t0) || agg.RequestCancel(ReasonCancelled, t0) {
			t.Fatal("cancel must record once and repeat as a no-op")
		}
		if agg.Run.Status.Terminal() {
			t.Fatal("a run with admitted work completed before settlement")
		}
		for _, at := range []*Attempt{a, b} {
			c := agg.Control(at.ID)
			if at.Status != AttemptCancelled || c.Live() || c.TerminationRequestedAt == nil {
				t.Errorf("%s: %s live=%v termination=%v", at.ID, at.Status, c.Live(), c.TerminationRequestedAt)
			}
		}
		if got := agg.Step("c").Status; got != StepCancelled {
			t.Errorf("a pending step is %s", got)
		}
		if len(agg.Revoked) != 2 {
			t.Errorf("%d attempts queued for termination", len(agg.Revoked))
		}
		if agg.Run.AcceptsDispatch() || agg.NextRunnable(t0.Add(time.Hour), hooks) != nil {
			t.Fatal("dispatch is possible after cancellation")
		}
		// Terminal completion requires settlement of every attempt.
		if done, _ := agg.Finish(t0); done {
			t.Fatal("finished without settlement")
		}
		agg.Control(a.ID).confirmTermination(LaunchNotStarted, t0)
		if out, err := agg.Settle(a, t0); err != nil || out != SettledNow {
			t.Fatalf("%s %v", out, err)
		}
		if done, _ := agg.Finish(t0); done {
			t.Fatal("finished with one attempt still unsettled")
		}
		if out, _ := agg.Settle(b, t0); out != SettleAwaitingEnd {
			t.Fatalf("an attempt whose workload was not confirmed gone settled: %s", out)
		}
		agg.Control(b.ID).confirmTermination(LaunchStarted, t0)
		if _, err := agg.Settle(b, t0); err != nil {
			t.Fatal(err)
		}
		if done, err := agg.Finish(t0); err != nil || !done || agg.Run.Status != RunCancelled {
			t.Fatalf("%v %v %s", done, err, agg.Run.Status)
		}
		// Settling again changes nothing.
		if out, _ := agg.Settle(a, t0); out != SettleNotNeeded {
			t.Fatalf("second settlement: %s", out)
		}
	})

	t.Run("a queued run with nothing admitted is cancelled at once", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"))
		agg.RequestCancel(ReasonCancelled, t0)
		if done, err := agg.Finish(t0); err != nil || !done || agg.Run.Status != RunCancelled || agg.Step("a").Status != StepCancelled {
			t.Fatalf("%v %v %s", done, err, agg.Run.Status)
		}
	})

	t.Run("fail-fast drain", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"), container("b"), container("c", needs("a", "b")))
		a := claim(t, agg, t0, "att_a")
		b := claim(t, agg, t0, "att_b")
		if _, err := agg.Observe(a, Observation{Kind: ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}, t0); err != nil {
			t.Fatal(err)
		}
		if err := agg.CompleteAttempt(a, "sha256:"+hex64("a"), runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("boom"),
			Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: nil}, t0); err != nil {
			t.Fatal(err)
		}
		if agg.Run.FailureIntentAt == nil || agg.Run.FailureReason != ReasonCommandFailed {
			t.Fatalf("failure intent %v/%s", agg.Run.FailureIntentAt, agg.Run.FailureReason)
		}
		if b.Status != AttemptCancelled || b.Reason != ReasonRunFailed || agg.Control(b.ID).Live() {
			t.Fatalf("sibling %s/%s", b.Status, b.Reason)
		}
		if agg.Step("c").Status != StepCancelled {
			t.Fatalf("pending step %s", agg.Step("c").Status)
		}
		if a.Status != AttemptFailed || a.Class != ClassCommand {
			t.Fatalf("failed attempt %s/%s", a.Status, a.Class)
		}
		if done, _ := agg.Finish(t0); done || agg.Run.Status != RunRunning {
			t.Fatal("the run failed before its attempts settled")
		}
		agg.Control(a.ID).confirmTermination(LaunchStarted, t0)
		agg.Control(b.ID).confirmTermination(LaunchStarted, t0)
		settleAll(t, agg, t0)
		if done, err := agg.Finish(t0); err != nil || !done || agg.Run.Status != RunFailed || agg.Step("a").Status != StepFailed {
			t.Fatalf("%v %v run %s step a %s", done, err, agg.Run.Status, agg.Step("a").Status)
		}
	})

	t.Run("retry with persisted backoff and the unknown-effect barrier", func(t *testing.T) {
		// A retryable failure waits for settlement; not-started evidence makes it safe.
		agg := newAgg(t, nil, container("a", retry(3, 20, pipeline.RetryInfrastructure)))
		a := claim(t, agg, t0, "att_1")
		if _, err := agg.Observe(a, Observation{Kind: ObservedFailed, Class: ClassInfrastructure, Reason: ReasonInfrastructure, Launch: LaunchNotStarted}, t0); err != nil {
			t.Fatal(err)
		}
		if agg.Run.FailureIntentAt != nil {
			t.Fatal("a retryable failure recorded failure intent before settlement")
		}
		agg.Control(a.ID).confirmTermination(LaunchNotStarted, t0)
		if out, err := agg.Settle(a, t0); err != nil || out != SettledNow {
			t.Fatalf("%s %v", out, err)
		}
		s := agg.Step("a")
		if s.Status != StepPending || s.NextAttemptAt == nil || !s.NextAttemptAt.Equal(t0.Add(20*time.Second)) {
			t.Fatalf("step %s next %v", s.Status, s.NextAttemptAt)
		}
		if agg.NextRunnable(t0.Add(10*time.Second), hooks) != nil || agg.NextRunnable(t0.Add(20*time.Second), hooks) == nil {
			t.Fatal("the backoff bound is wrong")
		}
		second := claim(t, agg, t0.Add(20*time.Second), "att_2")
		if second.Ordinal != 2 || second.ActionKey != a.ActionKey {
			t.Fatalf("retry attempt #%d key %s vs %s", second.Ordinal, second.ActionKey, a.ActionKey)
		}

		// An ambiguous launch blocks until trusted evidence arrives.
		amb := newAgg(t, nil, container("a", retry(3, 20, pipeline.RetryInfrastructure)))
		x := claim(t, amb, t0, "att_x")
		if _, err := amb.Observe(x, Observation{Kind: ObservedFailed, Class: ClassInfrastructure, Reason: ReasonInfrastructure, Launch: LaunchUnknown}, t0); err != nil {
			t.Fatal(err)
		}
		if x.Reason != ReasonOutcomeUnknown {
			t.Fatalf("reason %s", x.Reason)
		}
		amb.Control(x.ID).confirmTermination(LaunchUnknown, t0)
		if out, err := amb.Settle(x, t0); !errors.Is(err, ErrOutcomeUnknown) || out != SettleBlocked {
			t.Fatalf("%s %v", out, err)
		}
		if amb.Control(x.ID).Effect != EffectUnknown || amb.Control(x.ID).Settled() || amb.Step("a").Status != StepRunning {
			t.Fatal("the unknown effect did not block")
		}
		if amb.NextRunnable(t0.Add(time.Hour), hooks) != nil {
			t.Fatal("a blocked step is runnable")
		}
		if err := amb.Reconcile(x, ReconciliationEvidence{Verdict: VerdictStillUnknown, Source: "adapter"}, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := amb.Settle(x, t0); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("an inconclusive reconciliation settled: %v", err)
		}
		if err := amb.Reconcile(x, ReconciliationEvidence{Verdict: "guess"}, t0); err == nil {
			t.Fatal("unknown evidence verdicts are accepted")
		}
		if err := amb.Reconcile(x, ReconciliationEvidence{Verdict: VerdictReplaySafe, Source: "adapter"}, t0); err != nil {
			t.Fatal(err)
		}
		if out, err := amb.Settle(x, t0); err != nil || out != SettledNow || amb.Step("a").Status != StepPending {
			t.Fatalf("%s %v step %s", out, err, amb.Step("a").Status)
		}
		if x.Status != AttemptFailed || x.Reason != ReasonOutcomeUnknown {
			t.Fatalf("the failed attempt was rewritten to %s/%s", x.Status, x.Reason)
		}
	})

	t.Run("deadlines are distinct", func(t *testing.T) {
		agg := newAgg(t, nil, container("a"), container("b"))
		a := claim(t, agg, t0, "att_a")
		b := claim(t, agg, t0, "att_b")
		if _, err := agg.Observe(b, Observation{Kind: ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}, t0.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		if failed := agg.CheckDeadlines(t0.Add(30 * time.Second)); len(failed) != 0 {
			t.Fatal("a deadline fired early")
		}
		failed := agg.CheckDeadlines(t0.Add(70 * time.Second)) // startup budget (60s) passed; b's execution runs to 90s
		if len(failed) != 1 || failed[0] != a || a.Reason != ReasonStartupTimeout {
			t.Fatalf("startup deadline: %v", failed)
		}
	})
}

func ptr[T any](v T) *T { return &v }
