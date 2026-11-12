package factory

// Unit tests for the deterministic upstream model provider fixture.
//
// These exercise the fixture's fail-closed contract without any real
// infrastructure: a request advances the script only when its declarations and
// correlated history match, an exhausted script answers nothing, and a
// canceled or paused request is abandoned instead of consuming an exchange.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	unitModelCallID     = "call-ask-1"
	unitModelArguments  = `{"question":"Choose a branch","assignment":"role/reviewer"}`
	unitModelProviderID = "kd_task_0ask"
	unitModelOtherTool  = "repo_read"
)

// unitModelTool declares one provider-side tool.
func unitModelTool(name string) api.ChatTool {
	return api.ChatTool{
		Type: "function",
		Function: api.ChatToolFunction{
			Name:       name,
			Parameters: json.RawMessage(`{"type":"object"}`),
		},
	}
}

// unitModelBody renders one valid completion request.
func unitModelBody(t *testing.T, tools []string, correlate bool) []byte {
	t.Helper()
	declarations := make([]api.ChatTool, 0, len(tools))
	for _, name := range tools {
		declarations = append(declarations, unitModelTool(name))
	}
	messages := api.Messages{{Role: "user", Content: "choose a branch"}}
	if correlate {
		messages = append(messages,
			api.ChatMessage{
				Role:        "assistant",
				ContentNull: true,
				ToolCalls: []api.ChatToolCall{{
					ID:   unitModelCallID,
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      unitModelProviderID,
						Arguments: unitModelArguments,
					},
				}},
			},
			api.ChatMessage{Role: "tool", ToolCallID: unitModelCallID, Content: "role/reviewer"},
		)
	}
	body, err := json.Marshal(api.ChatCompletionRequest{
		Model:     ModelUpstreamModel,
		Messages:  messages,
		MaxTokens: 64,
		Tools:     declarations,
	})
	if err != nil {
		t.Fatalf("marshal unit model request: %v", err)
	}
	return body
}

// unitToolReply is the scripted tool-calling completion.
func unitToolReply() ModelReply {
	return ModelReply{
		ContentNull: true,
		ToolCalls: []api.ChatToolCall{{
			ID:       unitModelCallID,
			Type:     "function",
			Function: api.ChatToolCallFunction{Name: unitModelProviderID, Arguments: unitModelArguments},
		}},
		FinishReason: "tool_calls",
		Usage:        api.ChatUsage{PromptTokens: 17, CompletionTokens: 9, TotalTokens: 26},
	}
}

// unitToolExchange scripts the tool-calling exchange with correlated history.
func unitToolExchange() ModelExchange {
	return ModelExchange{
		Name:   "ask",
		Expect: ModelExpectation{Tools: []string{unitModelProviderID}, ToolCallID: unitModelCallID},
		Reply:  unitToolReply(),
	}
}

// postUnitModel sends one request to the fixture with the given credential.
func postUnitModel(ctx context.Context, t *testing.T, fixture *ModelFixture, body []byte, credential string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.Endpoint(), strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build unit provider request: %v", err)
	}
	request.Header.Set("Content-Type", api.MediaType)
	request.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Transport: fixture.Transport()}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("unit provider request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read unit provider response: %v", err)
	}
	return response.StatusCode, data
}

