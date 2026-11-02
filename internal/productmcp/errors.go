package productmcp

import (
	"errors"
	"fmt"
)

// Failure classes. Ports return them (wrapped, or inside an *Error that carries
// a stable contract code and client-safe text); the tool layer renders only a
// stable code and constant text, never the wrapped detail of a dependency.
var (
	// ErrInvalid: the request is malformed or violates a field rule.
	ErrInvalid = errors.New("productmcp: invalid request")
	// ErrDenied: the caller is authenticated but lacks the authority.
	ErrDenied = errors.New("productmcp: denied")
	// ErrNotFound: the resource is absent, foreign to the caller's tenant, or
	// outside the project a workload is bound to; callers cannot tell which.
	ErrNotFound = errors.New("productmcp: not found")
	// ErrConflict: the request conflicts with current state (a name in use, a
	// repository that is not empty or not ready).
	ErrConflict = errors.New("productmcp: conflict")
	// ErrIdempotencyConflict: the idempotency key was used for another request.
	ErrIdempotencyConflict = errors.New("productmcp: idempotency key reused for a different request")
	// ErrBudget: a bounded delegation budget (active provisioning runs) is spent.
	ErrBudget = errors.New("productmcp: budget exhausted")
	// ErrUnavailable: a dependency failed or an outcome is unknown. Nothing is
	// retried automatically; the caller retries with the same idempotency key.
	ErrUnavailable = errors.New("productmcp: unavailable")
	// ErrUnauthenticated: no valid MCP principal (handler level).
	ErrUnauthenticated = errors.New("productmcp: unauthenticated")
	// ErrNotConfigured: a required port was not injected (fail closed).
	ErrNotConfigured = errors.New("productmcp: required capability is not configured")
)

// Error is a classified failure with a stable code and client-safe text. An
// adapter uses it to surface a contract code (repository_name_taken,
// repository_not_empty, seed_pending, ...) through a tool without leaking
// anything else. Message must hold no credential, foreign identifier or
// dependency detail.
type Error struct {
	Kind    error // one of the sentinels above
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Kind }

// Refuse builds an *Error of the class kind.
func Refuse(kind error, code, format string, args ...any) error {
	return &Error{Kind: kind, Code: code, Message: fmt.Sprintf(format, args...)}
}

// invalidArgument is a field bound violated (length, size, count).
func invalidArgument(format string, args ...any) error {
	return Refuse(ErrInvalid, "invalid_argument", format, args...)
}

func invalid(format string, args ...any) error {
	return Refuse(ErrInvalid, "invalid_request", format, args...)
}

// classify returns the stable code and safe text of err.
func classify(err error) (code, message string) {
	var e *Error
	if errors.As(err, &e) && e.Code != "" && e.Message != "" {
		return e.Code, e.Message
	}
	switch {
	case errors.Is(err, ErrInvalid):
		return "invalid_request", "the request was refused as invalid"
	case errors.Is(err, ErrUnauthenticated):
		return "invalid_credential", "authentication is required"
	case errors.Is(err, ErrDenied):
		return "forbidden", "the caller lacks the required role"
	case errors.Is(err, ErrNotFound):
		return "not_found", "the resource was not found"
	case errors.Is(err, ErrIdempotencyConflict):
		return "idempotency_conflict", "the idempotency key was already used for a different request"
	case errors.Is(err, ErrConflict):
		return "conflict", "the request conflicts with current state"
	case errors.Is(err, ErrBudget):
		return "budget_exhausted", "the delegation budget is exhausted"
	}
	return "unavailable", "the service could not complete the request; retry with the same idempotency key"
}
