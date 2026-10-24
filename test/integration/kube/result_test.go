package kube_test

import (
	"context"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// jobComplete reports whether the Job controller considers the Job complete.
func (s *stack) jobComplete(in *execution.AttemptSpec) bool {
	j, ok := s.jobOf(in)
	return ok && j.Status.Succeeded >= 1
}

// Test_result_not_inferred_from_job (PX-03, IT-08): a Job that completed with the
// entrypoint exiting zero cannot manufacture success, and cannot trigger cleanup
// before the result is durably accepted and the attempt is settled.
func Test_result_not_inferred_from_job(t *testing.T) {
	ctx := context.Background()
	tenant := executiontest.ScopeA.TenantID

	t.Run("exit zero without a durable result", func(t *testing.T) {
		const retention = 10 * time.Minute
		s := newStack(t, stackOptions{Options: func(o *kube.Options) { o.CompletedRetention = retention }})
		// The runtime acknowledges the result without committing it: the
		// entrypoint exits zero and the Job completes with no accepted result.
		s.srv.AckWithoutCommit.Store(true)
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
		if ok, err := s.svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := s.e.Get(run)
		in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: agg.Attempts[0].ID, WorkloadName: agg.Attempts[0].WorkloadName}
		s.until(3*time.Minute, "the Job to complete", func() (bool, string) { return s.jobComplete(in), "not complete" })
		if s.srv.results.Load() == 0 {
			t.Fatal("the entrypoint never submitted its result")
		}
		for i := 0; i < 8; i++ {
			s.tick()
			time.Sleep(250 * time.Millisecond)
		}
		at, ctl := s.attempt(in)
		if at.Status != execution.AttemptRunning || ctl.Settled() || s.e.Get(run).Run.Status.Terminal() {
			t.Fatalf("a completed Job manufactured an outcome: attempt=%s settled=%v run=%s", at.Status, ctl.Settled(), s.e.Get(run).Run.Status)
		}
		if _, ok := s.jobOf(in); !ok {
			t.Fatal("the completed Job was cleaned up before the result was durable")
		}
		if !s.secretExists(in) {
			t.Fatal("the attempt's recovery record was removed before settlement")
		}
		if s.attemptsOf(run) != 1 {
			t.Fatal("a completed Job without a result was replayed")
		}

		// The durable result is what succeeds the attempt.
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptrTo(int64(0)), Outputs: map[string]runtimev1.Value{"v": runtimev1.StringValue("ok")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		hash, err := runtimev1.ResultHash(res)
		must(t, err)
		if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res}}); err != nil {
			t.Fatalf("commit result: %v", err)
		}
		s.until(time.Minute, "the run to succeed", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st == execution.RunSucceeded, string(st)
		})
		// Retained for the inspection window even though the attempt is final.
		for i := 0; i < 3; i++ {
			s.tick()
		}
		if _, ok := s.jobOf(in); !ok {
			t.Fatal("the Job was deleted inside the retention window")
		}
		s.e.Clock.Advance(retention + time.Minute)
		s.until(2*time.Minute, "the retained Job to be removed after the window", func() (bool, string) {
			_, ok := s.jobOf(in)
			return !ok, "still present"
		})
		if got := s.e.Get(run).Run.Status; got != execution.RunSucceeded {
			t.Fatalf("run %s", got)
		}
	})

	t.Run("exit zero after a failed result is not success", func(t *testing.T) {
		s := newStack(t, stackOptions{Options: func(o *kube.Options) { o.CompletedRetention = 10 * time.Minute }})
		run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "exit 3", Outputs: []string{"v"}})
		if ok, err := s.svc.DispatchNext(ctx, tenant); err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		agg := s.e.Get(run)
		in := &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, AttemptID: agg.Attempts[0].ID, WorkloadName: agg.Attempts[0].WorkloadName}
		s.until(3*time.Minute, "the run to fail", func() (bool, string) {
			st := s.e.Get(run).Run.Status
			return st.Terminal(), string(st)
		})
		if got := s.e.Get(run).Run.Status; got != execution.RunFailed {
			t.Fatalf("run %s: a failed result must not succeed", got)
		}
		if at, _ := s.attempt(in); at.Status != execution.AttemptFailed {
			t.Fatalf("attempt %s", at.Status)
		}
	})
}
