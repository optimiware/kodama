// Authentication bootstrap tests.
//
// A presented derived credential carries only a kind and an identity hint. The
// resolver asks the pre-authentication lookup for a CANDIDATE scope, then the
// existing Manager verifies the MAC, the current nonce/expiry and the current
// principal/service state inside that candidate scope. The candidate is never
// authority, and every authentication failure is one uniform refusal so the
// boundary is not a token-identity oracle. Human bearer tokens use the existing
// OIDC verifier plus the current-principal reader.
//
// Every dependency is a deterministic in-memory fake; no HTTP, database, KMS,
// runtime or real OIDC provider is involved.
package token_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// callLog records the order of lookup and scoped-read calls so a test can prove
// no scoped read happens before the candidate lookup.
type callLog struct {
	mu      sync.Mutex
	entries []string
}

func (log *callLog) add(entry string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.entries = append(log.entries, entry)
}

func (log *callLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.entries...)
}

type lookupKey struct {
	kind store.CredentialKind
	id   string
}

type fakeLookup struct {
	mu       sync.Mutex
	entries  map[lookupKey]store.CandidateScope
	err      error
	calls    int
	requests []lookupKey
	log      *callLog
}

func (fake *fakeLookup) LookupCredential(_ context.Context, kind store.CredentialKind, credentialID string) (store.CandidateScope, error) {
	fake.mu.Lock()
	fake.calls++
	fake.requests = append(fake.requests, lookupKey{kind: kind, id: credentialID})
	err := fake.err
	candidate, found := fake.entries[lookupKey{kind: kind, id: credentialID}]
	fake.mu.Unlock()
	if fake.log != nil {
		fake.log.add("lookup")
	}
	if err != nil {
		return store.CandidateScope{}, err
	}
	if !found {
		return store.CandidateScope{}, fault.New(fault.Unauthenticated, "credential lookup miss")
	}
	return candidate, nil
}

type fakeOIDCVerifier struct {
	mu        sync.Mutex
	principal identity.Principal
	err       error
	calls     int
	requests  []identity.OIDCRequest
	log       *callLog
}

func (fake *fakeOIDCVerifier) Verify(_ context.Context, request identity.OIDCRequest) (identity.Principal, error) {
	fake.mu.Lock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	principal, err := fake.principal, fake.err
	fake.mu.Unlock()
	if fake.log != nil {
		fake.log.add("oidc")
	}
	return principal, err
}

type fakeCredentialAuthenticator struct {
	mu       sync.Mutex
	result   token.Authenticated
	err      error
	calls    int
	requests []token.AuthenticateRequest
	log      *callLog
}

func (fake *fakeCredentialAuthenticator) Authenticate(_ context.Context, request token.AuthenticateRequest) (token.Authenticated, error) {
	fake.mu.Lock()
	fake.calls++
	fake.requests = append(fake.requests, request)
	result, err := fake.result, fake.err
	fake.mu.Unlock()
	if fake.log != nil {
		fake.log.add("authenticate")
	}
	return result, err
}

// resolverEnv binds the real Manager and deterministic readers to a fake
// pre-authentication lookup.
type resolverEnv struct {
	*testEnv
	lookup   *fakeLookup
	oidc     *fakeOIDCVerifier
	log      *callLog
	resolver *token.Resolver
}