// TestModelFixtureRequiresExpectedExchange proves a request advances the script
// only when its declarations, history correlation and provider credential all
// match. Every unexpected request fails closed and leaves the script untouched.
func TestModelFixtureRequiresExpectedExchange(t *testing.T) {
	t.Run("unexpected tool declaration is refused", func(t *testing.T) {
		fixture := NewModelFixture(t)
		fixture.Script(unitToolExchange())

		status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelOtherTool}, true), fixture.credential)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("unexpected declaration status = %d; want %d", status, http.StatusUnprocessableEntity)
		}
		if len(fixture.Requests()) != 0 || fixture.Exhausted() {
			t.Fatalf("refused declaration consumed the script: requests=%d exhausted=%v", len(fixture.Requests()), fixture.Exhausted())
		}
		if failures := fixture.TakeFailures(); len(failures) != 1 {
			t.Fatalf("refused declaration failures = %v; want one", failures)
		}

		status, data := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential)
		if status != http.StatusOK {
			t.Fatalf("matched declaration status = %d; want 200; body=%s", status, string(data))
		}
		var completion api.ChatCompletionResponse
		if err := json.Unmarshal(data, &completion); err != nil {
			t.Fatalf("decode completion: %v", err)
		}
		if completion.Choices[0].FinishReason != "tool_calls" || !completion.Choices[0].Message.ContentNull {
			t.Fatalf("completion = %+v; want a null-content tool_calls completion", completion)
		}
		fixture.AssertCredentialAbsent(t, fmt.Sprintf("%v", fixture.Requests()), fmt.Sprintf("%v", fixture.Failures()))
		fixture.AssertSatisfied(t)
	})

	t.Run("missing correlated history is refused", func(t *testing.T) {
		fixture := NewModelFixture(t, unitToolExchange())

		status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, false), fixture.credential)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("uncorrelated history status = %d; want %d", status, http.StatusUnprocessableEntity)
		}
		if fixture.Exhausted() {
			t.Fatal("uncorrelated history consumed the script")
		}
		fixture.TakeFailures()

		status, _ = postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential)
		if status != http.StatusOK {
			t.Fatalf("correlated history status = %d; want 200", status)
		}
		fixture.AssertSatisfied(t)
	})

	t.Run("wrong provider credential is refused", func(t *testing.T) {
		fixture := NewModelFixture(t, unitToolExchange())

		status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), "wrong-provider-credential")
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong credential status = %d; want %d", status, http.StatusUnauthorized)
		}
		if fixture.Exhausted() {
			t.Fatal("wrong credential consumed the script")
		}
		fixture.TakeFailures()

		status, _ = postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential)
		if status != http.StatusOK {
			t.Fatalf("correct credential status = %d; want 200", status)
		}
		fixture.AssertSatisfied(t)
	})

	t.Run("exhaustion fails closed", func(t *testing.T) {
		fixture := NewModelFixture(t)

		status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, nil, false), fixture.credential)
		if status != http.StatusServiceUnavailable {
			t.Fatalf("exhausted status = %d; want %d", status, http.StatusServiceUnavailable)
		}
		if len(fixture.Requests()) != 0 {
			t.Fatalf("exhausted fixture accepted %d request(s)", len(fixture.Requests()))
		}
		if failures := fixture.Failures(); len(failures) != 1 {
			t.Fatalf("exhausted failures = %v; want one", failures)
		}
		fixture.AssertCredentialAbsent(t, strings.Join(fixture.Failures(), " "), fixture.Endpoint())
	})

	t.Run("unexpected request after the script is refused", func(t *testing.T) {
		fixture := NewModelFixture(t, ModelExchange{
			Name:   "final",
			Expect: ModelExpectation{},
			Reply:  ModelReply{Content: "done", FinishReason: "stop", Usage: api.ChatUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
		})
		if status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, nil, false), fixture.credential); status != http.StatusOK {
			t.Fatalf("scripted request status = %d; want 200", status)
		}
		if status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, nil, false), fixture.credential); status != http.StatusServiceUnavailable {
			t.Fatalf("post-script request status = %d; want %d", status, http.StatusServiceUnavailable)
		}
		if len(fixture.Requests()) != 1 {
			t.Fatalf("accepted requests = %d; want only the scripted one", len(fixture.Requests()))
		}
	})
}

// TestModelFixtureExhaustionAndCancellation proves the deterministic barriers:
// waiters observe cancellation, a paused request is abandoned without consuming
// an exchange, and an exhausted script never synthesizes a completion.
func TestModelFixtureExhaustionAndCancellation(t *testing.T) {
	t.Run("waiter observes cancellation", func(t *testing.T) {
		fixture := NewModelFixture(t, unitToolExchange())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := fixture.WaitForRequests(ctx, 1); err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("WaitForRequests canceled = %v; want context cancellation", err)
		}
	})

	t.Run("paused request cancellation does not consume the script", func(t *testing.T) {
		fixture := NewModelFixture(t, unitToolExchange())
		release := fixture.PauseNextRequest()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.Endpoint(), strings.NewReader(string(unitModelBody(t, []string{unitModelProviderID}, true))))
			if err != nil {
				done <- err
				return
			}
			request.Header.Set("Content-Type", api.MediaType)
			request.Header.Set("Authorization", "Bearer "+fixture.credential)
			response, err := (&http.Client{Transport: fixture.Transport()}).Do(request)
			if err == nil {
				_ = response.Body.Close()
			}
			done <- err
		}()
		arrival, cancelArrival := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelArrival()
		if err := fixture.WaitForArrivals(arrival, 1); err != nil {
			t.Fatalf("WaitForArrivals: %v", err)
		}
		cancel()
		// Abandoning the paused request is the deterministic cancellation
		// boundary: the handler observed the canceled request, but it never
		// matched or consumed an exchange.
		release(false)
		handled, cancelHandled := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelHandled()
		if err := fixture.WaitForHandled(handled, 1); err != nil {
			t.Fatalf("WaitForHandled: %v", err)
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled provider request unexpectedly succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("canceled provider request did not return")
		}
		if len(fixture.Requests()) != 0 || fixture.Exhausted() {
			t.Fatalf("canceled request consumed the script: requests=%d exhausted=%v", len(fixture.Requests()), fixture.Exhausted())
		}
		if failures := fixture.Failures(); len(failures) != 0 {
			t.Fatalf("canceled request was recorded as a failure: %v", failures)
		}

		if status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential); status != http.StatusOK {
			t.Fatalf("request after cancellation status = %d; want 200", status)
		}
		fixture.AssertSatisfied(t)
	})
}

