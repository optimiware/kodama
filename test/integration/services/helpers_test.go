//go:build integration

package services

// Shared fixtures for the independent services integration suite.
//
// Everything here crosses the real served public edge: the service credential is
// issued through service.credential.issue, the request is created by a real
// pipeline start, and the responder adapter/app reaches the composition through
// the public operation handler the harness exposes. The only simulated pieces
// are the explicitly labelled fault-injection seams the scenarios need (a
// failing notification source, a lost resolve response, a delayed inbox page and
// a barrier on the outgoing resolve); none of them fabricates a product success.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	serviceadapter "github.com/optimiware/kodama/internal/userland/services"
	"github.com/optimiware/kodama/test/support/factory"
)

// responderRequestTimeout keeps every request deadline clear of the transaction
// clock for the duration of one scenario.
const responderRequestTimeout = "1h"

// pipelineDoc is the API-posted pipeline document this suite authors.
type pipelineDoc struct {
	Actions []pipelineActionDoc `yaml:"actions"`
}

// pipelineActionDoc is one action of the API-posted pipeline. Only a
// service-assigned request action is used here.
type pipelineActionDoc struct {
	Name    string         `yaml:"name"`
	Request string         `yaml:"request"`
	Subject map[string]any `yaml:"subject,omitempty"`
	Timeout string         `yaml:"timeout,omitempty"`
}

// marshalPipeline renders one exact pipeline YAML byte sequence.
func marshalPipeline(t *testing.T, document pipelineDoc) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal pipeline yaml: %v", err)
	}
	return encoded
}

// applyResponderPipeline applies one output-free request action assigned to the
// exact service. The request declares a minimal literal subject because
// settlement requires a resolved subject, and declares no response fields
// because the responder adapter submits none. The served inbox does expose
// declared output names; TestExplicitServiceResolution covers a request that
// declares them.
func applyResponderPipeline(t *testing.T, harness *factory.Harness, name, serviceID string) api.PipelineApplyResult {
	t.Helper()
	document := marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
		Name:    "answer",
		Request: "service/" + serviceID,
		Subject: map[string]any{"operation": "responder.rescan"},
		Timeout: responderRequestTimeout,
	}}})
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// readPipeline reads one stored pipeline definition.
func readPipeline(t *testing.T, harness *factory.Harness, pipelineID string) api.PipelineReadResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var read api.PipelineReadResult
	harness.Decode(result, &read)
	return read
}

// startPipeline starts one stored pipeline over REST at its currently observed
// stored version.
func startPipeline(t *testing.T, harness *factory.Harness, pipelineID, key string) api.PipelineStartResult {
	t.Helper()
	stored := readPipeline(t, harness, pipelineID)
	if stored.Version == 0 {
		t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	harness.Decode(result, &started)
	return started
}

// inspectExecution reads one execution through the public API.
func inspectExecution(t *testing.T, harness *factory.Harness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	result := harness.MustREST(token, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	var inspected api.ExecutionInspectResult
	harness.Decode(result, &inspected)
	return inspected
}

// actionByName returns one action observation by its admitted name.
func actionByName(t *testing.T, execution api.ExecutionInspectResult, name string) api.ExecutionAction {
	t.Helper()
	for _, action := range execution.Actions {
		if action.Name == name {
			return action
		}
	}
	t.Fatalf("execution %s has no action %q; actions=%+v", execution.ExecutionID, name, execution.Actions)
	return api.ExecutionAction{}
}

// waitRequestRunning waits for one named action to be the running action of a
// running execution and returns that observation. Most callers wait on a
// request action; the check itself does not depend on the action kind.
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

// waitExecutionState waits for one execution to reach a terminal state.
func waitExecutionState(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
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
		switch last.State {
		case "succeeded", "failed", "cancelled":
			return true
		default:
			return false
		}
	})
	return last
}

