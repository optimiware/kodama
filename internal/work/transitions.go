package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// edge is one allowed state change with the permission it requires and whether
// a reason is mandatory.
type edge struct {
	perm           Permission
	reasonRequired bool
}

type edgeKey struct{ from, to State }

// edges is the complete transition table of docs/specs/work-management.md. Any
// pair not listed, including every edge out of cancelled and out of done other
// than reopening, is forbidden.
var edges = map[edgeKey]edge{
	{StateBacklog, StateReady}:     {perm: PermRefine},
	{StateReady, StateBacklog}:     {perm: PermRefine, reasonRequired: true},
	{StateReady, StateActive}:      {perm: PermWork},
	{StateActive, StateReady}:      {perm: PermWork, reasonRequired: true},
	{StateActive, StateReview}:     {perm: PermWork},
	{StateReview, StateActive}:     {perm: PermWork, reasonRequired: true},
	{StateReview, StateDone}:       {perm: PermComplete},
	{StateDone, StateActive}:       {perm: PermReopen, reasonRequired: true},
	{StateBacklog, StateCancelled}: {perm: PermCancel, reasonRequired: true},
	{StateReady, StateCancelled}:   {perm: PermCancel, reasonRequired: true},
	{StateActive, StateCancelled}:  {perm: PermCancel, reasonRequired: true},
	{StateReview, StateCancelled}:  {perm: PermCancel, reasonRequired: true},
}

// AllowedEdge reports whether from -> to is an allowed edge.
func AllowedEdge(from, to State) bool {
	_, ok := edges[edgeKey{from, to}]
	return ok
}

// EntersActive reports whether the edge consumes an active WIP slot. Every
// entry into active, including rework and reopening, uses the same admission.
func EntersActive(from, to State) bool { return to == StateActive && from != StateActive }

// PermissionsForTarget lists the permissions that could grant a transition to
// the target state; the caller needs at least one. It supports an early
// authorization check before the current state is read; the exact edge
// permission is re-evaluated inside the transaction.
func PermissionsForTarget(to State) []Permission {
	var out []Permission
	for k, e := range edges {
		if k.to != to {
			continue
		}
		dup := false
		for _, p := range out {
			dup = dup || p == e.perm
		}
		if !dup {
			out = append(out, e.perm)
		}
	}
	return out
}

// TransitionRequest is an explicit business transition. Review and Completion
// carry the edge-specific evidence; the actor and expected version are not part
// of it.
type TransitionRequest struct {
	To         State
	Reason     string
	Review     *ReviewClaim
	Completion *CompletionClaim
}

// TransitionEnv supplies the current facts a transition depends on. The
// PostgreSQL adapter binds it to the mutating transaction, so every answer is
// evaluated after the item (and, for admissions, the project) is locked and a
// lock wait cannot leave it stale. Every method fails closed: an unavailable
// guard returns ErrUnavailable, never a pass.
type TransitionEnv interface {
	// Authorize re-checks the actor's current permission.
	Authorize(ctx context.Context, actor Actor, scope domain.Scope, perm Permission) error
	// AdmitActive checks active WIP capacity under the project lock and
	// returns ErrWIPLimit when none remains.
	AdmitActive(ctx context.Context, item Item) error
	// WriterSettled reports whether no writer attempt remains unsettled.
	WriterSettled(ctx context.Context, item Item) (bool, error)
	// DependenciesSatisfied returns an ErrPrerequisite error while a dependency
	// blocks admission.
	DependenciesSatisfied(ctx context.Context, item Item) error
	// VerifySubject confirms the claim against current durable state.
	VerifySubject(ctx context.Context, item Item, claim ReviewClaim) (VerifiedSubject, error)
	// ChildSummary summarizes an epic's children.
	ChildSummary(ctx context.Context, item Item) (ChildSummary, error)
}

