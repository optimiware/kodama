package runtimehttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The runtime v3 routes (T-150): the source-set wire of a run_shared run.
//
//	/runtime/v3/attempts/{attemptId}/{admit,token,context,begin,secrets/resolve,
//	                                  git/seed,git/integration,artifacts,
//	                                  artifacts/{id}/complete,result}
//	/runtime/v3/runs/{runId}/resources/{resourceKey}/{admit,token,context,
//	                                  secrets/resolve,preparation/complete}
//
// Admission, renewal, context, the begin fence, the two runtime Git grant routes
// and the resource routes are served here from the module's use cases. Secrets
// and artifacts have the v1 bodies and are served by their owners' v1 handlers
// behind a WireV3 guard (Shared); the result has its own v3 body and is served by
// the result module's v3 handler under the v3 path behind the same guard
// (Direct). There is no source-proposal route: an internally hosted source is
// pushed by the entrypoint with a Git grant and reported in the result.

// BeginRecorderV3 is the consumer-owned port of the v3 command-begin fence;
// execution's RecordBeginV3 is its implementation (runtimeauth/execbridge). Its
// failures are classified like BeginRecorder's.
type BeginRecorderV3 interface {
	RecordBeginV3(ctx context.Context, auth runtimeauth.Authorization, req runtimev3.BeginRequestV3) (BeginResult, error)
}

// ReceiptRecorderV3 is the consumer-owned port of the v3 preparation receipt
// (one entry per source); execution's RecordPreparationReceiptV3 is its
// implementation. Failures as ReceiptRecorder's.
type ReceiptRecorderV3 interface {
	RecordPreparationReceiptV3(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev3.PreparationReceiptRequestV3) (ReceiptResult, error)
}

