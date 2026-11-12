package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenant     = id.TenantID("tenant-a")
	testProject    = id.ProjectID("project-a")
	otherProject   = id.ProjectID("project-b")
	testExecution  = id.ExecutionID("exec-1")
	testAction     = id.ActionID("action-1")
	testPrincipal  = id.PrincipalID("principal-exec")
	testCredential = "kodex.exec-1.credential-material"
	testSecretName = id.SecretName("MODEL_API_KEY")
	testModel      = "small-model"
	testUpstream   = "upstream-small"
	testEndpoint   = "https://models.example.test/v1/chat/completions"
	testSecret     = "provider-secret-value"
)

var fixedNow = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func completionJSON() string {
	return `{"id":"cmpl-1","model":"upstream-small","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`
}

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func executionPrincipal() identity.Principal {
	return identity.Principal{
		ID:          testPrincipal,
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     3,
		ExecutionID: testExecution,
	}
}

// admittedExecution is the default admitted context for the default execution
// credential: a running execution with one active action and a live deadline.
func admittedExecution() authorize.AdmittedExecution {
	return authorize.AdmittedExecution{
		Principal:   executionPrincipal(),
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  authorize.ExecConfiguredModels,
		Deadline:    fixedNow.Add(time.Hour),
	}
}

func humanEngineer() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-engineer"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 2,
	}
}

func humanViewer() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-viewer"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 2,
	}
}

func validModelConfig() gateway.ModelConfig {
	return gateway.ModelConfig{
		Scope:               testScope(),
		Model:               testModel,
		UpstreamModel:       testUpstream,
		Endpoint:            testEndpoint,
		SecretName:          testSecretName,
		MaxRequestBytes:     4096,
		MaxResponseBytes:    4096,
		MaxCompletionTokens: 64,
		Timeout:             2 * time.Second,
	}
}

func modelWith(mutate func(*gateway.ModelConfig)) gateway.ModelConfig {
	config := validModelConfig()
	mutate(&config)
	return config
}

func validConfig() gateway.Config {
	return gateway.Config{Enabled: true, Models: []gateway.ModelConfig{validModelConfig()}}
}

func invocation() gateway.Invocation {
	return gateway.Invocation{
		Credential: testCredential,
		ProjectID:  testProject,
		Request: api.ChatCompletionRequest{
			Model:    testModel,
			Messages: []api.ChatMessage{{Role: "user", Content: "hi"}},
		},
	}
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("error = %v; want nil", err)
	}
}

// singleInsert returns the one plain insert a usage unit must carry.
func singleInsert(t *testing.T, unit store.Unit) store.Write {
	t.Helper()
	if len(unit.Writes) != 1 {
		t.Fatalf("unit writes = %d; want exactly one", len(unit.Writes))
	}
	write := unit.Writes[0]
	if !write.Insert || write.Delete {
		t.Fatalf("usage write is not a plain insert: %#v", write)
	}
	return write
}

// fakeClock is a deterministic injected time source.
type fakeClock struct {
	now time.Time
}

func (clock *fakeClock) Now() time.Time { return clock.now }

// fakePrincipals is a deterministic CurrentPrincipal resolver.
type fakePrincipals struct {
	mu         sync.Mutex
	principals map[string]identity.Principal
	failures   map[string]error
	calls      []string
}

func newFakePrincipals() *fakePrincipals {
	return &fakePrincipals{
		principals: make(map[string]identity.Principal),
		failures:   make(map[string]error),
	}
}

func (fake *fakePrincipals) set(credential string, principal identity.Principal, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.principals[credential] = principal
	if err != nil {
		fake.failures[credential] = err
		return
	}
	delete(fake.failures, credential)
}

func (fake *fakePrincipals) Resolve(_ context.Context, credential string) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, credential)
	if err := fake.failures[credential]; err != nil {
		return identity.Principal{}, err
	}
	principal, ok := fake.principals[credential]
	if !ok {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "unknown credential")
	}
	return principal, nil
}

func (fake *fakePrincipals) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// fakeExecutionReader is the consumer-owned execution read seam: it returns the
// configured admitted context for one execution identity and can report a
// boundary failure.
type fakeExecutionReader struct {
	mu       sync.Mutex
	admitted map[id.ExecutionID]authorize.AdmittedExecution
	err      error
	calls    int
}

func newFakeExecutionReader() *fakeExecutionReader {
	return &fakeExecutionReader{admitted: make(map[id.ExecutionID]authorize.AdmittedExecution)}
}

func (fake *fakeExecutionReader) set(admitted authorize.AdmittedExecution) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.admitted[admitted.ExecutionID] = admitted
}

