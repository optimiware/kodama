//go:build integration

package factorytest

// Actual protected-report replay negatives.
//
// These tests extend the protected golden path with the reporting half of the
// refusal matrix. The scenario itself is the golden-path run: one real
// task-associated execution, one real protected push to refs/heads/main, one
// reported journal entry, one scoped PostgreSQL verification row and one
// linked resource.verified/git.ref.updated event. Every test below reuses that
// actual run's journal source-event identity instead of inventing a report
// identity, so the assertions are about the served `resource.report`
// operation and its durable receipts/events.
//
// Covered:
//
//   - an exact replay after execution completion returns the original
//     verification and event identities even though the execution credential
//     has been revoked; the reporting service credential remains authoritative
//     and a revoked one is refused;
//   - changed subject content under the same source-event identity is refused
//     and alters no durable fact;
//   - a different genuine verification of the same effect is refused as changed
//     receipt content (the second verification is a real actor.verify of the
//     identical subject through a live execution, so linkage cannot mask the
//     deduplication refusal);
//   - caller-supplied causation/actor fields are refused by the strict public
//     schema, and the accepted event causation stays derived from the
//     verification.
//
// The changed-outer-envelope-key case follows
// docs/specs/08-operations-events.md: the report deduplication identity is
// (service, source-event-ID); identical content under the same source-event ID
// converges on the existing report whatever the outer request key, and changed
// content is refused. The test at the end pins the converging half; the
// changed-content halves are the ChangedSubject and ChangedVerification tests.

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// reportReplaySecondPipeline is the fixture-unique pipeline name of the
	// second live execution used to obtain an otherwise-valid verification of
	// the golden effect.
	reportReplaySecondPipeline = "protected-report-replay-second-verification"
	// reportReplaySecondAction is the blocking container action of that
	// execution. It keeps the execution actor live while the test verifies.
	reportReplaySecondAction = "hold"
	// reportReplaySecondRequest is the approval request action name.
	reportReplaySecondRequest = "review"
)

// reportReplayDurable is the durable reporting state one refused or replayed
// attempt must leave untouched.
type reportReplayDurable struct {
	ref          string
	journalCount int
	eventID      string
	receipts     int64
}

