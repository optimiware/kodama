package rtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

var (
	resScope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	resRun   = domain.RunID("run_01")
	prepUID  = "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"
	prepPod  = "kr-0123456789abcdef01234567-x7k2p"
	prepPodU = "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60"
	svcUID   = "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071"
)

type resEnv struct {
	*rtest.ResourceMemStack
	t        *testing.T
	deadline time.Time
	prep     runtimeauth.ResourceRef
	svc      runtimeauth.ResourceRef
	svcOther runtimeauth.ResourceRef
}

func newResEnv(t *testing.T) *resEnv {
	t.Helper()
	m := rtest.NewResourceMemStack(t)
	e := &resEnv{ResourceMemStack: m, t: t, deadline: m.Clock.Now().Add(time.Hour)}
	f := func(uid string) rtest.ResourceFixture {
		return rtest.ResourceFixture{Scope: resScope, RunID: resRun, WorkloadUID: uid, DeadlineAt: e.deadline}
	}
	binding := runtimev1.SecretBinding{Ref: "secret://project/test-db-password", Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile}
	e.prep = runtimeauth.ResourceRef{Scope: resScope, RunID: resRun, Key: "preparation"}
	e.svc = runtimeauth.ResourceRef{Scope: resScope, RunID: resRun, Key: "service.db"}
	e.svcOther = runtimeauth.ResourceRef{Scope: resScope, RunID: resRun, Key: "service.cache"}
	m.Resources.Put(rtest.PreparationState(f(prepUID)))
	m.Resources.Put(rtest.ServiceState(f(svcUID), "db", []runtimev1.SecretBinding{binding}))
	m.Resources.Put(rtest.ServiceState(f("9c4f4051-6d7e-4f80-9b12-3d4e5f607182"), "cache", nil))
	m.Resources.BindPod(prepUID, prepPod, prepPodU)
	return e
}

func (e *resEnv) register(ref runtimeauth.ResourceRef) runtimeauth.Credential {
	e.t.Helper()
	st, err := e.Resources.ObserveResource(context.Background(), ref)
	if err != nil {
		e.t.Fatal(err)
	}
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.Service.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{
		Scope: ref.Scope, RunID: ref.RunID, Key: ref.Key, Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
	}); err != nil {
		e.t.Fatalf("register %s: %v", ref.Key, err)
	}
	return cred
}

func (e *resEnv) identity(ref runtimeauth.ResourceRef) runtimev2.ResourceIdentity {
	st, _ := e.Resources.ObserveResource(context.Background(), ref)
	id := runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}
	if ref.Key == "preparation" {
		n, u := prepPod, prepPodU
		id.PodName, id.PodUID = &n, &u
	}
	return id
}

func (e *resEnv) admitReq(cred runtimeauth.Credential, nonce string, id runtimev2.ResourceIdentity) runtimev2.ResourceAdmitRequest {
	return runtimev2.ResourceAdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: nonce, ClientVersion: "1.4.0", Identity: id}
}

