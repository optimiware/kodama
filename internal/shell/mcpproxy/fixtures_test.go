// Deterministic in-memory fakes and shared fixtures for the proxy tests.
//
// No test starts a process, opens a network connection, reads a real secret or
// writes a database. Every dependency is an injected fake with an explicit
// barrier or fault hook, and the clock is advanced by the test.
package mcpproxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenant     = id.TenantID("tenant-a")
	testProject    = id.ProjectID("project-a")
	otherTenant    = id.TenantID("tenant-b")
	otherProject   = id.ProjectID("project-b")
	testPrincipal  = id.PrincipalID("principal-engineer")
	testExecution  = id.ExecutionID("exec-1")
	testAction     = id.ActionID("action-1")
	testToken      = "kodex.exec-1.credential-material"
	testSecret     = "MCP-SECRET-123"
	testSecretName = id.SecretName("MCP_TOKEN")
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: otherTenant, ProjectID: otherProject}
}

func humanEngineer() identity.Principal {
	return identity.Principal{
		ID:      testPrincipal,
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}
}

func humanViewer() identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("principal-viewer"),
		Kind:    identity.KindHuman,
		Scope:   testScope(),
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	}
}

func executionPrincipal() identity.Principal {
	return identity.Principal{
		ID:          id.PrincipalID("principal-exec"),
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: testExecution,
	}
}

func admittedExecution() authorize.AdmittedExecution {
	return authorize.AdmittedExecution{
		Principal:   executionPrincipal(),
		ExecutionID: testExecution,
		ActionID:    testAction,
		Permission:  authorize.ExecConfiguredTools,
		Deadline:    fixedNow.Add(time.Hour),
	}
}

// testServers is the configured allow-list: two servers, each with two tools.
func testServers() []mcpproxy.Server {
	return []mcpproxy.Server{
		{
			Name:     "files",
			Endpoint: "https://mcp.example.com/files",
			Secrets:  []id.SecretName{testSecretName},
			Tools: []mcpproxy.Tool{
				{
					Name:        "read_file",
					Description: "read one file",
					Validate: func(arguments json.RawMessage) error {
						var value struct {
							Path string `json:"path"`
						}
						if err := json.Unmarshal(arguments, &value); err != nil {
							return err
						}
						if value.Path == "" {
							return errors.New("path is required")
						}
						return nil
					},
				},
				{
					Name:        "echo",
					Description: "echo JSON",
					Validate: func(arguments json.RawMessage) error {
						if !json.Valid(arguments) {
							return errors.New("arguments are not valid JSON")
						}
						return nil
					},
				},
			},
		},
		{
			Name:     "search",
			Endpoint: "https://mcp.example.com/search",
			Tools: []mcpproxy.Tool{
				{
					Name:        "query",
					Description: "search the index",
					Validate: func(arguments json.RawMessage) error {
						if !json.Valid(arguments) {
							return errors.New("arguments are not valid JSON")
						}
						return nil
					},
				},
			},
		},
	}
}

func validConfig() mcpproxy.Config {
	return newFakes().config()
}

// gate is an explicit barrier: a fake enters it, the test observes the entry,
// and only the test releases it. Waiting honours context cancellation so a
// proxy timeout/cancel test cannot deadlock.
type gate struct {
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate {
	return &gate{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gate) wait(ctx context.Context) error {
	select {
	case g.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gate) releaseAll() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// fakeClock is the injected time source and timeout signal.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []fakeTimer
	waiting chan time.Duration
}

type fakeTimer struct {
	duration time.Duration
	channel  chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, waiting: make(chan time.Duration, 16)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) After(duration time.Duration) <-chan time.Time {
	clock.mu.Lock()
	timer := fakeTimer{duration: duration, channel: make(chan time.Time, 1)}
	clock.timers = append(clock.timers, timer)
	clock.mu.Unlock()
	select {
	case clock.waiting <- duration:
	default:
	}
	return timer.channel
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// Fire delivers the most recent timer with the exact configured duration.
func (clock *fakeClock) Fire(duration time.Duration) bool {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	for _, v := range slices.Backward(clock.timers) {
		if v.duration != duration {
			continue
		}
		select {
		case v.channel <- clock.now:
		default:
		}
		return true
	}
	return false
}

// fakeCredentials is authorize.CurrentPrincipal.
type fakeCredentials struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	tokens    []string
	onCall    func(int, string) (identity.Principal, error)
}

func (fake *fakeCredentials) Resolve(_ context.Context, token string) (authorize.Principal, error) {
	fake.mu.Lock()
	fake.calls++
	call := fake.calls
	fake.tokens = append(fake.tokens, token)
	onCall := fake.onCall
	principal, err := fake.principal, fake.err
	fake.mu.Unlock()
	if onCall != nil {
		return onCall(call, token)
	}
	return principal, err
}

func (fake *fakeCredentials) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeCredentials) setPrincipal(principal identity.Principal) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.principal = principal
}

