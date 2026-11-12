//go:build integration

package services

// Independent responder startup, restart and live-authority behaviour on the
// real factory composition.
//
// These scenarios extend the responder wire proof
// (test/integration/lifecycle/service_responder_test.go) from "the adapter can
// answer one request through the served DTOs" into the responder lifecycle:
//
//   - a request created before the responder starts, with a failing notification
//     source and the real broker unreachable, is discovered by the startup scan
//     and answered exactly once;
//   - a fresh adapter constructed after a committed answer whose response was
//     lost neither replaces the answer nor commits a second request.resolved
//     event, even when it processes a genuinely captured but now-stale inbox
//     page;
//   - a resolve delayed between inspection and submission is refused after the
//     service is disabled through the admin API, and the same request is then
//     answerable once the service is re-enabled with a fresh credential.
//
// Fault seams are labelled: the failing notification source, the lost resolve
// response, the delayed inbox page and the resolve barrier are test harness
// injections, not product success flags. Real broker unreachability is asserted
// separately through the outage probe. Every request here is output-free: the
// adapter owns no response policy. Resolution from the served subject and
// declared output names is proved by TestExplicitServiceResolution.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	serviceadapter "github.com/optimiware/kodama/internal/userland/services"
	servicesapp "github.com/optimiware/kodama/internal/userland/services/app"
	"github.com/optimiware/kodama/test/support/factory"
)

const responderServiceID = "responder-rescan-svc"

// TestResponderStartupFindsOldRequestWithoutNotification creates the request
// before the responder process starts, makes notifications fail explicitly and
// stops the real NATS broker, then proves the startup scan reached the served
// inbox and committed exactly one service answer.
func TestResponderStartupFindsOldRequestWithoutNotification(t *testing.T) {
	harness := factory.Start(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	access := createResponderService(t, harness, responderServiceID)
	applied := applyResponderPipeline(t, harness, "responder-startup", responderServiceID)
	started := startPipeline(t, harness, applied.PipelineID, "start-responder-startup")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
	action := actionByName(t, waiting, "answer")

	// The request is genuinely old: it exists and is discoverable through the
	// served inbox before any responder starts.
	inbox := inboxRequests(t, harness, access.token)
	item, found := findRequestInbox(inbox, started.ExecutionID, action.ActionID)
	if !found {
		t.Fatalf("service inbox does not contain the old request %s/%s: %+v",
			started.ExecutionID, action.ActionID, inbox.Items)
	}
	if item.Assignment != "service/"+responderServiceID {
		t.Fatalf("inbox assignment = %q; want service/%s", item.Assignment, responderServiceID)
	}
	if item.Version == 0 || item.At.IsZero() {
		t.Fatalf("inbox item lacks its action version/time: %+v", item)
	}

	// The real broker is genuinely unreachable for the whole startup window,
	// separately from the injected notification failure below.
	harness.NATS.Stop(t)
	requireBrokerUnreachable(t, harness)

	handler := newResponderHandler(harness, access.token)
	runErr := make(chan error, 1)
	go func() {
		runErr <- servicesapp.Run(ctx, servicesapp.Config{
			Kind:      serviceadapter.KindResponder,
			ServiceID: id.PrincipalID(access.id),
			TenantID:  id.TenantID(factory.TenantID),
			ProjectID: id.ProjectID(factory.ProjectID),
			Handler:   handler,
			// Explicitly labelled fault injection: the notification drain
			// fails before it can report a single hint.
			Events: faultNotificationSource{},
			Clock:  servicesapp.SystemClock{},
		})
	}()

	// One committed answer, observed through the served edge while the broker
	// is down. A responder that only follows notifications, that aborts its
	// startup pass on a notification failure, or that reports success without
	// submitting a resolution cannot produce this state.
	settled := waitExecutionState(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded after the startup answer (failure=%+v)",
			settled.State, settled.Failure)
	}
	assertAnswer(t, settled, responderServiceID, "pass")
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one startup answer", got)
	}
	harness.Eventually(t, 30*time.Second, func() bool {
		return handler.count(serviceadapter.OperationInbox) >= 1 && handler.resolveAttempts() == 1
	})
	if scans := handler.count(serviceadapter.OperationInbox); scans < 1 {
		t.Fatalf("inbox scans = %d; the startup pass did not scan the served inbox", scans)
	}
	if inspections := handler.count(serviceadapter.OperationInspect); inspections < 1 {
		t.Fatalf("inspections = %d; the startup pass did not revalidate the request", inspections)
	}
	if resolves := handler.resolveAttempts(); resolves != 1 {
		t.Fatalf("resolve submissions = %d; want exactly one committed answer", resolves)
	}
	if count := countResolvedEvents(t, harness, started.ExecutionID); count != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one", count)
	}

	// The answer did not depend on the broker returning.
	requireBrokerUnreachable(t, harness)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("responder startup run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("responder run did not stop after cancellation")
	}
	harness.NATS.Start(t)
}

