package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// TestInvokeForwardsConfiguredModelAndReturnsCompletion is the settled success
// path: authenticate, authorize, refresh authority, resolve the one configured
// secret, forward the mapped model under the configured completion cap, decode
// the bounded response, close the body and persist one attributable usage row.
func TestInvokeForwardsConfiguredModelAndReturnsCompletion(t *testing.T) {
	h := newHarness(t, nil)

	response, err := h.gateway.Invoke(context.Background(), invocation())
	requireNoError(t, err)
	if response.ID != "cmpl-1" {
		t.Errorf("completion id = %q; want cmpl-1", response.ID)
	}
	if response.Usage != (api.ChatUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}) {
		t.Errorf("usage = %#v; want 7/5/12", response.Usage)
	}

	if got := h.transport.count(); got != 1 {
		t.Fatalf("upstream calls = %d; want 1", got)
	}
	forwarded := h.transport.requestList()[0]
	if forwarded.method != http.MethodPost {
		t.Errorf("forwarded method = %q; want POST", forwarded.method)
	}
	if forwarded.url != testEndpoint {
		t.Errorf("forwarded url = %q; want %q", forwarded.url, testEndpoint)
	}
	if got := forwarded.header.Get("Authorization"); got != "Bearer "+testSecret {
		t.Errorf("forwarded Authorization = %q; want the resolved provider secret", got)
	}
	if strings.Contains(string(forwarded.body), testCredential) {
		t.Error("forwarded request carries the caller credential")
	}
	var body map[string]any
	if err := json.Unmarshal(forwarded.body, &body); err != nil {
		t.Fatalf("forwarded body is not JSON: %v", err)
	}
	if body["model"] != testUpstream {
		t.Errorf("forwarded model = %v; want the configured upstream model %q", body["model"], testUpstream)
	}
	if body["max_tokens"] != float64(64) {
		t.Errorf("forwarded max_tokens = %v; want the configured cap 64 when omitted", body["max_tokens"])
	}
	if _, present := body["stream"]; present {
		t.Error("forwarded body carries a stream member; the declared subset is non-streaming")
	}

	if got := h.principals.count(); got != 1 {
		t.Errorf("principal resolutions = %d; want 1", got)
	}
	reads := h.authority.lastReads()
	operation, ok := authorize.LookupOperation("model.invoke")
	if !ok {
		t.Fatal("model.invoke is not registered")
	}
	if len(reads) != len(operation.Authority) {
		t.Fatalf("authority reads = %d; want the registry row's %d checks", len(reads), len(operation.Authority))
	}
	for index, read := range reads {
		if read.Operation != operation.Name {
			t.Errorf("authority read %d operation = %q; want %q", index, read.Operation, operation.Name)
		}
		if read.Check != operation.Authority[index] {
			t.Errorf("authority read %d check = %q; want %q", index, read.Check, operation.Authority[index])
		}
		if read.Scope != testScope() {
			t.Errorf("authority read %d scope = %#v; want %#v", index, read.Scope, testScope())
		}
		if read.PrincipalID != testPrincipal {
			t.Errorf("authority read %d principal = %q; want %q", index, read.PrincipalID, testPrincipal)
		}
		switch read.Check {
		case authorize.CheckExecutionRunning:
			if read.TargetID != string(testExecution) {
				t.Errorf("execution-running target = %q; want the admitted execution %q", read.TargetID, testExecution)
			}
		case authorize.CheckActionDeadline:
			if read.TargetID != string(testAction) {
				t.Errorf("action-deadline target = %q; want the admitted action %q", read.TargetID, testAction)
			}
		}
	}
	if got := h.executions.count(); got != 1 {
		t.Errorf("execution reads = %d; want exactly one admitted-context read", got)
	}

	if got := h.secrets.count(); got != 1 {
		t.Fatalf("secret resolves = %d; want 1", got)
	}
	resolve := h.secrets.requestList()[0]
	if resolve.Scope != testScope() {
		t.Errorf("secret resolve scope = %#v; want %#v", resolve.Scope, testScope())
	}
	if len(resolve.Names) != 1 || resolve.Names[0] != testSecretName {
		t.Errorf("secret resolve names = %v; want exactly the configured %q", resolve.Names, testSecretName)
	}

	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want exactly 1", len(calls))
	}
	if calls[0].scope != testScope() {
		t.Errorf("usage write scope = %#v; want %#v", calls[0].scope, testScope())
	}
	if calls[0].ctxErr != nil {
		t.Errorf("usage settlement context error = %v; want a live settlement context", calls[0].ctxErr)
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["principal_id"] != string(testPrincipal) || values["execution_id"] != string(testExecution) {
		t.Errorf("usage attribution = %v/%v; want %q/%q",
			values["principal_id"], values["execution_id"], testPrincipal, testExecution)
	}
	if values["model"] != testModel || values["upstream_model"] != testUpstream {
		t.Errorf("usage model = %v/%v; want %q/%q", values["model"], values["upstream_model"], testModel, testUpstream)
	}
	if values["outcome"] != string(gateway.UsageSucceeded) || values["usage_known"] != true {
		t.Errorf("usage outcome = %v/%v; want succeeded/known", values["outcome"], values["usage_known"])
	}
	if values["prompt_tokens"] != int64(7) || values["completion_tokens"] != int64(5) || values["total_tokens"] != int64(12) {
		t.Errorf("usage tokens = %v/%v/%v; want 7/5/12",
			values["prompt_tokens"], values["completion_tokens"], values["total_tokens"])
	}

	if got := h.content.count(); got != 0 {
		t.Errorf("content records = %d; raw content must be off by default", got)
	}
	if body := h.transport.lastBody(); body == nil || !body.isClosed() {
		t.Error("upstream response body was not closed")
	}
}

