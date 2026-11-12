package app

// Tests for the optional configured model gateway composition.
//
// The named behavioral tests are:
//
//   - TestModelsDisabledIsInert
//   - TestModelCompositionRefusesIncompleteConfiguration
//   - TestModelCompositionRequiresVerifiedHTTPSInProduction
//   - TestConfiguredModelForwardsAliasAndShellCredential
//   - TestModelCompositionGatewayPolicyIsSeparateFromWorkload
//   - TestModelCompositionOwnsAVerifyingTransport
//   - TestModelCompositionCancellationAndResponseBodyClosure
//   - TestModelCompositionStartupFailureCleansUpTheTransport
//
// TestConfiguredModelThroughRESTAndMCP and
// TestMalformedModelStartupRefusesBeforeServing live in the composition
// integration suite (test/integration/composition/models_test.go).
// TestModelCompositionAuthorityAndAttribution lives in
// models_authority_test.go.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	modelTestAlias         = "small-model"
	modelTestUpstreamModel = "upstream-small"
	modelTestSecretName    = id.SecretName("MODEL_API_KEY")
	modelTestSecret        = "provider-secret-9f3a2c"
	modelTestCompletion    = `{"id":"cmpl-model-1","model":"upstream-small","choices":[{"index":0,"message":{"role":"assistant","content":"hello from provider"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
)

func modelTestScope(project id.ProjectID) tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-1", ProjectID: project}
}

// modelTestModelConfig returns one fully valid configured model. The caller
// supplies the alias/upstream pair and endpoint so a test can prove the exact
// mapping it configured.
func modelTestModelConfig(scope tenancy.Scope, model, upstream, endpoint string, recordContent bool) gateway.ModelConfig {
	return gateway.ModelConfig{
		Scope:               scope,
		Model:               model,
		UpstreamModel:       upstream,
		Endpoint:            endpoint,
		SecretName:          modelTestSecretName,
		MaxRequestBytes:     gateway.DefaultBounds.MaxRequestBytes,
		MaxResponseBytes:    gateway.DefaultBounds.MaxResponseBytes,
		MaxCompletionTokens: 1024,
		Timeout:             5 * time.Second,
		RecordContent:       recordContent,
	}
}

// modelTestModelsConfig is the complete trusted configuration of one enabled
// gateway with one configured model.
func modelTestModelsConfig(scope tenancy.Scope, endpoint string, transport http.RoundTripper) *ModelsConfig {
	return &ModelsConfig{
		Models: []gateway.ModelConfig{
			modelTestModelConfig(scope, modelTestAlias, modelTestUpstreamModel, endpoint, false),
		},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelTestSecretName},
		}},
		Transport: transport,
	}
}

// modelTestRequest is one valid non-streaming completion request.
func modelTestRequest(model string) api.ChatCompletionRequest {
	return api.ChatCompletionRequest{
		Model:    model,
		Messages: api.Messages{{Role: "user", Content: "hello"}},
	}
}

// modelTestUsageUnits returns the usage units a test executor recorded.
func modelTestUsageUnits(t *testing.T, executor *recordingToolExecutor) []store.Unit {
	t.Helper()
	executor.mu.Lock()
	defer executor.mu.Unlock()
	units := make([]store.Unit, 0, len(executor.units))
	for _, unit := range executor.units {
		if len(unit.Writes) == 1 && unit.Writes[0].Table == gateway.UsageTable {
			units = append(units, unit)
		}
	}
	return units
}

// modelTestUsageValues returns the written values of one usage unit.
func modelTestUsageValues(t *testing.T, unit store.Unit) map[string]any {
	t.Helper()
	if len(unit.Writes) != 1 {
		t.Fatalf("usage unit writes = %d; want 1", len(unit.Writes))
	}
	return unit.Writes[0].Values
}

// modelTestRows reads every committed row of one table in one scope.
func modelTestRows(t *testing.T, scanner store.Scanner, scope tenancy.Scope, table string) []store.Row {
	t.Helper()
	page, err := scanner.Query(context.Background(), scope, store.ScanQuery{Table: table, Limit: 20})
	if err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	return page.Rows
}

// modelTestSecretLookup is the NameLookup fixture. It seals one value with the
// exact associated identity the real resolver expects, so the composition
// exercises the production secret resolver rather than a parallel
// implementation.
type modelTestSecretLookup struct {
	cipher secret.EnvelopeCipher
	value  string
}

var _ secret.NameLookup = modelTestSecretLookup{}

func (lookup modelTestSecretLookup) Lookup(ctx context.Context, scope tenancy.Scope, name id.SecretName) (secret.Stored, error) {
	metadata := secret.Metadata{
		ID:        secret.SecretID("secret-model-1"),
		Scope:     scope,
		Name:      name,
		Version:   1,
		UpdatedAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
	}
	associated := secret.AssociatedData{
		InstallationID: "installation-1",
		TenantID:       scope.TenantID,
		ProjectID:      scope.ProjectID,
		SecretID:       metadata.ID,
		Name:           name,
		KeyVersion:     secret.MinKeyVersion,
	}
	envelope, err := lookup.cipher.Seal(ctx, associated, []byte(lookup.value))
	if err != nil {
		return secret.Stored{}, err
	}
	return secret.Stored{
		Metadata:  metadata,
		CreatedAt: metadata.UpdatedAt,
		Envelope:  envelope,
	}, nil
}

// modelTestWidePrincipal is one project-scoped human with the engineer grant
// model.invoke requires.
func modelTestWidePrincipal() identity.Principal {
	return identity.Principal{
		ID:      "admin-1",
		Kind:    identity.KindHuman,
		Scope:   modelTestScope("project-1"),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

// modelTestDependencies builds the direct constructor dependency bundle shared
// by the non-HTTP tests. Every boundary is real except the controller,
// authority answer and transport supplied by the caller.
func modelTestDependencies(t *testing.T, principal identity.Principal, controller store.Controller, authority authorize.AuthorityReader, usage *recordingToolExecutor, transport http.RoundTripper, clock Clock) (opsapp.ModelInvocation, func(context.Context) error) {
	t.Helper()
	return modelTestDependenciesAt(t, principal, controller, authority, usage, transport, clock,
		"https://models.example.test/v1/chat/completions")
}

// modelTestDependenciesAt is modelTestDependencies with an explicit model
// endpoint, so a transport-focused test can point the configured model at a
// local provider or an unroutable address.
func modelTestDependenciesAt(t *testing.T, principal identity.Principal, controller store.Controller, authority authorize.AuthorityReader, usage *recordingToolExecutor, transport http.RoundTripper, clock Clock, endpoint string) (opsapp.ModelInvocation, func(context.Context) error) {
	t.Helper()
	cipher := newTestCipher(t)
	models, stop, err := newConfiguredModels(modelGatewayDependencies{
		Config:         modelTestModelsConfig(modelTestScope("project-1"), endpoint, transport),
		Profile:        ProfileDevelopment,
		InstallationID: "installation-1",
		Controller:     controller,
		Credentials:    mustToolBinding(t, principal, clock),
		Authorizer:     authorize.NewMatrix(),
		Authority:      authority,
		Lookup:         modelTestSecretLookup{cipher: cipher, value: modelTestSecret},
		Cipher:         cipher,
		Store:          usage,
		IDs:            bridges.NewSecureIDs(),
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("newConfiguredModels: %v", err)
	}
	return models, stop
}

// modelTestTransport is the scripted provider transport. It records attempts
// and either returns one fixed response or one scripted error.
type modelTestTransport struct {
	mu       sync.Mutex
	attempts int
	response *http.Response
	err      error
	requests []*http.Request
}

func (transport *modelTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.attempts++
	transport.requests = append(transport.requests, request)
	transport.mu.Unlock()
	if transport.err != nil {
		return nil, transport.err
	}
	return transport.response, nil
}

func (transport *modelTestTransport) attemptCount() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.attempts
}

// modelTestBody tracks how often the gateway closes a response body.
type modelTestBody struct {
	reader io.Reader
	mu     sync.Mutex
	closes int
}

func (body *modelTestBody) Read(buffer []byte) (int, error) { return body.reader.Read(buffer) }

func (body *modelTestBody) Close() error {
	body.mu.Lock()
	body.closes++
	body.mu.Unlock()
	return nil
}

func (body *modelTestBody) closeCount() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

// modelTestResponse builds one successful completion response with a tracking
// body.
func modelTestResponse(body *modelTestBody) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{api.MediaType}},
		Body:       body,
	}
}

// modelTestUpstream is the deterministic local TLS provider of the full-app
// test. It records the exact request the gateway forwards.
type modelTestUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []modelTestUpstreamCall
}

type modelTestUpstreamCall struct {
	Header        http.Header
	Path          string
	Body          []byte
	Authorization string
}

func newModelTestUpstream(t *testing.T) *modelTestUpstream {
	t.Helper()
	upstream := &modelTestUpstream{}
	upstream.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		var decoded struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(data, &decoded)
		upstream.mu.Lock()
		upstream.calls = append(upstream.calls, modelTestUpstreamCall{
			Header:        request.Header.Clone(),
			Path:          request.URL.Path,
			Body:          data,
			Authorization: request.Header.Get("Authorization"),
		})
		upstream.mu.Unlock()
		response.Header().Set("Content-Type", api.MediaType)
		_, _ = io.WriteString(response, `{"id":"cmpl-model-1","model":"`+decoded.Model+`","choices":[{"index":0,"message":{"role":"assistant","content":"hello from provider"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (upstream *modelTestUpstream) url() string { return upstream.server.URL }

func (upstream *modelTestUpstream) transport() http.RoundTripper {
	return upstream.server.Client().Transport
}

func (upstream *modelTestUpstream) received() []modelTestUpstreamCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return append([]modelTestUpstreamCall(nil), upstream.calls...)
}

// writeModelSecret writes one encrypted gateway secret through the public REST
// edge.
func writeModelSecret(t *testing.T, app *App, value string) {
	t.Helper()
	recorder, _, publicError := restCall(t, app.Handler(), http.MethodPut,
		"/api/v1/projects/project-1/secrets/"+string(modelTestSecretName), "dev-project-token",
		api.SecretWriteRequest{ProjectID: "project-1", SecretID: string(modelTestSecretName), Value: value},
		nil, "model-secret-write")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("secret.write status=%d code=%q", recorder.Code, publicError.Code)
	}
}

// TestModelsDisabledIsInert pins the default: a nil model configuration keeps
// the disabled port, constructs no gateway dependency and returns no cleanup.
//
// Mutation reasoning: constructing the gateway before the nil check makes this
// call fail (no dependencies), and returning an enabled port makes the type
// assertion fail.
func TestModelsDisabledIsInert(t *testing.T) {
	models, stop, err := newConfiguredModels(modelGatewayDependencies{})
	if err != nil {
		t.Fatalf("disabled model construction: %v", err)
	}
	if _, ok := models.(disabledModels); !ok {
		t.Fatalf("disabled model port = %T; want disabledModels with no dependency", models)
	}
	if stop != nil {
		t.Fatal("disabled model configuration returned a cleanup; want none")
	}
}

// TestModelCompositionRefusesIncompleteConfiguration pins the construction-time
// refusal of empty or ambiguous model configuration before any adapter exists.
// Every case is a separate trusted-configuration defect; none is silently
// resolved by last-entry-wins or an invalid bound.
func TestModelCompositionRefusesIncompleteConfiguration(t *testing.T) {
	scope := modelTestScope("project-1")
	valid := func() *ModelsConfig {
		return modelTestModelsConfig(scope, "https://models.example.test/v1/chat/completions", nil)
	}
	tests := []struct {
		name   string
		mutate func(*ModelsConfig)
		needle string
	}{
		{
			name:   "enabled configuration without models",
			mutate: func(config *ModelsConfig) { config.Models = nil },
			needle: "configured models",
		},
		{
			name: "duplicate scoped model",
			mutate: func(config *ModelsConfig) {
				config.Models = append(config.Models, config.Models[0])
			},
			needle: "declared twice",
		},
		{
			name: "empty endpoint",
			mutate: func(config *ModelsConfig) {
				config.Models[0].Endpoint = ""
			},
			needle: "endpoint",
		},
		{
			name: "non-http endpoint",
			mutate: func(config *ModelsConfig) {
				config.Models[0].Endpoint = "ftp://models.example.test/v1/chat/completions"
			},
			needle: "endpoint",
		},
		{
			name: "endpoint without a usable host",
			mutate: func(config *ModelsConfig) {
				config.Models[0].Endpoint = "https:///v1/chat/completions"
			},
			needle: "host",
		},
		{
			name: "request bound above the maximum",
			mutate: func(config *ModelsConfig) {
				config.Models[0].MaxRequestBytes = gateway.DefaultBounds.MaxRequestBytes + 1
			},
			needle: "request bound",
		},
		{
			name: "zero response bound",
			mutate: func(config *ModelsConfig) {
				config.Models[0].MaxResponseBytes = 0
			},
			needle: "response bound",
		},
		{
			name: "zero completion cap",
			mutate: func(config *ModelsConfig) {
				config.Models[0].MaxCompletionTokens = 0
			},
			needle: "completion cap",
		},
		{
			name: "zero timeout",
			mutate: func(config *ModelsConfig) {
				config.Models[0].Timeout = 0
			},
			needle: "timeout",
		},
		{
			name: "negative content retention",
			mutate: func(config *ModelsConfig) {
				config.Models[0].ContentRetention = -time.Second
			},
			needle: "retention",
		},
		{
			name: "invalid gateway secret rule",
			mutate: func(config *ModelsConfig) {
				config.SecretRules = []secret.NameRule{{Scope: scope}}
			},
			needle: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid()
			test.mutate(config)
			err := config.validate(ProfileDevelopment)
			if err == nil {
				t.Fatalf("%s was accepted; want a startup refusal", test.name)
			}
			if test.needle != "" && !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("%s refusal = %v; want %q named", test.name, err, test.needle)
			}

			// The same defect must fail the enabled constructor itself, not
			// only the pre-adapter configuration check.
			if _, _, err := newConfiguredModels(modelGatewayDependencies{
				Config:  config,
				Profile: ProfileDevelopment,
			}); err == nil {
				t.Fatalf("%s was accepted by newConfiguredModels", test.name)
			}
		})
	}
}

