package receipt_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/receipt"
)

const (
	testTenant     = id.TenantID("tenant-a")
	testProject    = id.ProjectID("project-a")
	testPrincipal  = id.PrincipalID("principal-a")
	otherTenant    = id.TenantID("tenant-b")
	otherProject   = id.ProjectID("project-b")
	otherPrincipal = id.PrincipalID("principal-b")
	testOperation  = "task.create"
	testCallerKey  = "caller-key-1"

	contentHashOne = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	contentHashTwo = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// testKey is one project-scoped idempotency identity.
func testKey() receipt.Key {
	return receipt.Key{
		TenantID:    testTenant,
		ProjectID:   testProject,
		PrincipalID: testPrincipal,
		Operation:   testOperation,
		CallerKey:   testCallerKey,
	}
}

// committedRecord is the durable receipt of the first, already committed
// invocation.
func committedRecord() receipt.Record {
	return receipt.Record{
		ID:          id.ReceiptID("receipt-1"),
		Key:         testKey(),
		ContentHash: contentHashOne,
		Result:      []byte(`{"task_id":"task-1"}`),
		ExecutionID: id.ExecutionID("exec-1"),
		At:          fixedNow,
	}
}

// retryCandidate is an identical retry whose freshly computed result would
// differ if the adapter wrongly re-executed the operation. Only Key and
// ContentHash decide the match; Result, ExecutionID, ID and At must not.
func retryCandidate() receipt.Record {
	record := committedRecord()
	record.ID = id.ReceiptID("receipt-2")
	record.Result = []byte(`{"task_id":"task-2"}`)
	record.ExecutionID = id.ExecutionID("exec-2")
	record.At = fixedNow.Add(time.Minute)
	return record
}

// changedCandidate reuses the caller key with different request content.
func changedCandidate() receipt.Record {
	record := retryCandidate()
	record.ContentHash = contentHashTwo
	return record
}

// cloneRecord makes an independent copy so a test can detect mutation of the
// stored record by comparing before and after snapshots.
func cloneRecord(record receipt.Record) receipt.Record {
	clone := record
	clone.Result = append([]byte(nil), record.Result...)
	if record.SnapshotHash != nil {
		hash := *record.SnapshotHash
		clone.SnapshotHash = &hash
	}
	return clone
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want fault class %q", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok || got != want {
		t.Fatalf("fault class = %q (found=%v), want %q (err: %v)", got, ok, want, err)
	}
}
