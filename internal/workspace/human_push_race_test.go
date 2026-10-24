package workspace_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func proposeAsync(e *wt.Env, in workspace.ProposeInput) <-chan proposeResult {
	ch := make(chan proposeResult, 1)
	go func() {
		res, err := e.Svc.ProposeSource(context.Background(), in)
		ch <- proposeResult{res, err}
	}()
	return ch
}

type proposeResult struct {
	res workspace.ProposeResult
	err error
}

func await(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Test_human_push_race: WS-04, WS-09, WS-10 and WS-A07. A human fast-forward
// wins the ref race against a reserved writer; the explicit expected-tip CAS
// returns a conflict that preserves both the human history and the proposal;
// authorized synchronization records the observed commit; cross-tenant and
// oversized access fail.
func Test_human_push_race(t *testing.T) {
	t.Run("a human push after the push began wins; the proposal and the observed tip are preserved", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "agent change", gitfx.File{Path: "agent.txt", Content: "agent"})

		hold := e.Git.HoldReceivePack()
		done := proposeAsync(e, in)
		await(t, "the push to reach the server", hold.Reached)
		human := e.Repo.HumanPush(t, w.WorkBranch, "human change", gitfx.File{Path: "human.txt", Content: "human"})
		hold.Release()

		got := <-done
		var conflict *workspace.ConflictError
		if !errors.As(got.err, &conflict) || !errors.Is(got.err, workspace.ErrPublicationConflict) {
			t.Fatalf("want a publication conflict, got %+v", got)
		}
		if conflict.ObservedTip != workspace.Commit(human) {
			t.Fatalf("observed tip %s, want the human commit %s", conflict.ObservedTip, human)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != human {
			t.Fatalf("the human push was overwritten: %s", tip)
		}
		if refs := e.Repo.Git(t, "for-each-ref", "--contains", in.Commit.String(), "--format=%(refname)"); refs != "" {
			t.Fatalf("the rejected proposal became reachable from %s", refs)
		}

		cur, err := e.Svc.Get(context.Background(), wt.Engineer, wt.ScopeA, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Pending != nil || cur.AcceptedCommit != workspace.Commit(e.Repo.Initial) || cur.Writer == nil {
			t.Fatalf("a conflict must settle the publication and change nothing else: %+v", cur)
		}
		agg, _ := e.Store.View(context.Background(), w.Scope, w.ID, workspace.Load{Receipt: "att_1"})
		r := agg.Receipt
		if r == nil || r.Status != workspace.PublicationConflict || r.Commit != in.Commit || r.ObservedTip != workspace.Commit(human) || r.ArtifactID != in.BundleArtifactID {
			t.Fatalf("the proposal was not preserved: %+v", r)
		}

		// Repeating the proposal observes the same conflict; it never retries the push.
		again, err := e.Svc.ProposeSource(context.Background(), in)
		var c2 *workspace.ConflictError
		if !errors.As(err, &c2) || c2.ObservedTip != workspace.Commit(human) || again.Status != "" {
			t.Fatalf("repeat: %+v %v", again, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != human {
			t.Fatal("a repeated proposal changed the branch")
		}

		// Authorized synchronization records the observed human commit.
		synced, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil || synced.AcceptedCommit != workspace.Commit(human) {
			t.Fatalf("sync: %v %+v", err, synced)
		}
		if n := e.CountTenant(wt.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'workspace.checkpoint_created' AND convert_from(payload, 'UTF8') LIKE '%"humanSynchronization":true%'`); n != 1 {
			t.Fatalf("%d human synchronization events", n)
		}
		if n := e.CountSQL(`SELECT count(*) FROM audit WHERE action = 'workspace.synchronized' AND resource_id = $1`, string(w.ID)); n != 1 {
			t.Fatalf("%d synchronization audit rows", n)
		}
	})

	t.Run("a human push before the proposal is detected without pushing", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "agent change", gitfx.File{Path: "agent.txt", Content: "agent"})
		human := e.Repo.HumanPush(t, w.WorkBranch, "human", gitfx.File{Path: "human.txt", Content: "human"})
		isPush := func(r gitfx.Request) bool { return r.Service == "git-receive-pack" && r.Method == "POST" }
		before := e.Git.CountRequests(isPush)
		_, err := e.Svc.ProposeSource(context.Background(), in)
		var conflict *workspace.ConflictError
		if !errors.As(err, &conflict) || conflict.ObservedTip != workspace.Commit(human) {
			t.Fatalf("got %v", err)
		}
		if n := e.Git.CountRequests(isPush) - before; n != 0 {
			t.Fatalf("%d receive-pack posts for a proposal that was already known to conflict", n)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != human {
			t.Fatal("branch changed")
		}
	})

	t.Run("a proposal built on an older pin than the tip is not a fast-forward", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		// The writer is pinned to an input commit older than the tip it expects.
		human := e.Repo.HumanPush(t, w.WorkBranch, "human", gitfx.File{Path: "human.txt", Content: "human"})
		if _, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0); err != nil {
			t.Fatal(err)
		}
		e.Attempts.Register(w.Scope, "att_1", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(context.Background(), wt.Engineer, workspace.ReserveWriterInput{
			Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", InputCommit: workspace.Commit(e.Repo.Initial)}); err != nil {
			t.Fatal(err)
		}
		commit, bundle, _ := e.Repo.Bundle(t, e.Repo.Initial, "stale base", gitfx.File{Path: "agent.txt", Content: "a"})
		e.Bundles.Add(w.Scope, "art_stale", "att_1", bundle)
		_, err := e.Svc.ProposeSource(context.Background(), workspace.ProposeInput{
			Scope: w.Scope, AttemptID: "att_1", Commit: workspace.Commit(commit), ExpectedTip: workspace.Commit(human), BundleArtifactID: "art_stale"})
		var conflict *workspace.ConflictError
		if !errors.As(err, &conflict) || conflict.Reason != "not_fast_forward" {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("synchronization enforces tenant authorization, If-Match and fast-forward only", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		human := e.Repo.HumanPush(t, w.WorkBranch, "human", gitfx.File{Path: "human.txt", Content: "human"})

		if _, err := e.Svc.Sync(context.Background(), workspace.Actor{Type: workspace.ActorUser, ID: "usr_nobody"}, wt.ScopeA, w.ID, 0); !errors.Is(err, workspace.ErrDenied) {
			t.Fatalf("ungranted: %v", err)
		}
		if _, err := e.Svc.Sync(context.Background(), workspace.Actor{Type: workspace.ActorUser, ID: "usr_other_tenant"}, wt.ScopeA, w.ID, 0); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("other tenant: %v", err)
		}
		// The workspace id is scoped: the same id under another tenant does not exist.
		if _, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeB, w.ID, 0); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("foreign scope: %v", err)
		}
		if _, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA2, w.ID, 0); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("another project: %v", err)
		}
		if _, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, w.Version+7); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatalf("stale If-Match: %v", err)
		}
		synced, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, w.Version)
		if err != nil || synced.AcceptedCommit != workspace.Commit(human) || synced.Version <= w.Version {
			t.Fatalf("sync: %v %+v", err, synced)
		}
		// Repeating with the old version is stale; with the new one is a no-op.
		if _, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, w.Version); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatalf("replayed If-Match: %v", err)
		}
		same, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, synced.Version)
		if err != nil || same.Version != synced.Version {
			t.Fatalf("no-op sync changed the resource: %v %+v", err, same)
		}

		// A rewritten (non fast-forward) branch is never adopted.
		rogue := e.Repo.OrphanCommit(t, "rewritten history")
		e.Repo.SetRef(t, w.WorkBranch, rogue)
		div, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0)
		if !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("diverged: %v", err)
		}
		if div.AcceptedCommit != workspace.Commit(human) || div.LastError != workspace.ReasonBranchDiverged {
			t.Fatalf("a diverged branch must be recorded, not adopted: %+v", div)
		}
		// And admission is refused while it stays diverged.
		e.Attempts.Register(w.Scope, "att_9", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(context.Background(), wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_9"}); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("admission on a diverged branch: %v", err)
		}
	})

	t.Run("a deleted work branch is recorded, never recreated, and blocks admission", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Repo.DeleteRef(t, w.WorkBranch)
		cur, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0)
		if !errors.Is(err, workspace.ErrDiverged) || cur.LastError != workspace.ReasonBranchMissing || cur.AcceptedCommit != w.AcceptedCommit {
			t.Fatalf("%v %+v", err, cur)
		}
		if e.Repo.Tip(t, w.WorkBranch) != "" {
			t.Fatal("the branch was recreated by synchronization")
		}
		e.Attempts.Register(w.Scope, "att_1", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(context.Background(), wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1"}); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("admission without a branch: %v", err)
		}
		// Restoring the branch clears the reason.
		e.Repo.SetRef(t, w.WorkBranch, string(w.AcceptedCommit))
		ok, err := e.Svc.Sync(context.Background(), wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil || ok.LastError != "" {
			t.Fatalf("%v %+v", err, ok)
		}
	})

	t.Run("an oversized bundle is refused before it is expanded", func(t *testing.T) {
		e := wt.New(t, wt.Config{Git: func(o *gitad.Options) { o.MaxBundleBytes = 4096; o.MaxScratchBytes = 1 << 20 }})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "huge", gitfx.File{Path: "big.txt", Content: strings.Repeat("0123456789abcdef", 8192)})
		art, _ := e.Bundles.Open(context.Background(), w.Scope, in.BundleArtifactID)
		if art.Size <= 4096 {
			t.Skipf("bundle of %d bytes is unexpectedly small", art.Size)
		}
		_, err := e.Svc.ProposeSource(context.Background(), in)
		if !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("got %v", err)
		}
		cur, _ := e.Svc.Get(context.Background(), wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil {
			t.Fatal("an oversized proposal left a pending publication")
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_publications`); n != 0 {
			t.Fatalf("%d receipts for a refused bundle", n)
		}
	})
}
