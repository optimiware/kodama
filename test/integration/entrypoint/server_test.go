package entrypoint_test

// The in-process contract server: the eight runtime routes of
// contracts/runtime/v1 with strict decoding (runtimev1.DecodeRequest), result
// validation (runtimev1.CheckResult and the canonical hash), an artifact
// object store that verifies digests, and an optional Git publisher that
// applies the proposed bundle to a bare repository with an expected-tip CAS.
// It is a test double of the control plane, not production evidence; the
// behavior it exercises is the entrypoint's.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// action is what an interceptor decides for one request.
type action struct {
	errCode domain.ErrorCode // reply with this contract error, without processing
	details map[string]any   // error details (for example observedTip)
	lose    bool             // process the request, then drop the connection
	garbage bool             // reply 200 with a non-contract body, without processing
	status  int              // reply with this bare status (for example 503), without processing
	pre     func()           // runs under the server lock before processing
}

// interceptor is consulted for every request; n counts requests of op from 1.
type interceptor func(op string, n int, body []byte) action

type call struct {
	op          string
	n           int
	body        []byte
	token       string
	afterRevoke bool
}

type artifactRec struct {
	id     domain.ArtifactID
	req    runtimev1.ArtifactReserveRequest
	usable bool
	bytes  []byte
	has    bool
}

type acceptedResult struct {
	req runtimev1.ResultRequest
	ack runtimev1.ResultResponse
}

type contractServer struct {
	t *testing.T
	*httptest.Server
	store *httptest.Server

	mu         sync.Mutex
	ctx        runtimev1.Context
	job        runtimev1.JobIdentity
	admission  string
	secrets    map[string]string // ref -> value
	ttl        time.Duration
	maxExpires time.Time

	nonce       string
	token       string
	tokens      []string
	revoked     bool
	unsupported bool // admit answers profile_unsupported
	intercept   interceptor

	counts    map[string]int
	calls     []call
	artifacts map[domain.ArtifactID]*artifactRec
	byName    map[string]domain.ArtifactID
	nextArt   int
	uploads   int
	proposals []runtimev1.SourceProposalRequest
	proposed  [][]byte // raw proposal bodies
	submits   [][]byte // raw result bodies
	accepted  *acceptedResult
	replays   int

	// source publication
	pub            *publisher
	pendingLeft    int // proposals answered 202 before the publication is applied
	pendingForever bool
	publishedAt    int // number of calls recorded when the publication was applied (0 = never)
	order          []string
	uploadStatus   func(n int) int
}

func newContractServer(t *testing.T, c runtimev1.Context) *contractServer {
	t.Helper()
	s := &contractServer{
		t: t, ctx: c, admission: "adm_integration_credential_0123456789",
		job: runtimev1.JobIdentity{
			Name: "kodama-att-it01", UID: "6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
			PodName: "kodama-att-it01-x7k2p", PodUID: "7a2d2e3f-4b5c-4d6e-9fa0-1b2c3d4e5f60",
		},
		secrets: map[string]string{}, ttl: time.Hour, maxExpires: time.Now().Add(2 * time.Hour),
		counts: map[string]int{}, artifacts: map[domain.ArtifactID]*artifactRec{}, byName: map[string]domain.ArtifactID{},
	}
	s.store = httptest.NewServer(http.HandlerFunc(s.serveStore))
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() { s.Server.Close(); s.store.Close() })
	return s
}

// ---- state accessors for tests ----

func (s *contractServer) with(f func()) { s.mu.Lock(); defer s.mu.Unlock(); f() }

func (s *contractServer) Revoke()                 { s.with(func() { s.revoked = true }) }
func (s *contractServer) Intercept(i interceptor) { s.with(func() { s.intercept = i }) }

func (s *contractServer) Count(op string) (n int) {
	s.with(func() { n = s.counts[op] })
	return
}

func (s *contractServer) Calls() (out []call) {
	s.with(func() { out = append(out, s.calls...) })
	return
}

func (s *contractServer) Ops() (ops []string) {
	for _, c := range s.Calls() {
		ops = append(ops, c.op)
	}
	return
}

func (s *contractServer) CallsAfterRevoke() (out []string) {
	for _, c := range s.Calls() {
		if c.afterRevoke {
			out = append(out, c.op)
		}
	}
	return
}

func (s *contractServer) Accepted() (r *runtimev1.ResultRequest) {
	s.with(func() {
		if s.accepted != nil {
			c := s.accepted.req
			r = &c
		}
	})
	return
}

