//go:build integration

package recovery_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestRecoverySweepReportsAcceptedEntryExactlyOnce proves the report-recovery
// sweep over the real Git service, real hooks and the real durable journal: a
// push whose post-receive report failed leaves an accepted entry, and one
// sweep pass after a restart reports it exactly once over the durable state. A
// later pass is a settled no-op and never sends a second report. This is the
// restart case the post-receive-only path cannot recover until another push
// happens. The test calls RecoverPending directly, so it proves the sweep's
// durable behavior, not the production Run startup scheduling; the app unit
// test (TestRunSweepsAcceptedJournalAtStartup) proves that wiring.
func TestRecoverySweepReportsAcceptedEntryExactlyOnce(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	candidate := harness.Commit(t, directory, "sweep-report")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q: a failed report must not undo the applied ref", observed, candidate)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the applied push left no journal entry")
	}
	if entry.State != git.JournalAccepted {
		t.Fatalf("journal state after the failed report = %q; want accepted", entry.State)
	}
	if got := harness.CountReports(); got != baselineReports+1 {
		t.Fatalf("resource.report calls = %d; want %d", got, baselineReports+1)
	}

	// Restart the service over the same durable state. The accepted entry must
	// survive the crash and be reported by the startup sweep, not by a push.
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	survived := mustEntry(t, harness, entry.SourceEventID)
	if survived.State != git.JournalAccepted {
		t.Fatalf("journal state after restart = %q; want accepted before the sweep", survived.State)
	}

	if err := harness.Server().RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	reported := mustEntry(t, harness, entry.SourceEventID)
	if reported.State != git.JournalReported {
		t.Fatalf("journal state after the sweep = %q; want reported", reported.State)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the sweep = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls after the sweep = %d; want %d (one failure, one sweep retry)", got, baselineReports+2)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("the sweep moved main to %q; want %q: recovery reports, it never re-pushes", observed, candidate)
	}

	// A later sweep pass sees a reported entry and is a settled no-op.
	if err := harness.Server().RecoverPending(context.Background()); err != nil {
		t.Fatalf("second RecoverPending: %v", err)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the second sweep = %d; want exactly %d", got, baselineEvents+1)
	}
	if got := harness.CountReports(); got != baselineReports+2 {
		t.Fatalf("resource.report calls after the second sweep = %d; want %d: a reported entry makes no client call", got, baselineReports+2)
	}

	harness.RequireNoSecrets(t, stderr)
}
