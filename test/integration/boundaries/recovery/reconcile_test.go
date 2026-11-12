//go:build integration

package recovery_test

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// unreachablePostReceive makes the post-receive phase unreachable, so the
// native receive-pack apply happens but no acceptance or report is recorded.
// The durable pre-effect intent is what a restart must reconcile.
func unreachablePostReceive(t *testing.T, harness *recoveryfix.Harness) {
	t.Helper()
	harness.WriteHook(t, recoveryfix.Repository, "post-receive", "#!/bin/sh\nexit 1\n")
}

// TestReconcileOnRestartObservedCandidateAcceptsAndReports proves the first of
// the three observed states: the ref equals the journaled candidate, so
// recovery records acceptance and reports it exactly once.
func TestReconcileOnRestartObservedCandidateAcceptsAndReports(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "applied-without-post-receive")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want the applied %q", observed, candidate)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the applied push left no durable intent")
	}
	if entry.State != git.JournalIntended {
		t.Fatalf("journal state after the unreachable post-receive = %q; want intended", entry.State)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d before recovery", got, baselineEvents)
	}

	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	survived := mustEntry(t, harness, entry.SourceEventID)
	if survived.State != git.JournalIntended {
		t.Fatalf("journal state after restart = %q; want intended", survived.State)
	}

	recovery := recoverEntry(t, harness, survived.SourceEventID)
	if recovery.Outcome != git.RecoveryApplied {
		t.Fatalf("recovery outcome = %q; want applied", recovery.Outcome)
	}
	if recovery.Entry.State != git.JournalReported || !recovery.Reported {
		t.Fatalf("recovery = %+v; want an accepted and reported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("recovery moved main to %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileOnRestartObservedBaseRefusesWithoutReporting proves the second
// observed state: the ref still equals the journaled base, so the effect never
// happened and recovery records a refusal without ever reporting an update.
func TestReconcileOnRestartObservedBaseRefusesWithoutReporting(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "not-applied")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if entry.ExpectedBase != first {
		t.Fatalf("journal expected base = %q; want %q", entry.ExpectedBase, first)
	}
	// A later effect returns the ref to the journaled base, so repository
	// evidence proves the candidate is not the current ref.
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	refsBefore := harness.Refs(t, recoveryfix.Repository)

	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryNotApplied {
		t.Fatalf("recovery outcome = %q; want not_applied", recovery.Outcome)
	}
	if recovery.Entry.State != git.JournalRefused || recovery.Reported {
		t.Fatalf("recovery = %+v; want a refused, unreported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: a not-applied update is never reported", got, baselineEvents)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("recovery moved main to %q; want the observed base %q", observed, first)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("recovery changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileOnRestartObservedOtherStaysUncertain proves the third observed
// state: a ref that is neither the base nor the candidate leaves the entry
// uncertain and unreported. Recovery never guesses.
func TestReconcileOnRestartObservedOtherStaysUncertain(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unknown-outcome")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, candidate, "third observed state")
	if third == candidate || third == entry.ExpectedBase {
		t.Fatalf("fixture third commit %q is not a distinct observed state", third)
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)

	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryUnknown {
		t.Fatalf("recovery outcome = %q; want unknown", recovery.Outcome)
	}
	if recovery.Entry.State != git.JournalUncertain || recovery.Reported {
		t.Fatalf("recovery = %+v; want an uncertain, unreported entry", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: an unknown outcome is never reported", got, baselineEvents)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != third {
		t.Fatalf("recovery guessed a ref %q; want the observed third state %q", observed, third)
	}

	// A repeated recovery stays uncertain and never reports.
	again := recoverEntry(t, harness, entry.SourceEventID)
	if again.Outcome != git.RecoveryUnknown || again.Entry.State != git.JournalUncertain || again.Reported {
		t.Fatalf("second recovery = %+v; want the same uncertain state", again)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count after the second recovery = %d; want %d", got, baselineEvents)
	}
	if again.Entry.Candidate != candidate || again.Entry.ExpectedBase != entry.ExpectedBase {
		t.Fatalf("recovery rewrote the journal identity: %+v", again.Entry)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileOnRestartReportFailureKeepsAcceptedEntryRecoverable covers a
// report that fails DURING the restart recovery, not on the original push.
// Recovery must durably persist `accepted` before reporting; a failed report
// leaves the raw entry accepted (never reported, never intended, never
// re-applied) and a later pass reports exactly once.
func TestReconcileOnRestartReportFailureKeepsAcceptedEntryRecoverable(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "reconcile-report-failure")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// Queue the failure only after the restart, so only the recovery report can
	// consume it.
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	recovery, err := harness.Service().Recover(context.Background(), entry.SourceEventID)
	if err == nil {
		t.Fatalf("recovery = %+v; want the report dependency error", recovery)
	}
	if recovery.Reported {
		t.Fatalf("recovery = %+v; a failed report must not claim reported", recovery)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state after the failed recovery report = %q; want accepted", state)
	}
	raw := rawJournalFields(t, harness, entry.SourceEventID)
	if raw["candidate"] != candidate || raw["ref"] != "refs/heads/main" {
		t.Fatalf("raw journal identity changed after the failed report: %v", raw)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: a failed report must not report", got, baselineEvents)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("a failed recovery report changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}

	// The contract server returns; the durable accepted entry reports once.
	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("later recovery = %+v; want a reported entry", final)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "reported" {
		t.Fatalf("raw journal state after the successful retry = %q; want reported", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the retry = %d; want exactly %d", got, baselineEvents+1)
	}
	events := harness.Contract.EventIDs()
	if len(events) == 0 || final.Report.EventID != events[len(events)-1] {
		t.Fatalf("retry event id = %q; want the single new event %v", final.Report.EventID, events)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("the successful recovery changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileOnRestartReportDropKeepsAcceptedThenReplays proves the
// lost-response form during restart recovery: the server commits the report and
// drops the response, so recovery fails while the raw entry stays accepted; the
// next pass replays the original result with exactly one event and no ref move.
func TestReconcileOnRestartReportDropKeepsAcceptedThenReplays(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "reconcile-report-drop")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureDropAfterCommit})
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	recovery, err := harness.Service().Recover(context.Background(), entry.SourceEventID)
	if err == nil {
		t.Fatalf("recovery = %+v; want the dropped-response error", recovery)
	}
	if recovery.Reported {
		t.Fatalf("recovery = %+v; a lost response must not claim reported", recovery)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state after the dropped response = %q; want accepted", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d: the server committed exactly one report", got, baselineEvents+1)
	}
	events := harness.Contract.EventIDs()
	committedEvent := events[len(events)-1]
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("the dropped response changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("replay recovery = %+v; want a reported entry", final)
	}
	if !final.Report.Replayed {
		t.Fatalf("replay response = %+v; want the original result replayed", final.Report)
	}
	if final.Report.EventID != committedEvent {
		t.Fatalf("replay event id = %q; want the original %q", final.Report.EventID, committedEvent)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the replay = %d; want exactly %d", got, baselineEvents+1)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "reported" {
		t.Fatalf("raw journal state after the replay = %q; want reported", state)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("the replay changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileUncertainWithContractServerDownStaysUncertainThenRecovers proves
// the uncertain path while the contract server is down: recovery observes the
// unknown ref, reports nothing, and stays durably uncertain. When the ref later
// proves the candidate, a report failure during recovery still persists
// `accepted`, and the next pass reports exactly once.
func TestReconcileUncertainWithContractServerDownStaysUncertainThenRecovers(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "uncertain-report-down")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, candidate, "uncertain third state")
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryUnknown {
		t.Fatalf("recovery outcome = %q; want unknown", recovery.Outcome)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "uncertain" {
		t.Fatalf("raw journal state = %q; want uncertain", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: an unknown outcome is never reported", got, baselineEvents)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("uncertain recovery changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	if again := recoverEntry(t, harness, entry.SourceEventID); again.Outcome != git.RecoveryUnknown ||
		rawJournalState(t, harness, entry.SourceEventID) != "uncertain" {
		t.Fatalf("second uncertain recovery = %+v; want the same uncertain state", again)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count after the second uncertain recovery = %d; want %d", got, baselineEvents)
	}

	// The ref now proves the candidate. The queued report failure hits this
	// recovery: acceptance must be durable and the entry must stay recoverable.
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", candidate)
	failed, err := harness.Service().Recover(context.Background(), entry.SourceEventID)
	if err == nil {
		t.Fatalf("recovery = %+v; want the report dependency error", failed)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state after the failed report = %q; want accepted", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("final recovery = %+v; want a reported entry", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the final recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileRefusedWithContractServerDownDoesNotReport proves that a refusal
// proven from repository evidence does not depend on the contract server: it is
// recorded refused, reports nothing and never consumes the queued failure.
func TestReconcileRefusedWithContractServerDownDoesNotReport(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	baselineReports := harness.CountReports()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "refused-report-down")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	for pass := range 2 {
		recovery := recoverEntry(t, harness, entry.SourceEventID)
		if recovery.Entry.State != git.JournalRefused || recovery.Reported {
			t.Fatalf("refused recovery pass %d = %+v; want refused and unreported", pass, recovery)
		}
		// The first pass proves non-application; a later pass is the settled
		// refused no-op the product defines (no recovery outcome).
		if pass == 0 && recovery.Outcome != git.RecoveryNotApplied {
			t.Fatalf("first refused recovery outcome = %q; want not_applied", recovery.Outcome)
		}
		if state := rawJournalState(t, harness, entry.SourceEventID); state != "refused" {
			t.Fatalf("raw journal state after pass %d = %q; want refused", pass, state)
		}
		if got := harness.Contract.EventCount(); got != baselineEvents {
			t.Fatalf("report event count after pass %d = %d; want %d", pass, got, baselineEvents)
		}
		if got := harness.CountReports(); got != baselineReports {
			t.Fatalf("resource.report calls after pass %d = %d; want %d", pass, got, baselineReports)
		}
		if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
			t.Fatalf("refused recovery pass %d changed refs:\nbefore:\n%s\nafter:\n%s", pass, refsBefore, after)
		}
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestPostReceiveReconcilesWithoutVerificationOrGuessing proves the post-receive
// phase is observation-only: when the durable intent is lost between pre-receive
// and post-receive, post-receive refuses rather than re-verifying, journaling or
// guessing. A post-receive dispatched as a second pre-receive would verify
// again and leave a new intent.
func TestPostReceiveReconcilesWithoutVerificationOrGuessing(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()

	// The test-only pre-receive wrapper runs the real product hook and then
	// deletes every durable journal file, simulating a lost journal write
	// before receive-pack applies the ref.
	script := "#!/bin/sh\n" +
		"set -u\n" +
		shellQuote(harness.BinaryPath) + " git hook pre-receive\n" +
		"code=$?\n" +
		"rm -f " + shellQuote(harness.JournalDir) + "/*.json\n" +
		"exit $code\n"
	harness.WriteHook(t, recoveryfix.Repository, "pre-receive", script)

	candidate := harness.Commit(t, directory, "lost-journal")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want the applied %q", observed, candidate)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications+1 {
		t.Fatalf("verification count = %d; want %d: post-receive must not verify again", got, baselineVerifications+1)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: no journal evidence means no report", got, baselineEvents)
	}
	if entries := harness.JournalEntriesFor(t, "refs/heads/main"); len(entries) != 0 {
		t.Fatalf("journal entries = %+v; want none: post-receive must not journal or guess", entries)
	}
	if !strings.Contains(stderr, nativeRejectionMessage) {
		t.Fatalf("push stderr = %q; want the post-receive rejection", stderr)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestApplyObservedCandidateRecordsAcceptanceWithoutSecondEffect proves the
// apply observation boundary: an intended entry whose ref already equals the
// candidate is accepted from evidence, without a second ref command or report.
func TestApplyObservedCandidateRecordsAcceptanceWithoutSecondEffect(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()

	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "apply-candidate")
	entry, err := harness.Service().Prepare(context.Background(), git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidate,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "applyCandidate1",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if entry.State != git.JournalIntended {
		t.Fatalf("prepared entry state = %q; want intended", entry.State)
	}
	// Simulate the native apply having happened while the acceptance write was
	// lost: the ref already equals the journaled candidate.
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", candidate)

	applied, err := harness.Service().Apply(context.Background(), entry)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.State != git.JournalAccepted {
		t.Fatalf("applied state = %q; want accepted", applied.State)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state = %q; want accepted", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d: Apply never reports", got, baselineEvents)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery = %+v; want a reported entry", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t)
}

// TestReconcileObservationFailureLeavesIntentRecoverable proves that when
// repository evidence is unavailable DURING recovery, the durable intent must
// not be turned into a terminal refusal or a report. After the repository
// becomes observable again, the candidate is accepted and reported exactly
// once.
func TestReconcileObservationFailureLeavesIntentRecoverable(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unobservable-intent")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil || entry.State != git.JournalIntended {
		t.Fatalf("journal entry = %+v; want intended", entry)
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.HideRepository(t, recoveryfix.Repository)
	recovery, err := harness.Service().Recover(context.Background(), entry.SourceEventID)
	if err == nil {
		t.Fatalf("recovery without repository evidence = %+v; want an observation error", recovery)
	}
	if recovery.Reported {
		t.Fatalf("recovery without evidence = %+v; must not report", recovery)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "intended" {
		t.Fatalf("raw journal state without evidence = %q; want intended", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count without evidence = %d; want %d", got, baselineEvents)
	}
	restore()

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery after the repository returned = %+v; want reported", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileObservationFailureLeavesUncertainRecoverable repeats the
// observation failure for an already uncertain entry: no terminal state is
// invented and nothing is reported while evidence is unavailable.
func TestReconcileObservationFailureLeavesUncertainRecoverable(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unobservable-uncertain")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, candidate, "uncertain observed state")
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if first := recoverEntry(t, harness, entry.SourceEventID); first.Outcome != git.RecoveryUnknown ||
		rawJournalState(t, harness, entry.SourceEventID) != "uncertain" {
		t.Fatalf("first recovery = %+v; want uncertain", first)
	}

	restore := harness.HideRepository(t, recoveryfix.Repository)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery without repository evidence returned no error")
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "uncertain" {
		t.Fatalf("raw journal state without evidence = %q; want uncertain", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count without evidence = %d; want %d", got, baselineEvents)
	}
	restore()

	again := recoverEntry(t, harness, entry.SourceEventID)
	if again.Outcome != git.RecoveryUnknown || rawJournalState(t, harness, entry.SourceEventID) != "uncertain" {
		t.Fatalf("recovery after the repository returned = %+v; want the same uncertain state", again)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count after the uncertain recovery = %d; want %d", got, baselineEvents)
	}

	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", candidate)
	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("final recovery = %+v; want reported", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the candidate was observed = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestReconcileObservationFailureThenBaseRefuses proves the base-oid case: an
// observation failure must not pre-empt the refusal. Once the repository is
// observable and the ref is still the journaled base, recovery refuses and
// reports nothing.
func TestReconcileObservationFailureThenBaseRefuses(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unobservable-base")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	refsBefore := harness.Refs(t, recoveryfix.Repository)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.HideRepository(t, recoveryfix.Repository)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery without repository evidence returned no error")
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "intended" {
		t.Fatalf("raw journal state without evidence = %q; want intended", state)
	}
	restore()

	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryNotApplied || recovery.Entry.State != git.JournalRefused || recovery.Reported {
		t.Fatalf("recovery after the base was observed = %+v; want refused and unreported", recovery)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	if after := harness.Refs(t, recoveryfix.Repository); after != refsBefore {
		t.Fatalf("refused recovery changed refs:\nbefore:\n%s\nafter:\n%s", refsBefore, after)
	}
	harness.RequireNoSecrets(t, stderr)
}
