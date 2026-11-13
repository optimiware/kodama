package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kerneldefinition "github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	llmProxyTestAnthropicSecretName = id.SecretName("ANTHROPIC_API_KEY")
	llmProxyTestOpenAISecretName    = id.SecretName("OPENAI_API_KEY")
	llmProxyTestAnthropicSecret     = "sk-ant-real-secret-999"
	llmProxyTestOpenAISecret        = "sk-openai-real-secret-888"
	llmProxyTestWorkloadToken       = "kodex.execution-models-1.validmac"
)

type mapSecretLookup struct {
	cipher secret.EnvelopeCipher
	values map[id.SecretName]string
}

var _ secret.NameLookup = mapSecretLookup{}

func (lookup mapSecretLookup) Lookup(ctx context.Context, scope tenancy.Scope, name id.SecretName) (secret.Stored, error) {
	val, ok := lookup.values[name]
	if !ok {
		return secret.Stored{}, fault.New(fault.NotFound, "secret not found")
	}
	return (modelTestSecretLookup{cipher: lookup.cipher, value: val}).Lookup(ctx, scope, name)
}

type staticPrincipalResolver struct {
	byToken map[string]identity.Principal
}

func (resolver staticPrincipalResolver) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	principal, ok := resolver.byToken[credential]
	if !ok {
		return identity.Principal{}, derivedRefusal()
	}
	return principal, nil
}

type fixedExecutionReader struct {
	admitted authorize.AdmittedExecution
	err      error
}

func (reader fixedExecutionReader) ReadExecution(context.Context, id.ExecutionID) (authorize.AdmittedExecution, error) {
	return reader.admitted, reader.err
}

type mutableExecutionController struct {
	mu      sync.Mutex
	current *toolTestController
}

func (m *mutableExecutionController) set(next *toolTestController) {
	m.mu.Lock()
	m.current = next
	m.mu.Unlock()
}

func (m *mutableExecutionController) Read(ctx context.Context, table, key string) (store.Row, error) {
	m.mu.Lock()
	cur := m.current
	m.mu.Unlock()
	return cur.Read(ctx, table, key)
}

func (m *mutableExecutionController) Scan(ctx context.Context, query store.ControllerScan) (store.QueryPage, error) {
	m.mu.Lock()
	cur := m.current
	m.mu.Unlock()
	return cur.Scan(ctx, query)
}

func (m *mutableExecutionController) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, nil
}

