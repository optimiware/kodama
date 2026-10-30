package runtimehttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The runtime v2 routes (T-113, seam S7):
//
//	/runtime/v2/attempts/{attemptId}/{admit,token,context,begin,secrets/resolve,
//	                                  artifacts,artifacts/{id}/complete,source,result}
//	/runtime/v2/runs/{runId}/resources/{resourceKey}/{admit,token,context,
//	                                  secrets/resolve,preparation/complete}
//
// The admission, renewal and context routes of both families and the begin and
// preparation routes are served here from the module's use cases. The five
// attempt routes whose bodies v2 shares unchanged with v1 (secrets, artifacts,
// source, result) are served by the v1 handlers of their owners: this package
// only fences them (audience, wire, current authority) and hands the request
// over under the v1 path, so one implementation serves both wires.

// BeginResult is the durable acknowledgment of a command-begin.
type BeginResult struct {
	AttemptID      domain.AttemptID
	BeganAt        time.Time
	WorkspaceState runtimev2.WorkspaceState
	IgnoredPresent bool
}

// BeginRecorder is the consumer-owned port of the command-begin fence; the
// execution use case RecordBegin is its implementation (T-116 bridges; the
// execbridge package is that bridge). It receives the verified authorization the
// handler has just RE-checked (after the body was read), so an implementation can
// carry the session into its own transaction and run runtimeauth.CheckSession
// there. Its failures are classified with the runtimeauth error classes:
// ErrAttemptRevoked (cancelled or terminal, no command may start),
// ErrIdempotencyConflict (a repeat with another body), ErrInvalidRequest,
// ErrNotFound; anything else is unavailable.
type BeginRecorder interface {
	RecordBegin(ctx context.Context, auth runtimeauth.Authorization, req runtimev2.BeginRequest) (BeginResult, error)
}

// ReceiptResult is the acknowledgment of a recorded preparation receipt.
type ReceiptResult struct {
	Digest     string
	RecordedAt time.Time
}

// ReceiptRecorder is the consumer-owned port of the trusted checkout receipt;
// execution's RecordPreparationReceipt is its implementation. Like BeginRecorder
// it receives the authorization the handler has just re-checked. Failures:
// ErrReceiptConflict, ErrResourceRevoked, ErrResourceUnbound, ErrInvalidRequest,
// ErrNotFound; anything else is unavailable.
type ReceiptRecorder interface {
	RecordPreparationReceipt(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev2.PreparationReceiptRequest) (ReceiptResult, error)
}

// ResourceSecretResolver resolves one pinned binding of a service. The
// authorization carries the committed context, so only the service's own pinned
// bindings can be resolved; nothing about the binding set comes from the request.
// An error may be a *runtimev1.ContractError (the secrets adapter's translation).
type ResourceSecretResolver interface {
	ResolveResourceSecret(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error)
}

