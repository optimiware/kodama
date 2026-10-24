package workspace_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func checkpointEvents(e *wt.Env) int {
	return e.CountTenant(wt.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'workspace.checkpoint_created'`)
}

func pendingOf(t *testing.T, e *wt.Env, w *workspace.Workspace) *workspace.PendingPublication {
	t.Helper()
	cur, err := e.Svc.Get(context.Background(), wt.Engineer, w.Scope, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cur.Pending
}

// Test_push_ack_loss: WS-06, WS-08, WS-11, WS-A05 and WS-A06. A push whose
// acknowledgment is lost, a database failure after the remote update and a
// request that completes after the client timed out are reconciled exactly
// once; an unchanged tip is not proof of failure, so ownership is retained; the
// writer slot is not reassigned until the outcome is known; retries keep the
// proposal identity; credentials never enter the bundle.
func Test_push_ack_loss(t *testing.T) {
	ctx := context.Background()

	t.Run("a dropped acknowledgment is reconciled from the remote exactly once", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		e.Git.DropNextReceivePackAcks(1)

		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposeAccepted || res.AcceptedCommit != in.Commit {
			t.Fatalf("%+v %v", res, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("tip %s", tip)
		}
		if faults.Publisher.PublishCalls != 1 {
			t.Fatalf("the publisher pushed %d times; the lost acknowledgment must be resolved by reading the remote", faults.Publisher.PublishCalls)
		}
		if n := checkpointEvents(e); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.AcceptedCommit != in.Commit || cur.Pending != nil {
			t.Fatalf("%+v", cur)
		}
		// The writer is released only after settlement.
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: in.Commit}); err != nil {
			t.Fatal(err)
		}
		if c, err := e.Svc.SelectWriterOutput(ctx, wt.Engineer, wt.ScopeA, w.ID, "att_1"); err != nil || c != in.Commit {
			t.Fatalf("successful writer output: %v %v", c, err)
		}
	})

	t.Run("a database failure after the remote update is reconciled exactly once", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		faults.Publisher.AfterPublish = func(err error) {
			if err == nil {
				faults.Store.FailNextUpdates(1) // the control plane loses its database right after the push
			}
		}
		before := e.Git.CountRequests(isReceivePack)

		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("a result that could not be recorded must not be acknowledged: %+v %v", res, err)
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("the remote must already hold the commit: %s", tip)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending == nil || cur.AcceptedCommit != workspace.Commit(e.Repo.Initial) {
			t.Fatalf("nothing is accepted until it is durably recorded: %+v", cur)
		}
		// The slot cannot be taken over, released or reassigned while unsettled.
		e.Attempts.Register(w.Scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); !errors.Is(err, workspace.ErrWriterBusy) && !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("reassignment during an unknown publication: %v", err)
		}
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded}); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("settling before the publication is known: %v", err)
		}

		// A different control-plane process finds the crashed publisher's claim
		// once it is stale, and resumes the same intent from the remote evidence.
		other := e.NewService(nil)
		e.Clock.Advance(2 * time.Minute)
		if err := other.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ = e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil || cur.AcceptedCommit != in.Commit {
			t.Fatalf("not reconciled: %+v", cur)
		}
		if n := e.Git.CountRequests(isReceivePack) - before; n != 1 {
			t.Fatalf("reconciliation pushed again (%d requests)", n)
		}
		if n := checkpointEvents(e); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
		// Reconciling again changes nothing.
		v := cur.Version
		if err := other.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		if again, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID); again.Version != v {
			t.Fatal("a settled publication was changed by another reconcile")
		}
		// The repeated proposal observes the recorded outcome.
		res, err = e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposeAccepted {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("an unchanged tip after a timeout retains ownership until the delayed request is resolved", func(t *testing.T) {
		e := wt.New(t, wt.Config{Git: func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second }})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		before := e.Git.CountRequests(isReceivePack)

		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("a timed-out push is unknown, not failed: %+v %v", res, err)
		}
		select {
		case <-hold.Reached:
		default:
			t.Fatal("the push never reached the server")
		}
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != e.Repo.Initial {
			t.Fatalf("tip moved early: %s", tip)
		}
		first := pendingOf(t, e, w)
		if first == nil || first.Commit != in.Commit || first.ExpectedCommit != in.ExpectedTip {
			t.Fatalf("intent not retained: %+v", first)
		}

		// Reconciliation sees an unchanged ref. That does not prove failure.
		if err := e.Svc.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		still := pendingOf(t, e, w)
		if still == nil || still.AttemptID != "att_1" || still.Commit != in.Commit {
			t.Fatalf("ownership was released on an unchanged tip: %+v", still)
		}
		if n := e.Git.CountRequests(isReceivePack) - before; n != 1 {
			t.Fatalf("reconciliation pushed while the old request could still complete (%d)", n)
		}
		e.Attempts.Register(w.Scope, "att_2", w.ID, workspace.AccessWrite)
		if _, err := e.Svc.ReserveWriter(ctx, wt.Engineer, workspace.ReserveWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_2"}); err == nil {
			t.Fatal("the writer slot was reassigned during an uncertain publication")
		}

		// The delayed request finally completes on the server.
		hold.Release()
		e.Git.WaitIdle(t)
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("the delayed request did not complete: %s", tip)
		}
		if err := e.Svc.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil || cur.AcceptedCommit != in.Commit {
			t.Fatalf("%+v", cur)
		}
		if n := checkpointEvents(e); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
	})

	t.Run("a repeat after the redrive interval is the same intent and settles once", func(t *testing.T) {
		e := wt.New(t, wt.Config{Git: func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second }})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		if res, err := e.Svc.ProposeSource(ctx, in); err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("%+v %v", res, err)
		}
		// Long enough since the last attempt: the same CAS may be repeated.
		e.Clock.Advance(2 * time.Hour)
		if err := e.Svc.ReconcileWorkspace(ctx, w.Scope, w.ID); err != nil {
			t.Fatal(err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil || cur.AcceptedCommit != in.Commit {
			t.Fatalf("the repeated CAS did not settle: %+v", cur)
		}
		// The delayed original now arrives; its old-ref expectation no longer
		// holds, so it cannot change anything.
		hold.Release()
		e.Git.WaitIdle(t)
		if tip := e.Repo.Tip(t, w.WorkBranch); tip != in.Commit.String() {
			t.Fatalf("tip %s", tip)
		}
		if n := checkpointEvents(e); n != 1 {
			t.Fatalf("%d checkpoint events", n)
		}
	})

	t.Run("a request that never reached the remote is repeated, not forgotten", func(t *testing.T) {
		var cfg wt.Config
		faults := wt.WithFaults(&cfg)
		e := wt.New(t, cfg)
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		faults.Publisher.PublishNeverSent = 1
		res, err := e.Svc.ProposeSource(ctx, in)
		if err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("%+v %v", res, err)
		}
		e.Clock.Advance(2 * time.Hour)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		cur, _ := e.Svc.Get(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if cur.Pending != nil || cur.AcceptedCommit != in.Commit {
			t.Fatalf("%+v", cur)
		}
	})

	t.Run("retries keep the proposal identity", func(t *testing.T) {
		e := wt.New(t, wt.Config{Git: func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second }})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		hold := e.Git.HoldReceivePack()
		defer hold.Release()
		if res, err := e.Svc.ProposeSource(ctx, in); err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("%+v %v", res, err)
		}
		first := pendingOf(t, e, w)
		// Identical repeat while pending: same answer, same intent.
		if res, err := e.Svc.ProposeSource(ctx, in); err != nil || res.Status != workspace.ProposePending {
			t.Fatalf("repeat: %+v %v", res, err)
		}
		if again := pendingOf(t, e, w); again == nil || again.StartedAt != first.StartedAt || again.Commit != first.Commit {
			t.Fatalf("the intent changed: %+v vs %+v", again, first)
		}
		// A changed proposal by the same attempt cannot replace the in-flight one.
		other := e.Proposal(w, "att_1", e.Repo.Initial, "different", gitfx.File{Path: "b.txt", Content: "b"})
		if _, err := e.Svc.ProposeSource(ctx, other); !errors.Is(err, workspace.ErrProposalChanged) {
			t.Fatalf("changed proposal: %v", err)
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_publications WHERE workspace_id = $1`, string(w.ID)); n != 1 {
			t.Fatalf("%d receipts", n)
		}
	})

	t.Run("credentials never enter the bundle or the published source", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		e.Secrets.Set("att_1", e.Git.WriteToken, e.Git.ReadToken, "injected-deploy-key-0001")

		leak := e.Proposal(w, "att_1", e.Repo.Initial, "leak", gitfx.File{Path: "cfg", Content: "key=injected-deploy-key-0001"})
		if _, err := e.Svc.ProposeSource(ctx, leak); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("an injected secret in new source: %v", err)
		}
		if e.Repo.Tip(t, w.WorkBranch) != e.Repo.Initial {
			t.Fatal("source containing a secret was published")
		}
		if n := e.CountSQL(`SELECT count(*) FROM workspace_publications`); n != 0 {
			t.Fatalf("a refused proposal left %d receipts", n)
		}

		clean := e.Proposal(w, "att_1", e.Repo.Initial, "clean", gitfx.File{Path: "a.txt", Content: "a"})
		art, _ := e.Bundles.Open(ctx, w.Scope, clean.BundleArtifactID)
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(art.Body)
		for _, tok := range []string{e.Git.WriteToken, e.Git.ReadToken} {
			if bytes.Contains(buf.Bytes(), []byte(tok)) {
				t.Fatal("a Git credential is inside the attempt's bundle")
			}
		}
		if _, err := e.Svc.ProposeSource(ctx, clean); err != nil {
			t.Fatal(err)
		}
		log := e.Repo.Git(t, "log", "-p", "--all")
		for _, tok := range []string{e.Git.WriteToken, e.Git.ReadToken} {
			if bytes.Contains([]byte(log), []byte(tok)) {
				t.Fatal("a Git credential is inside the published history")
			}
		}
	})

	t.Run("recovery starts from the published commit and reports ephemeral loss", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		// Attempt 1 dies with uncommitted work: nothing was published.
		e.Attempts.Revoke("att_1")
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeFailed}); err != nil {
			t.Fatal(err)
		}
		rec, err := e.Svc.RecoveryPoint(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if err != nil || rec.LastPublishedCommit != workspace.Commit(e.Repo.Initial) || !rec.UncommittedWorkMayBeLost {
			t.Fatalf("%+v %v", rec, err)
		}
		// Attempt 2 publishes; recovery now starts from that commit.
		w2 := e.Writer(w, "att_2")
		in := e.Proposal(w2, "att_2", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		if _, err := e.Svc.ProposeSource(ctx, in); err != nil {
			t.Fatal(err)
		}
		rec, err = e.Svc.RecoveryPoint(ctx, wt.Engineer, wt.ScopeA, w.ID)
		if err != nil || rec.LastPublishedCommit != in.Commit || !rec.UncommittedWorkMayBeLost {
			t.Fatalf("%+v %v", rec, err)
		}
		if _, err := e.Svc.RecoveryPoint(ctx, workspace.Actor{Type: workspace.ActorUser, ID: "usr_other_tenant"}, wt.ScopeA, w.ID); !errors.Is(err, workspace.ErrNotFound) {
			t.Fatalf("recovery information for another tenant: %v", err)
		}
	})
}