func (s *contractServer) Submits() (out [][]byte) {
	s.with(func() { out = append(out, s.submits...) })
	return
}

func (s *contractServer) Proposals() (out []runtimev1.SourceProposalRequest) {
	s.with(func() { out = append(out, s.proposals...) })
	return
}

func (s *contractServer) ProposalBodies() (out [][]byte) {
	s.with(func() { out = append(out, s.proposed...) })
	return
}

func (s *contractServer) Replays() (n int) {
	s.with(func() { n = s.replays })
	return
}

func (s *contractServer) Tokens() (out []string) {
	s.with(func() { out = append(out, s.tokens...) })
	return
}

// Artifacts returns the reserved artifacts, optionally filtered by role.
func (s *contractServer) Artifacts(role runtimev1.ArtifactRole) (out []artifactRec) {
	s.with(func() {
		for _, a := range s.artifacts {
			if role == "" || a.req.Role == role {
				c := *a
				c.bytes = append([]byte(nil), a.bytes...)
				out = append(out, c)
			}
		}
	})
	return
}

// UploadedBytes returns every uploaded object keyed by artifact name.
func (s *contractServer) UploadedBytes() map[string][]byte {
	out := map[string][]byte{}
	for _, a := range s.Artifacts("") {
		if a.has {
			out[a.req.Name+"/"+string(a.id)] = a.bytes
		}
	}
	return out
}

// Order is the sequence of recorded milestones ("source_accepted", "result").
func (s *contractServer) Order() (o []string) {
	s.with(func() { o = append(o, s.order...) })
	return
}

// ---- HTTP plumbing ----

type reply struct {
	status int
	body   any
}

func errReply(code domain.ErrorCode, msg string, details map[string]any) reply {
	st, ok := runtimev1.Status(code)
	if !ok {
		st = http.StatusInternalServerError
	}
	if details == nil {
		details = map[string]any{}
	}
	return reply{st, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: msg, RequestID: "req-it", Details: details}}}
}

func contractErr(err error) reply {
	if ce, ok := err.(*runtimev1.ContractError); ok {
		return errReply(ce.Code, ce.Message, nil)
	}
	return errReply(domain.CodeInvalidRequest, err.Error(), nil)
}

func decodeReq[T any, P interface {
	*T
	Validate() error
}](body []byte) (T, *reply) {
	v, err := runtimev1.DecodeRequest[T, P](body)
	if err != nil {
		r := contractErr(err)
		var zero T
		return zero, &r
	}
	return v, nil
}

var opByRoute = func() map[string]string {
	m := map[string]string{}
	for _, o := range runtimev1.Operations {
		m[o.Method+" "+o.Path] = o.ID
	}
	return m
}()

func (s *contractServer) route(r *http.Request) (op, artifactID string) {
	prefix := "/runtime/v1/attempts/" + string(s.ctx.AttemptID)
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	if !ok {
		return "", ""
	}
	if strings.HasPrefix(rest, "/artifacts/") && strings.HasSuffix(rest, "/complete") {
		id := strings.TrimSuffix(strings.TrimPrefix(rest, "/artifacts/"), "/complete")
		return opByRoute[r.Method+" /artifacts/{artifactId}/complete"], id
	}
	return opByRoute[r.Method+" "+rest], ""
}

func (s *contractServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	op, artifactID := s.route(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	var rep reply
	var act action
	switch {
	case op == "":
		rep = errReply(domain.CodeNotFound, "no such route", nil)
	default:
		s.counts[op]++
		n := s.counts[op]
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.calls = append(s.calls, call{op: op, n: n, body: body, token: token, afterRevoke: s.revoked})
		if s.intercept != nil {
			act = s.intercept(op, n, body)
		}
		if act.pre != nil {
			act.pre()
		}
		switch {
		case act.errCode != "":
			rep = errReply(act.errCode, "injected "+string(act.errCode), act.details)
		case act.status != 0:
			rep = reply{act.status, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: domain.CodeUnavailable, Message: "injected", RequestID: "req-it", Details: map[string]any{}}}}
		case act.garbage:
			rep = reply{http.StatusOK, map[string]any{"unexpected": true}}
		default:
			rep = s.handle(op, artifactID, token, body)
		}
	}
	if act.lose {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	out, err := json.Marshal(rep.body)
	if err != nil {
		s.t.Errorf("marshal reply: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = w.Write(out)
}

func (s *contractServer) issueToken() runtimev1.TokenInfo {
	var rnd [16]byte
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(s.tokens))))
	copy(rnd[:], sum[:])
	s.token = "rt_" + hex.EncodeToString(rnd[:])
	s.tokens = append(s.tokens, s.token)
	exp := time.Now().Add(s.ttl)
	if exp.After(s.maxExpires) {
		exp = s.maxExpires
	}
	return runtimev1.TokenInfo{
		Token: s.token, TokenType: "Bearer", Audience: runtimev1.Audience,
		ExpiresAt:    runtimev1.Timestamp(exp.UTC().Format(time.RFC3339Nano)),
		MaxExpiresAt: runtimev1.Timestamp(s.maxExpires.UTC().Format(time.RFC3339Nano)),
	}
}

