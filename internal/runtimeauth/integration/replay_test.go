package integration_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// Test_admission_replay_same_nonce_then_new_nonce replays the frozen
// admission_replay_same_nonce_then_new_nonce scenario (IT-06) across service
// reconstruction and lost responses.
func Test_admission_replay_same_nonce_then_new_nonce(t *testing.T) {
	frozen := rtest.LoadScenario(t, "admission_replay_same_nonce_then_new_nonce")
	w := newWorld(t)
	otherNonce, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](frozen.Steps[2].Request)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus, wantCode := scenarioError(t, "admission_replay_same_nonce_then_new_nonce")
	otherNonce.AdmissionCredential = w.Cred.Reveal()

	first := w.mustAdmit(w.Req)

	// "Restart": a brand new service over the same database and key ring.
	restarted := w.NewService(rtest.TestPolicy, w.Store, w.Authority, w.Jobs, w.Signer)
	w.Clock.Advance(90 * time.Second)
	second, err := restarted.Admit(bg, "att_01", w.Req)
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if !reflect.DeepEqual(first.Context, second.Context) {
		t.Fatal("the replay returned a different context")
	}
	if first.Token.MaxExpiresAt != second.Token.MaxExpiresAt {
		t.Fatalf("the replay moved the maximum: %s -> %s", first.Token.MaxExpiresAt, second.Token.MaxExpiresAt)
	}
	if first.Token.Token == second.Token.Token {
		t.Fatal("the replay must return a fresh token")
	}
	for name, tok := range map[string]string{"first": first.Token.Token, "replayed": second.Token.Token} {
		if r := w.getContext(tok); r.Status != 200 {
			t.Errorf("%s token: %d %s", name, r.Status, r.Body)
		}
	}
	row := w.grantRow("att_01")

	// Another nonce after consumption is the frozen conflict.
	r := w.admit(otherNonce)
	if r.Status != wantStatus || r.errorCode(t) != wantCode {
		t.Fatalf("new nonce: %d %s, want %d %s", r.Status, r.Body, wantStatus, wantCode)
	}
	if after := w.grantRow("att_01"); after != row {
		t.Fatal("a conflicting exchange changed the grant")
	}
	// The conflicting exchange did not damage the legitimate nonce.
	if _, err := restarted.Admit(bg, "att_01", w.Req); err != nil {
		t.Fatalf("replay after a conflict: %v", err)
	}

	t.Run("replay is bounded by the session maximum", func(t *testing.T) {
		max, _ := first.Token.MaxExpiresAt.Time()
		w.Clock.Advance(max.Sub(w.Clock.Now()))
		r := w.admit(w.Req)
		if r.Status != 401 || r.errorCode(t) != "invalid_credential" {
			t.Fatalf("replay at the maximum: %d %s", r.Status, r.Body)
		}
	})

	t.Run("replay does not outlive cancellation", func(t *testing.T) {
		w := newWorld(t)
		w.mustAdmit(w.Req)
		if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", false); err != nil {
			t.Fatal(err)
		}
		if r := w.admit(w.Req); r.Status != 403 || r.errorCode(t) != "attempt_revoked" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("the credential is bound to its attempt", func(t *testing.T) {
		w := newWorld(t)
		r := w.call("POST", "att_99", "/admit", w.Req, "")
		if r.Status != 401 || r.errorCode(t) != "invalid_credential" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		if w.grantRow("att_01").Consumed {
			t.Fatal("consumed through another attempt's route")
		}
	})

	t.Run("an unexpired but unused credential stays redeemable until its window ends", func(t *testing.T) {
		w := newWorld(t)
		w.Clock.Advance(rtest.TestPolicy.AdmissionTTL - time.Second)
		w.mustAdmit(w.Req)
		w2 := newWorld(t)
		w2.Clock.Advance(rtest.TestPolicy.AdmissionTTL)
		if r := w2.admit(w2.Req); r.Status != 401 {
			t.Fatalf("an expired credential redeemed: %d %s", r.Status, r.Body)
		}
	})

	t.Run("registration is idempotent and never extends or replaces", func(t *testing.T) {
		w := newWorld(t)
		_, reg1, err := w.Register(w.Attempt, rtest.AllActions())
		if err == nil {
			t.Fatalf("a second credential replaced the grant: %+v", reg1)
		}
		reg, err := w.RegisterWith(w.Attempt, w.Cred, rtest.AllActions())
		if err != nil || reg.Created {
			t.Fatalf("the retry must observe the original registration: %+v %v", reg, err)
		}
		w.Clock.Advance(time.Minute)
		again, err := w.RegisterWith(w.Attempt, w.Cred, rtest.AllActions())
		if err != nil || !again.ExpiresAt.Equal(reg.ExpiresAt) {
			t.Fatalf("the retry extended the window: %v %v", again.ExpiresAt, reg.ExpiresAt)
		}
		narrow, _ := runtimeauth.NewActionSet(runtimeauth.ActionContextRead)
		if _, err := w.RegisterWith(w.Attempt, w.Cred, narrow); err == nil {
			t.Fatal("a conflicting ceiling replaced the registration")
		}
		// After revocation even an identical registration is refused.
		if err := w.Service.Revoke(bg, w.Attempt.Scope, "att_01"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.RegisterWith(w.Attempt, w.Cred, rtest.AllActions()); err == nil {
			t.Fatal("a revoked grant registered again")
		}
	})

	t.Run("registration validates against execution state", func(t *testing.T) {
		w := rtest.New(t, rtest.Options{})
		c, _ := w.HappyContext()
		wider := rtest.AllActions()
		a := w.SeedAttempt(c, func() runtimeauth.ActionSet {
			s, _ := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew)
			return s
		}())
		if _, _, err := w.Register(a, wider); err == nil {
			t.Fatal("a ceiling above the attempt's allowed actions was registered")
		}
		other := c
		other.Inputs = map[string]runtimev1.Value{"x": runtimev1.StringValue("not the pinned context")}
		if _, err := w.RegisterWith(rtest.Attempt{Scope: a.Scope, Context: other}, mustCredential(t), a.Actions); err == nil {
			t.Fatal("a context other than the attempt's pinned one was registered")
		}
		foreign := c
		foreign.TenantID = "t019_other"
		if _, err := w.RegisterWith(rtest.Attempt{Scope: a.Scope, Context: foreign}, mustCredential(t), a.Actions); err == nil {
			t.Fatal("a context of another tenant was registered")
		}
		if err := w.Authority.Cancel(bg, a.Scope, "att_01", false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := w.Register(a, a.Actions); err == nil {
			t.Fatal("a cancelled attempt accepted a registration")
		}
	})
}

func mustCredential(t *testing.T) runtimeauth.Credential {
	t.Helper()
	c, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Test_admission_failure_recovery (IT-06, PX-08): rollback, uncertain commit,
// signing failure, storage outage and key/ownership outage fail closed, never
// leak the cause, and recover through bounded replay with the same credential
// and nonce.
func Test_admission_failure_recovery(t *testing.T) {
	const secret = "SECRET-CAUSE-TEXT"
	unavailable := func(t *testing.T, w *world, r reply) {
		t.Helper()
		if r.Status != 503 || r.errorCode(t) != "unavailable" {
			t.Fatalf("got %d %s", r.Status, r.Body)
		}
		if r.Header.Get("Retry-After") == "" {
			t.Fatal("no Retry-After on an unavailable answer")
		}
		if contains(r.Body, secret, `"token"`, `"context"`) {
			t.Fatalf("the failure leaked: %s", r.Body)
		}
	}
	cases := []struct {
		name     string
		inject   func(*world)
		clear    func(*world)
		consumed bool // the failed exchange durably consumed the grant
	}{
		{"rollback before commit", func(w *world) { w.Faults.RollbackAfterWork(true) }, func(w *world) { w.Faults.RollbackAfterWork(false) }, false},
		{"commit succeeded but its outcome is unknown", func(w *world) { w.Faults.LoseCommit(true) }, func(w *world) { w.Faults.LoseCommit(false) }, true},
		{"signing failure", func(w *world) { w.Faults.FailSigning(true) }, func(w *world) { w.Faults.FailSigning(false) }, false},
		{"credential lookup outage", func(w *world) { w.Faults.FailLocate(true) }, func(w *world) { w.Faults.FailLocate(false) }, false},
		{"transaction outage", func(w *world) { w.Faults.FailTransactions(true) }, func(w *world) { w.Faults.FailTransactions(false) }, false},
		{"execution state outage", func(w *world) { w.Faults.FailObserveAttempt(true) }, func(w *world) { w.Faults.FailObserveAttempt(false) }, false},
		{"execution lock outage", func(w *world) { w.Faults.FailLockAttempt(true) }, func(w *world) { w.Faults.FailLockAttempt(false) }, false},
		{"Kubernetes outage", func(w *world) { w.Jobs.SetError(rtest.ErrInjected) }, func(w *world) { w.Jobs.SetError(nil) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.inject(w)
			unavailable(t, w, w.admit(w.Req))
			if got := w.grantRow("att_01").Consumed; got != c.consumed {
				t.Fatalf("consumed = %v, want %v", got, c.consumed)
			}
			c.clear(w)
			// Bounded replay with the same credential and nonce recovers.
			resp := w.mustAdmit(w.Req)
			if r := w.getContext(resp.Token.Token); r.Status != 200 {
				t.Fatalf("recovered token: %d %s", r.Status, r.Body)
			}
			// ...without creating a replacement grant or second consumption.
			var n int
			if err := w.Owner.QueryRow(bg, `SELECT count(*) FROM runtime_grants`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("grants = %d %v", n, err)
			}
			if strings.Contains(w.Logs.String(), secret) {
				t.Fatalf("the cause reached the logs:\n%s", w.Logs.String())
			}
		})
	}

	t.Run("a lost commit still lets only the original nonce through", func(t *testing.T) {
		w := newWorld(t)
		w.Faults.LoseCommit(true)
		unavailable(t, w, w.admit(w.Req))
		w.Faults.LoseCommit(false)
		other := w.Req
		other.Nonce = "mmmmmmmmmmmmmmmmmmmmmm"
		if r := w.admit(other); r.Status != 409 || r.errorCode(t) != "admission_consumed" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		w.mustAdmit(w.Req)
	})

	t.Run("outages after admission fail closed for tokens too", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		for name, inject := range map[string]func(bool){
			"transactions": w.Faults.FailTransactions, "execution lock": w.Faults.FailLockAttempt,
		} {
			inject(true)
			for op, r := range map[string]reply{"context": w.getContext(resp.Token.Token), "renew": w.renew(resp.Token.Token)} {
				if r.Status != 503 || contains(r.Body, secret, `"tenantId"`, `"token"`) {
					t.Errorf("%s outage on %s: %d %s", name, op, r.Status, r.Body)
				}
			}
			if _, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: resp.Token.Token, Action: runtimeauth.ActionResultSubmit}); err == nil {
				t.Errorf("%s outage authorized a protected action", name)
			}
			inject(false)
		}
		if r := w.getContext(resp.Token.Token); r.Status != 200 {
			t.Fatalf("after the outage: %d", r.Status)
		}
		w.Faults.FailSigning(true)
		if r := w.renew(resp.Token.Token); r.Status != 503 {
			t.Fatalf("a signing failure on renewal: %d %s", r.Status, r.Body)
		}
	})
}
