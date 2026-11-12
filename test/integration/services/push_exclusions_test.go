//go:build integration

package services

// Push trigger: the two exclusions the trigger processor must honour on the
// real composition.
//
//   - An execution-originated push is excluded across every subscribed
//     pipeline, not just the one under observation. Two pipelines subscribe to
//     the same ref; the execution push must start neither, and the later human
//     push must start each exactly once with the human inputs.
//   - A ref deletion stays refused and starts nothing. The subscription is
//     proven live by the positive control push that follows.
//
// Both scenarios assert only durable facts: the journal, the verified report
// event, the execution list and the admitted execution bytes.

import (
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestPushExecutionOriginatedExcludedAcrossPipelines proves one
// execution-originated push starts no pipeline, even when several pipelines
// subscribe to the pushed ref.
func TestPushExecutionOriginatedExcludedAcrossPipelines(t *testing.T) {
	fixture := startPushHarness(t)

	const ref = "refs/heads/work/execution-excluded"
	services := []struct {
		pipelineID string
		serviceID  string
	}{
		{pipelineID: "excluded-one", serviceID: "excluded-one-starter"},
		{pipelineID: "excluded-two", serviceID: "excluded-two-starter"},
	}
	documents := make(map[string][]byte, len(services))
	applied := make(map[string]api.PipelineApplyResult, len(services))
	for _, entry := range services {
		fixture.createPushService(t, entry.serviceID)
	}

	directory := fixture.clone(t, fixture.engineerToken, "exclusions")
	executionCommit := fixture.commit(t, directory, "execution-originated push")
	humanCommit := fixture.commit(t, directory, "human push")

	for _, entry := range services {
		documents[entry.pipelineID] = pushPipelineDocument(fixture.git.ProjectResourceID,
			entry.serviceID, fixture.Image.Reference, ref, humanCommit)
		applied[entry.pipelineID] = fixture.applyPushPipeline(t, entry.pipelineID, documents[entry.pipelineID])
	}

	// A live execution pushes the subscribed ref. Its report is correlated with
	// the execution and must select nothing, for either pipeline.
	executionID, executionToken := fixture.startPushExecution(t, "execution-pusher")
	if stderr, code := fixture.push(directory, executionToken, executionCommit+":"+ref); code != 0 {
		t.Fatalf("execution push %s: exit %d: %s", ref, code, stderr)
	}
	executionEntry := fixture.waitPushJournal(t, ref, executionCommit)
	executionReport := fixture.waitPushReportEvent(t, executionEntry.VerificationID)
	if executionReport.CorrelationID != executionID {
		t.Fatalf("execution push report correlation = %q; want the pusher execution %q",
			executionReport.CorrelationID, executionID)
	}

	// The human push of the same ref is the positive control: it must start
	// each subscribed pipeline exactly once, with the human commit.
	if stderr, code := fixture.push(directory, fixture.engineerToken, humanCommit+":"+ref); code != 0 {
		t.Fatalf("human push %s: exit %d: %s", ref, code, stderr)
	}
	humanEntry := fixture.waitPushJournal(t, ref, humanCommit)
	humanReport := fixture.waitPushReportEvent(t, humanEntry.VerificationID)
	if humanReport.CorrelationID != "" {
		t.Fatalf("human push report correlation = %q; want none", humanReport.CorrelationID)
	}

	for pipelineID, appliedPipeline := range applied {
		executions := fixture.waitPushExecutionCount(t, appliedPipeline.PipelineID, 1)
		t.Cleanup(func() { fixture.stopPushExecution(t, executions[0].ExecutionID) })
		fixture.waitPushExecutionSucceeded(t, executions[0].ExecutionID)
		if admitted := fixture.executionSnapshotYAML(t, executions[0].ExecutionID); string(admitted) != string(documents[pipelineID]) {
			t.Fatalf("pipeline %s admitted YAML = %q; want the human push's first selection %q",
				pipelineID, admitted, documents[pipelineID])
		}
		queued := fixture.queuedEvents(t, executions[0].ExecutionID)
		if len(queued) != 1 || queued[0].CorrelationID != humanReport.ID {
			t.Fatalf("pipeline %s queued events = %+v; want exactly one correlated with the human report %s",
				pipelineID, queued, humanReport.ID)
		}
		if remaining := fixture.pushPipelineExecutions(t, appliedPipeline.PipelineID); len(remaining) != 1 {
			t.Fatalf("pipeline %s has %d executions; want only the human push's one",
				pipelineID, len(remaining))
		}
	}
}

// TestPushRefusedDeleteStartsNothing proves a refused ref deletion starts no
// pipeline, cannot move the ref, and leaves the subscription live for a later
// positive-control push.
func TestPushRefusedDeleteStartsNothing(t *testing.T) {
	fixture := startPushHarness(t)

	const (
		ref       = "refs/heads/work/refused-delete"
		serviceID = "refused-delete-starter"
	)
	fixture.createPushService(t, serviceID)

	directory := fixture.clone(t, fixture.engineerToken, "delete")
	seedCommit := fixture.commit(t, directory, "delete seed")
	if stderr, code := fixture.push(directory, fixture.engineerToken, seedCommit+":"+ref); code != 0 {
		t.Fatalf("seed push %s: exit %d: %s", ref, code, stderr)
	}
	seedEntry := fixture.waitPushJournal(t, ref, seedCommit)
	if seedEntry.VerificationID == "" {
		t.Fatal("the seed push has no verification identity")
	}

	// Apply the subscription after the seed so the seed itself is not a
	// candidate; only the refused deletion and the positive control follow.
	candidateCommit := fixture.commit(t, directory, "delete positive control")
	document := pushPipelineDocument(fixture.git.ProjectResourceID, serviceID,
		fixture.Image.Reference, ref, candidateCommit)
	applied := fixture.applyPushPipeline(t, "refused-delete", document)

	if stderr, code := fixture.push(directory, fixture.engineerToken, ":"+ref); code == 0 {
		t.Fatalf("ref deletion %s succeeded; want the ref-delete refusal: %s", ref, stderr)
	}
	if observed := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); observed != seedCommit {
		t.Fatalf("ref %s moved to %q on a refused delete; want %q", ref, observed, seedCommit)
	}
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.Ref != ref {
			continue
		}
		if entry.Candidate != seedCommit {
			t.Fatalf("refused delete left a journal entry %+v; want no journal representation", entry)
		}
		if entry.State != git.JournalReported {
			t.Fatalf("seed journal entry state = %q; want reported", entry.State)
		}
	}
	if executions := fixture.pushPipelineExecutions(t, applied.PipelineID); len(executions) != 0 {
		t.Fatalf("pipeline %s has %d executions after a refused delete; want none", applied.PipelineID, len(executions))
	}

	// Positive control: the same subscription starts exactly one execution for
	// the next real update. Event-order processing means the refused deletion
	// would have settled before this execution exists.
	if stderr, code := fixture.push(directory, fixture.engineerToken, candidateCommit+":"+ref); code != 0 {
		t.Fatalf("positive control push %s: exit %d: %s", ref, code, stderr)
	}
	entry := fixture.waitPushJournal(t, ref, candidateCommit)
	report := fixture.waitPushReportEvent(t, entry.VerificationID)
	if report.CorrelationID != "" {
		t.Fatalf("positive control report correlation = %q; want none", report.CorrelationID)
	}
	executions := fixture.waitPushExecutionCount(t, applied.PipelineID, 1)
	t.Cleanup(func() { fixture.stopPushExecution(t, executions[0].ExecutionID) })
	fixture.waitPushExecutionSucceeded(t, executions[0].ExecutionID)
	if admitted := fixture.executionSnapshotYAML(t, executions[0].ExecutionID); string(admitted) != string(document) {
		t.Fatalf("admitted YAML = %q; want the positive control's first selection %q", admitted, document)
	}
	if executions := fixture.pushPipelineExecutions(t, applied.PipelineID); len(executions) != 1 {
		t.Fatalf("pipeline %s has %d executions; want exactly the positive control's one",
			applied.PipelineID, len(executions))
	}
}
