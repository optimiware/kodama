package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// ProposeStatus is the immediate answer to a source proposal.
type ProposeStatus string

const (
	// ProposeAccepted: the commit is on the remote branch and recorded.
	ProposeAccepted ProposeStatus = "accepted"
	// ProposePending: the publication is durably recorded but its outcome is
	// not yet known; repeat the identical proposal to observe it.
	ProposePending ProposeStatus = "pending"
)

// ProposeInput is an attempt's source proposal.
type ProposeInput struct {
	Scope            domain.Scope
	AttemptID        domain.AttemptID
	Commit           Commit
	ExpectedTip      Commit
	BundleArtifactID domain.ArtifactID
}

// ProposeResult is the observed state of the publication.
type ProposeResult struct {
	Status         ProposeStatus
	Commit         Commit
	AcceptedCommit Commit // equals Commit when accepted
}

const bundleRole = "bundle"

// ProposeSource validates and publishes an attempt's source commit.
//
// A new proposal is validated against the attempt's current authority, the live
// writer reservation, admission, the artifact's provenance and the bundle's
// contents; only then is the pending intent persisted, and only then is Git
// invoked. A repeated identical proposal observes the original outcome, a
// changed one is refused. A lost expected-tip race returns *ConflictError and
// preserves both the proposal and the observed tip. Acceptance is reported only
// after the remote branch is confirmed to contain the commit and that fact is
// durably recorded.
func (s *Service) ProposeSource(ctx context.Context, in ProposeInput) (ProposeResult, error) {
	if err := firstErr(in.Scope.Validate(), in.AttemptID.Validate(), in.Commit.Validate(), in.ExpectedTip.Validate(), in.BundleArtifactID.Validate()); err != nil {
		return ProposeResult{}, err
	}
	auth, err := s.d.Attempts.Authority(ctx, in.Scope, in.AttemptID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ProposeResult{}, ErrNotFound
	case err != nil:
		return ProposeResult{}, unavailable("attempt authority", err)
	}
	wsID := auth.WorkspaceID
	agg, err := s.d.Store.View(ctx, in.Scope, wsID, Load{Receipt: in.AttemptID})
	if err != nil {
		return ProposeResult{}, err
	}
	if agg.Receipt != nil {
		return s.observeReceipt(ctx, auth, agg.Receipt, in)
	}

	if !auth.Live {
		return ProposeResult{}, ErrAttemptRevoked
	}
	if auth.Access != AccessWrite {
		return ProposeResult{}, ErrNotWriter
	}
	w := agg.Workspace
	if err := w.requireExternal("a bundle proposal"); err != nil {
		return ProposeResult{}, err
	}
	switch {
	case w.Fence.Fenced:
		return ProposeResult{}, ErrFenced
	case w.Writer == nil || w.Writer.AttemptID != in.AttemptID:
		return ProposeResult{}, ErrNotWriter
	case !w.AdmissionOpen():
		return ProposeResult{}, ErrAdmissionClosed
	case in.ExpectedTip != w.Writer.ExpectedTip:
		return ProposeResult{}, fmt.Errorf("%w: expected tip does not match the reservation", ErrInvalidValue)
	}

	bundle, err := s.d.Bundles.Open(ctx, in.Scope, in.BundleArtifactID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ProposeResult{}, ErrNotFound
	case err != nil:
		return ProposeResult{}, unavailable("bundle artifact", err)
	}
	defer bundle.Body.Close()
	switch {
	case bundle.Scope != in.Scope || bundle.AttemptID != in.AttemptID:
		return ProposeResult{}, ErrNotFound // another attempt's or tenant's artifact
	case !bundle.Complete || bundle.Role != bundleRole:
		return ProposeResult{}, ErrBundleIncomplete
	}
	needles, err := s.d.Secrets.Needles(ctx, in.Scope, in.AttemptID)
	if err != nil {
		return ProposeResult{}, unavailable("secret checks", err)
	}

	pctx, cancel := s.gitCtx(ctx)
	prepared, err := s.d.Source.Prepare(pctx, PrepareRequest{
		Repo: s.repoRef(w), Branch: w.WorkBranch, Body: bundle.Body, Digest: bundle.Digest, Size: bundle.Size,
		Input: w.Writer.InputCommit, ExpectedTip: in.ExpectedTip, Commit: in.Commit, Needles: needles,
	})
	cancel()
	if err != nil {
		return ProposeResult{}, err
	}
	defer prepared.Release()

	proposal := Proposal{AttemptID: in.AttemptID, Commit: in.Commit, ExpectedTip: in.ExpectedTip, ArtifactID: in.BundleArtifactID, BundleDigest: bundle.Digest}
	observed := prepared.ObservedTip()
	reason := ""
	if observed != in.ExpectedTip {
		reason = "tip_moved"
	} else if ff, err := prepared.DescendsFrom(ctx, in.ExpectedTip); err != nil {
		return ProposeResult{}, unavailable("ancestry check", err)
	} else if !ff {
		reason = "not_fast_forward"
	}

	actor := Actor{Type: ActorWorkload, ID: string(in.AttemptID)}
	var replay *Publication
	_, err = s.d.Store.Update(ctx, in.Scope, wsID, Load{Receipt: in.AttemptID}, func(a *Aggregate) (Effects, error) {
		r, disp, err := a.Workspace.BeginPublication(proposal, a.Receipt, s.now())
		if err != nil {
			return Effects{}, err
		}
		if disp == DispositionReplay {
			replay = r
			return Effects{}, nil
		}
		fx := Effects{Receipt: r, Audit: []Audit{audit(actor, "workspace.publication_recorded", "", map[string]any{
			"attemptId": string(in.AttemptID), "commit": string(in.Commit), "expectedTip": string(in.ExpectedTip), "artifactId": string(in.BundleArtifactID)})}}
		if reason != "" {
			if err := a.Workspace.RecordConflict("", r, observed, reason, s.now()); err != nil {
				return Effects{}, err
			}
			fx.Audit = append(fx.Audit, audit(actor, "workspace.publication_conflict", reason, map[string]any{"observedTip": string(observed)}))
		}
		return fx, nil
	})
	if err != nil {
		return ProposeResult{}, err
	}
	if replay != nil {
		return s.observeReceipt(ctx, auth, replay, in)
	}
	if reason != "" {
		_ = s.d.Bundles.Settled(ctx, in.Scope, in.BundleArtifactID)
		return ProposeResult{}, &ConflictError{ObservedTip: observed, Reason: reason}
	}
	if err := s.advance(ctx, in.Scope, wsID, prepared); err != nil {
		if errors.Is(err, ErrFenced) {
			return ProposeResult{}, err
		}
		s.d.Logger.Warn("publication not settled", "workspace", wsID, "error", err.Error())
	}
	return s.receiptResult(ctx, in)
}

