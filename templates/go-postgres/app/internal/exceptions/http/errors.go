package exhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/identity"
	"example.invalid/inventory/internal/tenancy"
)

var errPreconditionRequired = errors.New("exhttp: If-Match with the expected version is required")

// Retry says what a client may safely do after an error.
//
//	none      the request is wrong or forbidden; do not repeat it unchanged.
//	refresh   read the current state, then decide (version/state conflicts).
//	same      repeat the identical request; commands carry an Idempotency-Key, so
//	          a repeat observes the original outcome rather than a second effect.
const (
	retryNone    = "none"
	retryRefresh = "refresh"
	retrySame    = "same"
)

type problem struct {
	status int
	code   string
	msg    string
	retry  string
}

func classify(err error) problem {
	switch {
	case errors.Is(err, identity.ErrUnauthenticated):
		return problem{http.StatusUnauthorized, "unauthenticated", "a valid bearer token is required", retryNone}
	case errors.Is(err, identity.ErrUnavailable), errors.Is(err, tenancy.ErrUnavailable):
		return problem{http.StatusServiceUnavailable, "unavailable", "a required dependency is unavailable", retrySame}
	case errors.Is(err, tenancy.ErrNotMember):
		return problem{http.StatusNotFound, "tenant_not_found", "the tenant does not exist or you are not a member", retryNone}
	case errors.Is(err, tenancy.ErrForbidden):
		return problem{http.StatusForbidden, "forbidden", "your role does not permit this operation", retryNone}
	case errors.Is(err, errPreconditionRequired):
		return problem{http.StatusPreconditionRequired, "precondition_required", err.Error(), retryNone}
	case errors.Is(err, exceptions.ErrInvalid):
		return problem{http.StatusBadRequest, "invalid_request", err.Error(), retryNone}
	case errors.Is(err, exceptions.ErrNotFound):
		return problem{http.StatusNotFound, "not_found", "the exception does not exist", retryNone}
	case errors.Is(err, exceptions.ErrVersionConflict):
		return problem{http.StatusPreconditionFailed, "version_conflict", "the exception changed; read it again", retryRefresh}
	case errors.Is(err, exceptions.ErrInvalidState):
		return problem{http.StatusConflict, "invalid_state", err.Error(), retryRefresh}
	case errors.Is(err, exceptions.ErrIdempotencyConflict):
		return problem{http.StatusConflict, "idempotency_conflict", "the Idempotency-Key was already used for a different request", retryNone}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), errors.Is(err, exceptions.ErrUnavailable):
		return problem{http.StatusServiceUnavailable, "unavailable", "the request could not be completed; repeat it with the same Idempotency-Key", retrySame}
	}
	return problem{http.StatusInternalServerError, "internal", "an internal error occurred", retrySame}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	p := classify(err)
	if p.status >= 500 {
		h.log.Error("request failed", "requestId", requestID(r), "error", err)
	}
	writeJSON(w, p.status, map[string]any{"error": map[string]any{"code": p.code, "message": p.msg, "retry": p.retry, "requestId": requestID(r)}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
