package workspace

import (
	"context"
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// This file is the synchronization of the internally hosted task branches: what
// the host shows is observed outside SQL (observeSync) and what was found is
// applied in one mutation (applySync) that the caller guards with the workspace
// version or If-Match and with the accepted commits it observed against.

// maxMovementRecords bounds the applied push records read to explain how a task
// branch moved; a longer unexplained history is refused, never adopted.
const maxMovementRecords = 256

type syncKind int

const (
	syncNone    syncKind = iota // nothing to write (a push is still pending)
	syncClear                   // the branch matches again: drop a recorded observation
	syncAdopt                   // an explained human fast-forward
	syncObserve                 // the branch is missing, diverged or moved by an unaccepted push
)

// syncDecision is what was observed for one created association: the accepted
// commit it was judged against, and what to record. err is the refusal the
// caller returns once the observation is recorded.
type syncDecision struct {
	repo   RepositoryID
	from   Commit
	tip    Commit
	kind   syncKind
	reason string
	err    error
}

// firstRefusal is the error a set of decisions refuses with, most serious
// first: a diverged or missing branch, a workload push that is not an accepted
// checkpoint, a push still pending.
func firstRefusal(ds []syncDecision) error {
	for _, want := range []error{ErrDiverged, ErrCheckpointRequired, ErrPushPending} {
		for _, d := range ds {
			if d.err != nil && errors.Is(d.err, want) {
				return d.err
			}
		}
	}
	return nil
}

// observeSync observes every created association. A refusal for one does not
// stop the others; only infrastructure failures are errors.
func (s *Service) observeSync(ctx context.Context, w *Workspace, assocs []*RepositoryAssociation, cps []*InternalCheckpoint) ([]syncDecision, error) {
	var out []syncDecision
	for _, a := range assocs {
		if a.State != AssociationCreated {
			continue
		}
		d, err := s.observeOne(ctx, w, a, cps)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Service) observeOne(ctx context.Context, w *Workspace, a *RepositoryAssociation, cps []*InternalCheckpoint) (syncDecision, error) {
	d := syncDecision{repo: a.RepositoryID, from: a.AcceptedCommit}
	tip, ok, err := s.readRef(ctx, w.Scope, a.RepositoryID, a.TaskBranch)
	switch {
	case err != nil:
		return d, err
	case !ok:
		d.kind, d.reason, d.err = syncObserve, ReasonBranchMissing, ErrDiverged
		return d, nil
	case tip == d.from:
		d.kind = syncClear
		return d, nil
	}
	d.tip = tip
	if s.d.Inspector == nil {
		return d, fmt.Errorf("%w: push inspector", ErrNotConfigured)
	}
	repo, err := s.internalRepository(ctx, w.Scope, a.RepositoryID)
	if err != nil {
		return d, err
	}
	ictx, cancel := s.gitCtx(ctx)
	rel, err := s.d.Inspector.Inspect(ictx, InspectRequest{Repo: s.internalRepoRef(w.Scope, repo), Ref: a.TaskBranch, Commit: tip, Since: d.from})
	cancel()
	switch {
	case errors.Is(err, ErrLimitExceeded):
		return d, err
	case err != nil:
		return d, unavailable("branch history", err)
	case rel != RelationDescends:
		d.kind, d.reason, d.err = syncObserve, ReasonBranchDiverged, ErrDiverged
		return d, nil
	}
	switch err := s.explainMovement(ctx, w, a, tip, cps); {
	case err == nil:
		d.kind = syncAdopt
	case errors.Is(err, ErrPushPending):
		d.kind, d.err = syncNone, err
	case errors.Is(err, ErrCheckpointRequired):
		d.kind, d.reason, d.err = syncObserve, ReasonUnacceptedPush, err
	default:
		return d, err
	}
	return d, nil
}

// explainMovement decides, from durable push records alone, whether the task
// branch's move from the accepted commit to tip is human work that may be
// adopted. It is, only when no push of the ref is unsettled and the applied
// entries of the ref chain from the accepted commit to tip with every step made
// by an engineer; a step made by a workload counts only if that push was
// accepted as a checkpoint. A workload push by the attempt that currently holds
// the writer slot and is not accepted yet is pending (ErrPushPending, nothing
// recorded): its output may still be reported. A move nothing explains, or one
// that includes a workload push that was refused or never reported by an
// attempt that no longer holds the slot, is never adopted: the branch stays
// unaccepted (ErrCheckpointRequired) until an engineer resets it.
func (s *Service) explainMovement(ctx context.Context, w *Workspace, a *RepositoryAssociation, tip Commit, cps []*InternalCheckpoint) error {
	scope := w.Scope
	if err := s.requireSettled(ctx, scope, a); err != nil {
		return err
	}
	recs, err := s.d.RefPushes.AppliedOnRef(ctx, scope, string(a.RepositoryID), a.TaskBranch, maxMovementRecords)
	if err != nil {
		return s.refPushError("push records", err)
	}
	cur := tip
	for range len(recs) + 1 {
		if cur == a.AcceptedCommit {
			return nil
		}
		rec, entry := appliedEntryTo(recs, a, cur)
		if entry == nil {
			return fmt.Errorf("%w: no applied push explains how the task branch reached %s", ErrCheckpointRequired, short(cur))
		}
		switch rec.Actor.Type {
		case gitv1.ActorUser:
		case gitv1.ActorWorkload:
			if cp := FindCheckpoint(cps, a.RepositoryID, rec.PushID, a.TaskBranch); cp == nil || cp.Commit != cur {
				if w.Writer != nil && rec.Actor.AttemptID != nil && *rec.Actor.AttemptID == string(w.Writer.AttemptID) {
					// The current writer's own push, not yet reported through
					// AcceptCheckpoint: it is in progress, not a refusal.
					return fmt.Errorf("%w: the writer's push to %s is not accepted yet", ErrPushPending, short(cur))
				}
				return fmt.Errorf("%w: the push that moved the task branch to %s was not accepted", ErrCheckpointRequired, short(cur))
			}
		default:
			return fmt.Errorf("%w: the push that moved the task branch to %s has an unknown actor", ErrCheckpointRequired, short(cur))
		}
		if gitv1.IsZeroObjectID(entry.Old) {
			return fmt.Errorf("%w: the task branch was recreated", ErrCheckpointRequired)
		}
		cur = Commit(entry.Old)
	}
	return fmt.Errorf("%w: the push history of the task branch is not a chain", ErrCheckpointRequired)
}

// appliedEntryTo finds the newest record whose applied entry for the task
// branch set it to commit.
func appliedEntryTo(recs []gitv1.PushRecord, a *RepositoryAssociation, commit Commit) (gitv1.PushRecord, *gitv1.PushEntry) {
	for _, r := range recs {
		if r.RepositoryID != string(a.RepositoryID) {
			continue
		}
		for i := range r.Entries {
			e := &r.Entries[i]
			if e.Ref == a.TaskBranch && e.State == gitv1.EntryApplied && e.New == string(commit) {
				return r, e
			}
		}
	}
	return gitv1.PushRecord{}, nil
}

func short(c Commit) string {
	if len(c) > 12 {
		return string(c[:12])
	}
	return string(c)
}

// applySync writes the decisions into a locked aggregate: it never adopts what
// was not explained, and refuses (ErrStale) when any association's accepted
// commit is no longer the one the decision was made against. The workspace
// version moves once however many associations changed.
func (s *Service) applySync(a *Aggregate, actor Actor, ds []syncDecision) (Effects, error) {
	var fx Effects
	touched := false
	for _, d := range ds {
		as := findAssociation(a.Associations, d.repo)
		switch {
		case as == nil:
			return Effects{}, ErrNotFound
		case as.State != AssociationCreated || as.AcceptedCommit != d.from:
			return Effects{}, ErrStale
		}
		switch d.kind {
		case syncClear:
			if as.ClearObservation(s.now()) {
				touched = true
				fx.Associations = append(fx.Associations, as)
				fx.Audit = append(fx.Audit, audit(actor, "workspace.sync_cleared", "", map[string]any{"repositoryId": string(as.RepositoryID)}))
			}
		case syncObserve:
			changed, err := as.RecordObservation(d.from, d.reason, s.now())
			if err != nil {
				return Effects{}, err
			}
			if changed {
				touched = true
				fx.Associations = append(fx.Associations, as)
				fx.Audit = append(fx.Audit, audit(actor, "workspace.sync_diverged", d.reason, map[string]any{"repositoryId": string(as.RepositoryID)}))
			}
		case syncAdopt:
			if err := as.Advance(d.from, d.tip, s.now()); err != nil {
				return Effects{}, err
			}
			if as.RepositoryID == a.Workspace.RepositoryID {
				a.Workspace.AcceptedCommit = d.tip
			}
			touched = true
			fx.Associations = append(fx.Associations, as)
			fx.Audit = append(fx.Audit, audit(actor, "workspace.synchronized", "", map[string]any{
				"repositoryId": string(as.RepositoryID), "previousCommit": string(d.from), "commit": string(d.tip)}))
			fx.Events = append(fx.Events, Event{Kind: EventCheckpointCreated, Actor: actor, PreviousCommit: d.from, NewCommit: d.tip, HumanSynchronization: true, Repository: as.RepositoryID})
		}
	}
	if touched {
		a.Workspace.TouchAssociations(s.now())
	}
	return fx, nil
}

func (s *Service) internalRepoRef(scope domain.Scope, repo InternalRepository) RepoRef {
	return RepoRef{Scope: scope, URL: repo.URL, CheckoutSecretRef: repo.CheckoutSecretRef, PublisherSecretRef: NoPublisherRef}
}
