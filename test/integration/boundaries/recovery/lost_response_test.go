//go:build integration

package recovery_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestLostReportResponseReplaysReportingOnly proves that a report committed by
// the contract server whose response is lost is retried with the same source
// event identity: the server replays the original result and records exactly
// one event, while the retry performs no verification, apply or ref movement.
func TestLostReportResponseReplaysReportingOnly(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDropAfterCommit})

	candidate := harness.Commit(t, directory, "lost-response")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the applied push left no journal entry")
	}
	if entry.State != git.JournalAccepted {
		t.Fatalf("journal state after the lost response = %q; want accepted", entry.State)
	}
	// The server committed the report before the response was dropped.
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d after the committed report", got, baselineEvents+1)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications+1)
	}
	if got := harness.CountReports(); got != baselineReports+1 {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports+1)
	}
	events := harness.Contract.EventIDs()
	committedEvent := events[len(events)-1]

	// A later effect moves the ref. A reporting-only retry must not re-apply
	// the accepted candidate or move the ref back.
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, candidate, "later effect")
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)
	refsBefore := harness.Refs(t, recoveryfix.Repository)

	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Entry.State != git.JournalReported || !recovery.Reported {
		t.Fatalf("recovery = %+v; want a reported entry", recovery)
	}
	if !recovery.Report.Replayed {
		t.Fatalf("retry response = %+v; want the original committed result replayed", recovery.Report)
	}
	if recovery.Report.EventID != committedEvent {
		t.Fatalf("retry event id = %q; want the original %q", recovery.Report.EventID, committedEvent)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after retry = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count after retry = %d; want %d: the retry must not verify again", got, baselineVerifications+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls = %d; want %d (lost response plus replay)", got, baselineReports+2)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != third {
		t.Fatalf("retry moved main to %q; want the later effect %q: reporting is not an apply", observed, third)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("retry changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t, stderr)
}