// V3Service is the consumer-owned port of the v3 handlers; *runtimeauth.Service
// implements it.
type V3Service interface {
	AdmitV3(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (runtimev3.AdmitResponseV3, error)
	RenewV3(ctx context.Context, route domain.AttemptID, bearer string) (runtimev2.TokenResponse, error)
	ContextV3(ctx context.Context, route domain.AttemptID, bearer string) (runtimev3.ContextV3, error)
	Authorize(ctx context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error)
	Reauthorize(ctx context.Context, a runtimeauth.Authorization) error
	SeedGitGrant(ctx context.Context, a runtimeauth.Authorization, req runtimev3.GitSeedRequest) (runtimev3.GitGrantResponse, error)
	IntegrationGitGrant(ctx context.Context, a runtimeauth.Authorization, req runtimev3.GitIntegrationRequest) (runtimev3.GitGrantResponse, error)

	AdmitResourceV3(ctx context.Context, route runtimeauth.ResourceRef, req runtimev2.ResourceAdmitRequest) (runtimev3.ResourceAdmitResponseV3, error)
	RenewResourceV3(ctx context.Context, run domain.RunID, key, bearer string) (runtimev2.ResourceTokenResponse, error)
	ResourceContextV3(ctx context.Context, run domain.RunID, key, bearer string) (runtimev3.ResourceContextV3, error)
	AuthorizeResource(ctx context.Context, req runtimeauth.ResourceAuthorizeRequest) (runtimeauth.ResourceAuthorization, error)
	ReauthorizeResource(ctx context.Context, a runtimeauth.ResourceAuthorization) error
}

var _ V3Service = (*runtimeauth.Service)(nil)

// SharedOpsV3 are the attempt operations whose bodies are shared with v1 and
// served by the v1 owners' handlers; DirectOpsV3 are served natively under the v3
// path by another owner's handler (the result module's v3 handler).
var (
	SharedOpsV3 = []string{"resolveSecret", "reserveArtifact", "completeArtifact"}
	DirectOpsV3 = []string{"submitResult"}
)

// V3Config configures NewV3Handler. A route whose collaborator is nil is not
// wired: it answers the contract's not_found envelope, never a silent success.
type V3Config struct {
	Service         V3Service
	Begins          BeginRecorderV3
	Receipts        ReceiptRecorderV3
	ResourceSecrets ResourceSecretResolver
	// Shared maps a shared operation ID (SharedOpsV3) to the v1 handler serving its
	// body, invoked with the v1 path of the same operation.
	Shared map[string]http.Handler
	// Direct maps a direct operation ID (DirectOpsV3) to the handler serving it
	// under its v3 path.
	Direct map[string]http.Handler
	Logger *slog.Logger
}

type v3handler struct {
	*handler
	svc      V3Service
	begins   BeginRecorderV3
	receipts ReceiptRecorderV3
	secrets  ResourceSecretResolver
}

// NewV3Handler returns the handler of the /runtime/v3 routes.
func NewV3Handler(cfg V3Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("runtimehttp: a v3 service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for op := range cfg.Shared {
		if !slices.Contains(SharedOpsV3, op) {
			return nil, errors.New("runtimehttp: " + op + " is not an operation with a body shared with v1")
		}
	}
	for op := range cfg.Direct {
		if !slices.Contains(DirectOpsV3, op) {
			return nil, errors.New("runtimehttp: " + op + " is not an operation served natively on the v3 wire")
		}
	}
	h := &v3handler{handler: &handler{log: logger}, svc: cfg.Service, begins: cfg.Begins, receipts: cfg.Receipts, secrets: cfg.ResourceSecrets}
	guard := &WireGuard{svc: cfg.Service, wire: runtimeauth.WireV3, h: h.handler}
	mux := http.NewServeMux()
	for _, op := range runtimev3.Operations {
		var fn http.Handler
		switch op.ID {
		case "admit":
			fn = http.HandlerFunc(h.admit)
		case "renewToken":
			fn = http.HandlerFunc(h.renew)
		case "getContext":
			fn = http.HandlerFunc(h.context)
		case "begin":
			if h.begins != nil {
				fn = http.HandlerFunc(h.begin)
			}
		case "seedGitGrant":
			fn = http.HandlerFunc(h.seed)
		case "integrationGitGrant":
			fn = http.HandlerFunc(h.integration)
		case "admitResource":
			fn = http.HandlerFunc(h.admitResource)
		case "renewResourceToken":
			fn = http.HandlerFunc(h.renewResource)
		case "getResourceContext":
			fn = http.HandlerFunc(h.resourceContext)
		case "resolveResourceSecret":
			if h.secrets != nil {
				fn = http.HandlerFunc(h.resolveResourceSecret)
			}
		case "completePreparation":
			if h.receipts != nil {
				fn = http.HandlerFunc(h.completePreparation)
			}
		case "submitResult":
			if next, ok := cfg.Direct[op.ID]; ok && next != nil {
				fn = guard.Wrap(op.ID, next)
			}
		default:
			if next, ok := cfg.Shared[op.ID]; ok && next != nil {
				fn = guard.Wrap(op.ID, rewriteToV1(op.ID, next))
			}
		}
		if fn == nil {
			continue
		}
		mux.Handle(op.Method+" "+v3Pattern(op), fn)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError3(w, newRequestID(), domain.CodeNotFound, nil)
	})
	return mux, nil
}

func v3Pattern(op runtimev3.Operation) string {
	if op.Subject == runtimev3.SubjectResource {
		return runtimev3.ResourceBasePath + op.Path
	}
	return runtimev3.AttemptBasePath + op.Path
}

// ---- error rendering of the v3 envelope -----------------------------------------

// v3Code maps an error to a code the operation's v3 inventory permits. A code the
// inventory excludes, or an unclassified failure, is unavailable: fail closed.
func v3Code(opID string, err error) domain.ErrorCode {
	var ce *runtimev1.ContractError
	code := classify(err)
	if errors.As(err, &ce) {
		code = ce.Code
	}
	for _, op := range runtimev3.Operations {
		if op.ID == opID {
			if slices.Contains(op.Errors, code) {
				return code
			}
			break
		}
	}
	if opID == "" {
		switch code {
		case domain.CodeNotFound, domain.CodeUnavailable:
			return code
		}
	}
	return domain.CodeUnavailable
}