func (fake *fakeCredentials) setError(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.err = err
}

// fakeAuthorizer is authorize.Authorizer.
type fakeAuthorizer struct {
	mu       sync.Mutex
	decision authorize.Decision
	err      error
	calls    []authorizeCall
	onCall   func(int, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error)
}

type authorizeCall struct {
	principal authorize.Principal
	operation authorize.OperationName
	scope     tenancy.Scope
}

func (fake *fakeAuthorizer) Authorize(_ context.Context, principal authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Decision, error) {
	fake.mu.Lock()
	call := len(fake.calls) + 1
	fake.calls = append(fake.calls, authorizeCall{principal: principal, operation: operation, scope: scope})
	onCall := fake.onCall
	decision, err := fake.decision, fake.err
	fake.mu.Unlock()
	if onCall != nil {
		return onCall(call, principal, operation, scope)
	}
	return decision, err
}

func (fake *fakeAuthorizer) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

func (fake *fakeAuthorizer) setDecision(decision authorize.Decision) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.decision = decision
}

func (fake *fakeAuthorizer) lastCall() (authorizeCall, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		return authorizeCall{}, false
	}
	return fake.calls[len(fake.calls)-1], true
}

// fakeAuthority is authorize.AuthorityReader.
type fakeAuthority struct {
	mu      sync.Mutex
	allowed map[authorize.AuthorityCheck]bool
	err     error
	calls   [][]authorize.AuthorityRead
	onRead  func(int, []authorize.AuthorityRead) ([]authorize.AuthorityFact, error)
}

func newFakeAuthority() *fakeAuthority {
	return &fakeAuthority{allowed: map[authorize.AuthorityCheck]bool{
		authorize.CheckMembership:       true,
		authorize.CheckServiceEnabled:   true,
		authorize.CheckPrincipalEnabled: true,
		authorize.CheckExecutionRunning: true,
		authorize.CheckActionDeadline:   true,
	}}
}

func (fake *fakeAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	fake.mu.Lock()
	call := len(fake.calls) + 1
	fake.calls = append(fake.calls, append([]authorize.AuthorityRead(nil), reads...))
	onRead := fake.onRead
	err := fake.err
	allowed := make(map[authorize.AuthorityCheck]bool, len(fake.allowed))
	maps.Copy(allowed, fake.allowed)
	fake.mu.Unlock()
	if onRead != nil {
		return onRead(call, reads)
	}
	if err != nil {
		return nil, err
	}
	facts := make([]authorize.AuthorityFact, 0, len(reads))
	for _, read := range reads {
		facts = append(facts, authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Allowed:     allowed[read.Check],
		})
	}
	return facts, nil
}

func (fake *fakeAuthority) setAllowed(check authorize.AuthorityCheck, value bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.allowed[check] = value
}

func (fake *fakeAuthority) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

func (fake *fakeAuthority) lastReads() []authorize.AuthorityRead {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		return nil
	}
	return append([]authorize.AuthorityRead(nil), fake.calls[len(fake.calls)-1]...)
}

// readCalls returns every authority read request in call order.
func (fake *fakeAuthority) readCalls() [][]authorize.AuthorityRead {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	calls := make([][]authorize.AuthorityRead, len(fake.calls))
	for index, reads := range fake.calls {
		calls[index] = append([]authorize.AuthorityRead(nil), reads...)
	}
	return calls
}

// fakeExecutionReader is mcpproxy.ExecutionReader.
type fakeExecutionReader struct {
	mu       sync.Mutex
	admitted authorize.AdmittedExecution
	err      error
	calls    int
	onCall   func(int, id.ExecutionID) (authorize.AdmittedExecution, error)
}

func (fake *fakeExecutionReader) ReadExecution(_ context.Context, executionID id.ExecutionID) (authorize.AdmittedExecution, error) {
	fake.mu.Lock()
	fake.calls++
	call := fake.calls
	onCall := fake.onCall
	admitted, err := fake.admitted, fake.err
	fake.mu.Unlock()
	if onCall != nil {
		return onCall(call, executionID)
	}
	return admitted, err
}

func (fake *fakeExecutionReader) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeExecutionReader) setAdmitted(admitted authorize.AdmittedExecution) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.admitted = admitted
}