// waitExecutionSettled waits for one execution to reach a terminal state and
// for its served view to become quiescent: the independent cleanup record is
// settled and two consecutive reads observe the same execution version. A
// terminal commit and the cleanup intent that follows it are separate commits,
// so this barrier is what makes a later version comparison meaningful.
func waitExecutionSettled(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	waitExecutionState(t, harness, executionID)
	var last api.ExecutionInspectResult
	var lastVersion uint64
	haveBaseline := false
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			haveBaseline = false
			return false
		}
		var current api.ExecutionInspectResult
		harness.Decode(result, &current)
		switch current.State {
		case "succeeded", "failed", "cancelled":
		default:
			haveBaseline = false
			last = current
			return false
		}
		if current.Cleanup.State != "complete" && current.Cleanup.State != "none" {
			haveBaseline = false
			last = current
			return false
		}
		if haveBaseline && current.Version == lastVersion {
			last = current
			return true
		}
		haveBaseline = true
		lastVersion = current.Version
		last = current
		return false
	})
	return last
}

// inboxRequests reads the request-kind inbox for one principal through the
// served task.inbox operation.
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

// readProjectEvents reads committed events of the given types through the
// public event read. The shared project token is an engineer, which may read
// project events.
func readProjectEvents(t *testing.T, harness *factory.Harness, types ...string) api.EventReadResult {
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

// countResolvedEvents counts committed request.resolved events for one
// execution. A settlement commits exactly one owner event keyed by the
// execution identity.
func countResolvedEvents(t *testing.T, harness *factory.Harness, executionID string) int {
	t.Helper()
	events := readProjectEvents(t, harness, "request.resolved")
	count := 0
	for _, event := range events.Events {
		if event.Type == "request.resolved" && event.SubjectID == executionID {
			count++
		}
	}
	return count
}

// serviceAccess is one created service account and its current credential.
type serviceAccess struct {
	id      string
	version uint64
	token   string
}

// createResponderService creates one service account with the operator role and
// issues its first credential through the real admin operations. Both calls are
// mutations of the same service row, so each later call uses the version the
// previous result committed.
func createResponderService(t *testing.T, harness *factory.Harness, serviceID string) serviceAccess {
	t.Helper()
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: serviceID,
		Roles:     []string{"operator"},
	}, nil, "create-"+serviceID)
	var createdResult api.ServiceCreateResult
	harness.Decode(created, &createdResult)
	token, version := issueServiceCredential(t, harness, serviceID, createdResult.Version, "issue-"+serviceID)
	return serviceAccess{id: serviceID, version: version, token: token}
}

// serviceCredentialWithRoles creates one project service with the exact roles
// and issues its one-time credential through the public operations.
func serviceCredentialWithRoles(t *testing.T, harness *factory.Harness, serviceID string, roles []string) string {
	t.Helper()
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: serviceID,
		Roles:     roles,
	}, nil, "create-"+serviceID)
	var service api.ServiceCreateResult
	harness.Decode(created, &service)
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: 3600,
	}, &service.Version, "issue-"+serviceID)
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatalf("service %s issued no credential", serviceID)
	}
	return *credential.Token
}

// issueServiceCredential issues one fresh credential at the observed service
// version and returns its one-time token and committed version.
func issueServiceCredential(t *testing.T, harness *factory.Harness, serviceID string, version uint64, key string) (string, uint64) {
	t.Helper()
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: 3600,
	}, &version, key)
	var issuedResult api.ServiceCredentialIssueResult
	harness.Decode(issued, &issuedResult)
	if issuedResult.Token == nil || *issuedResult.Token == "" {
		t.Fatal("service.credential.issue returned no credential token")
	}
	return *issuedResult.Token, issuedResult.Version
}

// disableService disables the service through the admin API and returns the
// committed service version. A disablement revokes the current credential in
// the same commit, so the previously issued token must be refused afterwards.
func disableService(t *testing.T, harness *factory.Harness, serviceID string, version uint64, key string) api.ServiceDisableResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "service.disable", api.ServiceDisableRequest{
		ServiceID: serviceID,
	}, &version, key)
	var disabled api.ServiceDisableResult
	harness.Decode(result, &disabled)
	if disabled.State != "disabled" {
		t.Fatalf("service.disable state = %q; want disabled", disabled.State)
	}
	if disabled.RevokedCredentials == 0 {
		t.Fatalf("service.disable revoked %d credentials; want the live credential revoked", disabled.RevokedCredentials)
	}
	return disabled
}

