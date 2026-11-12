// Package receipt_test pins the receipt-matching surface these tests rely on:
// the pure matching decision an adapter uses to replay an equal retry or refuse
// changed content, on top of receipt identity, normalization and the durable
// Record. No effectful receipt port is declared, because a receipt is always
// committed inside the business commit unit it protects.
//
// API:
//
//	type receipt.Outcome string
//	const (
//	    receipt.OutcomeNew      receipt.Outcome = "new"
//	    receipt.OutcomeReplay   receipt.Outcome = "replay"
//	    receipt.OutcomeConflict receipt.Outcome = "conflict"
//	)
//	func (receipt.Outcome) Valid() bool
//	func receipt.Match(existing *receipt.Record, candidate receipt.Record) (receipt.Outcome, error)
//
// Match semantics:
//
//   - A nil existing record is OutcomeNew.
//   - A candidate whose Key equals the stored Key and whose ContentHash equals
//     the stored ContentHash is OutcomeReplay. The caller answers the retry
//     with the stored *existing Record: its original Result, ExecutionID and
//     SnapshotHash. It never answers with the candidate and never performs the
//     effect again.
//   - A candidate whose Key equals the stored Key but whose ContentHash differs
//     is OutcomeConflict together with a fault.ChangedIdempotencyContent error.
//   - A candidate whose Key differs in any one of tenant, project, principal,
//     operation or caller key never matches the stored record: OutcomeNew. A
//     different scope is a different receipt identity, not a conflict.
//   - Match never mutates, overwrites or re-stamps the stored record.
//   - A candidate or stored record whose Key is invalid or whose ContentHash is
//     empty is refused with fault.InvalidInput. An unreadable receipt is never
//     reported as a replay or as a fresh invocation.
//   - Outcome values outside the three constants are not Valid, so an unknown
//     outcome can never be treated as success.
package receipt_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/receipt"
)

// TestReceiptOutcomeVocabulary asserts the closed outcome vocabulary. An
// unknown outcome is never a successful or replayable result.
func TestReceiptOutcomeVocabulary(t *testing.T) {
	valid := []receipt.Outcome{
		receipt.OutcomeNew,
		receipt.OutcomeReplay,
		receipt.OutcomeConflict,
	}
	for _, outcome := range valid {
		if !outcome.Valid() {
			t.Errorf("outcome %q is not Valid", outcome)
		}
	}
	for _, outcome := range []receipt.Outcome{"", "unknown", "replayed", "OK"} {
		if outcome.Valid() {
			t.Errorf("outcome %q is Valid; only the declared outcomes may be", outcome)
		}
	}
}