// observeReceipt answers a repeated proposal from the recorded receipt,
// resuming the same publication when it is still pending and the attempt still
// holds authority. It never creates or changes a publication.
func (s *Service) observeReceipt(ctx context.Context, auth WriterAuthority, r *Publication, in ProposeInput) (ProposeResult, error) {
	same := r.AttemptID == in.AttemptID && r.Commit == in.Commit && r.ExpectedCommit == in.ExpectedTip && r.ArtifactID == in.BundleArtifactID
	if !same {
		return ProposeResult{}, ErrProposalChanged
	}
	if r.Status == PublicationPending && auth.Live && auth.Access == AccessWrite {
		if err := s.advance(ctx, in.Scope, r.WorkspaceID, nil); err != nil {
			if errors.Is(err, ErrFenced) {
				return ProposeResult{}, err
			}
			s.d.Logger.Warn("publication not settled", "workspace", r.WorkspaceID, "error", err.Error())
		}
	}
	return s.receiptResult(ctx, in)
}

func (s *Service) receiptResult(ctx context.Context, in ProposeInput) (ProposeResult, error) {
	auth, err := s.d.Attempts.Authority(ctx, in.Scope, in.AttemptID)
	if err != nil {
		return ProposeResult{}, unavailable("attempt authority", err)
	}
	agg, err := s.d.Store.View(ctx, in.Scope, auth.WorkspaceID, Load{Receipt: in.AttemptID})
	if err != nil {
		return ProposeResult{}, err
	}
	r := agg.Receipt
	if r == nil {
		return ProposeResult{}, ErrNotFound
	}
	switch r.Status {
	case PublicationPublished:
		return ProposeResult{Status: ProposeAccepted, Commit: r.Commit, AcceptedCommit: r.Commit}, nil
	case PublicationConflict:
		return ProposeResult{}, &ConflictError{ObservedTip: r.ObservedTip, Reason: r.ConflictReason}
	}
	return ProposeResult{Status: ProposePending, Commit: r.Commit}, nil
}

