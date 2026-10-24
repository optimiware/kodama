package workspace_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func archiveOptions(wait time.Duration) workspace.Options {
	o := wt.DefaultOptions()
	o.ArchiveWait = wait
	return o
}

// Test_archive_publish_race: WS-07, WS-12 and WS-A08. Archive closes admission
// before it waits for settlement, cannot complete during an uncertain
// publication, survives a restart of the control plane, and restore reopens the
// same workspace without losing source.
func Test_archive_publish_race(t *testing.T) {
	ctx := context.Background()

	t.Run("archive closes admission; a proposal arriving after it is refused", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "late", gitfx.File{Path: "a.txt", Content: "a"})

		archived, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil || archived.State != workspace.StateArchived {
			t.Fatalf("%+v %v", archived, err)
		}
		before := e.Git.CountRequests(isReceivePack)
		if _, err := e.Svc.ProposeSource(ctx, in); !errors.Is(err, workspace.ErrAdmissionClosed) {
			t.Fatalf("proposal after archive: %v", err)
		}
		e.Attempts.Register(w.Scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err == nil {
			t.Fatal("an archived workspace admitted a writer")
		}
		if e.Git.CountRequests(isReceivePack) != before || e.CountSQL(`SELECT count(*) FROM workspace_publications`) != 0 {
			t.Fatal("archived workspace accepted source")
		}
		// Repeating the archive is idempotent.
		again, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil || again.Version != archived.Version {
			t.Fatalf("repeat: %+v %v", again, err)
		}
	})

	t.Run("archive cannot complete during an uncertain publication, survives restart, and restore reopens the workspace", func(t *testing.T) {
		e := wt.New(t, wt.Config{
			Service: archiveOptions(time.Second),
			Git:     func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second },
		})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		if res, err := e.Svc.ProposeSource(ctx, in); err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("%+v %v", res, err)
		}

		// The archive request is recorded durably, then settlement is awaited.
		got, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, 0)
		if !errors.Is(err, workspace.ErrArchiveUncertain) {
			t.Fatalf("archive must never report completion early: %+v %v", got, err)
		}
		if got == nil || got.State != workspace.StateAvailable || got.ArchiveRequestedAt == nil || got.Pending == nil {
			t.Fatalf("the request must be retained with the publication still pending: %+v", got)
		}
		// Admission is closed while the archive is unsettled.
		e.Attempts.Register(w.Scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err == nil {
			t.Fatal("admission was open during the archive")
		}
		// A different proposal by the same attempt cannot replace the in-flight one.
		other := e.Proposal(w, "att_1", e.Repo.Initial, "other", gitfx.File{Path: "b.txt", Content: "b"})
		if _, err := e.Svc.ProposeSource(ctx, other); !errors.Is(err, workspace.ErrProposalChanged) {
			t.Fatalf("changed proposal during archive: %v", err)
		}

		// A restarted control plane (new process, same database) keeps waiting.
		restarted := e.NewService(nil)
		if err := restarted.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ := restarted.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.State != workspace.StateAvailable || cur.Pending == nil || cur.ArchiveRequestedAt == nil {
			t.Fatalf("restart lost or completed the archive early: %+v", cur)
		}
		// Restore cannot reopen a workspace whose publication is unsettled.
		if _, err := restarted.Restore(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("restore during an unsettled publication: %v", err)
		}

		// The delayed request completes; reconciliation settles it and then finishes the archive.
		hold.Release()
		e.Git.WaitIdle(t)
		if err := restarted.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ = restarted.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.State != workspace.StateArchived || cur.Pending != nil || cur.AcceptedCommit != in.Commit {
			t.Fatalf("archive did not complete after settlement: %+v", cur)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("source lost: %s", tip)
		}

		// The writer is settled, then restore reopens the same workspace.
		if _, err := restarted.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: in.Commit}); err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.Restore(ctx, wt.Engineer, wt.ScopeA, w.ID, cur.Version+9); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatalf("stale restore: %v", err)
		}
		restored, err := restarted.Restore(ctx, wt.Engineer, wt.ScopeA, w.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if restored.ID != w.ID || restored.WorkBranch != w.WorkBranch || restored.AcceptedCommit != in.Commit || restored.State != workspace.StateAvailable || restored.ArchiveRequestedAt != nil {
			t.Fatalf("restore must reopen the same workspace without source loss: %+v", restored)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspaces`); n != 1 {
			t.Fatalf("%d workspaces after restore", n)
		}
		// Provisioning the same work item again returns it, never a replacement.
		again, created, err := e.Svc.Provision(ctx, wt.Engineer, wt.ScopeA, "wi_1")
		if err != nil || created || again.ID != w.ID {
			t.Fatalf("provision after restore: %+v %v %v", again, created, err)
		}
		// The restored workspace admits a writer again, from the preserved commit.
		w2 := e.Writer(restored, "att_2")
		if w2.Writer.ExpectedTip != in.Commit {
			t.Fatalf("%+v", w2.Writer)
		}
	})

	t.Run("reconciliation in the same service respects an in-flight publisher", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		defer hold.Release()
		proposed := proposeAsync(e, in)
		await(t, "the in-flight push", hold.Reached)
		// The same process may also be running an archive or background pass.
		// Completing its pass must not release the request's still-live claim.
		if err := e.Svc.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		hold.Release()
		if got := <-proposed; got.err != nil || got.res.Status != workspace.ProposeAccepted {
			t.Fatalf("same-service reconciliation fenced the live publisher: %+v %v", got.res, got.err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("published tip %s, want %s", tip, in.Commit)
		}
	})

	t.Run("archive waits for a concurrent publication to settle before returning", func(t *testing.T) {
		e := wt.New(t, wt.Config{Service: archiveOptions(20 * time.Second)})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		proposed := proposeAsync(e, in)
		await(t, "the push", hold.Reached)

		type archiveResult struct {
			w   *workspace.Workspace
			err error
		}
		archived := make(chan archiveResult, 1)
		go func() {
			w, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, 0)
			archived <- archiveResult{w, err}
		}()
		// Admission closes first, while the publication is still in flight.
		deadline := time.Now().Add(10 * time.Second)
		for {
			cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
			if cur.ArchiveRequestedAt != nil {
				if cur.State != workspace.StateAvailable || cur.Pending == nil {
					t.Fatalf("admission closed but the publication settled early: %+v", cur)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("archive never closed admission")
			}
			time.Sleep(10 * time.Millisecond)
		}
		select {
		case r := <-archived:
			t.Fatalf("archive returned before settlement: %+v %v", r.w, r.err)
		case <-time.After(300 * time.Millisecond):
		}
		hold.Release()
		if got := <-proposed; got.err != nil || got.res.Status != workspace.ProposeAccepted {
			t.Fatalf("the in-flight publication must complete: %+v %v", got.res, got.err)
		}
		r := <-archived
		if r.err != nil || r.w.State != workspace.StateArchived || r.w.Pending != nil || r.w.AcceptedCommit != in.Commit {
			t.Fatalf("%+v %v", r.w, r.err)
		}
	})

	t.Run("archive, restore and provisioning-state rules", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		e.Items.Add(wt.ScopeA, "wi_p", "story")
		w := e.Ready(wt.ScopeA, "wi_1")
		if _, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, w.Version+3); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatalf("stale If-Match: %v", err)
		}
		if _, err := e.Svc.Archive(ctx, workspace.Actor{Type: workspace.ActorUser, ID: "usr_other_tenant"}, wt.ScopeA, w.ID, 0); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("other tenant: %v", err)
		}
		if _, err := e.Svc.Restore(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("restoring an available workspace: %v", err)
		}
		arch, err := e.Svc.Archive(ctx, wt.Engineer, wt.ScopeA, w.ID, w.Version)
		if err != nil {
			t.Fatal(err)
		}
		// Archiving preserves the retained source history.
		if e.Repo.Tip(t, w.WorkBranch) != e.Repo.Initial {
			t.Fatal("branch lost")
		}
		if n := e.CountTenant(wt.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'workspace.state_changed' AND convert_from(payload, 'UTF8') LIKE '%"to":"archived"%'`); n != 1 {
			t.Fatalf("%d archived events", n)
		}
		if _, err := e.Svc.Restore(ctx, wt.Engineer, wt.ScopeA, w.ID, arch.Version); err != nil {
			t.Fatal(err)
		}
	})
}