// TestModelCompositionRequiresVerifiedHTTPSInProduction pins the production
// boundary: an unverified plaintext endpoint is refused, and the composition
// never accepts a caller-supplied transport that could weaken TLS verification.
// Development and test may use an explicit local endpoint.
func TestModelCompositionRequiresVerifiedHTTPSInProduction(t *testing.T) {
	scope := modelTestScope("project-1")
	plain := func() *ModelsConfig {
		config := modelTestModelsConfig(scope, "http://127.0.0.1:8099/v1/chat/completions", nil)
		return config
	}

	if err := plain().validate(ProfileDevelopment); err != nil {
		t.Fatalf("development refused an explicit local http endpoint: %v", err)
	}
	if err := plain().validate(ProfileProduction); err == nil {
		t.Fatal("production accepted a plaintext model endpoint")
	} else if !strings.Contains(err.Error(), "https") {
		t.Fatalf("production plaintext refusal = %v; want the verified https requirement named", err)
	}

	verified := func() *ModelsConfig {
		config := modelTestModelsConfig(scope, "https://models.example.test/v1/chat/completions", nil)
		return config
	}
	if err := verified().validate(ProfileProduction); err != nil {
		t.Fatalf("production refused a verified https endpoint: %v", err)
	}

	withTransport := verified()
	withTransport.Transport = &modelTestTransport{}
	if err := withTransport.validate(ProfileProduction); err == nil {
		t.Fatal("production accepted a caller-supplied model transport")
	} else if !strings.Contains(err.Error(), "transport") {
		t.Fatalf("production injected transport refusal = %v; want the transport named", err)
	}

	// Every configured model is checked, not only the first: a plaintext
	// second entry must still refuse production startup.
	secondPlain := verified()
	secondPlain.Models = append(secondPlain.Models,
		modelTestModelConfig(scope, "second-model", "upstream-second", "http://127.0.0.1:8099/v1/chat/completions", false))
	err := secondPlain.validate(ProfileProduction)
	if err == nil {
		t.Fatal("production accepted a second configured model with a plaintext endpoint")
	}
	if !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "model 2") {
		t.Fatalf("second plaintext model refusal = %v; want the verified https requirement for model 2", err)
	}
}

