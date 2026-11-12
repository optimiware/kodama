// Call behavior: authentication and authorization precede lookup, secret
// resolution and transport; current authority is refreshed immediately before
// the effect; requests cannot override trusted configuration.
package mcpproxy_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestCallHappyPathExecution(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if result.IsError {
		t.Fatal("Call reported a successful tool result as an error")
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatalf("CallResult.Content is not JSON: %v (%q)", err, result.Content)
	}
	if content.Text != "hello" {
		t.Fatalf("CallResult.Content text = %q; want hello", content.Text)
	}
	if strings.Contains(string(result.Content), testSecret) {
		t.Fatalf("CallResult.Content leaked a credential: %q", result.Content)
	}

	call, ok := fake.transport.lastCall()
	if !ok {
		t.Fatal("Call did not invoke the transport")
	}
	if call.Server != "files" {
		t.Fatalf("upstream server = %q; want files", call.Server)
	}
	if call.Endpoint != "https://mcp.example.com/files" {
		t.Fatalf("upstream endpoint = %q; want the configured endpoint", call.Endpoint)
	}
	if call.Tool != "read_file" {
		t.Fatalf("upstream tool = %q; want read_file", call.Tool)
	}
	if string(call.Arguments) != `{"path":"/etc/hosts"}` {
		t.Fatalf("upstream arguments = %q; want the validated request arguments", call.Arguments)
	}
	if call.Timeout != 30*time.Second {
		t.Fatalf("upstream timeout = %v; want 30s", call.Timeout)
	}
	if len(call.Credentials) != 1 {
		t.Fatalf("upstream credentials = %d; want exactly the configured secret", len(call.Credentials))
	}
	if call.Credentials[0].Name != testSecretName || call.Credentials[0].Value != testSecret {
		t.Fatalf("upstream credential = %+v; want the configured secret", call.Credentials[0])
	}

	request, ok := fake.resolver.lastRequest()
	if !ok {
		t.Fatal("Call did not resolve a configured secret")
	}
	if request.Scope != testScope() {
		t.Fatalf("secret resolve scope = %+v; want %+v", request.Scope, testScope())
	}
	if len(request.Names) != 1 || request.Names[0] != testSecretName {
		t.Fatalf("secret resolve names = %v; want [%s]", request.Names, testSecretName)
	}

	reads := fake.authority.lastReads()
	if !hasCheck(reads, authorize.CheckExecutionRunning) || !hasCheck(reads, authorize.CheckActionDeadline) {
		t.Fatalf("authority reads = %+v; want execution_running and action_deadline", reads)
	}
	for _, read := range reads {
		if read.Operation != authorize.OperationName("tool.invoke") {
			t.Fatalf("authority read operation = %q; want tool.invoke", read.Operation)
		}
		if read.Scope != testScope() {
			t.Fatalf("authority read scope = %+v; want %+v", read.Scope, testScope())
		}
		if read.PrincipalID != executionPrincipal().ID {
			t.Fatalf("authority read principal = %q; want %q", read.PrincipalID, executionPrincipal().ID)
		}
	}
	if fake.executions.callCount() == 0 {
		t.Fatal("Call did not read the current admitted execution")
	}
	if fake.store.callCount() != 1 {
		t.Fatalf("usage writes = %d; want 1", fake.store.callCount())
	}
}

func TestCallHappyPathHuman(t *testing.T) {
	fake := newFakes()
	fake.credentials.setPrincipal(humanEngineer())
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err != nil {
		t.Fatalf("Call returned error for an authorized human: %v", err)
	}
	if result.IsError {
		t.Fatal("Call reported a successful tool result as an error")
	}
	call, ok := fake.transport.lastCall()
	if !ok {
		t.Fatal("Call did not invoke the transport")
	}
	if call.Server != "files" || call.Tool != "read_file" {
		t.Fatalf("upstream call = %s/%s; want files/read_file", call.Server, call.Tool)
	}
	last, ok := fake.authorizer.lastCall()
	if !ok {
		t.Fatal("Call did not authorize the human caller")
	}
	if last.operation != authorize.OperationName("tool.invoke") {
		t.Fatalf("authorized operation = %q; want tool.invoke", last.operation)
	}
	if last.scope != testScope() {
		t.Fatalf("authorized scope = %+v; want %+v", last.scope, testScope())
	}
	reads := fake.authority.lastReads()
	if !hasCheck(reads, authorize.CheckMembership) {
		t.Fatalf("authority reads = %+v; want membership", reads)
	}
	if hasCheck(reads, authorize.CheckExecutionRunning) || hasCheck(reads, authorize.CheckActionDeadline) {
		t.Fatalf("human authority reads = %+v; must not include execution-only checks", reads)
	}
}

