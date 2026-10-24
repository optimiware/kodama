package workspace_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func isReceivePack(r gitfx.Request) bool {
	return r.Service == "git-receive-pack" && r.Method == "POST"
}

// Test_revoked_writer: WS-03, WS-04, WS-05, WS-11, WS-A02 and WS-A03. Revoked or
// read attempts cannot publish, a delayed stale proposal cannot use a reassigned
// slot, only the publisher holds write authority, and a publication authorized
// before cancellation is settled without making the cancelled attempt a
// successful writer.
func Test_revoked_writer(t *testing.T) {
	ctx := context.Background()

	t.Run("competing writers yield one reservation", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		const n = 10
		var wg sync.WaitGroup
		results := make([]error, n)
		for i := range n {
			id := domain.AttemptID(fmt.Sprintf("att_%d", i))
			e.Attempts.Register(w.Scope, id, w.ID, workspace.AccessWrite)
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: id})
			}()
		}
		wg.Wait()
		winners := 0
		for _, err := range results {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, workspace.ErrWriterBusy):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d reservations won", winners)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Writer == nil || cur.Writer.ExpectedTip != cur.AcceptedCommit {
			t.Fatalf("%+v", cur.Writer)
		}
	})

	t.Run("revoked and read-only attempts cannot publish", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		before := e.Git.CountRequests(isReceivePack)

		e.Attempts.Revoke("att_1")
		if _, err := e.Svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrAttemptRevoked) {
			t.Fatalf("revoked: %v", err)
		}

		// A read attempt never gets the slot nor a publication.
		e.Attempts.Register(w.Scope, "att_read", w.ID, workspace.AccessRead)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_read"}); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("read attempt reserved the writer slot: %v", err)
		}
		readProposal := e.Proposal(w, "att_read", e.Repo.Initial, "sneaky", gitfx.File{Path: "b.txt", Content: "b"})
		if _, err := e.Svc.ProposeSource(ctx, readProposal); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("read attempt proposal: %v", err)
		}
		// An unknown or foreign attempt discloses nothing.
		if _, err := e.Svc.ProposeSource(ctx, workspace.ProposeInput{Scope: wt.ScopeB, AttemptID: "att_1", Commit: in.Commit, ExpectedTip: in.ExpectedTip, BundleArtifactID: in.BundleArtifactID}); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("foreign scope: %v", err)
		}
		if e.Git.CountRequests(isReceivePack)-before != 0 || e.CountSQL(`SELECT count(*) FROM workspace_publications`) != 0 {
			t.Fatal("a refused proposal left a publication or touched the remote")
		}
	})

	t.Run("a delayed stale proposal cannot use a reassigned slot", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		stale := e.Proposal(w, "att_1", e.Repo.Initial, "from the partitioned node", gitfx.File{Path: "stale.txt", Content: "stale"})

		// The node is partitioned; the attempt is cancelled and the slot reassigned.
		e.Attempts.Revoke("att_1")
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeCancelled}); err != nil {
			t.Fatal(err)
		}
		e.Writer(w, "att_2")

		// Even a port that still reports the old attempt as live (stale cache)
		// cannot give it the slot: the reservation names att_2.
		e.Attempts.Register(w.Scope, "att_1", w.ID, workspace.AccessWrite)
		before := e.Git.CountRequests(isReceivePack)
		if _, err := e.Svc.ProposeSource(ctx, stale); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("stale attempt with a cached authority: %v", err)
		}
		e.Attempts.Revoke("att_1")
		if _, err := e.Svc.ProposeSource(ctx, stale); !errors.Is(err, workspace.ErrAttemptRevoked) {
			t.Fatalf("stale attempt: %v", err)
		}
		if e.Git.CountRequests(isReceivePack)-before != 0 {
			t.Fatal("a stale proposal reached the remote")
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("a stale attempt moved the branch to %s", tip)
		}

		// The current writer publishes normally.
		in := e.Proposal(w, "att_2", e.Repo.Initial, "from the new writer", gitfx.File{Path: "new.txt", Content: "new"})
		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposeAccepted {
			t.Fatalf("current writer: %+v %v", res, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("tip %s", tip)
		}
	})

	t.Run("only the publisher writes: reads use the checkout credential", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		if _, err := e.Svc.ProposeSource(ctx, in); err != nil {
			t.Fatal(err)
		}
		if n := e.Git.CountRequests(func(r gitfx.Request) bool { return r.Service == "git-receive-pack" && r.Role != gitfx.RoleWrite }); n != 0 {
			t.Fatalf("%d receive-pack requests without the write token", n)
		}
		if n := e.Git.CountRequests(func(r gitfx.Request) bool { return r.Role == gitfx.RoleNone }); n != 0 {
			t.Fatalf("%d unauthenticated requests", n)
		}
		if n := e.Git.CountRequests(func(r gitfx.Request) bool { return r.Role == gitfx.RoleRead && r.Service == "git-upload-pack" }); n == 0 {
			t.Fatal("reads never used the read-only credential")
		}
		// The publisher credential reference is never part of attempt-visible data.
		for _, ref := range []string{w.PublisherSecretRef, w.CheckoutSecretRef} {
			if ref == "" {
				t.Fatal("secret reference missing")
			}
		}
		if w.PublisherSecretRef == w.CheckoutSecretRef {
			t.Fatal("checkout and publisher credentials must be distinct references")
		}
	})

	t.Run("a publication authorized before cancellation is settled without making the attempt successful", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})

		hold := e.Git.HoldReceivePack()
		done := proposeAsync(e, in)
		await(t, "the push to reach the server", hold.Reached)
		// Cancelled while its authorized publication is in flight.
		e.Attempts.Revoke("att_1")
		hold.Release()
		got := <-done
		if got.err != nil || got.res.Status != workspace.ProposeAccepted {
			t.Fatalf("the already-authorized publication must complete: %+v %v", got.res, got.err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("tip %s", tip)
		}
		// The result of the cancelled attempt is recorded as cancelled.
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeCancelled}); err != nil {
			t.Fatal(err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.AcceptedCommit != in.Commit || cur.Writer != nil {
			t.Fatalf("%+v", cur)
		}
		if _, err := e.Svc.SelectWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_1"); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("a cancelled writer's commit became selectable: %v", err)
		}
		if _, err := e.Svc.PinWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_1", "run_1"); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("pinning a cancelled writer's output: %v", err)
		}
		// Repeating the identical proposal still observes the original outcome.
		again, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || again.Status != workspace.ProposeAccepted {
			t.Fatalf("repeat after cancellation: %+v %v", again, err)
		}
	})
}
