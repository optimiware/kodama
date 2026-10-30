package runtimehttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The v2 routes are judged against the frozen runtime v2 scenarios
// (contracts/runtime/v2/fixtures/scenarios.json). Part one replays every step of
// every scenario through the handlers with a scripted service: the handler must
// answer the recorded status and an equal body (for errors, the recorded code), so
// the transport mapping, the strict decoding, the route table and the error
// inventories are checked against the contract and not against this package's own
// reading of it. Part two runs the same exchanges through the real service over the
// memory authority, including the cross-version and cross-audience refusals.

var errByCode = map[domain.ErrorCode]error{
	domain.CodeInvalidRequest:            runtimeauth.ErrInvalidRequest,
	domain.CodeInvalidCredential:         runtimeauth.ErrInvalidCredential,
	domain.CodeForbidden:                 runtimeauth.ErrForbidden,
	runtimev1.CodeAttemptRevoked:         runtimeauth.ErrAttemptRevoked,
	runtimev1.CodeAdmissionConsumed:      runtimeauth.ErrAdmissionConsumed,
	runtimev1.CodeProfileUnsupported:     runtimeauth.ErrProfileUnsupported,
	runtimev2.CodeContextVersionMismatch: runtimeauth.ErrContextVersionMismatch,
	runtimev2.CodeReceiptConflict:        runtimeauth.ErrReceiptConflict,
	runtimev2.CodeResourceUnbound:        runtimeauth.ErrResourceUnbound,
	runtimev2.CodeResourceRevoked:        runtimeauth.ErrResourceRevoked,
	domain.CodeIdempotencyConflict:       runtimeauth.ErrIdempotencyConflict,
	domain.CodeNotFound:                  &runtimev1.ContractError{Code: domain.CodeNotFound, Message: "no such binding"},
}

// scripted is every port of the v2 handler, answering the current step.
type scripted struct {
	step     rtest.V2Step
	prepCtx  runtimev2.ResourceContext
	seenAuth []runtimeauth.AuthorizeRequest
}

