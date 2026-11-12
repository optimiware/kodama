//go:build integration

package recovery_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestDuplicatePushIdentitySettlesExistingEntryAndNewPushVerifiesAfresh proves
// that redelivering the same push identity settles the existing journal entry
// without a second verification or report, while a new push identity is
// verified and reported afresh.
func TestDuplicatePushIdentitySettlesExistingEntryAndNewPushVerifiesAfresh(t *testing.T) {
	events := recoveryfix.NewSourceEvents()
	harness := newHarness(t, recoveryfix.Options{SourceEvents: events.Next})
	harness.InitBareRepository(t, recoveryfix.Repository)
	harness.InstallProductHooks(t, recoveryfix.Repository)
	directory := cloneFor(t, harness, recoveryfix.ExecutionToken, "replay-clone")

	events.Set("source-seed-1")
	first := harness.Commit(t, directory, "seed")
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("seed push: %s", stderr)
	}
	events.Set("source-dup-1")
	dupCommit := harness.Commit(t, directory, "duplicate-delivery")
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/dup"); code != 0 {
		t.Fatalf("first dup push: %s", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/dup"); observed != dupCommit {
		t.Fatalf("remote dup = %q; want %q", observed, dupCommit)
	}
	if got := harness.Contract.VerificationCount(); got != 2 {
		t.Fatalf("verification count = %d; want 2", got)
	}
	if got := harness.Contract.EventCount(); got != 2 {
		t.Fatalf("report event count = %d; want 2", got)
	}
	if got := harness.CountReports(); got != 2 {
		t.Fatalf("resource.report calls = %d; want 2", got)
	}
	entries := harness.JournalEntriesFor(t, "refs/heads/dup")
	if len(entries) != 1 || entries[0].State != git.JournalReported {
		t.Fatalf("dup journal = %+v; want one reported entry", entries)
	}
	duplicateSourceEvent := entries[0].SourceEventID

	// Remove the remote ref so the exact same create can be delivered again
	// with the same push identity.
	harness.DeleteRemoteRef(t, recoveryfix.Repository, "refs/heads/dup")
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/dup"); code != 0 {
		t.Fatalf("duplicate push: %s", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/dup"); observed != dupCommit {
		t.Fatalf("remote dup = %q; want %q after the duplicate apply", observed, dupCommit)
	}
	if got := harness.Contract.VerificationCount(); got != 2 {
		t.Fatalf("verification count = %d; want 2: a duplicate delivery must not verify again", got)
	}
	if got := harness.Contract.EventCount(); got != 2 {
		t.Fatalf("report event count = %d; want 2: a duplicate delivery must not report again", got)
	}
	if got := harness.CountReports(); got != 2 {
		t.Fatalf("resource.report calls = %d; want 2", got)
	}
	entries = harness.JournalEntriesFor(t, "refs/heads/dup")
	if len(entries) != 1 || entries[0].SourceEventID != duplicateSourceEvent {
		t.Fatalf("duplicate delivery journal = %+v; want the same single entry", entries)
	}

	// A new push identity for the same update must verify and report afresh.
	harness.DeleteRemoteRef(t, recoveryfix.Repository, "refs/heads/dup")
	events.Set("source-dup-2")
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/dup"); code != 0 {
		t.Fatalf("fresh push: %s", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/dup"); observed != dupCommit {
		t.Fatalf("remote dup = %q; want %q after the fresh apply", observed, dupCommit)
	}
	if got := harness.Contract.VerificationCount(); got != 3 {
		t.Fatalf("verification count = %d; want 3: a new push identity verifies afresh", got)
	}
	if got := harness.Contract.EventCount(); got != 3 {
		t.Fatalf("report event count = %d; want 3: a new push identity reports afresh", got)
	}
	if got := harness.CountReports(); got != 3 {
		t.Fatalf("resource.report calls = %d; want 3", got)
	}
	entries = harness.JournalEntriesFor(t, "refs/heads/dup")
	if len(entries) != 2 {
		t.Fatalf("dup journal = %+v; want the duplicate entry plus the fresh entry", entries)
	}
	if first == dupCommit {
		t.Fatal("fixture commits are identical")
	}
	harness.RequireNoSecrets(t)
}

// TestDuplicateDeliveryOfAcceptedEntryRetriesReportingOnly proves the accepted
// form of duplicate delivery: after the original push's report failed, the same
// push identity is delivered again. The existing accepted entry must retry its
// report (no second verification, no second apply) and settle reported.
func TestDuplicateDeliveryOfAcceptedEntryRetriesReportingOnly(t *testing.T) {
	events := recoveryfix.NewSourceEvents()
	harness := newHarness(t, recoveryfix.Options{SourceEvents: events.Next})
	directory, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	events.Set("source-late-delivery-1")
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	candidate := harness.Commit(t, directory, "late-delivery")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no journal entry")
	}
	if entry.State != git.JournalAccepted {
		t.Fatalf("journal state after the failed report = %q; want accepted", entry.State)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}

	// Re-deliver the exact same push identity and update.
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("duplicate delivery: %s", stderr)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d: duplicate delivery must not verify again", got, baselineVerifications+1)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d: the accepted entry must retry its report", got, baselineEvents+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls = %d; want %d (failed attempt plus retry)", got, baselineReports+2)
	}
	entries := harness.JournalEntriesFor(t, "refs/heads/main")
	var settled *git.JournalEntry
	for _, current := range entries {
		if current.SourceEventID == entry.SourceEventID {
			copied := current
			settled = &copied
		}
	}
	if settled == nil || settled.State != git.JournalReported {
		t.Fatalf("duplicate delivery journal = %+v; want the same entry reported", settled)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestSameSourceEventWithDifferentContentRefuses proves the stable-source-event
// identity guard at the service boundary: reusing one source event for a
// different ref update is a conflict, not a duplicate delivery. The conflict is
// detected before verification and never changes the ref.
func TestSameSourceEventWithDifferentContentRefuses(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()

	candidateA := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "conflict-a")
	candidateB := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "conflict-b")
	if candidateA == candidateB {
		t.Fatal("fixture commits are identical")
	}
	request := git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidateA,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "conflictSourceEvent1",
	}
	entry, err := harness.Service().Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if entry.State != git.JournalIntended || entry.Candidate != candidateA {
		t.Fatalf("prepared entry = %+v; want the first candidate intended", entry)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications+1)
	}

	conflicting := request
	conflicting.Candidate = candidateB
	if _, err := harness.Service().Prepare(context.Background(), conflicting); !errors.Is(err, git.ErrSourceEventConflict) {
		t.Fatalf("Prepare with a reused source event and different content = %v; want ErrSourceEventConflict", err)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d: a conflict must not verify", got, baselineVerifications+1)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	if raw := rawJournalFields(t, harness, request.SourceEventID); raw["candidate"] != candidateA || raw["state"] != "intended" {
		t.Fatalf("raw journal after the conflict = %v; want the first candidate intended", raw)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want %q: a conflict never applies", observed, first)
	}
	harness.RequireNoSecrets(t)
}