func newLLMProxyTestHandler(
	t *testing.T,
	config *ModelsConfig,
	principals map[string]identity.Principal,
	controller store.Controller,
	executions gateway.ExecutionReader,
	authority authorize.AuthorityReader,
	clock Clock,
	watchInterval time.Duration,
) http.Handler {
	t.Helper()
	cipher := newTestCipher(t)
	handler, stop, err := newLLMProxyHandler(llmProxyDependencies{
		Config:         config,
		Profile:        ProfileDevelopment,
		InstallationID: "installation-1",
		Controller:     controller,
		Executions:     executions,
		Principals:     staticPrincipalResolver{byToken: principals},
		Authorizer:     authorize.NewMatrix(),
		Authority:      authority,
		Lookup: mapSecretLookup{
			cipher: cipher,
			values: map[id.SecretName]string{
				llmProxyTestAnthropicSecretName: llmProxyTestAnthropicSecret,
				llmProxyTestOpenAISecretName:    llmProxyTestOpenAISecret,
			},
		},
		Cipher:        cipher,
		Clock:         clock,
		WatchInterval: watchInterval,
	})
	if err != nil {
		t.Fatalf("newLLMProxyHandler: %v", err)
	}
	if stop != nil {
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	return handler
}

func TestLLMProxyAnthropicStreamingSSEChunkByChunk(t *testing.T) {
	clock := newFakeClock()
	scope := modelTestScope("project-1")

	chunkGate := make(chan struct{})
	var seenHeaders http.Header
	var seenQuery string
	var seenPath string
	var seenBody string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)

		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream response writer is not a Flusher")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		flusher.Flush()

		// Wait until the client confirms receipt of chunk 1 before emitting
		// chunk 2: a buffering proxy would deadlock here.
		<-chunkGate

		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"delta\":\"hello\"}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	modelsCfg := &ModelsConfig{
		Providers: []ModelProviderConfig{{
			Scope:      scope,
			Name:       "anthropic",
			BaseURL:    upstream.URL + "/v1", // also exercises /v1 deduplication
			SecretName: llmProxyTestAnthropicSecretName,
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{llmProxyTestAnthropicSecretName},
		}},
		Transport: upstream.Client().Transport,
	}

	controller := modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour))
	proxyHandler := newLLMProxyTestHandler(t, modelsCfg, map[string]identity.Principal{
		llmProxyTestWorkloadToken: modelTestExecutionPrincipal(),
	}, controller, nil, allowAllToolAuthority{}, clock, 50*time.Millisecond)

	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	reqBody := `{"model":"claude-sonnet-4-6","stream":true,"max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/llm/anthropic/v1/messages?beta=true", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", llmProxyTestWorkloadToken)
	req.Header.Set("X-Leak-Attempt", llmProxyTestWorkloadToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; body = %s", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first SSE line: %v", err)
	}
	if strings.TrimSpace(firstLine) != "event: message_start" {
		t.Fatalf("first SSE line = %q; want event: message_start", firstLine)
	}
	// Unblock upstream chunks 2 and 3 after proving chunk 1 streamed immediately.
	close(chunkGate)

	restBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining SSE stream: %v", err)
	}
	fullStream := firstLine + string(restBytes)
	for _, want := range []string{"message_start", `"delta":"hello"`, "message_stop"} {
		if !strings.Contains(fullStream, want) {
			t.Fatalf("stream missing %q:\n%s", want, fullStream)
		}
	}

	if seenPath != "/v1/messages" {
		t.Fatalf("upstream path = %q; want /v1/messages (no double /v1)", seenPath)
	}
	if seenQuery != "beta=true" {
		t.Fatalf("upstream query = %q; want beta=true", seenQuery)
	}
	if seenBody != reqBody {
		t.Fatalf("upstream body = %q; want %q", seenBody, reqBody)
	}
	if got := seenHeaders.Get("x-api-key"); got != llmProxyTestAnthropicSecret {
		t.Fatalf("upstream x-api-key = %q; want %q", got, llmProxyTestAnthropicSecret)
	}
	if got := seenHeaders.Get("anthropic-version"); got != DefaultAnthropicVersion {
		t.Fatalf("upstream anthropic-version = %q; want %q", got, DefaultAnthropicVersion)
	}
	if got := seenHeaders.Get("Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q; want empty", got)
	}
	for name, vals := range seenHeaders {
		for _, val := range vals {
			if strings.Contains(val, llmProxyTestWorkloadToken) || strings.Contains(val, "kodex.") {
				t.Fatalf("upstream header %s leaked workload token: %q", name, val)
			}
		}
	}
}

func TestLLMProxyOpenAIStreamingSSEAndEndpointDerivation(t *testing.T) {
	clock := newFakeClock()
	scope := modelTestScope("project-1")

	var seenHeaders http.Header
	var seenPath string

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, item := range []string{
			`data: {"type":"response.created"}`,
			`data: {"type":"response.output_text.delta","delta":"world"}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, item+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	// Configure Endpoint only (/v1/chat/completions) to verify clean base URL
	// derivation when calling /v1/responses.
	modelsCfg := &ModelsConfig{
		Providers: []ModelProviderConfig{{
			Scope:      tenancy.Scope{TenantID: scope.TenantID}, // tenant-wide fallback
			Name:       "openai",
			Endpoint:   upstream.URL + "/v1/chat/completions",
			SecretName: llmProxyTestOpenAISecretName,
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{llmProxyTestOpenAISecretName},
		}},
		Transport: upstream.Client().Transport,
	}

	controller := modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour))
	proxyHandler := newLLMProxyTestHandler(t, modelsCfg, map[string]identity.Principal{
		llmProxyTestWorkloadToken: modelTestExecutionPrincipal(),
	}, controller, nil, allowAllToolAuthority{}, clock, 50*time.Millisecond)

	proxyServer := httptest.NewServer(proxyHandler)
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/llm/openai/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+llmProxyTestWorkloadToken)
	req.Header.Set("x-api-key", llmProxyTestWorkloadToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	streamBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !strings.Contains(string(streamBytes), `"delta":"world"`) || !strings.Contains(string(streamBytes), "[DONE]") {
		t.Fatalf("unexpected SSE stream:\n%s", string(streamBytes))
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q; want /v1/responses", seenPath)
	}
	if got := seenHeaders.Get("Authorization"); got != "Bearer "+llmProxyTestOpenAISecret {
		t.Fatalf("upstream Authorization = %q; want Bearer <secret>", got)
	}
	if got := seenHeaders.Get("x-api-key"); got != "" {
		t.Fatalf("upstream x-api-key = %q; want stripped", got)
	}
}

