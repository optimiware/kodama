package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
)

// afterAuthorize runs a hook between the workload authorization and admission:
// the window a cancellation can race.
type afterAuthorize struct {
	inner gateway.Workloads
	hook  func()
}

func (a afterAuthorize) Authorize(ctx context.Context, attempt domain.AttemptID, bearer string) (gateway.Workload, error) {
	w, err := a.inner.Authorize(ctx, attempt, bearer)
	if err == nil && a.hook != nil {
		a.hook()
	}
	return w, err
}

func untouched(t *testing.T, e *gatewaytest.Env, a gatewaytest.Attempt) {
	t.Helper()
	if n := len(e.Provider.Calls()); n != 0 {
		t.Fatalf("provider traffic: %d requests", n)
	}
	if n := e.Count(`SELECT count(*) FROM model_requests WHERE run_id = $1`, string(a.RunID)); n != 0 {
		t.Fatalf("%d requests were admitted", n)
	}
	if n := e.Count(`SELECT count(*) FROM model_run_bindings WHERE run_id = $1 AND requests_admitted > 0`, string(a.RunID)); n != 0 {
		t.Fatal("the run's request counter moved")
	}
}

// Test_gateway_authority: GM-01, GM-A01. Foreign project references, forged
// scope, wrong audience, expired and revoked sessions, a missing model.invoke
// grant, and a cancellation racing admission all fail closed before any
// provider traffic.
func Test_gateway_authority(t *testing.T) {
	t.Run("foreign project and tenant references fail", func(t *testing.T) {
		e := newWorld(t)
		scopeA2 := executiontest.ScopeA2

		// Settings cannot reference another project's secret, a non-model secret,
		// or a version that does not exist.
		foreign := e.CreateSecret(scopeA2, "other-key", secrets.KindModel, e.Payload(gatewaytest.Bearer, e.Provider.Origin()))
		ed := e.Edit()
		ed.Credential = gateway.CredentialRef{Ref: "secret://project/other-key", Version: foreign.CurrentVersion()}
		if _, err := e.TryConfigure(e.Replica, scopeA, ed); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("a foreign project's secret: %v", err)
		}
		e.CreateSecret(scopeA, "generic-key", secrets.KindGeneric, []byte("x"))
		ed.Credential = gateway.CredentialRef{Ref: "secret://project/generic-key", Version: 1}
		if _, err := e.TryConfigure(e.Replica, scopeA, ed); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("a non-model secret: %v", err)
		}
		ed.Credential = gateway.CredentialRef{Ref: "secret://project/" + gatewaytest.SecretName, Version: 9}
		if _, err := e.TryConfigure(e.Replica, scopeA, ed); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("a missing version: %v", err)
		}

		// A project without settings, or another tenant, gets no model access.
		other := e.NewAttempt(scopeA2)
		if _, err := e.Invoke(e.Replica, other, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("an unconfigured project: %v", err)
		}
		tenantB := e.NewAttempt(executiontest.ScopeB)
		if _, err := e.Invoke(e.Replica, tenantB, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrNotConfigured) {
			t.Fatalf("another tenant: %v", err)
		}
		untouched(t, e, other)
		untouched(t, e, tenantB)

		// Usage of one project's request is not readable through another scope.
		a := e.NewAttempt(scopeA)
		res, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{})
		if err != nil {
			t.Fatal(err)
		}
		admin := gateway.Actor{ID: gatewaytest.Admin}
		if _, err := e.Replica.Svc.UsageOf(context.Background(), admin, scopeA, res.RequestID); err != nil {
			t.Fatalf("own usage: %v", err)
		}
		for _, s := range []domain.Scope{scopeA2, executiontest.ScopeB} {
			if _, err := e.Replica.Svc.UsageOf(context.Background(), admin, s, res.RequestID); err == nil {
				t.Fatalf("usage was readable through %v", s)
			}
		}
	})

	t.Run("a forged workload scope is refused by the transaction-bound check", func(t *testing.T) {
		e := newWorld(t)
		victim := e.NewAttempt(executiontest.ScopeA2)
		forged := victim.Workload
		forged.Scope = scopeA // another project of the same tenant, which has settings
		_, err := e.Replica.Store.Admit(context.Background(), gateway.AdmitCommand{
			Workload: forged, RequestID: "mrq_forged0000000000000000000000000", Model: "gpt-a", RequestBytes: 10, InputBound: 5, OutputMax: 8, Now: time.Now(),
		})
		if !errors.Is(err, gateway.ErrForbidden) && !errors.Is(err, gateway.ErrUnauthenticated) {
			t.Fatalf("a forged scope: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM model_requests`); n != 0 {
			t.Fatalf("%d requests exist", n)
		}
	})

	t.Run("tokens: wrong audience, issuer, tenant, route and lifetime", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		other := e.NewAttempt(scopeA)
		call := func(bearer string, route domain.AttemptID) error {
			_, err := e.Replica.Svc.Invoke(context.Background(), gateway.InvokeRequest{Attempt: route, Bearer: bearer, Body: body("x"), Sink: &gatewaytest.Sink{}})
			return err
		}
		// The same minting path with no broken rule is accepted: the failures below
		// are the one rule each breaks.
		if err := call(e.Mint(a, nil), a.ID); err != nil {
			t.Fatalf("a correctly minted token: %v", err)
		}
		for name, tok := range map[string]string{
			"audience": e.Mint(a, func(c *rtest.MintClaims) { c.Audience = "kodama-other" }),
			"issuer":   e.Mint(a, func(c *rtest.MintClaims) { c.Issuer = "https://evil.invalid" }),
			"tenant":   e.Mint(a, func(c *rtest.MintClaims) { c.Tenant = string(executiontest.ScopeB.TenantID) }),
			"expired": e.Mint(a, func(c *rtest.MintClaims) {
				c.IssuedAt, c.ExpiresAt = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
			}),
			"garbage": "not-a-token",
			"empty":   "",
		} {
			if err := call(tok, a.ID); !errors.Is(err, gateway.ErrUnauthenticated) && !errors.Is(err, gateway.ErrForbidden) {
				t.Errorf("%s: %v, want a refusal", name, err)
			}
		}
		// A valid token of one attempt is not valid on another attempt's route.
		if err := call(a.Token, other.ID); err == nil {
			t.Error("a token was accepted on another attempt's route")
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d, want only the one correct request", n)
		}
	})

	t.Run("expired session", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		e.X.Clock.Advance(10 * time.Minute) // beyond the 5 minute token lifetime
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrUnauthenticated) {
			t.Fatalf("an expired token: %v", err)
		}
		untouched(t, e, a)
	})

	t.Run("revoked session", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		if err := e.RT.Revoke(context.Background(), a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("a revoked session: %v", err)
		}
		untouched(t, e, a)
	})

	t.Run("a grant without model.invoke", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewRunWith(scopeA, 1, runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew)[0]
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("missing model.invoke: %v", err)
		}
		untouched(t, e, a)
	})

	t.Run("cancellation racing admission", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		real, err := adapters.NewRuntimeWorkloads(e.RT)
		if err != nil {
			t.Fatal(err)
		}
		// The run is cancelled after the bearer was verified and before admission
		// takes its locks: the verification is not a permit.
		racing := e.NewReplica(gatewaytest.ReplicaOptions{Workloads: afterAuthorize{inner: real, hook: func() { e.CancelRun(a) }}})
		if _, err := e.Invoke(racing, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrForbidden) {
			t.Fatalf("admission after cancellation: %v", err)
		}
		untouched(t, e, a)
	})

	t.Run("settings need current management authority", func(t *testing.T) {
		e := newWorld(t)
		ctx := context.Background()
		cur, err := e.Replica.Svc.GetSettings(ctx, gateway.Actor{ID: gatewaytest.Viewer}, scopeA)
		if err != nil || !cur.Enabled || cur.Revision != 2 {
			t.Fatalf("viewer read: %+v %v", cur, err)
		}
		un, err := e.Replica.Svc.GetSettings(ctx, gateway.Actor{ID: gatewaytest.Admin}, executiontest.ScopeA2)
		if err != nil || un.Enabled || un.Revision != 1 || un.Stored() {
			t.Fatalf("a project without settings reads the disabled revision one: %+v %v", un, err)
		}
		if _, err := e.Replica.Svc.ReplaceSettings(ctx, gateway.Actor{ID: gatewaytest.Viewer}, scopeA, cur.Revision, e.Edit(), "r"); !errors.Is(err, gateway.ErrDenied) {
			t.Fatalf("a viewer replacing settings: %v", err)
		}
		// Another tenant's administrator cannot even see the project.
		if _, err := e.Replica.Svc.GetSettings(ctx, gateway.Actor{ID: executiontest.OtherEng}, scopeA); !errors.Is(err, gateway.ErrHidden) {
			t.Fatalf("a foreign tenant's user: %v", err)
		}
		if _, err := e.Replica.Svc.GetSettings(ctx, gateway.Actor{}, scopeA); !errors.Is(err, gateway.ErrUnauthenticated) {
			t.Fatalf("no actor: %v", err)
		}
		// A permission revoked between the check and the write is seen by the
		// transaction-bound recheck: the use case's earlier answer is not a permit.
		e.X.Revoke(scopeA, gatewaytest.Admin)
		if _, err := e.Replica.Svc.ReplaceSettings(ctx, gateway.Actor{ID: gatewaytest.Admin}, scopeA, cur.Revision, e.Edit(), "r"); err == nil {
			t.Fatal("a revoked administrator replaced settings")
		}
		// And the store itself refuses without a current permission, whatever the
		// use case believed.
		if _, err := e.Replica.Store.UpdateSettings(ctx, gateway.UpdateSettings{Actor: gateway.Actor{ID: gatewaytest.Admin}, Scope: scopeA, Expected: cur.Revision, Edit: e.Edit(), Now: time.Now()}); err == nil {
			t.Fatal("the store accepted a write from a revoked administrator")
		}
		if got, _ := e.Replica.Store.Settings(ctx, scopeA); got.Revision != cur.Revision {
			t.Fatalf("revision moved to %d", got.Revision)
		}
	})
}
