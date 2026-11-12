package session_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/userland/session"
)

func assertRestoreReason(t *testing.T, store session.Store, ref session.Ref, reason session.Reason) {
	t.Helper()
	record, err := store.Restore(context.Background(), ref)
	if err == nil {
		t.Fatalf("Restore(%s) succeeded with %+v; want %s", ref, record, reason)
	}
	var restoreError session.RestoreError
	if !errors.As(err, &restoreError) {
		t.Fatalf("Restore(%s) error = %v; want a typed RestoreError", ref, err)
	}
	if restoreError.Reason != reason {
		t.Fatalf("Restore(%s) reason = %q; want %q", ref, restoreError.Reason, reason)
	}
	if restoreError.Ref != ref {
		t.Fatalf("Restore(%s) error ref = %q", ref, restoreError.Ref)
	}
}

// TestRestoreTypedReasons pins the restoration vocabulary over retained fake
// storage: missing, unsupported version, corrupt and missing context are typed
// refusals, never an empty success.
func TestRestoreTypedReasons(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	missing := mustRef(t, "kodama-checkpoint/v1/ctx-9999")
	assertRestoreReason(t, store, missing, session.ReasonMissing)

	versionRef := mustRef(t, "kodama-checkpoint/v1/ctx-0101")
	unsupported := baseRecord()
	unsupported.Version = "v2"
	storage.put(versionRef, unsupported)
	assertRestoreReason(t, store, versionRef, session.ReasonVersionMismatch)

	corruptRef := mustRef(t, "kodama-checkpoint/v1/ctx-0102")
	corrupt := baseRecord()
	corrupt.Messages = []session.Message{{Role: "wizard", Content: "x"}}
	storage.put(corruptRef, corrupt)
	assertRestoreReason(t, store, corruptRef, session.ReasonCorrupt)

	missingContext := baseRecord()
	missingContext.ContextRefs = []session.Ref{missing}
	contextRef, err := store.Save(context.Background(), missingContext)
	if err != nil {
		t.Fatalf("Save with a durable context reference: %v", err)
	}
	assertRestoreReason(t, store, contextRef, session.ReasonContextMissing)

	if _, err := store.Restore(context.Background(), session.Ref("not-a-checkpoint")); err == nil {
		t.Fatal("Restore accepted an unparseable reference")
	} else {
		if _, ok := errors.AsType[session.RestoreError](err); ok {
			t.Fatalf("unparseable reference reported a typed restore error: %v", err)
		}
	}
}

// TestRestoreUnsupportedReferenceVersionIsTyped pins that a checkpoint
// reference carrying an unsupported version of the checkpoint family is a
// typed version mismatch, refused before any storage read, rather than a
// generic parse error.
func TestRestoreUnsupportedReferenceVersionIsTyped(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	for _, ref := range []session.Ref{
		"kodama-checkpoint/v2/ctx-0001",
		"kodama-checkpoint/v2/",
		"kodama-checkpoint/v2",
		"kodama-checkpoint/v10/ctx-0001",
	} {
		record, err := store.Restore(context.Background(), ref)
		if err == nil {
			t.Fatalf("Restore(%s) succeeded with %+v; want a version mismatch", ref, record)
		}
		var restoreError session.RestoreError
		if !errors.As(err, &restoreError) {
			t.Fatalf("Restore(%s) error = %v; want a typed RestoreError", ref, err)
		}
		if restoreError.Reason != session.ReasonVersionMismatch {
			t.Fatalf("Restore(%s) reason = %q; want %q", ref, restoreError.Reason, session.ReasonVersionMismatch)
		}
		if restoreError.Ref != ref {
			t.Fatalf("Restore(%s) error ref = %q", ref, restoreError.Ref)
		}
	}
	if len(storage.reads) != 0 {
		t.Fatalf("Restore read storage for an unsupported version: %v", storage.reads)
	}

	// A supported version with a malformed payload stays an untyped refusal.
	if _, err := store.Restore(context.Background(), "kodama-checkpoint/v1/"); err == nil {
		t.Fatal("Restore accepted a malformed supported-version reference")
	} else {
		if _, ok := errors.AsType[session.RestoreError](err); ok {
			t.Fatalf("malformed supported-version reference reported a typed restore error: %v", err)
		}
	}
}

// TestRestoreResolvesDurableContext pins that a checkpoint whose context
// references still resolve restores, and the references are returned as
// references rather than inlined content.
func TestRestoreResolvesDurableContext(t *testing.T) {
	storage := newFakeStorage()
	store := newTestStore(t, storage, &fakeClock{now: testNow})

	contextRef, err := store.Save(context.Background(), baseRecord())
	if err != nil {
		t.Fatalf("Save(context): %v", err)
	}
	record := baseRecord()
	record.Revision = 1
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.ContextRefs = []session.Ref{contextRef}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save(dependent): %v", err)
	}

	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !reflect.DeepEqual(restored.ContextRefs, record.ContextRefs) {
		t.Fatalf("restored context refs = %v; want %v", restored.ContextRefs, record.ContextRefs)
	}
	if !reflect.DeepEqual(restored.Messages, record.Messages) {
		t.Fatalf("restored messages = %+v; want the record's own messages, not inlined context", restored.Messages)
	}
}

// TestFreshStoreRestoresRetainedStorage pins recovery without process
// continuity: a new store over the same durable storage restores the
// checkpoint, and no approval, scratch or process state is carried.
func TestFreshStoreRestoresRetainedStorage(t *testing.T) {
	storage := newFakeStorage()
	clock := &fakeClock{now: testNow}
	first := newTestStore(t, storage, clock)

	record := baseRecord()
	record.Retention = session.RetentionPolicy{RawContent: true}
	record.Usage = []session.Usage{{PrincipalID: "prn-1", Model: "model-a", Prompt: 10, Completion: 5}}
	record.Outcome = session.Outcome{Status: "completed", Summary: "done"}
	record.Messages = append(record.Messages, session.Message{Role: "user", Content: "which option should I take?"})
	ref, err := first.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh execution builds a new store over retained storage only.
	second := newTestStore(t, storage, clock)
	restored, err := second.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("Restore from a fresh store: %v", err)
	}
	if restored.SessionID != record.SessionID {
		t.Fatalf("restored session = %q; want %q", restored.SessionID, record.SessionID)
	}
	if restored.Revision != 1 {
		t.Fatalf("restored revision = %d; want 1", restored.Revision)
	}
	if restored.Version != session.RefVersion {
		t.Fatalf("restored version = %q; want %q", restored.Version, session.RefVersion)
	}
	if !reflect.DeepEqual(restored.Messages, record.Messages) {
		t.Fatalf("restored messages = %+v; want %+v", restored.Messages, record.Messages)
	}
	if !reflect.DeepEqual(restored.Usage, record.Usage) {
		t.Fatalf("restored usage = %+v; want %+v", restored.Usage, record.Usage)
	}
	if restored.Outcome != record.Outcome {
		t.Fatalf("restored outcome = %+v; want %+v", restored.Outcome, record.Outcome)
	}

	assertNoFieldMatching(t, reflect.TypeFor[session.Record](),
		"credential", "token", "password", "secret", "bearer",
		"approval", "scratch", "process", "worktree", "workspace", "pid",
	)
}