// V2Service is the consumer-owned port of the v2 handlers; *runtimeauth.Service
// implements it.
type V2Service interface {
	AdmitV2(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (runtimev2.AdmitResponseV2, error)
	RenewV2(ctx context.Context, route domain.AttemptID, bearer string) (runtimev2.TokenResponse, error)
	ContextV2(ctx context.Context, route domain.AttemptID, bearer string) (runtimev2.ContextV2, error)
	Authorize(ctx context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error)
	Reauthorize(ctx context.Context, a runtimeauth.Authorization) error

	AdmitResource(ctx context.Context, route runtimeauth.ResourceRef, req runtimev2.ResourceAdmitRequest) (runtimev2.ResourceAdmitResponse, error)
	RenewResource(ctx context.Context, run domain.RunID, key, bearer string) (runtimev2.ResourceTokenResponse, error)
	ResourceContext(ctx context.Context, run domain.RunID, key, bearer string) (runtimev2.ResourceContext, error)
	AuthorizeResource(ctx context.Context, req runtimeauth.ResourceAuthorizeRequest) (runtimeauth.ResourceAuthorization, error)
	ReauthorizeResource(ctx context.Context, a runtimeauth.ResourceAuthorization) error
}

var _ V2Service = (*runtimeauth.Service)(nil)

// SharedOps are the operation IDs of the attempt routes whose bodies are shared
// with v1 and served by the v1 owners' handlers.
var SharedOps = []string{"resolveSecret", "reserveArtifact", "completeArtifact", "proposeSource", "submitResult"}

// V2Config configures NewV2Handler. A route whose collaborator is nil is not
// wired: it answers the contract's not_found envelope, never a silent success.
type V2Config struct {
	Service         V2Service
	Begins          BeginRecorder
	Receipts        ReceiptRecorder
	ResourceSecrets ResourceSecretResolver
	// Shared maps a shared operation ID (SharedOps) to the v1 handler that serves
	// its body. The handler is invoked with the v1 path of the same operation.
	Shared map[string]http.Handler
	Logger *slog.Logger
}

type v2handler struct {
	*handler
	svc      V2Service
	begins   BeginRecorder
	receipts ReceiptRecorder
	secrets  ResourceSecretResolver
}

const v1Prefix = "/runtime/v1/attempts/"

// beginAction is the action that authorizes the command-begin. The frozen action
// ceiling (the database's closed list) has no action of its own for it, and the
// begin must never ride the result receipt's replay exemption: it requires a live
// attempt exactly as the context read does. Execution's RecordBegin linearizes
// the command authorization against cancellation under the run lock.
const beginAction = runtimeauth.ActionContextRead

// NewV2Handler returns the handler of the /runtime/v2 routes.
func NewV2Handler(cfg V2Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("runtimehttp: a v2 service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for op := range cfg.Shared {
		if !slices.Contains(SharedOps, op) {
			return nil, errors.New("runtimehttp: " + op + " is not an operation with a body shared with v1")
		}
	}
	h := &v2handler{handler: &handler{log: logger}, svc: cfg.Service, begins: cfg.Begins, receipts: cfg.Receipts, secrets: cfg.ResourceSecrets}
	guard := &WireGuard{svc: cfg.Service, wire: runtimeauth.WireV2, h: h.handler}
	mux := http.NewServeMux()
	for _, op := range runtimev2.Operations {
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
		default:
			if next, ok := cfg.Shared[op.ID]; ok && next != nil {
				fn = guard.Wrap(op.ID, rewriteToV1(op.ID, next))
			}
		}
		if fn == nil {
			continue
		}
		mux.Handle(op.Method+" "+v2Pattern(op), fn)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError(w, "", newRequestID(), domain.CodeNotFound)
	})
	return mux, nil
}

func v2Pattern(op runtimev2.Operation) string {
	if op.Subject == runtimev2.SubjectResource {
		return runtimev2.ResourceBasePath + op.Path
	}
	return runtimev2.AttemptBasePath + op.Path
}

