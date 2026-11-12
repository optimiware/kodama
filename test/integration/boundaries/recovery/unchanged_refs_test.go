//go:build integration

package recovery_test

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestRefusedPushLeavesRefsAndObjectStoreIdentical proves that a push refused
// before any effect leaves every ref byte-identical and does not leave the
// pushed objects in the repository object store (receive-pack's quarantine is
// discarded).
func TestRefusedPushLeavesRefsAndObjectStoreIdentical(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	objectsBefore := harness.ObjectFiles(t, recoveryfix.Repository)

	candidate := harness.Commit(t, directory, "denied-protected")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/protected")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)

	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("a refused push changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	if after := harness.ObjectFiles(t, recoveryfix.Repository); !reflect.DeepEqual(after, objectsBefore) {
		t.Fatalf("a refused push left objects in the store:\nbefore: %v\nafter:  %v", objectsBefore, after)
	}
	entry := entryForCandidate(t, harness, "refs/heads/protected", candidate)
	if entry == nil {
		t.Fatal("the refused push left no journal entry")
	}
	if entry.State != git.JournalRefused {
		t.Fatalf("refused journal state = %q; want refused", entry.State)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: a refusal is never reported", got, baselineEvents)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileRefusedPushLeavesRefsByteIdentical proves the reconcile-refused
// form of the unchanged-refs rule: when observation proves the candidate never
// became the ref, recovery records a refusal and every ref stays byte-identical.
func TestReconcileRefusedPushLeavesRefsByteIdentical(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "reconcile-refused")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}

	// Restore the base ref so observation proves the effect never happened.
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryNotApplied || recovery.Entry.State != git.JournalRefused || recovery.Reported {
		t.Fatalf("recovery = %+v; want a refused, unreported entry", recovery)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("reconcile-refused changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	harness.RequireNoSecrets(t, stderr)
}
