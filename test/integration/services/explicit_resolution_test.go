//go:build integration

package services

// Explicit service resolution on the real factory composition.
//
// A request inbox item is self-sufficient: it carries the immutable typed
// subject, the declared response output names and the action deadline.
// execution.request.resolve is action-only. This scenario proves the two
// together decide an answer without a second read: the service reads the
// declaration from its served inbox item and submits an explicit response
// through the served resolve operation.
//
// The adapter deliberately submits no automatic response: a response policy is
// a separately specified decision boundary. Every explicit value below is
// supplied by the test principal through the public operation, exactly as a
// service that owns such a policy would.
//
// Actor attribution (the service principal), one settlement, identical replay,
// changed-content refusal, wrong-service and human-role refusal and live
// disablement are all observed through the served edges; nothing here reaches
// into Shell or Userland internals.

import (
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	explicitResolutionServiceID = "explicit-resolution-svc"
	explicitWrongServiceID      = "explicit-resolution-other"
	explicitDisabledServiceID   = "explicit-resolution-disabled"
)

// explicitPipelineAction is one request action that declares a literal subject
// and its response output names.
type explicitPipelineAction struct {
	Name    string         `yaml:"name"`
	Request string         `yaml:"request"`
	Subject map[string]any `yaml:"subject,omitempty"`
	Outputs []string       `yaml:"outputs,omitempty"`
	Timeout string         `yaml:"timeout,omitempty"`
}

// explicitPipelineDoc is the exact authored pipeline document.
type explicitPipelineDoc struct {
	Actions []explicitPipelineAction `yaml:"actions"`
}

// explicitDeclaration is the literal subject every request in this scenario
// declares.
var explicitDeclaration = map[string]string{"operation": "explicit.resolve", "ticket": "visa-7"}

// applyExplicitPipeline applies one request action assigned to the service with
// the supplied declared response names.
func applyExplicitPipeline(t *testing.T, harness *factory.Harness, name, serviceID string, outputs []string) api.PipelineApplyResult {
	t.Helper()
	subject := make(map[string]any, len(explicitDeclaration))
	for key, value := range explicitDeclaration {
		subject[key] = value
	}
	document, err := yaml.Marshal(explicitPipelineDoc{Actions: []explicitPipelineAction{{
		Name:    "answer",
		Request: "service/" + serviceID,
		Subject: subject,
		Outputs: outputs,
		Timeout: responderRequestTimeout,
	}}})
	if err != nil {
		t.Fatalf("marshal explicit-resolution pipeline: %v", err)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// explicitItem reads the served inbox item for one request and proves it
// carries the immutable subject, the declared output names and the deadline.
func explicitItem(t *testing.T, harness *factory.Harness, token, executionID, actionID, serviceID string, outputs []string) api.InboxItem {
	t.Helper()
	inbox := inboxRequests(t, harness, token)
	item, found := findRequestInbox(inbox, executionID, actionID)
	if !found {
		t.Fatalf("service inbox does not contain %s/%s: %+v", executionID, actionID, inbox.Items)
	}
	if item.Kind != "request" || item.Assignment != "service/"+serviceID {
		t.Fatalf("inbox item kind/assignment = %q/%q; want request/service/%s", item.Kind, item.Assignment, serviceID)
	}
	if item.Version == 0 || item.At.IsZero() {
		t.Fatalf("inbox item lacks its action version/deadline: %+v", item)
	}
	if got := subjectValues(t, item.Subject); !reflect.DeepEqual(got, explicitDeclaration) {
		t.Fatalf("inbox subject = %v; want the immutable declaration %v", got, explicitDeclaration)
	}
	if !reflect.DeepEqual(item.Outputs, outputs) {
		t.Fatalf("inbox declared outputs = %v; want %v", item.Outputs, outputs)
	}
	return item
}

// subjectValues flattens one api subject to name->literal value.
func subjectValues(t *testing.T, subject *api.Subject) map[string]string {
	t.Helper()
	if subject == nil {
		t.Fatal("request inbox item carries no subject")
	}
	if len(subject.Fields) == 0 {
		t.Fatal("request inbox item carries an empty subject")
	}
	values := make(map[string]string, len(subject.Fields))
	for _, field := range subject.Fields {
		if field.Kind != "string" {
			t.Fatalf("subject field %q kind = %q; want a resolved string literal", field.Name, field.Kind)
		}
		values[field.Name] = field.Value
	}
	return values
}

// declaredResponse builds the explicit response from the output names the inbox
// item declared, so the answer cannot use a private guess at the schema.
func declaredResponse(item api.InboxItem) map[string]string {
	response := make(map[string]string, len(item.Outputs))
	for _, name := range item.Outputs {
		response[name] = "supplied-" + name
	}
	return response
}

// resolveRequest submits one served resolve with explicit outputs and returns
// the committed result or the public refusal.
func resolveRequest(t *testing.T, harness *factory.Harness, token, executionID, actionID string, expectedExecution, expectedAction uint64, decision string, outputs map[string]string, key string) (api.ExecutionRequestResolveResult, api.Error) {
	t.Helper()
	result, publicError := harness.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              actionID,
		ExpectedActionVersion: expectedAction,
		Decision:              decision,
		Outputs:               outputs,
	}, &expectedExecution, key)
	if publicError.Code != "" {
		return api.ExecutionRequestResolveResult{}, publicError
	}
	var resolved api.ExecutionRequestResolveResult
	harness.Decode(result, &resolved)
	return resolved, api.Error{}
}

