package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
)

// SecretResolver is the runtime secret route: it authenticates the attempt's
// bearer, restricts resolution to what the attempt's pinned environment names
// and returns the pinned secret version. *execution/adapters.SecretRoute
// implements it. The error is always a *runtimev1.ContractError.
type SecretResolver interface {
	Resolve(ctx context.Context, attempt domain.AttemptID, bearer string, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error)
}

var _ SecretResolver = (*execadapters.SecretRoute)(nil)

const (
	resolveSecretOp       = "resolveSecret"
	maxSecretRequestBytes = 16 << 10
	maxSecretBearerBytes  = 1100
	secretRequestIDBytes  = 16
	secretUnavailableText = "The service cannot decide right now; retry."
)

// secretHandler serves resolveSecret of the frozen runtime contract
// (POST /runtime/v1/attempts/{attemptId}/secrets/resolve). It owns transport
// only: strict bounded decoding, the contract's error envelope restricted to
// the operation's frozen error inventory, and no-store headers. Authority and
// the audit belong to the route behind it. Nothing it logs carries the
// reference, the value or the cause.
type secretHandler struct {
	route SecretResolver
	log   *slog.Logger
}

func newSecretHandler(route SecretResolver, log *slog.Logger) (http.Handler, error) {
	if route == nil {
		return nil, errors.New("app: a secret resolver is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &secretHandler{route: route, log: log}, nil
}

func (h *secretHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := newSecretRequestID()
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail(w, r, rid, "", domain.CodeInvalidRequest)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		h.fail(w, r, rid, attempt, domain.CodeInvalidCredential)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		h.fail(w, r, rid, attempt, domain.CodeInvalidRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSecretRequestBytes))
	if err != nil {
		h.fail(w, r, rid, attempt, domain.CodeInvalidRequest)
		return
	}
	req, err := runtimev1.DecodeRequest[runtimev1.SecretResolveRequest](body)
	if err != nil {
		h.fail(w, r, rid, attempt, domain.CodeInvalidRequest)
		return
	}
	resp, err := h.route.Resolve(r.Context(), attempt, token, req, rid)
	if err != nil {
		code := domain.CodeUnavailable
		var ce *runtimev1.ContractError
		if errors.As(err, &ce) {
			code = ce.Code
		}
		h.fail(w, r, rid, attempt, code)
		return
	}
	h.log.LogAttrs(r.Context(), slog.LevelInfo, "runtime request",
		slog.String("op", resolveSecretOp), slog.String("requestId", rid), slog.String("attemptId", string(attempt)), slog.Int("status", http.StatusOK))
	h.write(w, http.StatusOK, rid, resp)
}

// fail writes the contract envelope for code, degraded to the nearest code the
// operation's frozen inventory permits.
func (h *secretHandler) fail(w http.ResponseWriter, r *http.Request, rid string, attempt domain.AttemptID, code domain.ErrorCode) {
	if !slices.Contains(secretErrorCodes(), code) {
		code = domain.CodeUnavailable
	}
	status, ok := runtimev1.Status(code)
	if !ok {
		status = http.StatusServiceUnavailable
	}
	switch code {
	case domain.CodeInvalidCredential:
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-runtime"`)
	case domain.CodeUnavailable:
		w.Header().Set("Retry-After", "1")
	}
	attrs := []slog.Attr{slog.String("op", resolveSecretOp), slog.String("requestId", rid), slog.String("code", string(code)), slog.Int("status", status)}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	h.write(w, status, rid, runtimev1.ErrorResponse{Error: runtimev1.ErrorBody{Code: code, Message: secretMessage(code), RequestID: rid, Details: map[string]any{}}})
}

func (h *secretHandler) write(w http.ResponseWriter, status int, rid string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusServiceUnavailable, []byte(`{"error":{"code":"unavailable","message":"`+secretUnavailableText+`","requestId":"`+rid+`","details":{}}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Request-ID", rid)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// secretErrorCodes is the frozen error inventory of resolveSecret.
func secretErrorCodes() []domain.ErrorCode {
	for _, op := range runtimev1.Operations {
		if op.ID == resolveSecretOp {
			return op.Errors
		}
	}
	return nil
}

func secretMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeInvalidRequest:
		return "The request is not valid."
	case domain.CodeInvalidCredential:
		return "The credential is not valid."
	case domain.CodeForbidden:
		return "The action is not granted."
	case domain.CodeNotFound:
		return "Not found."
	case runtimev1.CodeAttemptRevoked:
		return "The attempt is no longer authorized."
	}
	return secretUnavailableText
}

func newSecretRequestID() string {
	b := make([]byte, secretRequestIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(b)
}

// bearerToken extracts the single bearer token of the Authorization header.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > maxSecretBearerBytes {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
