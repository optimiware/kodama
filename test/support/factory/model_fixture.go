package factory

// Deterministic upstream model provider fixture.
//
// The fixture is the opt-in upstream half of the configured model gateway. It
// starts one bounded loopback TLS provider with scripted exchanges, captures
// every accepted request in a sanitized form and fails closed on an unexpected
// request or on exhaustion. It replaces only the upstream model: the real
// router, authorizer, gateway, usage persistence and credential custody all stay
// in the path. The fixture therefore never imports a product adapter to fake
// one; it is an HTTP provider and nothing more.
//
// A request only advances the script when it matches the next exchange: the
// exact ordered provider tool declarations and, when required, a correlated
// history carrying the assistant tool call and its matching tool result. Every
// other request is refused with a non-2xx status, is recorded as a failure and
// leaves the script where it was. An exhausted script refuses every later
// request instead of answering with a synthesized completion.
//
// The provider credential is generated per fixture with crypto/rand. It is
// written through the public secret.write operation by Start and is never
// stored in request capture, evidence, logs or the fixture's diagnostic text.
// Request capture records only whether the provider credential matched, never
// the Authorization header.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Fixture upstream model identity. The configured caller alias and the
// provider-side model are deliberately different so a test can prove the
// gateway forwarded the configured upstream name and not the alias.
const (
	// ModelAlias is the configured caller-facing model name.
	ModelAlias = "fixture-model"
	// ModelUpstreamModel is the provider-side model name the alias maps to.
	ModelUpstreamModel = "fixture-upstream-model"
	// ModelSecretName is the separate model-gateway secret name. It is never
	// part of the workload secret allow-list.
	ModelSecretName id.SecretName = "MODEL_PROVIDER_KEY"
	// ModelProviderPath is the completion path of the fixture provider.
	ModelProviderPath = "/v1/chat/completions"
)

const (
	// modelFixtureCredentialBytes is the entropy of one generated provider
	// credential. 24 bytes are 192 bits, above the 128-bit minimum.
	modelFixtureCredentialBytes = 24
	// modelFixtureMaxCompletionTokens is the configured completion cap.
	modelFixtureMaxCompletionTokens = 512
	// modelFixtureTimeout is the configured finite provider timeout.
	modelFixtureTimeout = 10 * time.Second
)

// ModelExchange is one scripted provider exchange: the request shape the script
// requires before it advances, and the completion the provider answers with.
type ModelExchange struct {
	// Name is an optional label used in failure diagnostics.
	Name string
	// Expect is the required request shape.
	Expect ModelExpectation
	// Reply is the scripted completion.
	Reply ModelReply
}

// ModelExpectation is the exact request shape one exchange requires.
//
// Tools is the ordered list of provider function names the request must
// declare. A nil or empty list requires no declaration. ToolCallID, when set,
// requires the conversation history to carry an assistant tool call with that
// exact identity followed by a tool message carrying the same identity, which
// proves the provider observed correlated history rather than a fresh prompt.
type ModelExpectation struct {
	Tools      []string
	ToolCallID string
}

// ModelReply is one scripted completion. ContentNull writes explicit JSON-null
// assistant content; a tool-calling reply sets it. Usage is the deterministic
// provider usage report the gateway must persist.
type ModelReply struct {
	Content      string
	ContentNull  bool
	ToolCalls    []api.ChatToolCall
	FinishReason string
	Usage        api.ChatUsage
}

// ModelRequest is the sanitized capture of one accepted provider request. It
// deliberately carries no Authorization header and no raw body: evidence can
// therefore never contain the generated provider credential.
type ModelRequest struct {
	Model              string
	MessageCount       int
	Tools              []string
	ToolCallIDs        []string
	ToolResultIDs      []string
	ProviderCredential bool
}

// ModelFixture is one bounded loopback TLS provider with a scripted exchange
// list. It is safe for concurrent use.
type ModelFixture struct {
	t          testing.TB
	tls        *TLSFixture
	server     *httptest.Server
	endpoint   string
	credential string

	mu         sync.Mutex
	exchanges  []ModelExchange
	index      int
	requests   []ModelRequest
	failures   []string
	transports int
	gate       chan bool
	arrivals   int
	handled    int
	notify     chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
}