// TestProtectedFactoryReportReplayPreservesIdentity proves the exact replay is
// receipt-first after the execution is terminal: the original verification and
// event identities come back, one journaled effect and one linked event
// survive, a revoked execution credential is irrelevant to reporting the
// already accepted effect, and reporting-service revocation is authoritative.
func TestProtectedFactoryReportReplayPreservesIdentity(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	if !evidence.reportReplayed {
		t.Fatalf("the scenario's exact resource.report replay did not converge")
	}
	if evidence.replayEvent.ID != evidence.reportEvent.ID ||
		evidence.replayEvent.CausationID != evidence.verification.ID {
		t.Fatalf("scenario replay event = %+v; want the original event %q caused by verification %q",
			evidence.replayEvent, evidence.reportEvent.ID, evidence.verification.ID)
	}
	serviceToken := reportReplayServiceToken(t, fixture)
	before := reportReplayDurableFacts(t, fixture, evidence)
	if before.journalCount != 1 {
		t.Fatalf("reported protected journal entries = %d; want exactly one", before.journalCount)
	}
	if before.ref != evidence.candidate {
		t.Fatalf("protected main = %q; want the candidate %q", before.ref, evidence.candidate)
	}

	// The execution is terminal and its credential material is cleared, so the
	// mounted integrate token is no longer a usable bearer. This is the
	// actor-token revocation the report path must ignore: the effect was
	// already accepted and remains reportable by the service.
	if cleared, err := fixture.ExecutionCredentialCleared(t.Context(), evidence.executionID); err != nil {
		t.Fatalf("read execution credential clearing: %v", err)
	} else if !cleared {
		t.Fatalf("execution %s still carries credential material after success; want it cleared",
			evidence.executionID)
	}
	_, publicError := fixture.REST(evidence.integrateToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: evidence.executionID,
	}, nil, "")
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("the terminal execution credential: execution.inspect code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeUnauthenticated)
	}

	// Repeat the exact report after completion. It must converge on the same
	// verification and event without repeating the effect.
	request := reportReplayRequest(t, evidence, evidence.verification.ID, reportReplayGoldenSubject(t, evidence))
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("post-completion exact replay refused: %s (%s)", publicError.Code, publicError.Message)
	}
	if !response.Replayed {
		t.Fatalf("post-completion exact replay was not reported as replayed: %+v", response)
	}
	if response.VerificationID != evidence.verification.ID || response.EventID != evidence.reportEvent.ID {
		t.Fatalf("post-completion replay named verification=%q event=%q; want %q/%q",
			response.VerificationID, response.EventID, evidence.verification.ID, evidence.reportEvent.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the post-completion exact replay")
	event, ok, err := fixture.git.ReportEvent(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the linked report event: %v", err)
	}
	if !ok || event.ID != evidence.reportEvent.ID {
		t.Fatalf("linked report event after replay = %+v present=%t; want the original %q",
			event, ok, evidence.reportEvent.ID)
	}

	// Revoke the reporting service credential. The report path re-authorizes
	// the reporting service on every attempt, so a revoked credential must be
	// refused even though it could previously report this effect.
	revoked := reportReplayRevokeService(t, fixture)
	_, publicError = reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("revoked reporting service: resource.report code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeUnauthenticated)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the revoked-service report refusal")

	// Positive control: a freshly issued credential reports the same accepted
	// effect again. The refusal above was the credential, not a broken report.
	fresh := reportReplayIssueService(t, fixture, revoked)
	response, publicError = reportReplayCall(t, fixture, fresh, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("fresh reporting service credential refused the exact replay: %s (%s)",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed || response.EventID != evidence.reportEvent.ID {
		t.Fatalf("fresh-credential exact replay = %+v; want the original replayed event %q",
			response, evidence.reportEvent.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the fresh-credential exact replay")
}

// TestProtectedFactoryReportReplayRejectsChangedSubject proves altered subject
// content under the existing source-event identity is refused and changes no
// durable fact, then that the exact report still converges.
func TestProtectedFactoryReportReplayRejectsChangedSubject(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	serviceToken := reportReplayServiceToken(t, fixture)
	before := reportReplayDurableFacts(t, fixture, evidence)

	subject := reportReplayGoldenSubject(t, evidence)
	changed := false
	for index := range subject.Fields {
		if subject.Fields[index].Name != "candidate" {
			continue
		}
		// A valid 40-hex value, so the refusal is the subject mismatch and not
		// a schema error.
		subject.Fields[index].Value = evidence.base
		changed = true
	}
	if !changed {
		t.Fatalf("the accepted verification subject carries no candidate field: %+v", subject)
	}
	request := reportReplayRequest(t, evidence, evidence.verification.ID, subject)
	_, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != api.CodeForbidden {
		t.Fatalf("changed subject report code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeForbidden)
	}
	if publicError.Message != "report subject does not match the verification record" {
		t.Fatalf("changed subject report message = %q; want the exact linkage refusal",
			publicError.Message)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the changed-subject refusal")

	// Valid control: the exact report still converges after the refusal.
	request = reportReplayRequest(t, evidence, evidence.verification.ID, reportReplayGoldenSubject(t, evidence))
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("exact replay after the changed-subject refusal was refused: %s (%s)",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed || response.VerificationID != evidence.verification.ID ||
		response.EventID != evidence.reportEvent.ID {
		t.Fatalf("exact replay after the changed-subject refusal = %+v; want the original identities", response)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the changed-subject control replay")
}

// TestProtectedFactoryReportReplayRejectsChangedVerification substitutes a
// second genuine, otherwise-valid verification of the same effect under the
// original source-event identity. Linkage passes (same service, binding,
// resource, operation and subject), so only the report receipt content
// differs; the report must be refused as changed idempotency content and must
// not publish a second event.
func TestProtectedFactoryReportReplayRejectsChangedVerification(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	serviceToken := reportReplayServiceToken(t, fixture)
	before := reportReplayDurableFacts(t, fixture, evidence)

	second := reportReplaySecondVerification(t, fixture, evidence)
	if second.ID == evidence.verification.ID {
		t.Fatalf("the second verification reused the accepted verification id %q", second.ID)
	}
	if second.Result != "approved" || !second.Approved {
		t.Fatalf("second verification = %+v; want a genuine approved verification", second)
	}
	if second.ServiceID != evidence.verification.ServiceID ||
		second.BindingID != evidence.verification.BindingID ||
		second.ResourceID != evidence.verification.ResourceID ||
		second.Operation != evidence.verification.Operation {
		t.Fatalf("second verification linkage = %+v; want the same service/binding/resource/operation", second)
	}
	var secondSubject, acceptedSubject api.Subject
	if err := json.Unmarshal([]byte(second.Subject), &secondSubject); err != nil {
		t.Fatalf("decode the second verification subject: %v", err)
	}
	if err := json.Unmarshal([]byte(evidence.verification.Subject), &acceptedSubject); err != nil {
		t.Fatalf("decode the accepted verification subject: %v", err)
	}
	if !reflect.DeepEqual(secondSubject, acceptedSubject) {
		t.Fatalf("second verification subject = %+v; want the accepted effect subject %+v",
			secondSubject, acceptedSubject)
	}

	request := reportReplayRequest(t, evidence, second.ID, reportReplayGoldenSubject(t, evidence))
	_, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed verification report code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeChangedIdempotencyContent)
	}
	// The exact changed-content refusal the store returns; a linkage refusal
	// here would mean the second verification was not otherwise valid and the
	// deduplication ordering is unproven.
	if publicError.Message != "a receipt with the same caller key already holds different request content" {
		t.Fatalf("changed verification report message = %q; want the exact receipt-content refusal",
			publicError.Message)
	}
	if event, ok, err := fixture.git.ReportEvent(t.Context(), second.ID); err != nil {
		t.Fatalf("read an event for the second verification: %v", err)
	} else if ok {
		t.Fatalf("the refused changed-verification report published event %+v for verification %s",
			event, second.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the changed-verification refusal")

	// Valid control: the original exact report still converges.
	request = reportReplayRequest(t, evidence, evidence.verification.ID, reportReplayGoldenSubject(t, evidence))
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("exact replay after the changed-verification refusal was refused: %s (%s)",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed || response.EventID != evidence.reportEvent.ID {
		t.Fatalf("exact replay after the changed-verification refusal = %+v; want the original event %q",
			response, evidence.reportEvent.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the changed-verification control replay")
}

// TestProtectedFactoryReportRejectsCallerSuppliedCausation proves the raw
// public report schema refuses caller-owned causation/actor fields, and that
// the accepted event causation is the verification identity the Shell derived,
// never a caller value.
func TestProtectedFactoryReportRejectsCallerSuppliedCausation(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	serviceToken := reportReplayServiceToken(t, fixture)
	before := reportReplayDurableFacts(t, fixture, evidence)

	// The accepted causation is the verification and the correlation is the
	// accepting execution. These are derived from the record, not the body.
	if evidence.reportEvent.CausationID != evidence.verification.ID {
		t.Fatalf("accepted event causation = %q; want the verification %q",
			evidence.reportEvent.CausationID, evidence.verification.ID)
	}
	if evidence.reportEvent.CorrelationID != evidence.executionID {
		t.Fatalf("accepted event correlation = %q; want the execution %q",
			evidence.reportEvent.CorrelationID, evidence.executionID)
	}

	base := reportReplayRequestMap(t, reportReplayRequest(t, evidence,
		evidence.verification.ID, reportReplayGoldenSubject(t, evidence)))
	for _, field := range []struct {
		name  string
		value any
	}{
		{name: "causation_id", value: "caller-supplied-causation"},
		{name: "actor_id", value: "caller-supplied-actor"},
		{name: "principal_id", value: "caller-supplied-principal"},
		{name: "credential", value: "caller-supplied-credential"},
	} {
		input := make(map[string]any, len(base)+1)
		maps.Copy(input, base)
		input[field.name] = field.value
		_, publicError := fixture.REST(serviceToken, "resource.report", input, nil,
			evidence.protectedJournal.SourceEventID)
		if publicError.Code != api.CodeInvalidInput {
			t.Fatalf("caller-supplied %s: resource.report code = %q (%s); want %q",
				field.name, publicError.Code, publicError.Message, api.CodeInvalidInput)
		}
		if !strings.Contains(publicError.Message, "unknown field") ||
			!strings.Contains(publicError.Message, field.name) {
			t.Fatalf("caller-supplied %s: message = %q; want the strict-schema unknown-field refusal",
				field.name, publicError.Message)
		}
		requireReportReplayUnchanged(t, fixture, evidence, before,
			"after the caller-supplied "+field.name+" refusal")
	}

	// Valid control: the exact report still converges, so the schema refusals
	// did not damage the receipt.
	request := reportReplayRequest(t, evidence, evidence.verification.ID, reportReplayGoldenSubject(t, evidence))
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, evidence.protectedJournal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("exact replay after the caller-supplied refusals was refused: %s (%s)",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed || response.EventID != evidence.reportEvent.ID {
		t.Fatalf("exact replay after the caller-supplied refusals = %+v; want the original event %q",
			response, evidence.reportEvent.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the caller-supplied control replay")
}

// TestProtectedFactoryReportReplayChangedEnvelopeKeyConverges pins the
// specified behavior (docs/specs/08-operations-events.md): an unchanged report
// whose outer request idempotency key changes while the source event id (the
// report deduplication identity) stays the same converges on the original
// accepted report, exactly like an unchanged replay. Changed content under the
// same source event id is refused (see the ChangedSubject and
// ChangedVerification tests).
//
// Why the outer key does not matter: the served resource.report handler passes
// only SourceEventID into ReportCommand (internal/shell/app/ops/verify.go:60-74)
// and the report receipt key is derived from it
// (internal/shell/verify/report.go:334-341, CallerKey: command.SourceEventID);
// the specification requires (service, source-event-ID) report deduplication.
func TestProtectedFactoryReportReplayChangedEnvelopeKeyConverges(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	serviceToken := reportReplayServiceToken(t, fixture)
	before := reportReplayDurableFacts(t, fixture, evidence)

	request := reportReplayRequest(t, evidence, evidence.verification.ID, reportReplayGoldenSubject(t, evidence))
	changedKey := evidence.protectedJournal.SourceEventID + "-changed-outer-envelope-key"
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, changedKey)
	if publicError.Code != "" {
		t.Fatalf("changed-outer-key report refused: %s (%s); the pinned current behavior is convergence",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed {
		t.Fatalf("changed-outer-key report = %+v; current behavior is a replayed original", response)
	}
	if response.VerificationID != evidence.verification.ID || response.EventID != evidence.reportEvent.ID {
		t.Fatalf("changed-outer-key report named verification=%q event=%q; want the original %q/%q",
			response.VerificationID, response.EventID, evidence.verification.ID, evidence.reportEvent.ID)
	}
	requireReportReplayUnchanged(t, fixture, evidence, before, "after the changed-outer-key report")
}

// reportReplayServiceToken reads the mounted Git service credential.
func reportReplayServiceToken(t *testing.T, fixture *gitHarness) string {
	t.Helper()
	token, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the mounted Git service credential: %v", err)
	}
	return token
}

// reportReplayGoldenSubject decodes the accepted verification's effect
// subject.
func reportReplayGoldenSubject(t *testing.T, evidence protectedGoldenEvidence) api.Subject {
	t.Helper()
	var subject api.Subject
	if err := json.Unmarshal([]byte(evidence.verification.Subject), &subject); err != nil {
		t.Fatalf("decode the accepted verification subject %s: %v", evidence.verification.Subject, err)
	}
	if len(subject.Fields) == 0 {
		t.Fatalf("the accepted verification carries no reportable subject")
	}
	return subject
}

// reportReplayRequest renders one public report request from the actual run.
func reportReplayRequest(t *testing.T, evidence protectedGoldenEvidence, verificationID string, subject api.Subject) api.ReportRequest {
	t.Helper()
	return api.ReportRequest{
		ResourceID:     evidence.verification.ResourceID,
		SourceEventID:  evidence.protectedJournal.SourceEventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: verificationID,
		Subject:        subject,
	}
}

// reportReplayRequestMap renders one report request as the raw JSON object a
// caller could post, so extra members survive marshalling.
func reportReplayRequestMap(t *testing.T, request api.ReportRequest) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal the report request: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode the report request map: %v", err)
	}
	return fields
}

// reportReplayCall performs one resource.report attempt without failing on a
// refusal, so the exact public error can be asserted.
func reportReplayCall(
	t *testing.T,
	fixture *gitHarness,
	token string,
	request api.ReportRequest,
	key string,
) (api.ReportResponse, api.Error) {
	t.Helper()
	result, publicError := fixture.REST(token, "resource.report", request, nil, key)
	if publicError.Code != "" {
		return api.ReportResponse{}, publicError
	}
	var response api.ReportResponse
	fixture.Decode(result, &response)
	return response, api.Error{}
}

// reportReplayDurableFacts reads the exact durable reporting facts one refusal
// or replay must not alter.
func reportReplayDurableFacts(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence) reportReplayDurable {
	t.Helper()
	receipts, err := fixture.ReceiptCount(t.Context(), evidence.protectedJournal.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts: %v", err)
	}
	return reportReplayDurable{
		ref:          fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef),
		journalCount: protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef),
		eventID:      reportReplayGoldenEventID(t, fixture, evidence),
		receipts:     receipts,
	}
}

// reportReplayGoldenEventID reads the one event caused by the accepted
// verification and fails if it is missing or duplicated.
func reportReplayGoldenEventID(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence) string {
	t.Helper()
	event, ok, err := fixture.git.ReportEvent(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the accepted report event: %v", err)
	}
	if !ok {
		t.Fatalf("verification %s has no linked report event", evidence.verification.ID)
	}
	return event.ID
}

// requireReportReplayUnchanged asserts a refused or replayed attempt left every
// durable fact exactly as it was: protected ref, reported journal count,
// linked event identity and the report receipt count. The journal entry and the
// verification row are compared in full, not by count alone.
func requireReportReplayUnchanged(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence, before reportReplayDurable, label string) {
	t.Helper()
	if ref := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); ref != before.ref {
		t.Fatalf("protected main %s = %q; want the unchanged %q", label, ref, before.ref)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); count != before.journalCount {
		t.Fatalf("reported protected journal entries %s = %d; want the unchanged %d",
			label, count, before.journalCount)
	}
	if eventID := reportReplayGoldenEventID(t, fixture, evidence); eventID != before.eventID {
		t.Fatalf("linked report event %s = %q; want the unchanged %q", label, eventID, before.eventID)
	}
	receipts, err := fixture.ReceiptCount(t.Context(), evidence.protectedJournal.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts %s: %v", label, err)
	}
	if receipts != before.receipts {
		t.Fatalf("report receipts %s = %d; want the unchanged %d", label, receipts, before.receipts)
	}
	// The journal entry itself must keep the accepted candidate, base and
	// verification linkage.
	journal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, evidence.candidate)
	if journal.VerificationID != evidence.protectedJournal.VerificationID ||
		journal.SourceEventID != evidence.protectedJournal.SourceEventID ||
		journal.ExpectedBase != evidence.protectedJournal.ExpectedBase {
		t.Fatalf("protected journal %s = %+v; want the unchanged %+v", label, journal, evidence.protectedJournal)
	}
	afterRow := fixture.verificationByID(t, evidence.verification.ID)
	if afterRow.Result != evidence.verification.Result || afterRow.Approved != evidence.verification.Approved ||
		afterRow.Subject != evidence.verification.Subject {
		t.Fatalf("accepted verification %s = %+v; want the unchanged result/approval/subject", label, afterRow)
	}
}

// reportReplaySecondVerification obtains one genuine, otherwise-valid
// verification of the golden effect through the public actor.verify operation.
//
// The golden-path execution is terminal, so its actor credential is revoked by
// design and can never verify again. This helper starts a second, task-free
// execution in the same fixture, records one exact human pass for the same
// canonical five-field effect subject through execution.request.resolve, then
// verifies with that live execution's credential and the same Git service
// credential the real hook uses. The resulting row has the same service,
// binding, resource, operation and subject as the accepted protected
// verification and a different verification identity: linkage therefore passes
// and only the receipt content differs, which is exactly the deduplication
// ordering the negative needs.
func reportReplaySecondVerification(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence) factory.VerificationRow {
	t.Helper()
	applied := fixture.applyPipeline(reportReplaySecondPipeline, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    reportReplaySecondRequest,
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: reportReplayCanonicalSubject(fixture, evidence),
			},
			{
				Name:    reportReplaySecondAction,
				Image:   fixture.Image.Reference,
				Timeout: approvalContainerTimeout,
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`while [ ! -e /workspace/release ]; do sleep 1; done`,
				},
			},
		},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-"+reportReplaySecondPipeline)
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })

	// Resolve the exact human pass for the canonical subject. The request runs
	// without a workload; the later hold action then runs with a fresh live
	// credential.
	waitProtectedActionRunning(t, fixture, started.ExecutionID, reportReplaySecondRequest)
	pending := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, started.ExecutionID)
	review := actionByName(t, pending, reportReplaySecondRequest)
	resolveProtectedGoldenRequest(t, fixture, protectedGoldenEvidence{
		executionID:   started.ExecutionID,
		reviewerToken: evidence.reviewerToken,
	}, review)

	waitProtectedActionRunning(t, fixture, started.ExecutionID, reportReplaySecondAction)
	credential := waitProtectedGoldenCredential(t, fixture, started.ExecutionID, reportReplaySecondAction)
	serviceToken := reportReplayServiceToken(t, fixture)
	subject := reportReplayGoldenSubject(t, evidence)
	result, publicError := fixture.REST(serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: credential,
		ResourceID:      fixture.git.ProjectResourceID,
		Operation:       "git.ref.update",
		Subject:         subject,
		Approval:        "role/reviewer",
	}, nil, "verify-"+reportReplaySecondPipeline)
	if publicError.Code != "" {
		t.Fatalf("the second genuine actor.verify was refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var verified api.VerifyResponse
	fixture.Decode(result, &verified)
	if !verified.Approval {
		t.Fatalf("the second actor.verify recorded no approval: %+v", verified)
	}
	if verified.VerificationID == "" || verified.VerificationID == evidence.verification.ID {
		t.Fatalf("the second actor.verify returned verification %q; want a new identity distinct from %q",
			verified.VerificationID, evidence.verification.ID)
	}
	return fixture.verificationByID(t, verified.VerificationID)
}

// reportReplayCanonicalSubject renders the literal canonical five-field subject
// for the second execution's request action. Literals are deliberate: the
// approval evidence must be byte-equal to the accepted protected subject.
func reportReplayCanonicalSubject(fixture *gitHarness, evidence protectedGoldenEvidence) map[string]any {
	return map[string]any{
		"operation":     "git.ref.update",
		"resource":      fixture.git.ProjectResourceID,
		"ref":           gitMainRef,
		"expected_base": evidence.base,
		"candidate":     evidence.candidate,
	}
}

// reportReplayRevokeService revokes the reporting service credential through
// the public operation at its current version and requires affirmative
// invalidation.
func reportReplayRevokeService(t *testing.T, fixture *gitHarness) uint64 {
	t.Helper()
	// Service credential routes are id-only and project-scoped: the tenant-wide
	// admin credential is refused by the scope rule, so this uses the project
	// credential like the Git service revocation test.
	result := fixture.MustREST(fixture.ProjectToken, "service.credential.revoke",
		api.ServiceCredentialRevokeRequest{ServiceID: fixture.git.ServiceID},
		&fixture.git.ServiceVersion, "revoke-"+fixture.git.ServiceID)
	var revoked api.ServiceCredentialRevokeResult
	fixture.Decode(result, &revoked)
	if revoked.Invalidated == nil || !*revoked.Invalidated {
		t.Fatalf("service.credential.revoke reported no invalidation: %+v", revoked)
	}
	if revoked.Version <= fixture.git.ServiceVersion {
		t.Fatalf("service.credential.revoke version = %d; want an advance past %d",
			revoked.Version, fixture.git.ServiceVersion)
	}
	return revoked.Version
}

// reportReplayIssueService issues a fresh reporting service credential at the
// given (post-revocation) version.
func reportReplayIssueService(t *testing.T, fixture *gitHarness, version uint64) string {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{
			ServiceID:  fixture.git.ServiceID,
			TTLSeconds: int(time.Hour / time.Second),
		}, &version, "reissue-"+fixture.git.ServiceID)
	var issued api.ServiceCredentialIssueResult
	fixture.Decode(result, &issued)
	if issued.Token == nil || *issued.Token == "" {
		t.Fatalf("service.credential.issue returned no fresh token")
	}
	return *issued.Token
}

// TestFactoryReportAfterBindingWithdrawal proves the current reporting binding
// is re-read on every report attempt, including an exact replay whose receipt
// is already durable. Withdrawing the binding's reportable event type makes the
// exact report refuse with invalid_input before any receipt, event or ref fact
// changes; restoring the declaration lets the same exact report converge on the
// original event. This extends the report replay tests beyond actor-token
// expiry: a live reporting credential and an accepted effect are not enough
// when the binding no longer declares the event type.
func TestFactoryReportAfterBindingWithdrawal(t *testing.T) {
	fixture := startGitHarness(t)
	effect := pushReportEffect(t, fixture, "binding-withdrawal")
	serviceToken := reportReplayServiceToken(t, fixture)
	request := effect.reportRequest(t)
	event := fixture.waitReportEvent(t, effect.verification.ID)
	before := effect.durable(t, fixture, event.ID)

	binding := fixture.readBinding(t, fixture.git.ProjectBindingID)
	if !slices.Contains(binding.ReportableEvents, "git.ref.updated") {
		t.Fatalf("project binding reportable events = %v; want git.ref.updated before withdrawal",
			binding.ReportableEvents)
	}
	withdrawn := fixture.MustREST(fixture.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
		BindingID:        binding.BindingID,
		ResourceID:       binding.ResourceID,
		EnforcingService: binding.EnforcingService,
		TrustedOrigins:   binding.TrustedOrigins,
		Operations:       binding.Operations,
		ReportableEvents: []string{},
		Metadata:         binding.Metadata,
	}, &binding.Version, "withdraw-report-"+binding.BindingID)
	var withdrawal api.ResourceBindingUpdateResult
	fixture.Decode(withdrawn, &withdrawal)
	if withdrawal.Version != binding.Version+1 {
		t.Fatalf("reportable-event withdrawal version = %d; want %d", withdrawal.Version, binding.Version+1)
	}

	_, publicError := reportReplayCall(t, fixture, serviceToken, request, effect.journal.SourceEventID)
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("report after binding withdrawal = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeInvalidInput)
	}
	if !strings.Contains(publicError.Message, "does not declare reportable event type") ||
		!strings.Contains(publicError.Message, "git.ref.updated") {
		t.Fatalf("report after binding withdrawal message = %q; want the undeclared-event refusal",
			publicError.Message)
	}
	requireReportEffectUnchanged(t, fixture, effect, before, "after the withdrawn-binding refusal")

	// Restoring the declaration is the positive control: the accepted effect
	// is still reportable, and the exact receipt content converges on the
	// original event instead of publishing a second one.
	restored := fixture.MustREST(fixture.ProjectToken, "resource.binding.update", api.ResourceBindingUpdateRequest{
		BindingID:        binding.BindingID,
		ResourceID:       binding.ResourceID,
		EnforcingService: binding.EnforcingService,
		TrustedOrigins:   binding.TrustedOrigins,
		Operations:       binding.Operations,
		ReportableEvents: []string{"git.ref.updated"},
		Metadata:         binding.Metadata,
	}, &withdrawal.Version, "restore-report-"+binding.BindingID)
	var restoration api.ResourceBindingUpdateResult
	fixture.Decode(restored, &restoration)
	if restoration.Version != withdrawal.Version+1 {
		t.Fatalf("reportable-event restoration version = %d; want %d",
			restoration.Version, withdrawal.Version+1)
	}
	response, publicError := reportReplayCall(t, fixture, serviceToken, request, effect.journal.SourceEventID)
	if publicError.Code != "" {
		t.Fatalf("report after declaration restoration refused: %s (%s)",
			publicError.Code, publicError.Message)
	}
	if !response.Replayed || response.VerificationID != effect.verification.ID ||
		response.EventID != event.ID {
		t.Fatalf("report after declaration restoration = %+v; want the original replayed %q/%q",
			response, effect.verification.ID, event.ID)
	}
	requireReportEffectUnchanged(t, fixture, effect, before, "after the restored-binding replay")
}

// TestFactoryReportLostResponseSurvivesGitRestart proves a report committed by
// the Shell whose response was lost is retried by the restarted Git service's
// startup recovery sweep with the same stable source-event identity. The Shell
// replays the original event, so exactly one receipt and one
// resource.verified event survive, no verification is repeated and the ref
// stays at the accepted candidate. The report-only retry never re-applies the
// push.
func TestFactoryReportLostResponseSurvivesGitRestart(t *testing.T) {
	lost := newLostReportResponse()
	fixture := startGitHarnessWith(t, factory.WithGitTransport(lost.wrap))
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "lost-report")
	ref := "refs/heads/work/lost-report"
	candidate := fixture.commit(t, directory, "lost report response")

	lost.arm()
	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref)
	if code != 0 {
		t.Fatalf("lost-response push: exit %d: %s", code, stderr)
	}
	entry := waitProtectedRefJournalState(t, fixture, ref, candidate, git.JournalAccepted)
	if entry.VerificationID == "" || entry.SourceEventID == "" {
		t.Fatalf("accepted journal entry has no verification/source identity: %+v", entry)
	}
	if lost.dropped() == 0 {
		t.Fatal("the labelled transport seam dropped no report response; the lost-response path was not exercised")
	}
	verification := fixture.verificationByID(t, entry.VerificationID)
	event := fixture.waitReportEvent(t, verification.ID)
	receipts, err := fixture.ReceiptCount(t.Context(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts after the lost response: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("report receipts after the lost response = %d; want exactly one committed receipt", receipts)
	}
	verificationsBefore := fixture.countVerificationRows(t, "git.ref.update")

	// The response loss left the journal accepted, not reported. The ref is
	// already the candidate: recovery must report it, never push it again.
	lost.disarm()
	fixture.git.Restart(t)

	reported := waitProtectedRefJournalState(t, fixture, ref, candidate, git.JournalReported)
	if reported.VerificationID != entry.VerificationID ||
		reported.SourceEventID != entry.SourceEventID {
		t.Fatalf("recovered journal identity = %q/%q; want the original %q/%q",
			reported.VerificationID, reported.SourceEventID, entry.VerificationID, entry.SourceEventID)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != candidate {
		t.Fatalf("recovery moved %s to %q; want the accepted candidate %q", ref, remote, candidate)
	}
	replayed, ok, err := fixture.git.ReportEvent(t.Context(), verification.ID)
	if err != nil {
		t.Fatalf("read the recovered report event: %v", err)
	}
	if !ok || replayed.ID != event.ID {
		t.Fatalf("recovered report event = %+v present=%t; want the original %q", replayed, ok, event.ID)
	}
	afterReceipts, err := fixture.ReceiptCount(t.Context(), entry.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts after recovery: %v", err)
	}
	if afterReceipts != receipts {
		t.Fatalf("report receipts after recovery = %d; want the unchanged %d", afterReceipts, receipts)
	}
	if after := fixture.countVerificationRows(t, "git.ref.update"); after != verificationsBefore {
		t.Fatalf("verification rows after recovery = %d; want %d: recovery reports, it never verifies again",
			after, verificationsBefore)
	}
}

// reportEffect is one plain served work-ref push whose report was produced by
// the real hooks. It is the non-protected control shape the report binding and
// restart tests need: a real verification, journal entry and report event.
type reportEffect struct {
	ref          string
	candidate    string
	journal      git.JournalEntry
	verification factory.VerificationRow
}

// reportDurable is every durable fact a refused or replayed report must leave
// untouched for one reportEffect.
type reportDurable struct {
	ref          string
	journalCount int
	eventID      string
	receipts     int64
}

// pushReportEffect performs one real engineer push to a fixture-unique work ref
// and reads the resulting journal, verification and report facts back.
func pushReportEffect(t *testing.T, fixture *gitHarness, name string) reportEffect {
	t.Helper()
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, name)
	ref := "refs/heads/work/" + name
	candidate := fixture.commit(t, directory, name)
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref); code != 0 {
		t.Fatalf("report effect push %s: exit %d: %s", ref, code, stderr)
	}
	journal := fixture.waitJournal(t, ref, candidate)
	if journal.State != git.JournalReported {
		t.Fatalf("report effect journal state = %q; want reported", journal.State)
	}
	return reportEffect{
		ref:          ref,
		candidate:    candidate,
		journal:      journal,
		verification: fixture.verificationByID(t, journal.VerificationID),
	}
}

// reportRequest renders the exact public report for one served effect.
func (effect reportEffect) reportRequest(t *testing.T) api.ReportRequest {
	t.Helper()
	var subject api.Subject
	if err := json.Unmarshal([]byte(effect.verification.Subject), &subject); err != nil {
		t.Fatalf("decode report effect subject %s: %v", effect.verification.Subject, err)
	}
	if len(subject.Fields) == 0 {
		t.Fatalf("report effect carries no reportable subject")
	}
	return api.ReportRequest{
		ResourceID:     effect.verification.ResourceID,
		SourceEventID:  effect.journal.SourceEventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: effect.verification.ID,
		Subject:        subject,
	}
}

// durable reads the exact durable facts one report attempt must not alter.
func (effect reportEffect) durable(t *testing.T, fixture *gitHarness, eventID string) reportDurable {
	t.Helper()
	receipts, err := fixture.ReceiptCount(t.Context(), effect.journal.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts: %v", err)
	}
	return reportDurable{
		ref:          fixture.git.RemoteRef(t, factory.GitProjectRepository, effect.ref),
		journalCount: protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, effect.ref),
		eventID:      eventID,
		receipts:     receipts,
	}
}

