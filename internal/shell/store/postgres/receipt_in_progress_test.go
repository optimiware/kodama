// In-progress receipt behavior.
//
// An incomplete receipt MAY persist an in-progress result (the trigger Select's
// exact selection) while its completed_at marker stays NULL. An equal-key
// replay returns that stored result and applies nothing; a completion
// compare-and-set overwrites it with the final result and sets the marker
// exactly once. These tests script the fake database seam directly, so they
// prove the statement arguments and the no-reapply paths the shared vectors
// also exercise over the emulator.
package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
)

func testInProgressReceipt(t *testing.T) receipt.Record {
	t.Helper()
	record := testReceipt(t)
	record.Result = []byte("selection-v1")
	return record
}

func receiptInsertCall(t *testing.T, calls []recordedCall) recordedCall {
	t.Helper()
	index := indexOfTable(calls, "receipts", "insert")
	if index < 0 {
		t.Fatal("the unit did not insert a receipt")
	}
	return calls[index]
}

func receiptUpdateCall(t *testing.T, calls []recordedCall) recordedCall {
	t.Helper()
	index := indexOfTable(calls, "receipts", "update")
	if index < 0 {
		t.Fatal("the unit did not complete a receipt")
	}
	return calls[index]
}

// TestIncompleteInsertPersistsInProgressResult proves the first rule: an
// incomplete insert stores the result the trigger Select committed, while the
// completion marker stays NULL.
func TestIncompleteInsertPersistsInProgressResult(t *testing.T) {
	db := newFakeDB()
	record := testInProgressReceipt(t)
	unit := store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete}

	result, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	if err != nil {
		t.Fatalf("incomplete insert with an in-progress result: %v", err)
	}
	if result.Replayed {
		t.Fatal("a fresh incomplete insert was reported as a replay")
	}
	if string(result.Receipt.Result) != "selection-v1" {
		t.Errorf("committed result = %q; want the in-progress result", result.Receipt.Result)
	}
	call := receiptInsertCall(t, db.conn(t).callsCopy())
	if got, _ := call.args[7].([]byte); string(got) != "selection-v1" {
		t.Errorf("inserted result = %v; want selection-v1", call.args[7])
	}
	if call.args[8] != nil {
		t.Errorf("incomplete insert completed_at = %v; want NULL", call.args[8])
	}
}

// TestIncompleteReplayReturnsInProgressResultWithoutApplying proves the second
// rule: an equal-key replay of an incomplete receipt returns its stored
// in-progress result, does not complete it and does not re-apply the unit.
func TestIncompleteReplayReturnsInProgressResultWithoutApplying(t *testing.T) {
	db := newFakeDB()
	stored := testInProgressReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))

	record := testInProgressReceipt(t)
	unit := store.Unit{
		Receipt:     &record,
		ReceiptMode: store.ReceiptInsertIncomplete,
		Writes:      []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
	}
	result, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	if err != nil {
		t.Fatalf("equal incomplete replay: %v", err)
	}
	if !result.Replayed {
		t.Fatal("an equal incomplete receipt did not replay")
	}
	if string(result.Receipt.Result) != "selection-v1" {
		t.Errorf("replayed result = %q; want the stored in-progress result", result.Receipt.Result)
	}
	conn := db.conn(t)
	calls := conn.callsCopy()
	if indexOfTable(calls, "executions", "insert") >= 0 {
		t.Error("an incomplete replay re-applied the business write")
	}
	if indexOfTable(calls, "receipts", "insert") >= 0 {
		t.Error("an incomplete replay inserted a second receipt")
	}
	if indexOfTable(calls, "receipts", "update") >= 0 {
		t.Error("an incomplete replay completed the receipt")
	}
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
}

// TestCompletionOverwritesInProgressResultOnce proves the completion CAS
// overwrites the in-progress result and sets completed_at in the same guarded
// statement.
func TestCompletionOverwritesInProgressResultOnce(t *testing.T) {
	db := newFakeDB()
	stored := testInProgressReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))

	record := testReceipt(t)
	record.Result = []byte("started")
	expect := store.ReceiptExpect{Completed: false, ContentHash: record.ContentHash}
	unit := store.Unit{
		Receipt:       &record,
		ReceiptMode:   store.ReceiptComplete,
		ReceiptExpect: &expect,
		Writes:        []store.Write{{Table: "executions", ID: testExecutionID, Insert: true, Values: unitValues("executions")}},
	}
	result, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	if err != nil {
		t.Fatalf("completion over an in-progress receipt: %v", err)
	}
	if result.Replayed {
		t.Fatal("the first completion was reported as a replay")
	}
	if string(result.Receipt.Result) != "started" {
		t.Errorf("completed result = %q; want the final result", result.Receipt.Result)
	}
	conn := db.conn(t)
	calls := conn.callsCopy()
	update := receiptUpdateCall(t, calls)
	if !strings.Contains(strings.ToLower(update.sql), "completed_at is null") {
		t.Errorf("completion SQL does not guard on completed_at IS NULL: %s", update.sql)
	}
	if !strings.Contains(strings.ToLower(update.sql), "content_sha256") {
		t.Errorf("completion SQL does not guard on the content hash: %s", update.sql)
	}
	if got, _ := update.args[0].([]byte); string(got) != "started" {
		t.Errorf("completion result argument = %v; want started", update.args[0])
	}
	if update.args[1] != record.At {
		t.Errorf("completion completed_at = %v; want %v", update.args[1], record.At)
	}
	if indexOfTable(calls, "receipts", "insert") >= 0 {
		t.Error("a completion inserted a second receipt")
	}
	if indexOfTable(calls, "executions", "insert") < 0 {
		t.Error("a completion did not apply its business write")
	}
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
}

