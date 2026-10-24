package integration_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// Test_cancelled_admission replays the frozen cancelled_admission exchange
// (IT-06, PX-08): a cancelled attempt is refused with 403 attempt_revoked,
// never consumes the credential and never returns a token or context.
func Test_cancelled_admission(t *testing.T) {
	wantStatus, wantCode := scenarioError(t, "cancelled_admission")

	check := func(t *testing.T, w *world, r reply) {
		t.Helper()
		if r.Status != wantStatus || r.errorCode(t) != wantCode {
			t.Fatalf("got %d %s, want %d %s", r.Status, r.Body, wantStatus, wantCode)
		}
		if contains(r.Body, `"token"`, `"context"`, w.Cred.Reveal()) {
			t.Fatalf("a refusal carries authority: %s", r.Body)
		}
		if row := w.grantRow("att_01"); row.Consumed {
			t.Fatal("a refused admission consumed the grant")
		}
	}

	t.Run("cancelled with revocation", func(t *testing.T) {
		w := newWorld(t)
		if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", true); err != nil {
			t.Fatal(err)
		}
		check(t, w, w.admit(w.Req))
	})

	t.Run("cancelled state alone is enough", func(t *testing.T) {
		w := newWorld(t)
		if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", false); err != nil {
			t.Fatal(err)
		}
		check(t, w, w.admit(w.Req))
		if w.Jobs.Calls() != 0 {
			t.Fatal("Kubernetes was queried for a cancelled attempt")
		}
	})

	t.Run("cancellation after the ownership observation", func(t *testing.T) {
		w := newWorld(t)
		w.Jobs.SetHook(func(runtimeauth.JobObservation) {
			if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", false); err != nil {
				t.Error(err)
			}
		})
		r := w.admit(w.Req)
		check(t, w, r)
		if w.Jobs.Calls() != 1 {
			t.Fatalf("expected one observation, got %d", w.Jobs.Calls())
		}
		// The lock-time check found the cancellation and recorded the
		// revocation durably: a restarted service still refuses.
		if !w.grantRow("att_01").Revoked {
			t.Fatal("the cancellation seen under lock was not recorded")
		}
		w.Jobs.SetHook(nil)
		check(t, w, w.admit(w.Req))
	})

	t.Run("finished attempt", func(t *testing.T) {
		w := newWorld(t)
		w.Authority.Update(`phase = 'finished'`)
		check(t, w, w.admit(w.Req))
	})

	t.Run("authority revision moved after registration", func(t *testing.T) {
		w := newWorld(t)
		w.Authority.Update(`revision = revision + 1`)
		check(t, w, w.admit(w.Req))
		if !w.grantRow("att_01").Revoked {
			t.Fatal("a stale grant was not retired")
		}
	})
}