func newResolverEnv(t *testing.T) *resolverEnv {
	t.Helper()
	env := newTestEnv(t)
	log := &callLog{}
	lookup := &fakeLookup{
		entries: map[lookupKey]store.CandidateScope{
			{kind: store.CredentialExecution, id: string(env.executionID)}: {
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: env.principalID,
			},
			{kind: store.CredentialService, id: string(env.serviceID)}: {
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: id.PrincipalID(env.serviceID),
			},
		},
		log: log,
	}
	oidc := &fakeOIDCVerifier{log: log}
	resolver, err := token.NewResolver(token.ResolverConfig{
		Lookup:      lookup,
		Credentials: env.manager,
		OIDC:        oidc,
		Principals:  env.principals,
		Clock:       env.clock,
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return &resolverEnv{testEnv: env, lookup: lookup, oidc: oidc, log: log, resolver: resolver}
}

func mustResolver(t *testing.T, config token.ResolverConfig) *token.Resolver {
	t.Helper()
	resolver, err := token.NewResolver(config)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return resolver
}

func deriveExecution(t *testing.T, key []byte, tenantID id.TenantID, executionID id.ExecutionID, nonce []byte, expiry time.Time) string {
	t.Helper()
	presented, err := token.DeriveExecutionToken(key, token.ExecutionClaims{
		TenantID:    tenantID,
		ExecutionID: executionID,
		Nonce:       nonce,
		Expiry:      expiry,
	})
	if err != nil {
		t.Fatalf("derive execution token: %v", err)
	}
	return presented
}

func macSegment(presented string) string {
	index := strings.LastIndexByte(presented, '.')
	if index < 0 {
		return presented
	}
	return presented[index+1:]
}

func tamperMAC(presented string) string {
	last := presented[len(presented)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	return presented[:len(presented)-1] + string(replacement)
}

func humanIdentity(scope tenancy.Scope, version uint64, roles ...tenancy.Role) identity.Principal {
	return identity.Principal{
		ID:      id.PrincipalID("human-1"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   roles,
		Enabled: true,
		Version: version,
	}
}

// TestResolverExecutionTokenUsesCandidateScopeAndCurrentState proves a valid
// execution token authenticates only after the candidate lookup and the current
// credential/principal re-read, returning current state rather than token
// claims.
func TestResolverExecutionTokenUsesCandidateScopeAndCurrentState(t *testing.T) {
	env := newResolverEnv(t)
	env.principals.principal.Version = 9
	presented := executionToken(t, env.testEnv)

	principal, err := env.resolver.Resolve(context.Background(), presented)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.Kind != identity.KindExecution {
		t.Fatalf("Kind = %q; want execution", principal.Kind)
	}
	if principal.ID != env.principalID || principal.Scope != env.scope || principal.ExecutionID != env.executionID {
		t.Fatalf("principal = %+v; want current execution identity/scope", principal)
	}
	if principal.Version != 9 {
		t.Fatalf("Version = %d; want current version 9", principal.Version)
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("execution roles = %v; want empty", principal.Roles)
	}
	if !principal.Enabled {
		t.Fatal("principal is disabled; want enabled current principal")
	}
	if env.lookup.calls != 1 {
		t.Fatalf("lookup calls = %d; want 1", env.lookup.calls)
	}
	if got := env.lookup.requests[0]; got.kind != store.CredentialExecution || got.id != string(env.executionID) {
		t.Fatalf("lookup request = %+v; want execution/%s", got, env.executionID)
	}
	if env.creds.calls != 1 || env.keys.calls != 1 || len(env.testEnv.resolver.keys) != 1 || env.principals.calls != 1 {
		t.Fatalf("scoped reads = creds:%d keys:%d resolver:%d principals:%d; want 1 each",
			env.creds.calls, env.keys.calls, len(env.testEnv.resolver.keys), env.principals.calls)
	}
	if env.creds.requests[0].Scope != env.scope || env.creds.requests[0].ExecutionID != env.executionID {
		t.Fatalf("credential read = %+v; want candidate scope and execution", env.creds.requests[0])
	}
}

// TestResolverServiceTokenUsesCandidateScopeAndCurrentRoles proves a valid
// service token returns the current scoped roles, never the Admin role.
func TestResolverServiceTokenUsesCandidateScopeAndCurrentRoles(t *testing.T) {
	env := newResolverEnv(t)
	env.creds.fact = token.CredentialFact{
		Scope:       env.scope,
		PrincipalID: id.PrincipalID(env.serviceID),
		ServiceID:   env.serviceID,
		Enabled:     true,
		Version:     3,
		Credential:  token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)},
	}
	env.services.service.State = service.StateEnabled
	env.services.service.Roles = []tenancy.Role{tenancy.RoleViewer, tenancy.RoleOperator}
	env.services.service.Version = 6
	presented := serviceToken(t, env.testEnv)

	principal, err := env.resolver.Resolve(context.Background(), presented)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.Kind != identity.KindService {
		t.Fatalf("Kind = %q; want service", principal.Kind)
	}
	if principal.ID != id.PrincipalID(env.serviceID) || principal.Scope != env.scope || principal.ExecutionID != "" {
		t.Fatalf("principal = %+v; want service identity/scope", principal)
	}
	if principal.Version != 6 {
		t.Fatalf("Version = %d; want current service version 6", principal.Version)
	}
	if !reflect.DeepEqual(principal.Roles, []tenancy.Role{tenancy.RoleViewer, tenancy.RoleOperator}) {
		t.Fatalf("roles = %v; want current service roles", principal.Roles)
	}
	if got := env.lookup.requests[0]; got.kind != store.CredentialService || got.id != string(env.serviceID) {
		t.Fatalf("lookup request = %+v; want service/%s", got, env.serviceID)
	}
}

// TestResolverHumanTokenUsesVerifierAndCurrentReader proves an opaque human
// bearer token goes through the OIDC verifier and then the current-principal
// reader, with no credential lookup or derived-credential read.
func TestResolverHumanTokenUsesVerifierAndCurrentReader(t *testing.T) {
	env := newResolverEnv(t)
	env.oidc.principal = humanIdentity(env.scope, 4, tenancy.RoleEngineer)
	env.principals.principal = humanIdentity(env.scope, 8, tenancy.RoleViewer)

	principal, err := env.resolver.Resolve(context.Background(), "opaque-human-bearer-token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.Kind != identity.KindHuman || principal.ID != id.PrincipalID("human-1") || principal.Scope != env.scope {
		t.Fatalf("principal = %+v; want current human identity", principal)
	}
	if principal.Version != 8 || !reflect.DeepEqual(principal.Roles, []tenancy.Role{tenancy.RoleViewer}) {
		t.Fatalf("principal = %+v; want current roles/version, not token claims", principal)
	}
	if env.lookup.calls != 0 || env.creds.calls != 0 || env.keys.calls != 0 {
		t.Fatalf("derived reads = lookup:%d creds:%d keys:%d; want none for a human token",
			env.lookup.calls, env.creds.calls, env.keys.calls)
	}
	if env.oidc.calls != 1 || env.principals.calls != 1 {
		t.Fatalf("verifier/current calls = %d/%d; want 1/1", env.oidc.calls, env.principals.calls)
	}
	request := env.oidc.requests[0]
	if request.AccessToken != "opaque-human-bearer-token" || !request.Now.Equal(env.now) {
		t.Fatalf("OIDC request = %+v; want presented token and authoritative time", request)
	}
	if got := env.principals.requests[0]; got.Scope != env.scope || got.PrincipalID != id.PrincipalID("human-1") {
		t.Fatalf("principal request = %+v; want verified current identity", got)
	}
}

// TestResolverLooksUpBeforeAnyScopedRead proves the candidate lookup is the
// first dependency call and that the Manager receives exactly the candidate
// scope.
func TestResolverLooksUpBeforeAnyScopedRead(t *testing.T) {
	env := newTestEnv(t)
	log := &callLog{}
	lookup := &fakeLookup{
		entries: map[lookupKey]store.CandidateScope{
			{kind: store.CredentialExecution, id: string(env.executionID)}: {
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: env.principalID,
			},
		},
		log: log,
	}
	authenticator := &fakeCredentialAuthenticator{
		result: token.Authenticated{
			PrincipalID: env.principalID,
			Kind:        identity.KindExecution,
			Scope:       env.scope,
			Enabled:     true,
			Version:     3,
			ExecutionID: env.executionID,
		},
		log: log,
	}
	resolver := mustResolver(t, token.ResolverConfig{
		Lookup:      lookup,
		Credentials: authenticator,
		OIDC:        &fakeOIDCVerifier{log: log},
		Principals:  env.principals,
		Clock:       env.clock,
	})
	presented := executionToken(t, env)

	if _, err := resolver.Resolve(context.Background(), presented); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := log.snapshot(); !reflect.DeepEqual(got, []string{"lookup", "authenticate"}) {
		t.Fatalf("call order = %v; want lookup before the scoped authentication read", got)
	}
	request := authenticator.requests[0]
	if request.Scope != env.scope || request.PrincipalID != env.principalID ||
		request.ExecutionID != env.executionID || request.Presented != presented || !request.Now.Equal(env.now) {
		t.Fatalf("authenticate request = %+v; want candidate scope and parsed identity", request)
	}
}

// TestResolverUnknownIDIssuesNoScopedRead proves a lookup miss refuses before
// any scoped credential/principal read.
func TestResolverUnknownIDIssuesNoScopedRead(t *testing.T) {
	env := newTestEnv(t)
	log := &callLog{}
	lookup := &fakeLookup{entries: map[lookupKey]store.CandidateScope{}, log: log}
	authenticator := &fakeCredentialAuthenticator{log: log}
	resolver := mustResolver(t, token.ResolverConfig{
		Lookup:      lookup,
		Credentials: authenticator,
		OIDC:        &fakeOIDCVerifier{log: log},
		Principals:  env.principals,
		Clock:       env.clock,
	})
	presented := executionToken(t, env)

	_, err := resolver.Resolve(context.Background(), presented)
	requireClass(t, err, fault.Unauthenticated)
	if authenticator.calls != 0 {
		t.Fatalf("scoped authenticator calls = %d; want 0 after a lookup miss", authenticator.calls)
	}
	if got := log.snapshot(); !reflect.DeepEqual(got, []string{"lookup"}) {
		t.Fatalf("call order = %v; want only the lookup", got)
	}
}

// TestResolverAllAuthenticationFailuresShareOneRefusal proves malformed,
// unknown, wrong-kind, bad-MAC, expired, revoked, disabled, rotated-nonce,
// cross-tenant and foreign-principal presentations are indistinguishable.
func TestResolverAllAuthenticationFailuresShareOneRefusal(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, env *resolverEnv) string
	}{
		{"malformed token", func(t *testing.T, env *resolverEnv) string {
			return token.ExecutionPrefix + "not-a-token"
		}},
		{"empty token", func(t *testing.T, env *resolverEnv) string {
			return ""
		}},
		{"unknown execution id", func(t *testing.T, env *resolverEnv) string {
			return deriveExecution(t, env.keyBytes, env.tenantID, id.ExecutionID("exec-unknown"), testNonce(), env.now.Add(time.Hour))
		}},
		{"wrong kind", func(t *testing.T, env *resolverEnv) string {
			// A token derived in the execution domain presented under the
			// service prefix; the lookup finds a service candidate but the
			// service MAC input cannot match the execution-domain MAC.
			env.creds.fact = token.CredentialFact{
				Scope:       env.scope,
				PrincipalID: id.PrincipalID(env.serviceID),
				ServiceID:   service.ServiceID(env.executionID),
				Enabled:     true,
				Version:     3,
				Credential:  token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)},
			}
			env.services.service = service.Service{
				ID:      service.ServiceID(env.executionID),
				Scope:   env.scope,
				State:   service.StateEnabled,
				Roles:   []tenancy.Role{tenancy.RoleViewer},
				Version: 3,
			}
			env.lookup.entries[lookupKey{kind: store.CredentialService, id: string(env.executionID)}] = store.CandidateScope{
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: id.PrincipalID(env.serviceID),
			}
			execution := executionToken(t, env.testEnv)
			return token.ServicePrefix + string(env.executionID) + "." + macSegment(execution)
		}},
		{"bad mac", func(t *testing.T, env *resolverEnv) string {
			return tamperMAC(executionToken(t, env.testEnv))
		}},
		{"expired credential", func(t *testing.T, env *resolverEnv) string {
			expiry := env.now.Add(-time.Minute)
			env.creds.fact.Credential.Expiry = expiry
			return deriveExecution(t, env.keyBytes, env.tenantID, env.executionID, env.creds.fact.Credential.Nonce, expiry)
		}},
		{"revoked credential", func(t *testing.T, env *resolverEnv) string {
			env.setAbsentCredential()
			return deriveExecution(t, env.keyBytes, env.tenantID, env.executionID, testNonce(), env.now.Add(time.Hour))
		}},
		{"disabled service", func(t *testing.T, env *resolverEnv) string {
			env.creds.fact = token.CredentialFact{
				Scope:       env.scope,
				PrincipalID: id.PrincipalID(env.serviceID),
				ServiceID:   env.serviceID,
				Enabled:     true,
				Version:     3,
				Credential:  token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)},
			}
			env.services.service.State = service.StateDisabled
			return serviceToken(t, env.testEnv)
		}},
		{"rotated nonce", func(t *testing.T, env *resolverEnv) string {
			presented := deriveExecution(t, env.keyBytes, env.tenantID, env.executionID, testNonce(), env.now.Add(time.Hour))
			env.creds.fact.Credential = token.Credential{
				Nonce:  bytes.Repeat([]byte{0x11}, token.MinNonceBytes),
				Expiry: env.now.Add(time.Hour),
			}
			return presented
		}},
		{"cross tenant candidate", func(t *testing.T, env *resolverEnv) string {
			return deriveExecution(t, env.keyBytes, id.TenantID("tenant-b"), env.executionID,
				env.creds.fact.Credential.Nonce, env.creds.fact.Credential.Expiry)
		}},
		{"foreign principal candidate", func(t *testing.T, env *resolverEnv) string {
			env.lookup.entries[lookupKey{kind: store.CredentialExecution, id: string(env.executionID)}] = store.CandidateScope{
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: id.PrincipalID("principal-2"),
			}
			return executionToken(t, env.testEnv)
		}},
	}

	messages := make([]string, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newResolverEnv(t)
			presented := testCase.setup(t, env)

			principal, err := env.resolver.Resolve(context.Background(), presented)
			if err == nil {
				t.Fatalf("Resolve succeeded with principal %+v; want one uniform refusal", principal)
			}
			requireClass(t, err, fault.Unauthenticated)
			if principal.ID != "" || principal.Kind != "" || principal.Scope != (tenancy.Scope{}) {
				t.Fatalf("principal = %+v; want zero value on refusal", principal)
			}
			if presented != "" && strings.Contains(err.Error(), presented) {
				t.Fatalf("error %q leaks the presented token", err)
			}
			messages = append(messages, err.Error())
		})
	}
	if len(messages) != len(cases) {
		t.Fatalf("collected %d refusal messages for %d cases", len(messages), len(cases))
	}
	for index := 1; index < len(messages); index++ {
		if messages[index] != messages[0] {
			t.Fatalf("refusal message %q differs from %q; authentication failures must be indistinguishable",
				messages[index], messages[0])
		}
	}
}

