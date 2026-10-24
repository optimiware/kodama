// Package gitwebhookhttp serves the native receive-hook endpoint described in
// api/git-webhooks.yaml. It is a thin adapter: it bounds the body, extracts the
// credential headers and maps the service's failure classes to the common error
// envelope. It performs no authentication of its own and registers no bearer
// route; the signature is the only credential.
package gitwebhookhttp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

// Pattern is the route of the hook endpoint (Go 1.22 ServeMux syntax).
const Pattern = "POST /api/v1/git-hooks/{triggerId}"

// Handler adapts a gitwebhook.Service to HTTP.
type Handler struct{ svc *gw.Service }

// New returns the handler.
func New(svc *gw.Service) *Handler { return &Handler{svc: svc} }

// Register mounts the hook endpoint on mux.
func (h *Handler) Register(mux *http.ServeMux) { mux.Handle(Pattern, h) }

type response struct {
	Outcome   gw.Outcome `json:"outcome"`
	Reason    string     `json:"reason,omitempty"`
	RunID     string     `json:"runId,omitempty"`
	Ref       string     `json:"ref,omitempty"`
	Commit    string     `json:"commit,omitempty"`
	Duplicate bool       `json:"duplicate"`
}

type errorBody struct {
	Error struct {
		Code      string         `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"requestId"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := requestID()
	w.Header().Set("X-Request-ID", rid)
	id := gw.TriggerID(r.PathValue("triggerId"))
	if id == "" {
		writeError(w, rid, http.StatusUnauthorized, "invalid_credential", "the notification is not authenticated", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gw.MaxBodyBytes))
	if err != nil {
		writeError(w, rid, http.StatusRequestEntityTooLarge, "payload_too_large", "the notification body is too large", nil)
		return
	}
	receipt, err := h.svc.Receive(r.Context(), gw.Notification{
		TriggerID:  id,
		DeliveryID: r.Header.Get(gw.HeaderDeliveryID),
		Timestamp:  r.Header.Get(gw.HeaderTimestamp),
		Signature:  r.Header.Get(gw.HeaderSignature),
		Body:       body,
	})
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusAccepted
		if receipt.Outcome == gw.OutcomeIgnored {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response{
			Outcome: receipt.Outcome, Reason: receipt.Reason, RunID: string(receipt.RunID),
			Ref: receipt.Ref, Commit: string(receipt.Commit), Duplicate: receipt.Duplicate,
		})
	case errors.Is(err, gw.ErrUnauthenticated):
		writeError(w, rid, http.StatusUnauthorized, "invalid_credential", "the notification is not authenticated", nil)
	case errors.Is(err, gw.ErrInvalid):
		writeError(w, rid, http.StatusBadRequest, "invalid_request", "the notification payload is invalid", nil)
	case errors.Is(err, gw.ErrDeliveryConflict):
		writeError(w, rid, http.StatusConflict, "delivery_conflict", "the delivery identifier was used with a different payload", nil)
	case errors.Is(err, gw.ErrRejected):
		code := "delivery_rejected"
		if errors.Is(err, gw.ErrForeignRepository) {
			code = "foreign_repository"
		}
		writeError(w, rid, http.StatusUnprocessableEntity, code, "the delivery was recorded and refused", map[string]any{"reason": receipt.Reason})
	case errors.Is(err, gw.ErrUnavailable):
		w.Header().Set("Retry-After", "5")
		writeError(w, rid, http.StatusServiceUnavailable, "unavailable", "a dependency is unavailable; retry the same delivery", nil)
	default:
		writeError(w, rid, http.StatusInternalServerError, "internal_error", "internal error", nil)
	}
}

func writeError(w http.ResponseWriter, rid string, status int, code, msg string, details map[string]any) {
	if details == nil {
		details = map[string]any{}
	}
	var b errorBody
	b.Error.Code, b.Error.Message, b.Error.RequestID, b.Error.Details = code, msg, rid, details
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}