// TestModelFixtureCanceledPauseIsAbandonedWithoutAResponse pins the fixture's
// cancellation boundary at the handler itself. A paused request whose context
// is canceled must leave the script untouched and must never yield an HTTP
// response: a handler that merely returns lets net/http write an implicit 200,
// and the HTTP transport deliberately prefers a response that races the
// caller's cancellation, so the canceled request could be observed as
// successful. The gate release and the cancellation are both ready in every
// attempt, so the handler's select may pick either case; every branch must
// abandon the request.
func TestModelFixtureCanceledPauseIsAbandonedWithoutAResponse(t *testing.T) {
	fixture := NewModelFixture(t, unitToolExchange())
	for attempt := range 32 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		request := httptest.NewRequest(http.MethodPost, ModelProviderPath,
			strings.NewReader(string(unitModelBody(t, []string{unitModelProviderID}, true))))
		request.Header.Set("Content-Type", api.MediaType)
		request.Header.Set("Authorization", "Bearer "+fixture.credential)
		request = request.WithContext(ctx)

		gate := make(chan bool, 1)
		gate <- attempt%2 == 0
		fixture.mu.Lock()
		fixture.gate = gate
		fixture.mu.Unlock()

		aborted := func() (aborted bool) {
			defer func() { aborted = recover() == http.ErrAbortHandler }()
			fixture.handle(httptest.NewRecorder(), request)
			return false
		}()
		if !aborted {
			t.Fatalf("attempt %d: canceled paused request was answered instead of aborted", attempt)
		}
	}
	if len(fixture.Requests()) != 0 || fixture.Exhausted() {
		t.Fatalf("canceled requests consumed the script: requests=%d exhausted=%v", len(fixture.Requests()), fixture.Exhausted())
	}
	if failures := fixture.Failures(); len(failures) != 0 {
		t.Fatalf("canceled requests were recorded as failures: %v", failures)
	}
}

// TestModelFixtureWaitSamplesUnderTheFixtureLock pins the barrier invariant that
// closes the paused-cancellation join race: the completion count and the
// notification channel must be sampled in one critical section. When they were
// sampled separately, a completion that signalled between the two samples was
// lost: WaitForHandled then blocked until its own deadline even though the
// handler had already left (the load-sensitive failure this test guards).
func TestModelFixtureWaitSamplesUnderTheFixtureLock(t *testing.T) {
	fixture := &ModelFixture{notify: make(chan struct{}), closed: make(chan struct{})}
	sampled := false
	count := func() int {
		if fixture.mu.TryLock() {
			fixture.mu.Unlock()
			t.Fatal("wait sampled the completion count without holding the fixture lock")
		}
		sampled = true
		return 1
	}
	if err := fixture.wait(context.Background(), count, 1); err != nil {
		t.Fatalf("wait = %v; want the sampled completion", err)
	}
	if !sampled {
		t.Fatal("wait returned without sampling the completion count")
	}
}

// TestModelFixtureScriptSurvivesControllerRebuild proves a controller rebuild
// (which asks the fixture for a fresh transport) never resets the scripted
// exchange position or the accepted-request capture. A restart that lost the
// fixture state would re-serve the first exchange and never reach the final
// content.
func TestModelFixtureScriptSurvivesControllerRebuild(t *testing.T) {
	fixture := NewModelFixture(t, unitToolExchange(), unitToolExchange())
	if status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential); status != http.StatusOK {
		t.Fatalf("first scripted request status = %d; want 200", status)
	}
	before := len(fixture.Requests())
	// Every controller construction adopts a freshly built transport.
	_ = fixture.Transport()
	_ = fixture.Transport()
	if got := len(fixture.Requests()); got != before {
		t.Fatalf("transport rebuild changed accepted requests from %d to %d", before, got)
	}
	if fixture.Exhausted() {
		t.Fatal("transport rebuild exhausted the script")
	}
	if status, _ := postUnitModel(context.Background(), t, fixture, unitModelBody(t, []string{unitModelProviderID}, true), fixture.credential); status != http.StatusOK {
		t.Fatalf("second scripted request status = %d; want 200", status)
	}
	fixture.AssertSatisfied(t)
}

// TestWithModelFixtureIsOptIn proves the default factory asks for no model
// configuration at all. The option is the only way to enable the surface.
func TestWithModelFixtureIsOptIn(t *testing.T) {
	scope := tenancy.Scope{TenantID: "tenant-i04", ProjectID: "project-i04"}
	var options Options
	if config := options.composeModelsConfig(scope); config != nil {
		t.Fatal("the default factory composed a model configuration")
	}
	fixture := NewModelFixture(t)
	options.apply(WithModelFixture(fixture))
	config := options.composeModelsConfig(scope)
	if config == nil {
		t.Fatal("WithModelFixture did not enable the model configuration")
	}
	if len(config.Models) != 1 || config.Models[0].Model != ModelAlias || config.Models[0].UpstreamModel != ModelUpstreamModel {
		t.Fatalf("model configuration = %+v; want the one fixture alias/upstream pair", config.Models)
	}
	if len(config.SecretRules) != 1 || len(config.SecretRules[0].Names) != 1 || config.SecretRules[0].Names[0] != ModelSecretName {
		t.Fatalf("gateway secret rules = %+v; want exactly the fixture credential", config.SecretRules)
	}
	if config.Transport == nil {
		t.Fatal("model configuration has no CA-verifying transport")
	}
}