// Transition applies req to the item and returns the new item with its history
// entry. It checks the edge, the actor's permission, the required reason and
// every prerequisite of the edge through env.
func (it Item) Transition(ctx context.Context, env TransitionEnv, req TransitionRequest, actor Actor, now time.Time, historyID, requestID string) (Item, HistoryEntry, error) {
	if err := actor.Validate(); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if !req.To.Valid() {
		return Item{}, HistoryEntry{}, invalid("unknown target state %q", string(req.To))
	}
	e, ok := edges[edgeKey{it.State, req.To}]
	if !ok {
		return Item{}, HistoryEntry{}, fmt.Errorf("%w: %s to %s is not allowed", ErrInvalidTransition, it.State, req.To)
	}
	// Completion is a human decision: the actor type is verified by the
	// authority port, and the current permission is re-read in the transaction.
	if req.To == StateDone && !actor.IsHuman() {
		return Item{}, HistoryEntry{}, fmt.Errorf("%w: completion is recorded by a human with %s", ErrNotHuman, PermComplete)
	}
	if err := env.Authorize(ctx, actor, it.Scope, e.perm); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if err := validateReason(req.Reason); err != nil {
		return Item{}, HistoryEntry{}, err
	}
	if e.reasonRequired && req.Reason == "" {
		return Item{}, HistoryEntry{}, prerequisite("a reason is required to move %s to %s", it.State, req.To)
	}
	if req.Review != nil && !(it.State == StateActive && req.To == StateReview) {
		return Item{}, HistoryEntry{}, invalid("review evidence applies only to the active to review transition")
	}
	if req.Completion != nil && req.To != StateDone {
		return Item{}, HistoryEntry{}, invalid("completion applies only to the transition to done")
	}

	next := it
	var review *ReviewSubject
	var completion *CompletionRecord
	now = now.UTC().Truncate(time.Microsecond)

	switch {
	case it.State == StateBacklog && req.To == StateReady:
		if !it.HasSpecification() {
			return Item{}, HistoryEntry{}, prerequisite("a specification and acceptance criteria are required before ready")
		}

	case it.State == StateActive && (req.To == StateReady || req.To == StateReview):
		// A request to leave active while a writer remains is refused; the
		// caller cancels the run and retries after settlement.
		if it.Kind.Actionable() {
			settled, err := env.WriterSettled(ctx, it)
			if err != nil {
				return Item{}, HistoryEntry{}, err
			}
			if !settled {
				return Item{}, HistoryEntry{}, fmt.Errorf("%w: a writer attempt has not settled", ErrWorkspaceBusy)
			}
		}
		if req.To == StateReview {
			r, err := it.reviewSubject(ctx, env, req.Review, now)
			if err != nil {
				return Item{}, HistoryEntry{}, err
			}
			review = r
		}

	case req.To == StateDone:
		rec, err := it.complete(ctx, env, req.Completion, actor, req.Reason, now)
		if err != nil {
			return Item{}, HistoryEntry{}, err
		}
		completion = rec
	}

	if it.State == StateReady && req.To == StateActive {
		if it.BlockedReason != "" {
			return Item{}, HistoryEntry{}, prerequisite("a blocked work item cannot start; clear the blocked reason first")
		}
		if err := env.DependenciesSatisfied(ctx, it); err != nil {
			return Item{}, HistoryEntry{}, err
		}
	}
	// WIP admission is evaluated last, after the cheaper prerequisites, but in
	// the same transaction that writes the transition.
	if EntersActive(it.State, req.To) {
		if err := env.AdmitActive(ctx, it); err != nil {
			return Item{}, HistoryEntry{}, err
		}
	}

	next.State = req.To
	next.Version = it.Version + 1
	if err := next.Version.Validate(); err != nil {
		return Item{}, HistoryEntry{}, invalid("%v", err)
	}
	next.UpdatedAt = now
	switch req.To {
	case StateReview:
		next.ReviewSubject, next.Completion = review, nil
	case StateDone:
		next.ReviewSubject, next.Completion = it.ReviewSubject, completion
	default:
		next.ReviewSubject, next.Completion = nil, nil
	}
	h := HistoryEntry{
		ID: historyID, Scope: it.Scope, WorkItemID: it.ID, Version: next.Version, ContentRevision: next.ContentRevision,
		Kind: ChangeTransitioned, Actor: actor, OccurredAt: now, Reason: req.Reason,
		From: it.State, To: req.To, Before: map[string]any{}, After: map[string]any{},
		Review: review, Completion: completion, RequestID: requestID,
	}
	if req.To == StateDone {
		h.Review = it.ReviewSubject
	}
	return next, h, nil
}

// reviewSubject verifies the evidence for active -> review.
func (it Item) reviewSubject(ctx context.Context, env TransitionEnv, claim *ReviewClaim, now time.Time) (*ReviewSubject, error) {
	if it.Kind == KindEpic {
		if claim != nil {
			return nil, invalid("an epic enters review with its child summary, not a candidate")
		}
		sum, err := env.ChildSummary(ctx, it)
		if err != nil {
			return nil, err
		}
		if !sum.Complete() {
			return nil, prerequisite("every child of the epic must be done or cancelled, and at least one done (children %d, done %d, cancelled %d)", sum.Total, sum.Done, sum.Cancelled)
		}
		return &ReviewSubject{ChildSummary: &sum, ContentRevision: it.ContentRevision, RecordedAt: now}, nil
	}
	if claim == nil || claim.CandidateCommit == "" || len(claim.Evidence) == 0 {
		return nil, prerequisite("a candidate commit and evidence are required to enter review")
	}
	if !IsCommit(claim.CandidateCommit) {
		return nil, invalid("candidate commit must be 40 or 64 lowercase hex digits")
	}
	if err := validateEvidenceLinks(claim.Evidence); err != nil {
		return nil, err
	}
	verified, err := env.VerifySubject(ctx, it, *claim)
	if err != nil {
		return nil, err
	}
	if verified.CandidateCommit != claim.CandidateCommit || len(verified.Evidence) == 0 {
		return nil, prerequisite("the candidate and evidence could not be verified")
	}
	return &ReviewSubject{CandidateCommit: verified.CandidateCommit, Evidence: cloneEvidence(verified.Evidence), ContentRevision: it.ContentRevision, RecordedAt: now}, nil
}

// IsBusinessError reports whether err is one of the module's classified
// failures rather than an infrastructure error.
func IsBusinessError(err error) bool {
	for _, s := range []error{ErrInvalid, ErrNotFound, ErrDenied, ErrHidden, ErrVersionConflict, ErrInvalidTransition,
		ErrPrerequisite, ErrWorkspaceBusy, ErrWIPLimit, ErrStaleSubject, ErrNotHuman, ErrUnavailable, ErrNotActionable, ErrCancelled} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}
