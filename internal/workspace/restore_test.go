package workspace_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

var operator = workspace.Actor{Type: workspace.ActorOperator, ID: "ops_recovery"}

// Test_publisher_restore_fence: after installation recovery a stale publisher
// can neither finalize its in-flight publication nor start new writes, and
// publication resumes only with credential rotation evidence; the in-flight
// publication is then settled from remote evidence, not abandoned.
func Test_publisher_restore_fence(t *testing.T) {
	ctx := context.Background()

	t.Run("a fenced publisher cannot finalize or admit; resumption needs rotation evidence and settles from the remote", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Authz.Grant(operator.ID, wt.ScopeA)
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})

		// The old control plane's push is in flight when recovery fences it.
		hold := e.Git.HoldReceivePack()
		stale := proposeAsync(e, in)
		await(t, "the stale publisher's push", hold.Reached)
		n, err := e.Svc.FencePublishers(ctx, operator, wt.ScopeA)
		if err != nil || n != 1 {
			t.Fatalf("fence: %d %v", n, err)
		}
		hold.Release()

		got := <-stale
		if !errors.Is(got.err, workspace.ErrFenced) {
			t.Fatalf("the stale publisher finalized after fencing: %+v %v", got.res, got.err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending == nil || cur.AcceptedCommit != workspace.Commit(e.Repo.Initial) || !cur.Fence.Fenced {
			t.Fatalf("a fenced publisher must not change accepted state: %+v", cur)
		}
		// The remote did take the update; only the recording is withheld.
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("tip %s", tip)
		}

		// No new writes: no admission, no proposals, no reconciliation by the stale process.
		e.Attempts.Register(w.Scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err == nil {
			t.Fatal("a fenced workspace admitted a writer")
		}
		if _, err := e.Svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrFenced) && err == nil {
			t.Fatalf("a repeated proposal drove a fenced publication: %v", err)
		}
		if err := e.Svc.ReconcileWorkspace(ctx, w.Scope, w.ID); err == nil {
			t.Fatal("reconciliation finalized under a fence")
		}
		if cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID); cur.Pending == nil {
			t.Fatal("the publication was settled while fenced")
		}

		// Resumption requires authority and verifiable rotation of the publisher credentials.
		if _, err := e.Svc.ResumePublishing(ctx, workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}, wt.ScopeA, workspace.RotationEvidence{RotationID: "rot_1"}); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("an ungranted user lifted the fence: %v", err)
		}
		if _, err := e.Svc.ResumePublishing(ctx, operator, wt.ScopeA, workspace.RotationEvidence{}); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("no evidence: %v", err)
		}
		e.Rotation.Err = errors.New("credentials not rotated")
		if _, err := e.Svc.ResumePublishing(ctx, operator, wt.ScopeA, workspace.RotationEvidence{RotationID: "rot_1"}); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("unrotated credentials: %v", err)
		}
		if cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID); !cur.Fence.Fenced {
			t.Fatal("the fence was lifted without rotation evidence")
		}
		e.Rotation.Err = nil
		if n, err := e.Svc.ResumePublishing(ctx, operator, wt.ScopeA, workspace.RotationEvidence{RotationID: "rot_1"}); err != nil || n != 1 {
			t.Fatalf("resume: %d %v", n, err)
		}

		// The new publisher resolves the in-flight publication from remote evidence.
		fresh := e.NewService(nil)
		pushes := e.Git.CountRequests(isReceivePack)
		if err := fresh.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ = fresh.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil || cur.AcceptedCommit != in.Commit || cur.Fence.Fenced {
			t.Fatalf("not settled after resumption: %+v", cur)
		}
		if e.Git.CountRequests(isReceivePack) != pushes {
			t.Fatal("recovery pushed again instead of reading the remote")
		}
		if n := checkpointEvents(e); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
		if n := auditCount(e, "workspace.publisher_fenced", w.ID); n != 1 {
			t.Fatalf("%d fence audit rows", n)
		}
		if n := auditCount(e, "workspace.publishing_resumed", w.ID); n != 1 {
			t.Fatalf("%d resume audit rows", n)
		}
		// The slot is released only through settlement, then writers are admitted again.
		if _, err := fresh.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: in.Commit}); err != nil {
			t.Fatal(err)
		}
		if _, err := fresh.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err != nil {
			t.Fatalf("admission after resumption: %v", err)
		}
	})

	t.Run("fencing is scoped to the project and authorized", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Authz.Grant(operator.ID, wt.ScopeA)
		wa := e.Ready(wt.ScopeA, "wi_1")
		wa2 := e.Ready(wt.ScopeA2, "wi_2")
		wb := e.Ready(wt.ScopeB, "wi_3")
		if _, err := e.Svc.FencePublishers(ctx, wt.Engineer, wt.ScopeB); err != nil {
			t.Fatalf("the fixture engineer has grants in B: %v", err)
		}
		if _, err := e.Svc.FencePublishers(ctx, workspace.Actor{Type: workspace.ActorOperator, ID: "ops_nobody"}, wt.ScopeA); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("ungranted operator: %v", err)
		}
		if _, err := e.Svc.FencePublishers(ctx, operator, wt.ScopeA); err != nil {
			t.Fatal(err)
		}
		fenced := func(w *workspace.Workspace) bool {
			cur, _ := e.Store.Get(ctx, w.Scope, w.ID)
			return cur.Fence.Fenced
		}
		if !fenced(wa) || fenced(wa2) {
			t.Fatalf("project A fenced=%v, project A2 fenced=%v", fenced(wa), fenced(wa2))
		}
		if !fenced(wb) {
			t.Fatal("project B was fenced by its own request")
		}
	})
}