func (fake *fakeExecutionReader) setError(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.err = err
}

func (fake *fakeExecutionReader) ReadExecution(_ context.Context, executionID id.ExecutionID) (authorize.AdmittedExecution, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if fake.err != nil {
		return authorize.AdmittedExecution{}, fake.err
	}
	admitted, ok := fake.admitted[executionID]
	if !ok {
		return authorize.AdmittedExecution{}, fault.New(fault.NotFound, "unknown execution")
	}
	return admitted, nil
}

func (fake *fakeExecutionReader) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

// fakeAuthority returns one current fact per read in input order and can block
// on an explicit barrier or deny selected checks.
type fakeAuthority struct {
	mu     sync.Mutex
	calls  [][]authorize.AuthorityRead
	denied map[authorize.AuthorityCheck]bool
	err    error
	before func()
}

func newFakeAuthority() *fakeAuthority {
	return &fakeAuthority{denied: make(map[authorize.AuthorityCheck]bool)}
}

func (fake *fakeAuthority) deny(check authorize.AuthorityCheck) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.denied[check] = true
}

func (fake *fakeAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	fake.mu.Lock()
	fake.calls = append(fake.calls, append([]authorize.AuthorityRead(nil), reads...))
	before := fake.before
	fake.mu.Unlock()

	if before != nil {
		before()
	}

	fake.mu.Lock()
	denied := make(map[authorize.AuthorityCheck]bool, len(fake.denied))
	maps.Copy(denied, fake.denied)
	err := fake.err
	fake.mu.Unlock()

	if err != nil {
		return nil, err
	}
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     !denied[read.Check],
		}
	}
	return facts, nil
}

func (fake *fakeAuthority) lastReads() []authorize.AuthorityRead {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), fake.calls[len(fake.calls)-1]...)
}

func (fake *fakeAuthority) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// fakeSecrets records every resolve request and can rotate values per call.
type fakeSecrets struct {
	mu       sync.Mutex
	requests []secret.ResolveRequest
	values   []string
	index    int
	err      error
	resolve  func(secret.ResolveRequest) ([]secret.Resolved, error)
}

func (fake *fakeSecrets) Resolve(_ context.Context, request secret.ResolveRequest) ([]secret.Resolved, error) {
	fake.mu.Lock()
	fake.requests = append(fake.requests, secret.ResolveRequest{
		Scope: request.Scope,
		Names: append([]id.SecretName(nil), request.Names...),
	})
	index := fake.index
	fake.index++
	values := append([]string(nil), fake.values...)
	err := fake.err
	resolve := fake.resolve
	fake.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if resolve != nil {
		return resolve(request)
	}
	value := ""
	if len(values) > 0 {
		if index >= len(values) {
			index = len(values) - 1
		}
		value = values[index]
	}
	resolved := make([]secret.Resolved, 0, len(request.Names))
	for _, name := range request.Names {
		resolved = append(resolved, secret.Resolved{Name: name, Value: value, Scope: request.Scope})
	}
	return resolved, nil
}

func (fake *fakeSecrets) requestList() []secret.ResolveRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]secret.ResolveRequest(nil), fake.requests...)
}

func (fake *fakeSecrets) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.requests)
}

type storeCall struct {
	scope  tenancy.Scope
	unit   store.Unit
	ctxErr error
}

// fakeStore records the exact usage commit unit and the settlement context
// state at Execute time.
type fakeStore struct {
	mu    sync.Mutex
	calls []storeCall
	err   error
}

func (fake *fakeStore) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, storeCall{scope: scope, unit: unit, ctxErr: ctx.Err()})
	if fake.err != nil {
		return store.UnitResult{}, fake.err
	}
	return store.UnitResult{}, nil
}

func (fake *fakeStore) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, nil
}

func (fake *fakeStore) callList() []storeCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]storeCall(nil), fake.calls...)
}

func (fake *fakeStore) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// fakeContent records authorized raw-content opt-ins.
type fakeContent struct {
	mu      sync.Mutex
	records []gateway.ContentRecord
	err     error
}

func (fake *fakeContent) Record(_ context.Context, record gateway.ContentRecord) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.records = append(fake.records, record)
	return fake.err
}

func (fake *fakeContent) recordList() []gateway.ContentRecord {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]gateway.ContentRecord(nil), fake.records...)
}

func (fake *fakeContent) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.records)
}

type recordedRequest struct {
	method string
	url    string
	header http.Header
	body   []byte
}

// fakeTransport is the injected http.RoundTripper: it records the exact
// forwarded request and never opens a socket. It also implements the optional
// PreWriteFailure capability so a test can prove a failure happened before any
// request bytes were written.
type fakeTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	bodies   []*trackingBody
	handler  func(*http.Request) (*http.Response, error)
	preWrite func(error) bool
}

