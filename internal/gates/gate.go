package gates

import (
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// EvidenceRole says why an artifact is bound into a gate subject.
type EvidenceRole string

const (
	// RoleEvidence: named by the gate step's evidence references.
	RoleEvidence EvidenceRole = "evidence"
	// RoleReport: the candidate's design conformance report.
	RoleReport EvidenceRole = "report"
	// RoleSupport: an artifact the conformance report cites for a check.
	RoleSupport EvidenceRole = "support"
)

// RequiredEvidence is one entry of the required-evidence snapshot: an artifact
// the subject binds, why, and where it came from.
type RequiredEvidence struct {
	Role       EvidenceRole      `json:"role"`
	ID         domain.ArtifactID `json:"id"`
	Digest     string            `json:"digest"`
	Expression string            `json:"expression,omitempty"`
	Producer   string            `json:"producer,omitempty"`
}

// Conformance statuses of the snapshot.
const (
	ConformanceVerified = "verified"
	ConformanceBlocked  = "blocked"
)

// ConformanceSnapshot records, at request time, the outcome of validating the
// candidate's design conformance report against the trusted expected baseline
// and required checks. A blocked report is preserved for review: approval and
// progression are refused, rejection stays possible. The report, the baseline
// and the candidate are all immutable, so the outcome cannot change; a new
// candidate or baseline needs a new run and fresh evidence.
type ConformanceSnapshot struct {
	Status         string            `json:"status"`
	Code           string            `json:"code,omitempty"`
	ProducerStep   string            `json:"producerStep"`
	BaselineDigest string            `json:"baselineDigest,omitempty"`
	RequiredChecks []string          `json:"requiredChecks"`
	ReportID       domain.ArtifactID `json:"reportId,omitempty"`
	ReportDigest   string            `json:"reportDigest,omitempty"`
}

// Required is the immutable required-evidence snapshot of a gate.
type Required struct {
	Evidence    []RequiredEvidence   `json:"evidence"`
	Conformance *ConformanceSnapshot `json:"conformance"`
}

// ConformanceDenial returns the denial of approval caused by the conformance
// snapshot, or nil when approval is not blocked by it.
func (r Required) ConformanceDenial() error {
	if r.Conformance == nil || r.Conformance.Status == ConformanceVerified {
		return nil
	}
	code := r.Conformance.Code
	if code == "" {
		code = CodeConformanceMissing
	}
	return &ConformanceError{Code: code}
}

// DecisionRecord is the immutable human decision.
type DecisionRecord struct {
	By       Actor
	Decision Decision
	Reason   string
	At       time.Time
}

// Invalidation records that the subject stopped being applicable. It is
// separate from the decision, which is never rewritten.
type Invalidation struct {
	At                  time.Time
	Reason              string
	SupersedingCommit   string
	SupersedingRevision int64
}

// Gate is one recorded request for one authorized human decision about an
// identified subject. Its subject, evidence snapshot, ownership and deadline
// never change; its status moves pending to approved or rejected exactly once.
type Gate struct {
	ID    ID
	Scope domain.Scope
	RunID domain.RunID
	// StepID is the gate step of the run this gate pauses.
	StepID   string
	Subject  Subject
	Required Required

	Status       Status
	Version      domain.Version
	RequestedAt  time.Time
	Deadline     time.Time
	Decision     *DecisionRecord
	Invalidation *Invalidation

	CreatedAt time.Time
	UpdatedAt time.Time

	dirty bool
}

// RequestInput creates a gate.
type RequestInput struct {
	ID       ID
	Scope    domain.Scope
	RunID    domain.RunID
	StepID   string
	Subject  Subject
	Required Required
	Now      time.Time
	Deadline time.Time
}

// Request builds a pending gate. The deadline is snapshotted once.
func Request(in RequestInput) (*Gate, error) {
	if err := in.ID.Validate(); err != nil {
		return nil, err
	}
	if err := in.Scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := in.RunID.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if in.StepID == "" || len(in.StepID) > 63 {
		return nil, invalidf("a gate step is required")
	}
	if in.Subject.IsZero() || in.Subject.Scope() != in.Scope {
		return nil, invalidf("the subject must belong to the gate's scope")
	}
	if !in.Deadline.After(in.Now) {
		return nil, invalidf("the gate deadline must be in the future")
	}
	if err := in.Required.validate(in.Subject); err != nil {
		return nil, err
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	return &Gate{
		ID: in.ID, Scope: in.Scope, RunID: in.RunID, StepID: in.StepID, Subject: in.Subject, Required: in.Required,
		Status: StatusPending, Version: 1, RequestedAt: now, Deadline: in.Deadline.UTC().Truncate(time.Microsecond),
		CreatedAt: now, UpdatedAt: now, dirty: true,
	}, nil
}

// validate checks the snapshot covers exactly the subject's evidence.
func (r Required) validate(s Subject) error {
	want := s.Evidence()
	have := map[domain.ArtifactID]string{}
	type refKey struct {
		id   domain.ArtifactID
		role EvidenceRole
	}
	roles := map[refKey]bool{}
	for _, e := range r.Evidence {
		if e.Role != RoleEvidence && e.Role != RoleReport && e.Role != RoleSupport {
			return invalidf("unknown evidence role")
		}
		if roles[refKey{e.ID, e.Role}] {
			return invalidf("evidence %s is listed twice for one role", e.ID)
		}
		roles[refKey{e.ID, e.Role}] = true
		if prev, ok := have[e.ID]; ok && prev != e.Digest {
			return invalidf("evidence %s has two digests", e.ID)
		}
		have[e.ID] = e.Digest
	}
	if len(have) != len(want) {
		return invalidf("the required evidence does not match the subject")
	}
	for _, w := range want {
		if have[w.ID] != w.Digest {
			return invalidf("the required evidence does not match the subject")
		}
	}
	if c := r.Conformance; c != nil {
		if err := c.validate(r.Evidence); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the snapshot's own invariants against the evidence snapshot.
// A snapshot that is usable as approval evidence (verified) must identify the
// report it bound and the expected baseline and checks it was verified against;
// a malformed snapshot is refused at request and at restore, never read as an
// approval. A blocked snapshot needs its denial code and, when it names a
// report, that report must be bound.
func (c *ConformanceSnapshot) validate(evidence []RequiredEvidence) error {
	switch c.Status {
	case ConformanceVerified, ConformanceBlocked:
	default:
		return invalidf("unknown conformance status")
	}
	if c.Status == ConformanceBlocked && validCode("conformance code", c.Code) != nil {
		return invalidf("a blocked conformance snapshot needs a denial code")
	}
	if c.Status == ConformanceVerified && c.Code != "" {
		return invalidf("a verified conformance snapshot has no denial code")
	}
	if c.ProducerStep == "" || len(c.ProducerStep) > 63 {
		return invalidf("a conformance snapshot identifies the producing step")
	}
	if c.BaselineDigest != "" {
		if err := runtimev1.Digest(c.BaselineDigest).Validate(); err != nil {
			return invalidf("the conformance baseline digest is malformed")
		}
	}
	if (c.ReportID == "") != (c.ReportDigest == "") {
		return invalidf("the conformance report is identified by both id and digest")
	}
	if c.ReportID != "" {
		if err := c.ReportID.Validate(); err != nil {
			return invalidf("%v", err)
		}
		if err := validDigest("conformance report digest", c.ReportDigest); err != nil {
			return err
		}
		bound := false
		for _, e := range evidence {
			if e.Role == RoleReport && e.ID == c.ReportID && e.Digest == c.ReportDigest {
				bound = true
			}
		}
		if !bound {
			return invalidf("the conformance report is not bound in the required evidence")
		}
	}
	for i, id := range c.RequiredChecks {
		if id == "" || (i > 0 && c.RequiredChecks[i-1] >= id) {
			return invalidf("the required checks are strictly sorted without duplicates")
		}
	}
	if c.Status == ConformanceVerified {
		if c.BaselineDigest == "" || c.ReportID == "" || c.RequiredChecks == nil {
			return invalidf("a verified conformance snapshot identifies its baseline, required checks and report")
		}
	}
	return nil
}

// Record is the persisted form of a Gate.
type Record struct {
	ID           ID
	Scope        domain.Scope
	RunID        domain.RunID
	StepID       string
	Subject      []byte
	Required     Required
	Status       Status
	Version      domain.Version
	RequestedAt  time.Time
	Deadline     time.Time
	Decision     *DecisionRecord
	Invalidation *Invalidation
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Restore rebuilds a gate from storage and rejects rows that violate its
// invariants, so a corrupted row is never treated as an approval.
func Restore(rec Record) (*Gate, error) {
	s, err := ParseSubject(rec.Subject)
	if err != nil {
		return nil, err
	}
	if s.Scope() != rec.Scope {
		return nil, invalidf("stored subject belongs to another scope")
	}
	if err := rec.Required.validate(s); err != nil {
		return nil, err
	}
	switch rec.Status {
	case StatusPending:
		if rec.Decision != nil {
			return nil, invalidf("a pending gate has no decision")
		}
	case StatusApproved, StatusRejected:
		if rec.Decision == nil || rec.Decision.Decision.Resulting() != rec.Status {
			return nil, invalidf("a decided gate has its matching decision")
		}
	default:
		return nil, invalidf("unknown gate status")
	}
	if err := rec.Version.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	return &Gate{
		ID: rec.ID, Scope: rec.Scope, RunID: rec.RunID, StepID: rec.StepID, Subject: s, Required: rec.Required,
		Status: rec.Status, Version: rec.Version, RequestedAt: rec.RequestedAt, Deadline: rec.Deadline,
		Decision: rec.Decision, Invalidation: rec.Invalidation, CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}, nil
}

// Dirty / MarkClean support persistence adapters.
func (g *Gate) Dirty() bool { return g.dirty }
func (g *Gate) MarkClean()  { g.dirty = false }

func (g *Gate) touch(now time.Time) {
	if !g.dirty {
		g.Version++
		g.dirty = true
	}
	g.UpdatedAt = now.UTC().Truncate(time.Microsecond)
}

// Expired reports whether the deadline passed. Expiry is derived from the
// snapshotted deadline; it is not a status and is never written.
func (g *Gate) Expired(now time.Time) bool { return !now.Before(g.Deadline) }

// SubjectDigest is the digest a decision is bound to.
func (g *Gate) SubjectDigest() string { return g.Subject.Digest() }

// DecideCommand is one reviewer decision. The actor is derived by the server
// from the verified principal.
type DecideCommand struct {
	Decision        Decision
	Reason          string
	Actor           Actor
	ExpectedVersion domain.Version
	SubjectDigest   string
}

// Validate checks the command shape.
func (c DecideCommand) Validate() error {
	if !c.Decision.Valid() {
		return invalidf("decision must be approve or reject")
	}
	if r := strings.TrimSpace(c.Reason); r == "" || len(c.Reason) > 2048 {
		return invalidf("a decision needs a reason of at most 2048 bytes")
	}
	if err := validDigest("subject digest", c.SubjectDigest); err != nil {
		return err
	}
	if err := c.ExpectedVersion.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.Actor.Validate(); err != nil {
		return err
	}
	return nil
}

// Decide records the one immutable decision. Order of refusals: only a human
// decides; the expected version, then the state, then the subject digest, any
// invalidation and the deadline, then the current facts; approval additionally
// needs applicable conformance evidence. A refusal changes nothing.
func (g *Gate) Decide(c DecideCommand, f Facts, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Actor.Type != ActorUser {
		return ErrDenied
	}
	if c.ExpectedVersion != g.Version {
		return ErrVersionConflict
	}
	if g.Status != StatusPending {
		return transitionf("the gate is already %s", g.Status)
	}
	if c.SubjectDigest != g.Subject.Digest() {
		return stale("subject_digest_mismatch")
	}
	if g.Invalidation != nil {
		return stale(ReasonInvalidated)
	}
	if g.Expired(now) {
		return stale(ReasonExpired)
	}
	if err := g.CheckCurrent(f); err != nil {
		return err
	}
	if c.Decision == DecisionApprove {
		if err := g.Required.ConformanceDenial(); err != nil {
			return err
		}
	}
	g.Status = c.Decision.Resulting()
	g.Decision = &DecisionRecord{By: c.Actor, Decision: c.Decision, Reason: strings.TrimSpace(c.Reason), At: now.UTC().Truncate(time.Microsecond)}
	g.touch(now)
	return nil
}

// Invalidate records that the subject stopped being applicable. It is
// idempotent (the first invalidation wins) and never rewrites a decision.
func (g *Gate) Invalidate(reason, supersedingCommit string, supersedingRevision int64, now time.Time) bool {
	if g.Invalidation != nil {
		return false
	}
	g.Invalidation = &Invalidation{At: now.UTC().Truncate(time.Microsecond), Reason: reason, SupersedingCommit: supersedingCommit, SupersedingRevision: supersedingRevision}
	g.touch(now)
	return true
}

// Applicable reports why an approval cannot be used right now, or nil. It looks
// only at the gate's own record; current facts are checked by Revalidate.
func (g *Gate) Applicable(now time.Time) error {
	switch g.Status {
	case StatusPending:
		return stale(ReasonNotApproved)
	case StatusRejected:
		return stale(ReasonRejected)
	}
	if g.Invalidation != nil {
		return stale(ReasonInvalidated)
	}
	if g.Expired(now) {
		return stale(ReasonExpired)
	}
	return g.Required.ConformanceDenial()
}
