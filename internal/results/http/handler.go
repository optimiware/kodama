// Package resultshttp exposes the two runtime routes this module owns,
//
//	POST /runtime/v1/attempts/{attemptId}/source
//	POST /runtime/v1/attempts/{attemptId}/result
//
// over net/http. It owns transport concerns only: bearer extraction, bounded
// strict decoding with the frozen runtime DTOs, response headers, request IDs,
// contract error envelopes and safe logging. Authority, validation and state
// belong to results.Service. The artifact reserve and complete routes are
// registered by artifactshttp, not here (see README.md for the wiring order).
package resultshttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
)

// Bounds. The result envelope carries bounded outputs and diagnostic IDs; the
// source proposal is three identifiers.
const (
	MaxResultBodyBytes = 256 << 10
	MaxSourceBodyBytes = 16 << 10
	maxBearerBytes     = 1100
)

// Service is the consumer-owned port of the handlers; *results.Service
// implements it.
type Service interface {
	Submit(ctx context.Context, cmd results.SubmitCommand) (runtimev1.ResultResponse, error)
	ProposeSource(ctx context.Context, cmd results.ProposeCommand) (results.SourceOutcome, error)
}

var _ Service = (*results.Service)(nil)

// Config configures NewHandler.
type Config struct {
	Service Service
	Logger  *slog.Logger
}

type handler struct {
	svc Service
	log *slog.Logger
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b)
}

func (h *handler) source(w http.ResponseWriter, r *http.Request) {
	const op = "proposeSource"
	rid := newRequestID()
	attempt, bearer, body, ok := h.prepare(w, r, op, rid, MaxSourceBodyBytes)
	if !ok {
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.SourceProposalRequest](body)
	if err != nil {
		h.fail(w, r, op, rid, attempt, results.FromContract(err))
		return
	}
	out, err := h.svc.ProposeSource(r.Context(), results.ProposeCommand{Attempt: attempt, Bearer: bearer, Request: req})
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt, out.HTTPStatus)
	h.writeJSON(w, rid, out.HTTPStatus, out.Response)
}

func (h *handler) result(w http.ResponseWriter, r *http.Request) {
	const op = "submitResult"
	rid := newRequestID()
	attempt, bearer, body, ok := h.prepare(w, r, op, rid, MaxResultBodyBytes)
	if !ok {
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.ResultRequest](body)
	if err != nil {
		h.fail(w, r, op, rid, attempt, results.FromContract(err))
		return
	}
	resp, err := h.svc.Submit(r.Context(), results.SubmitCommand{Attempt: attempt, Bearer: bearer, Request: req})
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt, http.StatusOK)
	h.writeJSON(w, rid, http.StatusOK, resp)
}

// prepare validates the route attempt and reads the bearer and the bounded body.
// The bearer is checked before the body is read, so an unauthenticated caller
// cannot make the server parse a large document.
func (h *handler) prepare(w http.ResponseWriter, r *http.Request, op, rid string, max int64) (domain.AttemptID, string, []byte, bool) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, op, rid, "", results.Fail(results.ErrInvalidRequest, err, "invalid attempt"))
		return "", "", nil, false
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail(w, r, op, rid, attempt, err)
		return "", "", nil, false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		h.fail(w, r, op, rid, attempt, results.Fail(results.ErrInvalidRequest, nil, "unsupported content type"))
		return "", "", nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		h.fail(w, r, op, rid, attempt, results.Fail(results.ErrInvalidRequest, err, "unreadable or oversized body"))
		return "", "", nil, false
	}
	return attempt, tok, body, true
}

// bearer extracts the token of the Authorization header. A missing, repeated,
// malformed or oversized header is an invalid credential.
func bearer(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxBearerBytes {
		return "", results.Fail(results.ErrUnauthenticated, nil, "no usable bearer token")
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", results.Fail(results.ErrUnauthenticated, nil, "no usable bearer token")
	}
	return token, nil
}

func (h *handler) writeJSON(w http.ResponseWriter, requestID string, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		h.writeError(w, requestID, domain.CodeUnavailable, nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Request-ID", requestID)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) writeError(w http.ResponseWriter, requestID string, code domain.ErrorCode, details map[string]any) {
	if code == domain.CodeInvalidCredential {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-runtime"`)
	}
	if code == domain.CodeUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	if details == nil {
		details = map[string]any{}
	}
	msg := messages[code]
	if msg == "" {
		msg = messages[domain.CodeUnavailable]
	}
	h.writeJSON(w, requestID, statusOf(code), runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: msg, RequestID: requestID, Details: details}})
}

// fail renders err for the operation and logs only the operation, the validated
// attempt identifier and the code.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, op, rid string, attempt domain.AttemptID, err error) {
	code, details := routeCode(op, err)
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("op", op), slog.String("requestId", rid), slog.String("code", string(code)), slog.Int("status", statusOf(code))}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	h.writeError(w, rid, code, details)
}

func (h *handler) ok(r *http.Request, op, rid string, attempt domain.AttemptID, status int) {
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "runtime request",
		slog.String("op", op), slog.String("requestId", rid), slog.String("attemptId", string(attempt)), slog.Int("status", status))
}