// TestResolverLookupErrorsFailClosed proves lookup unavailability is a distinct
// fail-closed refusal and a lookup miss is the same authentication refusal as a
// bad MAC.
func TestResolverLookupErrorsFailClosed(t *testing.T) {
	t.Run("dependency error", func(t *testing.T) {
		env := newTestEnv(t)
		resolver := mustResolver(t, token.ResolverConfig{
			Lookup:      &fakeLookup{entries: map[lookupKey]store.CandidateScope{}, err: errors.New("lookup backend down")},
			Credentials: &fakeCredentialAuthenticator{},
			OIDC:        &fakeOIDCVerifier{},
			Principals:  env.principals,
			Clock:       env.clock,
		})
		presented := executionToken(t, env)
		_, err := resolver.Resolve(context.Background(), presented)
		requireClass(t, err, fault.DependencyUnavailable)
		if strings.Contains(err.Error(), presented) {
			t.Fatalf("error %q leaks the presented token", err)
		}
	})

	t.Run("authentication miss", func(t *testing.T) {
		env := newTestEnv(t)
		resolver := mustResolver(t, token.ResolverConfig{
			Lookup:      &fakeLookup{entries: map[lookupKey]store.CandidateScope{}},
			Credentials: &fakeCredentialAuthenticator{},
			OIDC:        &fakeOIDCVerifier{},
			Principals:  env.principals,
			Clock:       env.clock,
		})
		presented := executionToken(t, env)
		_, err := resolver.Resolve(context.Background(), presented)
		requireClass(t, err, fault.Unauthenticated)
		if strings.Contains(err.Error(), presented) {
			t.Fatalf("error %q leaks the presented token", err)
		}
	})

	t.Run("invalid candidate", func(t *testing.T) {
		env := newTestEnv(t)
		resolver := mustResolver(t, token.ResolverConfig{
			Lookup: &fakeLookup{entries: map[lookupKey]store.CandidateScope{
				{kind: store.CredentialExecution, id: string(env.executionID)}: {},
			}},
			Credentials: &fakeCredentialAuthenticator{},
			OIDC:        &fakeOIDCVerifier{},
			Principals:  env.principals,
			Clock:       env.clock,
		})
		presented := executionToken(t, env)
		_, err := resolver.Resolve(context.Background(), presented)
		requireClass(t, err, fault.DependencyUnavailable)
	})
}

