//go:build integration

package services

// Push trigger: a late verified report keeps its verification and attribution.
//
// The real Git service verifies the push and journals the accepted effect, but
// its report call is held in a labelled transport seam until the pushing human's
// OIDC session is expired. The report is then delivered with the service
// credential; the accepted resource.verified event must still cite the original
// verification as its causation (never a report-body value), carry no execution
// correlation for a human push, and preserve the verified actor on the
// verification row. The trigger processor must start exactly one execution with
// the pushed inputs and the automatic service as its initiator.
//
// The hold is a transport seam, not a fabricated success: the product performs
// every verification, journal, report and start commit.

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestPushLateVerifiedReportKeepsVerificationAndAttribution proves that an
// accepted effect reported after the actor credential expired keeps the
// verification identity and the human attribution end to end.
func TestPushLateVerifiedReportKeepsVerificationAndAttribution(t *testing.T) {
	hold := newReportHold()
	fixture := startPushHarness(t, factory.WithGitTransport(hold.transport))

	const (
		ref        = "refs/heads/work/late-report"
		serviceID  = "late-report-starter"
		pipelineID = "late-report"
	)
	fixture.createPushService(t, serviceID)

	directory := fixture.clone(t, fixture.engineerToken, "late-report")
	commit := fixture.commit(t, directory, "late verified report")

	document := pushPipelineDocument(fixture.git.ProjectResourceID, serviceID,
		fixture.Image.Reference, ref, commit)
	applied := fixture.applyPushPipeline(t, pipelineID, document)

	// Hold the post-receive report before the push starts. The push command
	// blocks in the Git service's report call while the verified effect and its
	// journal entry are already durable.
	hold.arm()
	done := make(chan pushResult, 1)
	go func() {
		stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref)
		done <- pushResult{stderr: stderr, code: code}
	}()
	hold.waitReached(t)

	accepted := fixture.waitPushJournalState(t, ref, commit, git.JournalAccepted)
	if accepted.VerificationID == "" {
		t.Fatal("the accepted journal entry carries no verification identity")
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), accepted.VerificationID); err != nil {
		t.Fatalf("read report event before delivery: %v", err)
	} else if ok {
		t.Fatal("the report event was committed before the held report was released")
	}

	// Advance the real OIDC provider clock past the engineer's default
	// one-hour session. The report is delivered with the service credential, so
	// it must be accepted even though the actor can no longer authenticate.
	fixture.OIDC.Provider.Clock().Advance(2 * time.Hour)
	hold.releaseAll()

	result := <-done
	if result.code != 0 {
		t.Fatalf("late-report push %s: exit %d: %s", ref, result.code, result.stderr)
	}

	reported := fixture.waitPushJournal(t, ref, commit)
	if reported.VerificationID != accepted.VerificationID {
		t.Fatalf("reported journal verification = %q; want the accepted verification %q",
			reported.VerificationID, accepted.VerificationID)
	}
	report := fixture.waitPushReportEvent(t, accepted.VerificationID)
	if report.Type != "resource.verified" || report.ReportType != "git.ref.updated" {
		t.Fatalf("late report event = %+v; want a verified git.ref.updated report", report)
	}
	if report.CausationID != accepted.VerificationID {
		t.Fatalf("report causation = %q; want the original verification %q",
			report.CausationID, accepted.VerificationID)
	}
	if report.CorrelationID != "" {
		t.Fatalf("human report correlation = %q; want no execution correlation", report.CorrelationID)
	}
	if report.SubjectID != fixture.git.ProjectResourceID {
		t.Fatalf("report subject = %q; want the pushed repository resource %q",
			report.SubjectID, fixture.git.ProjectResourceID)
	}
	if report.SourceKind != "service" {
		t.Fatalf("report source kind = %q; want the enforcing service", report.SourceKind)
	}

	verification := fixture.pushVerification(t, accepted.VerificationID)
	if verification.Operation != "git.ref.update" || verification.Result != "approved" {
		t.Fatalf("verification = %+v; want an approved git.ref.update", verification)
	}
	if verification.ActorID != fixture.engineerPrincipal {
		t.Fatalf("verification actor = %q; want the pushing engineer %q",
			verification.ActorID, fixture.engineerPrincipal)
	}
	if verification.ExecutionID != "" {
		t.Fatalf("human verification execution = %q; want empty", verification.ExecutionID)
	}

	// The expired human credential is genuinely unusable now.
	_, publicError := fixture.REST(fixture.engineerToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
	}, nil, "")
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("expired engineer credential refused with %q; want %q", publicError.Code, api.CodeUnauthenticated)
	}

	executions := fixture.waitPushExecutionCount(t, applied.PipelineID, 1)
	t.Cleanup(func() { fixture.stopPushExecution(t, executions[0].ExecutionID) })
	fixture.waitPushExecutionSucceeded(t, executions[0].ExecutionID)

	if admitted := fixture.executionSnapshotYAML(t, executions[0].ExecutionID); string(admitted) != string(document) {
		t.Fatalf("late report execution admitted YAML = %q; want the first selected bytes %q", admitted, document)
	}
	queued := fixture.queuedEvents(t, executions[0].ExecutionID)
	if len(queued) != 1 {
		t.Fatalf("execution %s has %d execution.queued events; want exactly one",
			executions[0].ExecutionID, len(queued))
	}
	if queued[0].Source.PrincipalID != serviceID {
		t.Fatalf("execution.queued source = %q; want the automatic service %q",
			queued[0].Source.PrincipalID, serviceID)
	}
	if queued[0].CorrelationID != report.ID {
		t.Fatalf("execution.queued correlation = %q; want the report event %q",
			queued[0].CorrelationID, report.ID)
	}
	if remaining := fixture.pushPipelineExecutions(t, applied.PipelineID); len(remaining) != 1 {
		t.Fatalf("pipeline %s has %d executions; want exactly one", applied.PipelineID, len(remaining))
	}
}

// pushResult is one Git push outcome collected off the blocking goroutine.
type pushResult struct {
	stderr string
	code   int
}
