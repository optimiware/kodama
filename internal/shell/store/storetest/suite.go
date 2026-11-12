// Package storetest holds the store conformance scenarios every adapter must
// satisfy, so a rule the memory adapter enforces cannot be missing from the
// PostgreSQL adapter (or the reverse).
//
// The package imports only the neutral store seam and testing. Each adapter
// supplies its own backend through Run, so no adapter imports another and the
// scenarios never depend on a concrete harness.
package storetest

import (
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Backend is one adapter under test.
type Backend struct {
	Executor store.Executor
}

// Scope returns the project scope every scenario runs in.
func Scope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-parity", ProjectID: "project-parity"}
}

// Run executes the shared conformance scenarios against one adapter.
func Run(t *testing.T, newBackend func(*testing.T) Backend) {
	t.Run("refuses a business write to a publication table", func(t *testing.T) {
		backend := newBackend(t)
		for _, table := range []string{"receipts", "audit"} {
			_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
				Writes: []store.Write{{Table: table, ID: "row-parity-1", Insert: true, Values: map[string]any{}}},
			})
			requireClass(t, err, fault.InvalidInput, "business write to "+table)
		}
	})

	t.Run("refuses generic events writes", func(t *testing.T) {
		backend := newBackend(t)
		cases := []struct {
			name  string
			write store.Write
		}{
			{name: "insert", write: store.Write{Table: "events", ID: "event-parity-1", Insert: true, Values: map[string]any{"type": "task.created"}}},
			{name: "unpublishable state", write: store.Write{Table: "events", ID: "event-parity-1", Values: map[string]any{"publish_state": "claimed"}, Expect: map[string]any{"publish_state": "pending"}}},
			{name: "no guard", write: store.Write{Table: "events", ID: "event-parity-1", Values: map[string]any{"publish_state": "published"}}},
			{name: "extra column", write: store.Write{Table: "events", ID: "event-parity-1", Values: map[string]any{"publish_state": "published", "type": "task.created"}, Expect: map[string]any{"publish_state": "pending"}}},
		}
		for _, testCase := range cases {
			_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{Writes: []store.Write{testCase.write}})
			requireClass(t, err, fault.InvalidInput, "events "+testCase.name)
		}
	})

	t.Run("a root row outside the scope is forbidden before its shape is judged", func(t *testing.T) {
		backend := newBackend(t)
		// The write is both cross-tenant and shapeless (an update without its
		// expected version). Scope is checked first by every adapter, so the
		// refusal is forbidden, never invalid_input from one and forbidden
		// from the other.
		_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{Table: "tenants", ID: "tenant-other", Values: map[string]any{"name": "renamed"}}},
		})
		requireClass(t, err, fault.Forbidden, "cross-tenant root write")
	})

	t.Run("refuses an unknown column", func(t *testing.T) {
		backend := newBackend(t)
		_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{Table: "tasks", ID: "task-parity-1", Insert: true, Values: map[string]any{"no_such_column": "x"}}},
		})
		requireClass(t, err, fault.InvalidInput, "unknown column")
	})

	t.Run("refuses a receipt mode without a receipt", func(t *testing.T) {
		backend := newBackend(t)
		for _, mode := range []store.ReceiptMode{store.ReceiptInsertIncomplete, store.ReceiptComplete} {
			_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{ReceiptMode: mode})
			requireClass(t, err, fault.InvalidInput, string(mode)+" without a receipt")
		}
	})

	t.Run("completion keeps, sets or replaces the snapshot digest", func(t *testing.T) {
		first := [32]byte{1, 2, 3}
		second := [32]byte{9, 8, 7}
		cases := []struct {
			name       string
			inserted   *[32]byte
			completing *[32]byte
			want       *[32]byte
		}{
			{name: "keeps the inserted digest", inserted: &first, want: &first},
			{name: "sets an absent digest", completing: &second, want: &second},
			{name: "replaces the inserted digest", inserted: &first, completing: &second, want: &second},
			{name: "leaves an absent digest absent"},
		}
		for index, testCase := range cases {
			backend := newBackend(t)
			record := parityReceipt(t, index)
			record.Result = []byte("selection")
			record.SnapshotHash = testCase.inserted
			if _, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
				Receipt: &record, ReceiptMode: store.ReceiptInsertIncomplete,
			}); err != nil {
				t.Fatalf("%s: insert the incomplete receipt: %v", testCase.name, err)
			}
			completion := record
			completion.Result = []byte("started")
			completion.SnapshotHash = testCase.completing
			unit := store.Unit{
				Receipt:       &completion,
				ReceiptMode:   store.ReceiptComplete,
				ReceiptExpect: &store.ReceiptExpect{ContentHash: record.ContentHash},
			}
			if _, err := backend.Executor.Execute(t.Context(), Scope(), unit); err != nil {
				t.Fatalf("%s: complete the receipt: %v", testCase.name, err)
			}
			// A second completion replays the durable row, which is what a
			// retrying caller observes.
			replay, err := backend.Executor.Execute(t.Context(), Scope(), unit)
			if err != nil || !replay.Replayed {
				t.Fatalf("%s: replay = %+v, %v; want a replay of the completed receipt", testCase.name, replay, err)
			}
			if string(replay.Receipt.Result) != "started" {
				t.Errorf("%s: durable result = %q; want started", testCase.name, replay.Receipt.Result)
			}
			got := replay.Receipt.SnapshotHash
			switch {
			case testCase.want == nil && got != nil:
				t.Errorf("%s: durable snapshot digest = %x; want none", testCase.name, *got)
			case testCase.want != nil && (got == nil || *got != *testCase.want):
				t.Errorf("%s: durable snapshot digest = %v; want %x", testCase.name, got, *testCase.want)
			}
		}
	})

	t.Run("nil expectation matches only a NULL column", func(t *testing.T) {
		backend := newBackend(t)
		seedTaskWithoutExecution(t, backend)

		// The row's current_execution_id is NULL, so a nil expectation matches it.
		if _, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{
				Table:  "tasks",
				ID:     taskID,
				Values: map[string]any{"current_execution_id": "execution-parity"},
				Expect: map[string]any{"current_execution_id": nil, "lifecycle_version": int64(1)},
			}},
		}); err != nil {
			t.Fatalf("update guarded by a nil expectation: %v", err)
		}
		row, err := backend.Executor.Read(t.Context(), Scope(), "tasks", taskID)
		if err != nil {
			t.Fatalf("read updated task: %v", err)
		}
		if got := row.Values["current_execution_id"]; got != "execution-parity" {
			t.Fatalf("current_execution_id = %v; want execution-parity", got)
		}

		// The column is no longer NULL, so the same guard is now a conflict.
		_, err = backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{
				Table:  "tasks",
				ID:     taskID,
				Values: map[string]any{"current_execution_id": nil},
				Expect: map[string]any{"current_execution_id": nil},
			}},
		})
		requireClass(t, err, fault.VersionConflict, "stale nil expectation")
	})

	t.Run("version conflict on a stale expected version", func(t *testing.T) {
		backend := newBackend(t)
		seedService(t, backend)

		if _, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{
				Table:         "services",
				ID:            serviceID,
				Values:        map[string]any{"state": "disabled"},
				ExpectVersion: 1,
			}},
		}); err != nil {
			t.Fatalf("update at the observed version: %v", err)
		}
		_, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
			Writes: []store.Write{{
				Table:         "services",
				ID:            serviceID,
				Values:        map[string]any{"state": "enabled"},
				ExpectVersion: 1,
			}},
		})
		requireClass(t, err, fault.VersionConflict, "stale expected version")
	})
}

