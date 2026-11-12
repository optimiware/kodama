package receipt

import (
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
)

// Outcome is the closed receipt-matching decision for one retry.
//
// It is deliberately not a boolean: an unknown outcome can never be treated as
// a successful replay, and a conflict is distinguishable from a fresh
// invocation.
type Outcome string

const (
	// OutcomeNew reports that no durable receipt exists for the candidate's
	// identity, so the caller may perform the effect.
	OutcomeNew Outcome = "new"
	// OutcomeReplay reports that the stored receipt has the same identity and
	// normalized content. The caller must answer with the stored record, never
	// with the candidate, and must not perform the effect again.
	OutcomeReplay Outcome = "replay"
	// OutcomeConflict reports that the identity was reused with different
	// normalized content. The caller refuses with changed_idempotency_content
	// and never replays the old result.
	OutcomeConflict Outcome = "conflict"
)

var outcomes = [...]Outcome{OutcomeNew, OutcomeReplay, OutcomeConflict}

// Valid reports whether the outcome is one of the declared outcomes. An unknown
// outcome is never a successful or replayable result.
func (outcome Outcome) Valid() bool {
	for _, candidate := range outcomes {
		if outcome == candidate {
			return true
		}
	}
	return false
}

// Match decides how one invocation candidate relates to an already stored
// durable receipt.
//
// A nil existing record is OutcomeNew. Equal identity (tenant, project,
// principal, operation, caller key) and equal content fingerprint is
// OutcomeReplay: the caller answers with *existing, so the original Result,
// ExecutionID and SnapshotHash survive a crash after commit. Equal identity
// with different content is OutcomeConflict and a
// fault.ChangedIdempotencyContent error. Any difference in the identity is a
// different receipt, not a conflict, and is OutcomeNew.
//
// Match never mutates, overwrites or re-stamps the stored record. A candidate
// or stored record with an invalid key or empty content fingerprint is refused
// with fault.InvalidInput: an unreadable receipt is never reported as a replay
// or as a fresh invocation.
func Match(existing *Record, candidate Record) (Outcome, error) {
	if err := candidate.Key.Validate(); err != nil {
		return "", err
	}
	if candidate.ContentHash == "" {
		return "", fault.New(fault.InvalidInput, "candidate receipt has no content fingerprint")
	}
	if existing == nil {
		return OutcomeNew, nil
	}
	if err := existing.Key.Validate(); err != nil {
		return "", err
	}
	if existing.ContentHash == "" {
		return "", fault.New(fault.InvalidInput, "stored receipt has no content fingerprint")
	}
	if candidate.Key != existing.Key {
		return OutcomeNew, nil
	}
	if candidate.ContentHash == existing.ContentHash {
		return OutcomeReplay, nil
	}
	return OutcomeConflict, fault.New(fault.ChangedIdempotencyContent, fmt.Sprintf(
		"caller key %q was reused with changed content", candidate.Key.CallerKey))
}