func TestLLMProxyAuthorityAndSafetyRefusalsBeforeUpstream(t *testing.T) {
	clock := newFakeClock()
	scope := modelTestScope("project-1")

	var upstreamHits atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	modelsCfg := &ModelsConfig{
		Providers: []ModelProviderConfig{{
			Scope:           scope,
			Name:            "openai",
			BaseURL:         upstream.URL,
			SecretName:      llmProxyTestOpenAISecretName,
			MaxRequestBytes: 256,
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{llmProxyTestOpenAISecretName},
		}},
		Transport: upstream.Client().Transport,
	}

	execPrincipal := modelTestExecutionPrincipal()
	principals := map[string]identity.Principal{
		llmProxyTestWorkloadToken: execPrincipal,
		"human-token":             modelTestWidePrincipal(),
		"service-token":           modelTestServicePrincipal(),
	}

	runningCtrl := modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour))

	cases := []struct {
		name       string
		path       string
		authHeader string
		apiKey     string
		body       string
		controller store.Controller
		executions gateway.ExecutionReader
		authority  authorize.AuthorityReader
		wantStatus int
	}{
		{
			name:       "missing credential",
			path:       "/llm/openai/v1/responses",
			controller: runningCtrl,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "conflicting bearer and x-api-key",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			apiKey:     "kodex.other.token",
			controller: runningCtrl,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "human token refused",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer human-token",
			controller: runningCtrl,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "service token refused",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer service-token",
			controller: runningCtrl,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "execution lacking configured_models permission",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			executions: fixedExecutionReader{
				admitted: authorize.AdmittedExecution{
					Principal:   execPrincipal,
					ExecutionID: execPrincipal.ExecutionID,
					ActionID:    "action-models-1",
					Permission:  authorize.ExecNone,
					Deadline:    clock.Now().Add(time.Hour),
				},
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "cancelled execution",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: modelTestExecutionContext("cancelled", "", "", time.Time{}),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "succeeded execution",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: modelTestExecutionContext("succeeded", "", "", time.Time{}),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "paused waiting on review request action",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: modelTestExecutionContext("running", "request", "running", clock.Now().Add(time.Hour)),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "revoked live execution running authority",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: runningCtrl,
			authority:  deniedModelAuthority{check: authorize.CheckExecutionRunning},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "unconfigured provider",
			path:       "/llm/unknown-provider/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: runningCtrl,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "encoded path separator smuggling",
			path:       "/llm/openai/v1%2f..%2fadmin",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: runningCtrl,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "credential smuggled in query string",
			path:       "/llm/openai/v1/responses?api_key=" + llmProxyTestWorkloadToken,
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			controller: runningCtrl,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "oversized request body",
			path:       "/llm/openai/v1/responses",
			authHeader: "Bearer " + llmProxyTestWorkloadToken,
			body:       strings.Repeat("x", 300),
			controller: runningCtrl,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authority := tc.authority
			if authority == nil {
				authority = allowAllToolAuthority{}
			}
			handler := newLLMProxyTestHandler(t, modelsCfg, principals, tc.controller, tc.executions, authority, clock, 50*time.Millisecond)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			if tc.apiKey != "" {
				req.Header.Set("x-api-key", tc.apiKey)
			}
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d; want %d (body=%s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			var publicErr api.Error
			if err := json.Unmarshal(recorder.Body.Bytes(), &publicErr); err != nil || publicErr.Code == "" {
				t.Fatalf("response is not a structured api.Error: %s", recorder.Body.String())
			}
			if hits := upstreamHits.Load(); hits != 0 {
				t.Fatalf("upstream received %d requests on refused call; want 0", hits)
			}
		})
	}
}

