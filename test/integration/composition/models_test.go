//go:build integration

package composition

// Composition integration evidence for the optional configured model gateway:
// both public edges reach one explicitly configured, project-scoped
// provider with the configured upstream model mapping, the Shell-resolved
// provider credential and one attributable usage row; raw content capture is
// absent by default and explicit retention is honored; a malformed model
// configuration refuses startup before any provider dependency is used.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	modelAlias           = "small-model"
	modelUpstream        = "upstream-small"
	modelRetainedAlias   = "retained-model"
	modelRetainedUp      = "upstream-retained"
	modelSecretName      = id.SecretName("MODEL_API_KEY")
	modelSecretValue     = "model-provider-secret-5a7c"
	modelRetentionWindow = 10 * 24 * time.Hour
)

// modelEdgeUpstream is the deterministic local TLS provider of the composition
// suite. It records every forwarded request and answers one valid completion.
type modelEdgeUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []modelEdgeCall
}

type modelEdgeCall struct {
	Header        http.Header
	Path          string
	Model         string
	Authorization string
}

func newModelEdgeUpstream(t *testing.T) *modelEdgeUpstream {
	t.Helper()
	upstream := &modelEdgeUpstream{}
	upstream.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		var decoded struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(data, &decoded)
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, modelEdgeCall{
			Header:        request.Header.Clone(),
			Path:          request.URL.Path,
			Model:         decoded.Model,
			Authorization: request.Header.Get("Authorization"),
		})
		upstream.mu.Unlock()
		response.Header().Set("Content-Type", api.MediaType)
		_, _ = fmt.Fprintf(response,
			`{"id":"cmpl-edge-1","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"edge completion"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":6,"total_tokens":19}}`,
			decoded.Model)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (upstream *modelEdgeUpstream) url() string {
	return upstream.server.URL
}

func (upstream *modelEdgeUpstream) transport() http.RoundTripper {
	return upstream.server.Client().Transport
}

func (upstream *modelEdgeUpstream) received() []modelEdgeCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]modelEdgeCall(nil), upstream.calls...)
}

// modelEdgeConfig builds one valid configured model.
func modelEdgeConfig(scope tenancy.Scope, model, upstream, endpoint string, record bool) gateway.ModelConfig {
	return gateway.ModelConfig{
		Scope:               scope,
		Model:               model,
		UpstreamModel:       upstream,
		Endpoint:            endpoint,
		SecretName:          modelSecretName,
		MaxRequestBytes:     gateway.DefaultBounds.MaxRequestBytes,
		MaxResponseBytes:    gateway.DefaultBounds.MaxResponseBytes,
		MaxCompletionTokens: 1024,
		Timeout:             5 * time.Second,
		RecordContent:       record,
		ContentRetention:    modelRetentionWindow,
	}
}

// modelEdgeRequest is one valid non-streaming completion input.
func modelEdgeRequest(model string) api.ModelInvokeRequest {
	return api.ModelInvokeRequest{
		ProjectID: projectID,
		Model:     model,
		Messages:  []api.ChatMessage{{Role: "user", Content: "hello"}},
	}
}