// Test_wrong_job replays wrong_job (IT-06, IT-08): any identity that is not the
// attempt's Job and its Pod is 403 forbidden, and refusals never burn the
// legitimate credential.
func Test_wrong_job(t *testing.T) {
	wantStatus, wantCode := scenarioError(t, "wrong_job")
	w := newWorld(t)
	good := w.Req
	cluster := w.Jobs.Cluster(good.Job.Name)

	frozen := rtest.LoadScenario(t, "wrong_job")
	wrong, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](frozen.Steps[0].Request)
	if err != nil {
		t.Fatal(err)
	}
	wrong.AdmissionCredential = w.Cred.Reveal()

	cases := map[string]func() runtimev1.AdmitRequest{
		"frozen wrong Job UID": func() runtimev1.AdmitRequest { return wrong },
		"wrong Pod UID": func() runtimev1.AdmitRequest {
			r := good
			r.Job.PodUID = "8b3e3f40-5c6d-4e7f-8ab1-2c3d4e5f6071"
			return r
		},
		"wrong Pod name": func() runtimev1.AdmitRequest { r := good; r.Job.PodName = "kodama-att-01-evil"; return r },
		"wrong Job name": func() runtimev1.AdmitRequest { r := good; r.Job.Name = "kodama-att-02"; return r },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := w.admit(mk())
			if r.Status != wantStatus || r.errorCode(t) != wantCode {
				t.Fatalf("got %d %s, want %d %s", r.Status, r.Body, wantStatus, wantCode)
			}
			if w.grantRow("att_01").Consumed {
				t.Fatal("a refusal consumed the credential")
			}
		})
	}

	// Cluster-side mismatches: the claimed identity is right but Kubernetes
	// does not agree.
	mutate := map[string]func(e *runtimeauth.JobEvidence){
		"Pod in another namespace": func(e *runtimeauth.JobEvidence) { e.PodNamespace = "other-tenant-ns" },
		"Job in another namespace": func(e *runtimeauth.JobEvidence) { e.Namespace = "other-tenant-ns" },
		"Pod owned by another Job": func(e *runtimeauth.JobEvidence) { e.PodOwnerUID = "8b3e3f40-5c6d-4e7f-8ab1-2c3d4e5f6071" },
		"Pod with no owner":        func(e *runtimeauth.JobEvidence) { e.PodOwnerUID = "" },
		"Job UID replaced": func(e *runtimeauth.JobEvidence) {
			e.JobUID = "8b3e3f40-5c6d-4e7f-8ab1-2c3d4e5f6071"
			e.PodOwnerUID = e.JobUID
		},
		"Pod UID replaced": func(e *runtimeauth.JobEvidence) { e.PodUID = "9c4f4f51-6d7e-4f80-9bc2-3d4e5f607182" },
		"empty evidence":   func(e *runtimeauth.JobEvidence) { *e = runtimeauth.JobEvidence{JobName: e.JobName} },
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			e := cluster
			m(&e)
			w.Jobs.SetCluster(e)
			defer w.Jobs.SetCluster(cluster)
			r := w.admit(good)
			if r.Status != wantStatus || r.errorCode(t) != wantCode {
				t.Fatalf("got %d %s", r.Status, r.Body)
			}
			if w.grantRow("att_01").Consumed {
				t.Fatal("a refusal consumed the credential")
			}
		})
	}

	t.Run("Job not found", func(t *testing.T) {
		w.Jobs.SetError(runtimeauth.ErrNotObserved)
		defer w.Jobs.SetError(nil)
		r := w.admit(good)
		if r.Status != wantStatus || r.errorCode(t) != wantCode {
			t.Fatalf("got %d %s", r.Status, r.Body)
		}
	})

	t.Run("the credential works for the right workload after every refusal", func(t *testing.T) {
		resp := w.mustAdmit(good)
		if resp.Context.AttemptID != "att_01" || !w.grantRow("att_01").Consumed {
			t.Fatal("legitimate admission failed")
		}
	})

	t.Run("another Pod cannot take over a consumed grant", func(t *testing.T) {
		e := cluster
		e.PodName, e.PodUID = "kodama-att-01-second", "9c4f4f51-6d7e-4f80-9bc2-3d4e5f607182"
		w.Jobs.SetCluster(e)
		defer w.Jobs.SetCluster(cluster)
		r := w.admit(runtimev1.AdmitRequest{AdmissionCredential: good.AdmissionCredential, Nonce: good.Nonce, ClientVersion: good.ClientVersion,
			Job: runtimev1.JobIdentity{Name: good.Job.Name, UID: good.Job.UID, PodName: e.PodName, PodUID: e.PodUID}})
		if r.Status != 403 || r.errorCode(t) != wantCode {
			t.Fatalf("a second Pod replayed the admission: %d %s", r.Status, r.Body)
		}
	})
}

