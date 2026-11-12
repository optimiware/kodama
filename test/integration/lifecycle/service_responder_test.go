//go:build integration

package lifecycle

// Service-responder suite: the responder adapter against the real
// served public DTOs.
//
// The unit tests prove the adapter speaks the served task.inbox,
// execution.inspect and execution.request.resolve schemas. This suite proves
// the same adapter against one real factory composition: an issued service
// credential discovers its explicitly service-assigned request, revalidates the
// current execution and action versions separately, answers it, and converges
// after a committed answer whose response was lost.
//
// The request action declares no outputs, so the answer carries no response
// fields. The served inbox item carries the subject and the declared output
// names; a responder that answers from them is exercised by
// TestExplicitServiceResolution in test/integration/services, not here.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
	"github.com/optimiware/kodama/test/support/factory"
)

const responderServiceID = "responder-svc"

// responderClock is the adapter's injected time boundary. The responder path
// never reads it, but the configuration requires one.
type responderClock struct{ now time.Time }

func (clock responderClock) Now() time.Time { return clock.now }

// emptyEventSource serves no notifications. The startup scan must not depend on
// an event hint.
type emptyEventSource struct{}

func (emptyEventSource) Poll(context.Context, string) (services.NotificationPage, error) {
	return services.NotificationPage{}, nil
}

// servedResponderHandler adapts one real service credential to the public
// operation boundary the adapter consumes. It records the exact resolve
// invocation so the suite can replay it byte-for-byte.
type servedResponderHandler struct {
	harness *factory.Harness
	token   string

	mu               sync.Mutex
	resolveCalls     int
	loseFirstResolve bool
	committed        api.Result
	lastResolve      api.Invocation
}

func (handler *servedResponderHandler) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	result, publicError, err := handler.harness.Invoke(ctx, handler.token, invocation.Operation,
		json.RawMessage(invocation.Request.Input), invocation.Request.ExpectedVersion, invocation.Request.IdempotencyKey)
	if err != nil {
		return api.Result{}, err
	}
	if publicError.Code != "" {
		return api.Result{}, &api.Error{Code: publicError.Code, Message: publicError.Message}
	}
	if invocation.Operation != services.OperationResolve {
		return result, nil
	}

	handler.mu.Lock()
	handler.resolveCalls++
	handler.lastResolve = api.Invocation{
		Operation: invocation.Operation,
		Request: api.Request{
			Input:           append(json.RawMessage(nil), invocation.Request.Input...),
			ExpectedVersion: invocation.Request.ExpectedVersion,
			IdempotencyKey:  invocation.Request.IdempotencyKey,
		},
	}
	lose := handler.loseFirstResolve
	if lose {
		handler.loseFirstResolve = false
		handler.committed = result
	}
	handler.mu.Unlock()

	if lose {
		// The effect committed at the served edge; only the response was lost.
		return api.Result{}, errors.New("simulated lost resolve response")
	}
	return result, nil
}

func (handler *servedResponderHandler) resolveCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.resolveCalls
}

func (handler *servedResponderHandler) replay() (api.Invocation, api.Result) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.lastResolve, handler.committed
}

// newServedResponderAdapter builds the real responder adapter over one served
// service credential.
func newServedResponderAdapter(t *testing.T, handler api.Handler) *services.Adapter {
	t.Helper()
	adapter, err := services.NewAdapter(services.AdapterConfig{
		Kind:      services.KindResponder,
		ServiceID: id.PrincipalID(responderServiceID),
		TenantID:  id.TenantID(factory.TenantID),
		ProjectID: id.ProjectID(factory.ProjectID),
		Handler:   handler,
		Events:    emptyEventSource{},
		Clock:     responderClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)},
	})
	if err != nil {
		t.Fatalf("services.NewAdapter: %v", err)
	}
	return adapter
}