func TestCallUnknownServerRefused(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	request := readCallRequest()
	request.Server = "missing"

	result, err := proxy.Call(context.Background(), request)
	if err == nil {
		t.Fatal("Call accepted an unknown server")
	}
	assertFaultClass(t, err, fault.NotFound)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("unknown server returned a result: %+v", result)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

func TestCallUnknownToolRefused(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	request := readCallRequest()
	request.Tool = "delete_file"

	result, err := proxy.Call(context.Background(), request)
	if err == nil {
		t.Fatal("Call accepted an unknown tool")
	}
	assertFaultClass(t, err, fault.NotFound)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("unknown tool returned a result: %+v", result)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

// TestCallAuthenticatesBeforeAnyOtherDependency pins that a failed credential
// is refused before the proxy authorizes, reads authority or an execution,
// resolves a secret, calls the transport or records usage, and that the
// refusal names no configured server or tool and never echoes the credential.
func TestCallAuthenticatesBeforeAnyOtherDependency(t *testing.T) {
	fake := newFakes()
	fake.credentials.setError(fault.New(fault.Unauthenticated, "bad credential "+testToken))
	proxy := fake.newProxy(t)

	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted an unauthenticated credential")
	}
	assertFaultClass(t, err, fault.Unauthenticated)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("unauthenticated caller received a result: %+v", result)
	}
	for _, secret := range []string{"files", "read_file", "search", "query", testToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("unauthenticated error disclosed %q: %v", secret, err)
		}
	}
	if fake.credentials.callCount() != 1 {
		t.Fatalf("credential resolutions = %d; want 1", fake.credentials.callCount())
	}
	if fake.authorizer.callCount() != 0 {
		t.Fatalf("authorizer calls = %d; want 0 before authentication", fake.authorizer.callCount())
	}
	if fake.authority.callCount() != 0 {
		t.Fatalf("authority reads = %d; want 0 before authentication", fake.authority.callCount())
	}
	if fake.executions.callCount() != 0 {
		t.Fatalf("execution reads = %d; want 0 before authentication", fake.executions.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.store.callCount() != 0 {
		t.Fatalf("usage writes = %d; want 0", fake.store.callCount())
	}
}

func TestCallDeniedBeforeLookup(t *testing.T) {
	fake := newFakes()
	fake.credentials.setPrincipal(humanViewer())
	fake.authorizer.setDecision(authorize.DecisionDeny)
	proxy := fake.newProxy(t)

	// The request names a real configured tool. A denied caller must not learn
	// whether that name exists.
	result, err := proxy.Call(context.Background(), readCallRequest())
	if err == nil {
		t.Fatal("Call accepted a viewer")
	}
	assertFaultClass(t, err, fault.Forbidden)
	if len(result.Content) != 0 || result.IsError {
		t.Fatalf("denied caller received a result: %+v", result)
	}
	for _, secret := range []string{"files", "read_file", "search", "query", "missing"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("denied error disclosed %q: %v", secret, err)
		}
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
	if fake.resolver.callCount() != 0 {
		t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
	}
}

func TestCallMalformedArgumentsBeforeUpstream(t *testing.T) {
	cases := []struct {
		name      string
		arguments json.RawMessage
	}{
		{"missing required field", json.RawMessage(`{"path":""}`)},
		{"invalid json", json.RawMessage(`{"path":`)},
		{"null arguments", json.RawMessage(`null`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakes()
			proxy := fake.newProxy(t)
			request := readCallRequest()
			request.Arguments = testCase.arguments

			result, err := proxy.Call(context.Background(), request)
			if err == nil {
				t.Fatalf("Call accepted %s", testCase.name)
			}
			assertFaultClass(t, err, fault.InvalidInput)
			if len(result.Content) != 0 || result.IsError {
				t.Fatalf("invalid arguments returned a result: %+v", result)
			}
			if fake.transport.callCount() != 0 {
				t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
			}
			if fake.resolver.callCount() != 0 {
				t.Fatalf("secret resolutions = %d; want 0", fake.resolver.callCount())
			}
		})
	}
}

func TestCallResolvesOnlyConfiguredSecrets(t *testing.T) {
	fake := newFakes()
	proxy := fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	request, ok := fake.resolver.lastRequest()
	if !ok {
		t.Fatal("Call did not resolve a secret")
	}
	if len(request.Names) != 1 || request.Names[0] != testSecretName {
		t.Fatalf("resolved names = %v; want exactly [%s]", request.Names, testSecretName)
	}

	// An extra returned name is refused, never forwarded.
	fake = newFakes()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		return []secret.Resolved{
			{Name: testSecretName, Value: testSecret, Scope: request.Scope},
			{Name: id.SecretName("OTHER_SECRET"), Value: "other", Scope: request.Scope},
		}, nil
	}
	proxy = fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
		t.Fatal("Call accepted an extra resolved secret")
	} else {
		assertFaultClass(t, err, fault.DependencyUnavailable)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}

	// A missing configured value is refused.
	fake = newFakes()
	fake.resolver.values = map[id.SecretName]string{}
	proxy = fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
		t.Fatal("Call accepted a missing configured secret")
	} else {
		assertFaultClass(t, err, fault.DependencyUnavailable)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}

	// A value resolved from another scope is refused.
	fake = newFakes()
	fake.resolver.onResolve = func(_ int, _ secret.ResolveRequest) ([]secret.Resolved, error) {
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: otherScope()}}, nil
	}
	proxy = fake.newProxy(t)
	if _, err := proxy.Call(context.Background(), readCallRequest()); err == nil {
		t.Fatal("Call accepted a secret from another scope")
	} else {
		assertFaultClass(t, err, fault.DependencyUnavailable)
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0", fake.transport.callCount())
	}
}

