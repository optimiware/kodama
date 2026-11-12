package git_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	git "github.com/optimiware/kodama/internal/userland/git"
)

// TestRecoverCrashBeforeEffect pins the crash-before-effect boundary: an intent
// is durable, the ref never moved, and recovery proves not-applied without
// running a repository command or reporting an effect.
func TestRecoverCrashBeforeEffect(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if entry.State != git.JournalIntended {
		t.Fatalf("prepared state = %q; want intended", entry.State)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("Prepare performed a ref effect")
	}

	result, err := f.service.Recover(context.Background(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != git.RecoveryNotApplied {
		t.Fatalf("outcome = %q; want not_applied", result.Outcome)
	}
	if result.Entry.State != git.JournalRefused {
		t.Fatalf("recovered state = %q; want refused", result.Entry.State)
	}
	if result.Reported {
		t.Fatal("not-applied recovery reported an effect")
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("recovery ran %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 0 {
		t.Fatalf("not-applied recovery reported %d times", len(calls))
	}

	// A second recovery is a settled no-op.
	again, err := f.service.Recover(context.Background(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if again.Outcome != "" || again.Entry.State != git.JournalRefused {
		t.Fatalf("settled recovery = %q,%q; want empty outcome and refused", again.Outcome, again.Entry.State)
	}
}

// TestRecoverLostResponseAfterEffect pins the lost-response-after-effect
// boundary: the compare-and-swap happened but the response was lost, so
// recovery observes the candidate, records acceptance and reports exactly once
// without ever pushing again.
func TestRecoverLostResponseAfterEffect(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	f.executor.failAfter("update-ref", errors.New("connection reset after effect"))
	applied, err := f.service.Apply(context.Background(), entry)
	if !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("Apply error = %v; want ErrUncertain", err)
	}
	if applied.State != git.JournalUncertain {
		t.Fatalf("state after lost response = %q; want uncertain", applied.State)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("update-ref commands = %d; want one", f.executor.updateRefCount())
	}
	if got := f.executor.commit("shop/web", testRef); got != testCandidate {
		t.Fatalf("ref = %q; want the applied candidate", got)
	}

	result, err := f.service.Recover(context.Background(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != git.RecoveryApplied {
		t.Fatalf("outcome = %q; want applied", result.Outcome)
	}
	if result.Entry.State != git.JournalReported || !result.Reported {
		t.Fatalf("recovered state = %q,%v; want reported", result.Entry.State, result.Reported)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("recovery re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
	reports := f.verifier.reportRequests()
	if len(reports) != 1 {
		t.Fatalf("recovery reports = %d; want one", len(reports))
	}
	accepted := result.Entry
	accepted.State = git.JournalAccepted
	want, err := accepted.ReportRequest()
	if err != nil {
		t.Fatalf("ReportRequest: %v", err)
	}
	if !reflect.DeepEqual(reports[0], want) {
		t.Fatalf("recovery report = %+v; want %+v", reports[0], want)
	}
}

// TestRecoverAcceptanceRecordLost pins the crash-after-effect-before-acceptance
// boundary: the ref moved, the acceptance write was lost, and recovery settles
// the still-intended entry from observation.
func TestRecoverAcceptanceRecordLost(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	f.journal.failRecordWhen(func(candidate git.JournalEntry) bool {
		return candidate.State == git.JournalAccepted
	})
	if _, err := f.service.Apply(context.Background(), entry); !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("Apply error = %v; want ErrUncertain after a lost acceptance write", err)
	}
	if state, ok := f.journal.state(entry.SourceEventID); !ok || state != git.JournalUncertain {
		t.Fatalf("durable state = %q,%v; want uncertain", state, ok)
	}

	f.journal.mu.Lock()
	f.journal.recordErr = nil
	f.journal.mu.Unlock()

	result, err := f.service.Recover(context.Background(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != git.RecoveryApplied || result.Entry.State != git.JournalReported {
		t.Fatalf("recovery = %q,%q; want applied,reported", result.Outcome, result.Entry.State)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("recovery re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("recovery reports = %d; want one", len(calls))
	}
}

// TestRecoverUnknownNeverRetries pins that a third observed commit is unknown,
// not proof of either outcome: the entry stays uncertain, no report is sent and
// no update is replayed.
func TestRecoverUnknownNeverRetries(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	f.executor.failAfter("update-ref", errors.New("timeout"))
	if _, err := f.service.Apply(context.Background(), entry); !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("Apply error = %v; want ErrUncertain", err)
	}

	f.observer.overrides = map[string]string{"shop/web|" + testRef: "third-commit"}
	for attempt := range 2 {
		result, err := f.service.Recover(context.Background(), entry.SourceEventID)
		if err != nil {
			t.Fatalf("Recover attempt %d: %v", attempt, err)
		}
		if result.Outcome != git.RecoveryUnknown {
			t.Fatalf("outcome = %q; want unknown", result.Outcome)
		}
		if result.Entry.State != git.JournalUncertain {
			t.Fatalf("state = %q; want uncertain", result.Entry.State)
		}
		if result.Reported {
			t.Fatal("unknown recovery reported an effect")
		}
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("unknown recovery re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 0 {
		t.Fatalf("unknown recovery reported %d times", len(calls))
	}
}

// TestReportRetryIsReportOnly pins that a failed report retries the identical
// report and never the ref update.
func TestReportRetryIsReportOnly(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	accepted, err := f.service.Apply(context.Background(), entry)
	if err != nil || accepted.State != git.JournalAccepted {
		t.Fatalf("Apply = %q,%v; want accepted", accepted.State, err)
	}

	f.verifier.failNextReports(1)
	if _, err := f.service.Report(context.Background(), accepted); err == nil {
		t.Fatal("Report succeeded despite a transport failure")
	}
	if state, _ := f.journal.state(entry.SourceEventID); state != git.JournalAccepted {
		t.Fatalf("state after failed report = %q; want accepted", state)
	}

	reported, err := f.service.Report(context.Background(), accepted)
	if err != nil {
		t.Fatalf("Report retry: %v", err)
	}
	if err := reported.Validate(); err != nil {
		t.Fatalf("report response refused: %v", err)
	}
	if state, _ := f.journal.state(entry.SourceEventID); state != git.JournalReported {
		t.Fatalf("state after retry = %q; want reported", state)
	}
	reports := f.verifier.reportRequests()
	if len(reports) != 2 {
		t.Fatalf("report calls = %d; want two", len(reports))
	}
	if !reflect.DeepEqual(reports[0], reports[1]) {
		t.Fatalf("report retry changed content: %+v vs %+v", reports[0], reports[1])
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("report retry re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}

	settled := accepted
	settled.State = git.JournalReported
	response, err := f.service.Report(context.Background(), settled)
	if err != nil {
		t.Fatalf("Report on reported entry: %v", err)
	}
	if !response.Replayed {
		t.Fatalf("already-reported response = %+v; want Replayed", response)
	}
	if calls := f.verifier.reportRequests(); len(calls) != 2 {
		t.Fatalf("already-reported entry called the client again: %d", len(calls))
	}
}

// TestReportRecordFailureRetriesReporting pins that a lost "reported" journal
// write keeps the accepted entry and a retry reports again with identical
// content; the ref is never touched.
func TestReportRecordFailureRetriesReporting(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	accepted, err := f.service.Apply(context.Background(), entry)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	f.journal.failRecordWhen(func(candidate git.JournalEntry) bool {
		return candidate.State == git.JournalReported
	})
	if _, err := f.service.Report(context.Background(), accepted); err == nil {
		t.Fatal("Report succeeded despite a lost reported write")
	}
	if state, _ := f.journal.state(entry.SourceEventID); state != git.JournalAccepted {
		t.Fatalf("state = %q; want accepted", state)
	}

	f.journal.mu.Lock()
	f.journal.recordErr = nil
	f.journal.mu.Unlock()

	if _, err := f.service.Report(context.Background(), accepted); err != nil {
		t.Fatalf("Report retry: %v", err)
	}
	reports := f.verifier.reportRequests()
	if len(reports) != 2 || !reflect.DeepEqual(reports[0], reports[1]) {
		t.Fatalf("report retry = %+v; want identical retries", reports)
	}
	if state, _ := f.journal.state(entry.SourceEventID); state != git.JournalReported {
		t.Fatalf("state = %q; want reported", state)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("report retry re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
}

// TestRecoverAcceptedReportsWithoutRefEffect pins report-only recovery of a
// durably accepted entry.
func TestRecoverAcceptedReportsWithoutRefEffect(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := f.service.Apply(context.Background(), entry); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	result, err := f.service.Recover(context.Background(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != git.RecoveryApplied || !result.Reported {
		t.Fatalf("recovery = %q,%v; want applied,reported", result.Outcome, result.Reported)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("recovery re-pushed: %d update-ref commands", f.executor.updateRefCount())
	}
	if calls := f.verifier.reportRequests(); len(calls) != 1 {
		t.Fatalf("recovery reports = %d; want one", len(calls))
	}
}

// TestRecoverSettledRefusalIsNoOp pins that a refused entry is never reported
// or re-applied by recovery.
func TestRecoverSettledRefusalIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, humanScript("prn-view", "viewer"))
	request := updateRequestFor("shop/web", "refs/heads/work/x", "", "work-1", testSourceEventID)
	refused, err := f.service.Prepare(context.Background(), request)
	if !errors.Is(err, git.ErrPolicyDenied) {
		t.Fatalf("Prepare error = %v; want ErrPolicyDenied", err)
	}
	if refused.State != git.JournalRefused {
		t.Fatalf("prepared state = %q; want refused", refused.State)
	}

	result, err := f.service.Recover(context.Background(), refused.SourceEventID)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if result.Outcome != "" || result.Entry.State != git.JournalRefused || result.Reported {
		t.Fatalf("settled refusal recovery = %+v", result)
	}
	if f.executor.updateRefCount() != 0 || len(f.verifier.reportRequests()) != 0 {
		t.Fatal("settled refusal caused an effect or report")
	}
}

// TestApplyIsIdempotent pins that applying an already accepted intent settles
// from the durable entry instead of performing a second ref update.
func TestApplyIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	accepted, err := f.service.Apply(context.Background(), entry)
	if err != nil || accepted.State != git.JournalAccepted {
		t.Fatalf("first Apply = %q,%v", accepted.State, err)
	}
	again, err := f.service.Apply(context.Background(), entry)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if again.State != git.JournalAccepted {
		t.Fatalf("second Apply state = %q; want accepted", again.State)
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("idempotent Apply ran %d update-ref commands", f.executor.updateRefCount())
	}
}

// TestPrepareRefusesZeroClock pins the injected clock: a zero time cannot be
// journaled and no ref effect happens.
func TestPrepareRefusesZeroClock(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)
	f.clock.now = time.Time{}

	if _, err := f.service.Prepare(context.Background(), f.updateRequest()); err == nil {
		t.Fatal("Prepare accepted a zero clock time")
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("zero-clock attempt performed a ref effect")
	}
	if state, ok := f.journal.state(testSourceEventID); ok {
		t.Fatalf("zero-clock attempt persisted a %q entry", state)
	}
}

// TestPrepareDuplicateIntentIsNotReverified pins that a second Prepare for the
// same stable source event returns the durable intent without a second
// verification.
func TestPrepareDuplicateIntentIsNotReverified(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	first, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	second, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if second.State != first.State || second.VerificationID != first.VerificationID {
		t.Fatalf("second Prepare = %+v; want the durable intent", second)
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("duplicate Prepare verified %d times; want one", len(calls))
	}
}

// TestApplyObserverFailureIsUncertain pins that an unobservable ref is
// uncertain, never a confirmed success or a silent retry.
func TestApplyObserverFailureIsUncertain(t *testing.T) {
	f := newFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, testBase)
	f.executor.setAncestor("shop/web", testBase, testCandidate, true)

	entry, err := f.service.Prepare(context.Background(), f.updateRequest())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	f.observer.err = errors.New("repository unavailable")
	if _, err := f.service.Apply(context.Background(), entry); !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("Apply error = %v; want ErrUncertain", err)
	}
	if state, _ := f.journal.state(entry.SourceEventID); state != git.JournalUncertain {
		t.Fatalf("state = %q; want uncertain", state)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("unobservable Apply ran %d update-ref commands", f.executor.updateRefCount())
	}
}