// TestConfiguredModelForwardsAliasAndShellCredential drives one configured
// model through the composed HTTP edge against a local TLS provider.
//
// Mutation reasoning: leaving disabledModels installed returns not_found;
// forwarding the configured alias instead of the upstream name sends the wrong
// body model; forwarding the caller bearer instead of the resolved secret
// changes Authorization; dropping the principal from the usage row loses
// attribution; capturing content by default commits a content_records row.
func TestConfiguredModelForwardsAliasAndShellCredential(t *testing.T) {
	upstream := newModelTestUpstream(t)
	scope := modelTestScope("project-1")
	config := toolAppConfig(t, func(config *Config) {
		config.Models = modelTestModelsConfig(scope, upstream.url()+"/v1/chat/completions", upstream.transport())
	})
	composed := startToolApp(t, config)
	grantProjectEngineer(t, composed.Handler(), "dev-admin-token")
	writeModelSecret(t, composed, modelTestSecret)

	recorder, result, publicError := restCall(t, composed.Handler(), http.MethodPost,
		"/api/v1/projects/project-1/models/"+modelTestAlias+"/invoke", "dev-project-token",
		modelTestRequest(modelTestAlias), nil, "")
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("model.invoke status=%d code=%q message=%q", recorder.Code, publicError.Code, publicError.Message)
	}
	completion := decodeBody[api.ChatCompletionResponse](t, result)
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello from provider" {
		t.Fatalf("completion = %+v", completion)
	}
	if completion.Usage.TotalTokens != 18 {
		t.Fatalf("completion usage = %+v; want the provider's validated counts", completion.Usage)
	}

	calls := upstream.received()
	if len(calls) != 1 {
		t.Fatalf("upstream calls = %d; want exactly 1", len(calls))
	}
	call := calls[0]
	if call.Path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q; want the configured provider path", call.Path)
	}
	if call.Authorization != "Bearer "+modelTestSecret {
		t.Fatalf("upstream authorization = %q; want only the Shell-resolved configured secret", call.Authorization)
	}
	if strings.Contains(string(call.Body), "dev-project-token") {
		t.Fatalf("the caller credential reached the upstream body: %s", string(call.Body))
	}
	var forwarded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(call.Body, &forwarded); err != nil {
		t.Fatalf("decode upstream body: %v; body=%s", err, string(call.Body))
	}
	if forwarded.Model != modelTestUpstreamModel {
		t.Fatalf("upstream model = %q; want the configured alias mapping %q", forwarded.Model, modelTestUpstreamModel)
	}

	usageRows := modelTestRows(t, config.Adapters.Scanner, scope, gateway.UsageTable)
	if len(usageRows) != 1 {
		t.Fatalf("model usage rows = %d; want 1", len(usageRows))
	}
	row := usageRows[0]
	if row.Values["principal_id"] != "admin-1" {
		t.Fatalf("usage principal = %v; want the authenticated caller admin-1", row.Values["principal_id"])
	}
	if row.Values["execution_id"] != nil {
		t.Fatalf("human usage execution_id = %v; want nil", row.Values["execution_id"])
	}
	if row.Values["model"] != modelTestAlias || row.Values["upstream_model"] != modelTestUpstreamModel {
		t.Fatalf("usage model attribution = %v/%v; want %q/%q",
			row.Values["model"], row.Values["upstream_model"], modelTestAlias, modelTestUpstreamModel)
	}
	if row.Values["outcome"] != string(gateway.UsageSucceeded) {
		t.Fatalf("usage outcome = %v; want succeeded", row.Values["outcome"])
	}
	if known, _ := row.Values["usage_known"].(bool); !known {
		t.Fatal("usage row is not marked known on a validated completion")
	}

	if contentRows := modelTestRows(t, config.Adapters.Scanner, scope, gateway.ContentTable); len(contentRows) != 0 {
		t.Fatalf("content records = %d; raw capture is off by default", len(contentRows))
	}
}