// TestResolverAuthenticatorErrorsFailClosed proves a Manager dependency failure
// is unavailable while a Manager authentication refusal stays uniform.
func TestResolverAuthenticatorErrorsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want fault.Class
	}{
		{"dependency unavailable", fault.New(fault.DependencyUnavailable, "key store down"), fault.DependencyUnavailable},
		{"internal failure", fault.New(fault.InternalFailure, "bug"), fault.DependencyUnavailable},
		{"authentication refusal", fault.New(fault.Unauthenticated, "private detail"), fault.Unauthenticated},
		{"forbidden", fault.New(fault.Forbidden, "private detail"), fault.Unauthenticated},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			lookup := &fakeLookup{entries: map[lookupKey]store.CandidateScope{
				{kind: store.CredentialExecution, id: string(env.executionID)}: {
					TenantID:    env.tenantID,
					ProjectID:   env.scope.ProjectID,
					PrincipalID: env.principalID,
				},
			}}
			resolver := mustResolver(t, token.ResolverConfig{
				Lookup:      lookup,
				Credentials: &fakeCredentialAuthenticator{err: testCase.err},
				OIDC:        &fakeOIDCVerifier{},
				Principals:  env.principals,
				Clock:       env.clock,
			})
			presented := executionToken(t, env)
			_, err := resolver.Resolve(context.Background(), presented)
			requireClass(t, err, testCase.want)
			if strings.Contains(err.Error(), presented) {
				t.Fatalf("error %q leaks the presented token", err)
			}
			if testCase.want == fault.Unauthenticated && strings.Contains(err.Error(), "private detail") {
				t.Fatalf("error %q leaks an internal refusal cause", err)
			}
		})
	}
}

