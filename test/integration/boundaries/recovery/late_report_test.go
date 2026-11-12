//go:build integration

package recovery_test

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestLateReportFailureLeavesAcceptedEntryAndRecoversExactlyOnce proves the
// late-report boundary: a reporting failure after the native receive-pack apply
// never undoes the ref, never loses the durable entry and is recovered by a
// later pass on a restarted app exactly once.
func TestLateReportFailureLeavesAcceptedEntryAndRecoversExactlyOnce(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})

	candidate := harness.Commit(t, directory, "late-report")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	// post-receive runs after receive-pack applied the ref; its failure is
	// reported to the client but does not reject or undo the native apply.
	requireSuccess(t, stderr, code)
	if !strings.Contains(stderr, nativeRejectionMessage) {
		t.Fatalf("push stderr = %q; want the post-receive reporting rejection", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q: a reporting failure must not undo the applied ref", observed, candidate)
	}

	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the applied push left no journal entry")
	}
	if entry.State != git.JournalAccepted {
		t.Fatalf("journal state after the reporting failure = %q; want accepted", entry.State)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d before recovery", got, baselineEvents)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications+1)
	}
	if got := harness.CountReports(); got != baselineReports+1 {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports+1)
	}

	// Restart the Git app over the same journal directory. The accepted entry
	// must survive in memory loss.
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	survived := mustEntry(t, harness, entry.SourceEventID)
	if survived.State != git.JournalAccepted {
		t.Fatalf("journal state after restart = %q; want accepted", survived.State)
	}

	recovery := recoverEntry(t, harness, survived.SourceEventID)
	if recovery.Entry.State != git.JournalReported || !recovery.Reported {
		t.Fatalf("recovery = %+v; want a reported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	events := harness.Contract.EventIDs()
	if len(events) == 0 || recovery.Report.EventID != events[len(events)-1] {
		t.Fatalf("recovery event id = %q; want the single new event %v", recovery.Report.EventID, events)
	}

	// A second recovery pass is a settled no-op: still exactly one report.
	second := recoverEntry(t, harness, survived.SourceEventID)
	if second.Entry.State != git.JournalReported || !second.Reported {
		t.Fatalf("second recovery = %+v; want a reported no-op", second)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the second recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls = %d; want %d (one failure, one retry)", got, baselineReports+2)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("recovery moved main to %q; want %q", observed, candidate)
	}

	harness.RequireNoSecrets(t, stderr)
}

// TestDelayedReportTimesOutAndRecoversOnRetry covers the timeout form of the
// late report: the reporting call outlives the bounded hook callback, the entry
// stays accepted and the retry reports exactly once.
func TestDelayedReportTimesOutAndRecoversOnRetry(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{HookTimeout: time.Second})
	directory, _ := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDelay, Delay: 1500 * time.Millisecond})

	candidate := harness.Commit(t, directory, "delayed-report")
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
		t.Fatalf("journal state after the delayed report = %q; want accepted", entry.State)
	}

	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Entry.State != git.JournalReported || !recovery.Reported {
		t.Fatalf("recovery = %+v; want a reported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count after recovery = %d; want %d (reporting-only retry)", got, baselineVerifications+1)
	}
	// The timed-out request may still be in flight; wait for it to finish so
	// the report-call count is not read mid-request.
	waitForReports(t, harness, baselineReports+2)
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the delayed request settled = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}
