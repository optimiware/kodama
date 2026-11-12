//go:build integration

package lifecycle

// Request waits on the real factory composition.
//
// Coverage map (docs/specs/11-verification.md):
//   - AC 4 request settlement: pass advances the same execution, fail fails it
//     with the declared failure vocabulary, deadline expiry fails it with
//     timeout, execution cancel settles the request without an answer, stale
//     versions and a human without the assigned role are refused without
//     settling.
//   - AC 16 planned waits: a running request keeps the execution running, has no
//     running workload, exposes the assignment only to the assigned role, and
//     the next container runs in the same execution/workspace with the resolved
//     outputs and a fresh execution credential.
//   - AC 6 recovery: NOT credited here. This suite proves the durable request
//     events and the lost-response-after-effect replay through one receipt, not
//     a process or broker restart.
//   - AC 17 handoffs: NOT credited here; task handoffs are in handoffs_test.go.
//
// The request inbox item is self-sufficient: it carries the immutable typed
// subject, the declared response output names and the action deadline, and
// execution.inspect adds the action view's subject and assignment.
// TestRequestInboxItemIsSelfSufficient pins that public path. A test-only
// harness probe also reads the persisted actions.request_subject through the
// runtime role's transaction-local scope (no product behavior and no
// synthesized value), so the exact resolved binding is asserted directly while
// the request waits, after a pipeline edit and after resolution. The tests
// prove the binding through its observable effects too: activation resolves
// input and earlier-output references, a pipeline edit while the request waits
// does not rebind the action, and the original declared response schema still
// settles.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/kernel/definition"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	requestSeed       = "request-seed"
	requestArtifact   = "built:" + requestSeed
	requestApproval   = "approved:" + requestArtifact
	requestIntegrated = "integrated:" + requestApproval

	// requestWaitTimeout is the long request deadline used by every subtest
	// except the expiry subtest. The persisted deadline is compared by the
	// store's transaction clock (real PostgreSQL time), so a long timeout keeps
	// the wait deterministic while the harness's adjustable clock is not
	// advanced; the expiry subtest uses the 1m minimum and advances 61s.
	requestWaitTimeout = "1h"
	// containerTimeout keeps container action deadlines well clear of the
	// transaction clock for the duration of one subtest.
	containerTimeout = "1h"
)

// TestRequestWaits proves the real request wait lifecycle. Each subtest settles
// the execution it starts; the expiry subtest owns a separate harness so its
// clock advance cannot leak into another subtest.
func TestRequestWaits(t *testing.T) {
	harness := factory.Start(t)

	t.Run("pass continues the same execution and workspace", func(t *testing.T) {
		testRequestPass(t, harness)
	})
	t.Run("rejected request fails the execution without running later actions", func(t *testing.T) {
		testRequestRejected(t, harness)
	})
	t.Run("execution cancel during the wait settles the request without an answer", func(t *testing.T) {
		testRequestCancelled(t, harness)
	})
	t.Run("credential is revoked during the wait and fresh after pass", func(t *testing.T) {
		testRequestCredential(t, harness)
	})
	t.Run("stale versions and a missing role are refused without settling", func(t *testing.T) {
		testRequestRefusals(t, harness)
	})
	t.Run("deadline expiry fails the request without an answer", func(t *testing.T) {
		// A separate harness is required: this subtest advances the shared
		// authoritative clock by 61s, which would expire every later wait.
		deadlineHarness := factory.Start(t)
		testRequestDeadline(t, deadlineHarness)
	})
}