type evidenceKind int

const (
	evUnknown evidenceKind = iota
	evPublished
	evUnchanged
	evConflict
)

type evidence struct {
	kind     evidenceKind
	observed Commit
	reason   string
}

// remoteEvidence reads the remote branch and classifies the pending
// publication. An unchanged tip is not proof the push failed: only a moved ref
// (which makes any delayed request fail its compare-and-swap) or a later
// successful repeat settles it.
func (s *Service) remoteEvidence(ctx context.Context, w *Workspace) evidence {
	p := w.Pending
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()
	info, err := s.d.Refs.BranchContains(rctx, s.repoRef(w), w.WorkBranch, p.Commit)
	switch {
	case err != nil:
		return evidence{kind: evUnknown}
	case !info.Exists:
		return evidence{kind: evConflict, reason: "branch_missing"}
	case info.Contains:
		return evidence{kind: evPublished, observed: info.Tip}
	case info.Tip == p.ExpectedCommit:
		return evidence{kind: evUnchanged, observed: info.Tip}
	}
	return evidence{kind: evConflict, observed: info.Tip, reason: "tip_moved"}
}

// advance moves the workspace's pending publication toward settlement: claim
// the intent, read the remote, and either record the result, repeat the
// idempotent push, or leave it pending with ownership retained. prepared may be
// nil; it is then rebuilt from the retained bundle when a push is needed.
func (s *Service) advance(ctx context.Context, scope domain.Scope, id ID, prepared Prepared) error {
	// Claims belong to one drive invocation, not the whole Service. A
	// concurrent archive/reconcile pass in this process must respect a live
	// push rather than renew and release its claim under the same identity.
	driverID := randomID("pub")
	var due bool
	agg, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
		w := a.Workspace
		if w.Pending == nil {
			return Effects{}, nil
		}
		due = w.Pending.RedriveDue(s.now(), s.o.RedriveAfter)
		if err := w.ClaimPublisher(driverID, s.now(), s.o.DriverStaleAfter); err != nil {
			return Effects{}, err
		}
		return Effects{}, nil
	})
	switch {
	case errors.Is(err, ErrPublisherBusy):
		return nil // another driver owns the same intent
	case err != nil:
		return err
	}
	w := agg.Workspace
	if w.Pending == nil {
		return nil
	}
	p := *w.Pending
	release := func() {
		_, _ = s.d.Store.Update(context.WithoutCancel(ctx), scope, id, Load{}, func(a *Aggregate) (Effects, error) {
			a.Workspace.ReleasePublisher(driverID, s.now())
			return Effects{}, nil
		})
	}
	// A completed drive no longer owns live work, even when settlement failed.
	// Release only this invocation's claim; the pending publication/writer stay
	// durable for another invocation or process to reconcile.
	defer release()

	ev := s.remoteEvidence(ctx, w)
	if ev.kind == evUnchanged && due {
		if perr := s.push(ctx, driverID, w, p, prepared); perr != nil {
			s.d.Logger.Warn("publication push failed; reading remote", "workspace", id, "error", perr.Error())
			if errors.Is(perr, ErrFenced) {
				return perr
			}
		}
		ev = s.remoteEvidence(ctx, w)
	}
	switch ev.kind {
	case evPublished:
		return s.settlePublished(ctx, driverID, w, p)
	case evConflict:
		return s.settleConflict(ctx, driverID, w, p, ev)
	}
	return nil
}