func (s *scripted) outcome() error {
	if s.step.Status < 400 {
		return nil
	}
	var b struct {
		Error struct {
			Code domain.ErrorCode `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(s.step.Body, &b); err != nil {
		panic(err)
	}
	e, ok := errByCode[b.Error.Code]
	if !ok {
		panic("unscripted code " + string(b.Error.Code))
	}
	return e
}

func decodeBody[T any, P interface {
	*T
	Validate() error
}](s *scripted) T {
	v, err := runtimev2.DecodeResponse[T, P](s.step.Body)
	if err != nil {
		panic(err)
	}
	return v
}

func (s *scripted) AdmitV2(context.Context, domain.AttemptID, runtimev1.AdmitRequest) (runtimev2.AdmitResponseV2, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.AdmitResponseV2{}, err
	}
	return decodeBody[runtimev2.AdmitResponseV2](s), nil
}

func (s *scripted) RenewV2(context.Context, domain.AttemptID, string) (runtimev2.TokenResponse, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.TokenResponse{}, err
	}
	return decodeBody[runtimev2.TokenResponse](s), nil
}

func (s *scripted) ContextV2(context.Context, domain.AttemptID, string) (runtimev2.ContextV2, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.ContextV2{}, err
	}
	return decodeBody[runtimev2.ContextV2](s), nil
}

// Authorize answers the guard of begin and of the shared routes: an error step
// of those operations is an authority refusal, a success passes.
func (s *scripted) Authorize(_ context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error) {
	s.seenAuth = append(s.seenAuth, req)
	if req.ContextVersion != runtimeauth.WireV2 {
		panic("the v2 routes must authorize on the v2 wire")
	}
	if s.step.Op == "begin" || s.step.Op == "submitResult" {
		if err := s.outcome(); err != nil && s.step.Status != 409 || (err != nil && s.step.Status == 409 && errors.Is(err, runtimeauth.ErrContextVersionMismatch)) {
			return runtimeauth.Authorization{}, err
		}
	}
	return runtimeauth.Authorization{Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, RunID: "run_01", AttemptID: "att_02", ContextVersion: runtimeauth.WireV2}, nil
}

// Reauthorize and ReauthorizeResource are the recheck before a durable record; the
// scripted authorizations stay current.
func (s *scripted) Reauthorize(context.Context, runtimeauth.Authorization) error { return nil }

func (s *scripted) ReauthorizeResource(context.Context, runtimeauth.ResourceAuthorization) error {
	return nil
}

func (s *scripted) RecordBegin(_ context.Context, auth runtimeauth.Authorization, req runtimev2.BeginRequest) (runtimehttp.BeginResult, error) {
	if auth.Scope != (domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}) {
		panic("the scope must come from the verified authorization")
	}
	if err := s.outcome(); err != nil {
		return runtimehttp.BeginResult{}, err
	}
	r := decodeBody[runtimev2.BeginResponse](s)
	at, _ := r.BeganAt.Time()
	return runtimehttp.BeginResult{AttemptID: r.AttemptID, BeganAt: at, WorkspaceState: r.WorkspaceState, IgnoredPresent: r.IgnoredPresent}, nil
}

func (s *scripted) AdmitResource(_ context.Context, route runtimeauth.ResourceRef, _ runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.ResourceAdmitResponse{}, err
	}
	r := decodeBody[runtimev2.ResourceAdmitResponse](s)
	s.prepCtx = r.Context
	return r, nil
}

func (s *scripted) RenewResource(_ context.Context, _ domain.RunID, _, _ string) (runtimev2.ResourceTokenResponse, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.ResourceTokenResponse{}, err
	}
	return decodeBody[runtimev2.ResourceTokenResponse](s), nil
}

func (s *scripted) ResourceContext(_ context.Context, _ domain.RunID, _, _ string) (runtimev2.ResourceContext, error) {
	if err := s.outcome(); err != nil {
		return runtimev2.ResourceContext{}, err
	}
	return decodeBody[runtimev2.ResourceContext](s), nil
}

func (s *scripted) AuthorizeResource(_ context.Context, req runtimeauth.ResourceAuthorizeRequest) (runtimeauth.ResourceAuthorization, error) {
	// The recorded refusals of these steps are authority refusals (a wrong
	// audience, a kind without the route); a success or a downstream failure passes.
	if s.step.Status == 403 {
		return runtimeauth.ResourceAuthorization{}, s.outcome()
	}
	return runtimeauth.ResourceAuthorization{Ref: runtimeauth.ResourceRef{Scope: domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}, RunID: req.RunID, Key: req.Key}, Context: s.prepCtx}, nil
}

func (s *scripted) RecordPreparationReceipt(context.Context, runtimeauth.ResourceAuthorization, runtimev2.PreparationReceiptRequest) (runtimehttp.ReceiptResult, error) {
	if err := s.outcome(); err != nil {
		return runtimehttp.ReceiptResult{}, err
	}
	r := decodeBody[runtimev2.PreparationReceiptResponse](s)
	at, _ := r.RecordedAt.Time()
	return runtimehttp.ReceiptResult{Digest: string(r.ReceiptDigest), RecordedAt: at}, nil
}

func (s *scripted) ResolveResourceSecret(context.Context, runtimeauth.ResourceAuthorization, runtimev1.SecretResolveRequest, string) (runtimev1.SecretResolveResponse, error) {
	if err := s.outcome(); err != nil {
		return runtimev1.SecretResolveResponse{}, err
	}
	return decodeBody[runtimev1.SecretResolveResponse](s), nil
}

// shared is a v1 owner's handler of a shared body: it records the path it was
// given and answers 200 with a fixed body.
type shared struct{ paths *[]string }

func (h shared) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	*h.paths = append(*h.paths, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func pathOf(op runtimev2.Operation, s rtest.V2Step, attempt string) string {
	p := runtimev2.AttemptBasePath
	if op.Subject == runtimev2.SubjectResource {
		p = runtimev2.ResourceBasePath
	}
	p += op.Path
	key := "service.db"
	if s.ResourceKind == "preparation" {
		key = "preparation"
	}
	r := strings.NewReplacer("{attemptId}", attempt, "{runId}", "run_01", "{resourceKey}", key, "{artifactId}", "art_01")
	return r.Replace(p)
}

func Test_unit_runtime_v2_routes_contract(t *testing.T) {
	t.Run("every recorded exchange of the frozen scenarios", func(t *testing.T) {
		var paths []string
		sc := &scripted{}
		h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{
			Service: sc, Begins: sc, Receipts: sc, ResourceSecrets: sc,
			Shared: map[string]http.Handler{"submitResult": shared{&paths}, "resolveSecret": shared{&paths}, "proposeSource": shared{&paths}, "reserveArtifact": shared{&paths}, "completeArtifact": shared{&paths}},
			Logger: newLogger(&logBuf{}),
		})
		if err != nil {
			t.Fatal(err)
		}
		v1h, err := runtimehttp.NewHandler(runtimehttp.Config{Service: v1Mismatch{}, Logger: newLogger(&logBuf{})})
		if err != nil {
			t.Fatal(err)
		}
		steps := 0
		for _, scn := range rtest.LoadV2Scenarios(t) {
			attempt := "att_02" // the attempt of the scenario's latest admission
			for i, st := range scn.Steps {
				steps++
				sc.step = st
				if st.Op == "admit" && st.Status == 200 {
					attempt = string(rtest.ContextV2Of(t, st).AttemptID)
				}
				var ops runtimev2.Operation
				found := false
				if st.API == "v1" {
					for _, v1 := range runtimev1.Operations {
						if v1.ID == st.Op {
							ops, found = runtimev2.Operation{ID: v1.ID, Method: v1.Method, Path: v1.Path}, true
						}
					}
				} else {
					for _, o := range runtimev2.Operations {
						if o.ID == st.Op {
							ops, found = o, true
						}
					}
				}
				if !found {
					t.Fatalf("%s #%d: unknown operation %q", scn.Name, i, st.Op)
				}
				var path string
				var handler http.Handler = h
				if st.API == "v1" {
					path, handler = strings.Replace(runtimev1.BasePath, "{attemptId}", attempt, 1)+ops.Path, v1h
				} else {
					path = pathOf(ops, st, attempt)
				}
				var body []byte
				if len(st.Request) > 0 && string(st.Request) != "null" {
					body = st.Request
				}
				hdr := map[string]string{}
				if st.Op != "admit" && st.Op != "admitResource" {
					hdr["Authorization"] = "Bearer rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				}
				rec := do(handler, ops.Method, path, body, hdr)
				ctx := scn.Name + " #" + string(rune('0'+i)) + " " + st.Op
				if rec.Code != st.Status {
					t.Fatalf("%s: status %d, the contract records %d: %s", ctx, rec.Code, st.Status, rec.Body)
				}
				var got, want any
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("%s: %v", ctx, err)
				}
				if err := json.Unmarshal(st.Body, &want); err != nil {
					t.Fatalf("%s: %v", ctx, err)
				}
				if st.Status >= 400 {
					gc := got.(map[string]any)["error"].(map[string]any)["code"]
					wc := want.(map[string]any)["error"].(map[string]any)["code"]
					if gc != wc {
						t.Fatalf("%s: code %v, the contract records %v", ctx, gc, wc)
					}
					if _, err := runtimev2.DecodeResponse[runtimev2.ErrorResponse](rec.Body.Bytes()); err != nil {
						t.Fatalf("%s: not a v2 error envelope: %v", ctx, err)
					}
					continue
				}
				if st.Op == "submitResult" || strings.HasPrefix(st.Op, "reserve") || st.Op == "proposeSource" {
					continue // served by the shared v1 owner
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: body differs from the contract\n got %s\nwant %s", ctx, rec.Body, st.Body)
				}
				if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
					t.Fatalf("%s: headers %v", ctx, rec.Header())
				}
			}
		}
		if steps < 50 {
			t.Fatalf("only %d recorded exchanges were replayed", steps)
		}
	})

	t.Run("shared bodies reach the v1 owners under the v1 path, fenced first", func(t *testing.T) {
		var paths []string
		sc := &scripted{step: rtest.V2Step{Status: 200}}
		h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{
			Service: sc, Shared: map[string]http.Handler{"proposeSource": shared{&paths}, "completeArtifact": shared{&paths}}, Logger: newLogger(&logBuf{}),
		})
		if err != nil {
			t.Fatal(err)
		}
		tok := map[string]string{"Authorization": "Bearer rt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		if rec := do(h, "POST", "/runtime/v2/attempts/att_02/source", []byte(`{}`), tok); rec.Code != 200 {
			t.Fatalf("source: %d %s", rec.Code, rec.Body)
		}
		if rec := do(h, "POST", "/runtime/v2/attempts/att_02/artifacts/art_01/complete", []byte(`{}`), tok); rec.Code != 200 {
			t.Fatalf("complete: %d %s", rec.Code, rec.Body)
		}
		want := []string{"/runtime/v1/attempts/att_02/source", "/runtime/v1/attempts/att_02/artifacts/art_01/complete"}
		if !reflect.DeepEqual(paths, want) {
			t.Fatalf("v1 owners were called with %v", paths)
		}
		if got := sc.seenAuth[0]; got.Action != runtimeauth.ActionSourcePropose || got.ContextVersion != runtimeauth.WireV2 || got.AttemptID != "att_02" {
			t.Fatalf("fence: %+v", got)
		}
		// Unwired shared routes and unknown paths are the not_found envelope.
		if rec := do(h, "POST", "/runtime/v2/attempts/att_02/result", []byte(`{}`), tok); rec.Code != 404 {
			t.Fatalf("an unwired route answered %d", rec.Code)
		}
		if rec := do(h, "POST", "/runtime/v2/runs/run_01/resources/workspace/admit", []byte(`{}`), nil); rec.Code != 400 && rec.Code != 404 {
			t.Fatalf("a resource key without identity: %d", rec.Code)
		}
		// A shared operation that is not shared is refused at construction.
		if _, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: sc, Shared: map[string]http.Handler{"begin": shared{&paths}}}); err == nil {
			t.Fatal("a non-shared operation was accepted as shared")
		}
		if _, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{}); err == nil {
			t.Fatal("a handler without a service was built")
		}
	})

	t.Run("the real service: cross-version and cross-audience refusals", func(t *testing.T) {
		e := rtest.NewResourceMemStack(t)
		begins := &recordBegin{}
		v2h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Begins: begins, Logger: newLogger(&logBuf{})})
		if err != nil {
			t.Fatal(err)
		}
		v1h, err := runtimehttp.NewHandler(runtimehttp.Config{Service: e.Service, Logger: newLogger(&logBuf{})})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()

		// A run_shared attempt admitted through the HTTP route.
		c2 := rtest.ContextV2Of(t, rtest.LoadV2Scenario(t, "begin_replay_with_a_different_body_conflicts").Steps[0])
		acts, _ := runtimeauth.NewActionSet(runtimeauth.DefaultActions(false)...)
		att := e.SeedAttemptV2(c2, acts)
		cred, _ := runtimeauth.GenerateCredential(nil)
		if _, err := e.RegisterV2With(att, c2, cred, acts); err != nil {
			t.Fatal(err)
		}
		req := e.AdmitRequest(att, cred, "nnnnnnnnnnnnnnnnnnnnnn")
		raw, _ := json.Marshal(req)
		base := "/runtime/v2/attempts/" + string(c2.AttemptID)
		rec := do(v2h, "POST", base+"/admit", raw, nil)
		if rec.Code != 200 {
			t.Fatalf("v2 admit: %d %s", rec.Code, rec.Body)
		}
		resp, err := runtimev2.DecodeResponse[runtimev2.AdmitResponseV2](rec.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		bearer := map[string]string{"Authorization": "Bearer " + resp.Token.Token}
		if resp.Context.ContextVersion != 2 || resp.Token.Audience != runtimev2.AttemptAudience {
			t.Fatalf("admit: %+v", resp.Token)
		}
		// The same exchange on the v1 route is a version mismatch with nothing changed.
		v1base := "/runtime/v1/attempts/" + string(c2.AttemptID)
		for _, tc := range []struct{ method, path string }{{"GET", v1base + "/context"}, {"POST", v1base + "/token"}} {
			rec := do(v1h, tc.method, tc.path, []byte(`{}`), bearer)
			if rec.Code != 409 || codeOf(t, rec) != runtimev2.CodeContextVersionMismatch {
				t.Fatalf("v1 %s: %d %s", tc.path, rec.Code, rec.Body)
			}
		}
		rec = do(v1h, "POST", v1base+"/admit", raw, nil)
		if rec.Code != 409 || codeOf(t, rec) != runtimev2.CodeContextVersionMismatch {
			t.Fatalf("v1 admit of a v2 attempt: %d %s", rec.Code, rec.Body)
		}
		// On its own wire it works.
		if rec := do(v2h, "GET", base+"/context", nil, bearer); rec.Code != 200 {
			t.Fatalf("v2 context: %d %s", rec.Code, rec.Body)
		}
		// begin reaches the recorder with the attempt's scope, only on the v2 wire.
		if rec := do(v2h, "POST", base+"/begin", []byte(`{"workspace":{"head":"2222222222222222222222222222222222222222","trackedClean":true,"untrackedPresent":false,"ignoredPresent":true}}`), bearer); rec.Code != 200 {
			t.Fatalf("begin: %d %s", rec.Code, rec.Body)
		}
		if begins.scope != att.Scope || begins.attempt != c2.AttemptID {
			t.Fatalf("begin was recorded for %+v %s", begins.scope, begins.attempt)
		}
		// A malformed (omitted field) begin is invalid_request and records nothing more.
		before := begins.calls
		if rec := do(v2h, "POST", base+"/begin", []byte(`{}`), bearer); rec.Code != 400 {
			t.Fatalf("begin without its workspace member: %d %s", rec.Code, rec.Body)
		}
		if begins.calls != before {
			t.Fatal("a malformed begin reached the recorder")
		}

		// A v1 attempt on the v2 routes.
		hc, hreq := e.HappyContext()
		ha := e.SeedAttempt(hc, rtest.AllActions())
		hcred, _, err := e.Register(ha, rtest.AllActions())
		if err != nil {
			t.Fatal(err)
		}
		hreq.AdmissionCredential = hcred.Reveal()
		hraw, _ := json.Marshal(hreq)
		hv1 := "/runtime/v1/attempts/" + string(hc.AttemptID)
		hv2 := "/runtime/v2/attempts/" + string(hc.AttemptID)
		if rec := do(v1h, "POST", hv1+"/admit", hraw, nil); rec.Code != 200 {
			t.Fatalf("v1 admit: %d %s", rec.Code, rec.Body)
		}
		if rec := do(v2h, "POST", hv2+"/admit", hraw, nil); rec.Code != 409 || codeOf(t, rec) != runtimev2.CodeContextVersionMismatch {
			t.Fatalf("v2 admit of a v1 attempt: %d %s", rec.Code, rec.Body)
		}
		v1resp, err := e.Service.Admit(ctx, hc.AttemptID, hreq)
		if err != nil {
			t.Fatal(err)
		}
		v1tok := map[string]string{"Authorization": "Bearer " + v1resp.Token.Token}
		for _, p := range []string{hv2 + "/context", hv2 + "/begin"} {
			m := "GET"
			if strings.HasSuffix(p, "begin") {
				m = "POST"
			}
			rec := do(v2h, m, p, []byte(`{"workspace":null}`), v1tok)
			if rec.Code != 409 || codeOf(t, rec) != runtimev2.CodeContextVersionMismatch {
				t.Fatalf("v2 %s of a v1 attempt: %d %s", p, rec.Code, rec.Body)
			}
		}

		// Resources: admission, replay, consumption and the audience fences.
		f := rtest.ResourceFixture{Scope: att.Scope, RunID: c2.RunID, WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071", DeadlineAt: e.Clock.Now().Add(time.Hour)}
		e.Resources.Put(rtest.ServiceState(f, "db", []runtimev1.SecretBinding{{Ref: "secret://project/test-db-password", Version: 3, Name: "postgres-password", Delivery: runtimev1.DeliveryFile}}))
		st, _ := e.Resources.ObserveResource(ctx, runtimeauth.ResourceRef{Scope: att.Scope, RunID: c2.RunID, Key: "service.db"})
		rcred, _ := runtimeauth.GenerateCredential(nil)
		if err := e.Service.RegisterResource(ctx, runtimeauth.ResourceRegisterCommand{Scope: att.Scope, RunID: c2.RunID, Key: "service.db", Credential: rcred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}); err != nil {
			t.Fatal(err)
		}
		rbase := "/runtime/v2/runs/" + string(c2.RunID) + "/resources/service.db"
		areq, _ := json.Marshal(runtimev2.ResourceAdmitRequest{AdmissionCredential: rcred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0",
			Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}})
		rec = do(v2h, "POST", rbase+"/admit", areq, nil)
		if rec.Code != 200 {
			t.Fatalf("resource admit: %d %s", rec.Code, rec.Body)
		}
		rresp, err := runtimev2.DecodeResponse[runtimev2.ResourceAdmitResponse](rec.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		rtok := map[string]string{"Authorization": "Bearer " + rresp.Token.Token}
		if rec := do(v2h, "POST", rbase+"/admit", areq, nil); rec.Code != 200 {
			t.Fatalf("replay after a lost response: %d %s", rec.Code, rec.Body)
		}
		other, _ := json.Marshal(runtimev2.ResourceAdmitRequest{AdmissionCredential: rcred.Reveal(), Nonce: "mmmmmmmmmmmmmmmmmmmmmm", ClientVersion: "1.4.0",
			Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}})
		if rec := do(v2h, "POST", rbase+"/admit", other, nil); rec.Code != 409 || codeOf(t, rec) != runtimev1.CodeAdmissionConsumed {
			t.Fatalf("another nonce: %d %s", rec.Code, rec.Body)
		}
		if rec := do(v2h, "GET", rbase+"/context", nil, rtok); rec.Code != 200 {
			t.Fatalf("resource context: %d %s", rec.Code, rec.Body)
		}
		if rec := do(v2h, "POST", rbase+"/token", []byte(`{}`), rtok); rec.Code != 200 {
			t.Fatalf("resource renew: %d %s", rec.Code, rec.Body)
		}
		// preparation/complete is a preparation-only route; secrets/resolve is not wired here.
		for _, tc := range []struct {
			method, path string
			hdr          map[string]string
			want         int
			code         domain.ErrorCode
		}{
			// a resource token on every attempt route: forbidden
			{"GET", base + "/context", rtok, 403, domain.CodeForbidden},
			{"POST", base + "/token", rtok, 403, domain.CodeForbidden},
			{"POST", base + "/begin", rtok, 403, domain.CodeForbidden},
			// an attempt token on every resource route: forbidden
			{"GET", rbase + "/context", bearer, 403, domain.CodeForbidden},
			{"POST", rbase + "/token", bearer, 403, domain.CodeForbidden},
			// garbage is unauthenticated, not a route-family error
			{"GET", rbase + "/context", map[string]string{"Authorization": "Bearer nonsense"}, 401, domain.CodeInvalidCredential},
			{"GET", base + "/context", nil, 401, domain.CodeInvalidCredential},
		} {
			rec := do(v2h, tc.method, tc.path, []byte(`{}`), tc.hdr)
			if rec.Code != tc.want || codeOf(t, rec) != tc.code {
				t.Fatalf("%s %s: %d %s (want %d %s)", tc.method, tc.path, rec.Code, rec.Body, tc.want, tc.code)
			}
		}
		// After the stop, the resource's context and renewal are resource_revoked.
		e.Resources.Stop(runtimeauth.ResourceRef{Scope: att.Scope, RunID: c2.RunID, Key: "service.db"}, true, e.Clock.Now())
		for _, tc := range []struct{ method, path string }{{"GET", rbase + "/context"}, {"POST", rbase + "/token"}} {
			rec := do(v2h, tc.method, tc.path, []byte(`{}`), rtok)
			if rec.Code != 403 || codeOf(t, rec) != runtimev2.CodeResourceRevoked {
				t.Fatalf("%s after the stop: %d %s", tc.path, rec.Code, rec.Body)
			}
		}
		// No error body quotes a credential, token or nonce.
		for _, secret := range []string{rcred.Reveal(), rresp.Token.Token, resp.Token.Token, "nnnnnnnnnnnnnnnnnnnnnn"} {
			if strings.Contains(e.Logs.String(), secret) {
				t.Fatal("a credential or token reached the service log")
			}
		}
	})
}

type recordBegin struct {
	scope   domain.Scope
	attempt domain.AttemptID
	calls   int
}

func (r *recordBegin) RecordBegin(_ context.Context, auth runtimeauth.Authorization, _ runtimev2.BeginRequest) (runtimehttp.BeginResult, error) {
	r.scope, r.attempt = auth.Scope, auth.AttemptID
	attempt := auth.AttemptID
	r.calls++
	return runtimehttp.BeginResult{AttemptID: attempt, BeganAt: time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC), WorkspaceState: runtimev2.StateClean, IgnoredPresent: true}, nil
}

// v1Mismatch is a v1 service whose every attempt is a run_shared attempt.
type v1Mismatch struct{}

func (v1Mismatch) Admit(context.Context, domain.AttemptID, runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error) {
	return runtimev1.AdmitResponse{}, runtimeauth.ErrContextVersionMismatch
}
func (v1Mismatch) Renew(context.Context, domain.AttemptID, string) (runtimev1.TokenResponse, error) {
	return runtimev1.TokenResponse{}, runtimeauth.ErrContextVersionMismatch
}
func (v1Mismatch) Context(context.Context, domain.AttemptID, string) (runtimev1.Context, error) {
	return runtimev1.Context{}, runtimeauth.ErrContextVersionMismatch
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) domain.ErrorCode {
	t.Helper()
	var b struct {
		Error struct {
			Code domain.ErrorCode `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&b); err != nil {
		t.Fatalf("not an error envelope: %s", rec.Body)
	}
	if want, ok := runtimev2.Status(b.Error.Code); !ok || want != rec.Code {
		t.Fatalf("code %s is not served with status %d", b.Error.Code, rec.Code)
	}
	return b.Error.Code
}

// ---- review finding 7: durable mutations recheck the grant ---------------------------

// revokeOnRead is a request body that, when first read, runs a hook (a revocation or
// a cancellation committing) and only then delivers its bytes: the exact window
// between "authorized" and "recorded".
type revokeOnRead struct {
	data []byte
	hook func()
	done bool
	off  int
}

func (b *revokeOnRead) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		b.hook()
	}
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