// fakeResolver is secret.Resolver.
type fakeResolver struct {
	mu        sync.Mutex
	values    map[id.SecretName]string
	err       error
	requests  []secret.ResolveRequest
	onResolve func(int, secret.ResolveRequest) ([]secret.Resolved, error)
}

func (fake *fakeResolver) Resolve(_ context.Context, request secret.ResolveRequest) ([]secret.Resolved, error) {
	fake.mu.Lock()
	call := len(fake.requests) + 1
	fake.requests = append(fake.requests, secret.ResolveRequest{
		Scope: request.Scope,
		Names: append([]id.SecretName(nil), request.Names...),
	})
	onResolve := fake.onResolve
	err := fake.err
	values := make(map[id.SecretName]string, len(fake.values))
	maps.Copy(values, fake.values)
	fake.mu.Unlock()
	if onResolve != nil {
		return onResolve(call, request)
	}
	if err != nil {
		return nil, err
	}
	resolved := make([]secret.Resolved, 0, len(request.Names))
	for _, name := range request.Names {
		value, ok := values[name]
		if !ok {
			return nil, fault.New(fault.DependencyUnavailable, fmt.Sprintf("secret %q is missing", name))
		}
		resolved = append(resolved, secret.Resolved{Name: name, Value: value, Scope: request.Scope})
	}
	return resolved, nil
}

func (fake *fakeResolver) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.requests)
}

func (fake *fakeResolver) lastRequest() (secret.ResolveRequest, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) == 0 {
		return secret.ResolveRequest{}, false
	}
	request := fake.requests[len(fake.requests)-1]
	request.Names = append([]id.SecretName(nil), request.Names...)
	return request, true
}

// trackedBody is a deterministic io.ReadCloser that records Close.
type trackedBody struct {
	mu       sync.Mutex
	data     []byte
	readErr  error
	closes   int
	readGate *gate
}

func newTrackedBody(data []byte) *trackedBody {
	return &trackedBody{data: append([]byte(nil), data...)}
}

func (body *trackedBody) Read(buffer []byte) (int, error) {
	if body.readGate != nil {
		if err := body.readGate.wait(context.Background()); err != nil {
			return 0, err
		}
	}
	body.mu.Lock()
	defer body.mu.Unlock()
	if len(body.data) == 0 {
		if body.readErr != nil {
			return 0, body.readErr
		}
		return 0, io.EOF
	}
	n := copy(buffer, body.data)
	body.data = body.data[n:]
	return n, nil
}

func (body *trackedBody) Close() error {
	body.mu.Lock()
	defer body.mu.Unlock()
	body.closes++
	return nil
}

func (body *trackedBody) closeCount() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

// blockingBody blocks in Read until Close is called. It proves the proxy closes
// the response body on cancellation instead of waiting forever for a read.
type blockingBody struct {
	enterOnce sync.Once
	closeOnce sync.Once
	release   chan struct{}
	entered   chan struct{}
	mu        sync.Mutex
	closes    int
}

func newBlockingBody() *blockingBody {
	return &blockingBody{release: make(chan struct{}), entered: make(chan struct{})}
}

func (body *blockingBody) Read(_ []byte) (int, error) {
	body.enterOnce.Do(func() { close(body.entered) })
	<-body.release
	return 0, io.EOF
}

func (body *blockingBody) Close() error {
	body.mu.Lock()
	body.closes++
	body.mu.Unlock()
	body.closeOnce.Do(func() { close(body.release) })
	return nil
}

func (body *blockingBody) closeCount() int {
	body.mu.Lock()
	defer body.mu.Unlock()
	return body.closes
}

// fakeTransport is mcpproxy.Transport.
type fakeTransport struct {
	mu      sync.Mutex
	calls   []mcpproxy.UpstreamRequest
	respond func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error)
	barrier *gate
}

func (fake *fakeTransport) Call(ctx context.Context, request mcpproxy.UpstreamRequest) (io.ReadCloser, error) {
	fake.mu.Lock()
	fake.calls = append(fake.calls, cloneUpstreamRequest(request))
	respond := fake.respond
	barrier := fake.barrier
	fake.mu.Unlock()
	if barrier != nil {
		if err := barrier.wait(ctx); err != nil {
			return nil, err
		}
	}
	if respond != nil {
		return respond(ctx, request)
	}
	return newTrackedBody([]byte(`{"content":{"text":"hello"},"isError":false}`)), nil
}

func cloneUpstreamRequest(request mcpproxy.UpstreamRequest) mcpproxy.UpstreamRequest {
	clone := request
	clone.Arguments = append(json.RawMessage(nil), request.Arguments...)
	clone.Credentials = append([]secret.Resolved(nil), request.Credentials...)
	return clone
}