// requireReportEffectUnchanged asserts a refused or replayed report left the
// ref, the reported journal entry, the linked event and the receipt count
// exactly as they were.
func requireReportEffectUnchanged(t *testing.T, fixture *gitHarness, effect reportEffect, before reportDurable, label string) {
	t.Helper()
	if ref := fixture.git.RemoteRef(t, factory.GitProjectRepository, effect.ref); ref != before.ref {
		t.Fatalf("report effect ref %s = %q; want the unchanged %q", label, ref, before.ref)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, effect.ref); count != before.journalCount {
		t.Fatalf("reported journal entries %s = %d; want the unchanged %d", label, count, before.journalCount)
	}
	event, ok, err := fixture.git.ReportEvent(t.Context(), effect.verification.ID)
	if err != nil {
		t.Fatalf("read report effect event %s: %v", label, err)
	}
	if !ok || event.ID != before.eventID {
		t.Fatalf("report effect event %s = %+v present=%t; want the unchanged %q",
			label, event, ok, before.eventID)
	}
	receipts, err := fixture.ReceiptCount(t.Context(), effect.journal.SourceEventID)
	if err != nil {
		t.Fatalf("count report receipts %s: %v", label, err)
	}
	if receipts != before.receipts {
		t.Fatalf("report receipts %s = %d; want the unchanged %d", label, receipts, before.receipts)
	}
}