// resolvedEvent returns the one committed request.resolved event for an
// execution.
func resolvedEvent(t *testing.T, harness *factory.Harness, executionID string) api.Event {
	t.Helper()
	for _, event := range readProjectEvents(t, harness, "request.resolved").Events {
		if event.Type == "request.resolved" && event.SubjectID == executionID {
			return event
		}
	}
	t.Fatalf("execution %s has no committed request.resolved event", executionID)
	return api.Event{}
}

// requireExplicitAnswer proves one settled request carries exactly the explicit
// response, the pass decision and the service principal as its actor.
func requireExplicitAnswer(t *testing.T, harness *factory.Harness, execution api.ExecutionInspectResult, serviceID string, response map[string]string) api.ExecutionAction {
	t.Helper()
	action := actionByName(t, execution, "answer")
	if action.Decision != "pass" {
		t.Fatalf("recorded decision = %q; want pass (action=%+v)", action.Decision, action)
	}
	if action.Actor != serviceID {
		t.Fatalf("recorded actor = %q; want the service principal %q", action.Actor, serviceID)
	}
	if action.AnsweredAt == nil || action.AnsweredAt.IsZero() {
		t.Fatalf("answered request has no answered_at: %+v", action)
	}
	if !reflect.DeepEqual(action.Outputs, response) {
		t.Fatalf("recorded outputs = %v; want the explicit declared response %v", action.Outputs, response)
	}
	event := resolvedEvent(t, harness, execution.ExecutionID)
	if event.Source.PrincipalID != serviceID {
		t.Fatalf("request.resolved source principal = %q; want the service principal %q", event.Source.PrincipalID, serviceID)
	}
	if got := countResolvedEvents(t, harness, execution.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one settlement", got)
	}
	return action
}