func (e *resEnv) admit(ref runtimeauth.ResourceRef, cred runtimeauth.Credential, nonce string) (runtimev2.ResourceAdmitResponse, error) {
	return e.Service.AdmitResource(context.Background(), ref, e.admitReq(cred, nonce, e.identity(ref)))
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

const nonce1 = "nnnnnnnnnnnnnnnnnnnnnn"

func Test_unit_runtimeauth_resource_admission_and_replay(t *testing.T) {
	e := newResEnv(t)
	ctx := context.Background()
	st, _ := e.Resources.ObserveResource(ctx, e.prep)

	// Registration: idempotent for the same credential, conflicting otherwise.
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := runtimeauth.ResourceRegisterCommand{Scope: resScope, RunID: resRun, Key: "preparation", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}
	if err := e.Service.RegisterResource(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	if err := e.Service.RegisterResource(ctx, cmd); err != nil {
		t.Fatalf("an identical registration after a lost response must succeed: %v", err)
	}
	other, _ := runtimeauth.GenerateCredential(nil)
	c2 := cmd
	c2.Credential = other
	is(t, e.Service.RegisterResource(ctx, c2), runtimeauth.ErrConflict)
	c3 := cmd
	c3.SpecFingerprint = "sha256:" + strings.Repeat("b", 64)
	is(t, e.Service.RegisterResource(ctx, c3), runtimeauth.ErrConflict)
	c4 := cmd
	c4.ExpiresAt = st.DeadlineAt.Add(time.Second)
	is(t, e.Service.RegisterResource(ctx, c4), runtimeauth.ErrConflict)
	c5 := cmd
	c5.Key = "workspace"
	is(t, e.Service.RegisterResource(ctx, c5), runtimeauth.ErrInvalidRequest)
	c6 := cmd
	c6.Credential = runtimeauth.Credential{}
	is(t, e.Service.RegisterResource(ctx, c6), runtimeauth.ErrInvalidRequest)
	// The same credential cannot be registered for another resource.
	c7 := runtimeauth.ResourceRegisterCommand{Scope: resScope, RunID: resRun, Key: "service.db", Credential: cred, SpecFingerprint: rtest.Fingerprint, ExpiresAt: st.DeadlineAt}
	is(t, e.Service.RegisterResource(ctx, c7), runtimeauth.ErrConflict)

	// Not yet bound: resource_unbound, nothing consumed.
	claimed := e.identity(e.prep)
	e.Resources.Modify(e.prep, func(s *runtimeauth.ResourceState) { s.WorkloadUID = "" })
	_, err = e.Service.AdmitResource(ctx, e.prep, e.admitReq(cred, nonce1, claimed))
	is(t, err, runtimeauth.ErrResourceUnbound)
	e.Resources.Modify(e.prep, func(s *runtimeauth.ResourceState) { s.WorkloadUID = prepUID })

	// A claim that is not the committed workload is forbidden and consumes nothing.
	bad := e.identity(e.prep)
	bad.WorkloadUID = "9c4f4051-6d7e-4f80-9b12-3d4e5f607182"
	_, err = e.Service.AdmitResource(ctx, e.prep, e.admitReq(cred, nonce1, bad))
	is(t, err, runtimeauth.ErrForbidden)
	bad = e.identity(e.prep)
	bad.WorkloadName = "kr-other"
	_, err = e.Service.AdmitResource(ctx, e.prep, e.admitReq(cred, nonce1, bad))
	is(t, err, runtimeauth.ErrForbidden)
	// A Pod the backend does not confirm for the committed UID is forbidden.
	bad = e.identity(e.prep)
	other2 := "kr-other-pod"
	bad.PodName = &other2
	_, err = e.Service.AdmitResource(ctx, e.prep, e.admitReq(cred, nonce1, bad))
	is(t, err, runtimeauth.ErrForbidden)
	// An inconclusive Pod observation is unavailable, never a pass.
	e.Resources.SetPodError(errors.New("api outage"))
	_, err = e.admit(e.prep, cred, nonce1)
	is(t, err, runtimeauth.ErrUnavailable)
	e.Resources.SetPodError(nil)
	if g, _ := e.Resources.Grant(e.prep); g.Consumed() || g.Revoked() {
		t.Fatalf("a denied admission changed the grant: %+v", g)
	}
	// An incompatible client version is profile_unsupported.
	req := e.admitReq(cred, nonce1, e.identity(e.prep))
	req.ClientVersion = "0.0.1"
	_, err = e.Service.AdmitResource(ctx, e.prep, req)
	is(t, err, runtimeauth.ErrProfileUnsupported)
	// The route must be the credential's resource.
	_, err = e.Service.AdmitResource(ctx, e.svc, e.admitReq(cred, nonce1, e.identity(e.svc)))
	is(t, err, runtimeauth.ErrInvalidCredential)

	// First admission.
	first, err := e.admit(e.prep, cred, nonce1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token.Audience != runtimev2.ResourceAudience || first.Token.Token == "" {
		t.Fatalf("token: %+v", first.Token)
	}
	maxAt, _ := first.Token.MaxExpiresAt.Time()
	if !maxAt.Equal(e.deadline.Truncate(time.Microsecond)) {
		t.Fatalf("maxExpiresAt %s is not the resource deadline %s (the policy session is longer)", maxAt, e.deadline)
	}
	if first.Context.ResourceKind != runtimev2.ResourcePreparation || first.Context.Preparation == nil || first.Context.Preparation.Commit != "1111111111111111111111111111111111111111" {
		t.Fatalf("context: %+v", first.Context)
	}

	// Lost response: the same credential and nonce returns a fresh token under the
	// same maximum.
	e.Clock.Advance(time.Minute)
	replay, err := e.admit(e.prep, cred, nonce1)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Token.MaxExpiresAt != first.Token.MaxExpiresAt || replay.Token.Token == first.Token.Token {
		t.Fatalf("replay must refresh the token under the same maximum: %+v vs %+v", replay.Token, first.Token)
	}
	// Another nonce after consumption is admission_consumed.
	_, err = e.admit(e.prep, cred, "mmmmmmmmmmmmmmmmmmmmmm")
	is(t, err, runtimeauth.ErrAdmissionConsumed)
	// The same nonce from a different instance is forbidden.
	e.Resources.BindPod(prepUID, "kr-0123456789abcdef01234567-zz9", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f61")
	alt := e.identity(e.prep)
	a, b := "kr-0123456789abcdef01234567-zz9", "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f61"
	alt.PodName, alt.PodUID = &a, &b
	_, err = e.Service.AdmitResource(ctx, e.prep, e.admitReq(cred, nonce1, alt))
	is(t, err, runtimeauth.ErrForbidden)

	// Renewal: bound to the session maximum, never moved.
	renewed, err := e.Service.RenewResource(ctx, resRun, "preparation", replay.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Token.MaxExpiresAt != first.Token.MaxExpiresAt || renewed.Token.Audience != runtimev2.ResourceAudience {
		t.Fatalf("renewal: %+v", renewed.Token)
	}
	// The context route returns the committed context.
	got, err := e.Service.ResourceContext(ctx, resRun, "preparation", renewed.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Preparation == nil || got.Preparation.RepositoryURL != first.Context.Preparation.RepositoryURL {
		t.Fatalf("context: %+v", got)
	}

	// The credential after its admission window never opens a new session.
	svcCred := e.register(e.svc)
	e.Clock.Advance(11 * time.Minute)
	_, err = e.admit(e.svc, svcCred, nonce1)
	is(t, err, runtimeauth.ErrInvalidCredential)
}

func Test_unit_runtimeauth_resource_audiences_are_disjoint(t *testing.T) {
	e := newResEnv(t)
	ctx := context.Background()

	prepCred, svcCred, otherCred := e.register(e.prep), e.register(e.svc), e.register(e.svcOther)
	prep, err := e.admit(e.prep, prepCred, nonce1)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := e.admit(e.svc, svcCred, nonce1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.admit(e.svcOther, otherCred, nonce1)
	if err != nil {
		t.Fatal(err)
	}

	// An attempt token (audience kodama-runtime) from the real attempt flow.
	sc := rtest.LoadV2Scenario(t, "begin_clean_then_result_hash_is_the_v1_hash")
	c2 := rtest.ContextV2Of(t, sc.Steps[0])
	acts, err := runtimeauth.NewActionSet(runtimeauth.DefaultActions(false)...)
	if err != nil {
		t.Fatal(err)
	}
	att := e.SeedAttemptV2(c2, acts)
	acred, _ := runtimeauth.GenerateCredential(nil)
	if _, err := e.RegisterV2With(att, c2, acred, acts); err != nil {
		t.Fatal(err)
	}
	areq := e.AdmitRequest(att, acred, nonce1)
	attAdmit, err := e.Service.AdmitV2(ctx, c2.AttemptID, areq)
	if err != nil {
		t.Fatal(err)
	}
	if attAdmit.Token.Audience != runtimev1.Audience {
		t.Fatalf("the attempt token audience is %s", attAdmit.Token.Audience)
	}
	attTok := attAdmit.Token.Token

	// attempt token on every resource route: forbidden.
	for _, op := range []runtimeauth.ResourceOp{runtimeauth.OpResourceRenew, runtimeauth.OpResourceContext, runtimeauth.OpResourceSecrets, runtimeauth.OpResourceComplete} {
		for _, key := range []string{"preparation", "service.db"} {
			_, err := e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: key, Bearer: attTok, Op: op})
			is(t, err, runtimeauth.ErrForbidden)
		}
	}
	_, err = e.Service.RenewResource(ctx, resRun, "service.db", attTok)
	is(t, err, runtimeauth.ErrForbidden)
	_, err = e.Service.ResourceContext(ctx, resRun, "service.db", attTok)
	is(t, err, runtimeauth.ErrForbidden)

	// resource token on every attempt route (both wires): forbidden/never accepted.
	for _, tok := range []string{prep.Token.Token, svc.Token.Token} {
		_, err = e.Service.ContextV2(ctx, c2.AttemptID, tok)
		is(t, err, runtimeauth.ErrForbidden)
		_, err = e.Service.RenewV2(ctx, c2.AttemptID, tok)
		is(t, err, runtimeauth.ErrForbidden)
		for _, wire := range []int64{runtimeauth.WireV1, runtimeauth.WireV2} {
			_, err = e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: c2.AttemptID, Bearer: tok, Action: runtimeauth.ActionSourcePropose, ContextVersion: wire})
			if err == nil {
				t.Fatal("a resource token authorized an attempt action")
			}
			_, err = e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: c2.AttemptID, Bearer: tok, Action: runtimeauth.ActionResultSubmit, ContextVersion: wire})
			if err == nil {
				t.Fatal("a resource token authorized an attempt result")
			}
		}
		_, err = e.Service.Context(ctx, c2.AttemptID, tok)
		if err == nil {
			t.Fatal("a resource token read an attempt context on the v1 wire")
		}
	}

	// A preparation token has no secrets route, a service token no preparation
	// route, and each is limited to its own resource's routes.
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "preparation", Bearer: prep.Token.Token, Op: runtimeauth.OpResourceSecrets})
	is(t, err, runtimeauth.ErrForbidden)
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "service.db", Bearer: svc.Token.Token, Op: runtimeauth.OpResourceComplete})
	is(t, err, runtimeauth.ErrForbidden)
	for _, op := range []runtimeauth.ResourceOp{runtimeauth.OpResourceRenew, runtimeauth.OpResourceContext, runtimeauth.OpResourceComplete} {
		if _, err := e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "preparation", Bearer: prep.Token.Token, Op: op}); err != nil {
			t.Fatalf("preparation %s: %v", op, err)
		}
	}
	az, err := e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "service.db", Bearer: svc.Token.Token, Op: runtimeauth.OpResourceSecrets})
	if err != nil {
		t.Fatal(err)
	}
	if b := az.Bindings(); len(b) != 1 || b[0].Ref != "secret://project/test-db-password" || b[0].Version != 3 {
		t.Fatalf("a service resolves exactly its pinned bindings: %+v", b)
	}
	// A token of another service (copied) is not valid for this route; a token of
	// another run is not valid either.
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "service.db", Bearer: other.Token.Token, Op: runtimeauth.OpResourceSecrets})
	is(t, err, runtimeauth.ErrForbidden)
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: "run_02", Key: "service.db", Bearer: svc.Token.Token, Op: runtimeauth.OpResourceSecrets})
	is(t, err, runtimeauth.ErrForbidden)
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "service.cache", Bearer: svc.Token.Token, Op: runtimeauth.OpResourceContext})
	is(t, err, runtimeauth.ErrForbidden)
	// Keys without a runtime identity are refused as invalid, not mapped.
	_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "workspace", Bearer: svc.Token.Token, Op: runtimeauth.OpResourceContext})
	is(t, err, runtimeauth.ErrInvalidCredential)
	// A garbled bearer is an invalid credential, not a route-family error.
	_, err = e.Service.RenewResource(ctx, resRun, "service.db", "not-a-token")
	is(t, err, runtimeauth.ErrInvalidCredential)

	// Wire fences: the v2 attempt on the v1 routes and the v1 grant on the v2 route.
	_, err = e.Service.Context(ctx, c2.AttemptID, attTok)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.Renew(ctx, c2.AttemptID, attTok)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: c2.AttemptID, Bearer: attTok, Action: runtimeauth.ActionResultSubmit})
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	if _, err := e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: c2.AttemptID, Bearer: attTok, Action: runtimeauth.ActionResultSubmit, ContextVersion: runtimeauth.WireV2}); err != nil {
		t.Fatalf("the attempt on its own wire: %v", err)
	}
	if got, err := e.Service.ContextV2(ctx, c2.AttemptID, attTok); err != nil || got.AttemptID != c2.AttemptID {
		t.Fatalf("ContextV2: %v", err)
	}
	// A v1 attempt on the v2 routes. Reuse the v1 happy path.
	hc, hreq := e.HappyContext()
	ha := e.SeedAttempt(hc, rtest.AllActions())
	hcred, _, err := e.Register(ha, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	hreq.AdmissionCredential = hcred.Reveal()
	hresp, err := e.Service.Admit(ctx, hc.AttemptID, hreq)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Service.AdmitV2(ctx, hc.AttemptID, hreq)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.ContextV2(ctx, hc.AttemptID, hresp.Token.Token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	_, err = e.Service.RenewV2(ctx, hc.AttemptID, hresp.Token.Token)
	is(t, err, runtimeauth.ErrContextVersionMismatch)
	if _, err := e.Service.Context(ctx, hc.AttemptID, hresp.Token.Token); err != nil {
		t.Fatalf("a v1 attempt on the v1 route: %v", err)
	}
}

func Test_unit_runtimeauth_resource_revocation(t *testing.T) {
	ctx := context.Background()
	use := func(e *resEnv, ref runtimeauth.ResourceRef, tok string) error {
		_, err := e.Service.RenewResource(ctx, resRun, ref.Key, tok)
		return err
	}
	setup := func(t *testing.T) (*resEnv, runtimev2.ResourceAdmitResponse, runtimeauth.Credential) {
		e := newResEnv(t)
		cred := e.register(e.svc)
		resp, err := e.admit(e.svc, cred, nonce1)
		if err != nil {
			t.Fatal(err)
		}
		if err := use(e, e.svc, resp.Token.Token); err != nil {
			t.Fatal(err)
		}
		return e, resp, cred
	}

	t.Run("stop intent ends authority whether or not the owner revoked", func(t *testing.T) {
		for _, revoke := range []bool{true, false} {
			e, resp, cred := setup(t)
			e.Resources.Stop(e.svc, revoke, e.Clock.Now())
			is(t, use(e, e.svc, resp.Token.Token), runtimeauth.ErrResourceRevoked)
			_, err := e.Service.ResourceContext(ctx, resRun, "service.db", resp.Token.Token)
			is(t, err, runtimeauth.ErrResourceRevoked)
			_, err = e.Service.AuthorizeResource(ctx, runtimeauth.ResourceAuthorizeRequest{RunID: resRun, Key: "service.db", Bearer: resp.Token.Token, Op: runtimeauth.OpResourceSecrets})
			is(t, err, runtimeauth.ErrResourceRevoked)
			// Neither admission nor its replay revives it.
			_, err = e.admit(e.svc, cred, nonce1)
			is(t, err, runtimeauth.ErrResourceRevoked)
			// Nothing revives it, not even the state becoming active again.
			e.Resources.Modify(e.svc, func(s *runtimeauth.ResourceState) { s.Phase = runtimeauth.ResourceActive })
			if revoke {
				is(t, use(e, e.svc, resp.Token.Token), runtimeauth.ErrResourceRevoked)
			}
		}
	})

	t.Run("explicit revocation is irreversible and idempotent", func(t *testing.T) {
		e, resp, cred := setup(t)
		for range 2 {
			if err := e.Service.RevokeResource(ctx, e.svc); err != nil {
				t.Fatal(err)
			}
		}
		is(t, use(e, e.svc, resp.Token.Token), runtimeauth.ErrResourceRevoked)
		_, err := e.admit(e.svc, cred, nonce1)
		is(t, err, runtimeauth.ErrResourceRevoked)
		// Another resource of the run is unaffected.
		pc := e.register(e.prep)
		if _, err := e.admit(e.prep, pc, nonce1); err != nil {
			t.Fatal(err)
		}
		// Run cancellation revokes every resource of the run.
		if err := e.Service.RevokeRunResources(ctx, resScope, resRun); err != nil {
			t.Fatal(err)
		}
		_, err = e.admit(e.prep, pc, nonce1)
		is(t, err, runtimeauth.ErrResourceRevoked)
		// Registration after the stop is refused.
		e.Resources.Stop(e.svcOther, false, e.Clock.Now())
		st, _ := e.Resources.ObserveResource(ctx, e.svcOther)
		oc, _ := runtimeauth.GenerateCredential(nil)
		is(t, e.Service.RegisterResource(ctx, runtimeauth.ResourceRegisterCommand{Scope: resScope, RunID: resRun, Key: "service.cache", Credential: oc, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}), runtimeauth.ErrResourceRevoked)
	})

	t.Run("the deadline ends the session and caps every token", func(t *testing.T) {
		e, resp, cred := setup(t)
		maxAt, _ := resp.Token.MaxExpiresAt.Time()
		if !maxAt.Equal(e.deadline.Truncate(time.Microsecond)) {
			t.Fatalf("max %s", maxAt)
		}
		e.Clock.Advance(time.Hour - time.Minute)
		// The renewed token never outlives the deadline.
		r, err := e.Service.RenewResource(ctx, resRun, "service.db", resp.Token.Token)
		if err == nil {
			exp, _ := r.Token.ExpiresAt.Time()
			if exp.After(e.deadline) {
				t.Fatalf("token expires %s after the deadline %s", exp, e.deadline)
			}
		}
		e.Clock.Advance(2 * time.Minute)
		is(t, use(e, e.svc, resp.Token.Token), runtimeauth.ErrInvalidCredential) // the token itself expired
		_, err = e.admit(e.svc, cred, nonce1)
		if err == nil {
			t.Fatal("admission after the deadline succeeded")
		}
	})

	t.Run("a replaced or respecified instance is not the admitted one", func(t *testing.T) {
		e, resp, _ := setup(t)
		e.Resources.Modify(e.svc, func(s *runtimeauth.ResourceState) { s.WorkloadUID = "aaaaaaaa-6d7e-4f80-9b12-3d4e5f607182" })
		is(t, use(e, e.svc, resp.Token.Token), runtimeauth.ErrResourceRevoked)
		e2, resp2, _ := setup(t)
		e2.Resources.Modify(e2.svc, func(s *runtimeauth.ResourceState) { s.Fingerprint = "sha256:" + strings.Repeat("c", 64) })
		is(t, use(e2, e2.svc, resp2.Token.Token), runtimeauth.ErrResourceRevoked)
	})

	t.Run("a stop committed first denies the admission and revokes the grant", func(t *testing.T) {
		e := newResEnv(t)
		cred := e.register(e.svc)
		// The stop commits between the advisory observation and the locked
		// re-read: the transaction sees it, denies and revokes durably.
		e.Resources.SetHook(func() {
			e.Resources.ModifyLocked(e.svc, func(s *runtimeauth.ResourceState) { s.Phase = runtimeauth.ResourceEnded })
		})
		_, err := e.admit(e.svc, cred, nonce1)
		is(t, err, runtimeauth.ErrResourceRevoked)
		e.Resources.SetHook(nil)
		if g, _ := e.Resources.Grant(e.svc); !g.Revoked() || g.Consumed() {
			t.Fatalf("the denied admission must revoke without consuming: %+v", g)
		}
	})

	t.Run("a different tenant cannot touch the grant", func(t *testing.T) {
		e := newResEnv(t)
		cred := e.register(e.svc)
		resp, err := e.admit(e.svc, cred, nonce1)
		if err != nil {
			t.Fatal(err)
		}
		foreign := runtimeauth.ResourceRef{Scope: domain.Scope{TenantID: "t_other", ProjectID: "p_other"}, RunID: resRun, Key: "service.db"}
		if err := e.Service.RevokeResource(ctx, foreign); err != nil {
			t.Fatal(err) // nothing to revoke there: a no-op
		}
		if err := use(e, e.svc, resp.Token.Token); err != nil {
			t.Fatalf("a foreign revoke affected this tenant's resource: %v", err)
		}
	})
}
