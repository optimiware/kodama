//go:build integration

// Package actors_test exercises the Git actor-handle boundary end to end:
// the real smart-HTTP handler mints a handle, a fake backend captures the
// backend environment, and the real native hook resolves that handle and
// verifies the forwarded credential through the real public verification client
// against the contract-server fixture.
//
// The actor-verification, executor, observer and journal boundaries are
// labelled fixtures. The contract server is a fixture of Shell facts, not the
// Shell composition; no test here claims full approval proof.
package actors_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	actorapp "github.com/optimiware/kodama/internal/userland/git/app/actors"
	actorfix "github.com/optimiware/kodama/test/support/boundaries/actors"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

const fixturePolicy = `
repo shop/web
    R                               = @all
    RW  refs/heads/main             = @execution
`

func TestHandleRoundTripVerifiesThroughContractFixture(t *testing.T) {
	provider := newOIDC(t)
	contract := newContract(t, provider)
	fixture := newFixture(t, newVerification(t, contract))
	backend := actorfix.NewBackend()
	registry := newRegistry(t)
	handler := newHandler(t, fixture, backend, registry)

	ctx := t.Context()
	recorder := serve(ctx, handler, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", actorfix.ExecutionToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	request, ok := backend.Last()
	if !ok {
		t.Fatal("the backend was never called")
	}
	environment := actorfix.Environment(request.Environment)
	handle := environment[git.EnvActorRef]
	requireMintedHandle(t, handle, actorfix.ExecutionToken)
	assertNoCredential(t, request.Environment, actorfix.ExecutionToken)
	for key, values := range request.Headers {
		for _, value := range values {
			if strings.Contains(value, actorfix.ExecutionToken) {
				t.Fatalf("header %s leaked the credential", key)
			}
		}
	}
	if strings.Contains(recorder.Body.String(), actorfix.ExecutionToken) {
		t.Fatal("the response body leaked the credential")
	}

	// A hook runs while the request is live and resolves the handle through the
	// real registry. The real verification client then verifies the forwarded
	// credential against the contract fixture.
	hook := newHook(t, fixture, registry, environment)
	result, err := hook.RunUpdate(ctx, "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("2", 40))
	if err != nil {
		t.Fatalf("RunUpdate through the real hook: %v", err)
	}
	if !result.Accepted() {
		t.Fatalf("hook result = %+v; want accept", result)
	}
	if got := fixture.Executor.Ref("refs/heads/main"); got != strings.Repeat("2", 40) {
		t.Fatalf("applied ref = %q; want the candidate", got)
	}
	if contract.VerificationCount() != 1 {
		t.Fatalf("verification count = %d; want 1", contract.VerificationCount())
	}
	if contract.EventCount() != 1 {
		t.Fatalf("report event count = %d; want 1", contract.EventCount())
	}
	if strings.Contains(contract.RecordedJSON(), actorfix.ExecutionToken) {
		t.Fatal("the contract fixture recorded the credential")
	}
	if strings.Contains(contract.LogText(), actorfix.ExecutionToken) {
		t.Fatal("the contract fixture log contains the credential")
	}

	// A handle is not single-use within the live request.
	for range 2 {
		credential, err := registry.ResolveActor(ctx, handle)
		if err != nil || credential != actorfix.ExecutionToken {
			t.Fatalf("later resolution = (%q, %v); want the forwarded credential", credential, err)
		}
	}
}

func TestHandleDiesWhenTheRequestContextEnds(t *testing.T) {
	provider := newOIDC(t)
	contract := newContract(t, provider)
	fixture := newFixture(t, newVerification(t, contract))
	backend := actorfix.NewBackend()
	backend.Block()
	registry := newRegistry(t)
	handler := newHandler(t, fixture, backend, registry)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serve(ctx, handler, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", actorfix.ExecutionToken)
	}()

	select {
	case <-backend.Captured():
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never captured the request")
	}
	request, ok := backend.Last()
	if !ok {
		t.Fatal("the backend request was not recorded")
	}
	environment := actorfix.Environment(request.Environment)
	handle := environment[git.EnvActorRef]
	requireMintedHandle(t, handle, actorfix.ExecutionToken)

	cancel()
	recorder := <-done
	if recorder.Code != http.StatusRequestTimeout {
		t.Fatalf("status = %d; want 408 after the request context ended", recorder.Code)
	}

	if _, err := registry.ResolveActor(context.Background(), handle); err == nil {
		t.Fatal("ResolveActor accepted a handle after the issuing request ended")
	} else {
		requireUnauthenticated(t, err)
	}

	// The real hook refuses the same stale handle.
	hook := newHook(t, fixture, registry, environment)
	result, err := hook.RunUpdate(context.Background(), "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("3", 40))
	if err == nil || result.Accepted() || result.ExitCode != 1 {
		t.Fatalf("hook result = (%+v, %v); want a refusal after the request ended", result, err)
	}
	if contract.VerificationCount() != 0 {
		t.Fatalf("verification count = %d; want 0 for a stale handle", contract.VerificationCount())
	}
}

func TestConcurrentPushesGetDistinctHandles(t *testing.T) {
	provider := newOIDC(t)
	contract := newContract(t, provider)
	fixture := newFixture(t, newVerification(t, contract))
	backend := actorfix.NewBackend()
	registry := newRegistry(t)
	handler := newHandler(t, fixture, backend, registry)

	contexts := make([]context.Context, 2)
	cancels := make([]context.CancelFunc, 2)
	recorders := make([]*httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		contexts[index], cancels[index] = context.WithCancel(context.Background())
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			recorders[index] = serve(contexts[index], handler, http.MethodGet, "/shop/web/info/refs?service=git-receive-pack", actorfix.ExecutionToken)
		}(index)
	}
	wait.Wait()
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()

	for index, recorder := range recorders {
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d; want 200", index, recorder.Code)
		}
	}
	requests := backend.Requests()
	if len(requests) != 2 {
		t.Fatalf("backend calls = %d; want 2", len(requests))
	}
	handles := map[string]bool{}
	for _, request := range requests {
		environment := actorfix.Environment(request.Environment)
		handle := environment[git.EnvActorRef]
		requireMintedHandle(t, handle, actorfix.ExecutionToken)
		if handles[handle] {
			t.Fatalf("two concurrent pushes reused handle %q", handle)
		}
		handles[handle] = true
	}
	for handle := range handles {
		credential, err := registry.ResolveActor(context.Background(), handle)
		if err != nil || credential != actorfix.ExecutionToken {
			t.Fatalf("resolution = (%q, %v); want the forwarded credential", credential, err)
		}
	}
}

func TestClientCannotChooseOrInfluenceTheHandle(t *testing.T) {
	provider := newOIDC(t)
	contract := newContract(t, provider)
	fixture := newFixture(t, newVerification(t, contract))
	backend := actorfix.NewBackend()
	registry := newRegistry(t)
	handler := newHandler(t, fixture, backend, registry)

	forged := []string{"kah_forged_by_client", "kah_other_client_handle", actorfix.ExecutionToken}
	ctx := context.Background()
	first := serveWithHeaders(ctx, handler, forged)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d; want 200", first.Code)
	}
	firstRequest, _ := backend.Last()
	firstEnvironment := actorfix.Environment(firstRequest.Environment)
	firstHandle := firstEnvironment[git.EnvActorRef]
	requireMintedHandle(t, firstHandle, actorfix.ExecutionToken)
	for _, candidate := range forged {
		if firstHandle == candidate {
			t.Fatalf("the server used the client-supplied handle %q", candidate)
		}
	}

	// A client that presents a previously issued handle still receives a fresh
	// handle minted for its own request.
	second := serveWithHeaders(ctx, handler, []string{firstHandle})
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d; want 200", second.Code)
	}
	secondRequest, _ := backend.Last()
	secondEnvironment := actorfix.Environment(secondRequest.Environment)
	secondHandle := secondEnvironment[git.EnvActorRef]
	requireMintedHandle(t, secondHandle, actorfix.ExecutionToken)
	if secondHandle == firstHandle {
		t.Fatal("a client-influenced header selected the previously issued handle")
	}

	// A forged handle in the hook environment is refused, and the direct
	// resolver returns the same uniform refusal for every forged shape.
	for _, handle := range forged {
		forgedEnvironment := map[string]string{}
		maps.Copy(forgedEnvironment, secondEnvironment)
		forgedEnvironment[git.EnvActorRef] = handle
		hook := newHook(t, fixture, registry, forgedEnvironment)
		result, err := hook.RunUpdate(ctx, "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("4", 40))
		if err == nil || result.Accepted() || result.ExitCode != 1 {
			t.Fatalf("hook accepted forged handle %q: (%+v, %v)", handle, result, err)
		}
	}

	var wantText string
	for _, handle := range []string{"kah_unknown", "", "not a handle!", actorfix.ExecutionToken} {
		credential, err := registry.ResolveActor(ctx, handle)
		if err == nil {
			t.Fatalf("ResolveActor(%q) returned credential %q", handle, credential)
		}
		requireUnauthenticated(t, err)
		if wantText == "" {
			wantText = err.Error()
		} else if err.Error() != wantText {
			t.Fatalf("refusal %q differs from %q; refusals must be uniform", err.Error(), wantText)
		}
	}
}