// TestRestartedResponderDoesNotReplaceCommittedAnswer resolves one request,
// loses the committed response, discards the adapter and constructs fresh
// instances. The original answer and exactly one committed event survive, and a
// replayed page that was genuinely captured while the request was still pending
// is revalidated against current state instead of being answered again.
func TestRestartedResponderDoesNotReplaceCommittedAnswer(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	access := createResponderService(t, harness, responderServiceID)
	applied := applyResponderPipeline(t, harness, "responder-restart", responderServiceID)
	started := startPipeline(t, harness, applied.PipelineID, "start-responder-restart")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
	action := actionByName(t, waiting, "answer")

	// Capture one genuine pending inbox page through the served edge. A later
	// adapter processes this exact observed page after the settlement, which is
	// the durable shape of a scan that started before the commit and finished
	// after it.
	staleResult := harness.MustREST(access.token, "task.inbox", api.TaskInboxRequest{
		ProjectID: factory.ProjectID,
		Kinds:     []string{"request"},
	}, nil, "")
	var staleInbox api.TaskInboxResult
	harness.Decode(staleResult, &staleInbox)
	if _, found := findRequestInbox(staleInbox, started.ExecutionID, action.ActionID); !found {
		t.Fatalf("captured inbox page does not contain the pending request: %+v", staleInbox.Items)
	}

	// First adapter pass: the served edge commits the answer, then the response
	// is lost in transport (labelled fault injection).
	firstHandler := newResponderHandler(harness, access.token)
	firstHandler.loseFirstResolve = true
	firstAdapter := newResponderAdapter(t, access, firstHandler, emptyNotificationSource{})
	if err := firstAdapter.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce reported success although the resolve response was lost")
	} else {
		t.Logf("first RunOnce error (expected: the lost resolve response): %v", err)
	}
	if calls := firstHandler.resolveAttempts(); calls != 1 {
		t.Fatalf("resolve submissions = %d; want the one committed answer", calls)
	}

	settled := waitExecutionSettled(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded after the lost-response pass", settled.State)
	}
	answered := assertAnswer(t, settled, responderServiceID, "pass")
	if answered.AnsweredAt == nil {
		t.Fatal("committed answer has no answered_at")
	}
	answeredAt := *answered.AnsweredAt
	actionVersion := answered.Version
	executionVersion := settled.Version
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one", got)
	}

	// Destroy the first adapter instance. Nothing from it is reused.
	firstAdapter = nil
	_ = firstAdapter

	// A fresh instance does not know the request was answered. It processes the
	// real captured-but-stale page, so the request is offered to it again.
	secondHandler := newResponderHandler(harness, access.token)
	secondHandler.replayInbox = &staleResult
	secondAdapter := newResponderAdapter(t, access, secondHandler, emptyNotificationSource{})
	if err := secondAdapter.RunOnce(ctx); err != nil {
		t.Fatalf("restarted responder RunOnce: %v", err)
	}
	if scans := secondHandler.count(serviceadapter.OperationInbox); scans != 1 {
		t.Fatalf("restarted inbox scans = %d; want the one captured stale page to be processed", scans)
	}
	if inspections := secondHandler.count(serviceadapter.OperationInspect); inspections < 1 {
		t.Fatal("restarted responder did not revalidate the stale request against current state")
	}
	if calls := secondHandler.resolveAttempts(); calls != 0 {
		t.Fatalf("restarted resolve submissions = %d; want none for a settled request", calls)
	}

	// The original actor/time/result and exactly one committed event remain.
	afterRestart := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	afterAction := assertAnswer(t, afterRestart, responderServiceID, "pass")
	if afterRestart.Version != executionVersion {
		t.Fatalf("execution version after restart = %d; want the committed %d", afterRestart.Version, executionVersion)
	}
	if afterAction.Version != actionVersion {
		t.Fatalf("action version after restart = %d; want the committed %d", afterAction.Version, actionVersion)
	}
	if afterAction.AnsweredAt == nil || !afterAction.AnsweredAt.Equal(answeredAt) {
		t.Fatalf("answered_at after restart = %v; want the committed %v", afterAction.AnsweredAt, answeredAt)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events after restart = %d; want exactly one", got)
	}

	// Third instance: a real current inbox scan no longer offers the settled
	// request at all, independent of the replayed page above.
	thirdHandler := newResponderHandler(harness, access.token)
	thirdAdapter := newResponderAdapter(t, access, thirdHandler, emptyNotificationSource{})
	if err := thirdAdapter.RunOnce(ctx); err != nil {
		t.Fatalf("current-inbox responder RunOnce: %v", err)
	}
	if scans := thirdHandler.count(serviceadapter.OperationInbox); scans < 1 {
		t.Fatalf("current-inbox scans = %d; want a real scan", scans)
	}
	if calls := thirdHandler.resolveAttempts(); calls != 0 {
		t.Fatalf("current-inbox resolve submissions = %d; want none for a settled request", calls)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events after current-inbox scan = %d; want exactly one", got)
	}
}