type countingReceipts struct {
	got []runtimev2.PreparationReceiptRequest
}

func (c *countingReceipts) RecordPreparationReceipt(_ context.Context, _ runtimeauth.ResourceAuthorization, req runtimev2.PreparationReceiptRequest) (runtimehttp.ReceiptResult, error) {
	c.got = append(c.got, req)
	return runtimehttp.ReceiptResult{Digest: "sha256:" + strings.Repeat("e", 64), RecordedAt: time.Now()}, nil
}

func doReader(h http.Handler, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func Test_unit_runtime_v2_mutations_recheck_the_grant(t *testing.T) {
	ctx := context.Background()
	beginBody := []byte(`{"workspace":{"head":"2222222222222222222222222222222222222222","trackedClean":true,"untrackedPresent":false,"ignoredPresent":false}}`)

	t.Run("begin: a revocation or cancellation between the authorization and the record records nothing", func(t *testing.T) {
		for name, revoke := range map[string]func(e *rtest.ResourceMemStack, att rtest.Attempt){
			"cancelled and revoked by the owner": func(e *rtest.ResourceMemStack, att rtest.Attempt) {
				if err := e.Env.Cancel(ctx, att.Scope, att.Context.AttemptID, true, e.Clock.Now()); err != nil {
					t.Fatal(err)
				}
			},
			"cancelled by the owner without a revocation": func(e *rtest.ResourceMemStack, att rtest.Attempt) {
				if err := e.Env.Cancel(ctx, att.Scope, att.Context.AttemptID, false, e.Clock.Now()); err != nil {
					t.Fatal(err)
				}
			},
			"explicitly revoked": func(e *rtest.ResourceMemStack, att rtest.Attempt) {
				if err := e.Service.Revoke(ctx, att.Scope, att.Context.AttemptID); err != nil {
					t.Fatal(err)
				}
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := rtest.NewResourceMemStack(t)
				begins := &recordBegin{}
				v2h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Begins: begins, Logger: newLogger(&logBuf{})})
				if err != nil {
					t.Fatal(err)
				}
				att, tok := admitV2Attempt(t, e, v2h)
				base := "/runtime/v2/attempts/" + string(att.Context.AttemptID)
				rec := doReader(v2h, "POST", base+"/begin", &revokeOnRead{data: beginBody, hook: func() { revoke(e, att) }}, tok)
				if rec.Code != 403 || codeOf(t, rec) != runtimev1.CodeAttemptRevoked {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				if begins.calls != 0 {
					t.Fatalf("a begin was recorded for a revoked session (%d)", begins.calls)
				}
			})
		}
		// Control: without the hook the same request is recorded.
		e := rtest.NewResourceMemStack(t)
		begins := &recordBegin{}
		v2h, _ := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Begins: begins, Logger: newLogger(&logBuf{})})
		att, tok := admitV2Attempt(t, e, v2h)
		rec := doReader(v2h, "POST", "/runtime/v2/attempts/"+string(att.Context.AttemptID)+"/begin", &revokeOnRead{data: beginBody, hook: func() {}}, tok)
		if rec.Code != 200 || begins.calls != 1 {
			t.Fatalf("control: %d %s (%d records)", rec.Code, rec.Body, begins.calls)
		}
	})

	t.Run("preparation receipt: a stop or revocation after the authorization records nothing", func(t *testing.T) {
		for name, revoke := range map[string]func(e *rtest.ResourceMemStack, ref runtimeauth.ResourceRef){
			"stop intent that also revoked": func(e *rtest.ResourceMemStack, ref runtimeauth.ResourceRef) {
				e.Resources.Stop(ref, true, e.Clock.Now())
			},
			"stop intent alone": func(e *rtest.ResourceMemStack, ref runtimeauth.ResourceRef) {
				e.Resources.Stop(ref, false, e.Clock.Now())
			},
			"explicit revocation": func(e *rtest.ResourceMemStack, ref runtimeauth.ResourceRef) {
				if err := e.Service.RevokeResource(ctx, ref); err != nil {
					t.Fatal(err)
				}
			},
			"run revocation": func(e *rtest.ResourceMemStack, ref runtimeauth.ResourceRef) {
				if err := e.Service.RevokeRunResources(ctx, ref.Scope, ref.RunID); err != nil {
					t.Fatal(err)
				}
			},
		} {
			t.Run(name, func(t *testing.T) {
				e := rtest.NewResourceMemStack(t)
				rcp := &countingReceipts{}
				v2h, err := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Receipts: rcp, Logger: newLogger(&logBuf{})})
				if err != nil {
					t.Fatal(err)
				}
				ref, tok, body := admitPreparation(t, e, v2h)
				path := "/runtime/v2/runs/" + string(ref.RunID) + "/resources/preparation/preparation/complete"
				rec := doReader(v2h, "POST", path, &revokeOnRead{data: body, hook: func() { revoke(e, ref) }}, tok)
				if rec.Code != 403 || codeOf(t, rec) != runtimev2.CodeResourceRevoked {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				if len(rcp.got) != 0 {
					t.Fatalf("a receipt was recorded for a revoked resource: %+v", rcp.got)
				}
			})
		}
		e := rtest.NewResourceMemStack(t)
		rcp := &countingReceipts{}
		v2h, _ := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Receipts: rcp, Logger: newLogger(&logBuf{})})
		ref, tok, body := admitPreparation(t, e, v2h)
		rec := doReader(v2h, "POST", "/runtime/v2/runs/"+string(ref.RunID)+"/resources/preparation/preparation/complete", &revokeOnRead{data: body, hook: func() {}}, tok)
		if rec.Code != 200 || len(rcp.got) != 1 {
			t.Fatalf("control: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("Reauthorize and ReauthorizeResource are the session's current answer", func(t *testing.T) {
		e := rtest.NewResourceMemStack(t)
		v2h, _ := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: e.Service, Logger: newLogger(&logBuf{})})
		att, tok := admitV2Attempt(t, e, v2h)
		auth, err := e.Service.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: att.Context.AttemptID, Bearer: strings.TrimPrefix(tok["Authorization"], "Bearer "), Action: runtimeauth.ActionContextRead, ContextVersion: runtimeauth.WireV2})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Service.Reauthorize(ctx, auth); err != nil {
			t.Fatal(err)
		}
		// Another wire, another attempt's session and a forged scope are not this authorization.
		wrong := auth
		wrong.ContextVersion = runtimeauth.WireV1
		if err := e.Service.Reauthorize(ctx, wrong); err == nil {
			t.Fatal("a session of the other wire was accepted")
		}
		wrong = auth
		wrong.TokenGeneration++
		if err := e.Service.Reauthorize(ctx, wrong); err == nil {
			t.Fatal("a retired signing generation was accepted")
		}
		if err := e.Env.Cancel(ctx, att.Scope, att.Context.AttemptID, true, e.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		if err := e.Service.Reauthorize(ctx, auth); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
			t.Fatalf("after the cancellation: %v", err)
		}
	})
}