// enableService re-enables the service through the admin API and returns the
// committed service version. Enablement never resurrects a revoked credential.
func enableService(t *testing.T, harness *factory.Harness, serviceID string, version uint64, key string) api.ServiceEnableResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "service.enable", api.ServiceEnableRequest{
		ServiceID: serviceID,
	}, &version, key)
	var enabled api.ServiceEnableResult
	harness.Decode(result, &enabled)
	if enabled.State != "enabled" {
		t.Fatalf("service.enable state = %q; want enabled", enabled.State)
	}
	return enabled
}

// faultNotificationSource is an explicitly labelled fault-injection seam: it
// makes the notification drain fail before any page is delivered. It is not a
// direct NATS subscription and proves only that the adapter's startup scan does
// not depend on notifications.
type faultNotificationSource struct{}

// Poll always fails. The scenario supplies it before the adapter starts.
func (faultNotificationSource) Poll(context.Context, string) (serviceadapter.NotificationPage, error) {
	return serviceadapter.NotificationPage{}, errors.New("fault injection: notification source unavailable at responder startup")
}

// emptyNotificationSource delivers no notification and fails nothing. It is the
// neutral hint source for scenarios that exercise revalidation rather than
// startup discovery.
type emptyNotificationSource struct{}

// Poll returns an empty page.
func (emptyNotificationSource) Poll(context.Context, string) (serviceadapter.NotificationPage, error) {
	return serviceadapter.NotificationPage{}, nil
}

// newResponderAdapter builds the responder adapter over one injected public
// handler and hint source.
func newResponderAdapter(t *testing.T, access serviceAccess, handler api.Handler, events serviceadapter.EventSource) *serviceadapter.Adapter {
	t.Helper()
	adapter, err := serviceadapter.NewAdapter(serviceadapter.AdapterConfig{
		Kind:      serviceadapter.KindResponder,
		ServiceID: id.PrincipalID(access.id),
		TenantID:  id.TenantID(factory.TenantID),
		ProjectID: id.ProjectID(factory.ProjectID),
		Handler:   handler,
		Events:    events,
		Clock:     responderClock{},
	})
	if err != nil {
		t.Fatalf("services.NewAdapter: %v", err)
	}
	return adapter
}

// responderClock is the adapter's injected time boundary.
type responderClock struct{}

// Now returns the wall clock. The responder path never reads it.
func (responderClock) Now() time.Time { return time.Now() }

// requireBrokerUnreachable proves the outage window is real: the broker's
// client port refuses connections. It is asserted separately from the
// notification fault seam, which never claims direct NATS behaviour.
func requireBrokerUnreachable(t *testing.T, harness *factory.Harness) {
	t.Helper()
	harness.Eventually(t, 30*time.Second, func() bool {
		return harness.BrokerUnreachable(t.Context())
	})
}

// responderHandler adapts one real service credential to the public operation
// boundary the adapter consumes and records exactly what crossed it.
type responderHandler struct {
	harness *factory.Harness
	token   string

	mu       sync.Mutex
	counts   map[string]int
	resolveN int
	lastErr  api.Error

	// loseFirstResolve simulates one committed resolve whose response is lost
	// in transport. It is a labelled fault-injection seam, never a product
	// success flag.
	loseFirstResolve bool

	// replayInbox delivers one genuine inbox page captured earlier through the
	// served edge. It models a scan that started before a settlement and is
	// processed after it; the page is never synthesized.
	replayInbox *api.Result
	inboxReplay bool

	inspectDone    chan struct{}
	inspectOnce    sync.Once
	blockResolve   bool
	resolveRelease <-chan struct{}
	resolveEntered chan struct{}
	resolveOnce    sync.Once
}