// rewriteToV1 hands the request to a v1 handler under the v1 path of the same
// operation (the attempt ID and any artifact ID are the same path values).
func rewriteToV1(opID string, next http.Handler) http.Handler {
	var v1 *runtimev1.Operation
	for i := range runtimev1.Operations {
		if runtimev1.Operations[i].ID == opID {
			v1 = &runtimev1.Operations[i]
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v1 == nil {
			http.NotFound(w, r)
			return
		}
		path := v1Prefix + url.PathEscape(r.PathValue("attemptId")) + v1.Path
		if id := r.PathValue("artifactId"); id != "" {
			path = strings.Replace(path, "{artifactId}", url.PathEscape(id), 1)
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path, r2.URL.RawPath = path, ""
		r2.RequestURI = ""
		next.ServeHTTP(w, r2)
	})
}

// ---- error rendering of the v2 envelope -------------------------------------

// v2Code maps an error to a code the operation's v2 inventory permits. A code the
// inventory excludes, or an unclassified failure, is unavailable: fail closed.
func v2Code(opID string, err error) domain.ErrorCode {
	var ce *runtimev2.ContractError // the v1 type, aliased by v2
	code := classify(err)
	if errors.As(err, &ce) {
		code = ce.Code
	}
	for _, op := range runtimev2.Operations {
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

func (h *handler) fail2(w http.ResponseWriter, r *http.Request, opID, requestID string, attrs []slog.Attr, err error) {
	code := v2Code(opID, err)
	attrs = append([]slog.Attr{slog.String("op", opID), slog.String("requestId", requestID), slog.String("code", string(code)), slog.Int("status", statusOf(code))}, attrs...)
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	h.writeError(w, opID, requestID, code)
}

func attemptAttrs(a domain.AttemptID) []slog.Attr {
	if a.Validate() == nil {
		return []slog.Attr{slog.String("attemptId", string(a))}
	}
	return nil
}

func resourceAttrs(run domain.RunID, key string) []slog.Attr {
	var out []slog.Attr
	if run.Validate() == nil {
		out = append(out, slog.String("runId", string(run)))
	}
	if _, err := runtimeauth.ResourceKindOf(key); err == nil {
		out = append(out, slog.String("resourceKey", key))
	}
	return out
}

func (h *v2handler) okAttrs(r *http.Request, opID, requestID string, attrs []slog.Attr) {
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "runtime request",
		append([]slog.Attr{slog.String("op", opID), slog.String("requestId", requestID), slog.Int("status", http.StatusOK)}, attrs...)...)
}

// ---- attempt routes ----------------------------------------------------------

func (h *v2handler) attemptRoute(w http.ResponseWriter, r *http.Request, op, rid string) (domain.AttemptID, bool) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail2(w, r, op, rid, nil, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return "", false
	}
	return attempt, true
}

func (h *v2handler) admit(w http.ResponseWriter, r *http.Request) {
	const op = "admit"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev2.DecodeRequest[runtimev2.AdmitRequest](body)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.AdmitV2(r.Context(), attempt, req)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) renew(w http.ResponseWriter, r *http.Request) {
	const op = "renewToken"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	if _, err := runtimev2.DecodeRequest[runtimev2.TokenRequest](body); err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.RenewV2(r.Context(), attempt, tok)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) context(w http.ResponseWriter, r *http.Request) {
	const op = "getContext"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	resp, err := h.svc.ContextV2(r.Context(), attempt, tok)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

// begin is the command-begin fence. The bearer is checked (audience, wire,
// current authority) before the body is read; the attempt's scope comes from the
// verified authorization, never from the request.
func (h *v2handler) begin(w http.ResponseWriter, r *http.Request) {
	const op = "begin"
	rid := newRequestID()
	attempt, ok := h.attemptRoute(w, r, op, rid)
	if !ok {
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	auth, err := h.svc.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: beginAction, ContextVersion: runtimeauth.WireV2})
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	req, err := runtimev2.DecodeRequest[runtimev2.BeginRequest](body)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	// The authorization above was decided before the body arrived; a revocation or
	// cancellation may have committed since. Re-verify it now, immediately before
	// the durable record, so a begin is never recorded for a session that is no
	// longer current.
	if err := h.svc.Reauthorize(r.Context(), auth); err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), err)
		return
	}
	res, err := h.begins.RecordBegin(r.Context(), auth, req)
	if err != nil {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), runtimeauth.Classify(err))
		return
	}
	resp := runtimev2.BeginResponse{
		AttemptID: res.AttemptID, BeganAt: runtimev2.Timestamp(res.BeganAt.UTC().Format(time.RFC3339Nano)),
		WorkspaceState: res.WorkspaceState, IgnoredPresent: res.IgnoredPresent,
	}
	if err := resp.Validate(); err != nil || resp.AttemptID != attempt {
		h.fail2(w, r, op, rid, attemptAttrs(attempt), runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attemptAttrs(attempt))
	h.writeJSON(w, http.StatusOK, resp)
}

// ---- the wire guard ------------------------------------------------------------

// WireAuthorizer is the part of the service the guard uses.
type WireAuthorizer interface {
	Authorize(ctx context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error)
}

// WireGuard fences an attempt route whose handler belongs to another owner
// (secrets, artifacts, source, result): before the handler runs, the bearer must
// be a genuine attempt token (a resource token is forbidden), the attempt must
// belong to the route's wire (otherwise context_version_mismatch, with nothing
// changed) and hold the operation's action with current authority. T-116 wraps
// the v1 handlers with a WireV1 guard, so a run_shared attempt cannot reach them
// on /runtime/v1, and the v2 handler wraps them with a WireV2 guard.
type WireGuard struct {
	svc  WireAuthorizer
	wire int64
	h    *handler
}

