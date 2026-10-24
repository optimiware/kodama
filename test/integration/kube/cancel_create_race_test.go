package kube_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// Test_cancel_create_race (PX-A06, PX-08, IT-08): a cancellation commits while the
// Job create is in flight. The late workload's actual entrypoint admission is
// denied, the workload is deleted, late results are rejected, and the launch
// evidence the executor collected keeps settlement honest.
func Test_cancel_create_race(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: slow(60), Outputs: []string{"v"}})
	in := s.claim(executiontest.ScopeA.TenantID)

	// Pause the in-flight create after the API server applied it.
	created, release := make(chan struct{}), make(chan struct{})
	s.cl.After = func(op string, n int) {
		close(created)
		<-release
	}
	done := make(chan error, 1)
	go func() { done <- startErr(s.runtime.Start(ctx, *in)) }()
	select {
	case <-created:
	case <-time.After(30 * time.Second):
		t.Fatal("the create was never issued")
	}
	if _, ok := s.jobOf(in); !ok {
		t.Fatal("the paused create should already be applied")
	}

	// The cancellation commits while the create's response is still outstanding.
	// Execution requests termination right after the commit; holding the delete
	// open lets the late Pod reach admission, which the committed state must deny.
	gate := make(chan struct{})
	s.cl.Block = func(op string, n int) {
		if op == "delete job" {
			<-gate
		}
	}
	cancelled := make(chan error, 1)
	go func() { _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); cancelled <- err }()
	eventually(t, 30*time.Second, "the cancellation to commit", func() (bool, string) {
		return s.attemptStatus(in) == execution.AttemptCancelled, string(s.attemptStatus(in))
	})

	// The late workload's actual entrypoint is refused admission by the runtime.
	eventually(t, 90*time.Second, "the late Pod's entrypoint to be refused admission", func() (bool, string) {
		for _, e := range s.srv.admitEvents(in.AttemptID) {
			if e.Status != 200 && e.Code == string(runtimev1.CodeAttemptRevoked) {
				return true, ""
			}
		}
		return false, "admits: " + func() string { return fmt.Sprint(s.srv.admitEvents(in.AttemptID)) }()
	})
	for _, e := range s.srv.admitEvents(in.AttemptID) {
		if e.Status == 200 {
			t.Fatal("the late workload was admitted")
		}
	}
	if s.srv.token(in.AttemptID) != "" {
		t.Fatal("a runtime token was issued to the late workload")
	}

	// Release the delete and the create: the dispatch sees the cancellation and
	// terminates the workload instead of confirming it.
	s.cl.reset()
	close(gate)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, kube.ErrNotDispatchable) {
		t.Fatalf("dispatch: %v", err)
	}

	// A late result is rejected: the attempt has no authority.
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptrTo(int64(0)), Outputs: map[string]runtimev1.Value{"v": runtimev1.StringValue("late")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	must(t, err)
	if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res}}); !errors.Is(err, execution.ErrAttemptRevoked) {
		t.Fatalf("late result: %v", err)
	}

	// Reconciliation deletes the Job (verified UID, foreground) and the attempt
	// settles with launch evidence: the primary container did start (its
	// admission was refused after launch), so the evidence is conservative.
	s.until(3*time.Minute, "the run to settle", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st.Terminal(), string(st)
	})
	if _, ok := s.jobOf(in); ok {
		t.Fatal("the late Job survives")
	}
	if got := s.e.Get(run).Run.Status; got != execution.RunCancelled {
		t.Fatalf("run %s", got)
	}
	at, ctl := s.attempt(in)
	if at.Status != execution.AttemptCancelled || ctl.Settled() == false {
		t.Fatalf("attempt %s settled=%v", at.Status, ctl.Settled())
	}
	if ctl.Launch != execution.LaunchStarted {
		t.Fatalf("launch evidence %s", ctl.Launch)
	}
	if got := s.held(); got != 0 {
		t.Fatalf("%d reservations still held", got)
	}
	if s.srv.token(in.AttemptID) != "" {
		t.Fatal("a token reached the late workload")
	}
}

func ptrTo[T any](v T) *T { return &v }

// cancelAtRead cancels the run through the real service just before the n-th
// attempt read of the wrapped executor: a cancellation that commits after the
// create intent is recorded and before the create is sent.
type cancelAtRead struct {
	kube.Attempts
	n      int
	reads  int
	cancel func()
}

func (c *cancelAtRead) Attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (kube.AttemptRecord, error) {
	c.reads++
	if c.reads == c.n {
		c.cancel()
	}
	return c.Attempts.Attempt(ctx, scope, id)
}

// Test_cancel_before_create_settles (PX-08, IT-08): a cancellation that commits
// before the create is sent leaves a registered record with no issued create.
// Termination reports affirmative not_started evidence, so execution settles the
// cancellation and releases the reservation without reconciliation.
func Test_cancel_before_create_settles(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in := s.claim(executiontest.ScopeA.TenantID)

	// Reads: 1 dispatch, 2 before the create is sent.
	hook := &cancelAtRead{Attempts: s.attempts, n: 2, cancel: func() {
		if _, err := s.svc.Cancel(ctx, s.caller(), run.Scope, run.ID); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}}
	x, err := kube.NewRuntime(kube.Dependencies{Policy: s.policy, Cluster: s.cl, Attempts: hook, Registrar: s.registry},
		kube.Options{Clock: s.e.Clock.Now, CreateQuiescence: 3 * time.Second, OrphanGrace: 2 * time.Second, PreflightTTL: time.Second})
	must(t, err)
	if _, err := x.Start(ctx, *in); !errors.Is(err, kube.ErrNotDispatchable) {
		t.Fatalf("dispatch: %v", err)
	}
	if s.jobsNamed(in) != 0 {
		t.Fatal("a Job was created for a cancelled attempt")
	}

	s.until(3*time.Minute, "the cancellation to settle", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st.Terminal(), string(st)
	})
	if got := s.e.Get(run).Run.Status; got != execution.RunCancelled {
		t.Fatalf("run %s", got)
	}
	at, ctl := s.attempt(in)
	if at.Status != execution.AttemptCancelled || !ctl.Settled() {
		t.Fatalf("attempt %s settled=%v", at.Status, ctl.Settled())
	}
	if ctl.Launch != execution.LaunchNotStarted || ctl.ReconciledAt != nil {
		t.Fatalf("launch evidence %s reconciled=%v", ctl.Launch, ctl.ReconciledAt)
	}
	if got := s.held(); got != 0 {
		t.Fatalf("%d reservations still held", got)
	}
}
