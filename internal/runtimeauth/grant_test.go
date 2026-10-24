package runtimeauth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

var (
	t0     = time.Date(2026, 10, 19, 10, 0, 0, 123456789, time.UTC)
	policy = runtimeauth.Policy{AdmissionTTL: 10 * time.Minute, TokenTTL: 5 * time.Minute, MaxSession: 2 * time.Hour}
	podA   = runtimeauth.PodBinding{JobUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", PodName: "kodama-att-01-x7k2p", PodUID: "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"}
	podB   = runtimeauth.PodBinding{JobUID: podA.JobUID, PodName: "kodama-att-01-other", PodUID: "8b3e3f40-5c6d-4e7f-8ab1-2c3d4e5f6071"}
)

func actions(t *testing.T, as ...runtimeauth.Action) runtimeauth.ActionSet {
	t.Helper()
	s, err := runtimeauth.NewActionSet(as...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func provision(t *testing.T) runtimeauth.Provision {
	t.Helper()
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	return runtimeauth.Provision{
		Scope: domain.Scope{TenantID: "t019_acme", ProjectID: "p019_inventory"}, RunID: "run_01", AttemptID: "att_01",
		Job:              runtimeauth.ExpectedJob{Namespace: "t019-runs", Name: "kodama-att-01"},
		CredentialDigest: cred.Digest(), Generation: 3,
		Actions:           actions(t, runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionResultSubmit),
		AuthorityRevision: 7, ContextDigest: runtimeauth.NonceDigest("ctx"), Context: []byte(`{"a":1}`),
	}
}

func newGrant(t *testing.T) runtimeauth.AdmissionGrant {
	t.Helper()
	g, err := runtimeauth.NewAdmissionGrant(provision(t), t0, policy)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func session(t *testing.T) runtimeauth.SessionID {
	t.Helper()
	s, err := runtimeauth.NewSessionID(nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want %v", err, target)
	}
}

func Test_grant_transitions(t *testing.T) {
	nonce := runtimeauth.NonceDigest("nnnnnnnnnnnnnnnnnnnnnn")
	other := runtimeauth.NonceDigest("mmmmmmmmmmmmmmmmmmmmmm")

	t.Run("ownership validation", func(t *testing.T) {
		bad := []func(*runtimeauth.Provision){
			func(p *runtimeauth.Provision) { p.Scope.TenantID = "" },
			func(p *runtimeauth.Provision) { p.Scope.ProjectID = "bad id" },
			func(p *runtimeauth.Provision) { p.RunID = "" },
			func(p *runtimeauth.Provision) { p.AttemptID = "a/b" },
			func(p *runtimeauth.Provision) { p.Job.Namespace = "Upper" },
			func(p *runtimeauth.Provision) { p.Job.Name = "" },
			func(p *runtimeauth.Provision) { p.CredentialDigest = runtimeauth.Digest{} },
			func(p *runtimeauth.Provision) { p.Generation = 0 },
			func(p *runtimeauth.Provision) { p.Actions = runtimeauth.ActionSet{} },
			func(p *runtimeauth.Provision) { p.AuthorityRevision = 0 },
			func(p *runtimeauth.Provision) { p.ContextDigest = runtimeauth.Digest{} },
			func(p *runtimeauth.Provision) { p.Context = nil },
			func(p *runtimeauth.Provision) { p.Context = make([]byte, runtimeauth.MaxContextBytes+1) },
		}
		for i, mutate := range bad {
			p := provision(t)
			mutate(&p)
			if _, err := runtimeauth.NewAdmissionGrant(p, t0, policy); !errors.Is(err, runtimeauth.ErrInvalidRequest) {
				t.Errorf("case %d accepted: %v", i, err)
			}
		}
		if _, err := runtimeauth.NewAdmissionGrant(provision(t), t0, runtimeauth.Policy{}); err == nil {
			t.Error("a zero policy was accepted")
		}
	})

	t.Run("first consumption fixes session, pod and maximum", func(t *testing.T) {
		g := newGrant(t)
		if g.Consumed() || g.Revoked() {
			t.Fatal("new grant is not pristine")
		}
		ses := session(t)
		now := t0.Add(time.Minute)
		adm, err := g.Consume(nonce, podA, ses, now, policy)
		if err != nil || adm.Replayed {
			t.Fatalf("consume: %+v %v", adm, err)
		}
		if adm.SessionID != ses || adm.Pod != podA || !adm.ConsumedAt.Equal(now.Truncate(time.Microsecond)) {
			t.Fatalf("consumption = %+v", adm)
		}
		if want := adm.ConsumedAt.Add(policy.MaxSession); !adm.MaxExpiresAt.Equal(want) {
			t.Fatalf("max = %v, want %v", adm.MaxExpiresAt, want)
		}
		if !g.Consumed() {
			t.Fatal("grant not marked consumed")
		}
	})

	t.Run("expired admission never consumes or resurrects", func(t *testing.T) {
		g := newGrant(t)
		_, err := g.Consume(nonce, podA, session(t), t0.Add(policy.AdmissionTTL), policy)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
		if g.Consumed() {
			t.Fatal("expired grant was consumed")
		}
		// Going back in time (clock skew, restored database) is the caller's
		// problem; the entity only compares against the time it is given, and
		// a window that has passed stays passed for every later time.
		_, err = g.Consume(nonce, podA, session(t), t0.Add(24*time.Hour), policy)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
	})

	t.Run("bounded replay", func(t *testing.T) {
		g := newGrant(t)
		first, err := g.Consume(nonce, podA, session(t), t0, policy)
		if err != nil {
			t.Fatal(err)
		}
		// Replay long after the admission window but inside the session.
		at := t0.Add(time.Hour)
		again, err := g.Consume(nonce, podA, session(t), at, policy)
		if err != nil || !again.Replayed {
			t.Fatalf("replay: %+v %v", again, err)
		}
		if again.SessionID != first.SessionID || !again.MaxExpiresAt.Equal(first.MaxExpiresAt) {
			t.Fatal("replay changed the session or moved the maximum")
		}
		_, err = g.Consume(nonce, podA, session(t), first.MaxExpiresAt, policy)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
	})

	t.Run("another nonce or another pod is refused", func(t *testing.T) {
		g := newGrant(t)
		if _, err := g.Consume(nonce, podA, session(t), t0, policy); err != nil {
			t.Fatal(err)
		}
		_, err := g.Consume(other, podA, session(t), t0, policy)
		mustIs(t, err, runtimeauth.ErrAdmissionConsumed)
		_, err = g.Consume(nonce, podB, session(t), t0, policy)
		mustIs(t, err, runtimeauth.ErrForbidden)
		if g.Consumption.Pod != podA {
			t.Fatal("a refused exchange changed the binding")
		}
	})

	t.Run("malformed pod identity is refused before consuming", func(t *testing.T) {
		g := newGrant(t)
		_, err := g.Consume(nonce, runtimeauth.PodBinding{JobUID: "x", PodName: "p", PodUID: "y"}, session(t), t0, policy)
		mustIs(t, err, runtimeauth.ErrForbidden)
		if g.Consumed() {
			t.Fatal("consumed with a malformed identity")
		}
	})

	t.Run("revocation is irreversible", func(t *testing.T) {
		g := newGrant(t)
		if !g.Revoke(t0) {
			t.Fatal("first revoke reported no change")
		}
		first := *g.RevokedAt
		if g.Revoke(t0.Add(time.Hour)) || !g.RevokedAt.Equal(first) {
			t.Fatal("a second revoke changed the grant")
		}
		_, err := g.Consume(nonce, podA, session(t), t0, policy)
		mustIs(t, err, runtimeauth.ErrAttemptRevoked)
		mustIs(t, g.Authorize(runtimeauth.ActionContextRead, t0), runtimeauth.ErrAttemptRevoked)

		// Revocation after consumption also ends replay and every action.
		h := newGrant(t)
		adm, _ := h.Consume(nonce, podA, session(t), t0, policy)
		h.Revoke(t0.Add(time.Minute))
		_, err = h.Consume(nonce, podA, session(t), t0.Add(2*time.Minute), policy)
		mustIs(t, err, runtimeauth.ErrAttemptRevoked)
		mustIs(t, h.Authorize(runtimeauth.ActionContextRead, t0.Add(2*time.Minute)), runtimeauth.ErrAttemptRevoked)
		_ = adm
	})

	t.Run("authorize narrows to the ceiling and the lifetime", func(t *testing.T) {
		g := newGrant(t)
		mustIs(t, g.Authorize(runtimeauth.ActionContextRead, t0), runtimeauth.ErrInvalidCredential) // no session yet
		adm, _ := g.Consume(nonce, podA, session(t), t0, policy)
		if err := g.Authorize(runtimeauth.ActionResultSubmit, t0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		mustIs(t, g.Authorize(runtimeauth.ActionSecretResolve, t0), runtimeauth.ErrForbidden)
		mustIs(t, g.Authorize(runtimeauth.Action("gate.approve"), t0), runtimeauth.ErrForbidden)
		mustIs(t, g.Authorize(runtimeauth.ActionResultSubmit, adm.MaxExpiresAt), runtimeauth.ErrInvalidCredential)
	})

	t.Run("token expiry is clamped to the maximum", func(t *testing.T) {
		g := newGrant(t)
		adm, _ := g.Consume(nonce, podA, session(t), t0, policy)
		exp, err := g.TokenExpiry(t0, policy.TokenTTL)
		if err != nil || exp.After(t0.Add(policy.TokenTTL)) || exp.Nanosecond() != 0 {
			t.Fatalf("expiry = %v %v", exp, err)
		}
		late := adm.MaxExpiresAt.Add(-90 * time.Second)
		exp, err = g.TokenExpiry(late, policy.TokenTTL)
		if err != nil || exp.After(adm.MaxExpiresAt) {
			t.Fatalf("clamped expiry = %v (max %v) %v", exp, adm.MaxExpiresAt, err)
		}
		_, err = g.TokenExpiry(adm.MaxExpiresAt.Add(-100*time.Millisecond), policy.TokenTTL)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
		_, err = g.TokenExpiry(adm.MaxExpiresAt, policy.TokenTTL)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
		u := newGrant(t)
		_, err = u.TokenExpiry(t0, policy.TokenTTL)
		mustIs(t, err, runtimeauth.ErrInvalidCredential)
	})

	t.Run("registration comparison ignores only expiry", func(t *testing.T) {
		p := provision(t)
		a, _ := runtimeauth.NewAdmissionGrant(p, t0, policy)
		b, _ := runtimeauth.NewAdmissionGrant(p, t0.Add(time.Hour), policy)
		if !a.SameRegistration(b) {
			t.Fatal("a retry with a later clock must be the same registration")
		}
		mutations := []func(*runtimeauth.Provision){
			func(p *runtimeauth.Provision) { p.Scope.ProjectID = "p019_other" },
			func(p *runtimeauth.Provision) { p.AttemptID = "att_02" },
			func(p *runtimeauth.Provision) { p.Job.Name = "other-job" },
			func(p *runtimeauth.Provision) { p.Generation++ },
			func(p *runtimeauth.Provision) { p.Actions = actions(t, runtimeauth.ActionContextRead) },
			func(p *runtimeauth.Provision) { p.AuthorityRevision++ },
			func(p *runtimeauth.Provision) { p.Context = []byte(`{"a":2}`) },
			func(p *runtimeauth.Provision) { p.CredentialDigest = runtimeauth.NonceDigest("other") },
		}
		for i, m := range mutations {
			q := p
			m(&q)
			c, err := runtimeauth.NewAdmissionGrant(q, t0, policy)
			if err != nil {
				t.Fatal(err)
			}
			if a.SameRegistration(c) {
				t.Errorf("mutation %d considered the same registration", i)
			}
		}
	})
}

func Test_action_set(t *testing.T) {
	for _, bad := range []string{"", "*", "context.*", "admin", "gate.approve", "review.decide", "tenant.admin", "Context.Read", "context.read ", "secret.resolve,token.renew"} {
		if _, err := runtimeauth.ParseAction(bad); !errors.Is(err, runtimeauth.ErrInvalidRequest) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := runtimeauth.NewActionSet(); err == nil {
		t.Error("an empty set was accepted")
	}
	if _, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead, runtimeauth.ActionContextRead); err == nil {
		t.Error("a duplicate was accepted")
	}
	if _, err := runtimeauth.ParseActionSet([]string{"context.read", "*"}); err == nil {
		t.Error("a wildcard was accepted")
	}
	small := actions(t, runtimeauth.ActionContextRead)
	big := actions(t, runtimeauth.ActionResultSubmit, runtimeauth.ActionContextRead)
	if !small.SubsetOf(big) || big.SubsetOf(small) || !big.Equal(actions(t, runtimeauth.ActionContextRead, runtimeauth.ActionResultSubmit)) {
		t.Error("set algebra is wrong")
	}
	// No operation maps to a human review or administrative authority, and
	// every runtime operation that needs authority maps to a known action.
	for _, op := range []string{"renewToken", "getContext", "resolveSecret", "reserveArtifact", "completeArtifact", "proposeSource", "submitResult"} {
		a, ok := runtimeauth.ActionForOperation(op)
		if !ok {
			t.Errorf("%s has no action", op)
		}
		if _, err := runtimeauth.ParseAction(string(a)); err != nil {
			t.Errorf("%s maps to an unknown action %q", op, a)
		}
	}
	if _, ok := runtimeauth.ActionForOperation("admit"); ok {
		t.Error("admission must not map to an action")
	}
	for _, forbidden := range []string{"review", "approve", "gate", "admin", "decide", "*"} {
		for _, name := range actions(t, runtimeauth.ActionContextRead, runtimeauth.ActionTokenRenew, runtimeauth.ActionSecretResolve, runtimeauth.ActionArtifactReserve,
			runtimeauth.ActionArtifactComplete, runtimeauth.ActionSourcePropose, runtimeauth.ActionResultSubmit, runtimeauth.ActionModelInvoke,
			runtimeauth.ActionToolInvoke, runtimeauth.ActionDeploymentCall).Strings() {
			if strings.Contains(name, forbidden) {
				t.Errorf("action %q contains %q", name, forbidden)
			}
		}
	}
}

func Test_policy_validation(t *testing.T) {
	ok := runtimeauth.Policy{AdmissionTTL: time.Minute, TokenTTL: time.Minute, MaxSession: time.Hour}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []runtimeauth.Policy{
		{},
		{AdmissionTTL: -time.Second, TokenTTL: time.Minute, MaxSession: time.Hour},
		{AdmissionTTL: 48 * time.Hour, TokenTTL: time.Minute, MaxSession: time.Hour},
		{AdmissionTTL: time.Minute, TokenTTL: 2 * time.Hour, MaxSession: 3 * time.Hour},
		{AdmissionTTL: time.Minute, TokenTTL: time.Hour, MaxSession: time.Minute},
		{AdmissionTTL: time.Minute, TokenTTL: time.Minute, MaxSession: 30 * 24 * time.Hour},
		{AdmissionTTL: time.Millisecond, TokenTTL: time.Minute, MaxSession: time.Hour},
	}
	for i, p := range bad {
		if p.Validate() == nil {
			t.Errorf("policy %d accepted: %+v", i, p)
		}
	}
	w := runtimeauth.ExactWrapperPolicy{Supported: []string{"1.4.0"}}
	if err := w.Check("1.4.0", ""); err != nil {
		t.Error(err)
	}
	if err := w.Check("1.4.0", "1.4.0"); err != nil {
		t.Error(err)
	}
	mustIs(t, w.Check("0.1.0", ""), runtimeauth.ErrProfileUnsupported)
	mustIs(t, w.Check("1.4.0", "1.5.0"), runtimeauth.ErrProfileUnsupported)
	mustIs(t, runtimeauth.ExactWrapperPolicy{}.Check("1.4.0", ""), runtimeauth.ErrProfileUnsupported)
}

func Test_credential_redaction_and_digest(t *testing.T) {
	a, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := runtimeauth.GenerateCredential(nil)
	if a.Reveal() == b.Reveal() || a.Digest() == b.Digest() {
		t.Fatal("credentials collide")
	}
	if _, err := runtimeauth.ParseCredential(a.Reveal()); err != nil {
		t.Fatalf("a generated credential does not satisfy the contract syntax: %v", err)
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 513), "adm_ has space xxxxxxxxxxxx"} {
		if _, err := runtimeauth.ParseCredential(bad); !errors.Is(err, runtimeauth.ErrInvalidCredential) {
			t.Errorf("%q accepted", bad)
		}
	}
	secret := a.Reveal()
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "cred", a, "ptr", &a)
	js, _ := json.Marshal(struct{ C runtimeauth.Credential }{a})
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", a), "%+v": fmt.Sprintf("%+v", a), "%#v": fmt.Sprintf("%#v", a), "%s": fmt.Sprintf("%s", a),
		"json": string(js), "slog": logs.String(), "ptr %v": fmt.Sprintf("%v", &a),
	} {
		if strings.Contains(out, secret) || strings.Contains(out, strings.TrimPrefix(secret, "adm_")) {
			t.Errorf("%s leaks the credential: %s", name, out)
		}
	}
	if runtimeauth.NonceDigest("n").Equal(a.Digest()) {
		t.Error("domain separation failed")
	}
	if _, err := runtimeauth.DigestFromBytes([]byte{1}); err == nil {
		t.Error("a short digest was accepted")
	}
	if s := session(t); s.Validate() != nil {
		t.Error("generated session is invalid")
	}
	if err := runtimeauth.SessionID("ses_short").Validate(); err == nil {
		t.Error("a short session was accepted")
	}
}