// TestExplicitServiceResolution proves a service decides from the served inbox
// declaration and supplies an explicit declared response.
func TestExplicitServiceResolution(t *testing.T) {
	harness := factory.Start(t)
	access := createResponderService(t, harness, explicitResolutionServiceID)
	outputs := []string{"summary", "verdict"}

	t.Run("explicit answer, identical replay and changed-content refusal", func(t *testing.T) {
		applied := applyExplicitPipeline(t, harness, "explicit-answer", explicitResolutionServiceID, outputs)
		started := startPipeline(t, harness, applied.PipelineID, "start-explicit-answer")
		waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
		action := actionByName(t, waiting, "answer")

		item := explicitItem(t, harness, access.token, started.ExecutionID, action.ActionID, explicitResolutionServiceID, outputs)
		if action.Deadline == nil || !action.Deadline.Equal(item.At) {
			t.Fatalf("inbox deadline %v does not match the action deadline %v", item.At, action.Deadline)
		}
		response := declaredResponse(item)

		resolved, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-answer")
		if refusal.Code != "" {
			t.Fatalf("explicit resolve refused: %s (%s)", refusal.Code, refusal.Message)
		}
		if resolved.ExecutionVersion == 0 || resolved.ActionVersion == 0 {
			t.Fatalf("explicit resolve result = %+v; want both committed versions", resolved)
		}
		settled := waitExecutionSettled(t, harness, started.ExecutionID)
		if settled.State != "succeeded" {
			t.Fatalf("execution state = %q; want succeeded (failure=%+v)", settled.State, settled.Failure)
		}
		answered := requireExplicitAnswer(t, harness, settled, explicitResolutionServiceID, response)
		if answered.ActionID != action.ActionID {
			t.Fatalf("answered action = %q; want %q", answered.ActionID, action.ActionID)
		}

		// Identical replay: byte-identical content and key converge on the
		// original settlement and commit nothing new.
		replayed, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-answer")
		if refusal.Code != "" {
			t.Fatalf("identical replay refused: %s (%s)", refusal.Code, refusal.Message)
		}
		if replayed != resolved {
			t.Fatalf("identical replay result = %+v; want the original %+v", replayed, resolved)
		}
		afterReplay := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		if afterReplay.Version != settled.Version {
			t.Fatalf("execution version after identical replay = %d; want %d", afterReplay.Version, settled.Version)
		}
		requireExplicitAnswer(t, harness, afterReplay, explicitResolutionServiceID, response)

		// Changed content under the same request identity is refused, and the
		// settled answer is never replaced.
		changed := map[string]string{"summary": response["summary"], "verdict": response["verdict"] + "-changed"}
		if _, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", changed, "explicit-answer"); refusal.Code != api.CodeChangedIdempotencyContent {
			t.Fatalf("changed-content replay refusal = %q; want %q", refusal.Code, api.CodeChangedIdempotencyContent)
		}
		afterChange := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		if afterChange.Version != settled.Version {
			t.Fatalf("execution version after changed-content refusal = %d; want %d", afterChange.Version, settled.Version)
		}
		requireExplicitAnswer(t, harness, afterChange, explicitResolutionServiceID, response)
	})

	t.Run("wrong service and human role are refused", func(t *testing.T) {
		other := createResponderService(t, harness, explicitWrongServiceID)
		applied := applyExplicitPipeline(t, harness, "explicit-refusals", explicitResolutionServiceID, outputs)
		started := startPipeline(t, harness, applied.PipelineID, "start-explicit-refusals")
		waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
		action := actionByName(t, waiting, "answer")
		item := explicitItem(t, harness, access.token, started.ExecutionID, action.ActionID, explicitResolutionServiceID, outputs)
		response := declaredResponse(item)

		refusals := []struct {
			name  string
			token string
			key   string
		}{
			{"foreign service", other.token, "explicit-refused-foreign"},
			{"human engineer", harness.ProjectToken, "explicit-refused-human"},
		}
		for _, attempt := range refusals {
			if _, refusal := resolveRequest(t, harness, attempt.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, attempt.key); refusal.Code != api.CodeForbidden {
				t.Fatalf("%s resolve refusal = %q (%s); want %q", attempt.name, refusal.Code, refusal.Message, api.CodeForbidden)
			}
		}
		// The assigned service still cannot answer outside the declaration: an
		// undeclared response key is refused before any settlement.
		undeclared := map[string]string{"summary": response["summary"], "verdict": response["verdict"], "extra": "not-declared"}
		if _, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", undeclared, "explicit-undeclared"); refusal.Code != api.CodeInvalidInput {
			t.Fatalf("undeclared response refusal = %q (%s); want %q", refusal.Code, refusal.Message, api.CodeInvalidInput)
		}

		current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		if current.State != "running" {
			t.Fatalf("execution state after refusals = %q; want running", current.State)
		}
		if got := countResolvedEvents(t, harness, started.ExecutionID); got != 0 {
			t.Fatalf("committed request.resolved events after refusals = %d; want none", got)
		}

		// The exact assigned service answers with the explicit declaration.
		if _, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-after-refusals"); refusal.Code != "" {
			t.Fatalf("assigned service resolve refused: %s (%s)", refusal.Code, refusal.Message)
		}
		settled := waitExecutionSettled(t, harness, started.ExecutionID)
		requireExplicitAnswer(t, harness, settled, explicitResolutionServiceID, response)
	})

	t.Run("disabled service cannot resolve", func(t *testing.T) {
		access := createResponderService(t, harness, explicitDisabledServiceID)
		applied := applyExplicitPipeline(t, harness, "explicit-disabled", explicitDisabledServiceID, outputs)
		started := startPipeline(t, harness, applied.PipelineID, "start-explicit-disabled")
		waiting := waitRequestRunning(t, harness, started.ExecutionID, "answer")
		action := actionByName(t, waiting, "answer")
		item := explicitItem(t, harness, access.token, started.ExecutionID, action.ActionID, explicitDisabledServiceID, outputs)
		response := declaredResponse(item)

		disabled := disableService(t, harness, explicitDisabledServiceID, access.version, "disable-explicit-resolution")
		if _, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-disabled"); refusal.Code != api.CodeUnauthenticated {
			t.Fatalf("disabled-service resolve refusal = %q (%s); want %q", refusal.Code, refusal.Message, api.CodeUnauthenticated)
		}
		current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		if current.State != "running" {
			t.Fatalf("execution state after disabled-service refusal = %q; want running", current.State)
		}
		if got := countResolvedEvents(t, harness, started.ExecutionID); got != 0 {
			t.Fatalf("committed request.resolved events after disabled-service refusal = %d; want none", got)
		}

		// Enablement never resurrects a revoked credential; only a fresh
		// credential can answer the same still-current request, so the refusal
		// came from live authority.
		enabled := enableService(t, harness, explicitDisabledServiceID, disabled.Version, "re-enable-explicit-resolution")
		if _, refusal := resolveRequest(t, harness, access.token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-reenabled-revoked"); refusal.Code != api.CodeUnauthenticated {
			t.Fatalf("revoked-credential resolve after re-enable refusal = %q (%s); want %q", refusal.Code, refusal.Message, api.CodeUnauthenticated)
		}
		token, _ := issueServiceCredential(t, harness, explicitDisabledServiceID, enabled.Version, "reissue-explicit-resolution")
		if _, refusal := resolveRequest(t, harness, token, started.ExecutionID, action.ActionID, waiting.Version, action.Version, "pass", response, "explicit-reenabled"); refusal.Code != "" {
			t.Fatalf("re-enabled service resolve refused: %s (%s)", refusal.Code, refusal.Message)
		}
		settled := waitExecutionSettled(t, harness, started.ExecutionID)
		requireExplicitAnswer(t, harness, settled, explicitDisabledServiceID, response)
	})
}