// modelEdgeUsageRows reads the committed model_usage rows of one scope.
func modelEdgeUsageRows(t *testing.T, scanner store.Scanner, scope tenancy.Scope) []store.Row {
	t.Helper()
	page, err := scanner.Query(context.Background(), scope, store.ScanQuery{
		Table: gateway.UsageTable,
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("scan model_usage: %v", err)
	}
	return page.Rows
}

// modelEdgeContentRows reads the committed content_records rows of one scope.
func modelEdgeContentRows(t *testing.T, scanner store.Scanner, scope tenancy.Scope) []store.Row {
	t.Helper()
	page, err := scanner.Query(context.Background(), scope, store.ScanQuery{
		Table: gateway.ContentTable,
		Limit: 20,
	})
	if err != nil {
		t.Fatalf("scan content_records: %v", err)
	}
	return page.Rows
}

// TestConfiguredModelThroughRESTAndMCP proves the configured model is served
// identically on both public edges and that every dispatched call is
// attributed, while raw content capture stays off unless configured.
//
// Mutation reasoning: retaining the disabled binding makes both edges return
// not_found; forwarding the alias instead of the upstream model changes the
// provider body; forwarding the caller token instead of the configured secret
// changes Authorization; dropping the usage commit loses attribution; enabling
// capture unconditionally creates a content_records row for the default model.
func TestConfiguredModelThroughRESTAndMCP(t *testing.T) {
	upstream := newModelEdgeUpstream(t)
	harness := newHarness(t)
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	harness.config.Models = &app.ModelsConfig{
		Models: []gateway.ModelConfig{
			modelEdgeConfig(scope, modelAlias, modelUpstream, upstream.url()+"/v1/chat/completions", false),
			modelEdgeConfig(scope, modelRetainedAlias, modelRetainedUp, upstream.url()+"/v1/chat/completions", true),
		},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelSecretName},
		}},
		Transport: upstream.transport(),
	}
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)
	environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  string(modelSecretName),
		Value:     modelSecretValue,
	}, nil, "model-secret-write")

	request := modelEdgeRequest(modelAlias)
	restResult, restError := environment.call(projectToken, "model.invoke", request, nil, "")
	if restError.Code != "" {
		t.Fatalf("REST model.invoke code = %q message = %q", restError.Code, restError.Message)
	}
	mcpResult, mcpError, err := environment.mcpCall(projectToken, "model.invoke", request, nil, "")
	if err != nil {
		t.Fatalf("MCP model.invoke: %v", err)
	}
	if mcpError.Code != "" {
		t.Fatalf("MCP model.invoke code = %q message = %q", mcpError.Code, mcpError.Message)
	}

	var restCompletion, mcpCompletion api.ChatCompletionResponse
	environment.decode(restResult, &restCompletion)
	if err := json.Unmarshal(mcpResult.Body, &mcpCompletion); err != nil {
		t.Fatalf("decode MCP completion: %v; body=%s", err, string(mcpResult.Body))
	}
	if !reflect.DeepEqual(restCompletion, mcpCompletion) {
		t.Fatalf("REST and MCP completions differ:\nrest=%+v\nmcp=%+v", restCompletion, mcpCompletion)
	}
	if len(restCompletion.Choices) != 1 || restCompletion.Choices[0].Message.Content != "edge completion" {
		t.Fatalf("completion = %+v", restCompletion)
	}
	if restCompletion.Usage.TotalTokens != 19 {
		t.Fatalf("completion usage = %+v; want the provider's validated counts", restCompletion.Usage)
	}

	calls := upstream.received()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d; want one per public edge", len(calls))
	}
	for index, call := range calls {
		if call.Path != "/v1/chat/completions" {
			t.Fatalf("call %d path = %q; want the configured provider path", index, call.Path)
		}
		if call.Model != modelUpstream {
			t.Fatalf("call %d upstream model = %q; want the configured alias mapping %q", index, call.Model, modelUpstream)
		}
		if call.Authorization != "Bearer "+modelSecretValue {
			t.Fatalf("call %d authorization = %q; want only the Shell-resolved provider secret", index, call.Authorization)
		}
		if call.Header.Get("Authorization") == "Bearer "+projectToken {
			t.Fatalf("call %d forwarded the caller credential", index)
		}
	}

	usageRows := modelEdgeUsageRows(t, harness.store, scope)
	if len(usageRows) != 2 {
		t.Fatalf("model usage rows = %d; want one per public edge", len(usageRows))
	}
	for _, row := range usageRows {
		if row.Values["principal_id"] != "admin-1" {
			t.Fatalf("usage principal = %v; want the authenticated caller admin-1", row.Values["principal_id"])
		}
		if row.Values["execution_id"] != nil {
			t.Fatalf("human usage execution_id = %v; want nil", row.Values["execution_id"])
		}
		if row.Values["model"] != modelAlias || row.Values["upstream_model"] != modelUpstream {
			t.Fatalf("usage attribution = %v/%v; want %q/%q",
				row.Values["model"], row.Values["upstream_model"], modelAlias, modelUpstream)
		}
		if row.Values["outcome"] != string(gateway.UsageSucceeded) {
			t.Fatalf("usage outcome = %v; want succeeded", row.Values["outcome"])
		}
		if known, _ := row.Values["usage_known"].(bool); !known {
			t.Fatal("usage row is not marked known on a validated completion")
		}
		if prompt, _ := row.Values["prompt_tokens"].(int64); prompt != 13 {
			t.Fatalf("usage prompt tokens = %v; want the provider count 13", row.Values["prompt_tokens"])
		}
	}

	if records := modelEdgeContentRows(t, harness.store, scope); len(records) != 0 {
		t.Fatalf("content records after default calls = %d; raw capture is off by default", len(records))
	}

	// Explicit retention is honored only for the model that opts in.
	environment.mustInvoke(projectToken, "model.invoke", modelEdgeRequest(modelRetainedAlias), nil, "")
	records := modelEdgeContentRows(t, harness.store, scope)
	if len(records) != 1 {
		t.Fatalf("content records after the retained call = %d; want exactly 1", len(records))
	}
	record := records[0]
	if record.Values["model"] != modelRetainedAlias {
		t.Fatalf("content record model = %v; want %q", record.Values["model"], modelRetainedAlias)
	}
	if data, _ := record.Values["request_body"].([]byte); len(data) == 0 {
		t.Fatal("content record retained no request body")
	}
	if data, _ := record.Values["response_body"].([]byte); len(data) == 0 {
		t.Fatal("content record retained no response body")
	}
	recorded, ok := record.Values["recorded_at"].(time.Time)
	if !ok {
		t.Fatalf("content recorded_at = %T; want time.Time", record.Values["recorded_at"])
	}
	retain, ok := record.Values["retain_until"].(time.Time)
	if !ok {
		t.Fatalf("content retain_until = %T; want time.Time", record.Values["retain_until"])
	}
	if window := retain.Sub(recorded); window != modelRetentionWindow {
		t.Fatalf("content retention window = %s; want the explicitly configured %s", window, modelRetentionWindow)
	}
}