// TestResolverStoreOutageIsUnavailable proves the production authentication
// boundary turns a real Manager store outage into the retryable dependency
// class the HTTP and Git edges render as 503, while a genuinely absent row stays
// the uniform 401 refusal. It drives the real Manager over failing current-state
// readers, not a substituted error class.
func TestResolverStoreOutageIsUnavailable(t *testing.T) {
	t.Run("outage", func(t *testing.T) {
		env := newResolverEnv(t)
		presented := executionToken(t, env.testEnv)
		env.creds.err = errors.New("postgres connection refused")
		_, err := env.resolver.Resolve(context.Background(), presented)
		requireClass(t, err, fault.DependencyUnavailable)
		if strings.Contains(err.Error(), presented) {
			t.Fatalf("error %q leaks the presented token", err)
		}
		if strings.Contains(err.Error(), "postgres connection refused") {
			t.Fatalf("error %q leaks the private store cause", err)
		}
	})

	t.Run("missing row", func(t *testing.T) {
		env := newResolverEnv(t)
		presented := executionToken(t, env.testEnv)
		env.creds.err = fault.New(fault.NotFound, "credential row is missing")
		_, err := env.resolver.Resolve(context.Background(), presented)
		requireClass(t, err, fault.Unauthenticated)
	})
}

