package composition_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/workspace"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// TestGuardedSourceRecovery: PX-12, WS-06, WS-11, IT-06 (supports the
// `cancel_grant_atomicity` and `guarded_result_replay` source interleavings). The
// guarded workspace store makes a cancellation that commits before the
// publication intent prevent Git publication; a publication authorized earlier
// stays pending with its writer barrier when the database fails after the remote
// update, is settled by explicit trusted reconciliation and never turns the
// cancelled attempt into a success. Git and executor calls run outside SQL
// transactions. Real PostgreSQL, HTTPS Git and S3 through the real adapters; no
// NATS.
func TestGuardedSourceRecovery(t *testing.T) {
	ctx := context.Background()
	var faultStore *wst.FaultStore
	e := New(t, Options{Artifacts: true, Git: true, WrapWorkspaceStore: func(s workspace.Store) workspace.Store {
		faultStore = &wst.FaultStore{Store: s}
		return faultStore
	}})

	t.Run("a cancellation that commits before the source intent prevents the publication", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
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
		if !e.GrantRevoked(a) {
			t.Fatal("the cancellation left the grant usable")
		}
		if w := e.Workspace(p); w.Writer != nil {
			t.Fatalf("the cancelled writer still holds the slot: %+v", w.Writer)
		}
	})

	t.Run("an authorized publication followed by SQL failure stays pending and is settled by trusted reconciliation", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, art := e.Bundle(a, "change", gitfx.File{Path: "b.txt", Content: "b"})
		proposal := e.Proposal(a, commit, art)
		pushes := e.ReceivePacks()
		e.Publisher.AfterPublish = func(err error) {
			if err == nil {
				faultStore.FailNextUpdates(1) // the database is lost right after the remote update
			}
		}
		code, body := e.ProposeHTTP(a, proposal)
		e.Publisher.AfterPublish = nil
		if code != 202 {
			t.Fatalf("a publication that could not be recorded must not be acknowledged as accepted: %d %s", code, body)
		}
		if e.Repo.Tip(t, e.Branch(a)) != commit {
			t.Fatal("the remote must already hold the commit")
		}
		if w := e.Workspace(p); w.Pending == nil || w.Writer == nil || w.Writer.AttemptID != a.ID() {
			t.Fatalf("an unsettled publication must retain the writer barrier: %+v", w)
		}
		// Cancellation arrives while the publication is unsettled: the grant is
		// revoked at once; the writer barrier stays until the publication settles.
		if err := e.CancelErr(a); err != nil {
			t.Fatal(err)
		}
		if !e.GrantRevoked(a) {
			t.Fatal("cancellation did not revoke the grant")
		}
		if w := e.Workspace(p); w.Writer == nil || w.Pending == nil {
			t.Fatalf("cancellation released a writer barrier that guards an unknown publication: %+v", w)
		}
		res := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(commit)), Outputs: outputsOf("v1"), DiagnosticArtifactIDs: []domain.ArtifactID{}})
		if _, err := e.Submit(a, res); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a late result: %v", err)
		}
		if e.CheckpointEvents(ScopeA.TenantID) != 0 {
			t.Fatal("a checkpoint was announced for an unrecorded publication")
		}

		// Explicit trusted reconciliation (a restarted process, the undecorated path).
		e.X.Clock.Advance(2 * time.Minute)
		if err := e.WorkspaceRestart().ReconcileWorkspace(ctx, p.Scope, p.Workspace); err != nil {
			t.Fatal(err)
		}
		w := e.Workspace(p)
		if w.Pending != nil || string(w.AcceptedCommit) != commit {
			t.Fatalf("not reconciled: pending=%v accepted=%s", w.Pending, w.AcceptedCommit)
		}
		if e.ReceivePacks()-pushes != 1 || e.CheckpointEvents(ScopeA.TenantID) != 1 || e.Publications(p.Workspace) != 1 {
			t.Fatalf("pushes %d checkpoints %d receipts %d", e.ReceivePacks()-pushes, e.CheckpointEvents(ScopeA.TenantID), e.Publications(p.Workspace))
		}
		// Execution's reconciler then settles the cancelled attempt and releases
		// the barrier; the attempt never becomes a success.
		if _, err := e.ExecPlain.ReconcileTenant(ctx, ScopeA.TenantID); err != nil {
			t.Fatal(err)
		}
		if at := e.Persisted(a); at.Status != execution.AttemptCancelled {
			t.Fatalf("reconciliation changed the attempt to %s", at.Status)
		}
		if w := e.Workspace(p); w.Writer != nil {
			t.Fatalf("the barrier outlived the settlement: %+v", w.Writer)
		}
		if _, err := e.WSPlain.SelectWriterOutput(ctx, wsEngineer, p.Scope, p.Workspace, a.ID()); err == nil {
			t.Fatal("a cancelled attempt's commit was selected as a successful output")
		}
		if _, err := e.Submit(a, res); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("the result after reconciliation: %v", err)
		}
	})

	t.Run("the open-transaction probe detects a transaction held across a call (control)", func(t *testing.T) {
		probe := &External{pool: e.X.Runtime}
		err := e.inTenant(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&n); err != nil {
				return err
			}
			probe.Observe("held") // the transaction is open and idle while the call runs
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if probe.Inside("held") != 1 {
			t.Fatal("the probe cannot see an idle open transaction, so its absence proves nothing")
		}
	})

	t.Run("Git and executor calls never run inside a SQL transaction", func(t *testing.T) {
		if e.External.Calls("git.publish") == 0 || e.External.Calls("runtime.stop") == 0 {
			t.Fatalf("the scenarios made no external calls: %d publishes, %d terminations", e.External.Calls("git.publish"), e.External.Calls("runtime.stop"))
		}
		for _, call := range []string{"git.publish", "runtime.stop"} {
			if n := e.External.Inside(call); n != 0 {
				t.Fatalf("%d %s calls ran while a SQL transaction was open", n, call)
			}
		}
	})
}