func (s *contractServer) handle(op, artifactID, token string, body []byte) reply {
	if op != "admit" {
		if token == "" || token != s.token {
			return errReply(domain.CodeInvalidCredential, "bad bearer token", nil)
		}
		if s.revoked {
			return errReply(runtimev1.CodeAttemptRevoked, "attempt revoked", nil)
		}
	}
	switch op {
	case "admit":
		req, bad := decodeReq[runtimev1.AdmitRequest](body)
		if bad != nil {
			return *bad
		}
		switch {
		case req.AdmissionCredential != s.admission:
			return errReply(domain.CodeInvalidCredential, "bad admission credential", nil)
		case s.revoked:
			return errReply(runtimev1.CodeAttemptRevoked, "attempt revoked", nil)
		case req.Job != s.job:
			return errReply(domain.CodeForbidden, "job identity mismatch", nil)
		case s.unsupported:
			return errReply(runtimev1.CodeProfileUnsupported, "wrapper profile unsupported", nil)
		case s.nonce != "" && s.nonce != req.Nonce:
			return errReply(runtimev1.CodeAdmissionConsumed, "admission already consumed", nil)
		}
		s.nonce = req.Nonce
		return reply{http.StatusOK, runtimev1.AdmitResponse{Token: s.issueToken(), Context: s.ctx}}
	case "renewToken":
		if _, bad := decodeReq[runtimev1.TokenRequest](body); bad != nil {
			return *bad
		}
		return reply{http.StatusOK, runtimev1.TokenResponse{Token: s.issueToken()}}
	case "getContext":
		return reply{http.StatusOK, s.ctx}
	case "resolveSecret":
		req, bad := decodeReq[runtimev1.SecretResolveRequest](body)
		if bad != nil {
			return *bad
		}
		for _, b := range s.ctx.Secrets {
			if b.Ref == req.Ref && b.Version == req.Version {
				v, ok := s.secrets[b.Ref]
				if !ok {
					return errReply(domain.CodeNotFound, "secret not found", nil)
				}
				return reply{http.StatusOK, runtimev1.SecretResolveResponse{
					Ref: b.Ref, Version: b.Version, Name: b.Name, Delivery: b.Delivery,
					ValueBase64: base64.StdEncoding.EncodeToString([]byte(v)),
				}}
			}
		}
		return errReply(domain.CodeForbidden, "secret is not declared for this attempt", nil)
	case "reserveArtifact":
		return s.reserve(body)
	case "completeArtifact":
		return s.complete(artifactID, body)
	case "proposeSource":
		return s.propose(body)
	case "submitResult":
		return s.submit(body)
	}
	return errReply(domain.CodeNotFound, "no such operation", nil)
}

func (s *contractServer) reserve(body []byte) reply {
	req, bad := decodeReq[runtimev1.ArtifactReserveRequest](body)
	if bad != nil {
		return *bad
	}
	if id, ok := s.byName[req.Name+"|"+string(req.Role)]; ok {
		prev := s.artifacts[id]
		if fmt.Sprint(prev.req) != fmt.Sprint(req) {
			return errReply(domain.CodeIdempotencyConflict, "artifact name reused with different metadata", nil)
		}
		return reply{http.StatusOK, s.reserveResponse(prev)}
	}
	s.nextArt++
	id := domain.ArtifactID(fmt.Sprintf("art_it%03d", s.nextArt))
	a := &artifactRec{id: id, req: req}
	if req.Kind == runtimev1.KindOCI {
		a.usable = true
	}
	s.artifacts[id] = a
	s.byName[req.Name+"|"+string(req.Role)] = id
	return reply{http.StatusCreated, s.reserveResponse(a)}
}

