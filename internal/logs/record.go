package logs

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Bounds of the capture lifecycle. A generation is one frozen snapshot of
// bytes; a new generation exists only when the earlier snapshot's bytes were
// lost, so recapturing can never overwrite an immutable artifact reservation.
const (
	MaxGenerations = 3
	// MaxClaims bounds the collection attempts of one generation.
	MaxClaims = 5
)

// Record is the durable state of one log capture of one attempt and source.
// Identity (scope, attempt, run, step, source, generation) is immutable. State
// changes only through the transition methods, which enforce the table:
//
//	pending      --Begin-->   collecting
//	collecting   --Begin-->   collecting   (expired lease; fence increases)
//	collecting   --Freeze-->  collecting   (snapshot recorded once)
//	collecting   --Retain-->  retained     (verified linkage; immutable)
//	collecting   --Fail-->    unavailable
//	unavailable  --Begin-->   collecting   (retryable failure, bounded claims)
//	pending|unavailable --Observe--> retained   (wrapper source: verified link)
//	retained     --MarkRemoved--> retained (availability only)
//
// A retained record's artifacts and completeness evidence never change; a
// repeated identical linkage succeeds and a different one is ErrConflict.
type Record struct {
	Scope      domain.Scope
	ID         RecordID
	Attempt    domain.AttemptID
	Run        domain.RunID
	Step       string
	Source     Source
	Generation int

	State    State
	Claim    Claim
	Claims   int
	Failure  FailureClass
	Snapshot *Snapshot
	Link     *Link
	// RemovedAt is set when retention deleted the log content. It changes
	// availability only: Link and its completeness stay as recorded.
	RemovedAt *time.Time

	Version   domain.Version
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewRecord creates a pending record.
func NewRecord(scope domain.Scope, id RecordID, attempt domain.AttemptID, run domain.RunID, step string, src Source, generation int, now time.Time) (*Record, error) {
	switch {
	case scope.Validate() != nil:
		return nil, invalidf("scope is not valid")
	case id.Validate() != nil:
		return nil, invalidf("record id is not valid")
	case attempt.Validate() != nil:
		return nil, invalidf("attempt id is not valid")
	case run.Validate() != nil:
		return nil, invalidf("run id is not valid")
	case step == "" || len(step) > 128:
		return nil, invalidf("step must be 1..128 bytes")
	case !src.Valid():
		return nil, invalidf("unknown source")
	case generation < 1 || generation > MaxGenerations:
		return nil, invalidf("generation must be 1..%d", MaxGenerations)
	}
	now = now.UTC()
	return &Record{Scope: scope, ID: id, Attempt: attempt, Run: run, Step: step, Source: src, Generation: generation,
		State: StatePending, Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

func (r *Record) touch(now time.Time) {
	r.Version++
	r.UpdatedAt = now.UTC()
}

// held checks the fenced claim. A stale collector (older fence, other token, or
// a record that is no longer collecting) is ErrClaimLost.
func (r *Record) held(token string, fence int64) error {
	if r.State != StateCollecting || token == "" || r.Claim.Token != token || r.Claim.Fence != fence {
		return ErrClaimLost
	}
	return nil
}

// Begin takes the collection claim for a controller capture. It returns the
// claim the collector must present on every later transition.
func (r *Record) Begin(token string, lease time.Duration, now time.Time) (Claim, error) {
	if r.Source != SourceController {
		return Claim{}, transitionf("only a controller capture is collected")
	}
	if token == "" || lease <= 0 {
		return Claim{}, invalidf("a claim needs a token and a positive lease")
	}
	switch r.State {
	case StateRetained:
		return Claim{}, transitionf("the capture is already retained")
	case StateCollecting:
		if r.Claim.Active(now) {
			return Claim{}, ErrBusy
		}
	case StateUnavailable:
		if !r.Failure.Retryable() {
			return Claim{}, transitionf("the failure of this capture is not retryable")
		}
	}
	if r.Claims >= MaxClaims {
		return Claim{}, transitionf("collection attempts of this generation are exhausted")
	}
	r.State = StateCollecting
	r.Failure = ""
	r.Claims++
	r.Claim = Claim{Token: token, Fence: r.Claim.Fence + 1, Until: now.Add(lease).UTC()}
	r.touch(now)
	return r.Claim, nil
}

// Freeze records the snapshot of the bytes about to be uploaded. Repeating the
// identical snapshot is a no-op (changed reports false); a different snapshot
// is ErrConflict, because uploads and reconciliation depend on the frozen
// declaration.
func (r *Record) Freeze(c Claim, s Snapshot, now time.Time) (changed bool, err error) {
	if err := r.held(c.Token, c.Fence); err != nil {
		return false, err
	}
	if err := s.validate(r.Source); err != nil {
		return false, err
	}
	if r.Snapshot != nil {
		if r.Snapshot.equal(s) {
			return false, nil
		}
		return false, ErrConflict
	}
	snap := s
	snap.Reasons = append([]Reason(nil), s.Reasons...)
	r.Snapshot = &snap
	r.touch(now)
	return true, nil
}

// Retain commits the verified linkage of a controller capture under its claim.
// With a frozen snapshot the linkage must be that snapshot.
func (r *Record) Retain(c Claim, l Link, now time.Time) error {
	if err := r.held(c.Token, c.Fence); err != nil {
		return err
	}
	if err := l.validate(r.Source); err != nil {
		return err
	}
	if r.Snapshot == nil {
		return transitionf("a capture is frozen before it is linked")
	}
	if l.Log.Digest != r.Snapshot.Digest || l.Log.Size != r.Snapshot.Size || !l.Statement.equal(r.Snapshot.Statement) || l.Meta == nil {
		return ErrConflict
	}
	r.link(l, now)
	return nil
}

func (r *Record) link(l Link, now time.Time) {
	cp := l
	cp.Reasons = append([]Reason(nil), l.Reasons...)
	if l.Meta != nil {
		m := *l.Meta
		cp.Meta = &m
	}
	r.Link, r.State, r.Failure, r.Claim.Token, r.Claim.Until = &cp, StateRetained, "", "", time.Time{}
	r.touch(now)
}

// Observe records the verified linkage of a wrapper capture. It is idempotent:
// the identical linkage again succeeds without change (changed is false) and a
// different one is ErrConflict, so an upload racing a repeat can never replace
// earlier evidence or upgrade its completeness.
func (r *Record) Observe(l Link, now time.Time) (changed bool, err error) {
	if r.Source != SourceWrapper {
		return false, transitionf("only a wrapper capture is observed")
	}
	if err := l.validate(r.Source); err != nil {
		return false, err
	}
	switch r.State {
	case StateRetained:
		if r.Link != nil && r.Link.equal(l) {
			return false, nil
		}
		return false, ErrConflict
	case StatePending, StateUnavailable:
		r.link(l, now)
		return true, nil
	}
	return false, transitionf("a wrapper capture is %s", r.State)
}

// Fail ends a collection with a classified failure.
func (r *Record) Fail(c Claim, class FailureClass, now time.Time) error {
	if err := r.held(c.Token, c.Fence); err != nil {
		return err
	}
	if !class.Valid() {
		return invalidf("unknown failure class")
	}
	r.State, r.Failure, r.Claim.Token, r.Claim.Until = StateUnavailable, class, "", time.Time{}
	r.touch(now)
	return nil
}

// MarkRemoved records that retention removed the log content. It changes
// availability only; the linkage and its completeness are history.
func (r *Record) MarkRemoved(now time.Time) (changed bool, err error) {
	if r.State != StateRetained {
		return false, transitionf("only a retained capture can be removed")
	}
	if r.RemovedAt != nil {
		return false, nil
	}
	t := now.UTC()
	r.RemovedAt = &t
	r.touch(now)
	return true, nil
}

// Available reports whether the retained log content can still be read.
func (r *Record) Available() bool { return r.State == StateRetained && r.RemovedAt == nil }

// Next returns the record of the next generation of a capture whose frozen
// bytes were lost. The earlier record keeps its failure. At most MaxGenerations
// exist.
func (r *Record) Next(id RecordID, now time.Time) (*Record, error) {
	if r.State != StateUnavailable || r.Failure != FailureFrozenLost {
		return nil, transitionf("a new generation follows only lost frozen bytes")
	}
	if r.Generation >= MaxGenerations {
		return nil, ErrGenerationsExhausted
	}
	return NewRecord(r.Scope, id, r.Attempt, r.Run, r.Step, r.Source, r.Generation+1, now)
}

// Select picks the retained, available record to serve when the caller named no
// source: a verified complete wrapper capture first, then the latest wrapper
// capture, then the latest controller capture. Sources are never merged.
func Select(records []*Record) *Record {
	var best *Record
	rank := func(r *Record) int {
		switch {
		case r.Source == SourceWrapper && r.Link.Completeness == Complete:
			return 3
		case r.Source == SourceWrapper:
			return 2
		}
		return 1
	}
	for _, r := range records {
		if r == nil || !r.Available() || r.Link == nil {
			continue
		}
		if best == nil || rank(r) > rank(best) || (rank(r) == rank(best) && r.Generation > best.Generation) {
			best = r
		}
	}
	return best
}