// TestModelCompositionGatewayPolicyIsSeparateFromWorkload proves the model
// gateway resolves provider credentials through its own consumer allow-list. A
// workload rule naming the same secret must not authorize a provider call.
//
// Mutation reasoning: reusing Config.SecretRules as the gateway policy resolves
// the value, reaches the provider and commits a usage row; the denial then
// fails the refusal-code, upstream-call and usage-row assertions.
func TestModelCompositionGatewayPolicyIsSeparateFromWorkload(t *testing.T) {
	upstream := newModelTestUpstream(t)
	scope := modelTestScope("project-1")
	config := toolAppConfig(t, func(config *Config) {
		// The workload allow-list names the provider secret; the gateway
		// allow-list is deliberately empty.
		config.SecretRules = []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{modelTestSecretName},
		}}
		models := modelTestModelsConfig(scope, upstream.url()+"/v1/chat/completions", upstream.transport())
		models.SecretRules = nil
		config.Models = models
	})
	composed := startToolApp(t, config)
	grantProjectEngineer(t, composed.Handler(), "dev-admin-token")
	writeModelSecret(t, composed, modelTestSecret)

	recorder, _, publicError := restCall(t, composed.Handler(), http.MethodPost,
		"/api/v1/projects/project-1/models/"+modelTestAlias+"/invoke", "dev-project-token",
		modelTestRequest(modelTestAlias), nil, "")
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the workload secret rule authorized the model gateway consumer")
	}
	if publicError.Code != api.CodeDependencyUnavailable {
		t.Fatalf("consumer policy refusal = %q; want %q", publicError.Code, api.CodeDependencyUnavailable)
	}
	if calls := upstream.received(); len(calls) != 0 {
		t.Fatalf("unauthorized provider calls = %d; want 0", len(calls))
	}
	if rows := modelTestRows(t, config.Adapters.Scanner, scope, gateway.UsageTable); len(rows) != 0 {
		t.Fatalf("unauthorized provider usage rows = %d; want 0", len(rows))
	}
}

