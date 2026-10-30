package gitpush

import (
	"fmt"
	"slices"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Entry is one ref update of a push record with its own state. Kind is set at
// authorization from the worker's git facts and never changes. Reason is empty
// unless the entry is refused or unknown.
type Entry struct {
	Ref    string
	Old    string
	New    string
	Kind   gitv1.UpdateKind
	State  gitv1.EntryState
	Reason string
}

func (e Entry) update() gitv1.RefUpdate { return gitv1.RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New} }

func (e Entry) dto() gitv1.PushEntry {
	out := gitv1.PushEntry{Ref: e.Ref, Old: e.Old, New: e.New, Kind: e.Kind, State: e.State}
	if e.Reason != "" {
		r := e.Reason
		out.Reason = &r
	}
	return out
}

// Record is the push record aggregate: the durable intent and outcome of one
// receive operation, keyed by its ReceiveID within the repository. Entries
// change state only through the methods below, which keep the transition table
// of the specification ("Push journal and events"):
//
//	authorized -> committing | refused | unknown
//	committing -> applied | refused | unknown
//	applied, refused, unknown: no further transition here (unknown waits for an
//	operator).
//
// The record's status is derived from its entries (gitv1.DeriveStatus), never
// stored independently.
type Record struct {
	ID           string
	Scope        domain.Scope
	RepositoryID string
	ReceiveID    string
	GrantID      string
	GrantKind    gitv1.GrantKind
	Actor        gitv1.Actor
	PushOptions  []string
	// RepositoryEmpty is the worker's fact that the repository had no refs when the
	// receive began. EmptyReportPending is set when an applied entry made an empty
	// repository non-empty and the repository module has not been told yet.
	RepositoryEmpty    bool
	EmptyReportPending bool
	// RequestDigest identifies the authorization request: a repeated receive with
	// the identical request is a replay, any other is idempotency_conflict.
	RequestDigest string
	// Deadline is the receive deadline the worker enforces.
	Deadline  time.Time
	Entries   []Entry
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clone returns a deep copy.
func (r *Record) Clone() *Record {
	c := *r
	c.Entries = slices.Clone(r.Entries)
	c.PushOptions = slices.Clone(r.PushOptions)
	c.Actor.RunID = clonePtr(r.Actor.RunID)
	c.Actor.AttemptID = clonePtr(r.Actor.AttemptID)
	return &c
}

func clonePtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func (r *Record) dtoEntries() []gitv1.PushEntry {
	out := make([]gitv1.PushEntry, len(r.Entries))
	for i, e := range r.Entries {
		out[i] = e.dto()
	}
	return out
}

// Status is the derived record status.
func (r *Record) Status() gitv1.PushStatus { return gitv1.DeriveStatus(r.dtoEntries()) }

// Unsettled reports whether the record blocks a new seed: its status is
// authorized or unknown (gitv1.UnsettledPush).
func (r *Record) Unsettled() bool {
	st := r.Status()
	return st == gitv1.PushAuthorized || st == gitv1.PushUnknown
}

// OriginRunID is the run that made the push (the actor's run), nil for a person.
func (r *Record) OriginRunID() *string {
	if r.Actor.Type != gitv1.ActorWorkload {
		return nil
	}
	return clonePtr(r.Actor.RunID)
}

// DTO is the wire representation (the response of GET .../pushes/{pushId}).
func (r *Record) DTO() (gitv1.PushRecord, error) {
	out := gitv1.PushRecord{
		PushID: r.ID, RepositoryID: r.RepositoryID, ReceiveID: r.ReceiveID, Actor: r.Actor, GrantID: r.GrantID,
		OriginRunID: r.OriginRunID(), Status: r.Status(), Entries: r.dtoEntries(), CreatedAt: timestamp(r.CreatedAt),
	}
	if err := out.Validate(); err != nil {
		return gitv1.PushRecord{}, fmt.Errorf("gitpush: record %s is not a valid push record: %w", r.ID, err)
	}
	return out, nil
}

func timestamp(t time.Time) gitv1.Timestamp {
	return gitv1.Timestamp(t.UTC().Format("2006-01-02T15:04:05.999999999Z"))
}

// Verdicts are the per-update verdicts of the authorization: an entry refused
// with a push refusal reason was denied, every other entry was allowed (it may
// later be refused by settlement, which never uses a push refusal reason).
func (r *Record) Verdicts() []gitv1.UpdateVerdict {
	out := make([]gitv1.UpdateVerdict, len(r.Entries))
	for i, e := range r.Entries {
		out[i] = gitv1.UpdateVerdict{Ref: e.Ref, Verdict: gitv1.VerdictAllow}
		if e.State == gitv1.EntryRefused && gitv1.IsPushRefusalReason(e.Reason) {
			reason := e.Reason
			out[i].Verdict, out[i].Reason = gitv1.VerdictDeny, &reason
		}
	}
	return out
}

// AuthorizeResponse is the answer of the authorization that created the record,
// reproduced exactly on a replay.
func (r *Record) AuthorizeResponse() gitv1.PushAuthorizeResponse {
	return gitv1.PushAuthorizeResponse{PushID: r.ID, Deadline: timestamp(r.Deadline), Updates: r.Verdicts()}
}

func (r *Record) find(ref string) int {
	for i, e := range r.Entries {
		if e.Ref == ref {
			return i
		}
	}
	return -1
}

// match returns the index of the entry the update names; an update that is not
// an entry of the record (unknown ref or another old/new value) is forbidden:
// the record never authorized it.
func (r *Record) match(u gitv1.RefUpdate) (int, error) {
	i := r.find(u.Ref)
	if i < 0 || r.Entries[i].Old != u.Old || r.Entries[i].New != u.New {
		return -1, refuse(domain.CodeForbidden, "the update of %s is not part of this push record", truncate(u.Ref, 64))
	}
	return i, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// transitions is the entry transition table.
var transitions = map[gitv1.EntryState][]gitv1.EntryState{
	gitv1.EntryAuthorized: {gitv1.EntryCommitting, gitv1.EntryRefused, gitv1.EntryUnknown},
	gitv1.EntryCommitting: {gitv1.EntryApplied, gitv1.EntryRefused, gitv1.EntryUnknown},
}

// CanTransition reports whether an entry may move from one state to another.
func CanTransition(from, to gitv1.EntryState) bool { return slices.Contains(transitions[from], to) }

func (r *Record) set(i int, to gitv1.EntryState, reason string) error {
	e := &r.Entries[i]
	if !CanTransition(e.State, to) {
		return fmt.Errorf("gitpush: entry %s cannot move from %s to %s", e.Ref, e.State, to)
	}
	e.State, e.Reason = to, reason
	return nil
}

func (r *Record) touch(now time.Time) {
	r.Version++
	r.UpdatedAt = now
}

// CommitIntent moves the entries named by the updates from authorized to
// committing (the prepared hook with the refs locked). It refuses with
// push_settled when any named entry is not in flight (the record, or that
// entry, was settled) and receive_expired once the deadline passed; entries
// already committing stay so (a repeated request). An update the record never
// authorized is forbidden. It reports whether anything changed.
func (r *Record) CommitIntent(updates []gitv1.RefUpdate, now time.Time) (bool, error) {
	idx := make([]int, len(updates))
	for k, u := range updates {
		i, err := r.match(u)
		if err != nil {
			return false, err
		}
		idx[k] = i
	}
	for _, i := range idx {
		if !r.Entries[i].State.InFlight() {
			return false, refuse(gitv1.CodePushSettled, "the push record of this receive was already settled")
		}
	}
	if !now.Before(r.Deadline) {
		return false, refuse(gitv1.CodeReceiveExpired, "the receive deadline passed")
	}
	changed := false
	for _, i := range idx {
		if r.Entries[i].State == gitv1.EntryAuthorized {
			if err := r.set(i, gitv1.EntryCommitting, ""); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if changed {
		r.touch(now)
	}
	return changed, nil
}

// OutcomeResult describes what an outcome changed.
type OutcomeResult struct {
	Changed bool
	// Contradicted lists reported-applied refs whose entry was already refused:
	// the evidence of the worker contradicts a settled entry, which stays refused
	// and is audited (the record's version advances to carry the audit row).
	Contradicted []string
}

// ApplyOutcome records the post-receive report: the reported updates that
// reached committing become applied; an entry that never reached committing
// becomes refused (no_commit_intent), unless the worker reports it applied,
// which contradicts the fence and leaves the entry unknown (conflicting_evidence);
// committing entries that were not reported did not change their ref and are
// refused (receive_aborted). Final entries never change. An update the record never authorized
// is forbidden and nothing changes.
func (r *Record) ApplyOutcome(applied []gitv1.RefUpdate, now time.Time) (OutcomeResult, error) {
	reported := map[string]bool{}
	for _, u := range applied {
		i, err := r.match(u)
		if err != nil {
			return OutcomeResult{}, err
		}
		reported[r.Entries[i].Ref] = true
	}
	var res OutcomeResult
	for i, e := range r.Entries {
		in := reported[e.Ref]
		var err error
		switch {
		case e.State == gitv1.EntryCommitting && in:
			err = r.set(i, gitv1.EntryApplied, "")
		case e.State == gitv1.EntryCommitting:
			// post-receive lists exactly the refs Git updated, once, after the whole
			// receive ended: a transaction that obtained commit intent and is not
			// listed did not change its ref. (The journal's committed entry cannot
			// say otherwise: Git runs the committed hook for every update of a
			// batched transaction, including one it rejected.)
			err = r.set(i, gitv1.EntryRefused, gitv1.ReasonReceiveAborted)
		case e.State == gitv1.EntryAuthorized && in:
			err = r.set(i, gitv1.EntryUnknown, gitv1.ReasonConflictingEvidence)
		case e.State == gitv1.EntryAuthorized:
			err = r.set(i, gitv1.EntryRefused, gitv1.ReasonNoCommitIntent)
		case e.State == gitv1.EntryRefused && in:
			res.Contradicted = append(res.Contradicted, e.Ref)
			continue
		default:
			continue
		}
		if err != nil {
			return OutcomeResult{}, err
		}
		res.Changed = true
	}
	if len(res.Contradicted) > 0 {
		// The contradiction is an anomaly worth recording: the version advances so
		// the audit row of the contradiction is written with the record.
		res.Changed = true
	}
	if res.Changed {
		r.markEmptyReport()
		r.touch(now)
	}
	return res, nil
}

// RefuseLostWriter settles a record whose writer reservation was lost after it
// was authorized: every entry still authorized is refused, the entries of the
// named refs with ref_protected (the denial the worker is told) and the others
// with no_commit_intent (the receive fails as a whole). Entries that already
// moved on are left. It reports whether anything changed; the version advances
// once.
func (r *Record) RefuseLostWriter(refs map[string]bool, now time.Time) (bool, error) {
	changed := false
	for i, e := range r.Entries {
		if e.State != gitv1.EntryAuthorized {
			continue
		}
		reason := gitv1.ReasonNoCommitIntent
		if refs[e.Ref] {
			reason = gitv1.ReasonRefProtected
		}
		if err := r.set(i, gitv1.EntryRefused, reason); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		r.touch(now)
	}
	return changed, nil
}

// Resolve applies a settlement decision to one entry, only while the entry is
// still in the state the evidence was gathered for. wait changes nothing. It
// reports whether the entry changed.
func (r *Record) Resolve(ref string, from gitv1.EntryState, d gitv1.Decision, now time.Time) (bool, error) {
	i := r.find(ref)
	if i < 0 {
		return false, fmt.Errorf("gitpush: no entry for %s", ref)
	}
	if r.Entries[i].State != from {
		return false, nil // settled by someone else since the evidence was read
	}
	var err error
	switch d.Resolution {
	case gitv1.ResolveApplied:
		err = r.set(i, gitv1.EntryApplied, "")
	case gitv1.ResolveRefused:
		err = r.set(i, gitv1.EntryRefused, d.Reason)
	case gitv1.ResolveUnknown:
		if from == gitv1.EntryUnknown {
			return false, nil
		}
		err = r.set(i, gitv1.EntryUnknown, d.Reason)
	default:
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// OperatorResolve settles one unknown entry as the operator decided: applied
// (no reason) or refused (ref_unchanged: the ref did not take the new value).
// It is the only transition out of unknown and the caller must carry the
// Resolution with the saved record (the database refuses it otherwise). The
// version advances once; Seal records the empty-report obligation.
func (r *Record) OperatorResolve(ref string, to gitv1.EntryState, now time.Time) error {
	i := r.find(ref)
	if i < 0 {
		return fmt.Errorf("gitpush: no entry for %s", ref)
	}
	e := &r.Entries[i]
	if e.State != gitv1.EntryUnknown {
		return refuse(gitv1.CodePushSettled, "the entry of %s is not unknown", truncate(ref, 64))
	}
	switch to {
	case gitv1.EntryApplied:
		e.State, e.Reason = to, ""
	case gitv1.EntryRefused:
		e.State, e.Reason = to, gitv1.ReasonRefUnchanged
	default:
		return fmt.Errorf("gitpush: an unknown entry resolves to applied or refused, not %s", to)
	}
	r.Seal(now)
	return nil
}

// markEmptyReport sets the empty-report flag when a settlement applied an entry of a
// receive that started on an empty repository.
func (r *Record) markEmptyReport() {
	if !r.RepositoryEmpty || r.EmptyReportPending {
		return
	}
	for _, e := range r.Entries {
		if e.State == gitv1.EntryApplied {
			r.EmptyReportPending = true
			return
		}
	}
}

// Seal finishes a settlement: it bumps the version once and records the empty
// report obligation. Callers invoke it after one or more Resolve calls changed
// the record.
func (r *Record) Seal(now time.Time) {
	r.markEmptyReport()
	r.touch(now)
}