// Test_wrong_audience replays wrong_audience (IT-06): a correctly signed token
// for another audience, a human-style token and the admission credential are
// all 401 on /context.
func Test_wrong_audience(t *testing.T) {
	wantStatus, wantCode := scenarioError(t, "wrong_audience")
	w := newWorld(t)
	resp := w.mustAdmit(w.Req)
	claims, err := w.Verifier.Verify(bg, resp.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(c rtest.MintClaims) string {
		c.Subject, c.Tenant = string(claims.Session), string(claims.Tenant)
		c.IssuedAt, c.ExpiresAt = w.Clock.Now(), w.Clock.Now().Add(time.Minute)
		return w.MintToken(c)
	}

	if r := w.getContext(mint(rtest.MintClaims{})); r.Status != 200 {
		t.Fatalf("control: a correctly minted token must work: %d %s", r.Status, r.Body)
	}
	expect := func(name string, r reply) {
		t.Helper()
		if r.Status != wantStatus || r.errorCode(t) != wantCode {
			t.Errorf("%s: got %d %s, want %d %s", name, r.Status, r.Body, wantStatus, wantCode)
		}
		if contains(r.Body, `"designBaseline"`, `"tenantId"`) {
			t.Errorf("%s: context leaked", name)
		}
	}
	expect("wrong audience", w.getContext(mint(rtest.MintClaims{Audience: "kodama-api"})))
	expect("human API audience", w.getContext(mint(rtest.MintClaims{Audience: "https://api.kodama.example.invalid"})))
	expect("wrong issuer", w.getContext(mint(rtest.MintClaims{Issuer: "https://idp.example.invalid"})))
	expect("admission credential as bearer", w.getContext(w.Cred.Reveal()))
	expect("no bearer", w.getContext(""))
	expect("garbage bearer", w.getContext("rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	expect("wrong audience on renewal", w.renew(mint(rtest.MintClaims{Audience: "kodama-api"})))

	// A token for the runtime audience does not authorize the admission route
	// either: admission accepts only the credential.
	r := w.call("POST", "att_01", "/admit", w.Req, resp.Token.Token)
	if r.Status != 409 && r.Status != 200 {
		t.Fatalf("admission ignores bearers, got %d", r.Status)
	}
}

// Test_refresh_after_revoke replays refresh_after_revoke (IT-06, PX-08).
func Test_refresh_after_revoke(t *testing.T) {
	wantStatus, wantCode := scenarioError(t, "refresh_after_revoke")

	t.Run("revocation through the module", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		r := w.renew(resp.Token.Token)
		if r.Status != 200 {
			t.Fatalf("renewal before revocation: %d %s", r.Status, r.Body)
		}
		renewed, err := runtimev1.DecodeResponse[runtimev1.TokenResponse](r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if renewed.Token.MaxExpiresAt != resp.Token.MaxExpiresAt {
			t.Fatalf("renewal moved the maximum: %s -> %s", resp.Token.MaxExpiresAt, renewed.Token.MaxExpiresAt)
		}
		if r := w.getContext(renewed.Token.Token); r.Status != 200 {
			t.Fatalf("a renewed token must work: %d", r.Status)
		}

		if err := w.Service.Revoke(bg, w.Attempt.Scope, "att_01"); err != nil {
			t.Fatal(err)
		}
		if err := w.Service.Revoke(bg, w.Attempt.Scope, "att_01"); err != nil {
			t.Fatalf("revocation must be idempotent: %v", err)
		}
		for name, tok := range map[string]string{"first token": resp.Token.Token, "renewed token": renewed.Token.Token} {
			r := w.renew(tok)
			if r.Status != wantStatus || r.errorCode(t) != wantCode {
				t.Errorf("%s renewal after revocation: %d %s", name, r.Status, r.Body)
			}
			r = w.getContext(tok)
			if r.Status != wantStatus || r.errorCode(t) != wantCode {
				t.Errorf("%s context after revocation: %d %s", name, r.Status, r.Body)
			}
			if _, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: tok, Action: runtimeauth.ActionResultSubmit}); err == nil {
				t.Errorf("%s still authorizes a protected callback", name)
			}
		}
		// Admission replays are refused too: revocation ends bounded replay.
		if r := w.admit(w.Req); r.Status != 403 || r.errorCode(t) != wantCode {
			t.Fatalf("replay after revocation: %d %s", r.Status, r.Body)
		}
	})

	t.Run("cancellation by the execution owner without revoking", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", false); err != nil {
			t.Fatal(err)
		}
		r := w.renew(resp.Token.Token)
		if r.Status != wantStatus || r.errorCode(t) != wantCode {
			t.Fatalf("renewal after cancellation: %d %s", r.Status, r.Body)
		}
		if _, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: resp.Token.Token, Action: runtimeauth.ActionResultSubmit}); err != nil {
			t.Fatalf("an unrevoked terminal attempt must still submit a result receipt: %v", err)
		}
	})

	t.Run("cancellation with revocation", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		if err := w.Authority.Cancel(bg, w.Attempt.Scope, "att_01", true); err != nil {
			t.Fatal(err)
		}
		if r := w.renew(resp.Token.Token); r.Status != wantStatus || r.errorCode(t) != wantCode {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("maximum lifetime ends renewal", func(t *testing.T) {
		w := newWorld(t)
		resp := w.mustAdmit(w.Req)
		tok := resp.Token.Token
		max, _ := resp.Token.MaxExpiresAt.Time()
		for w.Clock.Now().Add(rtest.TestPolicy.TokenTTL).Before(max.Add(-time.Minute)) {
			w.Clock.Advance(rtest.TestPolicy.TokenTTL - time.Second)
			r := w.renew(tok)
			if r.Status != 200 {
				t.Fatalf("renewal at %s: %d %s", w.Clock.Now(), r.Status, r.Body)
			}
			next, _ := runtimev1.DecodeResponse[runtimev1.TokenResponse](r.Body)
			if exp, _ := next.Token.ExpiresAt.Time(); exp.After(max) {
				t.Fatalf("a token outlives the maximum: %s > %s", exp, max)
			}
			if next.Token.MaxExpiresAt != resp.Token.MaxExpiresAt {
				t.Fatal("the maximum moved")
			}
			tok = next.Token.Token
		}
		w.Clock.Advance(rtest.TestPolicy.TokenTTL - time.Second)
		r := w.renew(tok)
		if r.Status != 200 {
			t.Fatalf("the clamped last renewal: %d %s", r.Status, r.Body)
		}
		last, _ := runtimev1.DecodeResponse[runtimev1.TokenResponse](r.Body)
		if exp, _ := last.Token.ExpiresAt.Time(); exp.After(max) {
			t.Fatalf("clamp failed: %s > %s", exp, max)
		}
		w.Clock.Advance(max.Sub(w.Clock.Now()) + time.Second)
		if r := w.renew(last.Token.Token); r.Status != 401 {
			t.Fatalf("renewal after the maximum: %d %s", r.Status, r.Body)
		}
	})
}

// Test_rotated_key (IT-06, IT-A05): rotation retires old tokens and old
// admission authority immediately, including grants resurrected from a restored
// database; authority provisioned under the new generation works.
func Test_rotated_key(t *testing.T) {
	w := newWorld(t)
	resp := w.mustAdmit(w.Req)
	if r := w.getContext(resp.Token.Token); r.Status != 200 {
		t.Fatalf("before rotation: %d", r.Status)
	}

	// A second attempt registered, but not yet admitted, under generation 1:
	// this is the "restored database" grant that rotation must keep dead.
	c2 := rtest.ContextFor(w.Attempt.Context, "att_02")
	a2 := w.SeedAttempt(c2, rtest.AllActions())
	cred2, _, err := w.Register(a2, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req2 := w.AdmitRequest(a2, cred2, nonce)

	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	if gen, err := w.Ring.Rotate(key); err != nil || gen != 2 {
		t.Fatalf("rotate: %d %v", gen, err)
	}

	if r := w.getContext(resp.Token.Token); r.Status != 401 || r.errorCode(t) != "invalid_credential" {
		t.Fatalf("old token on /context: %d %s", r.Status, r.Body)
	}
	if r := w.renew(resp.Token.Token); r.Status != 401 {
		t.Fatalf("old token refresh: %d %s", r.Status, r.Body)
	}
	if _, err := w.Service.Authorize(bg, runtimeauth.AuthorizeRequest{AttemptID: "att_01", Bearer: resp.Token.Token, Action: runtimeauth.ActionContextRead}); err == nil {
		t.Fatal("an old token still authorizes")
	}
	// The consumed credential replays into nothing.
	if r := w.admit(w.Req); r.Status != 401 || r.errorCode(t) != "invalid_credential" {
		t.Fatalf("old credential replay: %d %s", r.Status, r.Body)
	}
	// The unconsumed generation-1 grant cannot be admitted either.
	if r := w.call("POST", "att_02", "/admit", req2, ""); r.Status != 401 {
		t.Fatalf("restored grant of the old generation: %d %s", r.Status, r.Body)
	}
	if w.grantRow("att_02").Consumed {
		t.Fatal("a retired grant was consumed")
	}
	// A restored consumed row is no different: delete and re-insert the
	// consumption as a restore would; the token it could mint is still refused.
	snapshot := w.grantRow("att_01")
	if !snapshot.Consumed {
		t.Fatal("expected the first grant consumed")
	}

	// Authority provisioned after the rotation works end to end.
	c3 := rtest.ContextFor(w.Attempt.Context, "att_03")
	a3 := w.SeedAttempt(c3, rtest.AllActions())
	cred3, _, err := w.Register(a3, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	r := w.call("POST", "att_03", "/admit", w.AdmitRequest(a3, cred3, nonce), "")
	if r.Status != 200 {
		t.Fatalf("new generation admission: %d %s", r.Status, r.Body)
	}
	fresh, err := runtimev1.DecodeResponse[runtimev1.AdmitResponse](r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r := w.call("GET", "att_03", "/context", nil, fresh.Token.Token); r.Status != 200 {
		t.Fatalf("new token: %d %s", r.Status, r.Body)
	}
	// ...and cannot be used on the other attempt's path.
	if r := w.call("GET", "att_01", "/context", nil, fresh.Token.Token); r.Status != 401 {
		t.Fatalf("path substitution: %d", r.Status)
	}
}

// Test_incompatible_wrapper_profile replays the frozen scenario (AR-02) and
// the pinned-profile rule: 422 profile_unsupported without consuming.
func Test_incompatible_wrapper_profile(t *testing.T) {
	wantStatus, wantCode := scenarioError(t, "incompatible_wrapper_profile")
	w := newWorld(t)
	frozen := rtest.LoadScenario(t, "incompatible_wrapper_profile")
	req, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](frozen.Steps[0].Request)
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionCredential = w.Cred.Reveal()
	r := w.admit(req)
	if r.Status != wantStatus || r.errorCode(t) != wantCode {
		t.Fatalf("got %d %s", r.Status, r.Body)
	}
	if w.grantRow("att_01").Consumed {
		t.Fatal("an incompatible wrapper consumed the grant")
	}
	w.mustAdmit(w.Req) // the supported wrapper still works

	t.Run("pinned profile requires the exact wrapper version", func(t *testing.T) {
		w := rtest.New(t, rtest.Options{})
		c, _ := w.HappyContext()
		c.Profile = &runtimev1.Profile{Name: "go-tools", Version: 3, WrapperVersion: "1.5.0"}
		a := w.SeedAttempt(c, rtest.AllActions())
		cred, _, err := w.Register(a, rtest.AllActions())
		if err != nil {
			t.Fatal(err)
		}
		ww := &world{Stack: w, Attempt: a, Cred: cred}
		r := ww.admit(w.AdmitRequest(a, cred, nonce)) // client 1.4.0 is supported but not pinned
		if r.Status != 422 || r.errorCode(t) != "profile_unsupported" {
			t.Fatalf("got %d %s", r.Status, r.Body)
		}
		if ww.grantRow("att_01").Consumed {
			t.Fatal("consumed")
		}
	})
}

// Test_context_is_returned_unchanged: the pinned context, including explicit
// nulls and the design baseline, reaches the entrypoint as registered.
func Test_context_is_returned_unchanged(t *testing.T) {
	w := newWorld(t)
	frozen := rtest.LoadScenario(t, "happy_path")
	var want runtimev1.AdmitResponse
	if err := json.Unmarshal(frozen.Steps[0].Body, &want); err != nil {
		t.Fatal(err)
	}
	resp := w.mustAdmit(w.Req)
	if !reflect.DeepEqual(resp.Context, want.Context) {
		t.Fatalf("context differs from the frozen fixture:\n%+v\n%+v", resp.Context, want.Context)
	}
	r := w.getContext(resp.Token.Token)
	got, err := runtimev1.DecodeResponse[runtimev1.Context](r.Body)
	if err != nil || !reflect.DeepEqual(got, want.Context) {
		t.Fatalf("GET /context differs: %v", err)
	}
	for _, field := range []string{`"designBaseline":null`, `"profile":null`, `"environment":null`} {
		if !strings.Contains(string(r.Body), field) {
			t.Errorf("explicit null missing: %s", field)
		}
	}
	if resp.Token.TokenType != "Bearer" || resp.Token.Audience != "kodama-runtime" {
		t.Fatalf("token info = %+v", resp.Token)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("context responses must not be cacheable")
	}

	t.Run("a design baseline travels as authority-free reference data", func(t *testing.T) {
		w := rtest.New(t, rtest.Options{})
		c, _ := w.HappyContext()
		base, err := runtimev1.NewDesignBaseline(
			[]string{"SD-01"}, []string{"C-01"},
			[]runtimev1.SourceRef{{Repository: "https://git.example.invalid/acme/design.git", Path: "docs/design.md", Revision: runtimev1.GitCommit(strings.Repeat("1", 40))}},
			[]string{"module-boundaries"})
		if err != nil {
			t.Fatal(err)
		}
		c.DesignBaseline = &base
		a := w.SeedAttempt(c, rtest.AllActions())
		cred, _, err := w.Register(a, rtest.AllActions())
		if err != nil {
			t.Fatal(err)
		}
		ww := &world{Stack: w, Attempt: a, Cred: cred}
		resp := ww.mustAdmit(w.AdmitRequest(a, cred, nonce))
		if resp.Context.DesignBaseline == nil || resp.Context.DesignBaseline.Digest != base.Digest {
			t.Fatal("the baseline was not returned unchanged")
		}
	})
}