// newResponderHandler returns a handler with no fault seams.
func newResponderHandler(harness *factory.Harness, token string) *responderHandler {
	return &responderHandler{
		harness:        harness,
		token:          token,
		counts:         make(map[string]int),
		inspectDone:    make(chan struct{}),
		resolveEntered: make(chan struct{}),
	}
}

// Invoke implements api.Handler over the real served public edge.
func (handler *responderHandler) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	handler.mu.Lock()
	if invocation.Operation == serviceadapter.OperationInbox && handler.replayInbox != nil && !handler.inboxReplay {
		handler.inboxReplay = true
		handler.counts[invocation.Operation]++
		replay := *handler.replayInbox
		handler.mu.Unlock()
		return replay, nil
	}
	handler.counts[invocation.Operation]++
	if invocation.Operation == serviceadapter.OperationResolve {
		handler.resolveN++
	}
	handler.mu.Unlock()

	if invocation.Operation == serviceadapter.OperationResolve && handler.blockResolve {
		// The barrier sits after the adapter's inspection and before the
		// outgoing submission, so the test can change live service authority in
		// between.
		handler.resolveOnce.Do(func() { close(handler.resolveEntered) })
		select {
		case <-handler.resolveRelease:
		case <-ctx.Done():
			return api.Result{}, ctx.Err()
		}
	}

	result, publicError, err := handler.harness.Invoke(ctx, handler.token, invocation.Operation,
		json.RawMessage(invocation.Request.Input), invocation.Request.ExpectedVersion, invocation.Request.IdempotencyKey)
	if err != nil {
		return api.Result{}, err
	}
	if publicError.Code != "" {
		handler.mu.Lock()
		handler.lastErr = publicError
		handler.mu.Unlock()
		return api.Result{}, &api.Error{Code: publicError.Code, Message: publicError.Message}
	}
	if invocation.Operation == serviceadapter.OperationInspect {
		handler.inspectOnce.Do(func() { close(handler.inspectDone) })
	}
	if invocation.Operation != serviceadapter.OperationResolve {
		return result, nil
	}

	handler.mu.Lock()
	lose := handler.loseFirstResolve
	handler.loseFirstResolve = false
	handler.mu.Unlock()
	if lose {
		return api.Result{}, errors.New("fault injection: lost resolve response after the committed answer")
	}
	return result, nil
}

// count returns how many times one operation crossed the handler.
func (handler *responderHandler) count(operation string) int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.counts[operation]
}

// resolveAttempts returns the number of resolve submissions attempted.
func (handler *responderHandler) resolveAttempts() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.resolveN
}

// resolveError returns the last public refusal of the resolve operation.
func (handler *responderHandler) resolveError() api.Error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.lastErr
}

// waitSignal waits for one test-visible signal with a bounded budget.
func waitSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

// assertAnswer proves one request carries exactly the service's committed
// answer with the expected actor and decision.
func assertAnswer(t *testing.T, execution api.ExecutionInspectResult, serviceID, decision string) api.ExecutionAction {
	t.Helper()
	action := actionByName(t, execution, "answer")
	if action.Decision != decision {
		t.Fatalf("recorded decision = %q; want %q (action=%+v)", action.Decision, decision, action)
	}
	if action.Actor != serviceID {
		t.Fatalf("recorded actor = %q; want %q", action.Actor, serviceID)
	}
	if action.AnsweredAt == nil || action.AnsweredAt.IsZero() {
		t.Fatalf("answered request has no answered_at: %+v", action)
	}
	return action
}

// assertUnanswered proves one request carries no settlement at all.
func assertUnanswered(t *testing.T, execution api.ExecutionInspectResult) api.ExecutionAction {
	t.Helper()
	action := actionByName(t, execution, "answer")
	if action.State != "running" {
		t.Fatalf("request state = %q; want running", action.State)
	}
	if action.Decision != "" || action.Actor != "" || action.AnsweredAt != nil {
		t.Fatalf("request carries a settlement: %+v", action)
	}
	return action
}