// conflictDetails carries the facts of a publication conflict: why and the tip
// the control plane read. Nothing else leaves in an error body.
func conflictDetails(err error) map[string]any {
	var pc *runtimeauth.PublicationConflict
	if !errors.As(err, &pc) {
		return nil
	}
	d := map[string]any{"reason": pc.Reason}
	if pc.ObservedTip != "" {
		d["observedTip"] = pc.ObservedTip
	}
	return d
}

func (h *handler) writeError3(w http.ResponseWriter, requestID string, code domain.ErrorCode, details map[string]any) {
	status, ok := runtimev3.Status(code)
	if !ok {
		status = http.StatusServiceUnavailable
	}
	if code == domain.CodeInvalidCredential {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-runtime"`)
	}
	if code == domain.CodeUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	msg := messages[code]
	if msg == "" {
		msg = messages[domain.CodeUnavailable]
	}
	if details == nil {
		details = map[string]any{}
	}
	w.Header().Set("X-Request-ID", requestID)
	h.writeJSON(w, status, runtimev3.ErrorResponse{Error: runtimev3.ErrorBody{Code: code, Message: msg, RequestID: requestID, Details: details}})
}

func (h *handler) fail3(w http.ResponseWriter, r *http.Request, opID, requestID string, attrs []slog.Attr, err error) {
	code := v3Code(opID, err)
	status, ok := runtimev3.Status(code)
	if !ok {
		status = http.StatusServiceUnavailable
	}
	attrs = append([]slog.Attr{slog.String("op", opID), slog.String("requestId", requestID), slog.String("code", string(code)), slog.Int("status", status)}, attrs...)
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	var details map[string]any
	if code == runtimev1.CodePublicationConflict {
		details = conflictDetails(err)
	}
	h.writeError3(w, requestID, code, details)
}

func (h *v3handler) okAttrs(r *http.Request, opID, requestID string, attrs []slog.Attr) {
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "runtime request",
		append([]slog.Attr{slog.String("op", opID), slog.String("requestId", requestID), slog.Int("status", http.StatusOK)}, attrs...)...)
}

// ---- attempt routes -------------------------------------------------------------

func (h *v3handler) attemptRoute(w http.ResponseWriter, r *http.Request, op, rid string) (domain.AttemptID, bool) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail3(w, r, op, rid, nil, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return "", false
	}
	return attempt, true
}

