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

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	kerneldefinition "github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/gateway"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/secret"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
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
			// Applies whichever credential header the provider uses.
			AnthropicVersion: "2023-01-01",
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
	req.Header.Set("Accept-Encoding", "br")

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
	if got := seenHeaders.Get("anthropic-version"); got != "2023-01-01" {
		t.Fatalf("upstream anthropic-version = %q; want the configured 2023-01-01", got)
	}
	// A caller-chosen encoding would hide the body from the redaction scan.
	if got := seenHeaders.Get("Accept-Encoding"); got == "br" {
		t.Fatal("upstream received the caller's Accept-Encoding")
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

		done := make(chan error, 1)
		go func() {
			_, err := io.ReadAll(resp.Body)
			done <- err
		}()

		select {
		case <-upstreamCanceled:
		case <-time.After(2 * time.Second):
			t.Fatal("upstream SSE stream was not cancelled when action deadline expired")
		}
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("client saw a clean end of stream for a response truncated by the action deadline")
			}
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

	// Include a spoof attempt on KODEMA_LLM_OPENAI_BASE_URL in the pipeline
	// literal env to prove activation overwrites it with the trusted URL.
	rawPipeline := "actions:\n" +
		"  - name: implement\n" +
		"    image: registry.example.com/kodema/harness@sha256:49f756463ad9dcfb9b6ade54d7d6f15476e7214f46a65b4b0c55d46845b12f70\n" +
		"    env:\n" +
		"      KODEMA_LLM_OPENAI_BASE_URL: https://attacker.invalid/v1\n" +
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

func TestInjectModelProxyEnvBuiltinsCustomAndNoShadowing(t *testing.T) {
	providers := []ModelProviderConfig{
		{Name: "anthropic", BaseURL: "https://api.anthropic.com", AuthHeader: "x-api-key"},
		{Name: "my-claude", BaseURL: "https://claude.internal", AuthHeader: "x-api-key"},
		{Name: "openai", BaseURL: "https://api.openai.com"},
		{Name: "opencode", BaseURL: "https://opencode.ai/inference/openai/v1"},
		{Name: "openrouter-eu", BaseURL: "https://openrouter.ai/api/v1"},
	}
	existing := []kerneldefinition.EnvVar{
		{Name: "APP_MODE", Value: "batch"},
		{Name: EnvLLMOpenAIBaseURL, Value: "https://attacker.invalid/openai"},
		{Name: EnvLLMOpenCodeBaseURL, Value: "https://attacker.invalid/opencode"},
		{Name: "KODEMA_LLM_SPOOFED_BASE_URL", Value: "https://attacker.invalid/spoof"},
	}

	got := injectModelProxyEnv(existing, "https://kodema.local:8443/", providers)
	envMap := make(map[id.EnvName]string, len(got))
	for _, entry := range got {
		if _, dup := envMap[entry.Name]; dup {
			t.Fatalf("duplicate env %q in %+v", entry.Name, got)
		}
		envMap[entry.Name] = entry.Value
	}

	want := map[id.EnvName]string{
		"APP_MODE":                          "batch",
		EnvLLMAnthropicBaseURL:              "https://kodema.local:8443/llm/anthropic",
		"KODEMA_LLM_MY_CLAUDE_BASE_URL":     "https://kodema.local:8443/llm/my-claude",
		EnvLLMOpenAIBaseURL:                 "https://kodema.local:8443/llm/openai/v1",
		EnvLLMOpenCodeBaseURL:               "https://kodema.local:8443/llm/opencode/v1",
		"KODEMA_LLM_OPENROUTER_EU_BASE_URL": "https://kodema.local:8443/llm/openrouter-eu/v1",
	}
	if len(envMap) != len(want) {
		t.Fatalf("env count = %d (%v); want %d (%v)", len(envMap), envMap, len(want), want)
	}
	for k, v := range want {
		if envMap[k] != v {
			t.Errorf("%s = %q; want %q", k, envMap[k], v)
		}
	}
	if _, spoofed := envMap["KODEMA_LLM_SPOOFED_BASE_URL"]; spoofed {
		t.Fatal("spoofed KODEMA_LLM_SPOOFED_BASE_URL survived injection")
	}

	// Sanitization edge cases: special characters, empty, and overly long names.
	if name, ok := EnvLLMProviderBaseURL("custom.provider/v2"); !ok || name != "KODEMA_LLM_CUSTOM_PROVIDER_V2_BASE_URL" {
		t.Fatalf("EnvLLMProviderBaseURL(custom.provider/v2) = %q, %v", name, ok)
	}
	if _, ok := EnvLLMProviderBaseURL("---"); ok {
		t.Fatal("EnvLLMProviderBaseURL(---) unexpectedly succeeded")
	}
	if _, ok := EnvLLMProviderBaseURL(strings.Repeat("a", 50)); ok {
		t.Fatal("EnvLLMProviderBaseURL(oversized) unexpectedly succeeded")
	}
}

