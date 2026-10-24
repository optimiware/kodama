package workspace_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func auditCount(e *wt.Env, action string, resource workspace.ID) int {
	return e.CountSQL(`SELECT count(*) FROM audit WHERE action = $1 AND resource_id = $2`, action, string(resource))
}

// Test_retained_commit: WS-02, WS-05, WS-11, WS-12, WS-A04 and WS-A08. Pinned
// readers recover their original commit after the branch moved and Git
// collected garbage; downstream selection requires a specific successful
// writer; removal of a referenced commit is refused and audited.
func Test_retained_commit(t *testing.T) {
	ctx := context.Background()

	t.Run("a pinned reader keeps its commit after the branch moves and Git collects garbage", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		pinned := e.Repo.HumanPush(t, w.WorkBranch, "reader's source", gitfx.File{Path: "src.txt", Content: "v1"})
		unpinned := e.Repo.HumanPush(t, w.WorkBranch, "unprotected", gitfx.File{Path: "src.txt", Content: "v2"})
		if _, err := e.Svc.Sync(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); err != nil {
			t.Fatal(err)
		}

		pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(pinned), RunID: "run_1"})
		if err != nil {
			t.Fatal(err)
		}
		if pin.Status != workspace.PinConfirmed || pin.Commit != workspace.Commit(pinned) {
			t.Fatalf("%+v", pin)
		}
		if e.Repo.Tip(t, pin.RefName) != pinned {
			t.Fatalf("the remote reference %s does not hold the commit", pin.RefName)
		}
		// Repeating the request returns the same pin.
		again, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(pinned), RunID: "run_1"})
		if err != nil || again.ID != pin.ID {
			t.Fatalf("repeat: %v %+v", err, again)
		}

		// The work branch is rewritten away from both commits and Git collects garbage.
		e.Repo.SetRef(t, w.WorkBranch, e.Repo.Initial)
		e.Repo.GC(t)
		if !e.Repo.HasCommit(t, pinned) {
			t.Fatal("the pinned commit was lost")
		}
		if e.Repo.HasCommit(t, unpinned) {
			t.Fatal("control: an unreferenced commit should have been collected")
		}
		// A reader still obtains exactly that commit with its read credential.
		got, found, err := e.Reader.ResolveRef(ctx, workspace.RepoRef{Scope: w.Scope, URL: w.RepositoryURL, CheckoutSecretRef: w.CheckoutSecretRef, PublisherSecretRef: w.PublisherSecretRef}, pin.RefName)
		if err != nil || !found || got != workspace.Commit(pinned) {
			t.Fatalf("reader: %v %v %v", got, found, err)
		}
		info, err := e.Reader.BranchContains(ctx, workspace.RepoRef{Scope: w.Scope, URL: w.RepositoryURL, CheckoutSecretRef: w.CheckoutSecretRef}, pin.RefName, workspace.Commit(pinned))
		if err != nil || !info.Contains {
			t.Fatalf("reader history: %+v %v", info, err)
		}
	})

	t.Run("a commit outside the workspace history cannot be pinned, nor by another tenant", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		orphan := e.Repo.OrphanCommit(t, "elsewhere")
		if _, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(orphan), RunID: "run_1"}); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("orphan commit: %v", err)
		}
		if _, err := e.Svc.PinCommit(ctx, workspace.Actor{Type: workspace.ActorUser, ID: "usr_other_tenant"}, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(e.Repo.Initial), RunID: "run_1"}); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("other tenant: %v", err)
		}
		if _, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: wt.ScopeB, WorkspaceID: w.ID, Commit: workspace.Commit(e.Repo.Initial), RunID: "run_1"}); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("foreign scope: %v", err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_retained_refs`); n != 0 {
			t.Fatalf("%d pins created by refused requests", n)
		}
	})

	t.Run("downstream selection requires a specific successful writer", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		if _, err := e.Svc.ProposeSource(ctx, in); err != nil {
			t.Fatal(err)
		}
		// Published, but the attempt has not (yet) succeeded.
		if _, err := e.Svc.SelectWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_1"); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("unsettled writer: %v", err)
		}
		// A reported source that is not the published one cannot succeed.
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: workspace.Commit(e.Repo.Initial)}); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("wrong reported source: %v", err)
		}
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: in.Commit}); err != nil {
			t.Fatal(err)
		}
		pin, err := e.Svc.PinWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_1", "run_2")
		if err != nil || pin.Purpose != workspace.PinWriterOutput || pin.AttemptID != "att_1" || pin.Commit != in.Commit {
			t.Fatalf("%+v %v", pin, err)
		}
		if e.Repo.Tip(t, pin.RefName) != in.Commit.String() {
			t.Fatal("writer output is not retained remotely")
		}
		// No other attempt can be selected for this workspace.
		if _, err := e.Svc.SelectWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_other"); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("another attempt: %v", err)
		}
		// A writer that failed after publishing is not a successful output.
		w2 := e.Writer(w, "att_2")
		in2 := e.Proposal(w2, "att_2", in.Commit.String(), "second", gitfx.File{Path: "b.txt", Content: "b"})
		if _, err := e.Svc.ProposeSource(ctx, in2); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2", Outcome: workspace.OutcomeFailed}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.PinWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_2", "run_3"); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("failed writer: %v", err)
		}
		// A no-change writer is verified against its expected tip, and succeeds without a proposal.
		w3 := e.Writer(w, "att_3")
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_3", Outcome: workspace.OutcomeSucceeded, SourceCommit: workspace.Commit(e.Repo.Initial)}); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("a no-change writer reporting a different commit: %v", err)
		}
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_3", Outcome: workspace.OutcomeSucceeded, SourceCommit: w3.Writer.ExpectedTip}); err != nil {
			t.Fatalf("no-change writer: %v", err)
		}
	})

	t.Run("removal of a referenced commit is refused and audited; denial, deferral and approval all leave a record", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		h := e.Repo.HumanPush(t, w.WorkBranch, "src", gitfx.File{Path: "s.txt", Content: "s"})
		if _, err := e.Svc.Sync(ctx, wt.Engineer, wt.ScopeA, w.ID, 0); err != nil {
			t.Fatal(err)
		}
		pin, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(h), RunID: "run_1"})
		if err != nil {
			t.Fatal(err)
		}

		// No deletion policy: denied, however the pin is used.
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); !errors.Is(err, workspace.ErrRemovalDenied) {
			t.Fatalf("no policy: %v", err)
		}
		// With a policy grant, a live reference is still protected.
		e.Policy.Allow = true
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); !errors.Is(err, workspace.ErrRemovalDenied) {
			t.Fatalf("referenced pin: %v", err)
		}
		if e.Repo.Tip(t, pin.RefName) != h {
			t.Fatal("a referenced commit was unprotected")
		}
		if n := auditCount(e, "workspace.retained_ref.removal_requested", w.ID); n != 2 {
			t.Fatalf("%d removal-request audit rows; denied requests must be audited", n)
		}

		// Released but the publication slot is unsettled: deferred, audited.
		if _, err := e.Svc.ReleasePin(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID); err != nil {
			t.Fatal(err)
		}
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", h, "pending one", gitfx.File{Path: "p.txt", Content: "p"})
		// Hold the push so the publication stays unsettled while removal is requested.
		hold := e.Git.HoldReceivePack()
		done := proposeAsync(e, in)
		await(t, "push", hold.Reached)
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); !errors.Is(err, workspace.ErrRemovalDeferred) {
			t.Fatalf("unsettled publication: %v", err)
		}
		if e.Repo.Tip(t, pin.RefName) != h {
			t.Fatal("deferred removal deleted the reference")
		}
		hold.Release()
		<-done

		// Settled and released: removal proceeds, remote ref deleted and recorded.
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "cleanup"); err != nil {
			t.Fatal(err)
		}
		if e.Repo.Tip(t, pin.RefName) != "" {
			t.Fatal("the reference was not deleted")
		}
		if n := auditCount(e, "workspace.retained_ref.removal_requested", w.ID); n != 4 {
			t.Fatalf("%d removal-request audit rows", n)
		}
		if n := auditCount(e, "workspace.retained_ref.removed", w.ID); n != 1 {
			t.Fatalf("%d removed audit rows", n)
		}
		// Removing again is idempotent.
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pin.ID, "again"); err != nil {
			t.Fatalf("repeat: %v", err)
		}
	})

	t.Run("interrupted reference creation and deletion are reconciled", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Policy.Allow = true

		// The remote ref is created but the acknowledgment is lost: not usable yet.
		faults.Publisher.CreateAckLost = 1
		if _, err := e.Svc.PinCommit(ctx, wt.Engineer, workspace.PinInput{Scope: w.Scope, WorkspaceID: w.ID, Commit: workspace.Commit(e.Repo.Initial), RunID: "run_1"}); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("a pin must not be usable before its reference is confirmed: %v", err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_retained_refs WHERE status = 'intent'`); n != 1 {
			t.Fatalf("%d intent pins", n)
		}
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_retained_refs WHERE status = 'confirmed'`); n != 1 {
			t.Fatalf("%d confirmed pins after reconciliation", n)
		}
		var pinID workspace.PinID
		if err := e.Migration.QueryRow(ctx, `SELECT id FROM workspace_retained_refs`).Scan(&pinID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.ReleasePin(ctx, wt.Engineer, wt.ScopeA, w.ID, pinID); err != nil {
			t.Fatal(err)
		}
		// Deletion applied remotely, acknowledgment lost: the pin stays "removing".
		faults.Publisher.DeleteAckLost = 1
		if err := e.Svc.RequestRemoval(ctx, wt.Engineer, wt.ScopeA, w.ID, pinID, "cleanup"); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("lost deletion acknowledgment: %v", err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_retained_refs WHERE status = 'removing'`); n != 1 {
			t.Fatalf("%d removing pins", n)
		}
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_retained_refs WHERE status = 'removed'`); n != 1 {
			t.Fatalf("%d removed pins after reconciliation", n)
		}
	})
}