// TestResolverRejectsUnconfirmedCandidateScope proves the resolver uses the
// candidate tenant for MAC verification and refuses when the candidate scope or
// principal does not match the authenticated identity.
func TestResolverRejectsUnconfirmedCandidateScope(t *testing.T) {
	t.Run("mac in candidate tenant", func(t *testing.T) {
		env := newResolverEnv(t)
		// A valid tenant-a token, but the lookup claims the execution lives in
		// tenant-b. The MAC is checked against tenant-b, so it cannot match.
		env.lookup.entries[lookupKey{kind: store.CredentialExecution, id: string(env.executionID)}] = store.CandidateScope{
			TenantID:    id.TenantID("tenant-b"),
			ProjectID:   env.scope.ProjectID,
			PrincipalID: env.principalID,
		}
		presented := executionToken(t, env.testEnv)
		principal, err := env.resolver.Resolve(context.Background(), presented)
		if err == nil {
			t.Fatalf("Resolve succeeded with %+v; want refusal", principal)
		}
		requireClass(t, err, fault.Unauthenticated)
		if env.principals.calls != 0 {
			t.Fatalf("current principal reads = %d; want 0 after MAC failure", env.principals.calls)
		}
	})

	t.Run("authenticated scope outside candidate", func(t *testing.T) {
		env := newTestEnv(t)
		lookup := &fakeLookup{entries: map[lookupKey]store.CandidateScope{
			{kind: store.CredentialExecution, id: string(env.executionID)}: {
				TenantID:    id.TenantID("tenant-b"),
				ProjectID:   env.scope.ProjectID,
				PrincipalID: env.principalID,
			},
		}}
		authenticator := &fakeCredentialAuthenticator{result: token.Authenticated{
			PrincipalID: env.principalID,
			Kind:        identity.KindExecution,
			Scope:       env.scope,
			Enabled:     true,
			Version:     3,
			ExecutionID: env.executionID,
		}}
		resolver := mustResolver(t, token.ResolverConfig{
			Lookup:      lookup,
			Credentials: authenticator,
			OIDC:        &fakeOIDCVerifier{},
			Principals:  env.principals,
			Clock:       env.clock,
		})
		presented := executionToken(t, env)
		principal, err := resolver.Resolve(context.Background(), presented)
		if err == nil {
			t.Fatalf("Resolve succeeded with %+v; want refusal", principal)
		}
		requireClass(t, err, fault.Unauthenticated)
		if got := authenticator.requests[0].Scope.TenantID; got != id.TenantID("tenant-b") {
			t.Fatalf("authenticator scope tenant = %q; want the candidate tenant-b", got)
		}
	})

	t.Run("authenticated principal outside candidate", func(t *testing.T) {
		env := newTestEnv(t)
		lookup := &fakeLookup{entries: map[lookupKey]store.CandidateScope{
			{kind: store.CredentialExecution, id: string(env.executionID)}: {
				TenantID:    env.tenantID,
				ProjectID:   env.scope.ProjectID,
				PrincipalID: env.principalID,
			},
		}}
		authenticator := &fakeCredentialAuthenticator{result: token.Authenticated{
			PrincipalID: id.PrincipalID("principal-2"),
			Kind:        identity.KindExecution,
			Scope:       env.scope,
			Enabled:     true,
			Version:     3,
			ExecutionID: env.executionID,
		}}
		resolver := mustResolver(t, token.ResolverConfig{
			Lookup:      lookup,
			Credentials: authenticator,
			OIDC:        &fakeOIDCVerifier{},
			Principals:  env.principals,
			Clock:       env.clock,
		})
		presented := executionToken(t, env)
		principal, err := resolver.Resolve(context.Background(), presented)
		if err == nil {
			t.Fatalf("Resolve succeeded with %+v; want refusal", principal)
		}
		requireClass(t, err, fault.Unauthenticated)
	})
}