// TestModelCompositionOwnsAVerifyingTransport pins that the composition builds
// its own provider transport when no test transport is supplied and keeps
// standard TLS verification. An untrusted local certificate is refused before
// the provider handler is reached.
//
// Mutation reasoning: cloning the standard transport with InsecureSkipVerify,
// or accepting any certificate, lets the request reach the handler and the
// invocation succeed, failing both assertions.
func TestModelCompositionOwnsAVerifyingTransport(t *testing.T) {
	upstream := newModelTestUpstream(t)
	scope := modelTestScope("project-1")
	config := toolAppConfig(t, func(config *Config) {
		config.Models = modelTestModelsConfig(scope, upstream.url()+"/v1/chat/completions", nil)
	})
	composed := startToolApp(t, config)
	grantProjectEngineer(t, composed.Handler(), "dev-admin-token")
	writeModelSecret(t, composed, modelTestSecret)

	recorder, _, publicError := restCall(t, composed.Handler(), http.MethodPost,
		"/api/v1/projects/project-1/models/"+modelTestAlias+"/invoke", "dev-project-token",
		modelTestRequest(modelTestAlias), nil, "")
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the owned transport trusted an unverified local certificate")
	}
	if publicError.Code != api.CodeUncertainExternalEffect {
		t.Fatalf("unverified provider refusal = %q; want %q (a lost post-dispatch outcome)",
			publicError.Code, api.CodeUncertainExternalEffect)
	}
	if calls := upstream.received(); len(calls) != 0 {
		t.Fatalf("unverified provider handler calls = %d; want 0", len(calls))
	}
}

