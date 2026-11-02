package runtimehttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth/gitauthtest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The source-set wire (T-150) over HTTP: the versioned route table, the version
// fence in both directions, the begin fence, the two runtime Git grant routes with
// their error envelopes, and the resource routes, through the real service over the
// memory authority. The owners' handlers (secrets, artifacts, result) are markers:
// what is judged here is that the v3 handler reaches them only through the guard
// and under the path their owner serves.

// recorder is a marker for an owner's handler: it notes the path it was invoked
// under.
type recorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

type beginFake struct{ called int }

func (b *beginFake) RecordBeginV3(_ context.Context, auth runtimeauth.Authorization, req runtimev3.BeginRequestV3) (runtimehttp.BeginResult, error) {
	b.called++
	if auth.ContextVersion != runtimeauth.WireV3 || auth.AttemptID == "" {
		panic("the begin fence must carry the verified v3 authorization")
	}
	return runtimehttp.BeginResult{AttemptID: auth.AttemptID, BeganAt: time.Date(2026, 10, 19, 10, 1, 0, 0, time.UTC), WorkspaceState: runtimev2.StateClean}, nil
}

type receiptFake struct{}

func (receiptFake) RecordPreparationReceiptV3(_ context.Context, auth runtimeauth.ResourceAuthorization, _ runtimev3.PreparationReceiptRequestV3) (runtimehttp.ReceiptResult, error) {
	if auth.ContextV3 == nil {
		panic("the receipt must carry the committed v3 context")
	}
	return runtimehttp.ReceiptResult{Digest: "sha256:" + strings.Repeat("e", 64), RecordedAt: time.Date(2026, 10, 19, 10, 2, 0, 0, time.UTC)}, nil
}

type secretFake struct{}

func (secretFake) ResolveResourceSecret(context.Context, runtimeauth.ResourceAuthorization, runtimev1.SecretResolveRequest, string) (runtimev1.SecretResolveResponse, error) {
	return runtimev1.SecretResolveResponse{}, &runtimev1.ContractError{Code: domain.CodeNotFound, Message: "no such binding"}
}

type v3http struct {
	*rtest.V3MemStack
	t        *testing.T
	h        http.Handler
	v2       http.Handler
	shared   *recorder
	direct   *recorder
	begins   *beginFake
	logs     *rtest.SafeBuffer
	scope    domain.Scope
	context  runtimev3.ContextV3
	attempt  rtest.Attempt
	token    string
	deadline time.Time
}