// TestResolverConcurrentAuthentication proves the resolver holds no mutable
// authentication state under concurrent valid and refused presentations.
func TestResolverConcurrentAuthentication(t *testing.T) {
	env := newResolverEnv(t)
	valid := executionToken(t, env.testEnv)
	tampered := tamperMAC(valid)
	if tampered == valid {
		t.Fatal("tampered token equals the valid token")
	}

	const workers = 16
	const iterations = 20
	var wait sync.WaitGroup
	failures := make(chan string, workers*iterations)
	for range workers {
		wait.Go(func() {
			for range iterations {
				principal, err := env.resolver.Resolve(context.Background(), valid)
				if err != nil {
					failures <- "valid token refused: " + err.Error()
					return
				}
				if principal.ID != env.principalID || principal.Kind != identity.KindExecution {
					failures <- "valid token resolved the wrong principal"
					return
				}
				_, err = env.resolver.Resolve(context.Background(), tampered)
				if err == nil {
					failures <- "tampered token authenticated"
					return
				}
				class, ok := fault.ClassOf(err)
				if !ok || class != fault.Unauthenticated {
					failures <- "tampered token returned a non-refusal class"
					return
				}
			}
		})
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

// TestResolverConstructorRequiresEveryDependency proves a partially configured
// resolver cannot be built.
func TestResolverConstructorRequiresEveryDependency(t *testing.T) {
	env := newTestEnv(t)
	base := token.ResolverConfig{
		Lookup:      &fakeLookup{entries: map[lookupKey]store.CandidateScope{}},
		Credentials: &fakeCredentialAuthenticator{},
		OIDC:        &fakeOIDCVerifier{},
		Principals:  env.principals,
		Clock:       env.clock,
	}
	configs := []struct {
		name   string
		mutate func(*token.ResolverConfig)
	}{
		{"nil lookup", func(config *token.ResolverConfig) { config.Lookup = nil }},
		{"nil credentials", func(config *token.ResolverConfig) { config.Credentials = nil }},
		{"nil oidc", func(config *token.ResolverConfig) { config.OIDC = nil }},
		{"nil principals", func(config *token.ResolverConfig) { config.Principals = nil }},
		{"nil clock", func(config *token.ResolverConfig) { config.Clock = nil }},
	}
	for _, testCase := range configs {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			testCase.mutate(&config)
			if _, err := token.NewResolver(config); err == nil {
				t.Fatal("NewResolver succeeded with a missing dependency")
			}
		})
	}
}
