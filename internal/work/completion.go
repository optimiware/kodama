package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ChildSummary is the review evidence of an epic: the states of its children.
type ChildSummary struct {
	Total     int `json:"total"`
	Done      int `json:"done"`
	Cancelled int `json:"cancelled"`
}

// Complete reports whether every child reached a terminal state and at least one
// was delivered.
func (c ChildSummary) Complete() bool {
	return c.Total > 0 && c.Done+c.Cancelled == c.Total && c.Done > 0
}

// ReviewClaim is what the caller asserts when moving an actionable item to
// review: the candidate commit and the evidence supporting it. The claim is
// verified against current durable state inside the transition transaction.
type ReviewClaim struct {
	CandidateCommit string        `json:"candidateCommit"`
	Evidence        []EvidenceRef `json:"evidence"`
}

// VerifiedSubject is the current durable review subject a SubjectGuard
// confirmed: the candidate commit, the evidence that exists in scope and any
// business findings recorded against them.
type VerifiedSubject struct {
	CandidateCommit string
	Evidence        []EvidenceRef
	Findings        []Finding
}

// ReviewSubject is the subject recorded when an item entered review. Completion
// is bound to it.
type ReviewSubject struct {
	CandidateCommit string                 `json:"candidateCommit,omitempty"`
	Evidence        []EvidenceRef          `json:"evidence,omitempty"`
	ChildSummary    *ChildSummary          `json:"childSummary,omitempty"`
	ContentRevision domain.ContentRevision `json:"contentRevision"`
	RecordedAt      time.Time              `json:"recordedAt"`
}

// CompletionClaim is the explicit human decision accompanying a transition to
// done: the observed content revision and candidate, the accepted criteria and
// any acknowledged findings.
type CompletionClaim struct {
	ContentRevision      domain.ContentRevision `json:"contentRevision"`
	CandidateCommit      string                 `json:"candidateCommit,omitempty"`
	AcceptedCriteria     []string               `json:"acceptedCriteria"`
	AcknowledgedFindings []Finding              `json:"acknowledgedFindings,omitempty"`
}

// CompletionRecord is the retained decision. It is stored on the item while it
// is done and snapshotted, immutably, in the transition's history entry.
type CompletionRecord struct {
	CompletedBy          Actor                  `json:"completedBy"`
	CompletedAt          time.Time              `json:"completedAt"`
	ContentRevision      domain.ContentRevision `json:"contentRevision"`
	CandidateCommit      string                 `json:"candidateCommit,omitempty"`
	ChildSummary         *ChildSummary          `json:"childSummary,omitempty"`
	AcceptedCriteria     []string               `json:"acceptedCriteria"`
	AcknowledgedFindings []Finding              `json:"acknowledgedFindings,omitempty"`
	Reason               string                 `json:"reason,omitempty"`
}