const (
	taskID    = "task-parity-1"
	serviceID = "service-parity-1"
)

// seedTaskWithoutExecution inserts one task row whose current_execution_id is
// NULL. Only the columns both adapters need are supplied.
func seedTaskWithoutExecution(t *testing.T, backend Backend) {
	t.Helper()
	if _, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
		Writes: []store.Write{{
			Table:  "tasks",
			ID:     taskID,
			Insert: true,
			Values: map[string]any{
				"content_version":   int64(1),
				"lifecycle_version": int64(1),
				"state":             "open",
				"initiator_id":      "principal-parity",
				"title":             "parity",
				"created_at":        parityTime(),
			},
		}},
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
}

// seedService inserts one enabled service row at version 1.
func seedService(t *testing.T, backend Backend) {
	t.Helper()
	if _, err := backend.Executor.Execute(t.Context(), Scope(), store.Unit{
		Writes: []store.Write{{
			Table:  "services",
			ID:     serviceID,
			Insert: true,
			Values: map[string]any{
				"name":       "parity",
				"roles":      []byte(`["engineer"]`),
				"state":      "enabled",
				"version":    int64(1),
				"created_at": parityTime(),
			},
		}},
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

// parityReceipt returns one receipt record under its own caller key.
func parityReceipt(t *testing.T, index int) receipt.Record {
	t.Helper()
	fingerprint, err := receipt.Fingerprint(receipt.Content{MediaType: "application/json", Bytes: []byte(`{"parity":true}`)})
	if err != nil {
		t.Fatalf("fingerprint receipt content: %v", err)
	}
	return receipt.Record{
		ID: id.ReceiptID(fmt.Sprintf("receipt-parity-%d", index)),
		Key: receipt.Key{
			TenantID:    Scope().TenantID,
			ProjectID:   Scope().ProjectID,
			PrincipalID: "principal-parity",
			Operation:   "pipeline.start",
			CallerKey:   fmt.Sprintf("caller-parity-%d", index),
		},
		ContentHash: fingerprint,
		At:          parityTime(),
	}
}

func parityTime() time.Time {
	return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
}

func requireClass(t *testing.T, err error, want fault.Class, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted; want %s", what, want)
	}
	if class, ok := fault.ClassOf(err); !ok || class != want {
		t.Fatalf("%s class = %v (%v); want %s", what, class, err, want)
	}
}