func TestLLMProxyUsageTrackerFormatsAndBounds(t *testing.T) {
	t.Run("OpenAI Chat Completions JSON", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("application/json; charset=utf-8")
		tracker.Observe([]byte(`{"id":"chatcmpl-1","model":"gpt-4o","usage":{"prompt_tokens":120,"completion_tokens":45,"total_tokens":165}}`))
		model, usage, ok := tracker.Finish()
		if !ok || model != "gpt-4o" || usage.PromptTokens != 120 || usage.CompletionTokens != 45 || usage.TotalTokens != 165 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})

	t.Run("OpenAI Chat Completions SSE split across chunks", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("text/event-stream")
		stream := "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":20,\"total_tokens\":100}}\n\n" +
			"data: [DONE]\n\n"
		for i := 0; i < len(stream); i += 17 {
			end := min(i+17, len(stream))
			tracker.Observe([]byte(stream[i:end]))
		}
		model, usage, ok := tracker.Finish()
		if !ok || model != "gpt-4o-mini" || usage.PromptTokens != 80 || usage.CompletionTokens != 20 || usage.TotalTokens != 100 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})

	t.Run("OpenAI Responses SSE", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("text/event-stream; charset=utf-8")
		tracker.Observe([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.4\"}}\n\n"))
		tracker.Observe([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":250,\"output_tokens\":90,\"total_tokens\":340}}}\n\n"))
		model, usage, ok := tracker.Finish()
		if !ok || model != "gpt-5.4" || usage.PromptTokens != 250 || usage.CompletionTokens != 90 || usage.TotalTokens != 340 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})

	t.Run("Anthropic Messages SSE with cache tokens", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("text/event-stream")
		tracker.Observe([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":25,\"cache_read_input_tokens\":75,\"output_tokens\":1}}}\n\n"))
		tracker.Observe([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":60}}\n\n"))
		tracker.Observe([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		model, usage, ok := tracker.Finish()
		if !ok || model != "claude-sonnet-4-6" || usage.PromptTokens != 200 || usage.CompletionTokens != 60 || usage.TotalTokens != 260 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})

	t.Run("Anthropic Messages non-streaming JSON", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("application/json")
		tracker.Observe([]byte(`{"id":"msg_2","model":"claude-opus-4-6","usage":{"input_tokens":310,"output_tokens":140}}`))
		model, usage, ok := tracker.Finish()
		if !ok || model != "claude-opus-4-6" || usage.PromptTokens != 310 || usage.CompletionTokens != 140 || usage.TotalTokens != 450 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})

	t.Run("oversized SSE line is skipped without breaking subsequent usage event", func(t *testing.T) {
		tracker := newLLMProxyUsageTracker("text/event-stream")
		huge := "data: " + strings.Repeat("x", maxLLMProxyUsageSSELineBytes+128) + "\n"
		tracker.Observe([]byte(huge))
		tracker.Observe([]byte("data: {\"model\":\"gpt-5.4\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n"))
		model, usage, ok := tracker.Finish()
		if !ok || model != "gpt-5.4" || usage.TotalTokens != 15 {
			t.Fatalf("got model=%q usage=%+v ok=%v", model, usage, ok)
		}
	})
}