// TestChangedContentOnInProgressReceiptIsRefused proves changed content is
// refused in either receipt state without touching the stored result.
func TestChangedContentOnInProgressReceiptIsRefused(t *testing.T) {
	db := newFakeDB()
	stored := testInProgressReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))

	record := testReceipt(t)
	record.ContentHash = testFingerprint(t, []byte(`{"a":2}`))
	record.Result = []byte("selection-v2")
	unit := store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete}
	_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)

	conn := db.conn(t)
	calls := conn.callsCopy()
	if indexOfTable(calls, "receipts", "insert") >= 0 || indexOfTable(calls, "receipts", "update") >= 0 {
		t.Error("changed incomplete content wrote to the receipt")
	}
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
}

// TestChangedCompletionContentDoesNotOverwriteInProgress proves a completion
// whose content hash differs from both the stored and expected hash is refused
// before the CAS runs.
func TestChangedCompletionContentDoesNotOverwriteInProgress(t *testing.T) {
	db := newFakeDB()
	stored := testInProgressReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))

	record := testReceipt(t)
	record.ContentHash = testFingerprint(t, []byte(`{"a":2}`))
	record.Result = []byte("started")
	expect := store.ReceiptExpect{Completed: false, ContentHash: stored.ContentHash}
	unit := store.Unit{Receipt: &record, ReceiptMode: store.ReceiptComplete, ReceiptExpect: &expect}
	_, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	assertFaultClass(t, err, fault.ChangedIdempotencyContent)

	conn := db.conn(t)
	if indexOfTable(conn.callsCopy(), "receipts", "update") >= 0 {
		t.Error("changed completion content ran the compare-and-set")
	}
	if conn.commits != 0 || conn.rollbacks != 1 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one rollback", conn.commits, conn.rollbacks)
	}
}

// TestExecuteUnitEnforcesUnitValidate proves the adapter applies the
// store.Unit.Validate rule before opening a transaction: a completed insert
// without a result is refused, while an incomplete insert with an in-progress
// result is accepted.
func TestExecuteUnitEnforcesUnitValidate(t *testing.T) {
	db := newFakeDB()
	record := testReceipt(t)
	record.Result = nil
	unit := store.Unit{Receipt: &record}
	if _, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit); err == nil {
		t.Fatal("a completed insert without a result was accepted")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if db.begins != 0 {
		t.Fatalf("an invalid unit opened %d transactions; want none", db.begins)
	}

	db = newFakeDB()
	record = testInProgressReceipt(t)
	unit = store.Unit{Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete}
	if _, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit); err != nil {
		t.Fatalf("an incomplete insert carrying an in-progress result was refused: %v", err)
	}
}

// TestCompletionPersistsTheSnapshotDigest proves a completion writes the
// snapshot digest the completed unit observed, exactly as an insert does. A
// completion statement that updated only the result, the marker and the
// execution identity would silently lose the snapshot digest the memory
// adapter keeps.
func TestCompletionPersistsTheSnapshotDigest(t *testing.T) {
	db := newFakeDB()
	stored := testInProgressReceipt(t)
	db.script.setQuery(queryByTable(map[string]map[string]any{"receipts": receiptFixture(t, stored)}))

	record := testReceipt(t)
	record.Result = []byte("started")
	digest := [32]byte{1, 2, 3, 4}
	record.SnapshotHash = &digest
	expect := store.ReceiptExpect{Completed: false, ContentHash: record.ContentHash}
	unit := store.Unit{Receipt: &record, ReceiptMode: store.ReceiptComplete, ReceiptExpect: &expect}

	if _, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit); err != nil {
		t.Fatalf("completion with a snapshot digest: %v", err)
	}
	update := receiptUpdateCall(t, db.conn(t).callsCopy())
	if !strings.Contains(strings.ToLower(update.sql), "snapshot_sha256") {
		t.Errorf("completion SQL does not write snapshot_sha256: %s", update.sql)
	}
	snapshot, ok := update.args[3].([]byte)
	if !ok || len(snapshot) != 32 {
		t.Fatalf("completion snapshot argument = %v; want the 32-byte digest", update.args[3])
	}
	for index, value := range digest {
		if snapshot[index] != value {
			t.Fatalf("completion snapshot argument differs from the observed digest")
		}
	}
}
