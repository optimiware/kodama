// Deterministic fakes and shared fixtures for the token tests.
//
// Every dependency is an in-memory fake with a recorded call log: there is no
// real clock, randomness source, database, KMS, broker or runtime process.
// The fakes make commit-before-delivery, lost responses, current-state re-reads
// and concurrent renewal observable and deterministic.
package token_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

type fixedReader struct {
	mu    sync.Mutex
	fill  byte
	err   error
	reads int
}

func (reader *fixedReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.err != nil {
		return 0, reader.err
	}
	reader.reads++
	for index := range buffer {
		buffer[index] = reader.fill + byte(reader.reads)
	}
	return len(buffer), nil
}

type fakeTenantKeys struct {
	mu       sync.Mutex
	key      token.TenantKey
	err      error
	calls    int
	requests []token.TenantKeyRequest
}

func (fake *fakeTenantKeys) ReadTenantKey(_ context.Context, request token.TenantKeyRequest) (token.TenantKey, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.key, fake.err
}

type fakeResolver struct {
	mu   sync.Mutex
	key  []byte
	err  error
	keys []token.TenantKey
}

func (fake *fakeResolver) ResolveDerivationKey(_ context.Context, key token.TenantKey) ([]byte, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.keys = append(fake.keys, key)
	if fake.err != nil {
		return nil, fake.err
	}
	return append([]byte(nil), fake.key...), nil
}

type fakeCredentials struct {
	mu       sync.Mutex
	fact     token.CredentialFact
	err      error
	calls    int
	requests []token.CredentialRequest
}

func (fake *fakeCredentials) ReadCredential(_ context.Context, request token.CredentialRequest) (token.CredentialFact, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.fact, fake.err
}

type fakePrincipals struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	requests  []identity.PrincipalRequest
}

func (fake *fakePrincipals) ReadPrincipal(_ context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.principal, fake.err
}

type fakeServices struct {
	mu       sync.Mutex
	service  service.Service
	err      error
	calls    int
	requests []service.ServiceReadRequest
}

func (fake *fakeServices) ReadService(_ context.Context, request service.ServiceReadRequest) (service.Service, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	return fake.service, fake.err
}

type fakeMutations struct {
	mu     sync.Mutex
	result token.MutationResult
	err    error
	units  []token.MutationUnit
}

func (fake *fakeMutations) Mutate(_ context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.units = append(fake.units, unit)
	if fake.err != nil {
		return token.MutationResult{}, fake.err
	}
	result := fake.result
	if unit.Kind == token.MutationRevoke {
		result.State = token.StateAbsent
	}
	return result, nil
}

type fakeDeliverer struct {
	mu         sync.Mutex
	err        error
	deliveries []token.Delivery
}

func (fake *fakeDeliverer) Deliver(_ context.Context, delivery token.Delivery) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.deliveries = append(fake.deliveries, delivery)
	return fake.err
}

type fakeInvalidator struct {
	mu            sync.Mutex
	err           error
	invalidations []token.Invalidation
}

func (fake *fakeInvalidator) Invalidate(_ context.Context, invalidation token.Invalidation) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.invalidations = append(fake.invalidations, invalidation)
	return fake.err
}

// fakeReceipts is an empty receipt seam. Tests that need a replay wire a
// stateful store instead; every other test sees "no committed receipt" and
// exercises the ordinary path.
type fakeReceipts struct {
	mu      sync.Mutex
	records map[receipt.Key]receipt.Record
	lookups []receipt.Key
	err     error
}

func (fake *fakeReceipts) Lookup(_ context.Context, key receipt.Key) (receipt.Record, bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.lookups = append(fake.lookups, key)
	if fake.err != nil {
		return receipt.Record{}, false, fake.err
	}
	record, found := fake.records[key]
	return record, found, nil
}

type testEnv struct {
	now         time.Time
	scope       tenancy.Scope
	tenantID    id.TenantID
	executionID id.ExecutionID
	serviceID   service.ServiceID
	principalID id.PrincipalID
	keyBytes    []byte

	clock       *fakeClock
	random      *fixedReader
	keys        *fakeTenantKeys
	resolver    *fakeResolver
	creds       *fakeCredentials
	principals  *fakePrincipals
	services    *fakeServices
	mutations   *fakeMutations
	receipts    *fakeReceipts
	deliverer   *fakeDeliverer
	invalidator *fakeInvalidator
	manager     *token.Manager
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tenantID, executionID, serviceID := testScope()
	scope := tenancy.Scope{TenantID: tenantID, ProjectID: id.ProjectID("project-1")}
	now := time.Unix(1_700_000_000, 0).UTC()
	principalID := id.PrincipalID("principal-1")
	keyBytes := testKey()

	env := &testEnv{
		now:         now,
		scope:       scope,
		tenantID:    tenantID,
		executionID: executionID,
		serviceID:   serviceID,
		principalID: principalID,
		keyBytes:    keyBytes,
		clock:       &fakeClock{now: now},
		random:      &fixedReader{fill: 0x42},
		keys: &fakeTenantKeys{key: token.TenantKey{
			TenantID:   tenantID,
			Version:    1,
			Ciphertext: []byte("encrypted-derivation-key"),
			WrappedKey: []byte("wrapped-data-key"),
			WrapNonce:  []byte("wrap-nonce-1"),
			KeyRef:     "kms/tenant-a",
			KeyVersion: 1,
		}},
		resolver: &fakeResolver{key: keyBytes},
		creds: &fakeCredentials{fact: token.CredentialFact{
			Scope:       scope,
			PrincipalID: principalID,
			ExecutionID: executionID,
			Enabled:     true,
			Version:     3,
			Credential: token.Credential{
				Nonce:  testNonce(),
				Expiry: now.Add(time.Hour),
			},
		}},
		principals: &fakePrincipals{principal: identity.Principal{
			ID:          principalID,
			Kind:        identity.KindExecution,
			Scope:       scope,
			Enabled:     true,
			Version:     3,
			ExecutionID: executionID,
		}},
		services: &fakeServices{service: service.Service{
			ID:      serviceID,
			Scope:   scope,
			State:   service.StateEnabled,
			Roles:   []tenancy.Role{tenancy.RoleEngineer},
			Version: 3,
		}},
		mutations:   &fakeMutations{result: token.MutationResult{State: token.StateCurrent, Version: 4}},
		receipts:    &fakeReceipts{records: map[receipt.Key]receipt.Record{}},
		deliverer:   &fakeDeliverer{},
		invalidator: &fakeInvalidator{},
	}
	env.manager = newManager(t, env, env.mutations)
	return env
}

func (env *testEnv) setAbsentCredential() {
	env.creds.fact = token.CredentialFact{
		Scope:       env.scope,
		PrincipalID: env.principalID,
		ExecutionID: env.executionID,
		Enabled:     true,
		Version:     3,
		Credential:  token.Credential{},
	}
}

func (env *testEnv) setServiceCredentialAbsent() {
	env.creds.fact = token.CredentialFact{
		Scope:       env.scope,
		PrincipalID: env.principalID,
		ServiceID:   env.serviceID,
		Enabled:     true,
		Version:     3,
		Credential:  token.Credential{},
	}
}
