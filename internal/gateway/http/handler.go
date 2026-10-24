// Package gatewayhttp is the HTTP adapter of the model gateway. It serves two
// audiences with two authentications and shares no handler between them:
//
//   - the management route `GET, PUT /tenants/{t}/projects/{p}/model-settings`,
//     registered on the shared httpapi.Router (authentication, authorization,
//     rate limiting, bounded JSON, ETag/If-Match, error envelope);
//   - the workload route `POST /gateway/v1/attempts/{attemptId}/chat/completions`,
//     a plain http.Handler authenticated by the attempt's runtime token, which
//     streams server-sent events.
//
// It holds transport concerns only: strict decoding, size bounds, headers,
// error envelopes and safe logging. Every rule comes from the gateway use
// cases. The wire contract is api/gateway.yaml; the types in gateway.gen.go are
// generated from it.
package gatewayhttp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Management authorization actions registered with the router. The use cases
// still check config:read / project:settings through the identity authority, so
// a route authorizer that is more permissive grants nothing.
const (
	ActionRead   httpapi.Action = "model_settings.read"
	ActionUpdate httpapi.Action = "model_settings.update"
)

// Paths. SettingsPath is relative to the router's prefix (/api/v1); the
// workload pattern is absolute and independent of it.
const (
	SettingsPath    = "/tenants/{tenantId}/projects/{projectId}/model-settings"
	WorkloadPattern = "POST /gateway/v1/attempts/{attemptId}/chat/completions"
)

const (
	maxBearerBytes = 1100
	// DefaultWriteTimeout bounds one write of the response stream to a slow
	// workload; a slower reader is treated as a disconnected one.
	DefaultWriteTimeout = 10 * time.Second
)

// Service is the consumer-owned port of the handlers; *gateway.Service
// implements it.
type Service interface {
	GetSettings(ctx context.Context, actor gateway.Actor, scope domain.Scope) (gateway.ModelSettings, error)
	ReplaceSettings(ctx context.Context, actor gateway.Actor, scope domain.Scope, expected int64, edit gateway.SettingsEdit, requestID string) (gateway.SettingsResult, error)
	Invoke(ctx context.Context, in gateway.InvokeRequest) (gateway.InvokeResult, error)
}

var _ Service = (*gateway.Service)(nil)

// Config wires a Handler.
type Config struct {
	Service Service
	// WriteTimeout bounds each write to the workload (default DefaultWriteTimeout).
	WriteTimeout time.Duration
	// Logger receives operation names, request identifiers and codes only: never
	// bodies, headers or provider text.
	Logger *slog.Logger
}

// Handler serves the gateway's routes.
type Handler struct {
	svc     Service
	timeout time.Duration
	log     *slog.Logger
}

// NewHandler requires a service.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("gatewayhttp: a service is required")
	}
	h := &Handler{svc: cfg.Service, timeout: cfg.WriteTimeout, log: cfg.Logger}
	if h.timeout == 0 {
		h.timeout = DefaultWriteTimeout
	}
	if h.timeout < 0 {
		return nil, errors.New("gatewayhttp: the write timeout must not be negative")
	}
	if h.log == nil {
		h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return h, nil
}

// Route is one registered management operation, exposed so tests and the
// integration owner can compare the table with api/gateway.yaml.
type Route struct{ Method, Path, Operation string }

// Routes returns the management route table.
func (h *Handler) Routes() []Route {
	return []Route{
		{http.MethodGet, SettingsPath, "getModelSettings"},
		{http.MethodPut, SettingsPath, "replaceModelSettings"},
	}
}

// RegisterRoutes registers the management routes on rt. The router's prefix
// (for example /api/v1) is applied by rt.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, r := range []httpapi.Route{
		{Method: http.MethodGet, Path: SettingsPath, Action: ActionRead, Handler: h.getSettings},
		{Method: http.MethodPut, Path: SettingsPath, Action: ActionUpdate, Handler: h.putSettings},
	} {
		if err := rt.Handle(r); err != nil {
			return fmt.Errorf("gatewayhttp: register %s %s: %w", r.Method, r.Path, err)
		}
	}
	return nil
}

// ---- management --------------------------------------------------------------

// actor bounds the router's verified principal to the audit column: a longer
// identifier is truncated and suffixed with a digest of the full value.
func actor(c *httpapi.Call) gateway.Actor {
	id := c.Principal.ID
	if len(id) > 256 {
		sum := sha256.Sum256([]byte(id))
		id = id[:200] + "#" + hex.EncodeToString(sum[:8])
	}
	return gateway.Actor{ID: id}
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapManagementError(err))
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	s, err := h.svc.GetSettings(r.Context(), actor(c), scope)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeSettings(w, r, s)
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, httpapi.BadRequestError("the scope is not valid"))
		return
	}
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		fail(w, r, err)
		return
	}
	var req ReplaceModelSettingsRequest
	if err := httpapi.DecodeJSONBytes(body, &req); err != nil {
		fail(w, r, err)
		return
	}
	edit := gateway.SettingsEdit{
		Enabled: req.Enabled, Endpoint: req.Endpoint, Credential: gateway.CredentialRef{Ref: req.Credential.Ref, Version: req.Credential.Version},
		Models: req.Models, Limits: limitsOf(req.Limits),
	}
	if req.Recording != nil {
		edit.Recording = *req.Recording
	}
	res, err := h.svc.ReplaceSettings(r.Context(), actor(c), scope, int64(expected), edit, c.RequestID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeSettings(w, r, res.Settings)
}

