// Receipt completion compare-and-set reclassification.
//
// The shared seam vectors exercise the successful and stale paths. This test
// scripts the one outcome the vectors cannot force sequentially: the CAS itself
// matches zero rows because a concurrent completer won, and the adapter must
// re-read and replay the first result instead of overwriting it.
package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/store"
)

func TestReceiptCompletionZeroRowReclassifiesAsReplay(t *testing.T) {
	db := newFakeDB()
	record := testReceipt(t)
	incomplete := receiptFixture(t, record)
	incomplete["completed_at"] = nil
	incomplete["result"] = nil
	completed := receiptFixture(t, record)
	completed["completed_at"] = record.At

	receiptReads := 0
	db.script.setQuery(func(sql string, args []any) (store.Rows, error) {
		if !strings.Contains(strings.ToLower(sql), "from receipts") {
			return &fakeRows{index: -1}, nil
		}
		receiptReads++
		if receiptReads == 1 {
			return singleRow("receipts", sql, incomplete), nil
		}
		return singleRow("receipts", sql, completed), nil
	})
	db.script.setExec(func(sql string, args []any) (store.Result, error) {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(sql)), "update receipts") {
			return store.Result{RowsAffected: 0}, nil
		}
		return store.Result{RowsAffected: 1}, nil
	})

	expect := store.ReceiptExpect{Completed: false, ContentHash: record.ContentHash}
	unit := store.Unit{Receipt: &record, ReceiptMode: store.ReceiptComplete, ReceiptExpect: &expect}
	result, err := ExecuteUnit(context.Background(), New(db), testProjectScope(), unit)
	if err != nil {
		t.Fatalf("zero-row completion: %v", err)
	}
	if !result.Replayed {
		t.Fatal("a zero-row compare-and-set did not reclassify as a replay")
	}
	if string(result.Receipt.Result) != string(record.Result) {
		t.Errorf("replayed result = %q; want the first committed result %q", result.Receipt.Result, record.Result)
	}
	if receiptReads != 2 {
		t.Errorf("receipt reads = %d; want the lookup plus the zero-row re-read", receiptReads)
	}
	conn := db.conn(t)
	if conn.commits != 1 || conn.rollbacks != 0 {
		t.Fatalf("terminal calls: commits=%d rollbacks=%d; want one commit", conn.commits, conn.rollbacks)
	}
}