func TestLLMProxyRefusesRedirectsWithoutFollowingOrLeakingSecret(t *testing.T) {
	clock := newFakeClock()
	scope := modelTestScope("project-1")

	var redirectTargetHits atomic.Int32
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL+"/steal")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	modelsCfg := &ModelsConfig{
		Providers: []ModelProviderConfig{{
			Scope:      scope,
			Name:       "anthropic",
			BaseURL:    upstream.URL,
			SecretName: llmProxyTestAnthropicSecretName,
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{llmProxyTestAnthropicSecretName},
		}},
		Transport: upstream.Client().Transport,
	}

	controller := modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour))
	handler := newLLMProxyTestHandler(t, modelsCfg, map[string]identity.Principal{
		llmProxyTestWorkloadToken: modelTestExecutionPrincipal(),
	}, controller, nil, allowAllToolAuthority{}, clock, 50*time.Millisecond)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", llmProxyTestWorkloadToken)
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("redirect status = %d; want 503 (body=%s)", recorder.Code, recorder.Body.String())
	}
	if loc := recorder.Header().Get("Location"); loc != "" {
		t.Fatalf("proxy leaked Location header %q", loc)
	}
	if hits := redirectTargetHits.Load(); hits != 0 {
		t.Fatalf("redirect target received %d calls; provider secret must never follow HTTP 302", hits)
	}
	if strings.Contains(recorder.Body.String(), llmProxyTestAnthropicSecret) {
		t.Fatal("error body leaked upstream secret")
	}
}

