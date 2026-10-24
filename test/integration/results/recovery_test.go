package results_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/workspace"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// Test_published_git_db_failure: PX-12, WS-06, WS-11, WS-A05 and PX-A07
// (acceptance `published_git_db_failure`). Git CAS succeeds while the receipt
// cannot be persisted: the publication stays pending and the writer slot is
// retained, result success stays blocked, a restarted reconciler records the
// remote commit from the remote, and the retried proposal and result record one
// checkpoint and one result without repeating execution or the push. Real
// PostgreSQL, S3 and Git through the real adapters; no NATS.
func Test_published_git_db_failure(t *testing.T) {
	var faultStore *wst.FaultStore
	e := New(t, Options{Artifacts: true, Git: true, WrapWorkspaceStore: func(s workspace.Store) workspace.Store {
		faultStore = &wst.FaultStore{Store: s}
		return faultStore
	}})
	ctx := context.Background()
	p := e.NewProject(ScopeA)
	a := e.Writer(p, labelJob)
	commit, art := e.Bundle(a, "change", gitfx.File{Path: "a.txt", Content: "a"})
	proposal := e.Proposal(a, commit, art)
	dispatched := len(e.X.Workloads.Starts())

	// The database is lost right after the remote update.
	e.Publisher.AfterPublish = func(err error) {
		if err == nil {
			faultStore.FailNextUpdates(1)
		}
	}
	pushes := e.ReceivePacks()
	code, body := e.ProposeHTTP(a, proposal)
	e.Publisher.AfterPublish = nil
	if code != 202 {
		t.Fatalf("a result that could not be recorded must not be acknowledged: %d %s", code, body)
	}
	var pending runtimev1.SourceProposalResponse
	if err := jsonUnmarshal(body, &pending); err != nil || pending.Status != runtimev1.PublishPending || pending.AcceptedCommit != nil || string(pending.Commit) != commit {
		t.Fatalf("%+v %v", pending, err)
	}
	if tip := e.Repo.Tip(t, e.Branch(a)); tip != commit {
		t.Fatalf("the remote must already hold the commit: %s", tip)
	}
	if e.ReceivePacks()-pushes != 1 {
		t.Fatalf("%d pushes", e.ReceivePacks()-pushes)
	}
	w := e.Workspace(p)
	if w.Pending == nil || string(w.AcceptedCommit) != e.Repo.Initial || w.Writer == nil || w.Writer.AttemptID != a.ID() {
		t.Fatalf("nothing is accepted until it is durably recorded, and ownership is retained: %+v", w)
	}

	// Result success is blocked: the writer's publication is unsettled.
	req := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(commit)), Outputs: outputsOf("v1"), DiagnosticArtifactIDs: []domain.ArtifactID{}})
	code, body = e.SubmitHTTP(a, req)
	if code != 503 || errCode(t, body) != domain.CodeUnavailable {
		t.Fatalf("an unsettled publication must block success: %d %s", code, body)
	}
	if at := e.Persisted(a); at.Status != execution.AttemptRunning || at.ResultHash != "" {
		t.Fatalf("the attempt was settled with an unknown publication: %s %q", at.Status, at.ResultHash)
	}
	if e.CheckpointEvents(ScopeA.TenantID) != 0 {
		t.Fatal("a checkpoint was announced for an unrecorded publication")
	}
	// The slot cannot be reassigned while the publication is unknown.
	if _, err := e.WSPlain.ReserveWriter(ctx, wst.Engineer, workspace.ReserveWriterInput{Scope: p.Scope, WorkspaceID: p.Workspace, AttemptID: "att_other_writer"}); err == nil {
		t.Fatal("the writer slot was reassigned during an unknown publication")
	}

	// A restart: another control-plane process finds the stale claim and resumes
	// the same intent from the remote evidence, without pushing again.
	restarted := e.WorkspaceRestart()
	e.X.Clock.Advance(2 * time.Minute)
	if err := restarted.ReconcileWorkspace(ctx, p.Scope, p.Workspace); err != nil {
		t.Fatal(err)
	}
	w = e.Workspace(p)
	if w.Pending != nil || string(w.AcceptedCommit) != commit {
		t.Fatalf("not reconciled: pending=%v accepted=%s", w.Pending, w.AcceptedCommit)
	}
	if e.ReceivePacks()-pushes != 1 {
		t.Fatalf("reconciliation pushed again (%d)", e.ReceivePacks()-pushes)
	}
	if e.CheckpointEvents(ScopeA.TenantID) != 1 || e.Publications(p.Workspace) != 1 {
		t.Fatalf("%d checkpoint events, %d receipts", e.CheckpointEvents(ScopeA.TenantID), e.Publications(p.Workspace))
	}

	// The retried proposal observes the recorded outcome; the retried result is
	// accepted exactly once.
	code, body = e.ProposeHTTP(a, proposal)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	ack, err := e.Submit(a, req)
	if err != nil || ack.AttemptStatus != runtimev1.AttemptSucceeded {
		t.Fatalf("%+v %v", ack, err)
	}
	if again, err := e.Submit(a, req); err != nil || again != ack {
		t.Fatalf("replay %+v %v", again, err)
	}
	if e.CheckpointEvents(ScopeA.TenantID) != 1 || e.Publications(p.Workspace) != 1 || e.ReceivePacks()-pushes != 1 {
		t.Fatal("the retries duplicated the checkpoint, the receipt or the push")
	}
	if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status = 'succeeded'`, string(a.Run.ID)); n != 1 {
		t.Fatalf("%d success transitions", n)
	}
	if n := e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(a.Run.ID)); n != 1 {
		t.Fatalf("%d attempts: execution was repeated", n)
	}
	if d := len(e.X.Workloads.Starts()); d != dispatched {
		t.Fatalf("execution was dispatched again (%d -> %d)", dispatched, d)
	}
	// The slot is released only by the successful settlement, and the commit is
	// now a selectable successful output.
	if w := e.Workspace(p); w.Writer != nil {
		t.Fatalf("the writer slot is still held: %+v", w.Writer)
	}
	if c, err := e.WSPlain.SelectWriterOutput(ctx, wst.Engineer, p.Scope, p.Workspace, a.ID()); err != nil || string(c) != commit {
		t.Fatalf("selected %q %v", c, err)
	}
	// A result from before the failure with a different source cannot rewrite it.
	other := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(e.Repo.Initial)), Outputs: outputsOf("v1"), DiagnosticArtifactIDs: []domain.ArtifactID{}})
	if _, err := e.Submit(a, other); !errors.Is(err, results.ErrResultConflict) {
		t.Fatalf("%v", err)
	}
}
