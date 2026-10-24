package kube_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// Test_deadlines_and_missing_workload (PX-09, PX-A06): a startup timeout is not an
// execution timeout; a confirmed workload that disappears is reported and never
// recreated; unknown launch effects are never replayed automatically.
func Test_deadlines_and_missing_workload(t *testing.T) {
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID

	t.Run("startup timeout: the primary never starts", func(t *testing.T) {
		lim := executiontest.DefaultLimit()
		lim.StartupBudget = 12 * time.Second
		s := newStack(t, stackOptions{Limits: &lim})
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}, Image: unpullable, RetryMax: 3})
		if ok, err := s.svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := s.e.Get(run)
		in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: agg.Attempts[0].ID, WorkloadName: agg.Attempts[0].WorkloadName}
		s.until(2*time.Minute, "the startup deadline", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.Status == execution.AttemptFailed, string(at.Status)
		})
		at, ctl := s.attempt(in)
		if at.Reason != execution.ReasonStartupTimeout || at.Class != execution.ClassTimeout || at.StartedAt != nil {
			t.Fatalf("reason=%s class=%s started=%v", at.Reason, at.Class, at.StartedAt)
		}
		// The Job is deleted by termination; the effect stays unknown, because a
		// Pod that never reached its primary container is not proof it never will
		// have: nothing replays automatically.
		s.until(2*time.Minute, "the Job to be gone and termination confirmed", func() (bool, string) {
			_, present := s.jobOf(in)
			_, c := s.attempt(in)
			return !present && c.TerminationConfirmedAt != nil, fmt.Sprintf("job present=%v termination confirmed=%v pods=%d", present, c.TerminationConfirmedAt != nil, len(s.podsOf(in)))
		})
		_, ctl = s.attempt(in)
		if ctl.TerminationConfirmedAt == nil || ctl.Launch != execution.LaunchUnknown {
			t.Fatalf("termination=%v launch=%s", ctl.TerminationConfirmedAt, ctl.Launch)
		}
		for i := 0; i < 3; i++ {
			s.tick()
		}
		if got := s.attemptsOf(run); got != 1 {
			t.Fatalf("%d attempts: an unknown launch must not be replayed", got)
		}
		if s.e.Get(run).Run.Status.Terminal() {
			t.Fatal("an unknown effect settled without evidence")
		}
		if _, c := s.attempt(in); c.Settled() {
			t.Fatal("settled with unknown launch evidence")
		}
		// Only a trusted reconciliation with affirmative evidence unblocks it.
		s.e.Effects.Set(execution.VerdictSettled, nil)
		s.until(time.Minute, "the trusted reconciliation to settle the attempt", func() (bool, string) {
			_, c := s.attempt(in)
			return c.Settled(), "blocked"
		})
		if got := s.attemptsOf(run); got != 1 {
			t.Fatalf("%d attempts: reconciliation settles, it does not replay", got)
		}
	})

	t.Run("execution timeout after a real start", func(t *testing.T) {
		s := newStack(t)
		// The wrapper cannot report its own timeout (the result route is down), so
		// execution's execution deadline is what ends the attempt.
		s.srv.RefuseResults.Store(true)
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "sleep 300", Timeout: 20, Outputs: []string{"v"}})
		if ok, err := s.svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := s.e.Get(run)
		in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: agg.Attempts[0].ID, WorkloadName: agg.Attempts[0].WorkloadName}
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		s.until(2*time.Minute, "the execution deadline", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
		at, ctl := s.attempt(in)
		if at.Reason != execution.ReasonTimeout || ctl.Launch != execution.LaunchStarted || !ctl.Settled() {
			t.Fatalf("reason=%s launch=%s settled=%v", at.Reason, ctl.Launch, ctl.Settled())
		}
		if _, ok := s.jobOf(in); ok {
			t.Fatal("the timed-out Job survives")
		}
		if got := s.e.Get(run).Run.Status; got != execution.RunFailed {
			t.Fatalf("run %s", got)
		}
	})

	t.Run("missing confirmed workload is reported and never recreated", func(t *testing.T) {
		s := newStack(t)
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "sleep 300", Outputs: []string{"v"}, RetryMax: 3})
		if ok, err := s.svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := s.e.Get(run)
		in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: agg.Attempts[0].ID, WorkloadName: agg.Attempts[0].WorkloadName,
			StepID: agg.Attempts[0].StepID, ActionKey: agg.Attempts[0].ActionKey, SpecFingerprint: agg.Attempts[0].SpecFingerprint,
			ContextDigest: agg.Attempts[0].ContextDigest, Access: agg.Attempts[0].Access, Purpose: execution.PurposeCleanup}
		s.until(time.Minute, "the primary container to start", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.StartedAt != nil, "not started"
		})
		// The workload is deleted behind the executor's back.
		s.mustKubectl("-n", s.ns(tenant), "delete", "job", in.WorkloadName, "--cascade=foreground", "--wait=true")

		// A restarted controller: new executor and reconciler, no memory.
		x := s.newRuntime(s.c.client)
		if _, err := x.Start(ctx, *in); !errors.Is(err, kube.ErrNotDispatchable) {
			t.Fatalf("redrive of a disappeared confirmed workload: %v", err)
		}
		s.until(2*time.Minute, "the loss to be reported and the attempt to fail", func() (bool, string) {
			at, _ := s.attempt(in)
			return at.Status == execution.AttemptFailed, string(at.Status)
		})
		at, ctl := s.attempt(in)
		if at.Reason == execution.ReasonStartupTimeout || ctl.Launch != execution.LaunchStarted {
			t.Fatalf("reason=%s launch=%s", at.Reason, ctl.Launch)
		}
		for i := 0; i < 3; i++ {
			s.tick()
		}
		if _, ok := s.jobOf(in); ok {
			t.Fatal("a disappeared confirmed workload was recreated")
		}
		if got := s.attemptsOf(run); got != 1 {
			t.Fatalf("%d attempts: a started workload with unknown effects must not be replayed", got)
		}
		if s.cl.calls("create job") != 1 {
			t.Fatalf("%d creates", s.cl.calls("create job"))
		}
	})
}