// modelTestProxy is a local TCP listener that records every connection routed
// to it. It never speaks HTTP: one accepted connection already proves that a
// provider request was proxied, which is the invariant these tests pin.
type modelTestProxy struct {
	listener net.Listener
	accepted atomic.Int64
}

func newModelTestProxy(t *testing.T) *modelTestProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for proxy fixture: %v", err)
	}
	proxy := &modelTestProxy{listener: listener}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.accepted.Add(1)
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return proxy
}

func (proxy *modelTestProxy) url() string {
	return "http://" + proxy.listener.Addr().String()
}

func (proxy *modelTestProxy) connections() int64 { return proxy.accepted.Load() }

// modelTestRoundTripper adapts one function to http.RoundTripper.
type modelTestRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip modelTestRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

// modelTestNonLoopbackIPv4 returns a local non-loopback IPv4 address. net/http
// deliberately never proxies a loopback request, so the environment-proxy
// proof needs a non-loopback destination that still fails fast locally.
func modelTestNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("list interfaces: %v", err)
	}
	for _, networkInterface := range interfaces {
		addresses, err := networkInterface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.To4() == nil || ip.IsLoopback() || !ip.IsGlobalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	t.Fatal("no local non-loopback IPv4 address is available for the environment-proxy proof")
	return ""
}

// modelTestClosedPort returns a port on address that is closed again before the
// caller uses it, so a direct dial fails immediately with connection refused.
func modelTestClosedPort(t *testing.T, address string) string {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatalf("reserve a closed port on %s: %v", address, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port on %s: %v", address, err)
	}
	return net.JoinHostPort(address, strconv.Itoa(port))
}

// TestModelCompositionOwnedTransportIgnoresEnvironmentProxy pins that the
// composition-built provider transport never consults the ambient
// HTTPS_PROXY/https_proxy/ALL_PROXY routing. A credential-bearing provider
// request must reach its configured host directly, never through an
// environment-chosen proxy.
//
// Mutation reasoning: cloning http.DefaultTransport (whose Proxy is
// ProxyFromEnvironment) routes the CONNECT to the listener, so the proxy
// connection count is no longer zero. Run this test alone against that mutant:
// net/http caches the proxy environment on first use, and the package suite may
// already have cached an empty configuration.
func TestModelCompositionOwnedTransportIgnoresEnvironmentProxy(t *testing.T) {
	proxy := newModelTestProxy(t)
	proxyURL := proxy.url()
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, proxyURL)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}

	address := modelTestNonLoopbackIPv4(t)
	endpoint := "https://" + modelTestClosedPort(t, address) + "/v1/chat/completions"
	// Initialize the standard proxy decision while the variables above are
	// set, so an inheriting transport would observe this proxy at all.
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	chosen, err := http.ProxyFromEnvironment(&http.Request{URL: parsed})
	if err != nil {
		t.Fatalf("resolve the environment proxy: %v", err)
	}
	if chosen == nil {
		t.Log("net/http already cached an empty proxy environment; the owned-transport assertion still holds")
	}

	usage := &recordingToolExecutor{}
	models, stop := modelTestDependenciesAt(t, modelTestWidePrincipal(), &toolTestController{},
		allowAllToolAuthority{}, usage, nil, newFakeClock(), endpoint)
	defer func() { _ = stop(context.Background()) }()

	if _, err := models.Invoke(context.Background(), modelTestWidePrincipal(), modelTestScope("project-1").ProjectID,
		modelTestRequest(modelTestAlias)); err == nil {
		t.Fatal("an unroutable provider endpoint without a proxy succeeded")
	}
	if connections := proxy.connections(); connections != 0 {
		t.Fatalf("environment proxy connections = %d; want 0 for a credential-bearing provider request", connections)
	}
}