// TestRequestInboxItemIsSelfSufficient proves a served request item carries the
// immutable typed subject, the declared response output names and the action
// deadline through the real public API, so a responder answers without a second
// read. execution.inspect exposes the same subject and the exact assignment.
func TestRequestInboxItemIsSelfSufficient(t *testing.T) {
	harness := factory.Start(t)
	applied := applyRequestPipeline(t, harness, "request-self-sufficient", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-self-sufficient")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")

	reviewerInbox := inboxRequests(t, harness, harness.ReviewerToken)
	item, ok := findRequestInbox(reviewerInbox, started.ExecutionID, approve.ActionID)
	if !ok {
		t.Fatalf("reviewer inbox does not contain the served request: %+v", reviewerInbox.Items)
	}
	if item.Subject == nil {
		t.Fatalf("served request item carries no immutable subject: %+v", item)
	}
	wantSubject := map[string]string{"seed": requestSeed, "artifact": requestArtifact}
	gotSubject := make(map[string]string, len(item.Subject.Fields))
	for _, field := range item.Subject.Fields {
		if field.Kind != "string" {
			t.Fatalf("request subject field %q kind = %q; want the resolved string literal", field.Name, field.Kind)
		}
		gotSubject[field.Name] = field.Value
	}
	if len(gotSubject) != len(wantSubject) {
		t.Fatalf("request subject = %v; want %v", gotSubject, wantSubject)
	}
	for name, value := range wantSubject {
		if gotSubject[name] != value {
			t.Fatalf("request subject %q = %q; want %q (subject=%+v)", name, gotSubject[name], value, gotSubject)
		}
	}
	if len(item.Outputs) != 1 || item.Outputs[0] != "approved" {
		t.Fatalf("request item outputs = %v; want the declared response name [approved]", item.Outputs)
	}

	inspected := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	approve = actionByName(t, inspected, "approve")
	if approve.Subject == nil {
		t.Fatalf("execution.inspect request action carries no subject: %+v", approve)
	}
	if approve.Assignment != "role/reviewer" {
		t.Fatalf("execution.inspect request assignment = %q; want role/reviewer", approve.Assignment)
	}
	if approve.Deadline == nil {
		t.Fatal("execution.inspect request action carries no deadline")
	}
	if !item.At.Equal(*approve.Deadline) {
		t.Fatalf("inbox item at = %s; want the action deadline %s", item.At, *approve.Deadline)
	}
	if !sameSubject(approve.Subject, item.Subject) {
		t.Fatalf("inspect subject = %+v; want the inbox item subject %+v", approve.Subject, item.Subject)
	}
}

// sameSubject reports whether two public typed subjects carry the same fields.
func sameSubject(left, right *api.Subject) bool {
	if left == nil || right == nil {
		return left == right
	}
	if len(left.Fields) != len(right.Fields) {
		return false
	}
	want := make(map[string]api.SubjectField, len(right.Fields))
	for _, field := range right.Fields {
		want[field.Name] = field
	}
	for _, field := range left.Fields {
		if want[field.Name] != field {
			return false
		}
	}
	return true
}

