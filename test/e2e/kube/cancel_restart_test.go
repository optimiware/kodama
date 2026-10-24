package kube_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
)

// Test_kube_component_cancel_restart repeats the cancel/restart acceptance of T-026
// (execution_assembly_cancel_restart; PX-A02, PX-A06, PX-08, IT-A05) on the
// Kubernetes runtime, and adds the two Kubernetes-only recovery forms T-026 no
// longer carries: adoption of a Job after a lost create acknowledgment and
// cancellation during create. Each subtest owns a fresh stack (database, runtime
// server, namespaces) over the shared cluster.
func Test_kube_component_cancel_restart(t *testing.T) {
	t.Run("adoption after a lost create acknowledgment", createAckCrash)
	t.Run("cancellation during create", cancelCreateRace)
	t.Run("cancellation before create", cancelBeforeCreate)
	t.Run("cancellation committed before the process stops", cancelThenRestart)
	t.Run("a runtime restart adopts the running Job", restartAdoptsRunningJob)
	t.Run("a foreign Job at the workload name", foreignJobAtName)
}

// createAckCrash (PX-A02, PX-03, PX-04): the Job create's acknowledgment is
// lost and the controller loses its memory; a restarted controller adopts the
// same matching Job and UID. One attempt, one credential, one reservation, one
// Job and one Pod, including under concurrent redrive.
func createAckCrash(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in := s.claim(executiontest.ScopeA.TenantID)

	// The create is applied by the API server, its response is lost, and the
	// read-back right after it also fails: the dispatch ends unconfirmed.
	gets := 0
	s.cl.Lose = func(op string, n int) bool { return op == "create job" && n == 1 }
	s.cl.Fail = func(op string, n int) error {
		if op == "get job" {
			gets++
			if gets == 2 { // the first read precedes the create; the second is the read-back
				return &kubeUnavailable
			}
		}
		return nil
	}
	_, err := s.runtime.Start(ctx, *in)
	if !errors.Is(err, kube.ErrUnavailable) {
		t.Fatalf("a lost acknowledgment with no read-back is unconfirmed: %v", err)
	}
	s.cl.reset()
	job, ok := s.jobOf(in)
	if !ok {
		t.Fatal("the create was applied: the Job must exist")
	}
	uid := job.Metadata.UID

	// Controller crash: a fresh executor, no memory, redriven concurrently by
	// several replicas with the same intent.
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = startErr(s.newRuntime(s.c.client).Start(ctx, *in))
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil && !errors.Is(e, kube.ErrUnavailable) {
			t.Fatalf("redrive %d: %v", i, e)
		}
	}
	if _, err := s.newRuntime(s.c.client).Start(ctx, *in); err != nil {
		t.Fatalf("redrive: %v", err)
	}
	s.observeDispatched(in)

	if got, _ := s.jobOf(in); got.Metadata.UID != uid {
		t.Fatalf("the Job was replaced: %s != %s", got.Metadata.UID, uid)
	}
	if n := s.jobsNamed(in); n != 1 {
		t.Fatalf("%d Jobs", n)
	}
	if s.attemptsOf(run) != 1 || s.grants(in) != 1 || s.held() != 1 {
		t.Fatalf("attempts=%d grants=%d reservations=%d", s.attemptsOf(run), s.grants(in), s.held())
	}
	if !s.secretExists(in) {
		t.Fatal("the admission Secret is missing")
	}

	// The adopted Job runs to a committed result through the real entrypoint.
	s.until(3*time.Minute, "the run to succeed", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st == execution.RunSucceeded, string(st)
	})
	if got := strOutput(t, s.e.Get(run), "v"); got != "ok" {
		t.Fatalf("output %q", got)
	}
	if n := len(s.podsOf(in)); n != 1 {
		t.Fatalf("%d Pods", n)
	}
	if s.count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)) != 1 {
		t.Fatal("a redrive created another attempt")
	}
}

// cancelCreateRace (PX-A06, PX-08, IT-08): a cancellation commits while the
// Job create is in flight. The late workload's actual entrypoint admission is
// denied, the workload is deleted, late results are rejected, and the launch
// evidence the executor collected keeps settlement honest.
func cancelCreateRace(t *testing.T) {
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

// cancelBeforeCreate (PX-08, IT-08): a cancellation that commits
// before the create is sent leaves a registered record with no issued create.
// Termination reports affirmative not_started evidence, so execution settles the
// cancellation and releases the reservation without reconciliation.
func cancelBeforeCreate(t *testing.T) {
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
	x, err := kube.NewRuntime(kube.Dependencies{Policy: s.policy, Cluster: s.cl, Attempts: hook, Registrar: s.registry, Logs: s.c.client},
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
