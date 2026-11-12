// Derived-credential authentication tests.
//
// A presented execution/service token is only a lookup hint until the current
// tenant key, the current nonce/expiry pair and the current principal/service
// state all agree. These tests drive that boundary with deterministic readers
// and a fixed clock; no HTTP, database, KMS or runtime is involved.
package token_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func executionToken(t *testing.T, env *testEnv) string {
	t.Helper()
	presented, err := token.DeriveExecutionToken(env.keyBytes, token.ExecutionClaims{
		TenantID:    env.tenantID,
		ExecutionID: env.executionID,
		Nonce:       env.creds.fact.Credential.Nonce,
		Expiry:      env.creds.fact.Credential.Expiry,
	})
	if err != nil {
		t.Fatalf("derive execution token: %v", err)
	}
	return presented
}

func serviceToken(t *testing.T, env *testEnv) string {
	t.Helper()
	presented, err := token.DeriveServiceToken(env.keyBytes, token.ServiceClaims{
		TenantID:  env.tenantID,
		ServiceID: env.serviceID,
		Nonce:     env.creds.fact.Credential.Nonce,
		Expiry:    env.creds.fact.Credential.Expiry,
	})
	if err != nil {
		t.Fatalf("derive service token: %v", err)
	}
	return presented
}

func executionAuthenticateRequest(env *testEnv, presented string) token.AuthenticateRequest {
	return token.AuthenticateRequest{
		Scope:       env.scope,
		Presented:   presented,
		PrincipalID: env.principalID,
		ExecutionID: env.executionID,
		Now:         env.now,
	}
}

func serviceAuthenticateRequest(env *testEnv, presented string) token.AuthenticateRequest {
	return token.AuthenticateRequest{
		Scope:       env.scope,
		Presented:   presented,
		PrincipalID: env.principalID,
		ServiceID:   env.serviceID,
		Now:         env.now,
	}
}

// TestManagerAuthenticateExecutionUsesCurrentState proves a valid execution
// token returns the current principal identity/version, not a cached copy.
func TestManagerAuthenticateExecutionUsesCurrentState(t *testing.T) {
	env := newTestEnv(t)
	presented := executionToken(t, env)
	env.principals.principal.Version = 4

	authenticated, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.Kind != identity.KindExecution {
		t.Fatalf("Kind = %q; want execution", authenticated.Kind)
	}
	if authenticated.Scope != env.scope || authenticated.PrincipalID != env.principalID || authenticated.ExecutionID != env.executionID {
		t.Fatalf("identity = %+v; want current execution", authenticated)
	}
	if authenticated.Version != 4 {
		t.Fatalf("Version = %d; want current version 4", authenticated.Version)
	}
	if len(authenticated.Roles) != 0 {
		t.Fatalf("execution roles = %v; want empty", authenticated.Roles)
	}
	if env.creds.calls != 1 || env.keys.calls != 1 || len(env.resolver.keys) != 1 || env.principals.calls != 1 {
		t.Fatalf("reads = creds:%d keys:%d resolver:%d principals:%d; want 1 each",
			env.creds.calls, env.keys.calls, len(env.resolver.keys), env.principals.calls)
	}
}

// TestManagerAuthenticateServiceUsesCurrentRoles proves service authentication
// returns the current scoped roles and never an Admin role.
func TestManagerAuthenticateServiceUsesCurrentRoles(t *testing.T) {
	env := newTestEnv(t)
	env.setServiceCredentialAbsent()
	env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
	env.services.service.Version = 5
	env.services.service.Roles = []tenancy.Role{tenancy.RoleViewer, tenancy.RoleOperator}
	presented := serviceToken(t, env)

	authenticated, err := env.manager.Authenticate(context.Background(), serviceAuthenticateRequest(env, presented))
	if err != nil {
		t.Fatalf("Authenticate service: %v", err)
	}
	if authenticated.Kind != identity.KindService {
		t.Fatalf("Kind = %q; want service", authenticated.Kind)
	}
	if authenticated.ServiceID != env.serviceID || authenticated.ExecutionID != "" {
		t.Fatalf("identity = %+v; want service target", authenticated)
	}
	if authenticated.Version != 5 {
		t.Fatalf("Version = %d; want 5", authenticated.Version)
	}
	if len(authenticated.Roles) != 2 || authenticated.Roles[0] != tenancy.RoleViewer || authenticated.Roles[1] != tenancy.RoleOperator {
		t.Fatalf("Roles = %v; want current [viewer operator]", authenticated.Roles)
	}
	if env.services.calls != 1 {
		t.Fatalf("service reads = %d; want 1", env.services.calls)
	}
}