// complete evaluates the review -> done decision. The caller already verified
// the edge and that the authority granted PermComplete to a human.
func (it Item) complete(ctx context.Context, env TransitionEnv, claim *CompletionClaim, actor Actor, reason string, now time.Time) (*CompletionRecord, error) {
	if claim == nil {
		return nil, prerequisite("completion requires the observed content revision, candidate and acceptance")
	}
	if err := validateCompletionClaim(claim); err != nil {
		return nil, err
	}
	subject := it.ReviewSubject
	if subject == nil {
		return nil, prerequisite("the item has no recorded review subject")
	}
	// The decision is bound to what the human observed.
	if claim.ContentRevision != it.ContentRevision || subject.ContentRevision != it.ContentRevision {
		return nil, fmt.Errorf("%w: the specification changed after review (content revision %d, observed %d)", ErrStaleSubject, it.ContentRevision, claim.ContentRevision)
	}
	rec := &CompletionRecord{
		CompletedBy: actor, CompletedAt: now.UTC().Truncate(time.Microsecond), ContentRevision: it.ContentRevision,
		AcceptedCriteria: slices.Clone(claim.AcceptedCriteria), Reason: reason,
	}
	var findings []Finding
	if it.Kind == KindEpic {
		if claim.CandidateCommit != "" {
			return nil, invalid("an epic has no candidate commit")
		}
		sum, err := env.ChildSummary(ctx, it)
		if err != nil {
			return nil, err
		}
		if subject.ChildSummary == nil || sum != *subject.ChildSummary || !sum.Complete() {
			return nil, fmt.Errorf("%w: the children changed after review", ErrStaleSubject)
		}
		rec.ChildSummary = &sum
	} else {
		if claim.CandidateCommit != subject.CandidateCommit {
			return nil, fmt.Errorf("%w: the candidate is not the recorded review candidate", ErrStaleSubject)
		}
		verified, err := env.VerifySubject(ctx, it, ReviewClaim{CandidateCommit: subject.CandidateCommit, Evidence: subject.Evidence})
		if errors.Is(err, ErrNotFound) {
			// A candidate or evidence that existed at review and is gone now is
			// a stale subject, not an unknown reference.
			return nil, fmt.Errorf("%w: the recorded candidate or evidence is no longer available", ErrStaleSubject)
		}
		if err != nil {
			return nil, err
		}
		if verified.CandidateCommit != subject.CandidateCommit {
			return nil, fmt.Errorf("%w: the candidate is no longer current", ErrStaleSubject)
		}
		rec.CandidateCommit = subject.CandidateCommit
		findings = verified.Findings
	}

	// Acceptance names every criterion of the observed revision.
	want := it.criterionIDs()
	seen := map[string]struct{}{}
	for _, id := range claim.AcceptedCriteria {
		if _, ok := want[id]; !ok {
			return nil, invalid("accepted criterion %q is not an acceptance criterion of this item", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != len(want) || len(claim.AcceptedCriteria) != len(seen) {
		return nil, prerequisite("completion must accept every acceptance criterion exactly once")
	}

	// Every open finding is acknowledged by name, and the reason names them.
	ack := map[string]Finding{}
	for _, f := range claim.AcknowledgedFindings {
		ack[f.ID] = f
	}
	open := map[string]struct{}{}
	for _, f := range findings {
		open[f.ID] = struct{}{}
		if _, ok := ack[f.ID]; !ok {
			return nil, prerequisite("finding %q must be acknowledged by name", f.ID)
		}
	}
	for id := range ack {
		if _, ok := open[id]; !ok {
			return nil, invalid("acknowledged finding %q is not a finding of the review subject", id)
		}
		if !strings.Contains(reason, id) {
			return nil, prerequisite("the completion reason must name acknowledged finding %q", id)
		}
	}
	rec.AcknowledgedFindings = slices.Clone(claim.AcknowledgedFindings)
	return rec, nil
}

func validateCompletionClaim(c *CompletionClaim) error {
	if err := c.ContentRevision.Validate(); err != nil {
		return invalid("%v", err)
	}
	if c.CandidateCommit != "" && !IsCommit(c.CandidateCommit) {
		return invalid("candidate commit must be 40 or 64 lowercase hex digits")
	}
	if len(c.AcceptedCriteria) > MaxCriteria {
		return invalid("too many accepted criteria")
	}
	for _, id := range c.AcceptedCriteria {
		if !criterionIDPattern.MatchString(id) {
			return invalid("accepted criterion id must match %s", criterionIDPattern)
		}
	}
	if len(c.AcknowledgedFindings) > MaxFindings {
		return invalid("at most %d findings may be acknowledged", MaxFindings)
	}
	seen := map[string]struct{}{}
	for _, f := range c.AcknowledgedFindings {
		if err := f.Validate(); err != nil {
			return err
		}
		if _, dup := seen[f.ID]; dup {
			return invalid("finding %q is acknowledged twice", f.ID)
		}
		seen[f.ID] = struct{}{}
	}
	return nil
}
