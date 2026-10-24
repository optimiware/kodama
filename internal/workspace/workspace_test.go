package workspace_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

var (
	t0    = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	cA    = workspace.Commit(strings.Repeat("a", 40))
	cB    = workspace.Commit(strings.Repeat("b", 40))
	cC    = workspace.Commit(strings.Repeat("c", 40))
	cD    = workspace.Commit(strings.Repeat("d", 40))
	scope = domain.Scope{TenantID: "t013_a", ProjectID: "p013_a"}
)

func newWS(t *testing.T) *workspace.Workspace {
	t.Helper()
	w, err := workspace.New(workspace.NewParams{
		ID: "ws_1", Scope: scope, WorkItemID: "wi_1", RepositoryID: "repo_1", RepositoryURL: "https://git.example.invalid/r.git",
		CheckoutSecretRef: "secret://read", PublisherSecretRef: "secret://write",
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func available(t *testing.T) *workspace.Workspace {
	t.Helper()
	w := newWS(t)
	if err := w.CompleteProvisioning(cA, t0); err != nil {
		t.Fatal(err)
	}
	return w
}

func proposal(attempt string, commit, tip workspace.Commit) workspace.Proposal {
	return workspace.Proposal{
		AttemptID: domain.AttemptID(attempt), Commit: commit, ExpectedTip: tip, ArtifactID: "art_1",
		BundleDigest: "sha256:" + strings.Repeat("0", 64),
	}
}

// Test_workspace_transitions: invalid transitions, changed proposals, premature
// release, invalid values and failed-writer output selection are rejected, and
// the valid path moves through explicit states with version bumps.
func Test_workspace_transitions(t *testing.T) {
	t.Run("a new workspace is provisioning, versioned and on the portable branch", func(t *testing.T) {
		w := newWS(t)
		if w.State != workspace.StateProvisioning || w.Version != 1 || w.WorkBranch != "refs/heads/kodama/work/wi_1" {
			t.Fatalf("%+v", w)
		}
		if w.AdmissionOpen() {
			t.Fatal("a provisioning workspace admits writers")
		}
		if err := w.ReserveWriter("att_1", cA, cA, t0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("reserve while provisioning: %v", err)
		}
	})

	t.Run("invalid values are rejected", func(t *testing.T) {
		bad := workspace.NewParams{ID: "ws_1", Scope: scope, WorkItemID: "wi_1", RepositoryID: "r", RepositoryURL: "https://x/y.git", CheckoutSecretRef: "same", PublisherSecretRef: "same"}
		if _, err := workspace.New(bad, t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("identical checkout and publisher references: %v", err)
		}
		bad.PublisherSecretRef = "other"
		bad.WorkItemID = "wi/../x"
		if _, err := workspace.New(bad, t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("hostile work item id: %v", err)
		}
		w := newWS(t)
		if err := w.CompleteProvisioning("deadbeef", t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("short commit: %v", err)
		}
		for _, ref := range []string{"main", "refs/heads/", "refs/heads/a..b", "refs/heads/x.lock", "refs/tags/v1", "refs/heads/-x", "refs/heads/a b"} {
			if workspace.ValidateBranch(ref) == nil {
				t.Errorf("%q accepted as a branch", ref)
			}
		}
	})

	t.Run("provisioning completes once and records the confirmed commit", func(t *testing.T) {
		w := newWS(t)
		if changed, err := w.RecordInitialIntent(cA, t0); err != nil || !changed {
			t.Fatalf("intent: %v %v", changed, err)
		}
		if changed, _ := w.RecordInitialIntent(cB, t0); changed || w.InitialCommit != cA {
			t.Fatal("the first recorded intent must win")
		}
		if _, err := w.RecordProvisioningError("repository_unavailable", t0); err != nil {
			t.Fatal(err)
		}
		if err := w.CompleteProvisioning(cA, t0); err != nil {
			t.Fatal(err)
		}
		if w.State != workspace.StateAvailable || w.AcceptedCommit != cA || w.LastError != "" {
			t.Fatalf("%+v", w)
		}
		if err := w.CompleteProvisioning(cA, t0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("second completion: %v", err)
		}
	})

	t.Run("one writer; the stale tip, a second attempt and archive are refused", func(t *testing.T) {
		w := available(t)
		if err := w.ReserveWriter("att_1", cA, cB, t0); !errors.Is(err, workspace.ErrStale) {
			t.Fatalf("stale tip: %v", err)
		}
		if err := w.ReserveWriter("att_1", cA, cA, t0); err != nil {
			t.Fatal(err)
		}
		if err := w.ReserveWriter("att_1", cA, cA, t0); err != nil {
			t.Fatalf("idempotent repeat: %v", err)
		}
		if err := w.ReserveWriter("att_2", cA, cA, t0); !errors.Is(err, workspace.ErrWriterBusy) {
			t.Fatalf("second writer: %v", err)
		}
		v := w.Version
		if err := w.ReserveWriter("att_1", cA, cA, t0); err != nil || w.Version != v {
			t.Fatal("an idempotent repeat changed the version")
		}
		if err := w.ReleaseWriter("att_9", t0); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("release by a stranger: %v", err)
		}
	})

	t.Run("a pending publication blocks release, sync, archive completion and restore", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		rcpt, disp, err := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if err != nil || disp != workspace.DispositionNew || w.Pending == nil {
			t.Fatalf("begin: %v %v", disp, err)
		}
		if err := w.ReleaseWriter("att_1", t0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("premature release: %v", err)
		}
		if _, err := w.ObserveHumanTip(cA, cC, true, t0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("sync while pending: %v", err)
		}
		if _, err := w.RequestArchive(t0); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CompleteArchive(t0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("archive while pending: %v", err)
		}
		if err := w.Restore(t0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("restore while pending: %v", err)
		}
		if err := w.SettleWriter("att_1", workspace.OutcomeSucceeded, rcpt, t0); !errors.Is(err, workspace.ErrPublicationPending) {
			t.Fatalf("settle while pending: %v", err)
		}
	})

	t.Run("publication identity is durable: repeats observe, changes are refused", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		r, _, err := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if err != nil {
			t.Fatal(err)
		}
		again, disp, err := w.BeginPublication(proposal("att_1", cB, cA), r, t0)
		if err != nil || disp != workspace.DispositionReplay || again != r {
			t.Fatalf("identical repeat: %v %v", disp, err)
		}
		changed := proposal("att_1", cC, cA)
		if _, _, err := w.BeginPublication(changed, r, t0); !errors.Is(err, workspace.ErrProposalChanged) {
			t.Fatalf("changed commit: %v", err)
		}
		other := proposal("att_1", cB, cA)
		other.ArtifactID = "art_2"
		if _, _, err := w.BeginPublication(other, r, t0); !errors.Is(err, workspace.ErrProposalChanged) {
			t.Fatalf("changed bundle: %v", err)
		}
		// A different attempt cannot take over the slot.
		if _, _, err := w.BeginPublication(proposal("att_2", cC, cA), nil, t0); !errors.Is(err, workspace.ErrNotWriter) {
			t.Fatalf("other attempt: %v", err)
		}
	})

	t.Run("proposal validation", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		if _, _, err := w.BeginPublication(proposal("att_1", cA, cA), nil, t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("no-change proposal: %v", err)
		}
		if _, _, err := w.BeginPublication(proposal("att_1", cB, cC), nil, t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("expected tip differing from the reservation: %v", err)
		}
		p := proposal("att_1", cB, cA)
		p.BundleDigest = "md5:zz"
		if _, _, err := w.BeginPublication(p, nil, t0); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("bad digest: %v", err)
		}
	})

	t.Run("only the live, unfenced claim finalizes; a fenced publisher cannot", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		r, _, _ := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if err := w.ClaimPublisher("pub_old", t0, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := w.ClaimPublisher("pub_new", t0.Add(time.Second), time.Minute); !errors.Is(err, workspace.ErrPublisherBusy) {
			t.Fatalf("a live claim must be respected: %v", err)
		}
		w.FencePublishing(t0)
		if _, err := w.RecordPublished("pub_old", r, t0); !errors.Is(err, workspace.ErrFenced) {
			t.Fatalf("fenced publisher finalized: %v", err)
		}
		if err := w.ReserveWriter("att_9", cA, cA, t0); !errors.Is(err, workspace.ErrFenced) {
			t.Fatalf("a fenced workspace admitted a writer: %v", err)
		}
		if !w.ResumePublishing(t0) {
			t.Fatal("resume")
		}
		// The old claim belongs to an earlier epoch: another driver resumes the same intent.
		if err := w.ClaimPublisher("pub_new", t0, time.Hour); err != nil {
			t.Fatalf("resuming after the fence: %v", err)
		}
		if _, err := w.RecordPublished("pub_old", r, t0); !errors.Is(err, workspace.ErrFenced) {
			t.Fatalf("the old publisher finalized after the new claim: %v", err)
		}
		prev, err := w.RecordPublished("pub_new", r, t0)
		if err != nil || prev != cA || w.AcceptedCommit != cB || w.Pending != nil || r.Status != workspace.PublicationPublished {
			t.Fatalf("finalize: %v %v %+v", prev, err, w)
		}
	})

	t.Run("a stale claim may be resumed for the same intent only", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		_, _, _ = w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		_ = w.ClaimPublisher("pub_a", t0, time.Minute)
		if err := w.ClaimPublisher("pub_b", t0.Add(2*time.Minute), time.Minute); err != nil {
			t.Fatal(err)
		}
		if w.Pending.Commit != cB || w.Pending.AttemptID != "att_1" || w.Pending.ExpectedCommit != cA {
			t.Fatalf("the intent changed: %+v", w.Pending)
		}
	})

	t.Run("conflict preserves the proposal and never moves the accepted commit", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		r, _, _ := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if err := w.RecordConflict("", r, cC, "tip_moved", t0); err != nil {
			t.Fatal(err)
		}
		if w.AcceptedCommit != cA || w.Pending != nil || r.Status != workspace.PublicationConflict || r.ObservedTip != cC || r.Commit != cB {
			t.Fatalf("%+v %+v", w, r)
		}
		// A lost race cannot be recorded as a successful writer, nor selected.
		if err := w.SettleWriter("att_1", workspace.OutcomeSucceeded, r, t0); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("succeeded after conflict: %v", err)
		}
		if _, err := r.SelectableOutput(); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("selectable: %v", err)
		}
		if err := w.SettleWriter("att_1", workspace.OutcomeFailed, r, t0); err != nil || w.Writer != nil {
			t.Fatalf("failed settlement: %v", err)
		}
	})

	t.Run("a published commit of a cancelled or failed writer is never a successful output", func(t *testing.T) {
		for _, outcome := range []workspace.WriterOutcome{workspace.OutcomeCancelled, workspace.OutcomeFailed} {
			w := available(t)
			_ = w.ReserveWriter("att_1", cA, cA, t0)
			r, _, _ := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
			_ = w.ClaimPublisher("pub", t0, time.Minute)
			if _, err := w.RecordPublished("pub", r, t0); err != nil {
				t.Fatal(err)
			}
			if err := w.SettleWriter("att_1", outcome, r, t0); err != nil {
				t.Fatal(err)
			}
			if w.AcceptedCommit != cB {
				t.Fatal("the published commit is history and stays accepted")
			}
			if _, err := r.SelectableOutput(); !errors.Is(err, workspace.ErrOutputNotSelectable) {
				t.Fatalf("%s writer selectable: %v", outcome, err)
			}
		}
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		r, _, _ := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		_ = w.ClaimPublisher("pub", t0, time.Minute)
		_, _ = w.RecordPublished("pub", r, t0)
		if err := w.SettleWriter("att_1", workspace.OutcomeSucceeded, r, t0); err != nil {
			t.Fatal(err)
		}
		if c, err := r.SelectableOutput(); err != nil || c != cB {
			t.Fatalf("successful writer: %v %v", c, err)
		}
		if err := w.SettleWriter("att_1", "bogus", r, t0); err == nil {
			t.Fatal("accepted an invalid outcome")
		}
	})

	t.Run("a writer's reported source is verified", func(t *testing.T) {
		w := available(t)
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		if err := w.VerifyWriterSource("att_1", cA, nil); err != nil {
			t.Fatalf("no-change writer reports the expected tip: %v", err)
		}
		if err := w.VerifyWriterSource("att_1", cB, nil); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("unproposed source: %v", err)
		}
		r, _, _ := w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if err := w.VerifyWriterSource("att_1", cB, r); !errors.Is(err, workspace.ErrOutputNotSelectable) {
			t.Fatalf("unpublished source: %v", err)
		}
	})

	t.Run("human synchronization only fast-forwards", func(t *testing.T) {
		w := available(t)
		if changed, err := w.ObserveHumanTip(cA, cB, true, t0); err != nil || !changed || w.AcceptedCommit != cB {
			t.Fatalf("fast-forward: %v %v", changed, err)
		}
		if _, err := w.ObserveHumanTip(cA, cC, true, t0); !errors.Is(err, workspace.ErrStale) {
			t.Fatalf("observation against an outdated accepted commit: %v", err)
		}
		if _, err := w.ObserveHumanTip(cB, cD, false, t0); !errors.Is(err, workspace.ErrDiverged) {
			t.Fatalf("diverged: %v", err)
		}
		if w.AcceptedCommit != cB || w.LastError != workspace.ReasonBranchDiverged {
			t.Fatalf("a diverged tip must be recorded, never adopted: %+v", w)
		}
		// Back in line: the safe reason clears.
		if changed, err := w.ObserveHumanTip(cB, cB, true, t0); err != nil || !changed || w.LastError != "" {
			t.Fatalf("recovery: %v %v", changed, err)
		}
	})

	t.Run("archive closes admission first; restore reopens the same workspace", func(t *testing.T) {
		w := available(t)
		if changed, err := w.RequestArchive(t0); err != nil || !changed || w.AdmissionOpen() {
			t.Fatalf("request: %v %v", changed, err)
		}
		if err := w.ReserveWriter("att_1", cA, cA, t0); !errors.Is(err, workspace.ErrAdmissionClosed) {
			t.Fatalf("admission after the archive request: %v", err)
		}
		if changed, _ := w.RequestArchive(t0); changed {
			t.Fatal("the archive request must be idempotent")
		}
		if changed, err := w.CompleteArchive(t0); err != nil || !changed || w.State != workspace.StateArchived {
			t.Fatalf("complete: %v %v", changed, err)
		}
		id, branch, commit := w.ID, w.WorkBranch, w.AcceptedCommit
		if err := w.Restore(t0); err != nil {
			t.Fatal(err)
		}
		if w.State != workspace.StateAvailable || w.ID != id || w.WorkBranch != branch || w.AcceptedCommit != commit || !w.AdmissionOpen() {
			t.Fatalf("restore replaced or lost the workspace: %+v", w)
		}
		if err := w.Restore(t0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("restoring an available workspace: %v", err)
		}
		if _, err := newWS(t).RequestArchive(t0); !errors.Is(err, workspace.ErrInvalidTransition) {
			t.Fatalf("archiving while provisioning: %v", err)
		}
	})

	t.Run("version preconditions", func(t *testing.T) {
		w := available(t)
		if err := w.CheckVersion(w.Version + 1); !errors.Is(err, workspace.ErrVersionConflict) {
			t.Fatal(err)
		}
		if err := w.CheckVersion(w.Version); err != nil {
			t.Fatal(err)
		}
		if err := w.CheckVersion(0); err != nil {
			t.Fatal("zero means no precondition")
		}
	})

	t.Run("retained references follow their lifecycle and refuse unsafe removal", func(t *testing.T) {
		w := available(t)
		pin, err := workspace.NewPin("pin_1", w, cA, workspace.PinReader, "run_1", "", workspace.DefaultRetainedRefPrefix, t0)
		if err != nil {
			t.Fatal(err)
		}
		if pin.RefName != "refs/heads/kodama/retained/ws_1/run_1-pin_1" || pin.Status != workspace.PinIntent || pin.Live() != true {
			t.Fatalf("%+v", pin)
		}
		if err := pin.Release(t0); err == nil {
			t.Fatal("released an unconfirmed pin")
		}
		_ = pin.Confirm(t0)
		d := workspace.EvaluateRemoval(w, pin, []*workspace.RetainedRef{pin}, true)
		if d.Verdict != workspace.VerdictDeny || d.Reason != "referenced" {
			t.Fatalf("live reference: %+v", d)
		}
		_ = pin.Release(t0)
		if d := workspace.EvaluateRemoval(w, pin, []*workspace.RetainedRef{pin}, false); d.Verdict != workspace.VerdictDeny || d.Reason != "deletion_policy" {
			t.Fatalf("no policy: %+v", d)
		}
		_ = w.ReserveWriter("att_1", cA, cA, t0)
		_, _, _ = w.BeginPublication(proposal("att_1", cB, cA), nil, t0)
		if d := workspace.EvaluateRemoval(w, pin, []*workspace.RetainedRef{pin}, true); d.Verdict != workspace.VerdictDefer {
			t.Fatalf("unsettled publication: %+v", d)
		}
		w.Pending = nil
		other, _ := workspace.NewPin("pin_2", w, cA, workspace.PinReader, "run_2", "", workspace.DefaultRetainedRefPrefix, t0)
		if d := workspace.EvaluateRemoval(w, pin, []*workspace.RetainedRef{pin, other}, true); d.Verdict != workspace.VerdictDefer || d.Reason != "pin_in_progress" {
			t.Fatalf("concurrent pin of the same commit: %+v", d)
		}
		if d := workspace.EvaluateRemoval(w, pin, []*workspace.RetainedRef{pin}, true); d.Verdict != workspace.VerdictAllow {
			t.Fatalf("eligible: %+v", d)
		}
		_ = pin.BeginRemoval(t0)
		if err := workspace.CheckPinAdmission(cA, []*workspace.RetainedRef{pin}); !errors.Is(err, workspace.ErrRetentionBusy) {
			t.Fatalf("pin admission during removal: %v", err)
		}
		_ = pin.CompleteRemoval(t0)
		if pin.Status != workspace.PinRemoved {
			t.Fatal(pin.Status)
		}
		if _, err := workspace.NewPin("pin_3", w, cA, workspace.PinWriterOutput, "run_1", "", workspace.DefaultRetainedRefPrefix, t0); err == nil {
			t.Fatal("a writer-output pin needs its attempt")
		}
	})
}