// TestModelCompositionOwnedTransportIgnoresPoisonedDefaultTransport pins that
// the composition-owned transport is built from explicit defaults rather than
// cloned from the mutable process-wide http.DefaultTransport. A poisoned
// default must neither weaken TLS verification nor choose provider routing.
//
// Mutation reasoning: cloning http.DefaultTransport inherits both poisons; the
// insecure-TLS case reaches the provider handler and the proxy case opens a
// CONNECT to the listener, failing the corresponding assertion.
func TestModelCompositionOwnedTransportIgnoresPoisonedDefaultTransport(t *testing.T) {
	t.Run("poisoned insecure TLS default is not inherited", func(t *testing.T) {
		upstream := newModelTestUpstream(t)
		original := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = original })
		http.DefaultTransport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

		usage := &recordingToolExecutor{}
		models, stop := modelTestDependenciesAt(t, modelTestWidePrincipal(), &toolTestController{},
			allowAllToolAuthority{}, usage, nil, newFakeClock(), upstream.url()+"/v1/chat/completions")
		defer func() { _ = stop(context.Background()) }()

		_, err := models.Invoke(context.Background(), modelTestWidePrincipal(),
			modelTestScope("project-1").ProjectID, modelTestRequest(modelTestAlias))
		if err == nil {
			t.Fatal("the owned transport accepted an untrusted certificate from a poisoned default transport")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
			t.Fatalf("untrusted certificate class = %v (ok=%v); want uncertain_external_effect", class, ok)
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("untrusted provider handler calls = %d; want 0", len(calls))
		}
	})

	t.Run("poisoned proxy default is not inherited", func(t *testing.T) {
		upstream := newModelTestUpstream(t)
		proxy := newModelTestProxy(t)
		proxyURL, err := url.Parse(proxy.url())
		if err != nil {
			t.Fatalf("parse proxy URL: %v", err)
		}
		original := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = original })
		http.DefaultTransport = &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}

		usage := &recordingToolExecutor{}
		models, stop := modelTestDependenciesAt(t, modelTestWidePrincipal(), &toolTestController{},
			allowAllToolAuthority{}, usage, nil, newFakeClock(), upstream.url()+"/v1/chat/completions")
		defer func() { _ = stop(context.Background()) }()

		_, err = models.Invoke(context.Background(), modelTestWidePrincipal(),
			modelTestScope("project-1").ProjectID, modelTestRequest(modelTestAlias))
		if err == nil {
			t.Fatal("the owned transport accepted an untrusted certificate from a poisoned default transport")
		}
		if calls := upstream.received(); len(calls) != 0 {
			t.Fatalf("untrusted provider handler calls = %d; want 0", len(calls))
		}
		if connections := proxy.connections(); connections != 0 {
			t.Fatalf("poisoned default proxy connections = %d; want 0", connections)
		}
	})
}

// TestModelCompositionAllocatesDistinctUsageIdentities pins that every
// dispatched model call carries its own usage identity. Two calls through the
// same composed gateway must commit two rows with distinct, non-empty ids.
//
// Mutation reasoning: allocating the id once (or a constant) makes both rows
// share one identity, so the distinctness assertion fails.
func TestModelCompositionAllocatesDistinctUsageIdentities(t *testing.T) {
	clock := newFakeClock()
	principal := modelTestWidePrincipal()
	scope := modelTestScope("project-1")
	transport := modelTestRoundTripper(func(*http.Request) (*http.Response, error) {
		return modelTestResponse(&modelTestBody{reader: strings.NewReader(modelTestCompletion)}), nil
	})
	usage := &recordingToolExecutor{}
	models, stop := modelTestDependencies(t, principal, &toolTestController{}, allowAllToolAuthority{}, usage, transport, clock)
	defer func() { _ = stop(context.Background()) }()

	for call := range 2 {
		if _, err := models.Invoke(context.Background(), principal, scope.ProjectID, modelTestRequest(modelTestAlias)); err != nil {
			t.Fatalf("model invocation %d: %v", call+1, err)
		}
	}
	units := modelTestUsageUnits(t, usage)
	if len(units) != 2 {
		t.Fatalf("usage rows = %d; want 2", len(units))
	}
	first, second := units[0].Writes[0].ID, units[1].Writes[0].ID
	if first == "" || second == "" {
		t.Fatalf("usage ids = %q/%q; want allocated identities", first, second)
	}
	if first == second {
		t.Fatalf("usage ids = %q twice; every dispatched call needs a distinct identity", first)
	}
}