// admitV2Attempt seeds, registers and admits a run_shared attempt and returns its
// attempt-token header.
func admitV2Attempt(t *testing.T, e *rtest.ResourceMemStack, h http.Handler) (rtest.Attempt, map[string]string) {
	t.Helper()
	c2 := rtest.ContextV2Of(t, rtest.LoadV2Scenario(t, "begin_replay_with_a_different_body_conflicts").Steps[0])
	acts, _ := runtimeauth.NewActionSet(runtimeauth.DefaultActions(false)...)
	att := e.SeedAttemptV2(c2, acts)
	cred, _ := runtimeauth.GenerateCredential(nil)
	if _, err := e.RegisterV2With(att, c2, cred, acts); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e.AdmitRequest(att, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
	rec := do(h, "POST", "/runtime/v2/attempts/"+string(c2.AttemptID)+"/admit", raw, nil)
	if rec.Code != 200 {
		t.Fatalf("admit: %d %s", rec.Code, rec.Body)
	}
	resp, err := runtimev2.DecodeResponse[runtimev2.AdmitResponseV2](rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return att, map[string]string{"Authorization": "Bearer " + resp.Token.Token}
}

// admitPreparation seeds, registers and admits a run's preparation and returns its
// reference, token header and a valid receipt body.
func admitPreparation(t *testing.T, e *rtest.ResourceMemStack, h http.Handler) (runtimeauth.ResourceRef, map[string]string, []byte) {
	t.Helper()
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	f := rtest.ResourceFixture{Scope: scope, RunID: "run_01", WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", DeadlineAt: e.Clock.Now().Add(time.Hour)}
	st := rtest.PreparationState(f)
	e.Resources.Put(st)
	ref := runtimeauth.ResourceRef{Scope: scope, RunID: "run_01", Key: "preparation"}
	cred, _ := runtimeauth.GenerateCredential(nil)
	if err := e.Service.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{Scope: scope, RunID: "run_01", Key: "preparation", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(runtimev2.ResourceAdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0",
		Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}})
	rec := do(h, "POST", "/runtime/v2/runs/run_01/resources/preparation/admit", raw, nil)
	if rec.Code != 200 {
		t.Fatalf("admit: %d %s", rec.Code, rec.Body)
	}
	resp, err := runtimev2.DecodeResponse[runtimev2.ResourceAdmitResponse](rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runtimev2.PreparationReceiptRequest{
		Commit: st.Context.Preparation.Commit, TreeID: runtimev1.GitCommit(strings.Repeat("3", 40)), HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1,
	})
	return ref, map[string]string{"Authorization": "Bearer " + resp.Token.Token}, body
}