// lostReportResponse is a labelled transport seam over the Git service's
// report call. While armed it lets the Shell commit the report and then
// discards the response, exactly like a lost response after a committed
// effect. It never rewrites the request or fabricates a result.
type lostReportResponse struct {
	inner http.RoundTripper

	mu        sync.Mutex
	armed     bool
	dropCount int
}

// newLostReportResponse returns one unarmed seam.
func newLostReportResponse() *lostReportResponse {
	return &lostReportResponse{}
}

// wrap installs the seam over the Git service's outbound transport.
func (lost *lostReportResponse) wrap(inner http.RoundTripper) http.RoundTripper {
	lost.mu.Lock()
	lost.inner = inner
	lost.mu.Unlock()
	return lost
}

// arm drops every report response until disarm.
func (lost *lostReportResponse) arm() {
	lost.mu.Lock()
	lost.armed = true
	lost.mu.Unlock()
}

// disarm stops dropping responses.
func (lost *lostReportResponse) disarm() {
	lost.mu.Lock()
	lost.armed = false
	lost.mu.Unlock()
}

// dropped reports how many committed report responses were discarded.
func (lost *lostReportResponse) dropped() int {
	lost.mu.Lock()
	defer lost.mu.Unlock()
	return lost.dropCount
}

// RoundTrip forwards the request, discards a committed report response while
// armed and returns the documented dependency failure to the caller.
func (lost *lostReportResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	lost.mu.Lock()
	inner := lost.inner
	armed := lost.armed
	if armed && strings.HasSuffix(request.URL.Path, "/api/v1/reports") {
		lost.dropCount++
	}
	lost.mu.Unlock()
	if !armed || !strings.HasSuffix(request.URL.Path, "/api/v1/reports") {
		return inner.RoundTrip(request)
	}
	response, err := inner.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	return nil, errors.New("simulated lost report response after the server committed the report")
}