// TestInvokeRefusesUnknownModelWithoutUpstreamCall proves an unconfigured model
// is refused inside the derived project scope before any secret or provider
// dependency is touched.
func TestInvokeRefusesUnknownModelWithoutUpstreamCall(t *testing.T) {
	h := newHarness(t, nil)
	request := invocation()
	request.Request.Model = "unlisted-model"
	_, err := h.gateway.Invoke(context.Background(), request)
	requireClass(t, err, fault.NotFound)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("refused unknown model touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesExecutionWithoutConfiguredModelsPermission proves the
// admitted execution permission is the registry's configured_models grant, not
// any execution credential.
func TestInvokeRefusesExecutionWithoutConfiguredModelsPermission(t *testing.T) {
	h := newHarness(t, nil)
	admitted := admittedExecution()
	admitted.Permission = authorize.ExecOwnOutputs
	h.executions.set(admitted)
	request := invocation()
	// Only the admitted execution read may decide the configured-models grant;
	// Invocation carries no caller-supplied permission to claim it.
	_, err := h.gateway.Invoke(context.Background(), request)
	requireClass(t, err, fault.Forbidden)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("denied execution touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesCrossProjectScope proves project ownership is derived from
// the resolved principal and a claimed project outside it is refused.
func TestInvokeRefusesCrossProjectScope(t *testing.T) {
	h := newHarness(t, nil)
	request := invocation()
	request.ProjectID = otherProject
	_, err := h.gateway.Invoke(context.Background(), request)
	requireClass(t, err, fault.Forbidden)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("cross-project caller touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}

	invalid := invocation()
	invalid.ProjectID = id.ProjectID("../project")
	_, err = h.gateway.Invoke(context.Background(), invalid)
	requireClass(t, err, fault.InvalidInput)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("invalid project touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesRevokedPrincipal proves a resolved-but-disabled execution is
// refused before forwarding.
func TestInvokeRefusesRevokedPrincipal(t *testing.T) {
	h := newHarness(t, nil)
	revoked := executionPrincipal()
	revoked.Enabled = false
	h.principals.set(testCredential, revoked, nil)
	admitted := admittedExecution()
	admitted.Principal.Enabled = false
	h.executions.set(admitted)
	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.Forbidden)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("revoked principal touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeAllowsHumanEngineerThroughMatrix proves the shared authorizer, not
// a gateway-local role check, decides a human caller.
func TestInvokeAllowsHumanEngineerThroughMatrix(t *testing.T) {
	h := newHarness(t, nil)
	h.principals.set(testCredential, humanEngineer(), nil)
	request := invocation()
	_, err := h.gateway.Invoke(context.Background(), request)
	requireNoError(t, err)
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1", len(calls))
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["principal_id"] != "principal-engineer" {
		t.Errorf("usage principal = %v; want the human engineer", values["principal_id"])
	}
	if value, present := values["execution_id"]; !present || value != nil {
		t.Errorf("usage execution = %v (present=%v); want SQL NULL for a human caller", value, present)
	}
}

// TestInvokeHumanCallerSkipsExecutionOnlyAuthorityReads proves a human caller
// never emits an execution-scoped authority read: a real authority reader
// rejects an execution or action check without a target, so the gateway must
// ask only for the membership fact a human can satisfy.
func TestInvokeHumanCallerSkipsExecutionOnlyAuthorityReads(t *testing.T) {
	h := newHarness(t, nil)
	h.principals.set(testCredential, humanEngineer(), nil)
	request := invocation()
	_, err := h.gateway.Invoke(context.Background(), request)
	requireNoError(t, err)

	reads := h.authority.lastReads()
	if len(reads) != 1 || reads[0].Check != authorize.CheckMembership {
		t.Fatalf("human authority reads = %#v; want exactly the membership check", reads)
	}
	if got := h.executions.count(); got != 0 {
		t.Errorf("execution reads = %d; want 0 for a human caller", got)
	}
}

// TestInvokeRefusesExecutionWithoutActiveAction proves an admitted execution
// with no current action cannot be evaluated against the registry's
// action-deadline fact and is refused before any authority read or effect.
func TestInvokeRefusesExecutionWithoutActiveAction(t *testing.T) {
	h := newHarness(t, nil)
	admitted := admittedExecution()
	admitted.ActionID = ""
	h.executions.set(admitted)

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.Forbidden)
	if h.authority.count() != 0 || h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("actionless execution touched a dependency: authority=%d transport=%d secrets=%d store=%d",
			h.authority.count(), h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesExpiredAdmittedDeadline proves the gateway refuses an
// execution whose admitted action deadline has passed even when the authority
// reader's own clock still reports the fact as live.
func TestInvokeRefusesExpiredAdmittedDeadline(t *testing.T) {
	h := newHarness(t, nil)
	admitted := admittedExecution()
	admitted.Deadline = fixedNow
	h.executions.set(admitted)

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.DeadlineExpired)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("expired execution touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesExecutionReaderFailure proves an unreadable admitted
// execution is a fail-closed refusal, never an implicit allow.
func TestInvokeRefusesExecutionReaderFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.executions.setError(errors.New("execution store unavailable"))

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.InternalFailure)
	if h.authority.count() != 0 || h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("unreadable execution touched a dependency: authority=%d transport=%d secrets=%d store=%d",
			h.authority.count(), h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesHumanViewer proves the registry grant is enforced for humans
// too.
func TestInvokeRefusesHumanViewer(t *testing.T) {
	h := newHarness(t, nil)
	h.principals.set(testCredential, humanViewer(), nil)
	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.Forbidden)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("viewer touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeRefusesUnverifiableCredential proves authentication failure is a
// redacted unauthenticated refusal with no effect.
func TestInvokeRefusesUnverifiableCredential(t *testing.T) {
	h := newHarness(t, nil)
	h.principals.set(testCredential, executionPrincipal(), errors.New("unknown credential "+testCredential))
	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.Unauthenticated)
	if strings.Contains(err.Error(), testCredential) {
		t.Errorf("authentication error leaks the credential: %v", err)
	}
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("unauthenticated caller touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}

	_, err = h.gateway.Invoke(context.Background(), gateway.Invocation{
		ProjectID: testProject,
		Request:   invocation().Request,
	})
	requireClass(t, err, fault.Unauthenticated)
}

// TestInvokeRefusesRequestOutsideDeclaredSubset pins the non-streaming OpenAI
// subset and the configured request bound before any dependency effect.
func TestInvokeRefusesRequestOutsideDeclaredSubset(t *testing.T) {
	temperature := 2.5
	cases := []struct {
		name   string
		config func(*gateway.Config)
		mutate func(*gateway.Invocation)
	}{
		{"stream is refused", nil, func(request *gateway.Invocation) { request.Request.Stream = true }},
		{"empty model", nil, func(request *gateway.Invocation) { request.Request.Model = "" }},
		{"no messages", nil, func(request *gateway.Invocation) { request.Request.Messages = nil }},
		{"unknown chat role", nil, func(request *gateway.Invocation) { request.Request.Messages[0].Role = "developer" }},
		{"negative max tokens", nil, func(request *gateway.Invocation) { request.Request.MaxTokens = -1 }},
		{"max tokens above the declared subset", nil, func(request *gateway.Invocation) {
			request.Request.MaxTokens = api.MaxCompletionTokens + 1
		}},
		{"temperature out of range", nil, func(request *gateway.Invocation) {
			request.Request.Temperature = &temperature
		}},
		{"request bytes above the configured bound", func(config *gateway.Config) {
			config.Models[0].MaxRequestBytes = 128
		}, func(request *gateway.Invocation) {
			request.Request.Messages[0].Content = strings.Repeat("x", 256)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, testCase.config)
			request := invocation()
			testCase.mutate(&request)
			_, err := h.gateway.Invoke(context.Background(), request)
			requireClass(t, err, fault.InvalidInput)
			if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
				t.Fatalf("invalid request touched a dependency: transport=%d secrets=%d store=%d",
					h.transport.count(), h.secrets.count(), h.store.count())
			}
		})
	}
}

// TestInvokeAppliesConfiguredCompletionCap pins the configured cap for an
// omitted, a smaller and an explicit oversized max_tokens.
func TestInvokeAppliesConfiguredCompletionCap(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		want      int
	}{
		{"omitted uses the cap", 0, 64},
		{"smaller request is preserved", 32, 32},
		{"exact cap is preserved", 64, 64},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			request := invocation()
			request.Request.MaxTokens = testCase.requested
			_, err := h.gateway.Invoke(context.Background(), request)
			requireNoError(t, err)
			var body map[string]any
			if err := json.Unmarshal(h.transport.requestList()[0].body, &body); err != nil {
				t.Fatalf("forwarded body is not JSON: %v", err)
			}
			if body["max_tokens"] != float64(testCase.want) {
				t.Errorf("forwarded max_tokens = %v; want %d", body["max_tokens"], testCase.want)
			}
		})
	}

	t.Run("oversized request is refused", func(t *testing.T) {
		h := newHarness(t, nil)
		request := invocation()
		request.Request.MaxTokens = 65
		_, err := h.gateway.Invoke(context.Background(), request)
		requireClass(t, err, fault.InvalidInput)
		if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
			t.Fatalf("oversized completion touched a dependency: transport=%d secrets=%d store=%d",
				h.transport.count(), h.secrets.count(), h.store.count())
		}
	})
}

// TestInvokeRefreshesAuthorityBeforeForwarding is the revocation barrier: a
// caller allowed at authorization time is refused when the current authority
// fact changed immediately before forwarding, with no secret fetched and no
// provider call.
func TestInvokeRefreshesAuthorityBeforeForwarding(t *testing.T) {
	h := newHarness(t, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.authority.before = func() {
		close(entered)
		<-release
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.gateway.Invoke(context.Background(), invocation())
		done <- err
	}()

	<-entered
	h.authority.deny(authorize.CheckExecutionRunning)
	close(release)

	err := <-done
	requireClass(t, err, fault.Forbidden)
	if h.transport.count() != 0 {
		t.Errorf("upstream calls = %d; want 0 after revocation", h.transport.count())
	}
	if h.secrets.count() != 0 {
		t.Errorf("secret resolves = %d; want 0: a revoked caller must not reach secret custody", h.secrets.count())
	}
	if h.store.count() != 0 {
		t.Errorf("usage writes = %d; want 0 for a refused caller", h.store.count())
	}
}

// TestInvokeRefusesDeniedActionDeadline proves a current action-deadline denial
// refuses before forwarding.
func TestInvokeRefusesDeniedActionDeadline(t *testing.T) {
	h := newHarness(t, nil)
	h.authority.deny(authorize.CheckActionDeadline)
	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.DeadlineExpired)
	if h.transport.count() != 0 || h.secrets.count() != 0 || h.store.count() != 0 {
		t.Fatalf("expired action touched a dependency: transport=%d secrets=%d store=%d",
			h.transport.count(), h.secrets.count(), h.store.count())
	}
}

// TestInvokeResolvesOnlyConfiguredSecretPerCall proves a rotation takes effect
// on the next call and no other declared secret is ever requested.
func TestInvokeResolvesOnlyConfiguredSecretPerCall(t *testing.T) {
	h := newHarness(t, nil)
	h.secrets.values = []string{"provider-secret-one", "provider-secret-two"}
	want := []string{"provider-secret-one", "provider-secret-two"}
	for index, value := range want {
		_, err := h.gateway.Invoke(context.Background(), invocation())
		requireNoError(t, err)
		requests := h.transport.requestList()
		if got := requests[index].header.Get("Authorization"); got != "Bearer "+value {
			t.Errorf("call %d Authorization = %q; want the rotated secret %q", index, got, value)
		}
	}
	for index, resolve := range h.secrets.requestList() {
		if len(resolve.Names) != 1 || resolve.Names[0] != testSecretName {
			t.Errorf("resolve %d names = %v; want exactly %q", index, resolve.Names, testSecretName)
		}
	}
	if h.secrets.count() != 2 {
		t.Errorf("secret resolves = %d; want one per call", h.secrets.count())
	}
}

// TestInvokeRefusesUnavailableSecret proves a secret-custody failure is a
// redacted dependency_unavailable refusal with no provider call and no usage
// row.
func TestInvokeRefusesUnavailableSecret(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*harness)
	}{
		{"resolver failure", func(h *harness) {
			h.secrets.err = errors.New("custody unavailable for " + testSecret)
		}},
		{"empty resolved value", func(h *harness) {
			h.secrets.values = []string{""}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			testCase.prepare(h)
			_, err := h.gateway.Invoke(context.Background(), invocation())
			requireClass(t, err, fault.DependencyUnavailable)
			if strings.Contains(err.Error(), testSecret) {
				t.Errorf("secret failure leaks the provider credential: %v", err)
			}
			if h.transport.count() != 0 {
				t.Errorf("upstream calls = %d; want 0 without a current provider credential", h.transport.count())
			}
			if h.store.count() != 0 {
				t.Errorf("usage writes = %d; want 0 before dispatch", h.store.count())
			}
		})
	}
}

// TestInvokeRedactsProviderCredentialAndCallerCredential proves neither
// credential reaches errors, usage rows, content records or the forwarded body
// when an ambiguous transport failure is settled as an uncertain effect.
func TestInvokeRedactsProviderCredentialAndCallerCredential(t *testing.T) {
	h := newHarness(t, func(config *gateway.Config) { config.Models[0].RecordContent = true })
	h.transport.handler = func(*http.Request) (*http.Response, error) {
		return nil, errors.New("read models.example.test: provider key " + testSecret + " rejected")
	}

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.UncertainExternalEffect)
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("error leaks the provider credential: %v", err)
	}
	if strings.Contains(err.Error(), testCredential) {
		t.Errorf("error leaks the caller credential: %v", err)
	}
	forwarded := h.transport.requestList()[0]
	if strings.Contains(string(forwarded.body), testCredential) ||
		strings.Contains(forwarded.header.Get("Authorization"), testCredential) {
		t.Error("forwarded request carries the caller credential")
	}
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1 for a dispatched call", len(calls))
	}
	values := singleInsert(t, calls[0].unit).Values
	if rendered := fmt.Sprintf("%v", values); strings.Contains(rendered, testSecret) || strings.Contains(rendered, testCredential) {
		t.Errorf("usage row leaks a credential: %s", rendered)
	}
	if values["outcome"] != string(gateway.UsageUncertain) || values["usage_known"] != false {
		t.Errorf("uncertain outcome = %v/%v; want uncertain/unknown", values["outcome"], values["usage_known"])
	}
	if h.content.count() != 0 {
		t.Errorf("content records = %d; want 0 for a lost call", h.content.count())
	}
	if body := h.transport.lastBody(); body != nil {
		t.Error("a failed transport unexpectedly returned a body")
	}
}