func newV3HTTP(t *testing.T, fixture string) *v3http {
	t.Helper()
	m := rtest.NewV3MemStack(t)
	e := &v3http{V3MemStack: m, t: t, shared: &recorder{}, direct: &recorder{}, begins: &beginFake{}, logs: &rtest.SafeBuffer{}}
	e.deadline = m.Clock.Now().Add(time.Hour)
	e.context = rtest.V3Context(t, fixture, func(c *runtimev3.ContextV3) {
		c.Run.StartedAt = runtimev3.Timestamp(m.Clock.Now().UTC().Format(time.RFC3339))
		c.Run.DeadlineAt = runtimev3.Timestamp(e.deadline.UTC().Format(time.RFC3339))
	})
	e.scope = e.context.Scope()
	e.attempt = m.SeedAttemptV3(e.context, rtest.AllActions())
	var err error
	e.h, err = runtimehttp.NewV3Handler(runtimehttp.V3Config{
		Service: m.Service, Begins: e.begins, Receipts: receiptFake{}, ResourceSecrets: secretFake{},
		Shared: map[string]http.Handler{"resolveSecret": e.shared, "reserveArtifact": e.shared, "completeArtifact": e.shared},
		Direct: map[string]http.Handler{"submitResult": e.direct},
		Logger: newLogger(e.logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.v2, err = runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: m.Service, Shared: map[string]http.Handler{"submitResult": e.shared}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *v3http) do(h http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	case json.RawMessage:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func (e *v3http) admit() string {
	e.t.Helper()
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.RegisterV3With(e.attempt, e.context, cred, rtest.AllActions()); err != nil {
		e.t.Fatal(err)
	}
	w := e.do(e.h, "POST", "/runtime/v3/attempts/"+string(e.context.AttemptID)+"/admit", "", e.AdmitRequest(e.attempt, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
	if w.Code != 200 {
		e.t.Fatalf("admit: %d %s", w.Code, w.Body)
	}
	resp, err := runtimev3.DecodeResponse[runtimev3.AdmitResponseV3](w.Body.Bytes())
	if err != nil {
		e.t.Fatal(err)
	}
	e.token = resp.Token.Token
	return e.token
}

type envelope struct {
	Error struct {
		Code    domain.ErrorCode `json:"code"`
		Details map[string]any   `json:"details"`
	} `json:"error"`
}

func code(t *testing.T, w *httptest.ResponseRecorder) domain.ErrorCode {
	t.Helper()
	var b envelope
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error envelope: %q", w.Body)
	}
	if want, ok := runtimev3.Status(b.Error.Code); !ok || want != w.Code {
		t.Fatalf("code %s with status %d", b.Error.Code, w.Code)
	}
	return b.Error.Code
}

func v3path(op runtimev3.Operation, attempt, run, key, artifact string) string {
	base := runtimev3.AttemptBasePath
	if op.Subject == runtimev3.SubjectResource {
		base = runtimev3.ResourceBasePath
	}
	r := strings.NewReplacer("{attemptId}", attempt, "{runId}", run, "{resourceKey}", key, "{artifactId}", artifact)
	return r.Replace(base + op.Path)
}

func Test_unit_runtime_routes_new_version(t *testing.T) {
	e := newV3HTTP(t, rtest.WriterContext)
	attempt := string(e.context.AttemptID)

	t.Run("the route table is the contract's", func(t *testing.T) {
		// Every operation of the v3 table is served under /runtime/v3 with the v3
		// error envelope: with no credential each answers an error of its inventory
		// (never the catch-all not_found, never a body of another version).
		for _, op := range runtimev3.Operations {
			path := v3path(op, attempt, "run_01", "preparation", "art_0001")
			if op.ID == "resolveResourceSecret" {
				path = v3path(op, attempt, "run_01", "service.db", "art_0001")
			}
			w := e.do(e.h, op.Method, path, "", nil)
			c := code(t, w)
			if op.ID == "admit" || op.ID == "admitResource" {
				// Admission has no bearer: it is judged by its request body.
				if c != domain.CodeInvalidRequest {
					t.Errorf("%s: %s", op.ID, c)
				}
				continue
			}
			if c != domain.CodeInvalidCredential {
				t.Errorf("%s without a credential: %s (want invalid_credential)", op.ID, c)
			}
		}
		if len(e.shared.seen())+len(e.direct.seen()) != 0 {
			t.Fatal("an owner's handler was reached without authority")
		}
	})

	t.Run("other versions and the source route are not served", func(t *testing.T) {
		for _, path := range []string{
			"/runtime/v1/attempts/" + attempt + "/context", "/runtime/v2/attempts/" + attempt + "/begin",
			"/runtime/v3/attempts/" + attempt + "/source", "/runtime/v3/attempts/" + attempt + "/artifacts/art_1/extra",
			"/runtime/v4/attempts/" + attempt + "/context", "/runtime/v3/attempts/" + attempt + "/git/other",
		} {
			if w := e.do(e.h, "POST", path, "", nil); w.Code != 404 || code(t, w) != domain.CodeNotFound {
				t.Errorf("%s: %d %s", path, w.Code, w.Body)
			}
		}
		if w := e.do(e.h, "GET", "/runtime/v3/attempts/"+attempt+"/admit", "", nil); w.Code == 200 {
			t.Error("a wrong method was served")
		}
	})

	tok := e.admit()

	t.Run("admission, renewal, context and the begin fence", func(t *testing.T) {
		w := e.do(e.h, "GET", "/runtime/v3/attempts/"+attempt+"/context", tok, nil)
		if w.Code != 200 {
			t.Fatalf("context: %d %s", w.Code, w.Body)
		}
		got, err := runtimev3.DecodeResponse[runtimev3.ContextV3](w.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		want, _, _ := runtimeauth.PinContextV3(e.context)
		have, _, _ := runtimeauth.PinContextV3(got)
		if want != have {
			t.Fatal("the served context is not the pinned one")
		}
		if got.Git == nil || got.Git.CredentialFile == nil || strings.Contains(w.Body.String(), "gitc_") {
			t.Fatalf("the context names the credential file and holds no credential: %+v", got.Git)
		}
		if w := e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/token", tok, runtimev3.TokenRequest{}); w.Code != 200 {
			t.Fatalf("renew: %d %s", w.Code, w.Body)
		}
		begin := json.RawMessage(rtest.V3Payload(t, "begin_request_v3_two_sources"))
		w = e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/begin", tok, begin)
		if w.Code != 200 || e.begins.called != 1 {
			t.Fatalf("begin: %d %s", w.Code, w.Body)
		}
		resp, err := runtimev3.DecodeResponse[runtimev3.BeginResponseV3](w.Body.Bytes())
		if err != nil || resp.AttemptID != e.context.AttemptID {
			t.Fatalf("begin response: %+v %v", resp, err)
		}
		// A strict body: a source-free observation of a source-backed attempt is not
		// a begin at all (the recorder's derivation refuses it), an unknown member is.
		if w := e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/begin", tok, json.RawMessage(`{"sources":[],"extra":1}`)); w.Code != 400 {
			t.Fatalf("an unknown member: %d", w.Code)
		}
	})

	t.Run("the version fence holds in both directions", func(t *testing.T) {
		// A v3 attempt's token on the v2 routes: mismatch, with nothing done.
		w := e.do(e.v2, "GET", "/runtime/v2/attempts/"+attempt+"/context", tok, nil)
		if w.Code != 409 || code(t, w) != runtimev3.CodeContextVersionMismatch {
			t.Fatalf("v2 context of a v3 attempt: %d %s", w.Code, w.Body)
		}
		if w := e.do(e.v2, "POST", "/runtime/v2/attempts/"+attempt+"/result", tok, json.RawMessage(`{}`)); w.Code != 409 {
			t.Fatalf("v2 shared route of a v3 attempt: %d %s", w.Code, w.Body)
		}
		// A v2 attempt's token on the v3 routes.
		o := rtest.NewV3MemStack(t)
		sc := rtest.LoadV2Scenario(t, "begin_clean_then_result_hash_is_the_v1_hash")
		v2ctx := rtest.ContextV2Of(t, sc.Steps[0])
		a2 := o.SeedAttemptV2(v2ctx, rtest.AllActions())
		cred, err := runtimeauth.GenerateCredential(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.RegisterV2With(a2, v2ctx, cred, rtest.AllActions()); err != nil {
			t.Fatal(err)
		}
		resp, err := o.Service.AdmitV2(context.Background(), v2ctx.AttemptID, o.AdmitRequest(a2, cred, "nnnnnnnnnnnnnnnnnnnnnn"))
		if err != nil {
			t.Fatal(err)
		}
		h3, err := runtimehttp.NewV3Handler(runtimehttp.V3Config{Service: o.Service, Direct: map[string]http.Handler{"submitResult": e.direct}, Shared: map[string]http.Handler{"reserveArtifact": e.shared}})
		if err != nil {
			t.Fatal(err)
		}
		id := string(v2ctx.AttemptID)
		for _, step := range []struct{ method, path string }{
			{"GET", "/runtime/v3/attempts/" + id + "/context"}, {"POST", "/runtime/v3/attempts/" + id + "/token"},
			{"POST", "/runtime/v3/attempts/" + id + "/result"}, {"POST", "/runtime/v3/attempts/" + id + "/artifacts"},
		} {
			w := e.do(h3, step.method, step.path, resp.Token.Token, json.RawMessage(`{}`))
			if w.Code != 409 || code(t, w) != runtimev3.CodeContextVersionMismatch {
				t.Errorf("%s %s of a v2 attempt: %d %s", step.method, step.path, w.Code, w.Body)
			}
		}
		if len(e.direct.seen())+len(e.shared.seen()) != 0 {
			t.Fatal("an owner's handler was reached across the version fence")
		}
	})

	t.Run("owners' handlers are reached through the guard under their paths", func(t *testing.T) {
		for _, c := range []struct{ op, method, path, want string }{
			{"resolveSecret", "POST", "/secrets/resolve", "/runtime/v1/attempts/" + attempt + "/secrets/resolve"},
			{"reserveArtifact", "POST", "/artifacts", "/runtime/v1/attempts/" + attempt + "/artifacts"},
			{"completeArtifact", "POST", "/artifacts/art_0001/complete", "/runtime/v1/attempts/" + attempt + "/artifacts/art_0001/complete"},
		} {
			before := len(e.shared.seen())
			if w := e.do(e.h, c.method, "/runtime/v3/attempts/"+attempt+c.path, tok, json.RawMessage(`{}`)); w.Code != 204 {
				t.Fatalf("%s: %d %s", c.op, w.Code, w.Body)
			}
			if got := e.shared.seen(); len(got) != before+1 || got[before] != c.want {
				t.Errorf("%s reached %v, want %s", c.op, got, c.want)
			}
		}
		if w := e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/result", tok, json.RawMessage(`{}`)); w.Code != 204 {
			t.Fatalf("result: %d %s", w.Code, w.Body)
		}
		if got := e.direct.seen(); len(got) != 1 || got[0] != "/runtime/v3/attempts/"+attempt+"/result" {
			t.Fatalf("the result handler is served natively under the v3 path: %v", got)
		}
		// A cancelled attempt reaches no owner.
		before := len(e.shared.seen()) + len(e.direct.seen())
		if err := e.Env.Cancel(context.Background(), e.scope, e.context.AttemptID, true, e.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		w := e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/artifacts", tok, json.RawMessage(`{}`))
		if w.Code != 403 || code(t, w) != runtimev1.CodeAttemptRevoked {
			t.Fatalf("after cancellation: %d %s", w.Code, w.Body)
		}
		if len(e.shared.seen())+len(e.direct.seen()) != before {
			t.Fatal("an owner's handler was reached after cancellation")
		}
	})
}

func Test_unit_runtime_routes_git_grants(t *testing.T) {
	e := newV3HTTP(t, rtest.ProvisionContext)
	attempt := string(e.context.AttemptID)
	tok := e.admit()
	seedURL := "/runtime/v3/attempts/" + attempt + "/git/seed"
	intURL := "/runtime/v3/attempts/" + attempt + "/git/integration"
	e.Git.PutRepository(e.scope, gitauthtest.NewRepository("repo_new", func(r *gitv1.Repository) { r.Empty = true }))
	seedBody := runtimev3.GitSeedRequest{RepositoryID: "repo_new"}

	// A repository the run did not create: 404, the same answer as an unknown one.
	for _, id := range []string{"repo_new", "repo_nowhere"} {
		w := e.do(e.h, "POST", seedURL, tok, runtimev3.GitSeedRequest{RepositoryID: id})
		if w.Code != 404 || code(t, w) != domain.CodeNotFound {
			t.Fatalf("seed of %s: %d %s", id, w.Code, w.Body)
		}
	}
	// No credential in the route layer's bytes without a bearer; a malformed body.
	if w := e.do(e.h, "POST", seedURL, "", seedBody); w.Code != 401 {
		t.Fatalf("seed without a credential: %d", w.Code)
	}
	if w := e.do(e.h, "POST", seedURL, tok, json.RawMessage(`{"repositoryId":"repo_new","extra":true}`)); w.Code != 400 {
		t.Fatalf("seed with an unknown member: %d", w.Code)
	}
	// An integration grant for a provisioning run: forbidden, no grant exists.
	w := e.do(e.h, "POST", intURL, tok, runtimev3.GitIntegrationRequest{RepositoryID: "repo_new", GateID: "gate_07"})
	if w.Code != 403 || code(t, w) != domain.CodeForbidden {
		t.Fatalf("integration of a provisioning run: %d %s", w.Code, w.Body)
	}
	if len(e.Git.Issued) != 0 {
		t.Fatalf("a grant was issued: %+v", e.Git.Issued)
	}

	e.Git.Create(e.scope, "run_01", "repo_new")
	e.Git.SetUnsettled(e.scope, "repo_new", true)
	if w := e.do(e.h, "POST", seedURL, tok, seedBody); w.Code != 409 || code(t, w) != gitv1.CodeSeedPending {
		t.Fatalf("an unsettled push: %d %s", w.Code, w.Body)
	}
	e.Git.SetUnsettled(e.scope, "repo_new", false)
	w = e.do(e.h, "POST", seedURL, tok, seedBody)
	if w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the credential must not be cached: %v", w.Header())
	}
	resp, err := runtimev3.DecodeResponse[runtimev3.GitGrantResponse](w.Body.Bytes())
	if err != nil || resp.CredentialFile != "/kodama/git/seed-credential" || resp.Grant.Kind != gitv1.GrantSeed {
		t.Fatalf("seed response: %+v %v", resp, err)
	}
	if strings.Contains(e.logs.String(), resp.Credential) || strings.Contains(e.logs.String(), tok) {
		t.Fatal("a credential or token reached the logs")
	}
	if !strings.Contains(e.logs.String(), "seedGitGrant") {
		t.Fatalf("the operation was not logged: %q", e.logs.String())
	}
	_ = tok
}

func Test_unit_runtime_routes_integration_conflict(t *testing.T) {
	e := newV3HTTP(t, rtest.WriterContext)
	attempt := string(e.context.AttemptID)
	tok := e.admit()
	url := "/runtime/v3/attempts/" + attempt + "/git/integration"
	req := runtimev3.GitIntegrationRequest{RepositoryID: "repo_01", GateID: "gate_07"}
	hexA, hexC := strings.Repeat("1", 40), strings.Repeat("3", 40)
	approval := runtimev3.IntegrationApproval{GateID: "gate_07", RepositoryID: "repo_01", Ref: "refs/heads/main", Base: hexA, Candidate: hexC, CurrentTip: hexA}

	if w := e.do(e.h, "POST", url, tok, req); w.Code != 403 || code(t, w) != domain.CodeForbidden {
		t.Fatalf("unapproved: %d %s", w.Code, w.Body)
	}
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalStale{SupersedingCommit: hexC})
	w := e.do(e.h, "POST", url, tok, req)
	var b envelope
	if w.Code != 409 || code(t, w) != runtimev1.CodePublicationConflict || json.Unmarshal(w.Body.Bytes(), &b) != nil ||
		b.Error.Details["reason"] != "base_moved" || b.Error.Details["observedTip"] != hexC {
		t.Fatalf("a moved base: %d %s", w.Code, w.Body)
	}
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, &runtimeauth.ApprovalApplied{Approval: approval})
	w = e.do(e.h, "POST", url, tok, req)
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &b) != nil || b.Error.Details["reason"] != "already_integrated" {
		t.Fatalf("already integrated: %d %s", w.Code, w.Body)
	}
	if len(e.Git.Issued) != 0 {
		t.Fatal("a grant was issued without a current approval")
	}
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", approval, nil)
	w = e.do(e.h, "POST", url, tok, req)
	if w.Code != 200 {
		t.Fatalf("integration: %d %s", w.Code, w.Body)
	}
	resp, err := runtimev3.DecodeResponse[runtimev3.GitGrantResponse](w.Body.Bytes())
	if err != nil || resp.Grant.Kind != gitv1.GrantIntegration || resp.CredentialFile != "/kodama/git/integration-credential" {
		t.Fatalf("integration response: %+v %v", resp, err)
	}
	// A seed from a work-item attempt: forbidden.
	if w := e.do(e.h, "POST", "/runtime/v3/attempts/"+attempt+"/git/seed", tok, runtimev3.GitSeedRequest{RepositoryID: "repo_01"}); w.Code != 403 {
		t.Fatalf("seed of a work-item attempt: %d %s", w.Code, w.Body)
	}
	// An error body carries a fixed message, never a cause.
	e.Git.Approve(e.scope, "run_01", "gate_07", "repo_01", runtimev3.IntegrationApproval{}, context.DeadlineExceeded)
	w = e.do(e.h, "POST", url, tok, req)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || strings.Contains(w.Body.String(), "deadline") {
		t.Fatalf("an outage: %d %s", w.Code, w.Body)
	}
}

func Test_unit_runtime_routes_resources(t *testing.T) {
	m := rtest.NewV3MemStack(t)
	logs := &rtest.SafeBuffer{}
	h, err := runtimehttp.NewV3Handler(runtimehttp.V3Config{Service: m.Service, Receipts: receiptFake{}, ResourceSecrets: secretFake{}, Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	e := &v3http{t: t}
	f := rtest.ResourceFixture{Scope: scope, RunID: "run_01", WorkloadUID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", DeadlineAt: m.Clock.Now().Add(time.Hour)}
	st := rtest.PreparationStateV3(t, f)
	m.Resources.Put(st)
	ctx := context.Background()
	cred, _ := runtimeauth.GenerateCredential(nil)
	if err := m.Service.RegisterResource(ctx, runtimeauth.ResourceRegisterCommand{Scope: scope, RunID: "run_01", Key: "preparation", Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}); err != nil {
		t.Fatal(err)
	}
	base := "/runtime/v3/runs/run_01/resources/preparation"
	adm := runtimev2.ResourceAdmitRequest{AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.4.0", Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID}}
	w := e.do(h, "POST", base+"/admit", "", adm)
	if w.Code != 200 {
		t.Fatalf("admit: %d %s", w.Code, w.Body)
	}
	resp, err := runtimev3.DecodeResponse[runtimev3.ResourceAdmitResponseV3](w.Body.Bytes())
	if err != nil || resp.Context.Preparation == nil {
		t.Fatalf("admit response: %+v %v", resp, err)
	}
	tok := resp.Token.Token
	if w := e.do(h, "GET", base+"/context", tok, nil); w.Code != 200 {
		t.Fatalf("context: %d %s", w.Code, w.Body)
	}
	if w := e.do(h, "POST", base+"/token", tok, runtimev3.ResourceTokenRequest{}); w.Code != 200 {
		t.Fatalf("renew: %d %s", w.Code, w.Body)
	}
	receipt := json.RawMessage(rtest.V3Payload(t, "preparation_receipt_v3_every_source"))
	if w := e.do(h, "POST", base+"/preparation/complete", tok, receipt); w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	// A receipt that is not the committed sources', and a service secret route for
	// a preparation, are refused before anything is recorded.
	wrong, err := runtimev3.DecodeRequest[runtimev3.PreparationReceiptRequestV3](receipt)
	if err != nil {
		t.Fatal(err)
	}
	wrong.Sources[0].Path = "elsewhere"
	bad, _ := json.Marshal(wrong)
	if w := e.do(h, "POST", base+"/preparation/complete", tok, bad); w.Code != 400 {
		t.Fatalf("a receipt of other sources: %d %s", w.Code, w.Body)
	}
	if w := e.do(h, "POST", base+"/secrets/resolve", tok, json.RawMessage(`{"ref":"secret://project/x","version":1,"name":"x"}`)); w.Code == 200 {
		t.Fatalf("a preparation resolved a secret: %d", w.Code)
	}
	// The v2 route family refuses the v3 resource.
	h2, _ := runtimehttp.NewV2Handler(runtimehttp.V2Config{Service: m.Service})
	if w := e.do(h2, "GET", "/runtime/v2/runs/run_01/resources/preparation/context", tok, nil); w.Code != 409 || code(t, w) != runtimev3.CodeContextVersionMismatch {
		t.Fatalf("v2 context of a v3 resource: %d %s", w.Code, w.Body)
	}
	// A stopped resource loses its authority.
	m.Resources.Stop(runtimeauth.ResourceRef{Scope: scope, RunID: "run_01", Key: "preparation"}, false, m.Clock.Now())
	if w := e.do(h, "GET", base+"/context", tok, nil); w.Code != 403 || code(t, w) != runtimev3.CodeResourceRevoked {
		t.Fatalf("after stop: %d %s", w.Code, w.Body)
	}
}