// NewModelFixture starts one bounded loopback TLS provider and registers its
// cleanup before the fallible TLS/server startup, so a startup failure cannot
// leak the listener. The scripted exchanges may be supplied here or with
// Script before Start consumes the fixture.
func NewModelFixture(t testing.TB, exchanges ...ModelExchange) *ModelFixture {
	t.Helper()
	fixture := &ModelFixture{
		t:      t,
		notify: make(chan struct{}),
		closed: make(chan struct{}),
	}
	t.Cleanup(fixture.Close)
	fixture.credential = newModelProviderCredential(t)
	fixture.script(exchanges...)

	// One private per-run CA and a leaf covering loopback. The provider binds
	// loopback only; there is no wildcard listener and no process-wide trust
	// change.
	fixture.tls = newTLSFixture(t, "127.0.0.1", "localhost")
	server := httptest.NewUnstartedServer(http.HandlerFunc(fixture.handle))
	server.TLS = fixture.tls.ServerConfig()
	server.StartTLS()
	fixture.server = server
	address, ok := server.Listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.IsLoopback() {
		server.Close()
		t.Fatalf("factory: model fixture listener %v is not loopback", server.Listener.Addr())
	}
	fixture.endpoint = server.URL + ModelProviderPath
	return fixture
}

// Script appends scripted exchanges. It refuses an invalid exchange before any
// request can observe it.
func (fixture *ModelFixture) Script(exchanges ...ModelExchange) {
	fixture.t.Helper()
	fixture.script(exchanges...)
}

func (fixture *ModelFixture) script(exchanges ...ModelExchange) {
	for _, exchange := range exchanges {
		if err := exchange.validate(); err != nil {
			fixture.t.Fatalf("factory: model fixture exchange: %v", err)
		}
	}
	fixture.mu.Lock()
	fixture.exchanges = append(fixture.exchanges, exchanges...)
	fixture.mu.Unlock()
}

// validate checks one exchange before it can be scripted.
func (exchange ModelExchange) validate() error {
	if exchange.Reply.FinishReason == "" {
		return fmt.Errorf("exchange %q has no finish reason", exchange.Name)
	}
	if exchange.Reply.ContentNull && exchange.Reply.Content != "" {
		return fmt.Errorf("exchange %q has both null content and content", exchange.Name)
	}
	if len(exchange.Reply.ToolCalls) > 0 {
		if exchange.Reply.Content != "" {
			return fmt.Errorf("exchange %q has an assistant tool call and content", exchange.Name)
		}
		for index, call := range exchange.Reply.ToolCalls {
			if err := call.Validate(); err != nil {
				return fmt.Errorf("exchange %q tool call %d: %w", exchange.Name, index, err)
			}
		}
	}
	if err := exchange.Reply.Usage.Validate(); err != nil {
		return fmt.Errorf("exchange %q usage: %w", exchange.Name, err)
	}
	return nil
}

// Endpoint returns the configured provider endpoint. It is an https loopback
// URL; the composition verifies the fixture CA on every controller.
func (fixture *ModelFixture) Endpoint() string { return fixture.endpoint }

// Transport returns a fresh transport that verifies the fixture CA. The
// composition calls this once per constructed controller and adopts the
// transport, so a restart rebuilds it instead of reusing a stopped owner's
// transport. The transport is bounded and never consults a process-wide proxy.
func (fixture *ModelFixture) Transport() http.RoundTripper {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(fixture.tls.CAPEM) {
		fixture.t.Fatalf("factory: model fixture CA is not a usable certificate pool")
	}
	fixture.mu.Lock()
	fixture.transports++
	fixture.mu.Unlock()
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}
}

// TransportsBuilt reports how many controller transports the fixture handed
// out. One per constructed controller is the restart invariant.
func (fixture *ModelFixture) TransportsBuilt() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.transports
}

