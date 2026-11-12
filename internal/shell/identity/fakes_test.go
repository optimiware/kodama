// Deterministic OIDC verifier and current-principal reader fakes.
//
// These fakes are the only identity provider and membership store these tests
// touch: no discovery request, JWKS fetch, HTTP client, database or provider
// SDK is involved. Each fake records its calls so a test can prove the
// authenticator re-reads current state and refuses before reading on a
// malformed request.
package identity_test

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/shell/identity"
)

type fakeVerifier struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	requests  []identity.OIDCRequest
}

func (fake *fakeVerifier) Verify(_ context.Context, request identity.OIDCRequest) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.principal, fake.err
}

type fakePrincipalReader struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	requests  []identity.PrincipalRequest
}

func (fake *fakePrincipalReader) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.principal, fake.err
}