func TestCallRefreshesAuthorityBeforeTransport(t *testing.T) {
	fake := newFakes()
	gate := newGate()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		if err := gate.wait(context.Background()); err != nil {
			return nil, err
		}
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: request.Scope}}, nil
	}
	proxy := fake.newProxy(t)

	type outcome struct {
		result mcpproxy.CallResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := proxy.Call(context.Background(), readCallRequest())
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the secret-resolution barrier")
	}
	if fake.transport.callCount() != 0 {
		t.Fatal("transport was called before secret resolution completed")
	}

	// Revoke every refresh path after the initial authorization.
	fake.credentials.onCall = func(call int, _ string) (identity.Principal, error) {
		if call >= 2 {
			principal := executionPrincipal()
			principal.Enabled = false
			return principal, nil
		}
		return executionPrincipal(), nil
	}
	fake.authorizer.setDecision(authorize.DecisionDeny)
	admitted := admittedExecution()
	admitted.Permission = authorize.ExecNone
	fake.executions.setAdmitted(admitted)
	fake.authority.setAllowed(authorize.CheckExecutionRunning, false)
	gate.releaseAll()

	select {
	case result := <-done:
		if result.err == nil {
			t.Fatal("Call proceeded after current authority was revoked")
		}
		assertFaultClass(t, result.err, fault.Forbidden)
		if len(result.result.Content) != 0 {
			t.Fatalf("revoked caller received content: %q", result.result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the authority refresh")
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 after the authority refresh", fake.transport.callCount())
	}
}

func TestCallPreEffectDeadlineExpired(t *testing.T) {
	fake := newFakes()
	gate := newGate()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		if err := gate.wait(context.Background()); err != nil {
			return nil, err
		}
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: request.Scope}}, nil
	}
	proxy := fake.newProxy(t)

	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(context.Background(), readCallRequest())
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the secret-resolution barrier")
	}

	fake.clock.Advance(2 * time.Hour)
	fake.authority.setAllowed(authorize.CheckActionDeadline, false)
	gate.releaseAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call proceeded after the action deadline passed")
		}
		assertFaultClass(t, err, fault.DeadlineExpired)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the deadline refresh")
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 after pre-effect expiry", fake.transport.callCount())
	}
}