// testRequestPass proves a pass advances the same execution/workspace, retains
// the decision/actor/answer, keeps the subject binding across a pipeline edit,
// and replays an MCP resolve to exactly one receipt.
func testRequestPass(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyRequestPipeline(t, harness, "request-pass", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-pass")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")
	prepare := actionByName(t, waiting, "prepare")
	if prepare.State != "succeeded" {
		t.Fatalf("prepare state = %q; want succeeded before the request is answerable", prepare.State)
	}
	if waiting.State != "running" {
		t.Fatalf("execution state during the request wait = %q; want running", waiting.State)
	}
	if approve.Kind != "request" {
		t.Fatalf("approve kind = %q; want request", approve.Kind)
	}
	if approve.TimeoutSeconds != int64((time.Hour)/time.Second) {
		t.Fatalf("approve timeout_seconds = %d; want %d", approve.TimeoutSeconds, int64(time.Hour/time.Second))
	}
	assertDeadlineRelation(t, approve)
	assertRequestSubject(t, harness, started.ExecutionID, "approve")
	if approve.Decision != "" || approve.Actor != "" || approve.AnsweredAt != nil {
		t.Fatalf("running request carries a decision/actor/answer: %+v", approve)
	}

	// The stop barrier must hold: the earlier workload is stopped and no
	// workload container is running while the request waits. The positive
	// control proves the same probes observe the retained workspace and the
	// stopped earlier container, and that RunningContainers can see a running
	// container selected by the execution label, before their absence is
	// trusted.
	assertRequestWaitResources(t, harness, started.ExecutionID)
	assertRunningContainersPositiveControl(t, harness, started.ExecutionID)

	// The inbox is derived from the authoritative running action and filtered by
	// the caller's current role: the reviewer sees it, an operator-only human
	// does not.
	reviewerInbox := inboxRequests(t, harness, harness.ReviewerToken)
	item, ok := findRequestInbox(reviewerInbox, started.ExecutionID, approve.ActionID)
	if !ok {
		t.Fatalf("reviewer inbox does not contain request %s/%s: %+v", started.ExecutionID, approve.ActionID, reviewerInbox.Items)
	}
	if item.Assignment != "role/reviewer" {
		t.Fatalf("inbox assignment = %q; want role/reviewer", item.Assignment)
	}
	if item.Version == 0 || item.At.IsZero() {
		t.Fatalf("inbox item lacks version/time: %+v", item)
	}
	operatorInbox := inboxRequests(t, harness, harness.OperatorToken)
	if _, found := findRequestInbox(operatorInbox, started.ExecutionID, approve.ActionID); found {
		t.Fatalf("operator-only inbox exposed a reviewer-assigned request: %+v", operatorInbox.Items)
	}

	// The activation is a durable committed event.
	requireEvent(t, readRequestEvents(t, harness, "action.requested"), "action.requested", started.ExecutionID, approve.ActionID)

	// A pipeline edit cannot rebind the running request: the admitted action,
	// deadline and response schema stay the original ones. The execution version
	// may legitimately advance from unrelated committed progress (for example
	// container cleanup), so the immutability assertion is on the action facts.
	replacement := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "replacement",
			Image:   harness.Image.Reference,
			Timeout: containerTimeout,
			Commands: []string{
				"true",
			},
		}},
	})
	updatePipeline(t, harness, applied.PipelineID, "request-pass", replacement, applied.Version)
	afterEdit := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterEdit.State != "running" {
		t.Fatalf("a pipeline edit changed the running execution state to %q", afterEdit.State)
	}
	editedApprove := actionByName(t, afterEdit, "approve")
	if editedApprove.ActionID != approve.ActionID ||
		editedApprove.State != "running" ||
		editedApprove.TimeoutSeconds != approve.TimeoutSeconds ||
		editedApprove.Deadline == nil || !editedApprove.Deadline.Equal(*approve.Deadline) ||
		editedApprove.Decision != "" || editedApprove.Actor != "" || editedApprove.AnsweredAt != nil {
		t.Fatalf("a pipeline edit rebound the running request: before=%+v after=%+v", approve, editedApprove)
	}
	// The persisted subject is immutable too, not just the public action facts.
	assertRequestSubject(t, harness, started.ExecutionID, "approve")

	// Resolve pass with both expected versions. The reviewer is the exact
	// current assignee. A concurrent committed progress revision is retried from
	// a fresh inspect, exactly like a workload output submission.
	outcome := requestResolveCurrent(t, harness, harness.ReviewerToken, started.ExecutionID, "approve",
		"pass", map[string]string{"approved": requestApproval}, "resolve-request-pass")
	resolved := outcome.Result
	if resolved.ExecutionVersion <= outcome.ExpectedExecution || resolved.ActionVersion <= outcome.ExpectedAction {
		t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
			resolved, outcome.ExpectedExecution, outcome.ExpectedAction)
	}

	// A lost response after the committed effect replays over MCP with the same
	// idempotency key and the same expected versions, returning the same result
	// and committing exactly one receipt.
	replayInput := api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              approve.ActionID,
		ExpectedActionVersion: outcome.ExpectedAction,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}
	replay := harness.MustMCP(harness.ReviewerToken, "execution.request.resolve", replayInput, &outcome.ExpectedExecution, "resolve-request-pass")
	var replayed api.ExecutionRequestResolveResult
	harness.Decode(replay, &replayed)
	if replayed != resolved {
		t.Fatalf("MCP replay result = %+v; want %+v", replayed, resolved)
	}
	receipts, err := harness.ReceiptCount(ctx, started.ExecutionID+"/"+approve.ActionID)
	if err != nil {
		t.Fatalf("count resolve receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("resolve receipt count = %d; want exactly 1", receipts)
	}

	// The same execution continues into integrate, which proves the shared
	// workspace, the promoted resolve output and the final output submission.
	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	if finished.ExecutionID != started.ExecutionID {
		t.Fatalf("pass continued execution %q; want %q", finished.ExecutionID, started.ExecutionID)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.Decision != "pass" {
		t.Fatalf("recorded decision = %q; want pass", finalApprove.Decision)
	}
	if finalApprove.Actor != factory.ReviewerPrincipal {
		t.Fatalf("recorded actor = %q; want %q", finalApprove.Actor, factory.ReviewerPrincipal)
	}
	if finalApprove.AnsweredAt == nil {
		t.Fatalf("recorded answer has no answered_at")
	}
	integrate := actionByName(t, finished, "integrate")
	if integrate.State != "succeeded" {
		t.Fatalf("integrate state = %q; want succeeded", integrate.State)
	}
	if integrate.Outputs["integrated"] != requestIntegrated {
		t.Fatalf("integrate output = %q; want %q", integrate.Outputs["integrated"], requestIntegrated)
	}

	// The resolution is a durable committed event too.
	requireEvent(t, readRequestEvents(t, harness, "request.resolved"), "request.resolved", started.ExecutionID, approve.ActionID)

	// Settlement does not rewrite the resolved subject, and the terminal
	// execution releases its workload and retained workspace.
	assertRequestSubject(t, harness, started.ExecutionID, "approve")
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testRequestRejected proves a failing decision fails the execution with the
// declared failure vocabulary, retains the answer, and never runs a later
// action.
func testRequestRejected(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyRequestPipeline(t, harness, "request-reject", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-reject")

	waitRequestRunning(t, harness, started.ExecutionID, "approve")
	requestResolveCurrent(t, harness, harness.ReviewerToken, started.ExecutionID, "approve",
		"fail", nil, "resolve-request-reject")

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if finished.State != "failed" {
		t.Fatalf("rejected execution state = %q; want failed", finished.State)
	}
	if finished.Failure == nil || finished.Failure.Kind != "runtime" || finished.Failure.Reason != "answer_rejected" {
		t.Fatalf("rejected execution failure = %+v; want kind runtime reason answer_rejected", finished.Failure)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "failed" {
		t.Fatalf("rejected request action state = %q; want failed", finalApprove.State)
	}
	if finalApprove.Decision != "fail" {
		t.Fatalf("retained decision = %q; want fail", finalApprove.Decision)
	}
	if finalApprove.Actor != factory.ReviewerPrincipal || finalApprove.AnsweredAt == nil {
		t.Fatalf("retained answer actor/time = %q/%v; want the reviewer and a time", finalApprove.Actor, finalApprove.AnsweredAt)
	}
	integrate := actionByName(t, finished, "integrate")
	if integrate.State != "cancelled" {
		t.Fatalf("later action state = %q; want cancelled", integrate.State)
	}
	containers, err := harness.Containers(ctx, runtimepodman.LabelAction, integrate.ActionID)
	if err != nil {
		t.Fatalf("list later-action containers: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("a rejected request started later-action containers %v", containers)
	}

	// The rejection is terminal, so no workload of this execution may remain and
	// the retained workspace must be released. Cleanup is separate from the
	// outcome, but a terminal execution still owns its earlier stopped workload
	// and retained workspace; leaving either behind is a leak, not a legitimate
	// "none" cleanup record.
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testRequestDeadline proves advancing the authoritative clock past the persisted
// deadline fails the request with timeout and no answer, and a late answer is
// refused without settling.
func testRequestDeadline(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyRequestPipeline(t, harness, "request-deadline", "1m", nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-deadline")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")
	if approve.TimeoutSeconds != int64(time.Minute/time.Second) {
		t.Fatalf("approve timeout_seconds = %d; want 60", approve.TimeoutSeconds)
	}
	assertDeadlineRelation(t, approve)
	assertRequestWaitResources(t, harness, started.ExecutionID)

	harness.Clock.Advance(61 * time.Second)

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if finished.State != "failed" {
		t.Fatalf("expired execution state = %q; want failed", finished.State)
	}
	if finished.Failure == nil || finished.Failure.Kind != "timeout" {
		t.Fatalf("expired execution failure = %+v; want kind timeout", finished.Failure)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "failed" {
		t.Fatalf("expired request action state = %q; want failed", finalApprove.State)
	}
	if finalApprove.Decision != "" || finalApprove.Actor != "" || finalApprove.AnsweredAt != nil {
		t.Fatalf("expired request retained an answer: %+v", finalApprove)
	}
	integrate := actionByName(t, finished, "integrate")
	if integrate.State != "cancelled" {
		t.Fatalf("later action after expiry = %q; want cancelled", integrate.State)
	}
	containers, err := harness.Containers(ctx, runtimepodman.LabelAction, integrate.ActionID)
	if err != nil {
		t.Fatalf("list later-action containers: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("an expired request started later-action containers %v", containers)
	}

	// The controller has already expired the action, so the answer's expected
	// versions are stale. Once the action is terminal, the refusal is
	// version_conflict; the service's own deadline check (deadline_expired) is
	// only reachable for an answer that reaches it before the controller scan,
	// which is inherently racing. Assert the deterministic code and that
	// nothing settled.
	_, publicError := harness.REST(harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              approve.ActionID,
		ExpectedActionVersion: approve.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &waiting.Version, "resolve-request-deadline-late")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("late resolve code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	after := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if after.State != "failed" {
		t.Fatalf("late resolve changed the execution state to %q", after.State)
	}
	lateApprove := actionByName(t, after, "approve")
	if lateApprove.Decision != "" || lateApprove.Actor != "" || lateApprove.AnsweredAt != nil {
		t.Fatalf("late resolve recorded an answer: %+v", lateApprove)
	}
	receipts, err := harness.ReceiptCount(ctx, started.ExecutionID+"/"+approve.ActionID)
	if err != nil {
		t.Fatalf("count late resolve receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("late resolve committed %d receipts; want 0", receipts)
	}

	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testRequestCancelled proves cancelling the execution settles the request
// without an answer and a stale answer is refused.
func testRequestCancelled(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyRequestPipeline(t, harness, "request-cancel", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-cancel")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")
	assertRequestWaitResources(t, harness, started.ExecutionID)

	cancelled := cancelExecutionCurrent(t, harness, harness.ProjectToken, started.ExecutionID, "cancel-request-cancel")
	if cancelled.ExecutionVersion <= waiting.Version {
		t.Fatalf("cancel execution version = %d; want > %d", cancelled.ExecutionVersion, waiting.Version)
	}

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if finished.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", finished.State)
	}
	finalApprove := actionByName(t, finished, "approve")
	if finalApprove.State != "cancelled" {
		t.Fatalf("cancelled request action state = %q; want cancelled", finalApprove.State)
	}
	if finalApprove.Decision != "" || finalApprove.Actor != "" || finalApprove.AnsweredAt != nil {
		t.Fatalf("cancelled request retained an answer: %+v", finalApprove)
	}

	_, publicError := harness.REST(harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              approve.ActionID,
		ExpectedActionVersion: approve.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &waiting.Version, "resolve-request-cancel-late")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale resolve after cancel code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	after := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if after.State != "cancelled" {
		t.Fatalf("stale resolve changed the cancelled execution state to %q", after.State)
	}
	lateApprove := actionByName(t, after, "approve")
	if lateApprove.Decision != "" || lateApprove.Actor != "" || lateApprove.AnsweredAt != nil {
		t.Fatalf("stale resolve recorded an answer after cancel: %+v", lateApprove)
	}
	receipts, err := harness.ReceiptCount(ctx, started.ExecutionID+"/"+approve.ActionID)
	if err != nil {
		t.Fatalf("count stale resolve receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("stale resolve after cancel committed %d receipts; want 0", receipts)
	}

	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// testRequestCredential proves the request wait has no workload credential: the
// earlier container's token is revoked on request entry, and the next container
// in the same execution receives a different token that authenticates while the
// old one does not. Tokens are never printed.
//
// The earlier token is retained in the execution workspace (/workspace) only as
// test evidence. A retained workspace file is not a log, event, receipt or task
// metadata, and the workspace is removed with the execution.
func testRequestCredential(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	prepareExtra := []string{
		`cat /kodama/secrets/KODAMA_TOKEN > /workspace/previous-token`,
		`sha256sum /kodama/secrets/KODAMA_TOKEN | cut -d' ' -f1 > /workspace/previous-token.sha256`,
	}
	integrateExtra := []string{credentialProbe()}
	applied := applyRequestPipeline(t, harness, "request-credential", requestWaitTimeout, prepareExtra, integrateExtra)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-credential")

	waitRequestRunning(t, harness, started.ExecutionID, "approve")
	assertRequestWaitResources(t, harness, started.ExecutionID)

	// The request-activation commit clears the execution credential. Read the
	// earlier container's token from the retained workspace and prove the live
	// server refuses it while the request waits. Without this in-wait probe the
	// clearing is unobservable: after the pass a fresh credential replaces the
	// old one, so the post-resume in-container probe cannot see a missing clear.
	// The token is only used as a bearer value and is never printed.
	previous, found, err := harness.ReadExecutionWorkspaceFile(ctx, started.ExecutionID, "previous-token")
	if err != nil {
		t.Fatalf("read the retained previous-token evidence: %v", err)
	}
	if !found {
		t.Fatalf("the retained workspace has no previous-token evidence")
	}
	revoked := strings.TrimSpace(string(previous))
	if revoked == "" {
		t.Fatalf("the retained previous-token evidence is empty")
	}
	_, publicError := harness.REST(revoked, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: started.ExecutionID,
	}, nil, "")
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("the revoked execution credential was not refused during the request wait: code=%q", publicError.Code)
	}
	// Positive control: the same operation accepts a valid credential, so the
	// refusal above is the revoked token and not a broken request path.
	_ = harness.MustREST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: started.ExecutionID,
	}, nil, "")

	requestResolveCurrent(t, harness, harness.ReviewerToken, started.ExecutionID, "approve",
		"pass", map[string]string{"approved": requestApproval}, "resolve-request-credential")

	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	integrate := actionByName(t, finished, "integrate")
	if integrate.State != "succeeded" {
		t.Fatalf("integrate state = %q; want succeeded (credential probe runs inside it)", integrate.State)
	}
	if integrate.Outputs["integrated"] != requestIntegrated {
		t.Fatalf("integrate output = %q; want %q", integrate.Outputs["integrated"], requestIntegrated)
	}
}

// testRequestRefusals proves stale expected action and execution versions and a
// human who does not currently hold the assigned role are refused without
// settling.
func testRequestRefusals(t *testing.T, harness *factory.Harness) {
	ctx := t.Context()
	applied := applyRequestPipeline(t, harness, "request-refusals", requestWaitTimeout, nil, nil)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": requestSeed}, "start-request-refusals")

	waiting := waitRequestRunning(t, harness, started.ExecutionID, "approve")
	approve := actionByName(t, waiting, "approve")
	resolveKey := started.ExecutionID + "/" + approve.ActionID
	assertRequestWaitResources(t, harness, started.ExecutionID)

	// A stale expected action version is refused before any effect. The current
	// execution version is re-read first so the refusal targets the action
	// version; a concurrent committed progress revision can also yield
	// version_conflict, which still commits nothing.
	current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	currentApprove := actionByName(t, current, "approve")
	_, publicError := harness.REST(harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              currentApprove.ActionID,
		ExpectedActionVersion: currentApprove.Version + 7,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &current.Version, "resolve-request-stale-action")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale action version code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	afterStale := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterStale.State != "running" {
		t.Fatalf("stale version changed the execution state to %q", afterStale.State)
	}
	staleApprove := actionByName(t, afterStale, "approve")
	if staleApprove.State != "running" || staleApprove.Decision != "" || staleApprove.Actor != "" || staleApprove.AnsweredAt != nil {
		t.Fatalf("stale version settled the request: %+v", staleApprove)
	}
	receipts, err := harness.ReceiptCount(ctx, resolveKey)
	if err != nil {
		t.Fatalf("count stale receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("stale version committed %d receipts; want 0", receipts)
	}

	// A stale expected execution version in the shared envelope with the current
	// action version is refused by the service's own version gate before any
	// effect. This is a direct call, not a retrying helper: the retry helpers
	// deliberately swallow version_conflict, which would hide a service that
	// ignored the execution version. The body names only the action version.
	current = inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	currentApprove = actionByName(t, current, "approve")
	staleExecution := current.Version + 7
	_, publicError = harness.REST(harness.ReviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              currentApprove.ActionID,
		ExpectedActionVersion: currentApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &staleExecution, "resolve-request-stale-execution")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale execution version code = %q; want %q", publicError.Code, api.CodeVersionConflict)
	}
	afterStaleExecution := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterStaleExecution.State != "running" {
		t.Fatalf("stale execution version changed the execution state to %q", afterStaleExecution.State)
	}
	staleExecutionApprove := actionByName(t, afterStaleExecution, "approve")
	if staleExecutionApprove.State != "running" || staleExecutionApprove.Decision != "" || staleExecutionApprove.Actor != "" || staleExecutionApprove.AnsweredAt != nil {
		t.Fatalf("stale execution version settled the request: %+v", staleExecutionApprove)
	}
	receipts, err = harness.ReceiptCount(ctx, resolveKey)
	if err != nil {
		t.Fatalf("count stale execution receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("stale execution version committed %d receipts; want 0", receipts)
	}

	// An operator-only human is authenticated and admitted by the grant stage but
	// does not hold the assigned reviewer role at the commit boundary.
	current = inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	currentApprove = actionByName(t, current, "approve")
	_, publicError = harness.REST(harness.OperatorToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           started.ExecutionID,
		ActionID:              currentApprove.ActionID,
		ExpectedActionVersion: currentApprove.Version,
		Decision:              "pass",
		Outputs:               map[string]string{"approved": requestApproval},
	}, &current.Version, "resolve-request-operator")
	if publicError.Code != api.CodeForbidden {
		t.Fatalf("operator-only resolve code = %q; want %q", publicError.Code, api.CodeForbidden)
	}
	afterForbidden := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterForbidden.State != "running" {
		t.Fatalf("forbidden resolve changed the execution state to %q", afterForbidden.State)
	}
	forbiddenApprove := actionByName(t, afterForbidden, "approve")
	if forbiddenApprove.State != "running" || forbiddenApprove.Decision != "" || forbiddenApprove.Actor != "" || forbiddenApprove.AnsweredAt != nil {
		t.Fatalf("forbidden resolve settled the request: %+v", forbiddenApprove)
	}
	receipts, err = harness.ReceiptCount(ctx, resolveKey)
	if err != nil {
		t.Fatalf("count forbidden receipts: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("forbidden resolve committed %d receipts; want 0", receipts)
	}

	// Settle the execution so the harness cleanup is not left with a live wait.
	cancelExecutionCurrent(t, harness, harness.ProjectToken, started.ExecutionID, "cancel-request-refusals")
	cancelled := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("cleanup cancel state = %q; want cancelled", cancelled.State)
	}
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// applyRequestPipeline authors and applies the prepare -> approve -> integrate
// pipeline. The request subject references both an admitted input and an earlier
// succeeded output, so activation proves immutable subject resolution.
func applyRequestPipeline(t *testing.T, harness *factory.Harness, name, requestTimeout string, prepareExtra, integrateExtra []string) api.PipelineApplyResult {
	t.Helper()
	prepare := []string{prepareRequestChecks()}
	prepare = append(prepare, prepareExtra...)
	prepare = append(prepare, submitScript("artifact", "ARTIFACT"))
	integrate := []string{integrateRequestChecks()}
	integrate = append(integrate, integrateExtra...)
	integrate = append(integrate,
		`INTEGRATED="integrated:$(cat /kodama/outputs/approve/approved)"`,
		submitScript("integrated", "INTEGRATED"),
	)
	document := marshalPipeline(t, pipelineDoc{
		Inputs: []string{"seed"},
		Actions: []pipelineActionDoc{
			{
				Name:     "prepare",
				Image:    harness.Image.Reference,
				Timeout:  containerTimeout,
				Outputs:  []string{"artifact"},
				Commands: prepare,
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: requestTimeout,
				Subject: map[string]any{
					"seed":     map[string]string{"from": "inputs.seed"},
					"artifact": map[string]string{"from": "prepare.outputs.artifact"},
				},
				Outputs: []string{"approved"},
			},
			{
				Name:     "integrate",
				Image:    harness.Image.Reference,
				Timeout:  containerTimeout,
				Outputs:  []string{"integrated"},
				Commands: integrate,
			},
		},
	})
	return applyPipeline(t, harness, name, document)
}

// prepareRequestChecks verifies the task-free context and input, writes the
// shared workspace marker and sets the first output.
func prepareRequestChecks() string {
	return fmt.Sprintf(`test ! -e /kodama/task.json
test -z "${KODAMA_TASK:-}"
test "$(cat /kodama/inputs/seed)" = %q
printf 'shared workspace\n' > /workspace/marker
ARTIFACT="built:$(cat /kodama/inputs/seed)"`, requestSeed)
}

// integrateRequestChecks verifies the same workspace and the promoted request
// output before the final output is set.
func integrateRequestChecks() string {
	return fmt.Sprintf(`test ! -e /kodama/task.json
test -z "${KODAMA_TASK:-}"
test "$(cat /workspace/marker)" = "shared workspace"
test "$(cat /kodama/outputs/prepare/artifact)" = %q
test "$(cat /kodama/outputs/approve/approved)" = %q`, requestArtifact, requestApproval)
}

// credentialProbe is the in-container proof that the earlier execution
// credential was revoked and the current one authenticates. It never prints a
// token: it compares only sha256 digests and public error codes.
func credentialProbe() string {
	return `ENDPOINT=${KODAMA_URL#http://}
API_HOST=${ENDPOINT%:*}
API_PORT=${ENDPOINT##*:}
CURRENT=$(cat /kodama/secrets/KODAMA_TOKEN)
OLD=$(cat /workspace/previous-token)
test -n "$CURRENT"
test -n "$OLD"
CURRENT_SHA=$(sha256sum /kodama/secrets/KODAMA_TOKEN | cut -d' ' -f1)
OLD_SHA=$(sha256sum /workspace/previous-token | cut -d' ' -f1)
test "$CURRENT_SHA" != "$OLD_SHA"
probe() {
  token=$1
  body='{}'
  length=$(printf '%s' "$body" | wc -c)
  { printf 'GET %s HTTP/1.0\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
      "/api/v1/executions/$KODAMA_EXECUTION" "$ENDPOINT" "$token" "$length" "$body"; sleep 2; } \
    | nc -w 10 "$API_HOST" "$API_PORT" | tr -d '\r' | sed '1,/^$/d'
}
OLD_RESPONSE=$(probe "$OLD")
case "$OLD_RESPONSE" in
  *'"operation":"execution.inspect"'*) echo "the revoked execution credential was accepted" >&2; exit 1 ;;
esac
case "$OLD_RESPONSE" in
  *'"code":"unauthenticated"'*) ;;
  *) echo "the revoked credential refusal was not unauthenticated" >&2; exit 1 ;;
esac
CURRENT_RESPONSE=$(probe "$CURRENT")
case "$CURRENT_RESPONSE" in
  *'"operation":"execution.inspect"'*) ;;
  *) echo "the current execution credential was refused" >&2; exit 1 ;;
esac`
}

// assertRequestSubject proves the persisted immutable subject resolved exactly
// the admitted input and the earlier succeeded output. It reads the
// controller-owned action row through the runtime role's transaction-local
// scope, never a synthesized value.
func assertRequestSubject(t *testing.T, harness *factory.Harness, executionID, actionName string) {
	t.Helper()
	subject, err := harness.RequestSubject(t.Context(), executionID, actionName)
	if err != nil {
		t.Fatalf("read persisted request subject of %s/%s: %v", executionID, actionName, err)
	}
	want := map[string]string{"seed": requestSeed, "artifact": requestArtifact}
	if len(subject) != len(want) {
		t.Fatalf("persisted request subject of %s/%s has %d fields; want %d: %+v",
			executionID, actionName, len(subject), len(want), subject)
	}
	for key, value := range want {
		field, ok := subject[key]
		if !ok || field.Kind != definition.SubjectLiteral || field.Literal != value {
			t.Fatalf("persisted request subject %q = %+v; want literal %q (subject=%+v)", key, field, value, subject)
		}
	}
}

// waitRequestRunning waits for one named request action to be the running action
// of a running execution and returns that observation.
func waitRequestRunning(t *testing.T, harness *factory.Harness, executionID, actionName string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		if last.State != "running" {
			return false
		}
		for _, action := range last.Actions {
			if action.Name == actionName {
				return action.State == "running"
			}
		}
		return false
	})
	return last
}

// resolveOutcome carries the successful resolve result and the expected versions
// the call actually used, so a lost-response replay can repeat them exactly.
type resolveOutcome struct {
	Result            api.ExecutionRequestResolveResult
	ExpectedExecution uint64
	ExpectedAction    uint64
}

// requestResolveCurrent resolves one request from the latest inspected versions.
// A concurrent committed progress revision (for example container cleanup) can
// advance the execution version between an inspect and the resolve, so a
// version_conflict is retried from a fresh inspect. Nothing is committed by a
// refused attempt, so the retry is safe.
func requestResolveCurrent(t *testing.T, harness *factory.Harness, token, executionID, actionName, decision string, outputs map[string]string, key string) resolveOutcome {
	t.Helper()
	for range 20 {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		action := actionByName(t, current, actionName)
		result, publicError := harness.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              decision,
			Outputs:               outputs,
		}, &current.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		harness.Decode(result, &resolved)
		return resolveOutcome{Result: resolved, ExpectedExecution: current.Version, ExpectedAction: action.Version}
	}
	t.Fatalf("request resolution did not converge on a current execution version")
	return resolveOutcome{}
}

// cancelExecutionCurrent cancels one execution from the latest inspected
// version, retrying a stale expected version from a fresh inspect.
func cancelExecutionCurrent(t *testing.T, harness *factory.Harness, token, executionID, key string) api.ExecutionCancelResult {
	t.Helper()
	for range 20 {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		result, publicError := harness.REST(token, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, &current.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var cancelled api.ExecutionCancelResult
		harness.Decode(result, &cancelled)
		return cancelled
	}
	t.Fatalf("execution.cancel did not converge on a current execution version")
	return api.ExecutionCancelResult{}
}

// inboxRequests reads the request-kind inbox for one principal.
func inboxRequests(t *testing.T, harness *factory.Harness, token string) api.TaskInboxResult {
	t.Helper()
	result := harness.MustREST(token, "task.inbox", api.TaskInboxRequest{
		ProjectID: factory.ProjectID,
		Kinds:     []string{"request"},
	}, nil, "")
	var inbox api.TaskInboxResult
	harness.Decode(result, &inbox)
	return inbox
}

// findRequestInbox returns the inbox item for one exact request action.
func findRequestInbox(inbox api.TaskInboxResult, executionID, actionID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "request" && item.ExecutionID == executionID && item.ActionID == actionID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// readRequestEvents reads committed events of the given types through the
// public event read. The shared fixture token is an engineer, which is allowed
// to read project events.
func readRequestEvents(t *testing.T, harness *factory.Harness, types ...string) api.EventReadResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "event.read", api.EventReadRequest{
		ProjectID: factory.ProjectID,
		Types:     types,
		Page:      &api.Page{Limit: api.MaxPageLimit},
	}, nil, "")
	var events api.EventReadResult
	harness.Decode(result, &events)
	return events
}

// requireEvent proves one committed event of the exact type names one of the
// accepted subjects. A request owner event may be keyed by the execution and the
// projected action transition by the action id, so both identities are accepted.
func requireEvent(t *testing.T, events api.EventReadResult, eventType string, subjects ...string) {
	t.Helper()
	for _, event := range events.Events {
		if event.Type != eventType {
			continue
		}
		if slices.Contains(subjects, event.SubjectID) {
			return
		}
	}
	encoded, _ := json.Marshal(events.Events)
	t.Fatalf("no committed %s event for subjects %v; events=%s", eventType, subjects, string(encoded))
}
