package runtimehttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Bounds. MaxHeaderBytes is for the composition root's http.Server (see
// ApplyServerLimits); the handler additionally rejects an oversized
// Authorization header itself.
const (
	MaxBodyBytes   = 16 << 10
	MaxHeaderBytes = 8 << 10
	maxBearerBytes = 1100
	requestIDLen   = 16
)

// Service is the consumer-owned port of the handlers.
type Service interface {
	Admit(ctx context.Context, route domain.AttemptID, req runtimev1.AdmitRequest) (runtimev1.AdmitResponse, error)
	Renew(ctx context.Context, route domain.AttemptID, bearer string) (runtimev1.TokenResponse, error)
	Context(ctx context.Context, route domain.AttemptID, bearer string) (runtimev1.Context, error)
}

var _ Service = (*runtimeauth.Service)(nil)

// Config configures NewHandler.
type Config struct {
	Service Service
	Logger  *slog.Logger
}

type handler struct {
	svc Service
	log *slog.Logger
}

// ApplyServerLimits sets the header bound and conservative timeouts on a
// server that serves the runtime interface.
func ApplyServerLimits(s *http.Server) {
	s.MaxHeaderBytes = MaxHeaderBytes
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = 10 * time.Second
	}
	if s.ReadTimeout == 0 {
		s.ReadTimeout = 30 * time.Second
	}
}

func newRequestID() string {
	b := make([]byte, requestIDLen)
	if _, err := rand.Read(b); err != nil {
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b)
}

func (h *handler) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.writeError(w, "", newRequestID(), domain.CodeUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) writeError(w http.ResponseWriter, opID, requestID string, code domain.ErrorCode) {
	status := statusOf(code)
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
	w.Header().Set("X-Request-ID", requestID)
	h.writeJSON(w, status, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{
		Code: code, Message: msg, RequestID: requestID, Details: map[string]any{},
	}})
}

// fail renders err for the operation and logs only the operation, the
// validated attempt identifier and the code.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, opID, requestID string, attempt domain.AttemptID, err error) {
	code := routeCode(opID, err)
	attrs := []slog.Attr{slog.String("op", opID), slog.String("requestId", requestID), slog.String("code", string(code)), slog.Int("status", statusOf(code))}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	h.writeError(w, opID, requestID, code)
}

func (h *handler) ok(r *http.Request, opID, requestID string, attempt domain.AttemptID) {
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "runtime request",
		slog.String("op", opID), slog.String("requestId", requestID), slog.String("attemptId", string(attempt)), slog.Int("status", http.StatusOK))
}

// readBody reads a bounded body and rejects trailing data and wrong types.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return nil, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("unsupported content type"))
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		return nil, errors.Join(runtimeauth.ErrInvalidRequest, errors.New("unreadable or oversized body"))
	}
	return data, nil
}

// bearer extracts the token of an Authorization header. A missing, malformed
// or oversized header is an invalid credential.
func bearer(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxBearerBytes {
		return "", errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no usable bearer token"))
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no usable bearer token"))
	}
	return token, nil
}

func (h *handler) admit(w http.ResponseWriter, r *http.Request) {
	const op = "admit"
	rid := newRequestID()
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, op, rid, "", errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.AdmitRequest](body)
	if err != nil {
		h.fail(w, r, op, rid, attempt, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.Admit(r.Context(), attempt, req)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *handler) renew(w http.ResponseWriter, r *http.Request) {
	const op = "renewToken"
	rid := newRequestID()
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, op, rid, "", errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	if _, err := runtimev1.DecodeRequest[runtimev1.TokenRequest](body); err != nil {
		h.fail(w, r, op, rid, attempt, errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	resp, err := h.svc.Renew(r.Context(), attempt, tok)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt)
	h.writeJSON(w, http.StatusOK, resp)
}

func (h *handler) context(w http.ResponseWriter, r *http.Request) {
	const op = "getContext"
	rid := newRequestID()
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, op, rid, "", errors.Join(runtimeauth.ErrInvalidRequest, err))
		return
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	resp, err := h.svc.Context(r.Context(), attempt, tok)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt)
	h.writeJSON(w, http.StatusOK, resp)
}