func TestCallRevokedCredentialRefresh(t *testing.T) {
	fake := newFakes()
	gate := newGate()
	fake.resolver.onResolve = func(_ int, request secret.ResolveRequest) ([]secret.Resolved, error) {
		if err := gate.wait(context.Background()); err != nil {
			return nil, err
		}
		return []secret.Resolved{{Name: testSecretName, Value: testSecret, Scope: request.Scope}}, nil
	}
	fake.credentials.onCall = func(call int, _ string) (identity.Principal, error) {
		if call >= 2 {
			return identity.Principal{}, fault.New(fault.Unauthenticated, "credential revoked")
		}
		return executionPrincipal(), nil
	}
	fake.executions.onCall = func(call int, _ id.ExecutionID) (authorize.AdmittedExecution, error) {
		if call >= 2 {
			return authorize.AdmittedExecution{}, fault.New(fault.Unauthenticated, "execution revoked")
		}
		return admittedExecution(), nil
	}
	proxy := fake.newProxy(t)

	done := make(chan error, 1)
	go func() {
		_, err := proxy.Call(context.Background(), readCallRequest())
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not reach the secret-resolution barrier")
	}
	gate.releaseAll()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Call proceeded after the credential was revoked")
		}
		assertFaultClass(t, err, fault.Unauthenticated)
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after the credential refresh")
	}
	if fake.transport.callCount() != 0 {
		t.Fatalf("transport calls = %d; want 0 after revocation", fake.transport.callCount())
	}
}

func TestCallDeniedCallersMakeZeroUpstreamCalls(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakes)
		class fault.Class
	}{
		{
			name: "unauthenticated",
			setup: func(fake *fakes) {
				fake.credentials.setError(fault.New(fault.Unauthenticated, "bad credential"))
			},
			class: fault.Unauthenticated,
		},
		{
			name: "disabled human",
			setup: func(fake *fakes) {
				principal := humanEngineer()
				principal.Enabled = false
				fake.credentials.setPrincipal(principal)
				fake.authorizer.setDecision(authorize.DecisionDeny)
			},
			class: fault.Forbidden,
		},
		{
			name: "viewer",
			setup: func(fake *fakes) {
				fake.credentials.setPrincipal(humanViewer())
				fake.authorizer.setDecision(authorize.DecisionDeny)
			},
			class: fault.Forbidden,
		},
		{
			name: "execution without configured tools permission",
			setup: func(fake *fakes) {
				admitted := admittedExecution()
				admitted.Permission = authorize.ExecNone
				fake.executions.setAdmitted(admitted)
			},
			class: fault.Forbidden,
		},
		{
			name: "cross-scope human",
			setup: func(fake *fakes) {
				principal := humanEngineer()
				principal.Scope = otherScope()
				fake.credentials.setPrincipal(principal)
				fake.authorizer.setDecision(authorize.DecisionDeny)
			},
			class: fault.Forbidden,
		},
		{
			name: "stopped execution",
			setup: func(fake *fakes) {
				fake.authority.setAllowed(authorize.CheckExecutionRunning, false)
			},
			class: fault.Forbidden,
		},
		{
			name: "action deadline passed",
			setup: func(fake *fakes) {
				fake.authority.setAllowed(authorize.CheckActionDeadline, false)
			},
			class: fault.DeadlineExpired,
		},
		{
			// A transient execution-read failure is a dependency problem, not
			// a refusal of the caller.
			name: "execution read failure",
			setup: func(fake *fakes) {
				fake.executions.err = fault.New(fault.DependencyUnavailable, "store unavailable")
			},
			class: fault.DependencyUnavailable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakes()
			testCase.setup(fake)
			proxy := fake.newProxy(t)

			result, err := proxy.Call(context.Background(), readCallRequest())
			if err == nil {
				t.Fatalf("Call accepted %s", testCase.name)
			}
			assertFaultClass(t, err, testCase.class)
			if len(result.Content) != 0 || result.IsError {
				t.Fatalf("%s returned a result: %+v", testCase.name, result)
			}
			if fake.transport.callCount() != 0 {
				t.Fatalf("%s: transport calls = %d; want 0", testCase.name, fake.transport.callCount())
			}
			if fake.resolver.callCount() != 0 {
				t.Fatalf("%s: secret resolutions = %d; want 0", testCase.name, fake.resolver.callCount())
			}
		})
	}
}

func hasCheck(reads []authorize.AuthorityRead, check authorize.AuthorityCheck) bool {
	for _, read := range reads {
		if read.Check == check {
			return true
		}
	}
	return false
}