func (h *v3handler) admit(w http.ResponseWriter, r *http.Request) {
	const op = "admit"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.AdmitRequest](body)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.AdmitV3(r.Context(), attempt, req)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) renew(w http.ResponseWriter, r *http.Request) {
	const op = "renewToken"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	if _, err := runtimev3.DecodeRequest[runtimev3.TokenRequest](body); err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.RenewV3(r.Context(), attempt, tok)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) context(w http.ResponseWriter, r *http.Request) {
	const op = "getContext"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	resp, err := h.svc.ContextV3(r.Context(), attempt, tok)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

// begin is the command-begin fence over every source. The bearer is checked
// (audience, wire, current authority) before the body is read; the attempt's
// scope comes from the verified authorization, never from the request.
func (h *v3handler) begin(w http.ResponseWriter, r *http.Request) {
	const op = "begin"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	auth, err := h.svc.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: beginAction, ContextVersion: runtimeauth.WireV3})
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.BeginRequestV3](body)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	if err := h.svc.Reauthorize(r.Context(), auth); err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	res, err := h.begins.RecordBeginV3(r.Context(), auth, req)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), runtimeauth.Classify(err))
		return
	}
	resp := runtimev3.BeginResponseV3{
		AttemptID: res.AttemptID, BeganAt: runtimev3.Timestamp(res.BeganAt.UTC().Format(time.RFC3339Nano)),
		WorkspaceState: res.WorkspaceState, IgnoredPresent: res.IgnoredPresent,
	}
	if err := resp.Validate(); err != nil || resp.AttemptID != attempt {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

// seed and integration are the two runtime Git grant routes. The bearer is the
// attempt's runtime token (the route authority); the Git credential in the answer
// is a different secret, shown once, never logged. The use case re-verifies the
// session under the attempt's lock and reads the capability from the attempt's own
// pinned context.
func (h *v3handler) seed(w http.ResponseWriter, r *http.Request) {
	const op = "seedGitGrant"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	auth, err := h.svc.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3})
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.GitSeedRequest](body)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.SeedGitGrant(r.Context(), auth, req)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) integration(w http.ResponseWriter, r *http.Request) {
	const op = "integrationGitGrant"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	auth, err := h.svc.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: runtimeauth.GitGrantAction, ContextVersion: runtimeauth.WireV3})
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.GitIntegrationRequest](body)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.IntegrationGitGrant(r.Context(), auth, req)
	if err != nil {
		h.fail3(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

// ---- resource routes ------------------------------------------------------------

func (h *v3handler) resourceRoute(w http.ResponseWriter, r *http.Request, op, rid string) (domain.RunID, string, bool) {
	run, key := domain.RunID(r.PathValue("runId")), r.PathValue("resourceKey")
	if err := run.Validate(); err != nil {
		h.fail3(w, r, op, rid, nil, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return "", "", false
	}
	if _, err := runtimeauth.ResourceKindOf(key); err != nil {
		h.fail3(w, r, op, rid, resourceAttrs(run, ""), err)
		return "", "", false
	}
	return run, key, true
}

func (h *v3handler) admitResource(w http.ResponseWriter, r *http.Request) {
	const op = "admitResource"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.ResourceAdmitRequest](body)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.AdmitResourceV3(r.Context(), runtimeauth.ResourceRef{RunID: run, Key: key}, req)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) renewResource(w http.ResponseWriter, r *http.Request) {
	const op = "renewResourceToken"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	if _, err := runtimev3.DecodeRequest[runtimev3.ResourceTokenRequest](body); err != nil {
		h.fail3(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.RenewResourceV3(r.Context(), run, key, tok)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) resourceContext(w http.ResponseWriter, r *http.Request) {
	const op = "getResourceContext"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	resp, err := h.svc.ResourceContextV3(r.Context(), run, key, tok)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) resolveResourceSecret(w http.ResponseWriter, r *http.Request) {
	const op = "resolveResourceSecret"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	auth, err := h.svc.AuthorizeResource(r.Context(), runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: key, Bearer: tok, Op: runtimeauth.OpResourceSecrets, ContextVersion: runtimeauth.WireV3})
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.SecretResolveRequest](body)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.secrets.ResolveResourceSecret(r.Context(), auth, req, rid)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	if err := resp.Validate(); err != nil {
		h.fail3(w, r, op, rid, attrs, runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v3handler) completePreparation(w http.ResponseWriter, r *http.Request) {
	const op = "completePreparation"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	auth, err := h.svc.AuthorizeResource(r.Context(), runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: key, Bearer: tok, Op: runtimeauth.OpResourceComplete, ContextVersion: runtimeauth.WireV3})
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.PreparationReceiptRequestV3](body)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	// The receipt must be about every source at its pinned commit within the try
	// bound, as the committed context says; execution rechecks under the run lock.
	if auth.ContextV3 == nil {
		h.fail3(w, r, op, rid, attrs, runtimeauth.ErrUnavailable)
		return
	}
	if err := runtimev3.CheckReceipt(*auth.ContextV3, req); err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	if err := h.svc.ReauthorizeResource(r.Context(), auth); err != nil {
		h.fail3(w, r, op, rid, attrs, err)
		return
	}
	res, err := h.receipts.RecordPreparationReceiptV3(r.Context(), auth, req)
	if err != nil {
		h.fail3(w, r, op, rid, attrs, runtimeauth.Classify(err))
		return
	}
	resp := runtimev3.PreparationReceiptResponse{ReceiptDigest: runtimev3.Digest(res.Digest), RecordedAt: runtimev3.Timestamp(res.RecordedAt.UTC().Format(time.RFC3339Nano))}
	if err := resp.Validate(); err != nil {
		h.fail3(w, r, op, rid, attrs, runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}