// CredentialName returns the separate model-gateway secret name the
// composition declares. It never returns the credential value.
func (fixture *ModelFixture) CredentialName() id.SecretName { return ModelSecretName }

// Requests returns a copy of the sanitized accepted-request capture.
func (fixture *ModelFixture) Requests() []ModelRequest {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]ModelRequest(nil), fixture.requests...)
}

// Failures returns a copy of the sanitized failure diagnostics. A value here is
// a provider-side refusal the test must treat as a defect.
func (fixture *ModelFixture) Failures() []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]string(nil), fixture.failures...)
}

// TakeFailures returns the sanitized failure diagnostics observed so far and
// clears them. A test that deliberately scripts an unexpected request asserts
// the one expected refusal through this seam; a later AssertSatisfied then only
// reports unplanned refusals.
func (fixture *ModelFixture) TakeFailures() []string {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	failures := append([]string(nil), fixture.failures...)
	fixture.failures = nil
	return failures
}

// Exhausted reports whether every scripted exchange has been consumed.
func (fixture *ModelFixture) Exhausted() bool {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.index >= len(fixture.exchanges)
}

// AssertSatisfied fails the test unless every scripted exchange was consumed
// exactly once and the provider recorded no refusal.
func (fixture *ModelFixture) AssertSatisfied(t testing.TB) {
	t.Helper()
	fixture.mu.Lock()
	pending := len(fixture.exchanges) - fixture.index
	failures := append([]string(nil), fixture.failures...)
	requests := len(fixture.requests)
	fixture.mu.Unlock()
	if pending != 0 {
		t.Fatalf("factory: model fixture has %d unconsumed exchange(s) after %d accepted request(s)", pending, requests)
	}
	if len(failures) != 0 {
		t.Fatalf("factory: model fixture provider refused %d request(s): %s", len(failures), strings.Join(failures, "; "))
	}
}

// AssertCredentialAbsent fails the test when any evidence string carries the
// generated provider credential. The credential itself is never rendered in the
// failure, so a failing assertion does not disclose it.
func (fixture *ModelFixture) AssertCredentialAbsent(t testing.TB, evidence ...string) {
	t.Helper()
	if fixture.credential == "" {
		t.Fatal("factory: model fixture has no provider credential")
	}
	for index, value := range evidence {
		if strings.Contains(value, fixture.credential) {
			t.Fatalf("factory: evidence value %d contains the generated provider credential", index)
		}
	}
}

// WaitForRequests waits until at least want requests have been accepted and
// matched, or the context is canceled. It is a deterministic barrier: no fixed
// sleep decides who wins.
func (fixture *ModelFixture) WaitForRequests(ctx context.Context, want int) error {
	return fixture.wait(ctx, func() int { return len(fixture.requests) }, want)
}

// WaitForArrivals waits until at least want requests have reached the provider
// handler, or the context is canceled. A paused request is an arrival before it
// is accepted.
func (fixture *ModelFixture) WaitForArrivals(ctx context.Context, want int) error {
	return fixture.wait(ctx, func() int { return fixture.arrivals }, want)
}

// WaitForHandled waits until at least want requests have left the provider
// handler, or the context is canceled. It is the join barrier a paused or
// abandoned request needs before the next assertion.
func (fixture *ModelFixture) WaitForHandled(ctx context.Context, want int) error {
	return fixture.wait(ctx, func() int { return fixture.handled }, want)
}

// wait blocks until count reports want, or the context is canceled. The count
// and the notification channel are sampled in one critical section: sampling
// them separately loses a completion that signals between the two samples, and
// a join barrier that loses a wakeup blocks until its own deadline even though
// the handler already left. count is called with fixture.mu held.
func (fixture *ModelFixture) wait(ctx context.Context, count func() int, want int) error {
	for {
		fixture.mu.Lock()
		reached := count() >= want
		notify := fixture.notify
		fixture.mu.Unlock()
		if reached {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-fixture.closed:
			fixture.mu.Lock()
			reached := count() >= want
			fixture.mu.Unlock()
			if reached {
				return nil
			}
			return fmt.Errorf("factory: model fixture is closed")
		case <-notify:
		}
	}
}