func (s *contractServer) reserveResponse(a *artifactRec) runtimev1.ArtifactReserveResponse {
	if a.req.Kind == runtimev1.KindOCI {
		return runtimev1.ArtifactReserveResponse{ArtifactID: a.id, State: runtimev1.StateRegistered}
	}
	return runtimev1.ArtifactReserveResponse{ArtifactID: a.id, State: runtimev1.StateReserved, Upload: &runtimev1.Upload{
		Method: "PUT", URL: s.store.URL + "/objects/" + string(a.id), Headers: map[string]string{"X-Kodama-Upload": "1"},
		ExpiresAt: runtimev1.Timestamp(time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)),
	}}
}

func digestOf(b []byte) runtimev1.Digest {
	sum := sha256.Sum256(b)
	return runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func (s *contractServer) complete(id string, body []byte) reply {
	req, bad := decodeReq[runtimev1.ArtifactCompleteRequest](body)
	if bad != nil {
		return *bad
	}
	a, ok := s.artifacts[domain.ArtifactID(id)]
	if !ok || a.req.Kind != runtimev1.KindFile {
		return errReply(domain.CodeNotFound, "artifact not found", nil)
	}
	if !a.has {
		return errReply(runtimev1.CodeArtifactIncomplete, "no object uploaded", nil)
	}
	if digestOf(a.bytes) != req.Digest || req.Digest != a.req.Digest || int64(len(a.bytes)) != req.SizeBytes || req.SizeBytes != *a.req.SizeBytes {
		return errReply(runtimev1.CodeDigestMismatch, "uploaded object does not match the reservation", nil)
	}
	a.usable = true
	return reply{http.StatusOK, runtimev1.ArtifactCompleteResponse{ArtifactID: a.id, State: runtimev1.StateUsable, Digest: req.Digest, SizeBytes: req.SizeBytes}}
}

func (s *contractServer) serveStore(w http.ResponseWriter, r *http.Request) {
	id := domain.ArtifactID(strings.TrimPrefix(r.URL.Path, "/objects/"))
	if r.Method != http.MethodPut || r.Header.Get("X-Kodama-Upload") != "1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "" {
		s.t.Errorf("upload carried an Authorization header")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads++
	if s.uploadStatus != nil {
		if st := s.uploadStatus(s.uploads); st != 0 {
			w.WriteHeader(st)
			return
		}
	}
	a, ok := s.artifacts[id]
	if !ok || err != nil || a.req.Kind != runtimev1.KindFile {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	a.bytes, a.has = data, true
	w.WriteHeader(http.StatusOK)
}

func (s *contractServer) metadata() map[domain.ArtifactID]runtimev1.ArtifactRef {
	m := map[domain.ArtifactID]runtimev1.ArtifactRef{}
	for id, a := range s.artifacts {
		m[id] = runtimev1.ArtifactRef{ID: id, Scope: s.ctx.Scope(), Attempt: s.ctx.AttemptID, Kind: a.req.Kind, Usable: a.usable}
	}
	return m
}

func (s *contractServer) propose(body []byte) reply {
	req, bad := decodeReq[runtimev1.SourceProposalRequest](body)
	if bad != nil {
		return *bad
	}
	s.proposals = append(s.proposals, req)
	s.proposed = append(s.proposed, append([]byte(nil), body...))
	w := s.ctx.Workspace
	if w.Access != runtimev1.AccessWrite || s.pub == nil {
		return errReply(domain.CodeForbidden, "this attempt cannot publish source", nil)
	}
	if req.ExpectedTip != *w.Source.ExpectedTip {
		return errReply(domain.CodeInvalidRequest, "expectedTip differs from the attempt context", nil)
	}
	a, ok := s.artifacts[req.BundleArtifactID]
	if !ok || a.req.Role != runtimev1.RoleBundle {
		return errReply(domain.CodeNotFound, "bundle artifact not found", nil)
	}
	if !a.usable {
		return errReply(runtimev1.CodeArtifactIncomplete, "bundle upload is not complete", nil)
	}
	if len(s.proposals) > 1 && string(s.proposed[0]) != string(body) {
		return errReply(domain.CodeInvalidRequest, "a repeated proposal must be identical", nil)
	}
	if s.pendingForever || s.pendingLeft > 0 {
		if s.pendingLeft > 0 {
			s.pendingLeft--
		}
		return reply{http.StatusAccepted, runtimev1.SourceProposalResponse{Status: runtimev1.PublishPending, Commit: req.Commit}}
	}
	observed, err := s.pub.apply(req, a.bytes, string(s.ctx.AttemptID))
	switch {
	case err != nil:
		s.t.Errorf("publisher: %v", err)
		return errReply(domain.CodeInvalidRequest, "bundle could not be applied", nil)
	case observed != "":
		return errReply(runtimev1.CodePublicationConflict, "the branch moved", map[string]any{"observedTip": observed})
	}
	if s.publishedAt == 0 {
		s.publishedAt = len(s.calls)
		s.order = append(s.order, "source_accepted")
	}
	c := req.Commit
	return reply{http.StatusOK, runtimev1.SourceProposalResponse{Status: runtimev1.PublishAccepted, Commit: req.Commit, AcceptedCommit: &c}}
}

func (s *contractServer) submit(body []byte) reply {
	s.submits = append(s.submits, append([]byte(nil), body...))
	req, bad := decodeReq[runtimev1.ResultRequest](body) // recomputes the hash: result_hash_mismatch
	if bad != nil {
		return *bad
	}
	if err := runtimev1.CheckResult(req.Result, s.ctx, s.metadata()); err != nil {
		return contractErr(err)
	}
	// Writer success needs the exact commit to be accepted by the publisher
	// (or to be the unchanged expected tip).
	if sc := req.Result.SourceCommit; sc != nil {
		unchanged := *sc == *s.ctx.Workspace.Source.ExpectedTip
		published := s.pub != nil && s.publishedAt != 0 && s.pub.tipNow() == string(*sc)
		if !unchanged && !published {
			return errReply(runtimev1.CodeOutputInvalid, "sourceCommit was not published", nil)
		}
	}
	if s.accepted != nil {
		if s.accepted.req.ResultHash != req.ResultHash {
			return errReply(runtimev1.CodeResultConflict, "a different result was accepted", nil)
		}
		s.replays++
		return reply{http.StatusOK, s.accepted.ack}
	}
	status := runtimev1.AttemptFailed
	if req.Result.Outcome == runtimev1.OutcomeCompleted {
		status = runtimev1.AttemptSucceeded
	}
	ack := runtimev1.ResultResponse{
		AttemptID: s.ctx.AttemptID, ResultHash: req.ResultHash, AttemptStatus: status,
		AcceptedAt: runtimev1.Timestamp(time.Now().UTC().Format(time.RFC3339Nano)),
	}
	s.accepted = &acceptedResult{req: req, ack: ack}
	s.order = append(s.order, "result")
	return reply{http.StatusOK, ack}
}

// preAccept makes the server already hold an accepted result with this hash.
func (s *contractServer) preAccept(hash runtimev1.Digest) {
	s.with(func() {
		s.accepted = &acceptedResult{
			req: runtimev1.ResultRequest{ResultHash: hash},
			ack: runtimev1.ResultResponse{AttemptID: s.ctx.AttemptID, ResultHash: hash, AttemptStatus: runtimev1.AttemptFailed,
				AcceptedAt: runtimev1.Timestamp("2026-10-19T10:00:00Z")},
		}
	})
}

// ---- Git publisher ----

// publisher applies a proposed bundle to a bare repository: it verifies the
// proposed commit arrives in the bundle and moves the branch with a
// compare-and-swap on the expected tip.
type publisher struct {
	bare   string
	branch string
}

func (p *publisher) tipNow() string {
	out, err := gitOut(p.bare, "rev-parse", "--verify", p.branch)
	if err != nil {
		return ""
	}
	return out
}

// apply returns "" on success (or an identical replay), or the observed tip
// when the branch moved. A non-nil error is a defect of the proposal.
func (p *publisher) apply(req runtimev1.SourceProposalRequest, bundle []byte, attempt string) (string, error) {
	tip := p.tipNow()
	if tip == string(req.Commit) {
		return "", nil // replay of an applied proposal
	}
	if tip != string(req.ExpectedTip) {
		return tip, nil
	}
	f := filepath.Join(os.TempDir(), "kodama-it-"+attempt+".bundle")
	if err := os.WriteFile(f, bundle, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(f)
	ref := "refs/kodama/proposals/" + attempt
	if _, err := gitOut(p.bare, "fetch", "--quiet", f, "+refs/kodama/candidate:"+ref); err != nil {
		return "", fmt.Errorf("fetch bundle: %w", err)
	}
	got, err := gitOut(p.bare, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || got != string(req.Commit) {
		return "", fmt.Errorf("bundle holds %q, not %s (%v)", got, req.Commit, err)
	}
	if _, err := gitOut(p.bare, "update-ref", p.branch, string(req.Commit), string(req.ExpectedTip)); err != nil {
		return p.tipNow(), nil
	}
	return "", nil
}