// TestInvokeRefusesProviderFailures pins the redacted dependency_unavailable
// mapping and the honest unknown usage row for definitive provider refusals.
func TestInvokeRefusesProviderFailures(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"provider authentication", http.StatusUnauthorized},
		{"provider rate limit", http.StatusTooManyRequests},
		{"provider outage", http.StatusServiceUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.transport.handler = func(*http.Request) (*http.Response, error) {
				return h.transport.reply(testCase.status, "application/json",
					[]byte(`{"error":"invalid api key sk-provider-secret"}`)), nil
			}
			_, err := h.gateway.Invoke(context.Background(), invocation())
			requireClass(t, err, fault.DependencyUnavailable)
			if strings.Contains(err.Error(), "sk-provider-secret") {
				t.Errorf("error leaks the provider body: %v", err)
			}
			if h.transport.count() != 1 {
				t.Errorf("upstream calls = %d; want exactly one attempt", h.transport.count())
			}
			if body := h.transport.lastBody(); body == nil || !body.isClosed() {
				t.Error("provider failure response body was not closed")
			}
			calls := h.store.callList()
			if len(calls) != 1 {
				t.Fatalf("usage writes = %d; want 1", len(calls))
			}
			values := singleInsert(t, calls[0].unit).Values
			if values["outcome"] != string(gateway.UsageFailed) || values["usage_known"] != false {
				t.Errorf("usage = %v/%v; want failed/unknown", values["outcome"], values["usage_known"])
			}
		})
	}
}