// PauseNextRequest arms one barrier before the next request is matched. The
// returned function opens it with admit=true, or abandons the request with
// admit=false without consuming an exchange or answering it. A request whose
// context is canceled while paused is also abandoned.
func (fixture *ModelFixture) PauseNextRequest() func(admit bool) {
	gate := make(chan bool)
	fixture.mu.Lock()
	fixture.gate = gate
	fixture.mu.Unlock()
	var once sync.Once
	return func(admit bool) {
		once.Do(func() { gate <- admit })
	}
}

// Close stops the provider. It is idempotent and wakes every barrier.
func (fixture *ModelFixture) Close() {
	fixture.closeOnce.Do(func() {
		fixture.mu.Lock()
		close(fixture.closed)
		fixture.signalLocked()
		fixture.mu.Unlock()
		if fixture.server != nil {
			fixture.server.Close()
		}
	})
}

// handle is the provider. Every failure path is fail-closed: no completion is
// synthesized and the script does not advance.
func (fixture *ModelFixture) handle(response http.ResponseWriter, request *http.Request) {
	defer func() {
		fixture.mu.Lock()
		fixture.handled++
		fixture.signalLocked()
		fixture.mu.Unlock()
	}()
	if request.Method != http.MethodPost {
		fixture.refuse(response, http.StatusMethodNotAllowed, "unexpected method %s", request.Method)
		return
	}
	if request.URL.Path != ModelProviderPath {
		fixture.refuse(response, http.StatusNotFound, "unexpected provider path %q", request.URL.Path)
		return
	}
	fixture.mu.Lock()
	fixture.arrivals++
	gate := fixture.gate
	fixture.gate = nil
	fixture.signalLocked()
	fixture.mu.Unlock()
	if gate != nil {
		select {
		case admit := <-gate:
			if !admit {
				fixture.abandon()
			}
		case <-request.Context().Done():
			fixture.abandon()
		case <-fixture.closed:
			return
		}
	}
	if request.Context().Err() != nil {
		// A canceled caller abandons the request without consuming the script.
		fixture.abandon()
	}
	presented := request.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(presented), []byte("Bearer "+fixture.credential)) != 1 {
		// The header is never recorded, so a failure cannot leak either the
		// generated credential or a caller credential that was wrongly
		// forwarded.
		fixture.refuse(response, http.StatusUnauthorized, "provider credential did not match")
		return
	}
	body, ok := readModelProviderBody(response, request)
	if !ok {
		return
	}
	decoded, err := api.DecodeChatCompletionRequest(body)
	if err != nil {
		fixture.refuse(response, http.StatusBadRequest, "request is outside the frozen completion subset: %v", err)
		return
	}
	if decoded.Model != ModelUpstreamModel {
		fixture.refuse(response, http.StatusBadRequest, "upstream model %q is not the configured %q", decoded.Model, ModelUpstreamModel)
		return
	}
	fixture.answer(response, decoded)
}

// abandon ends a request the fixture will not serve without writing any
// response. A handler that merely returns lets net/http write an implicit 200,
// and the HTTP transport deliberately prefers a response that races the
// caller's cancellation, so the canceled request could be observed as
// successful. Aborting the connection guarantees the caller never sees a
// success for a request the fixture abandoned.
func (fixture *ModelFixture) abandon() {
	panic(http.ErrAbortHandler)
}