func TestLLMProxyTerminatesStreamImmediatelyOnDeadlineOrCancellation(t *testing.T) {
	t.Run("action deadline expiry terminates in-flight SSE stream", func(t *testing.T) {
		clock := newFakeClock()
		scope := modelTestScope("project-1")

		streamStarted := make(chan struct{})
		upstreamCanceled := make(chan struct{})

		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				_, _ = io.WriteString(w, "data: {\"chunk\":1}\n\n")
				flusher.Flush()
			}
			close(streamStarted)
			<-r.Context().Done()
			close(upstreamCanceled)
		}))
		defer upstream.Close()

		modelsCfg := &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "openai",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestOpenAISecretName,
			}},
			SecretRules: []secret.NameRule{{
				Scope: scope,
				Names: []id.SecretName{llmProxyTestOpenAISecretName},
			}},
			Transport: upstream.Client().Transport,
		}

		// Action deadline is only 120ms in the future.
		controller := modelTestExecutionContext("running", "container", "running", clock.Now().Add(120*time.Millisecond))
		handler := newLLMProxyTestHandler(t, modelsCfg, map[string]identity.Principal{
			llmProxyTestWorkloadToken: modelTestExecutionPrincipal(),
		}, controller, nil, allowAllToolAuthority{}, clock, 20*time.Millisecond)

		proxyServer := httptest.NewServer(handler)
		defer proxyServer.Close()

		req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/llm/openai/v1/responses", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+llmProxyTestWorkloadToken)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		<-streamStarted

		done := make(chan struct{})
		go func() {
			_, _ = io.ReadAll(resp.Body)
			close(done)
		}()

		select {
		case <-upstreamCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("upstream SSE stream was not cancelled when action deadline expired")
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("client SSE stream did not terminate promptly on action deadline expiry")
		}
	})

	t.Run("execution cancellation mid-stream aborts upstream SSE stream", func(t *testing.T) {
		clock := newFakeClock()
		scope := modelTestScope("project-1")

		streamStarted := make(chan struct{})
		upstreamCanceled := make(chan struct{})

		upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				_, _ = io.WriteString(w, "data: {\"chunk\":1}\n\n")
				flusher.Flush()
			}
			close(streamStarted)
			<-r.Context().Done()
			close(upstreamCanceled)
		}))
		defer upstream.Close()

		modelsCfg := &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "anthropic",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestAnthropicSecretName,
			}},
			SecretRules: []secret.NameRule{{
				Scope: scope,
				Names: []id.SecretName{llmProxyTestAnthropicSecretName},
			}},
			Transport: upstream.Client().Transport,
		}

		dynController := &mutableExecutionController{
			current: modelTestExecutionContext("running", "container", "running", clock.Now().Add(time.Hour)),
		}
		handler := newLLMProxyTestHandler(t, modelsCfg, map[string]identity.Principal{
			llmProxyTestWorkloadToken: modelTestExecutionPrincipal(),
		}, dynController, nil, allowAllToolAuthority{}, clock, 20*time.Millisecond)

		proxyServer := httptest.NewServer(handler)
		defer proxyServer.Close()

		req, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/llm/anthropic/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("x-api-key", llmProxyTestWorkloadToken)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		<-streamStarted
		// Transition execution to cancelled while stream is open.
		dynController.set(modelTestExecutionContext("cancelled", "", "", time.Time{}))

		select {
		case <-upstreamCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("upstream SSE stream was not aborted after execution cancellation")
		}
	})
}