// TestInvokeRefusesMalformedAndOversizedResponses pins the bounded,
// non-streaming response contract and that every body is closed.
func TestInvokeRefusesMalformedAndOversizedResponses(t *testing.T) {
	cases := []struct {
		name        string
		config      func(*gateway.Config)
		contentType string
		body        string
	}{
		{"malformed json", nil, "application/json", `{"id":`},
		{"invalid completion", nil, "application/json",
			`{"id":"cmpl-x","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
		{"oversized response", func(config *gateway.Config) { config.Models[0].MaxResponseBytes = 64 }, "application/json", completionJSON()},
		{"event stream response", nil, "text/event-stream", "data: {\"id\":\"cmpl-1\"}\n\n"},
		{"stream body on a json content type", nil, "application/json", "data: {\"id\":\"cmpl-1\"}\n\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, testCase.config)
			h.transport.handler = func(*http.Request) (*http.Response, error) {
				return h.transport.reply(http.StatusOK, testCase.contentType, []byte(testCase.body)), nil
			}
			_, err := h.gateway.Invoke(context.Background(), invocation())
			requireClass(t, err, fault.DependencyUnavailable)
			if h.transport.count() != 1 {
				t.Errorf("upstream calls = %d; want exactly one attempt", h.transport.count())
			}
			if body := h.transport.lastBody(); body == nil || !body.isClosed() {
				t.Error("refused response body was not closed")
			}
			calls := h.store.callList()
			if len(calls) != 1 {
				t.Fatalf("usage writes = %d; want 1 for a dispatched call", len(calls))
			}
			values := singleInsert(t, calls[0].unit).Values
			if values["outcome"] != string(gateway.UsageFailed) || values["usage_known"] != false {
				t.Errorf("usage = %v/%v; want failed/unknown", values["outcome"], values["usage_known"])
			}
			if h.content.count() != 0 {
				t.Errorf("content records = %d; want 0", h.content.count())
			}
		})
	}
}

// TestInvokeMapsLostResponseToUncertainWithoutReplay is the lost-response case:
// the provider answered headers but the body is lost, so the outcome is unknown
// and the gateway must not retry.
func TestInvokeMapsLostResponseToUncertainWithoutReplay(t *testing.T) {
	h := newHarness(t, nil)
	h.transport.handler = func(*http.Request) (*http.Response, error) {
		body := &trackingBody{data: []byte(`{"id":"cmpl-1"`), err: io.ErrUnexpectedEOF}
		h.transport.mu.Lock()
		h.transport.bodies = append(h.transport.bodies, body)
		h.transport.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}, nil
	}

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.UncertainExternalEffect)
	if h.transport.count() != 1 {
		t.Fatalf("upstream calls = %d; want exactly one: an uncertain attempt is never replayed", h.transport.count())
	}
	if body := h.transport.lastBody(); body == nil || !body.isClosed() {
		t.Error("lost-response body was not closed")
	}
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1", len(calls))
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["outcome"] != string(gateway.UsageUncertain) || values["usage_known"] != false {
		t.Errorf("usage = %v/%v; want uncertain/unknown", values["outcome"], values["usage_known"])
	}
}

// TestInvokeSettlesAmbiguousTransportFailureAsUncertain proves a transport
// error that is not provably pre-write may have followed a request the
// provider already processed: an EOF waiting for headers, a connection reset
// or a transport that reports no response at all is settled as
// uncertain_external_effect with an uncertain usage row, and the attempt is
// never replayed. A dial-shaped message without transport proof is deliberately
// included: the gateway never guesses pre-write safety from error text.
func TestInvokeSettlesAmbiguousTransportFailureAsUncertain(t *testing.T) {
	cases := []struct {
		name  string
		fail  error
		empty bool
	}{
		{name: "EOF waiting for headers", fail: io.ErrUnexpectedEOF},
		{name: "connection reset after the body", fail: errors.New("read tcp 10.0.0.7:51234: connection reset by peer")},
		{name: "unproven dial-shaped message", fail: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")},
		{name: "no response after dispatch", empty: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.transport.handler = func(*http.Request) (*http.Response, error) {
				if testCase.empty {
					return nil, nil
				}
				return nil, testCase.fail
			}

			_, err := h.gateway.Invoke(context.Background(), invocation())
			requireClass(t, err, fault.UncertainExternalEffect)
			if h.transport.count() != 1 {
				t.Fatalf("upstream calls = %d; want exactly one: an ambiguous attempt is never replayed", h.transport.count())
			}
			calls := h.store.callList()
			if len(calls) != 1 {
				t.Fatalf("usage writes = %d; want 1 for a dispatched call", len(calls))
			}
			values := singleInsert(t, calls[0].unit).Values
			if values["outcome"] != string(gateway.UsageUncertain) || values["usage_known"] != false {
				t.Errorf("usage = %v/%v; want uncertain/unknown", values["outcome"], values["usage_known"])
			}
			if h.content.count() != 0 {
				t.Errorf("content records = %d; want 0 for an ambiguous call", h.content.count())
			}
		})
	}
}

// TestInvokeSettlesProvenPreWriteTransportFailureAsFailed proves the one
// definitive case: a transport that proves the failure preceded any request
// bytes (a connection that could not be dialled) keeps the attempt a
// replay-safe dependency_unavailable with a failed usage row.
func TestInvokeSettlesProvenPreWriteTransportFailureAsFailed(t *testing.T) {
	h := newHarness(t, nil)
	dialError := errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	h.transport.handler = func(*http.Request) (*http.Response, error) {
		return nil, dialError
	}
	h.transport.preWrite = func(err error) bool { return errors.Is(err, dialError) }

	_, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.DependencyUnavailable)
	if h.transport.count() != 1 {
		t.Fatalf("upstream calls = %d; want exactly one attempt", h.transport.count())
	}
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1", len(calls))
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["outcome"] != string(gateway.UsageFailed) || values["usage_known"] != false {
		t.Errorf("usage = %v/%v; want failed/unknown", values["outcome"], values["usage_known"])
	}
}

// TestInvokeReportsUnattributedCallAsUncertain proves a dispatched call whose
// usage row cannot be persisted is not reported as success or as a replay-safe
// internal_failure: the provider already acted, so the caller sees
// uncertain_external_effect, the completion is not returned and nothing is
// replayed.
func TestInvokeReportsUnattributedCallAsUncertain(t *testing.T) {
	h := newHarness(t, nil)
	h.store.err = errors.New("store unavailable")

	response, err := h.gateway.Invoke(context.Background(), invocation())
	requireClass(t, err, fault.UncertainExternalEffect)
	if response.ID != "" || len(response.Choices) != 0 {
		t.Errorf("completion = %#v; want no result for an unattributed call", response)
	}
	if h.transport.count() != 1 {
		t.Fatalf("upstream calls = %d; want exactly one", h.transport.count())
	}
	if h.store.count() != 1 {
		t.Fatalf("usage writes = %d; want one attempted settlement", h.store.count())
	}
	if h.content.count() != 0 {
		t.Errorf("content records = %d; want 0 when attribution was lost", h.content.count())
	}
}

// TestInvokeHonoursConfiguredTimeout proves the configured finite timeout
// cancels the in-flight call, closes the body and still settles usage on a live
// context.
func TestInvokeHonoursConfiguredTimeout(t *testing.T) {
	h := newHarness(t, func(config *gateway.Config) {
		config.Models[0].Timeout = 25 * time.Millisecond
	})
	var body *blockingBody
	h.transport.handler = func(request *http.Request) (*http.Response, error) {
		body = &blockingBody{ctx: request.Context()}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}, nil
	}

	started := time.Now()
	_, err := h.gateway.Invoke(context.Background(), invocation())
	elapsed := time.Since(started)
	requireClass(t, err, fault.DeadlineExpired)
	if elapsed > 2*time.Second {
		t.Errorf("timeout took %v; the configured finite timeout was not applied", elapsed)
	}
	if body == nil || !body.isClosed() {
		t.Error("timed-out response body was not closed")
	}
	if h.transport.count() != 1 {
		t.Errorf("upstream calls = %d; want exactly one", h.transport.count())
	}
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("usage settlement context error = %v; want a context that survives the timeout", calls[0].ctxErr)
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["outcome"] != string(gateway.UsageUncertain) || values["usage_known"] != false {
		t.Errorf("usage = %v/%v; want uncertain/unknown", values["outcome"], values["usage_known"])
	}
}

// TestInvokeHonoursCallerCancellation proves caller cancellation stops the
// in-flight call, closes the body and still records attribution.
func TestInvokeHonoursCallerCancellation(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	var body *blockingBody
	h.transport.handler = func(request *http.Request) (*http.Response, error) {
		body = &blockingBody{ctx: request.Context()}
		close(entered)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       body,
		}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.gateway.Invoke(ctx, invocation())
		done <- err
	}()
	<-entered
	cancel()

	err := <-done
	requireClass(t, err, fault.DeadlineExpired)
	if body == nil || !body.isClosed() {
		t.Error("cancelled response body was not closed")
	}
	if h.transport.count() != 1 {
		t.Errorf("upstream calls = %d; want exactly one", h.transport.count())
	}
	calls := h.store.callList()
	if len(calls) != 1 {
		t.Fatalf("usage writes = %d; want 1", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("usage settlement context error = %v; want a context that survives cancellation", calls[0].ctxErr)
	}
	values := singleInsert(t, calls[0].unit).Values
	if values["outcome"] != string(gateway.UsageUncertain) || values["usage_known"] != false {
		t.Errorf("usage = %v/%v; want uncertain/unknown", values["outcome"], values["usage_known"])
	}
}

// TestInvokeRefusesCancelledCallerBeforeDispatch proves an already-cancelled
// attempt has no provider effect and no usage row.
func TestInvokeRefusesCancelledCallerBeforeDispatch(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.gateway.Invoke(ctx, invocation())
	requireClass(t, err, fault.DeadlineExpired)
	if h.transport.count() != 0 {
		t.Errorf("upstream calls = %d; want 0 before dispatch", h.transport.count())
	}
	if h.store.count() != 0 {
		t.Errorf("usage writes = %d; want 0 before dispatch", h.store.count())
	}
}

// TestInvokeRecordsRawContentOnOptIn proves raw content is recorded only when
// the configured model opts in, with the exact bytes and the seven-day default
// retention, and that the record never carries the provider credential.
func TestInvokeRecordsRawContentOnOptIn(t *testing.T) {
	cases := []struct {
		name      string
		retention time.Duration
		want      time.Duration
	}{
		{"default seven-day retention", 0, gateway.DefaultContentRetention},
		{"configured retention", 48 * time.Hour, 48 * time.Hour},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, func(config *gateway.Config) {
				config.Models[0].RecordContent = true
				config.Models[0].ContentRetention = testCase.retention
			})
			_, err := h.gateway.Invoke(context.Background(), invocation())
			requireNoError(t, err)
			records := h.content.recordList()
			if len(records) != 1 {
				t.Fatalf("content records = %d; want 1", len(records))
			}
			record := records[0]
			if record.Scope != testScope() {
				t.Errorf("content scope = %#v; want %#v", record.Scope, testScope())
			}
			if record.PrincipalID != testPrincipal || record.ExecutionID != testExecution {
				t.Errorf("content attribution = %q/%q; want %q/%q",
					record.PrincipalID, record.ExecutionID, testPrincipal, testExecution)
			}
			if record.Model != testModel {
				t.Errorf("content model = %q; want %q", record.Model, testModel)
			}
			if !bytes.Equal(record.Request, h.transport.requestList()[0].body) {
				t.Error("content request bytes do not match the forwarded request")
			}
			if !bytes.Equal(record.Response, []byte(completionJSON())) {
				t.Error("content response bytes do not match the provider response")
			}
			if !record.At.Equal(fixedNow) {
				t.Errorf("content time = %v; want %v", record.At, fixedNow)
			}
			if !record.RetainUntil.Equal(fixedNow.Add(testCase.want)) {
				t.Errorf("content retention = %v; want %v", record.RetainUntil, fixedNow.Add(testCase.want))
			}
			if bytes.Contains(record.Request, []byte(testSecret)) || bytes.Contains(record.Response, []byte(testSecret)) {
				t.Error("content record carries the provider credential")
			}
		})
	}
}

// TestInvokeConcurrentCallsKeepAttribution proves concurrent attempts share no
// per-call state: each execution keeps its own model, secret and usage row.
func TestInvokeConcurrentCallsKeepAttribution(t *testing.T) {
	const workers = 8
	config := gateway.Config{Enabled: true}
	for index := range workers {
		model := validModelConfig()
		model.Model = fmt.Sprintf("model-%d", index)
		model.UpstreamModel = fmt.Sprintf("upstream-%d", index)
		model.SecretName = id.SecretName(fmt.Sprintf("KEY_%d", index))
		config.Models = append(config.Models, model)
	}
	h := newHarness(t, func(target *gateway.Config) { *target = config })
	h.secrets.resolve = func(request secret.ResolveRequest) ([]secret.Resolved, error) {
		return []secret.Resolved{{
			Name:  request.Names[0],
			Value: "secret-" + string(request.Names[0]),
			Scope: request.Scope,
		}}, nil
	}
	h.transport.handler = func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		payload, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		model, _ := body["model"].(string)
		completion := fmt.Sprintf(`{"id":"cmpl-%s","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, model, model)
		return h.transport.reply(http.StatusOK, "application/json", []byte(completion)), nil
	}

	for index := range workers {
		credential := fmt.Sprintf("kodex.exec-%d.credential", index)
		principal := executionPrincipal()
		principal.ID = id.PrincipalID(fmt.Sprintf("principal-%d", index))
		principal.ExecutionID = id.ExecutionID(fmt.Sprintf("exec-%d", index))
		h.principals.set(credential, principal, nil)
		admitted := admittedExecution()
		admitted.Principal = principal
		admitted.ExecutionID = principal.ExecutionID
		h.executions.set(admitted)
	}

	var wait sync.WaitGroup
	failures := make(chan string, workers)
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			request := invocation()
			request.Credential = fmt.Sprintf("kodex.exec-%d.credential", index)
			request.Request.Model = fmt.Sprintf("model-%d", index)
			response, err := h.gateway.Invoke(context.Background(), request)
			if err != nil {
				failures <- fmt.Sprintf("worker %d: %v", index, err)
				return
			}
			if want := fmt.Sprintf("cmpl-upstream-%d", index); response.ID != want {
				failures <- fmt.Sprintf("worker %d: completion id = %q; want %q", index, response.ID, want)
			}
		}(index)
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}

	if h.transport.count() != workers {
		t.Fatalf("upstream calls = %d; want %d", h.transport.count(), workers)
	}
	calls := h.store.callList()
	if len(calls) != workers {
		t.Fatalf("usage writes = %d; want %d", len(calls), workers)
	}
	seen := make(map[string]string, workers)
	for _, call := range calls {
		values := singleInsert(t, call.unit).Values
		execution, _ := values["execution_id"].(string)
		model, _ := values["model"].(string)
		if previous, ok := seen[execution]; ok {
			t.Errorf("execution %q attributed to both %q and %q", execution, previous, model)
		}
		seen[execution] = model
	}
	for index := range workers {
		execution := fmt.Sprintf("exec-%d", index)
		if got, ok := seen[execution]; !ok || got != fmt.Sprintf("model-%d", index) {
			t.Errorf("execution %q model = %q (present=%v); want %q", execution, got, ok, fmt.Sprintf("model-%d", index))
		}
	}

	for _, request := range h.transport.requestList() {
		var body map[string]any
		if err := json.Unmarshal(request.body, &body); err != nil {
			t.Fatalf("forwarded body is not JSON: %v", err)
		}
		model, _ := body["model"].(string)
		var index int
		if _, err := fmt.Sscanf(model, "upstream-%d", &index); err != nil {
			t.Errorf("forwarded model = %q; want an upstream-N model", model)
			continue
		}
		if got := request.header.Get("Authorization"); got != "Bearer secret-KEY_"+fmt.Sprint(index) {
			t.Errorf("model %q Authorization = %q; want its own configured secret", model, got)
		}
	}
}
