package resultshttp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
)

// ServiceV3 is the consumer-owned port of the source-set result handler;
// *results.Service implements it.
type ServiceV3 interface {
	SubmitV3(ctx context.Context, cmd results.SubmitV3Command) (runtimev1.ResultResponse, error)
}

var _ ServiceV3 = (*results.Service)(nil)

// ConfigV3 configures NewV3Handler.
type ConfigV3 struct {
	Service ServiceV3
	Logger  *slog.Logger
}

// messageContextVersion is the one text this wire adds to the v1 messages.
const messageContextVersion = "The attempt belongs to another runtime version."

// NewV3Handler returns the handler of
//
//	POST /runtime/v3/attempts/{attemptId}/result
//
// with the v3 error envelope (the v1 codes plus context_version_mismatch). The
// route is mounted by the runtime v3 handler behind its WireV3 guard
// (runtimehttp.V3Config.Direct); the service authenticates again and asserts the
// wire itself, so it is never the only check.
func NewV3Handler(cfg ConfigV3) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("resultshttp: a service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &handler{log: logger}
	mux := http.NewServeMux()
	for _, op := range runtimev3.Operations {
		if op.ID != "submitResult" {
			continue
		}
		mux.HandleFunc(op.Method+" "+runtimev3.AttemptBasePath+op.Path, func(w http.ResponseWriter, r *http.Request) { h.resultV3(w, r, cfg.Service) })
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError3(w, newRequestID(), domain.CodeNotFound)
	})
	return mux, nil
}

func (h *handler) resultV3(w http.ResponseWriter, r *http.Request, svc ServiceV3) {
	const op = "submitResult"
	rid := newRequestID()
	attempt, bearer, body, ok := h.prepare3(w, r, op, rid, MaxResultBodyBytes)
	if !ok {
		return
	}
	req, err := runtimev3.DecodeRequest[runtimev3.ResultRequestV3](body)
	if err != nil {
		h.fail3(w, r, op, rid, attempt, results.FromContract(err))
		return
	}
	resp, err := svc.SubmitV3(r.Context(), results.SubmitV3Command{Attempt: attempt, Bearer: bearer, Request: req})
	if err != nil {
		h.fail3(w, r, op, rid, attempt, err)
		return
	}
	h.ok(r, op, rid, attempt, http.StatusOK)
	h.writeJSON(w, rid, http.StatusOK, resp)
}

// prepare3 is prepare with the v3 envelope for its refusals.
func (h *handler) prepare3(w http.ResponseWriter, r *http.Request, op, rid string, max int64) (domain.AttemptID, string, []byte, bool) {
	attempt := domain.AttemptID(r.PathValue("attemptId"))
	if err := attempt.Validate(); err != nil {
		h.fail3(w, r, op, rid, "", results.Fail(results.ErrInvalidRequest, err, "invalid attempt"))
		return "", "", nil, false
	}
	tok, err := bearer(r)
	if err != nil {
		h.fail3(w, r, op, rid, attempt, err)
		return "", "", nil, false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !hasJSON(ct) {
		h.fail3(w, r, op, rid, attempt, results.Fail(results.ErrInvalidRequest, nil, "unsupported content type"))
		return "", "", nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		h.fail3(w, r, op, rid, attempt, results.Fail(results.ErrInvalidRequest, err, "unreadable or oversized body"))
		return "", "", nil, false
	}
	return attempt, tok, body, true
}

func hasJSON(ct string) bool { return strings.HasPrefix(strings.ToLower(ct), "application/json") }

// classify3 is classify with the v3 wire's one additional class.
func classify3(err error) (domain.ErrorCode, map[string]any) {
	if errors.Is(err, results.ErrContextVersionMismatch) {
		return runtimev3.CodeContextVersionMismatch, nil
	}
	return classify(err)
}

func allowed3(opID string, code domain.ErrorCode) bool {
	for _, op := range runtimev3.Operations {
		if op.ID == opID {
			return slices.Contains(op.Errors, code)
		}
	}
	return false
}

// routeCode3 maps err to a code the v3 operation's inventory permits, degrading
// exactly as routeCode does.
func routeCode3(opID string, err error) (domain.ErrorCode, map[string]any) {
	code, details := classify3(err)
	if allowed3(opID, code) {
		return code, details
	}
	switch code {
	case domain.CodeForbidden, domain.CodeInvalidRequest:
		return domain.CodeInvalidCredential, nil
	}
	return domain.CodeUnavailable, nil
}

func (h *handler) fail3(w http.ResponseWriter, r *http.Request, op, rid string, attempt domain.AttemptID, err error) {
	code, _ := routeCode3(op, err)
	status, _ := runtimev3.Status(code)
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	level := slog.LevelInfo
	if code == domain.CodeUnavailable {
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{slog.String("op", op), slog.String("requestId", rid), slog.String("code", string(code)), slog.Int("status", status)}
	if attempt.Validate() == nil {
		attrs = append(attrs, slog.String("attemptId", string(attempt)))
	}
	h.log.LogAttrs(r.Context(), level, "runtime request denied", attrs...)
	h.writeError3(w, rid, code)
}

func (h *handler) writeError3(w http.ResponseWriter, requestID string, code domain.ErrorCode) {
	if code == domain.CodeInvalidCredential {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-runtime"`)
	}
	if code == domain.CodeUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	msg := messages[code]
	if code == runtimev3.CodeContextVersionMismatch {
		msg = messageContextVersion
	}
	if msg == "" {
		msg = messages[domain.CodeUnavailable]
	}
	status, ok := runtimev3.Status(code)
	if !ok {
		status = http.StatusServiceUnavailable
	}
	h.writeJSON(w, requestID, status, runtimev3.ErrorResponse{Error: runtimev3.ErrorBody{Code: code, Message: msg, RequestID: requestID, Details: map[string]any{}}})
}
