package results_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/workspace"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

const labelJob = "outputs: {label: {type: string}}"

func lateEvidence(e *Env, a *Attempt) int {
	return e.Count(`SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = 'late_result'`, string(a.ID()))
}

// Test_cancel_finalization_race: PX-08, PX-12, WS-04, WS-11, PX-A06, WS-A03
// (acceptance `cancel_finalization_race`). Controlled interleavings serialize
// cancellation and revocation against result acceptance and source intent: the
// loser sees the winner's committed state, a losing callback can neither succeed
// nor initiate publication, and a remote effect authorized before the
// cancellation stays reconcilable without turning the cancelled attempt into a
// success. Real PostgreSQL, S3 and Git through the real adapters; no NATS.
func Test_cancel_finalization_race(t *testing.T) {
	e := New(t, Options{Artifacts: true, Git: true})
	ctx := context.Background()

	t.Run("a cancellation that commits between authentication and acceptance wins", func(t *testing.T) {
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		e.Ctx.SetAfter(func() { e.Cancel(a) })
		defer e.Ctx.SetAfter(nil)
		req := Request(t, Completed(outputsOf("v1")))
		_, err := e.Submit(a, req)
		if !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("the losing callback: %v", err)
		}
		at := e.Persisted(a)
		if at.Status == execution.AttemptSucceeded || at.ResultHash != "" {
			t.Fatalf("a result was accepted after cancellation: %s %q", at.Status, at.ResultHash)
		}
		if lateEvidence(e, a) != 1 {
			t.Fatalf("the late result was not kept as evidence: %d", lateEvidence(e, a))
		}
		if got := e.Aggregate(a).Step("build").Outputs; len(got) != 0 {
			t.Fatalf("outputs of a cancelled attempt were exposed: %v", got)
		}
	})

	t.Run("a revocation that commits between authentication and acceptance wins", func(t *testing.T) {
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		e.Ctx.SetAfter(func() {
			if err := e.Auth.Revoke(ctx, a.Scope, a.ID()); err != nil {
				t.Error(err)
			}
		})
		defer e.Ctx.SetAfter(nil)
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		if e.Persisted(a).ResultHash != "" || e.Persisted(a).Status != execution.AttemptRunning {
			t.Fatal("a revoked callback changed the attempt")
		}
	})

	t.Run("an acceptance holding the lock makes the cancellation wait and not rewrite it", func(t *testing.T) {
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		waitPaused, release := e.Gate.Arm()
		req := Request(t, Completed(outputsOf("v1")))
		var ack runtimev1.ResultResponse
		var submitErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); ack, submitErr = e.Submit(a, req) }()
		waitPaused() // the mutation ran; the transaction holds the run lock and the grant share lock

		cancelled := make(chan struct{})
		go func() {
			e.Cancel(a)
			close(cancelled)
		}()
		select {
		case <-cancelled:
			t.Fatal("the cancellation did not wait for the acceptance's locks")
		case <-time.After(500 * time.Millisecond):
		}
		revoked := make(chan error, 1)
		go func() { revoked <- e.Auth.Revoke(ctx, a.Scope, a.ID()) }()
		select {
		case <-revoked:
			t.Fatal("the revocation did not wait for the acceptance's grant lock")
		case <-time.After(300 * time.Millisecond):
		}
		release()
		wg.Wait()
		if submitErr != nil || ack.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, submitErr)
		}
		<-cancelled
		if err := <-revoked; err != nil {
			t.Fatal(err)
		}
		at := e.Persisted(a)
		if at.Status != execution.AttemptSucceeded || at.ResultHash != string(req.ResultHash) {
			t.Fatalf("the cancellation rewrote the accepted result: %s", at.Status)
		}
		if got := e.Aggregate(a).Step("build").Outputs["label"].String(); got != "v1" {
			t.Fatalf("output %q", got)
		}
	})

	t.Run("a cancellation that commits before source intent prevents the publication", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p)
		commit, art := e.Bundle(a, "change", gitfx.File{Path: "a.txt", Content: "a"})
		tipBefore := e.Repo.Tip(t, e.Branch(a))
		pushes := e.ReceivePacks()
		var once sync.Once
		e.WSHooks.SetBefore(func(load workspace.Load) {
			if load.Receipt == a.ID() {
				once.Do(func() { e.Cancel(a) }) // after the workspace's own authority read, before the intent is recorded
			}
		})
		defer e.WSHooks.SetBefore(nil)
		code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art))
		if code != 403 || errCode(t, body) != runtimev1.CodeAttemptRevoked {
			t.Fatalf("%d %s", code, body)
		}
		if e.Publications(p.Workspace) != 0 || e.Workspace(p).Pending != nil {
			t.Fatal("a publication intent was recorded by a losing callback")
		}
		if e.Repo.Tip(t, e.Branch(a)) != tipBefore || e.ReceivePacks() != pushes {
			t.Fatal("Git was invoked by a losing callback")
		}
	})

	t.Run("a publication authorized before cancellation stays reconcilable and never becomes success", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, art := e.Bundle(a, "change", gitfx.File{Path: "a.txt", Content: "a"})
		var once sync.Once
		e.Publisher.AfterPublish = func(err error) {
			if err == nil {
				once.Do(func() { e.Cancel(a) }) // the push is on the remote; the control plane then loses the race
			}
		}
		defer func() { e.Publisher.AfterPublish = nil }()
		code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art))
		if code != 202 {
			t.Fatalf("the callback cannot record the publication it started: %d %s", code, body)
		}
		if tip := e.Repo.Tip(t, e.Branch(a)); tip != commit {
			t.Fatalf("remote tip %s", tip)
		}
		if w := e.Workspace(p); w.Pending == nil || w.Writer == nil {
			t.Fatalf("an unsettled publication must retain ownership: %+v", w)
		}
		if e.CheckpointEvents(ScopeA.TenantID) != 0 {
			t.Fatal("a checkpoint was announced before the publication was recorded")
		}
		// The workload's late result is denied and cannot succeed.
		res := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(commit)), Outputs: outputsOf("v1"), DiagnosticArtifactIDs: []domain.ArtifactID{}})
		if _, err := e.Submit(a, res); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("late result: %v", err)
		}

		// The owner's reconciliation (an undecorated path) records the remote commit.
		e.X.Clock.Advance(2 * time.Minute)
		if err := e.WSPlain.ReconcileWorkspace(ctx, p.Scope, p.Workspace); err != nil {
			t.Fatal(err)
		}
		w := e.Workspace(p)
		if w.Pending != nil || string(w.AcceptedCommit) != commit {
			t.Fatalf("not reconciled: pending=%v accepted=%s", w.Pending, w.AcceptedCommit)
		}
		if e.CheckpointEvents(ScopeA.TenantID) != 1 {
			t.Fatalf("%d checkpoint events", e.CheckpointEvents(ScopeA.TenantID))
		}
		// Settled as history only: the cancelled attempt is not a successful writer.
		if at := e.Persisted(a); at.Status == execution.AttemptSucceeded {
			t.Fatal("reconciliation turned a cancelled attempt into success")
		}
		if _, err := e.WSPlain.SelectWriterOutput(ctx, wsEngineer, p.Scope, p.Workspace, a.ID()); err == nil {
			t.Fatal("a cancelled attempt's commit was selected as a successful output")
		}
		if _, err := e.Submit(a, res); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("the result after reconciliation: %v", err)
		}
	})
}