// readModelProviderBody reads the bounded request body. A body above the
// model bound is refused, never truncated into a plausible request.
func readModelProviderBody(response http.ResponseWriter, request *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(request.Body, int64(api.MaxModelBodyBytes)+1))
	if err != nil {
		http.Error(response, "request body could not be read", http.StatusBadRequest)
		return nil, false
	}
	if len(body) > api.MaxModelBodyBytes {
		http.Error(response, "request body exceeds the frozen model bound", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

// answer matches one decoded request against the next scripted exchange and
// only then advances the script.
func (fixture *ModelFixture) answer(response http.ResponseWriter, request api.ChatCompletionRequest) {
	observed := observeModelRequest(request)
	fixture.mu.Lock()
	if fixture.index >= len(fixture.exchanges) {
		fixture.failures = append(fixture.failures,
			"unexpected provider request after the scripted exchanges were exhausted")
		fixture.signalLocked()
		fixture.mu.Unlock()
		http.Error(response, "fixture script exhausted", http.StatusServiceUnavailable)
		return
	}
	exchange := fixture.exchanges[fixture.index]
	if err := exchange.Expect.match(request); err != nil {
		fixture.failures = append(fixture.failures, fmt.Sprintf("exchange %d (%s): %v",
			fixture.index, exchange.Label(), err))
		fixture.signalLocked()
		fixture.mu.Unlock()
		http.Error(response, "request does not match the next scripted exchange", http.StatusUnprocessableEntity)
		return
	}
	completion := exchange.completion(fixture.index)
	fixture.index++
	observed.ProviderCredential = true
	fixture.requests = append(fixture.requests, observed)
	fixture.signalLocked()
	fixture.mu.Unlock()

	response.Header().Set("Content-Type", api.MediaType)
	response.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(response)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(completion); err != nil {
		fixture.t.Logf("factory: model fixture write completion: %v", err)
	}
}

// refuse records one sanitized provider refusal and answers without advancing
// the script.
func (fixture *ModelFixture) refuse(response http.ResponseWriter, status int, format string, args ...any) {
	fixture.mu.Lock()
	fixture.failures = append(fixture.failures, fmt.Sprintf(format, args...))
	fixture.signalLocked()
	fixture.mu.Unlock()
	http.Error(response, "fixture provider refused the request", status)
}

func (fixture *ModelFixture) signalLocked() {
	close(fixture.notify)
	fixture.notify = make(chan struct{})
}

// Label returns the exchange name or its index-free description.
func (exchange ModelExchange) Label() string {
	if exchange.Name != "" {
		return exchange.Name
	}
	return "unnamed"
}

// completion renders the scripted completion. Deterministic identities make a
// test's expected completion stable across runs.
func (exchange ModelExchange) completion(index int) api.ChatCompletionResponse {
	message := api.ChatMessage{
		Role:        "assistant",
		Content:     exchange.Reply.Content,
		ContentNull: exchange.Reply.ContentNull,
		ToolCalls:   exchange.Reply.ToolCalls,
	}
	if len(exchange.Reply.ToolCalls) > 0 {
		message.Content = ""
		message.ContentNull = true
	}
	return api.ChatCompletionResponse{
		ID:    fmt.Sprintf("cmpl-fixture-%d", index+1),
		Model: ModelUpstreamModel,
		Choices: []api.ChatChoice{{
			Index:        0,
			Message:      message,
			FinishReason: exchange.Reply.FinishReason,
		}},
		Usage: exchange.Reply.Usage,
	}
}

// match checks the required declarations and the correlated history.
func (expectation ModelExpectation) match(request api.ChatCompletionRequest) error {
	observedTools := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		observedTools = append(observedTools, tool.Function.Name)
	}
	if !equalModelStrings(observedTools, expectation.Tools) {
		return fmt.Errorf("tool declarations = %v; want %v", observedTools, expectation.Tools)
	}
	if expectation.ToolCallID == "" {
		return nil
	}
	callIndex := -1
	resultIndex := -1
	for index, message := range request.Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if call.ID == expectation.ToolCallID {
					callIndex = index
				}
			}
		}
		if message.Role == "tool" && message.ToolCallID == expectation.ToolCallID {
			resultIndex = index
		}
	}
	if callIndex < 0 {
		return fmt.Errorf("history carries no assistant tool call %q", expectation.ToolCallID)
	}
	if resultIndex < 0 {
		return fmt.Errorf("history carries no tool result for %q", expectation.ToolCallID)
	}
	if resultIndex < callIndex {
		return fmt.Errorf("tool result for %q precedes its assistant tool call", expectation.ToolCallID)
	}
	return nil
}