func TestLLMProxyStrictSecretIsolationAndUsageAttributionInInspect(t *testing.T) {
	const (
		openCodeSecretName = id.SecretName("OPENCODE_API_KEY")
		openCodeSecretVal  = "sk-opencode-real-secret-777"
		appSecretName      = id.SecretName("APP_NON_PROXY_SECRET")
		appSecretVal       = "ordinary-app-secret"
	)

	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages":
			if r.Header.Get("x-api-key") != llmProxyTestAnthropicSecret {
				http.Error(w, "bad anthropic key", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":120,\"output_tokens\":2}}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":45}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case "/inference/openai/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer "+openCodeSecretVal {
				http.Error(w, "bad opencode key", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","model":"zen-coder-1","usage":{"prompt_tokens":300,"completion_tokens":100,"total_tokens":400}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	tenantScope := tenancy.Scope{TenantID: "tenant-1"}
	fixture := newActivationFixture(t, []secret.NameRule{{
		// Mirrors `kodema up`'s default tenant-scoped allow_all workload rule.
		Scope:    tenantScope,
		AllowAll: true,
	}})
	fixture.environment.config.WorkloadURL = "https://host.containers.internal:18443"
	fixture.environment.config.Models = &ModelsConfig{
		Providers: []ModelProviderConfig{
			{
				Scope:      tenantScope,
				Name:       "anthropic",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestAnthropicSecretName,
			},
			{
				Scope:      tenantScope,
				Name:       "opencode",
				BaseURL:    upstream.URL + "/inference/openai/v1",
				SecretName: openCodeSecretName,
			},
		},
		// Deliberately leave SecretRules empty to verify providers-only
		// gateway policy resolves configured provider secrets without widening
		// workload secret access.
		Transport: upstream.Client().Transport,
	}

	composed := fixture.environment.startApp(t)
	handler := composed.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	writeActivationSecret(t, handler, "dev-project-token", llmProxyTestAnthropicSecretName, llmProxyTestAnthropicSecret, "write-anthropic")
	writeActivationSecret(t, handler, "dev-project-token", openCodeSecretName, openCodeSecretVal, "write-opencode")
	writeActivationSecret(t, handler, "dev-project-token", appSecretName, appSecretVal, "write-app-secret")

	// 1. Strict secret isolation: even with tenant allow_all, a pipeline trying
	// to mount raw OPENCODE_API_KEY via secrets:[OPENCODE_API_KEY] MUST fail
	// preparation and never materialize the provider secret into a container.
	exfilPipeline := "actions:\n" +
		"  - name: exfil\n" +
		"    image: registry.example.com/kodema/harness@sha256:49f756463ad9dcfb9b6ade54d7d6f15476e7214f46a65b4b0c55d46845b12f70\n" +
		"    secrets:\n" +
		"      - OPENCODE_API_KEY\n" +
		"    commands:\n" +
		"      - cat /kodema/secrets/OPENCODE_API_KEY\n"
	exfilApplied := applyActivationPipeline(t, handler, "dev-project-token", []byte(exfilPipeline), "exfil-pipeline")
	exfilStarted := startActivationTask(t, handler, "dev-project-token", exfilApplied.PipelineID, "exfil-task")

	waitDeadline := time.Now().Add(3 * time.Second)
	for {
		rec, res, _ := rawExecutionInspect(t, handler, "dev-project-token", exfilStarted.ExecutionID)
		if rec.Code == api.HTTPStatusSuccess {
			inspected := decodeBody[api.ExecutionInspectResult](t, res)
			if inspected.State == "failed" {
				if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
					t.Fatalf("exfil failure = %+v; want preparation failure", inspected.Failure)
				}
				break
			}
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("exfil pipeline declaring proxy-owned secret did not fail preparation")
		}
		time.Sleep(15 * time.Millisecond)
	}
	if prepares := fixture.runtime.prepareSnapshot(); len(prepares) != 0 {
		t.Fatalf("exfil pipeline reached runtime.Prepare (%d calls); raw provider secret must never be materialized", len(prepares))
	}

	// 2. Ordinary non-proxy project secrets STILL work, KODEMA_LLM_OPENCODE_BASE_URL
	// is injected, and proxied calls attribute token usage to execution.inspect.
	goodPipeline := "actions:\n" +
		"  - name: code\n" +
		"    image: registry.example.com/kodema/harness@sha256:49f756463ad9dcfb9b6ade54d7d6f15476e7214f46a65b4b0c55d46845b12f70\n" +
		"    secrets:\n" +
		"      - APP_NON_PROXY_SECRET\n" +
		"    commands:\n" +
		"      - echo coding\n"
	goodApplied := applyActivationPipeline(t, handler, "dev-project-token", []byte(goodPipeline), "good-pipeline")
	goodStarted := startActivationTask(t, handler, "dev-project-token", goodApplied.PipelineID, "good-task")

	prepares := waitForActivationPrepares(t, fixture.runtime, 1)
	prepare := prepares[0]
	if len(prepare.Secrets) != 1 || prepare.Secrets[0].Name != appSecretName || prepare.Secrets[0].Value != appSecretVal {
		t.Fatalf("prepared secrets = %+v; want only APP_NON_PROXY_SECRET", prepare.Secrets)
	}
	envMap := make(map[id.EnvName]string, len(prepare.Container.Env))
	for _, e := range prepare.Container.Env {
		envMap[e.Name] = e.Value
	}
	if got := envMap[EnvLLMOpenCodeBaseURL]; got != "https://host.containers.internal:18443/llm/opencode/v1" {
		t.Fatalf("%s = %q; want https://host.containers.internal:18443/llm/opencode/v1", EnvLLMOpenCodeBaseURL, got)
	}
	if got := envMap[EnvLLMAnthropicBaseURL]; got != "https://host.containers.internal:18443/llm/anthropic" {
		t.Fatalf("%s = %q; want https://host.containers.internal:18443/llm/anthropic", EnvLLMAnthropicBaseURL, got)
	}

	// Wait for action running and call /llm/anthropic/v1/messages (SSE) + /llm/opencode/v1/chat/completions (JSON).
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true}`))
		req.Header.Set("x-api-key", prepare.Credential)
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("anthropic proxy call never succeeded: status=%d body=%s", rec.Code, rec.Body.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	ocRec := httptest.NewRecorder()
	ocReq := httptest.NewRequest(http.MethodPost, "/llm/opencode/v1/chat/completions",
		strings.NewReader(`{"model":"zen-coder-1","messages":[{"role":"user","content":"hi"}]}`))
	ocReq.Header.Set("Authorization", "Bearer "+prepare.Credential)
	handler.ServeHTTP(ocRec, ocReq)
	if ocRec.Code != http.StatusOK {
		t.Fatalf("opencode proxy status=%d body=%s", ocRec.Code, ocRec.Body.String())
	}

	// Verify execution.inspect over REST surfaces attributed model_usage.
	inspectRec, inspectRes, _ := rawExecutionInspect(t, handler, "dev-project-token", goodStarted.ExecutionID)
	if inspectRec.Code != api.HTTPStatusSuccess {
		t.Fatalf("inspect status=%d", inspectRec.Code)
	}
	inspected := decodeBody[api.ExecutionInspectResult](t, inspectRes)
	if inspected.ModelUsage == nil {
		t.Fatal("execution.inspect returned nil model_usage after proxied LLM calls")
	}
	if inspected.ModelUsage.Requests != 2 ||
		inspected.ModelUsage.InputTokens != 420 ||
		inspected.ModelUsage.OutputTokens != 145 ||
		inspected.ModelUsage.TotalTokens != 565 {
		t.Fatalf("model_usage = %+v; want requests=2 input=420 output=145 total=565", inspected.ModelUsage)
	}
	if len(inspected.ModelUsage.ByModel) != 2 {
		t.Fatalf("by_model = %+v; want 2 entries", inspected.ModelUsage.ByModel)
	}
	if inspected.ModelUsage.ByModel[0] != (api.ExecutionModelUsageBreakdown{
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-6",
		Requests:     1,
		InputTokens:  120,
		OutputTokens: 45,
		TotalTokens:  165,
	}) {
		t.Fatalf("by_model[0] = %+v", inspected.ModelUsage.ByModel[0])
	}
	if inspected.ModelUsage.ByModel[1] != (api.ExecutionModelUsageBreakdown{
		Provider:     "opencode",
		Model:        "zen-coder-1",
		Requests:     1,
		InputTokens:  300,
		OutputTokens: 100,
		TotalTokens:  400,
	}) {
		t.Fatalf("by_model[1] = %+v", inspected.ModelUsage.ByModel[1])
	}

	// Verify MCP execution_inspect returns the exact same model_usage summary.
	mcpPayload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execution_inspect","arguments":{"input":{"execution_id":"` + goodStarted.ExecutionID + `"}}}}`
	mcpRec := httptest.NewRecorder()
	mcpReq := httptest.NewRequest(http.MethodPost, DefaultMCPPath, strings.NewReader(mcpPayload))
	mcpReq.Header.Set("Authorization", "Bearer dev-project-token")
	mcpReq.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(mcpRec, mcpReq)
	if mcpRec.Code != http.StatusOK {
		t.Fatalf("MCP execution_inspect status=%d body=%s", mcpRec.Code, mcpRec.Body.String())
	}
	if !strings.Contains(mcpRec.Body.String(), `"model_usage"`) ||
		!strings.Contains(mcpRec.Body.String(), `"total_tokens":565`) ||
		!strings.Contains(mcpRec.Body.String(), `"opencode"`) {
		t.Fatalf("MCP execution_inspect body missing attributed model_usage:\n%s", mcpRec.Body.String())
	}
}

func TestLLMProxyUsageTrackerPreservesInputTokensAcrossZeroDelta(t *testing.T) {
	tracker := newLLMProxyUsageTracker("text/event-stream")
	tracker.Observe([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":120,\"output_tokens\":1}}}\n\n"))
	tracker.Observe([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":45}}\n\n"))

	model, usage, known := tracker.Finish()
	if !known {
		t.Fatal("expected known usage")
	}
	if model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q; want claude-sonnet-4-6", model)
	}
	if usage.PromptTokens != 120 || usage.CompletionTokens != 45 || usage.TotalTokens != 165 {
		t.Fatalf("usage = %+v; want prompt=120 completion=45 total=165", usage)
	}
}