// TestResponderDisabledBetweenInspectAndResolveIsRefused barriers the outgoing
// resolve after inspection, disables the service through the admin API, then
// releases it. The resolve is refused and the request stays unanswered; an
// enabled positive control with a fresh credential then answers the same
// request, proving the refusal came from live authority and not a broken
// fixture.
func TestResponderDisabledBetweenInspectAndResolveIsRefused(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	access := createResponderService(t, harness, responderServiceID)
	applied := applyResponderPipeline(t, harness, "responder-disabled", responderServiceID)
	started := startPipeline(t, harness, applied.PipelineID, "start-responder-disabled")
	waitRequestRunning(t, harness, started.ExecutionID, "answer")

	release := make(chan struct{})
	barrierHandler := newResponderHandler(harness, access.token)
	barrierHandler.blockResolve = true
	barrierHandler.resolveRelease = release
	barrierAdapter := newResponderAdapter(t, access, barrierHandler, emptyNotificationSource{})

	done := make(chan error, 1)
	go func() { done <- barrierAdapter.RunOnce(ctx) }()

	// The inspection has completed; the adapter is now blocked before the
	// outgoing submission.
	waitSignal(t, barrierHandler.inspectDone, "the responder inspection before the resolve barrier")

	// Live authority changes between inspection and submission.
	disabled := disableService(t, harness, responderServiceID, access.version, "disable-between-inspect-and-resolve")
	close(release)

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("barriered RunOnce did not return after the release")
	}
	if runErr == nil {
		t.Fatal("RunOnce reported success although the resolve was refused after the service was disabled")
	}
	t.Logf("barriered RunOnce error (expected: the refusal): %v", runErr)
	if attempts := barrierHandler.resolveAttempts(); attempts != 1 {
		t.Fatalf("resolve attempts = %d; want the one barriered submission", attempts)
	}
	refusal := barrierHandler.resolveError()
	if refusal.Code != api.CodeUnauthenticated && refusal.Code != api.CodeForbidden {
		t.Fatalf("resolve refusal code = %q; want %q or %q (message=%s)",
			refusal.Code, api.CodeUnauthenticated, api.CodeForbidden, refusal.Message)
	}

	// The refused submission settled nothing and committed no event.
	current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if current.State != "running" {
		t.Fatalf("execution state after refused resolve = %q; want running", current.State)
	}
	assertUnanswered(t, current)
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 0 {
		t.Fatalf("committed request.resolved events after refusal = %d; want none", got)
	}
	// The request is still current and answerable in durable state; the
	// disabled service simply loses its projection and authority. The enabled
	// positive control below proves it is not gone.

	// Enabled positive control: the same request is answerable once the service
	// is re-enabled with a freshly issued credential.
	enabled := enableService(t, harness, responderServiceID, disabled.Version, "re-enable-between-inspect-and-resolve")
	token, version := issueServiceCredential(t, harness, responderServiceID, enabled.Version, "reissue-between-inspect-and-resolve")
	control := serviceAccess{id: responderServiceID, version: version, token: token}
	controlInbox := inboxRequests(t, harness, control.token)
	if _, found := findRequestInbox(controlInbox, started.ExecutionID, actionByName(t, current, "answer").ActionID); !found {
		t.Fatalf("the re-enabled service does not see the still-current request: %+v", controlInbox.Items)
	}

	controlHandler := newResponderHandler(harness, control.token)
	controlAdapter := newResponderAdapter(t, control, controlHandler, emptyNotificationSource{})
	if err := controlAdapter.RunOnce(ctx); err != nil {
		t.Fatalf("enabled positive-control RunOnce: %v", err)
	}
	if calls := controlHandler.resolveAttempts(); calls != 1 {
		t.Fatalf("positive-control resolve submissions = %d; want exactly one", calls)
	}
	settled := waitExecutionState(t, harness, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state after the enabled control = %q; want succeeded (failure=%+v)",
			settled.State, settled.Failure)
	}
	assertAnswer(t, settled, responderServiceID, "pass")
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events after the enabled control = %d; want exactly one", got)
	}
}