// observeModelRequest captures only the sanitized request shape. The
// Authorization header and raw body never enter evidence.
func observeModelRequest(request api.ChatCompletionRequest) ModelRequest {
	observed := ModelRequest{
		Model:        request.Model,
		MessageCount: len(request.Messages),
	}
	for _, tool := range request.Tools {
		observed.Tools = append(observed.Tools, tool.Function.Name)
	}
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			observed.ToolCallIDs = append(observed.ToolCallIDs, call.ID)
		}
		if message.Role == "tool" {
			observed.ToolResultIDs = append(observed.ToolResultIDs, message.ToolCallID)
		}
	}
	return observed
}

// equalModelStrings compares two string lists exactly, distinguishing a nil
// list from a list of empty strings only by content as the caller declares.
func equalModelStrings(observed, expected []string) bool {
	if len(observed) != len(expected) {
		return false
	}
	for index := range observed {
		if observed[index] != expected[index] {
			return false
		}
	}
	return true
}

// newModelProviderCredential generates one unpredictable provider credential.
// The value is never a compile-time constant and is never rendered.
func newModelProviderCredential(t testing.TB) string {
	t.Helper()
	material := make([]byte, modelFixtureCredentialBytes)
	if _, err := rand.Read(material); err != nil {
		t.Fatalf("factory: generate model provider credential: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(material)
}

// assertUsable refuses a fixture that was never started or was already closed.
func (fixture *ModelFixture) assertUsable(t testing.TB) {
	t.Helper()
	if fixture == nil {
		t.Fatal("factory: WithModelFixture requires a fixture")
	}
	select {
	case <-fixture.closed:
		t.Fatal("factory: model fixture is closed")
	default:
	}
	if fixture.server == nil || fixture.endpoint == "" {
		t.Fatal("factory: model fixture was not started")
	}
}

// modelsConfig renders the real trusted composition configuration for one
// project scope. The gateway secret policy is separate from the workload
// secret policy and names exactly the fixture credential.
func (fixture *ModelFixture) modelsConfig(scope tenancy.Scope) *app.ModelsConfig {
	return &app.ModelsConfig{
		Models: []gateway.ModelConfig{{
			Scope:               scope,
			Model:               ModelAlias,
			UpstreamModel:       ModelUpstreamModel,
			Endpoint:            fixture.endpoint,
			SecretName:          ModelSecretName,
			MaxRequestBytes:     gateway.DefaultBounds.MaxRequestBytes,
			MaxResponseBytes:    gateway.DefaultBounds.MaxResponseBytes,
			MaxCompletionTokens: modelFixtureMaxCompletionTokens,
			Timeout:             modelFixtureTimeout,
			RecordContent:       false,
		}},
		SecretRules: []secret.NameRule{{
			Scope: scope,
			Names: []id.SecretName{ModelSecretName},
		}},
		Transport: fixture.Transport(),
	}
}

// WithModelFixture enables the optional deterministic upstream model provider.
// A factory without this option keeps its default model-disabled composition.
func WithModelFixture(fixture *ModelFixture) Option {
	return func(options *Options) { options.ModelFixture = fixture }
}

// composeModelsConfig returns the configured model configuration for one
// harness scope, or nil when the fixture is not selected.
func (options Options) composeModelsConfig(scope tenancy.Scope) *app.ModelsConfig {
	if options.ModelFixture == nil {
		return nil
	}
	return options.ModelFixture.modelsConfig(scope)
}

// writeCredential writes the generated provider credential through the public
// secret.write operation. The value travels only in the request body.
func (fixture *ModelFixture) writeCredential(h *Harness) {
	h.T.Helper()
	h.MustREST(h.ProjectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: ProjectID,
		SecretID:  string(ModelSecretName),
		Value:     fixture.credential,
	}, nil, "secret-"+string(ModelSecretName))
}

// modelFixtureSecretConflicts reports whether a caller-supplied secret map
// already owns the model credential name. Two writers would race the same
// secret and silently break credential custody, so Start refuses it.
func modelFixtureSecretConflicts(secrets map[string]string) bool {
	_, conflict := secrets[string(ModelSecretName)]
	return conflict
}
