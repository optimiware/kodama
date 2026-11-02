// Package gitpushhttp is the HTTP adapter of the push-record module: the
// public read routes of the push records (GET P/repositories/{id}/pushes and
// .../pushes/{pushId}) registered through httpapi.Router, and the internal
// worker-facing routes of the control plane (POST .../pushes, .../commit-intent,
// .../outcome under gitv1.ControlBasePath) served under a tenant-bound worker
// identity. Both reuse the wire shapes of contracts/git/v1, decoded and
// validated by that contract and never redefined here. It contains no
// persistence code and registers nothing for the Git endpoint, credentials or
// repositories.
package gitpushhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/httpapi"
)

// ActionRead is the authorization action of the public push-record reads. The
// composition maps it to the project's read roles; the use case asks its
// ReadAuthorizer again with current state.
const ActionRead httpapi.Action = "git_push.read"

const (
	resourceKind = "repository"
	projectBase  = "/tenants/{tenantId}/projects/{projectId}/repositories"
	listSort     = "created_at_desc,id_desc"
)

// Reader is the consumer-owned port of the public routes; *gitpush.Service
// implements it.
type Reader interface {
	Get(ctx context.Context, principal string, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error)
	List(ctx context.Context, principal string, scope domain.Scope, repositoryID string, q gitpush.ListQuery) ([]gitpush.Page, error)
}

// Handler serves the public push-record routes.
type Handler struct {
	svc       Reader
	versioned VersionedReader
	resolver  Resolver
	codec     *httpapi.CursorCodec
}

// VersionedReader is the optional read that also returns the record's version,
// which the get route sends as the ETag the resolution's If-Match names;
// *gitpush.Service implements it.
type VersionedReader interface {
	GetVersioned(ctx context.Context, principal string, scope domain.Scope, repositoryID, pushID string) (gitpush.Resolved, error)
}

// NewHandler requires every collaborator.
func NewHandler(svc Reader, cursors *httpapi.CursorCodec) (*Handler, error) {
	if svc == nil || cursors == nil {
		return nil, errors.New("gitpushhttp: a reader and a cursor codec are required")
	}
	h := &Handler{svc: svc, codec: cursors}
	h.versioned, _ = svc.(VersionedReader)
	return h, nil
}

// RegisterRoutes contributes the two read routes and, only after WithResolver,
// the operator resolution route. The router authenticates, throttles and
// authorizes before a handler runs. No other route writes a push record.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: projectBase + "/{id}/pushes", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.list},
		{Method: http.MethodGet, Path: projectBase + "/{id}/pushes/{pushId}", Action: ActionRead, ResourceKind: resourceKind, ResourceParam: "id", Handler: h.get},
	} {
		if err := rt.Handle(r); err != nil {
			return err
		}
	}
	return h.registerResolve(rt)
}

func (h *Handler) ctx(r *http.Request, c *httpapi.Call) context.Context {
	return gitpush.WithRequestID(r.Context(), c.RequestID)
}

func fail(w http.ResponseWriter, r *http.Request, err error) { httpapi.WriteError(w, r, mapError(err)) }

func (h *Handler) scope(w http.ResponseWriter, r *http.Request, c *httpapi.Call) (domain.Scope, bool) {
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, httpapi.BadRequestError("the tenant or project identifier is not valid"))
		return domain.Scope{}, false
	}
	return scope, true
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	var rec gitv1.PushRecord
	var err error
	if h.versioned != nil {
		var v gitpush.Resolved
		if v, err = h.versioned.GetVersioned(h.ctx(r, c), c.Principal.ID, scope, r.PathValue("id"), r.PathValue("pushId")); err == nil {
			rec = v.Record
			_ = httpapi.SetETag(w, v.Version)
		}
	} else {
		rec, err = h.svc.Get(h.ctx(r, c), c.Principal.ID, scope, r.PathValue("id"), r.PathValue("pushId"))
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, rec)
}