func limitsOf(l ModelLimits) gateway.Limits {
	return gateway.Limits{MaxRequests: l.MaxRequests, TokenAllowance: l.TokenAllowance, MaxRequestBytes: l.MaxRequestBytes,
		MaxInputTokens: l.MaxInputTokens, MaxOutputTokens: l.MaxOutputTokens}
}

func settingsDTO(s gateway.ModelSettings) ModelSettings {
	out := ModelSettings{Version: s.Revision, Stored: s.Stored(), Enabled: s.Enabled, Models: []string{}, Recording: s.Recording}
	if !s.Stored() {
		return out
	}
	ep := s.Endpoint.String()
	out.Endpoint = &ep
	out.Credential = &CredentialReference{Ref: s.Credential.Ref, Version: s.Credential.Version}
	out.Models = append([]string{}, s.Models...)
	out.Limits = &ModelLimits{MaxRequests: s.Limits.MaxRequests, TokenAllowance: s.Limits.TokenAllowance, MaxRequestBytes: s.Limits.MaxRequestBytes,
		MaxInputTokens: s.Limits.MaxInputTokens, MaxOutputTokens: s.Limits.MaxOutputTokens}
	at, by := s.UpdatedAt, s.UpdatedBy
	out.UpdatedAt, out.UpdatedBy = &at, &by
	return out
}

func writeSettings(w http.ResponseWriter, r *http.Request, s gateway.ModelSettings) {
	if err := httpapi.SetETag(w, domain.Version(s.Revision)); err != nil {
		fail(w, r, httpapi.InternalServerError(err))
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, settingsDTO(s))
}

// ---- workload ----------------------------------------------------------------

// Workload returns the handler of the workload route and a contract
// not_found envelope for anything else. Mount it at "/gateway/v1/" on a server
// separate from, or beside, the management router; authentication is the
// attempt's runtime token and nothing else.
func (h *Handler) Workload() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(WorkloadPattern, h.complete)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError(w, r, newRequestID(), "", mapWorkloadError(gateway.Failf(gateway.ErrNotFound, "not found")))
	})
	return mux
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_00000000000000000000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// bearer extracts the token of the Authorization header: exactly one header
// with the Bearer scheme and a single token.
func bearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxBearerBytes {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	rid := newRequestID()
	w.Header().Set(httpapi.RequestIDHeader, rid)
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.writeError(w, r, rid, "", mapWorkloadError(gateway.Failf(gateway.ErrInvalid, "the attempt identifier is not valid")))
		return
	}
	token, ok := bearer(r)
	if !ok {
		h.writeError(w, r, rid, attempt, mapWorkloadError(gateway.Failf(gateway.ErrUnauthenticated, "no usable bearer token")))
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		h.writeError(w, r, rid, attempt, &apiError{status: http.StatusUnsupportedMediaType, code: "invalid_request", message: "Content-Type must be application/json"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gateway.MaxBytesCeiling))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.writeError(w, r, rid, attempt, &apiError{status: http.StatusRequestEntityTooLarge, code: "invalid_request", message: "the request body is too large"})
			return
		}
		h.writeError(w, r, rid, attempt, mapWorkloadError(gateway.Failf(gateway.ErrInvalid, "the request body could not be read")))
		return
	}

	sink := newSSE(w, h.timeout)
	res, err := h.svc.Invoke(r.Context(), gateway.InvokeRequest{Attempt: attempt, Bearer: token, Body: raw, Sink: sink})
	if err == nil {
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "model request", slog.String("requestId", rid), slog.String("attemptId", string(attempt)),
			slog.String("modelRequest", string(res.RequestID)), slog.String("state", string(res.State)))
		return
	}
	if sink.Started() {
		// Bytes were already delivered. The failure event was written by the use
		// case; abort the response so the transport shows an incomplete body
		// instead of a body that ends like a successful one.
		h.log.LogAttrs(r.Context(), slog.LevelInfo, "model stream failed", slog.String("requestId", rid), slog.String("attemptId", string(attempt)),
			slog.String("modelRequest", string(res.RequestID)), slog.String("code", res.Code))
		panic(http.ErrAbortHandler)
	}
	h.writeError(w, r, rid, attempt, mapWorkloadError(err))
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, rid string, attempt domain.AttemptID, e *apiError) {
	attrs := []slog.Attr{slog.String("requestId", rid), slog.String("code", e.code), slog.Int("status", e.status)}
	if attempt != "" && attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	level := slog.LevelInfo
	if e.status >= 500 {
		level = slog.LevelWarn
	}
	h.log.LogAttrs(r.Context(), level, "model request refused", attrs...)
	writeEnvelope(w, rid, e)
}