// TestServiceResponderServedInbox proves the responder answers one old,
// explicitly service-assigned request through the real served DTOs and does not
// create a replacement answer after a lost resolve response.
func TestServiceResponderServedInbox(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	// One service account with the operator role: it may read its inbox,
	// inspect the execution and answer its own request.
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: responderServiceID,
		Roles:     []string{"operator"},
	}, nil, "create-"+responderServiceID)
	var createdService api.ServiceCreateResult
	harness.Decode(created, &createdService)
	// service.credential.issue is a versioned mutation: it needs the current
	// service version from the create result.
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  responderServiceID,
		TTLSeconds: 3600,
	}, &createdService.Version, "issue-"+responderServiceID)
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatal("service.credential.issue returned no credential token")
	}
	serviceToken := *credential.Token

	// A single request action assigned to the exact service, declaring a
	// minimal literal subject (settlement requires a resolved subject) and no
	// response fields.
	document := marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
		Name:    "answer",
		Request: "service/" + responderServiceID,
		Subject: map[string]any{"operation": "responder.served"},
		Timeout: "1h",
	}}})
	applied := applyPipeline(t, harness, "responder-served", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-responder-served")
	waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
	action := actionByName(t, waiting, "answer")

	// The served inbox exposes exactly the request item, with the exact service
	// assignment and no question projection.
	inbox := inboxRequests(t, harness, serviceToken)
	item, ok := findRequestInbox(inbox, started.ExecutionID, action.ActionID)
	if !ok {
		t.Fatalf("service inbox does not contain request %s/%s: %+v", started.ExecutionID, action.ActionID, inbox.Items)
	}
	if item.Assignment != "service/"+responderServiceID {
		t.Fatalf("inbox assignment = %q; want service/%s", item.Assignment, responderServiceID)
	}
	if item.Version == 0 || item.At.IsZero() {
		t.Fatalf("inbox item lacks its action version/time: %+v", item)
	}
	if item.QuestionID != "" || item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 {
		t.Fatalf("request inbox item carries a question projection: %+v", item)
	}

	// The served inspect exposes the execution version and the action version as
	// separate current observations.
	current := inspectExecution(t, harness, serviceToken, started.ExecutionID)
	currentAction := actionByName(t, current, "answer")
	if current.Version == 0 || currentAction.Version == 0 {
		t.Fatalf("current versions = execution %d action %d; want both positive", current.Version, currentAction.Version)
	}
	if currentAction.State != "running" || currentAction.Decision != "" {
		t.Fatalf("current request is not answerable: %+v", currentAction)
	}

	// The first adapter pass commits the answer but loses the response.
	handler := &servedResponderHandler{harness: harness, token: serviceToken, loseFirstResolve: true}
	adapter := newServedResponderAdapter(t, handler)
	if err := adapter.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce reported success although the resolve response was lost")
	} else {
		t.Logf("first RunOnce error (expected: the lost resolve response): %v", err)
	}
	if calls := handler.resolveCount(); calls != 1 {
		t.Fatalf("resolve calls = %d; want the one committed answer", calls)
	}

	// The committed decision is observable even though the caller never saw the
	// response.
	settled := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded after the pass answer", settled.State)
	}
	answered := actionByName(t, settled, "answer")
	if answered.Decision != "pass" {
		t.Fatalf("recorded decision = %q; want pass", answered.Decision)
	}
	if answered.Actor != responderServiceID {
		t.Fatalf("recorded actor = %q; want %q", answered.Actor, responderServiceID)
	}
	if answered.AnsweredAt == nil || answered.AnsweredAt.IsZero() {
		t.Fatalf("answered request has no answered_at: %+v", answered)
	}
	if answered.Version <= currentAction.Version || settled.Version <= current.Version {
		t.Fatalf("settled versions = execution %d action %d; want both advanced past execution %d action %d",
			settled.Version, answered.Version, current.Version, currentAction.Version)
	}

	// The request is no longer current in the served inbox.
	if _, found := findRequestInbox(inboxRequests(t, harness, serviceToken), started.ExecutionID, action.ActionID); found {
		t.Fatal("the served inbox still lists the answered request")
	}

	// The identical replay is served from the receipt: the same committed
	// versions, no replacement answer.
	replay, committed := handler.replay()
	replayed, publicError := harness.REST(serviceToken, replay.Operation, json.RawMessage(replay.Request.Input),
		replay.Request.ExpectedVersion, replay.Request.IdempotencyKey)
	if publicError.Code != "" {
		t.Fatalf("identical resolve replay refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var committedResult api.ExecutionRequestResolveResult
	harness.Decode(committed, &committedResult)
	var replayedResult api.ExecutionRequestResolveResult
	harness.Decode(replayed, &replayedResult)
	if replayedResult != committedResult {
		t.Fatalf("replay result = %+v; want the committed %+v", replayedResult, committedResult)
	}

	// A fresh adapter scan converges without a second resolve.
	if err := adapter.RunOnce(ctx); err != nil {
		t.Fatalf("converging RunOnce: %v", err)
	}
	if calls := handler.resolveCount(); calls != 1 {
		t.Fatalf("resolve calls = %d; want no replacement answer after convergence", calls)
	}
}