// TestModelCompositionCancellationAndResponseBodyClosure pins cancellation,
// body closure and the non-replayable uncertain outcome at the composition
// boundary.
//
// Mutation reasoning: waiting for a provider call after the caller canceled
// makes the cancellation case attempt a round trip; omitting the response close
// leaves the tracking body open; retrying a plain transport error makes the
// attempts count two; settling a lost call as failed makes the outcome
// replay-safe.
func TestModelCompositionCancellationAndResponseBodyClosure(t *testing.T) {
	clock := newFakeClock()
	principal := modelTestWidePrincipal()
	scope := modelTestScope("project-1")

	t.Run("cancelled caller never dispatches", func(t *testing.T) {
		transport := &modelTestTransport{response: modelTestResponse(&modelTestBody{reader: strings.NewReader(modelTestCompletion)})}
		usage := &recordingToolExecutor{}
		models, stop := modelTestDependencies(t, principal, &toolTestController{}, allowAllToolAuthority{}, usage, transport, clock)
		defer func() { _ = stop(context.Background()) }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := models.Invoke(ctx, principal, scope.ProjectID, modelTestRequest(modelTestAlias))
		if class, ok := fault.ClassOf(err); !ok || class != fault.DeadlineExpired {
			t.Fatalf("cancelled invocation class = %v (ok=%v); want deadline_expired", class, ok)
		}
		if attempts := transport.attemptCount(); attempts != 0 {
			t.Fatalf("cancelled invocation attempts = %d; want 0", attempts)
		}
		if units := modelTestUsageUnits(t, usage); len(units) != 0 {
			t.Fatalf("cancelled invocation committed %d usage rows; want 0", len(units))
		}
	})

	t.Run("success closes the provider response body", func(t *testing.T) {
		body := &modelTestBody{reader: strings.NewReader(modelTestCompletion)}
		transport := &modelTestTransport{response: modelTestResponse(body)}
		usage := &recordingToolExecutor{}
		models, stop := modelTestDependencies(t, principal, &toolTestController{}, allowAllToolAuthority{}, usage, transport, clock)
		defer func() { _ = stop(context.Background()) }()

		if _, err := models.Invoke(context.Background(), principal, scope.ProjectID, modelTestRequest(modelTestAlias)); err != nil {
			t.Fatalf("model invocation: %v", err)
		}
		if closes := body.closeCount(); closes != 1 {
			t.Fatalf("provider body closes = %d; want exactly 1", closes)
		}
		if attempts := transport.attemptCount(); attempts != 1 {
			t.Fatalf("provider attempts = %d; want 1", attempts)
		}
		units := modelTestUsageUnits(t, usage)
		if len(units) != 1 {
			t.Fatalf("usage rows = %d; want 1", len(units))
		}
		values := modelTestUsageValues(t, units[0])
		if values["outcome"] != string(gateway.UsageSucceeded) {
			t.Fatalf("usage outcome = %v; want succeeded", values["outcome"])
		}
	})

	t.Run("uncertain transport failure is never replayed", func(t *testing.T) {
		transport := &modelTestTransport{err: errors.New("connection reset after the request was sent")}
		usage := &recordingToolExecutor{}
		models, stop := modelTestDependencies(t, principal, &toolTestController{}, allowAllToolAuthority{}, usage, transport, clock)
		defer func() { _ = stop(context.Background()) }()

		_, err := models.Invoke(context.Background(), principal, scope.ProjectID, modelTestRequest(modelTestAlias))
		if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
			t.Fatalf("transport failure class = %v (ok=%v); want uncertain_external_effect", class, ok)
		}
		if attempts := transport.attemptCount(); attempts != 1 {
			t.Fatalf("uncertain call attempts = %d; want exactly 1 (no replay)", attempts)
		}
		units := modelTestUsageUnits(t, usage)
		if len(units) != 1 {
			t.Fatalf("uncertain usage rows = %d; want 1", len(units))
		}
		values := modelTestUsageValues(t, units[0])
		if values["outcome"] != string(gateway.UsageUncertain) {
			t.Fatalf("uncertain usage outcome = %v; want uncertain", values["outcome"])
		}
		if known, _ := values["usage_known"].(bool); known {
			t.Fatal("uncertain usage row claimed known counts")
		}
		if total, _ := values["total_tokens"].(int64); total != 0 {
			t.Fatalf("uncertain usage total = %d; want honest zero", total)
		}
	})
}

// closeTrackingTransport is one owned transport fixture whose close is
// observable. It never dispatches in the startup-failure tests.
type closeTrackingTransport struct {
	mu     sync.Mutex
	closes int
}

func (transport *closeTrackingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the fixture transport must not dispatch")
}

func (transport *closeTrackingTransport) Close() error {
	transport.mu.Lock()
	transport.closes++
	transport.mu.Unlock()
	return nil
}

func (transport *closeTrackingTransport) closeCount() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.closes
}

// TestModelCompositionStartupFailureCleansUpTheTransport pins that the
// composition takes ownership of the configured model transport and stops it
// when a later startup step fails. A malformed model configuration fails before
// the transport is adopted.
func TestModelCompositionStartupFailureCleansUpTheTransport(t *testing.T) {
	scope := modelTestScope("project-1")

	t.Run("later startup failure stops the adopted transport", func(t *testing.T) {
		config := fullTestProfileConfig(t)
		transport := &closeTrackingTransport{}
		config.Models = modelTestModelsConfig(scope, "https://models.example.test/v1/chat/completions", transport)
		config.hooks = &startupHook{failAt: "router"}

		if _, err := New(context.Background(), config); err == nil {
			t.Fatal("the injected startup failure was ignored")
		}
		if closes := transport.closeCount(); closes != 1 {
			t.Fatalf("model transport closes = %d; want 1 after startup cleanup", closes)
		}
	})

	t.Run("malformed configuration fails before adopting the transport", func(t *testing.T) {
		config := fullTestProfileConfig(t)
		transport := &closeTrackingTransport{}
		models := modelTestModelsConfig(scope, "https://models.example.test/v1/chat/completions", transport)
		models.Models[0].MaxRequestBytes = 0
		config.Models = models

		if _, err := New(context.Background(), config); err == nil {
			t.Fatal("the malformed model configuration was accepted")
		}
		if closes := transport.closeCount(); closes != 0 {
			t.Fatalf("model transport closes = %d; want 0 for config refused before construction", closes)
		}
	})
}