// modelStartupTransport counts dispatch and close so the malformed-startup
// test can prove the provider boundary is never used.
type modelStartupTransport struct {
	mu       sync.Mutex
	attempts int
	closes   int
}

func (transport *modelStartupTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.attempts++
	transport.mu.Unlock()
	return nil, fmt.Errorf("the malformed-startup fixture must not dispatch")
}

func (transport *modelStartupTransport) Close() error {
	transport.mu.Lock()
	transport.closes++
	transport.mu.Unlock()
	return nil
}

func (transport *modelStartupTransport) counts() (attempts, closes int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.attempts, transport.closes
}

// TestConfiguredModelRefusesUpstreamRedirect proves one model invocation never
// follows an upstream redirect. A provider that answers 3xx must settle as a
// failed call with no second request to the redirect target, so neither the
// configured credential nor the request can reach a host the configuration did
// not name.
//
// Mutation reasoning: wrapping the injected transport in a redirect-following
// http.Client sends the second request to the redirect target, failing both the
// redirect-target call count and the single failed usage row.
func TestConfiguredModelRefusesUpstreamRedirect(t *testing.T) {
	var redirectMu sync.Mutex
	redirectCalls := 0
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		redirectMu.Lock()
		redirectCalls++
		redirectMu.Unlock()
		response.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(response, `{"id":"cmpl-redirect","model":"upstream-small","choices":[{"index":0,"message":{"role":"assistant","content":"redirected"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(redirectTarget.Close)

	var primaryMu sync.Mutex
	primaryCalls := 0
	primaryAuthorization := ""
	primary := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		primaryMu.Lock()
		primaryCalls++
		primaryAuthorization = request.Header.Get("Authorization")
		primaryMu.Unlock()
		http.Redirect(response, request, redirectTarget.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(primary.Close)

	harness := newHarness(t)
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	harness.config.Models = &app.ModelsConfig{
		Models: []gateway.ModelConfig{
			modelEdgeConfig(scope, modelAlias, modelUpstream, primary.URL+"/v1/chat/completions", false),
		},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelSecretName},
		}},
		Transport: primary.Client().Transport,
	}
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)
	environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  string(modelSecretName),
		Value:     modelSecretValue,
	}, nil, "model-secret-write")

	_, publicError := environment.call(projectToken, "model.invoke", modelEdgeRequest(modelAlias), nil, "")
	if publicError.Code != api.CodeDependencyUnavailable {
		t.Fatalf("redirecting provider refusal = %q message=%q; want %q", publicError.Code, publicError.Message, api.CodeDependencyUnavailable)
	}

	primaryMu.Lock()
	primaryCount, authorization := primaryCalls, primaryAuthorization
	primaryMu.Unlock()
	if primaryCount != 1 {
		t.Fatalf("primary provider calls = %d; want exactly 1", primaryCount)
	}
	if authorization != "Bearer "+modelSecretValue {
		t.Fatalf("primary authorization = %q; want the configured provider secret", authorization)
	}
	redirectMu.Lock()
	redirectCount := redirectCalls
	redirectMu.Unlock()
	if redirectCount != 0 {
		t.Fatalf("redirect target calls = %d; a 3xx must never be followed", redirectCount)
	}

	rows := modelEdgeUsageRows(t, harness.store, scope)
	if len(rows) != 1 {
		t.Fatalf("redirected model usage rows = %d; want exactly 1", len(rows))
	}
	if rows[0].Values["outcome"] != string(gateway.UsageFailed) {
		t.Fatalf("redirected model usage outcome = %v; want failed", rows[0].Values["outcome"])
	}
}

// TestMalformedModelStartupRefusesBeforeServing proves an incomplete enabled
// model configuration refuses startup without serving or reaching a provider.
func TestMalformedModelStartupRefusesBeforeServing(t *testing.T) {
	harness := newHarness(t)
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: projectID}
	transport := &modelStartupTransport{}
	model := modelEdgeConfig(scope, modelAlias, modelUpstream, "https://models.example.test/v1/chat/completions", false)
	model.MaxRequestBytes = 0
	harness.config.Models = &app.ModelsConfig{
		Models: []gateway.ModelConfig{model},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelSecretName},
		}},
		Transport: transport,
	}

	composed, err := app.New(context.Background(), harness.config)
	if err == nil {
		_ = composed.Close(context.Background())
		t.Fatal("the malformed model configuration started a composition")
	}
	attempts, closes := transport.counts()
	if attempts != 0 {
		t.Fatalf("provider attempts = %d; want 0 before configuration validation", attempts)
	}
	if closes != 0 {
		t.Fatalf("provider transport closes = %d; want 0 because it was never adopted", closes)
	}
}