func newOIDC(t *testing.T) *idfix.Provider {
	t.Helper()
	provider, err := idfix.NewProvider(idfix.OIDCOptions{Clock: idfix.NewClock(actorfix.DefaultTime())})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(provider.Close)
	return provider
}

func newContract(t *testing.T, provider *idfix.Provider) *idfix.ContractServer {
	t.Helper()
	contract, err := idfix.NewContractServer(idfix.ContractOptions{
		Clock: provider.Clock(),
		OIDC:  provider,
		Services: []idfix.ServiceAccount{{
			ID:        actorfix.ServiceID,
			Token:     actorfix.ServiceToken,
			TenantID:  string(actorfix.TenantID),
			ProjectID: actorfix.ProjectID,
			Enabled:   true,
			Roles:     []string{"engineer"},
		}},
		Executions: []idfix.ExecutionAccount{{
			ID:        actorfix.ExecutionID,
			Token:     actorfix.ExecutionToken,
			TenantID:  string(actorfix.TenantID),
			ProjectID: actorfix.ProjectID,
			Enabled:   true,
		}},
		Bindings: []idfix.Binding{{
			ID:               "binding-actors",
			ServiceID:        actorfix.ServiceID,
			TenantID:         string(actorfix.TenantID),
			ProjectID:        actorfix.ProjectID,
			ResourceID:       actorfix.ResourceID,
			Operations:       []string{string(api.ResourceOperationGitRefUpdate)},
			ReportableEvents: []string{"git.ref.updated"},
		}},
		ActorRoles: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("NewContractServer: %v", err)
	}
	t.Cleanup(contract.Close)
	return contract
}

type staticCredentialSource struct {
	credential client.Credential
}

func (source staticCredentialSource) Credential(context.Context) (client.Credential, error) {
	return source.credential, nil
}

func newVerification(t *testing.T, contract *idfix.ContractServer) *gitapp.VerificationClient {
	t.Helper()
	transport := &client.HTTPTransport{
		BaseURL:      contract.URL(),
		RoundTripper: contract.Client().Transport,
		Source: staticCredentialSource{credential: client.Credential{
			Kind:      client.KindService,
			Token:     actorfix.ServiceToken,
			ExpiresAt: actorfix.DefaultTime().Add(24 * time.Hour),
		}},
		Retry: client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		Sleep: func(time.Duration) {},
		Now:   func() time.Time { return actorfix.DefaultTime() },
	}
	verification, err := gitapp.NewVerificationClient(transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	return verification
}

func newFixture(t *testing.T, verification git.VerificationClient) *actorfix.Fixture {
	t.Helper()
	fixture, err := actorfix.NewFixture(actorfix.ServiceConfig{
		TenantID: actorfix.TenantID,
		Policy:   fixturePolicy,
		Verify:   verification,
		Clock:    actorfix.NewClock(actorfix.DefaultTime()),
	})
	if err != nil {
		t.Fatalf("NewFixture: %v", err)
	}
	fixture.Verifier.Allow(actorfix.ExecutionToken, git.Identity{Execution: true})
	return fixture
}

func newRegistry(t *testing.T) *actorapp.Registry {
	t.Helper()
	registry, err := actorapp.NewRegistry(actorapp.Config{
		TenantID:   actorfix.TenantID,
		Clock:      actorfix.NewClock(actorfix.DefaultTime()),
		MaxHandles: 32,
		TTL:        5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func newHandler(t *testing.T, fixture *actorfix.Fixture, backend git.GitBackend, handles git.ActorHandleIssuer) *git.SmartHTTP {
	t.Helper()
	handler, err := fixture.Handler(backend, handles, git.NewSourceEventID(), map[string]string{
		actorfix.Repository: actorfix.ResourceID,
	})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	return handler
}

func newHook(t *testing.T, fixture *actorfix.Fixture, source git.ActorSource, environment map[string]string) *git.Hook {
	t.Helper()
	hook, err := fixture.Hook(source, func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	})
	if err != nil {
		t.Fatalf("Hook: %v", err)
	}
	return hook
}

func serve(ctx context.Context, handler http.Handler, method, target, credential string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil).WithContext(ctx)
	request.SetBasicAuth("kodama", credential)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func serveWithHeaders(ctx context.Context, handler http.Handler, handles []string) *httptest.ResponseRecorder {
	target := "/shop/web/info/refs?service=git-receive-pack"
	if len(handles) > 0 {
		target += "&actor_ref=" + handles[0]
	}
	request := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	request.SetBasicAuth("kodama", actorfix.ExecutionToken)
	for index, handle := range handles {
		switch index {
		case 0:
			request.Header.Set("KODAMA_ACTOR_REF", handle)
		case 1:
			request.Header.Set("X-Kodama-Actor-Ref", handle)
		default:
			request.Header.Set("X-Actor-Handle", handle)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requireMintedHandle(t *testing.T, handle, credential string) {
	t.Helper()
	if handle == "" {
		t.Fatal("the backend environment carried no actor handle")
	}
	if len(handle) > 256 {
		t.Fatalf("handle length = %d; want at most 256", len(handle))
	}
	for index := 0; index < len(handle); index++ {
		character := handle[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			t.Fatalf("handle %q contains %q outside the validActorHandle alphabet", handle, character)
		}
	}
	if strings.Contains(handle, credential) {
		t.Fatal("the handle contains the credential")
	}
	if handle == credential {
		t.Fatal("the credential was used as its own handle")
	}
}

func assertNoCredential(t *testing.T, values []string, credential string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, credential) {
			t.Fatalf("backend environment value %q leaked the credential", value)
		}
	}
}

func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want an unauthenticated refusal")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.Unauthenticated {
		t.Fatalf("fault class = %v (ok=%v); want unauthenticated (err=%v)", class, ok, err)
	}
	if !errors.Is(err, actorapp.ErrUnauthenticated) {
		t.Fatalf("error %v does not wrap ErrUnauthenticated", err)
	}
}
