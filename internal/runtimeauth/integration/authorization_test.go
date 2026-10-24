package integration_test

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// effect stands for a protected effect (secret resolution, artifact upload,
// source publication, result commit). It must run only after Authorize says yes.
type effect struct{ calls int }

func (e *effect) run(s *runtimeauth.Service, req runtimeauth.AuthorizeRequest) error {
	if _, err := s.Authorize(bg, req); err != nil {
		return err
	}
	e.calls++
	return nil
}

func twoAttempts(t *testing.T) (*world, rtest.Attempt, runtimeauth.Credential) {
	t.Helper()
	w := newWorld(t)
	c2 := rtest.ContextFor(w.Attempt.Context, "att_02")
	a2 := w.SeedAttempt(c2, rtest.AllActions())
	cred2, _, err := w.Register(a2, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	return w, a2, cred2
}

// Test_action_scope_and_path_binding (IT-06, IT-08, AR-05): authority belongs
// to one session on one attempt in one tenant, only for granted actions, and
// never covers human review.
func Test_action_scope_and_path_binding(t *testing.T) {
	t.Run("foreign attempt and session substitution", func(t *testing.T) {
		w, a2, cred2 := twoAttempts(t)
		tok1 := w.mustAdmit(w.Req).Token.Token
		resp2, err := w.Service.Admit(bg, "att_02", w.AdmitRequest(a2, cred2, nonce))
		if err != nil {
			t.Fatal(err)
		}
		tok2 := resp2.Token.Token
		var fx effect

		if err := fx.run(w.Service, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: tok1, Action: runtimeauth.ActionResultSubmit}); err != nil {
			t.Fatalf("control: %v", err)
		}
		for name, req := range map[string]runtimeauth.AuthorizeRequest{
			"session 1 on attempt 2": {AttemptID: "att_02", Bearer: tok1, Action: runtimeauth.ActionResultSubmit},
			"session 2 on attempt 1": {AttemptID: "att_01", Bearer: tok2, Action: runtimeauth.ActionResultSubmit},
			"unknown attempt":        {AttemptID: "att_77", Bearer: tok1, Action: runtimeauth.ActionResultSubmit},
			"malformed attempt":      {AttemptID: "../att_01", Bearer: tok1, Action: runtimeauth.ActionResultSubmit},
			"missing bearer":         {AttemptID: "att_01", Bearer: "", Action: runtimeauth.ActionResultSubmit},
		} {
			err := fx.run(w.Service, req)
			if !errors.Is(err, runtimeauth.ErrInvalidCredential) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if fx.calls != 1 {
			t.Fatalf("a substituted request reached the protected effect (%d calls)", fx.calls)
		}
		// Through HTTP the same substitution is a 401 and discloses nothing.
		for _, r := range []reply{w.call("GET", "att_02", "/context", nil, tok1), w.call("POST", "att_02", "/token", runtimev1.TokenRequest{}, tok1), w.call("GET", "att_77", "/context", nil, tok1)} {
			if r.Status != 401 || r.errorCode(t) != "invalid_credential" || contains(r.Body, "att_02", "tenantId") {
				t.Errorf("%d %s", r.Status, r.Body)
			}
		}
	})

	t.Run("foreign tenant", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		claims, err := w.Verifier.Verify(bg, resp.Token.Token)
		if err != nil {
			t.Fatal(err)
		}
		// A second tenant with its own attempt.
		c := rtest.ContextFor(w.Attempt.Context, "att_other")
		c.TenantID, c.ProjectID = "t019_globex", "p019_ledger"
		other := w.SeedAttempt(c, rtest.AllActions())
		credO, _, err := w.Register(other, rtest.AllActions())
		if err != nil {
			t.Fatal(err)
		}
		respO, err := w.Service.Admit(bg, "att_other", w.AdmitRequest(other, credO, nonce))
		if err != nil {
			t.Fatal(err)
		}
		// Correctly signed tokens that mix tenant and session fail: row level
		// security hides the other tenant's session.
		forged := w.MintToken(rtest.MintClaims{Subject: string(claims.Session), Tenant: "t019_globex", IssuedAt: w.Clock.Now(), ExpiresAt: w.Clock.Now().Add(time.Minute)})
		for name, r := range map[string]reply{
			"tenant substituted":     w.call("GET", "att_01", "/context", nil, forged),
			"tenant substituted b":   w.call("GET", "att_other", "/context", nil, forged),
			"other tenant on path 1": w.call("GET", "att_01", "/context", nil, respO.Token.Token),
			"first on other path":    w.call("GET", "att_other", "/context", nil, resp.Token.Token),
		} {
			if r.Status != 401 || contains(r.Body, "t019_globex", "t019_acme") {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		if r := w.call("GET", "att_other", "/context", nil, respO.Token.Token); r.Status != 200 {
			t.Fatalf("control: %d", r.Status)
		}
	})

	t.Run("ungranted actions", func(t *testing.T) {
		w := rtest.New(t, rtest.Options{})
		c, _ := w.HappyContext()
		ceiling, _ := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionResultSubmit)
		a := w.SeedAttempt(c, rtest.AllActions())
		cred, _, err := w.Register(a, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		ww := &world{Stack: w, Attempt: a, Cred: cred}
		tok := ww.mustAdmit(w.AdmitRequest(a, cred, nonce)).Token.Token
		var fx effect
		granted := runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: tok}
		for _, act := range []runtimeauth.Action{runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionResultSubmit} {
			granted.Action = act
			if err := fx.run(w.Service, granted); err != nil {
				t.Fatalf("%s: %v", act, err)
			}
		}
		denied := []runtimeauth.Action{
			runtimeauth.ActionSecretResolve, runtimeauth.ActionArtifactReserve, runtimeauth.ActionArtifactComplete,
			runtimeauth.ActionSourcePropose, runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke, runtimeauth.ActionDeploymentCall,
			// Human review and administration are not workload actions at all.
			"gate.approve", "gate.decide", "work.complete", "tenant.admin", "membership.grant", "review", "*", "",
		}
		for _, act := range denied {
			granted.Action = act
			if err := fx.run(w.Service, granted); !errors.Is(err, runtimeauth.ErrForbidden) {
				t.Errorf("%q: %v", act, err)
			}
		}
		if fx.calls != 3 {
			t.Fatalf("an ungranted action reached its effect (%d calls)", fx.calls)
		}

		// A ceiling can be narrowed by execution at any time, never widened.
		w.Authority.Update(`actions = ARRAY['context.read']`)
		granted.Action = runtimeauth.ActionResultSubmit
		if err := fx.run(w.Service, granted); !errors.Is(err, runtimeauth.ErrForbidden) {
			t.Fatalf("a narrowed attempt still allowed result.submit: %v", err)
		}
		granted.Action = runtimeauth.ActionContextRead
		if err := fx.run(w.Service, granted); err != nil {
			t.Fatal(err)
		}
		// Frozen route inventories exclude `forbidden` for /token: 401 there.
		if r := ww.renew(tok); r.Status != 401 || r.errorCode(t) != "invalid_credential" {
			t.Fatalf("narrowed renewal: %d %s", r.Status, r.Body)
		}
		w.Authority.Update(`actions = ARRAY['context.read','token.renew','result.submit','secret.resolve']`)
		granted.Action = runtimeauth.ActionSecretResolve
		if err := fx.run(w.Service, granted); !errors.Is(err, runtimeauth.ErrForbidden) {
			t.Fatalf("widening the attempt's actions widened a grant's ceiling: %v", err)
		}
	})

	t.Run("registration cannot name unknown actions", func(t *testing.T) {
		if _, err := runtimeauth.ParseActionSet([]string{"context.read", "gate.approve"}); err == nil {
			t.Fatal("unknown action in a ceiling")
		}
	})

	t.Run("session lifetime ends authority", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		max, _ := resp.Token.MaxExpiresAt.Time()
		w.Clock.Advance(max.Sub(w.Clock.Now()) + time.Second)
		// A token minted by a holder of the key after the session maximum is
		// still stopped by the durable session's maximum.
		long := w.MintToken(rtest.MintClaims{Subject: w.grantRow("att_01").Session, Tenant: "t_acme", IssuedAt: w.Clock.Now(), ExpiresAt: w.Clock.Now().Add(time.Minute)})
		if _, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: long, Action: runtimeauth.ActionContextRead}); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Fatalf("a token minted past the session maximum authorized: %v", err)
		}
	})

	t.Run("effective actions are reported", func(t *testing.T) {
		w := newWorld(t)
		tok := w.mustAdmit(w.Req).Token.Token
		a, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: tok, Action: runtimeauth.ActionResultSubmit})
		if err != nil {
			t.Fatal(err)
		}
		if a.Scope != w.Attempt.Scope || a.RunID != "run_01" || a.AttemptID != "att_01" || a.Action != runtimeauth.ActionResultSubmit || !a.Actions.Equal(rtest.AllActions()) {
			t.Fatalf("authorization = %+v", a)
		}
	})
}