type listPosition struct {
	T  int64  `json:"t"` // creation time in microseconds
	ID string `json:"id"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, ok := h.scope(w, r, c)
	if !ok {
		return
	}
	if err := httpapi.CheckQuery(r); err != nil {
		fail(w, r, err)
		return
	}
	repositoryID := r.PathValue("id")
	cs := c.CursorScope(map[string][]string{}, listSort)
	page, err := h.codec.ParsePage(r, cs)
	if err != nil {
		fail(w, r, err)
		return
	}
	q := gitpush.ListQuery{Limit: page.FetchLimit()}
	if page.Position != nil {
		var pos listPosition
		if err := json.Unmarshal(page.Position, &pos); err != nil || !validID(pos.ID) {
			fail(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid for this request"))
			return
		}
		q.After = &gitpush.Position{CreatedAt: time.UnixMicro(pos.T).UTC(), ID: pos.ID}
	}
	rows, err := h.svc.List(h.ctx(r, c), c.Principal.ID, scope, repositoryID, q)
	if err != nil {
		fail(w, r, err)
		return
	}
	out, err := httpapi.BuildPage(h.codec, cs, page, rows, func(d gitpush.Page) ([]byte, error) {
		return json.Marshal(listPosition{T: d.CreatedAt.UnixMicro(), ID: d.Record.PushID})
	})
	if err != nil {
		fail(w, r, httpapi.InternalServerError(err))
		return
	}
	items := make([]gitv1.PushRecord, 0, len(out.Items))
	for _, it := range out.Items {
		items = append(items, it.Record)
	}
	httpapi.WriteJSON(w, r, http.StatusOK, httpapi.ListPage[gitv1.PushRecord]{Items: items, NextCursor: out.NextCursor})
}

var idRegexp = regexp.MustCompile(domain.IDPattern)

func validID(id string) bool { return idRegexp.MatchString(id) }

// messages are the client-safe texts of the module's failures.
var messages = map[domain.ErrorCode]string{
	domain.CodeNotFound:  "the resource was not found",
	domain.CodeForbidden: "the caller may not perform this operation",
}

// mapError renders a use-case error as an API error: contract errors keep their
// stable code and status, a missing or foreign object is the one not_found, an
// outage is a retryable 503 and anything else is the router's fixed 500.
func mapError(err error) error {
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		status, ok := gitv1.Status(ce.Code)
		if !ok {
			return httpapi.InternalServerError(err)
		}
		if msg, fixed := messages[ce.Code]; fixed {
			return httpapi.NewError(status, ce.Code, msg)
		}
		return httpapi.NewError(status, ce.Code, ce.Message)
	}
	switch {
	case errors.Is(err, gitpush.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, gitpush.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	}
	return err
}

// ---- internal worker routes ----

// WorkerService is the consumer-owned port of the worker routes;
// *gitpush.Service implements it.
type WorkerService interface {
	Authorize(ctx context.Context, tenant domain.TenantID, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error)
	CommitIntent(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error)
	Outcome(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error)
}

// WorkerIdentityVerifier verifies a worker identity token for the tenant of the
// route: signature, key, audience, validity window and gitv1.CheckWorkerCall
// (gitauth.WorkerIdentityVerifier, provided by T-136).
type WorkerIdentityVerifier interface {
	VerifyWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error)
}

// MaxBodyBytes bounds a worker request: 1024 updates with their facts fit with
// a wide margin.
const MaxBodyBytes = 4 << 20

// WorkerHandler serves the internal worker routes of the push journal under
// gitv1.ControlBasePath. Every request must carry a worker identity bound to the
// tenant of the path; the tenant of every record, grant and repository is that
// identity's, never anything in a body.
type WorkerHandler struct {
	svc      WorkerService
	verifier WorkerIdentityVerifier
	now      func() time.Time
}

// NewWorkerHandler requires the use cases and the identity verifier; a nil
// clock is time.Now.
func NewWorkerHandler(svc WorkerService, verifier WorkerIdentityVerifier, now func() time.Time) (*WorkerHandler, error) {
	if svc == nil || verifier == nil {
		return nil, errors.New("gitpushhttp: a service and a worker identity verifier are required")
	}
	if now == nil {
		now = time.Now
	}
	return &WorkerHandler{svc: svc, verifier: verifier, now: now}, nil
}

// base is the worker route family; {tenantId} is the tenant of the identity.
const base = gitv1.ControlBasePath

// Register mounts exactly the three routes of the push journal; any other
// method or path is the mux's 404 or 405.
func (h *WorkerHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+base+"/pushes", h.serve(h.authorize))
	mux.HandleFunc("POST "+base+"/pushes/{pushId}/commit-intent", h.serve(h.commitIntent))
	mux.HandleFunc("POST "+base+"/pushes/{pushId}/outcome", h.serve(h.outcome))
}

type workerCall func(ctx context.Context, tenant domain.TenantID, body []byte, r *http.Request) (any, error)

// serve authenticates the worker identity for the path's tenant, reads the
// bounded body, runs the use case and renders the answer or the error.
func (h *WorkerHandler) serve(call workerCall) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		w.Header().Set("X-Request-ID", rid)
		tenant := domain.TenantID(r.PathValue("tenantId"))
		if tenant.Validate() != nil {
			writeError(w, rid, http.StatusNotFound, domain.CodeNotFound, "the resource was not found")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeUnauthorized(w, rid)
			return
		}
		ctx := gitpush.WithRequestID(r.Context(), rid)
		identity, err := h.verifier.VerifyWorkerIdentity(ctx, token, tenant, h.now())
		if err != nil || identity.TenantID != string(tenant) {
			// Every refusal is the same answer: a foreign tenant's identity, an
			// expired or malformed token and a wrong audience are not told apart.
			writeUnauthorized(w, rid)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			writeError(w, rid, http.StatusRequestEntityTooLarge, domain.CodeInvalidRequest, "the request body is too large or unreadable")
			return
		}
		out, err := call(ctx, tenant, body, r)
		if err != nil {
			h.writeFailure(w, rid, err)
			return
		}
		data, err := json.Marshal(out)
		if err != nil {
			writeError(w, rid, http.StatusInternalServerError, "internal_error", "an internal error occurred")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func invalidBody() error {
	return &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: "the request body is not valid for this operation"}
}

func (h *WorkerHandler) authorize(ctx context.Context, tenant domain.TenantID, body []byte, _ *http.Request) (any, error) {
	req, err := gitv1.DecodeRequest[gitv1.PushAuthorizeRequest](body)
	if err != nil {
		return nil, invalidBody()
	}
	return h.svc.Authorize(ctx, tenant, req)
}

func (h *WorkerHandler) pushID(r *http.Request) (string, error) {
	id := r.PathValue("pushId")
	if !validID(id) {
		return "", &gitv1.ContractError{Code: domain.CodeNotFound, Message: "the resource was not found"}
	}
	return id, nil
}

func (h *WorkerHandler) commitIntent(ctx context.Context, tenant domain.TenantID, body []byte, r *http.Request) (any, error) {
	id, err := h.pushID(r)
	if err != nil {
		return nil, err
	}
	req, err := gitv1.DecodeRequest[gitv1.CommitIntentRequest](body)
	if err != nil {
		return nil, invalidBody()
	}
	return h.svc.CommitIntent(ctx, tenant, id, req)
}

func (h *WorkerHandler) outcome(ctx context.Context, tenant domain.TenantID, body []byte, r *http.Request) (any, error) {
	id, err := h.pushID(r)
	if err != nil {
		return nil, err
	}
	req, err := gitv1.DecodeRequest[gitv1.OutcomeRequest](body)
	if err != nil {
		return nil, invalidBody()
	}
	return h.svc.Outcome(ctx, tenant, id, req)
}

func (h *WorkerHandler) writeFailure(w http.ResponseWriter, rid string, err error) {
	var ce *gitv1.ContractError
	switch {
	case errors.As(err, &ce):
		if status, ok := gitv1.Status(ce.Code); ok {
			writeError(w, rid, status, ce.Code, ce.Message)
			return
		}
	case errors.Is(err, gitpush.ErrNotFound):
		writeError(w, rid, http.StatusNotFound, domain.CodeNotFound, "the resource was not found")
		return
	case errors.Is(err, gitpush.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("Retry-After", "1")
		writeError(w, rid, http.StatusServiceUnavailable, domain.CodeUnavailable, "a required service is unavailable; retry later")
		return
	}
	writeError(w, rid, http.StatusInternalServerError, "internal_error", "an internal error occurred")
}

func writeUnauthorized(w http.ResponseWriter, rid string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, rid, http.StatusUnauthorized, domain.CodeInvalidCredential, "the credential is not valid")
}

func writeError(w http.ResponseWriter, rid string, status int, code domain.ErrorCode, message string) {
	body, _ := json.Marshal(gitv1.ErrorResponse{Error: gitv1.ErrorBody{Code: code, Message: message, RequestID: rid, Details: map[string]any{}}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("gitpushhttp: random source unavailable: %v", err))
	}
	return "req_" + hex.EncodeToString(b[:])
}