// NewWireGuard builds a guard for the wire (runtimeauth.WireV1 or WireV2).
func NewWireGuard(svc WireAuthorizer, wire int64, logger *slog.Logger) (*WireGuard, error) {
	if svc == nil || (wire != runtimeauth.WireV1 && wire != runtimeauth.WireV2) {
		return nil, errors.New("runtimehttp: a service and a known wire are required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &WireGuard{svc: svc, wire: wire, h: &handler{log: logger}}, nil
}

// Wrap guards next, which serves the operation (an ID of the v1 or v2 tables).
func (g *WireGuard) Wrap(opID string, next http.Handler) http.Handler {
	action, ok := runtimeauth.ActionForOperation(opID)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		attempt := domain.AttemptID(r.PathValue("attemptId"))
		fail := func(err error) {
			if g.wire == runtimeauth.WireV2 {
				g.h.fail2(w, r, opID, rid, attemptAttrs(attempt), err)
				return
			}
			g.h.fail(w, r, opID, rid, attempt, err)
		}
		if !ok {
			fail(runtimeauth.ErrForbidden)
			return
		}
		if err := attempt.Validate(); err != nil {
			fail(errors.Join(runtimeauth.ErrInvalidRequest, err))
			return
		}
		tok, err := bearer(r)
		if err != nil {
			fail(err)
			return
		}
		if _, err := g.svc.Authorize(r.Context(), runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: tok, Action: action, ContextVersion: g.wire}); err != nil {
			fail(err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- resource routes -----------------------------------------------------------

func (h *v2handler) resourceRoute(w http.ResponseWriter, r *http.Request, op, rid string) (domain.RunID, string, bool) {
	run, key := domain.RunID(r.PathValue("runId")), r.PathValue("resourceKey")
	if err := run.Validate(); err != nil {
		h.fail2(w, r, op, rid, nil, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return "", "", false
	}
	if _, err := runtimeauth.ResourceKindOf(key); err != nil {
		h.fail2(w, r, op, rid, resourceAttrs(run, ""), err)
		return "", "", false
	}
	return run, key, true
}

func (h *v2handler) admitResource(w http.ResponseWriter, r *http.Request) {
	const op = "admitResource"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev2.DecodeRequest[runtimev2.ResourceAdmitRequest](body)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.AdmitResource(r.Context(), runtimeauth.ResourceRef{RunID: run, Key: key}, req)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) renewResource(w http.ResponseWriter, r *http.Request) {
	const op = "renewResourceToken"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	if _, err := runtimev2.DecodeRequest[runtimev2.ResourceTokenRequest](body); err != nil {
		h.fail2(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.RenewResource(r.Context(), run, key, tok)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) resourceContext(w http.ResponseWriter, r *http.Request) {
	const op = "getResourceContext"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	resp, err := h.svc.ResourceContext(r.Context(), run, key, tok)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) resolveResourceSecret(w http.ResponseWriter, r *http.Request) {
	const op = "resolveResourceSecret"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	auth, err := h.svc.AuthorizeResource(r.Context(), runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: key, Bearer: tok, Op: runtimeauth.OpResourceSecrets})
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev2.DecodeRequest[runtimev1.SecretResolveRequest](body)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.secrets.ResolveResourceSecret(r.Context(), auth, req, rid)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	if err := resp.Validate(); err != nil {
		h.fail2(w, r, op, rid, attrs, runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *v2handler) completePreparation(w http.ResponseWriter, r *http.Request) {
	const op = "completePreparation"
	rid := newRequestID()
	run, key, ok := h.resourceRoute(w, r, op, rid)
	if !ok {
		return
	}
	attrs := resourceAttrs(run, key)
	tok, err := bearer(r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	auth, err := h.svc.AuthorizeResource(r.Context(), runtimeauth.ResourceAuthorizeRequest{RunID: run, Key: key, Bearer: tok, Op: runtimeauth.OpResourceComplete})
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	req, err := runtimev2.DecodeRequest[runtimev2.PreparationReceiptRequest](body)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	// The receipt must be about the run's pinned commit within the try bound, as
	// the committed context says; execution rechecks under the run lock.
	if err := runtimev2.CheckReceipt(auth.Context, req); err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	if err := h.svc.ReauthorizeResource(r.Context(), auth); err != nil {
		h.fail2(w, r, op, rid, attrs, err)
		return
	}
	res, err := h.receipts.RecordPreparationReceipt(r.Context(), auth, req)
	if err != nil {
		h.fail2(w, r, op, rid, attrs, runtimeauth.Classify(err))
		return
	}
	resp := runtimev2.PreparationReceiptResponse{ReceiptDigest: runtimev2.Digest(res.Digest), RecordedAt: runtimev2.Timestamp(res.RecordedAt.UTC().Format(time.RFC3339Nano))}
	if err := resp.Validate(); err != nil {
		h.fail2(w, r, op, rid, attrs, runtimeauth.ErrUnavailable)
		return
	}
	h.okAttrs(r, op, rid, attrs)
	h.writeJSON(w, http.StatusOK, resp)
}