// PrecedesWrite reports the configured pre-write proof, if any.
func (fake *fakeTransport) PrecedesWrite(err error) bool {
	fake.mu.Lock()
	preWrite := fake.preWrite
	fake.mu.Unlock()
	return preWrite != nil && preWrite(err)
}

func (fake *fakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
		_ = request.Body.Close()
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	fake.mu.Lock()
	fake.requests = append(fake.requests, recordedRequest{
		method: request.Method,
		url:    request.URL.String(),
		header: request.Header.Clone(),
		body:   body,
	})
	handler := fake.handler
	fake.mu.Unlock()
	return handler(request)
}

// reply builds a recorded response body so a test can assert it was closed.
func (fake *fakeTransport) reply(status int, contentType string, data []byte) *http.Response {
	body := &trackingBody{data: data}
	fake.mu.Lock()
	fake.bodies = append(fake.bodies, body)
	fake.mu.Unlock()
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       body,
	}
}

func (fake *fakeTransport) requestList() []recordedRequest {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]recordedRequest(nil), fake.requests...)
}

func (fake *fakeTransport) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.requests)
}

func (fake *fakeTransport) lastBody() *trackingBody {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.bodies) == 0 {
		return nil
	}
	return fake.bodies[len(fake.bodies)-1]
}

// trackingBody is a bounded in-memory response body that records Close.
type trackingBody struct {
	mu     sync.Mutex
	data   []byte
	err    error
	closed bool
}

func (body *trackingBody) Read(buffer []byte) (int, error) {
	body.mu.Lock()
	defer body.mu.Unlock()
	if len(body.data) > 0 {
		read := copy(buffer, body.data)
		body.data = body.data[read:]
		return read, nil
	}
	if body.err != nil {
		return 0, body.err
	}
	return 0, io.EOF
}

func (body *trackingBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closed = true
	return nil
}

func (body *trackingBody) isClosed() bool {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closed
}

// blockingBody waits for the forwarded request context and is the deterministic
// stand-in for an in-flight provider call.
type blockingBody struct {
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func (body *blockingBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (body *blockingBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closed = true
	return nil
}

func (body *blockingBody) isClosed() bool {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closed
}

func newIDSequence() func() (gateway.UsageID, error) {
	var mu sync.Mutex
	next := 0
	return func() (gateway.UsageID, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return gateway.UsageID(fmt.Sprintf("usage-%d", next)), nil
	}
}

// fakes is the complete deterministic dependency set.
type fakes struct {
	principals *fakePrincipals
	authority  *fakeAuthority
	executions *fakeExecutionReader
	secrets    *fakeSecrets
	store      *fakeStore
	content    *fakeContent
	transport  *fakeTransport
	clock      *fakeClock
	ids        func() (gateway.UsageID, error)
}

func newFakes() *fakes {
	principals := newFakePrincipals()
	principals.set(testCredential, executionPrincipal(), nil)
	executions := newFakeExecutionReader()
	executions.set(admittedExecution())
	set := &fakes{
		principals: principals,
		authority:  newFakeAuthority(),
		executions: executions,
		secrets:    &fakeSecrets{values: []string{testSecret}},
		store:      &fakeStore{},
		content:    &fakeContent{},
		transport:  &fakeTransport{},
		clock:      &fakeClock{now: fixedNow},
		ids:        newIDSequence(),
	}
	set.transport.handler = func(*http.Request) (*http.Response, error) {
		return set.transport.reply(http.StatusOK, "application/json", []byte(completionJSON())), nil
	}
	return set
}

func (fake *fakes) dependencies() gateway.Dependencies {
	return gateway.Dependencies{
		Principals: fake.principals,
		Authorizer: authorize.NewMatrix(),
		Authority:  fake.authority,
		Secrets:    fake.secrets,
		Store:      fake.store,
		Content:    fake.content,
		Transport:  fake.transport,
		Clock:      fake.clock,
		NewID:      fake.ids,
	}
}

// harness is one gateway with its deterministic dependencies.
type harness struct {
	*fakes
	config  gateway.Config
	gateway *gateway.Gateway
}

func newHarness(t *testing.T, mutate func(*gateway.Config)) *harness {
	t.Helper()
	fake := newFakes()
	config := validConfig()
	if mutate != nil {
		mutate(&config)
	}
	instance, err := gateway.New(config, fake.dependencies(), gateway.WithExecutionReader(fake.executions))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{fakes: fake, config: config, gateway: instance}
}