func TestLLMProxyComposedAppInjectsBaseURLEnvAndStreamsForLiveWorkload(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if got := r.Header.Get("x-api-key"); got != llmProxyTestAnthropicSecret {
				http.Error(w, "bad anthropic key", http.StatusUnauthorized)
				return
			}
		case "/v1/responses":
			if got := r.Header.Get("Authorization"); got != "Bearer "+llmProxyTestOpenAISecret {
				http.Error(w, "bad openai key", http.StatusUnauthorized)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	scope := tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
	fixture := newActivationFixture(t, nil)
	fixture.environment.config.WorkloadURL = "https://host.containers.internal:18443"
	fixture.environment.config.Models = &ModelsConfig{
		Providers: []ModelProviderConfig{
			{
				Scope:      scope,
				Name:       "anthropic",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestAnthropicSecretName,
			},
			{
				Scope:      scope,
				Name:       "openai",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestOpenAISecretName,
			},
		},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{llmProxyTestAnthropicSecretName, llmProxyTestOpenAISecretName},
		}},
		Transport: upstream.Client().Transport,
	}

	app := fixture.environment.startApp(t)
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	writeActivationSecret(t, handler, "dev-project-token", llmProxyTestAnthropicSecretName, llmProxyTestAnthropicSecret, "write-anthropic")
	writeActivationSecret(t, handler, "dev-project-token", llmProxyTestOpenAISecretName, llmProxyTestOpenAISecret, "write-openai")

	// Include a spoof attempt on KODAMA_LLM_OPENAI_BASE_URL in the pipeline
	// literal env to prove activation overwrites it with the trusted URL.
	rawPipeline := "actions:\n" +
		"  - name: implement\n" +
		"    image: registry.example.com/kodama/harness@sha256:49f756463ad9dcfb9b6ade54d7d6f15476e7214f46a65b4b0c55d46845b12f70\n" +
		"    env:\n" +
		"      KODAMA_LLM_OPENAI_BASE_URL: https://attacker.invalid/v1\n" +
		"    commands:\n" +
		"      - echo running\n"
	applied := applyActivationPipeline(t, handler, "dev-project-token", []byte(rawPipeline), "llm-proxy-pipeline")
	started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "llm-proxy-task")

	prepares := waitForActivationPrepares(t, fixture.runtime, 1)
	prepare := prepares[0]
	envMap := make(map[id.EnvName]string, len(prepare.Container.Env))
	for _, entry := range prepare.Container.Env {
		if _, dup := envMap[entry.Name]; dup {
			t.Fatalf("duplicate env var %q in PrepareRequest.Container.Env: %+v", entry.Name, prepare.Container.Env)
		}
		envMap[entry.Name] = entry.Value
	}
	wantEnv := map[kerneldefinition.EnvVar]bool{
		{Name: EnvLLMOpenAIBaseURL, Value: "https://host.containers.internal:18443/llm/openai/v1"}:    true,
		{Name: EnvLLMAnthropicBaseURL, Value: "https://host.containers.internal:18443/llm/anthropic"}: true,
	}
	for want := range wantEnv {
		if got := envMap[want.Name]; got != want.Value {
			t.Fatalf("%s = %q; want %q", want.Name, got, want.Value)
		}
	}

	// Wait until the engine marks the container action running in the store so
	// live workload LLM proxy requests succeed.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages", bytes.NewReader([]byte(`{"stream":true}`)))
		req.Header.Set("x-api-key", prepare.Credential)
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			if !strings.Contains(rec.Body.String(), `"ok":true`) {
				t.Fatalf("anthropic stream body = %q", rec.Body.String())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload LLM proxy never became active: last status=%d body=%s", rec.Code, rec.Body.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	// OpenAI Bearer path with the same workload token also streams 200 OK.
	openAIRec := httptest.NewRecorder()
	openAIReq := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/responses", bytes.NewReader([]byte(`{"stream":true}`)))
	openAIReq.Header.Set("Authorization", "Bearer "+prepare.Credential)
	handler.ServeHTTP(openAIRec, openAIReq)
	if openAIRec.Code != http.StatusOK || !strings.Contains(openAIRec.Body.String(), `"ok":true`) {
		t.Fatalf("openai proxy status=%d body=%s", openAIRec.Code, openAIRec.Body.String())
	}

	// Cancel the execution and verify the workload token is immediately refused.
	inspectRec, inspectRes, _ := rawExecutionInspect(t, handler, "dev-project-token", started.ExecutionID)
	if inspectRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("inspect status=%d", inspectRec.Code)
	}
	inspected := decodeBody[api.ExecutionInspectResult](t, inspectRes)
	cancelRec, _, cancelErr := restCall(t, handler, http.MethodPost,
		"/api/v1/executions/"+started.ExecutionID+"/cancel", "dev-project-token",
		map[string]any{}, &inspected.Version, "cancel-llm-task")
	if cancelRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("cancel status=%d err=%+v", cancelRec.Code, cancelErr)
	}

	afterCancel := httptest.NewRecorder()
	afterReq := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages", bytes.NewReader([]byte(`{}`)))
	afterReq.Header.Set("x-api-key", prepare.Credential)
	handler.ServeHTTP(afterCancel, afterReq)
	if afterCancel.Code != http.StatusForbidden && afterCancel.Code != http.StatusUnauthorized {
		t.Fatalf("cancelled execution LLM proxy status = %d; want 401/403 (body=%s)", afterCancel.Code, afterCancel.Body.String())
	}
}