// TestManagerAuthenticateRejectsRevokedCredential proves a cleared pair cannot
// authenticate and the tenant key is not even read.
func TestManagerAuthenticateRejectsRevokedCredential(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()
	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, "kodex.exec-1.placeholder"))
	requireRefusal(t, err)
	if env.keys.calls != 0 {
		t.Fatalf("tenant key read for an absent credential: %d", env.keys.calls)
	}
}

// TestManagerAuthenticateRejectsExpiredCredential proves the expiry boundary is
// inclusive and current time is authoritative.
func TestManagerAuthenticateRejectsExpiredCredential(t *testing.T) {
	env := newTestEnv(t)
	env.creds.fact.Credential.Expiry = env.now
	presented := executionToken(t, env)

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateRejectsWrongKind proves an execution token cannot be
// presented at a service boundary and vice versa.
func TestManagerAuthenticateRejectsWrongKind(t *testing.T) {
	env := newTestEnv(t)
	env.setServiceCredentialAbsent()
	env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
	executionPresented := executionToken(t, env)

	_, err := env.manager.Authenticate(context.Background(), serviceAuthenticateRequest(env, executionPresented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateRejectsWrongTenant proves a token derived under
// another tenant's key never verifies against this tenant's current fact.
func TestManagerAuthenticateRejectsWrongTenant(t *testing.T) {
	env := newTestEnv(t)
	presented, err := token.DeriveExecutionToken(otherKey(), token.ExecutionClaims{
		TenantID:    id.TenantID("tenant-b"),
		ExecutionID: env.executionID,
		Nonce:       env.creds.fact.Credential.Nonce,
		Expiry:      env.creds.fact.Credential.Expiry,
	})
	if err != nil {
		t.Fatalf("derive foreign token: %v", err)
	}

	_, err = env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateRejectsWrongMAC proves any MAC change is a refusal.
func TestManagerAuthenticateRejectsWrongMAC(t *testing.T) {
	env := newTestEnv(t)
	presented := flipLastCharacter(executionToken(t, env))

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateRejectsDisabledState proves both the credential fact's
// enablement (request-wait/terminal execution) and the current principal's
// enablement fail closed.
func TestManagerAuthenticateRejectsDisabledState(t *testing.T) {
	t.Run("disabled credential fact", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.creds.fact.Enabled = false
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireRefusal(t, err)
	})

	t.Run("disabled current principal", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.principals.principal.Enabled = false
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireRefusal(t, err)
	})

	t.Run("disabled service", func(t *testing.T) {
		env := newTestEnv(t)
		env.setServiceCredentialAbsent()
		env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
		presented := serviceToken(t, env)
		env.services.service.State = service.StateDisabled
		_, err := env.manager.Authenticate(context.Background(), serviceAuthenticateRequest(env, presented))
		requireRefusal(t, err)
	})
}

// TestManagerAuthenticateRejectsCrossScopeFact proves a reader returning
// another tenant's fact cannot authenticate in the caller's scope.
func TestManagerAuthenticateRejectsCrossScopeFact(t *testing.T) {
	env := newTestEnv(t)
	presented := executionToken(t, env)
	env.creds.fact.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
	if env.keys.calls != 0 {
		t.Fatalf("tenant key read for a cross-scope fact: %d", env.keys.calls)
	}
}

// TestManagerAuthenticateReadsCurrentStateEveryTime proves no cached allow:
// repeated authentication re-reads the credential, key and principal.
func TestManagerAuthenticateReadsCurrentStateEveryTime(t *testing.T) {
	env := newTestEnv(t)
	presented := executionToken(t, env)

	for i := range 2 {
		if _, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented)); err != nil {
			t.Fatalf("Authenticate call %d: %v", i+1, err)
		}
	}
	if env.creds.calls != 2 || env.keys.calls != 2 || env.principals.calls != 2 {
		t.Fatalf("reads = creds:%d keys:%d principals:%d; want 2 each (no cached authority)",
			env.creds.calls, env.keys.calls, env.principals.calls)
	}
}

// TestManagerAuthenticateRejectsKeyRotation proves a tenant-key rotation
// immediately invalidates previously issued tokens.
func TestManagerAuthenticateRejectsKeyRotation(t *testing.T) {
	env := newTestEnv(t)
	presented := executionToken(t, env)
	if _, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented)); err != nil {
		t.Fatalf("pre-rotation Authenticate: %v", err)
	}

	env.resolver.key = otherKey()
	env.keys.key.Version = 2
	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateRejectsShortResolvedKey proves the plaintext
// derivation key is at least 256 bits before it is used.
func TestManagerAuthenticateRejectsShortResolvedKey(t *testing.T) {
	env := newTestEnv(t)
	presented := executionToken(t, env)
	env.resolver.key = env.resolver.key[:token.MinNonceBytes-1]

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	requireRefusal(t, err)
}

// TestManagerAuthenticateDependencyFailuresAreUnavailable proves a current-state
// read failure is a retryable dependency outage, never a credential verdict: a
// store or KMS outage must not tell the caller its token was revoked.
func TestManagerAuthenticateDependencyFailuresAreUnavailable(t *testing.T) {
	t.Run("credential read", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.creds.err = errors.New("credential store unavailable")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("tenant key read", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.keys.err = errors.New("key store unavailable")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("current principal read", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.principals.err = errors.New("membership store unavailable")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("derivation key resolve", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.resolver.err = errors.New("kms unavailable")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("current service read", func(t *testing.T) {
		env := newTestEnv(t)
		env.setServiceCredentialAbsent()
		env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
		presented := serviceToken(t, env)
		env.services.err = errors.New("service store unavailable")
		_, err := env.manager.Authenticate(context.Background(), serviceAuthenticateRequest(env, presented))
		requireClass(t, err, fault.DependencyUnavailable)
	})
}

// TestManagerAuthenticateMissingStateStaysUnauthenticated proves a genuinely
// absent current-state row is still an authentication refusal, not an outage:
// only the not-found class maps to 401, every other read error to 503.
func TestManagerAuthenticateMissingStateStaysUnauthenticated(t *testing.T) {
	t.Run("credential row", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.creds.err = fault.Wrap(fault.NotFound, "credential row is missing", errors.New("no rows"))
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.Unauthenticated)
	})

	t.Run("tenant key row", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.keys.err = fault.New(fault.NotFound, "tenant key row is missing")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.Unauthenticated)
	})

	t.Run("current principal row", func(t *testing.T) {
		env := newTestEnv(t)
		presented := executionToken(t, env)
		env.principals.err = fault.New(fault.NotFound, "principal row is missing")
		_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
		requireClass(t, err, fault.Unauthenticated)
	})

	t.Run("current service row", func(t *testing.T) {
		env := newTestEnv(t)
		env.setServiceCredentialAbsent()
		env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
		presented := serviceToken(t, env)
		env.services.err = fault.New(fault.NotFound, "service row is missing")
		_, err := env.manager.Authenticate(context.Background(), serviceAuthenticateRequest(env, presented))
		requireClass(t, err, fault.Unauthenticated)
	})
}

// TestManagerAuthenticateRejectsInvalidRequests proves malformed input is
// refused before any read.
func TestManagerAuthenticateRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testEnv, *token.AuthenticateRequest)
	}{
		{name: "empty token", mutate: func(_ *testEnv, request *token.AuthenticateRequest) { request.Presented = "" }},
		{name: "zero time", mutate: func(_ *testEnv, request *token.AuthenticateRequest) { request.Now = time.Time{} }},
		{name: "invalid scope", mutate: func(_ *testEnv, request *token.AuthenticateRequest) { request.Scope = tenancy.Scope{} }},
		{name: "no target", mutate: func(_ *testEnv, request *token.AuthenticateRequest) { request.ExecutionID = "" }},
		{name: "two targets", mutate: func(env *testEnv, request *token.AuthenticateRequest) { request.ServiceID = env.serviceID }},
		{name: "invalid principal", mutate: func(_ *testEnv, request *token.AuthenticateRequest) {
			request.PrincipalID = id.PrincipalID("../principal")
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			presented := executionToken(t, env)
			request := executionAuthenticateRequest(env, presented)
			testCase.mutate(env, &request)
			_, err := env.manager.Authenticate(context.Background(), request)
			requireClass(t, err, fault.InvalidInput)
			if env.creds.calls != 0 || env.keys.calls != 0 {
				t.Fatalf("invalid request reached reads: creds=%d keys=%d", env.creds.calls, env.keys.calls)
			}
		})
	}
}

// TestManagerAuthenticateRedactsTokenAndKey proves public errors never render
// the presented bearer token or the plaintext derivation key.
func TestManagerAuthenticateRedactsTokenAndKey(t *testing.T) {
	env := newTestEnv(t)
	presented := flipLastCharacter(executionToken(t, env))

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, presented))
	if err == nil {
		t.Fatal("tampered token authenticated")
	}
	message := err.Error()
	if strings.Contains(message, presented) {
		t.Fatalf("error leaked the bearer token: %s", message)
	}
	if strings.Contains(message, hex.EncodeToString(env.keyBytes)) {
		t.Fatalf("error leaked the derivation key: %s", message)
	}
}

// TestManagerAuthenticateRejectsServiceTokenAtExecutionBoundary proves the
// token kind is checked before the MAC lookup.
func TestManagerAuthenticateRejectsServiceTokenAtExecutionBoundary(t *testing.T) {
	env := newTestEnv(t)
	env.setServiceCredentialAbsent()
	env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
	servicePresented := serviceToken(t, env)

	_, err := env.manager.Authenticate(context.Background(), executionAuthenticateRequest(env, servicePresented))
	requireRefusal(t, err)
}