// push performs the explicit expected-old-ref update from verified scratch.
func (s *Service) push(ctx context.Context, driverID string, w *Workspace, p PendingPublication, prepared Prepared) error {
	if _, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{}, func(a *Aggregate) (Effects, error) {
		return Effects{}, a.Workspace.RecordPushAttempt(driverID, s.now())
	}); err != nil {
		return err
	}
	if prepared == nil {
		var err error
		if prepared, err = s.prepareRetained(ctx, w, p); err != nil {
			return err
		}
		defer prepared.Release()
	}
	pctx, cancel := s.gitCtx(ctx)
	defer cancel()
	return s.d.Publisher.Publish(pctx, s.repoRef(w), PublishRequest{Branch: w.WorkBranch, Expected: p.ExpectedCommit, Commit: p.Commit, Source: prepared})
}

// prepareRetained rebuilds verified scratch from the bundle kept until the
// publication settles. The proposal passed the secret scan at intake.
func (s *Service) prepareRetained(ctx context.Context, w *Workspace, p PendingPublication) (Prepared, error) {
	if w.Writer == nil {
		return nil, fmt.Errorf("%w: pending publication without a writer", ErrInvalidTransition)
	}
	b, err := s.d.Bundles.Open(ctx, w.Scope, p.ArtifactID)
	if err != nil {
		return nil, unavailable("bundle artifact", err)
	}
	defer b.Body.Close()
	if b.Digest != p.BundleDigest {
		return nil, &BundleError{Reason: ReasonDigestMismatch}
	}
	pctx, cancel := s.gitCtx(ctx)
	defer cancel()
	return s.d.Source.Prepare(pctx, PrepareRequest{
		Repo: s.repoRef(w), Branch: w.WorkBranch, Body: b.Body, Digest: b.Digest, Size: b.Size,
		Input: w.Writer.InputCommit, ExpectedTip: p.ExpectedCommit, Commit: p.Commit,
	})
}

func (s *Service) settlePublished(ctx context.Context, driverID string, w *Workspace, p PendingPublication) error {
	actor := Actor{Type: ActorWorkload, ID: string(p.AttemptID)}
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Receipt: p.AttemptID}, func(a *Aggregate) (Effects, error) {
		if a.Workspace.Pending == nil || a.Workspace.Pending.Commit != p.Commit {
			return Effects{}, nil // settled concurrently
		}
		prev, err := a.Workspace.RecordPublished(driverID, a.Receipt, s.now())
		if err != nil {
			return Effects{}, err
		}
		return Effects{
			Receipt: a.Receipt,
			Audit: []Audit{audit(actor, "workspace.published", "", map[string]any{
				"attemptId": string(p.AttemptID), "previousCommit": string(prev), "commit": string(p.Commit)})},
			Events: []Event{{Kind: EventCheckpointCreated, Actor: actor, PreviousCommit: prev, NewCommit: p.Commit, PublishingAttempt: p.AttemptID}},
		}, nil
	})
	return err
}

func (s *Service) settleConflict(ctx context.Context, driverID string, w *Workspace, p PendingPublication, ev evidence) error {
	actor := Actor{Type: ActorWorkload, ID: string(p.AttemptID)}
	_, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{Receipt: p.AttemptID}, func(a *Aggregate) (Effects, error) {
		if a.Workspace.Pending == nil || a.Workspace.Pending.Commit != p.Commit {
			return Effects{}, nil
		}
		if err := a.Workspace.RecordConflict(driverID, a.Receipt, ev.observed, ev.reason, s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{
			Receipt: a.Receipt,
			Audit: []Audit{audit(actor, "workspace.publication_conflict", ev.reason, map[string]any{
				"attemptId": string(p.AttemptID), "observedTip": string(ev.observed)})},
		}, nil
	})
	return err
}