func (fake *fakeTransport) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

func (fake *fakeTransport) lastCall() (mcpproxy.UpstreamRequest, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) == 0 {
		return mcpproxy.UpstreamRequest{}, false
	}
	return cloneUpstreamRequest(fake.calls[len(fake.calls)-1]), true
}

func (fake *fakeTransport) setResponse(response func(context.Context, mcpproxy.UpstreamRequest) (io.ReadCloser, error)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.respond = response
}

func (fake *fakeTransport) setBarrier(barrier *gate) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.barrier = barrier
}

// fakeStore is store.Executor.
type fakeStore struct {
	mu     sync.Mutex
	units  []store.Unit
	scopes []tenancy.Scope
	err    error
}

func (fake *fakeStore) Execute(_ context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.scopes = append(fake.scopes, scope)
	fake.units = append(fake.units, unit)
	return store.UnitResult{}, fake.err
}

func (fake *fakeStore) Read(context.Context, tenancy.Scope, string, string) (store.Row, error) {
	return store.Row{}, nil
}

func (fake *fakeStore) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.units)
}

func (fake *fakeStore) lastUnit() (store.Unit, tenancy.Scope, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.units) == 0 {
		return store.Unit{}, tenancy.Scope{}, false
	}
	return fake.units[len(fake.units)-1], fake.scopes[len(fake.scopes)-1], true
}

// allUnits returns every committed unit in commit order for schema-conformance
// checks.
func (fake *fakeStore) allUnits() []store.Unit {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]store.Unit(nil), fake.units...)
}

// fakes groups every injected dependency of one proxy under test.
type fakes struct {
	clock       *fakeClock
	credentials *fakeCredentials
	authorizer  *fakeAuthorizer
	authority   *fakeAuthority
	executions  *fakeExecutionReader
	resolver    *fakeResolver
	transport   *fakeTransport
	store       *fakeStore
}

func newFakes() *fakes {
	return &fakes{
		clock:       newFakeClock(fixedNow),
		credentials: &fakeCredentials{principal: executionPrincipal()},
		authorizer:  &fakeAuthorizer{decision: authorize.DecisionAllow},
		authority:   newFakeAuthority(),
		executions:  &fakeExecutionReader{admitted: admittedExecution()},
		resolver:    &fakeResolver{values: map[id.SecretName]string{testSecretName: testSecret}},
		transport:   &fakeTransport{},
		store:       &fakeStore{},
	}
}

func (fake *fakes) config() mcpproxy.Config {
	return mcpproxy.Config{
		Enabled:     true,
		Scope:       testScope(),
		Servers:     testServers(),
		Bounds:      mcpproxy.DefaultBounds,
		Timeout:     30 * time.Second,
		Retention:   mcpproxy.Retention{},
		Credentials: fake.credentials,
		Authorizer:  fake.authorizer,
		Authority:   fake.authority,
		Executions:  fake.executions,
		Secrets:     fake.resolver,
		Transport:   fake.transport,
		Store:       fake.store,
		Clock:       fake.clock,
	}
}

func (fake *fakes) newProxy(t *testing.T) *mcpproxy.Proxy {
	t.Helper()
	proxy, err := mcpproxy.New(fake.config())
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if proxy == nil {
		t.Fatal("New returned a nil proxy")
	}
	return proxy
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v has no fault class; want %q", err, want)
	}
	if got != want {
		t.Fatalf("fault class = %q; want %q (err: %v)", got, want, err)
	}
}

func readCallRequest() mcpproxy.CallRequest {
	return mcpproxy.CallRequest{
		Credential: testToken,
		Server:     "files",
		Tool:       "read_file",
		Arguments:  json.RawMessage(`{"path":"/etc/hosts"}`),
	}
}

// decodeDetail decodes one usage audit Detail JSON object.
func decodeDetail(t *testing.T, detail string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(detail), &value); err != nil {
		t.Fatalf("usage detail is not a JSON object: %v (detail: %q)", err, detail)
	}
	return value
}

var (
	_ mcpproxy.Clock             = (*fakeClock)(nil)
	_ mcpproxy.Transport         = (*fakeTransport)(nil)
	_ mcpproxy.ExecutionReader   = (*fakeExecutionReader)(nil)
	_ authorize.CurrentPrincipal = (*fakeCredentials)(nil)
	_ authorize.Authorizer       = (*fakeAuthorizer)(nil)
	_ authorize.AuthorityReader  = (*fakeAuthority)(nil)
	_ secret.Resolver            = (*fakeResolver)(nil)
	_ store.Executor             = (*fakeStore)(nil)
)
