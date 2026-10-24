package domain

import "errors"

// ErrorCode is a stable machine-readable error identifier. Codes are
// independent of HTTP status; each interface contract maps its codes to
// statuses.
type ErrorCode string

// Codes shared by the public and runtime interfaces (see the HTTP contract).
const (
	CodeInvalidRequest      ErrorCode = "invalid_request"
	CodeInvalidCredential   ErrorCode = "invalid_credential"
	CodeForbidden           ErrorCode = "forbidden"
	CodeNotFound            ErrorCode = "not_found"
	CodeIdempotencyConflict ErrorCode = "idempotency_conflict"
	CodeVersionConflict     ErrorCode = "version_conflict"
	CodeVersionRequired     ErrorCode = "version_required"
	CodeRateLimited         ErrorCode = "rate_limited"
	CodeUnavailable         ErrorCode = "unavailable"
)

// Error is an error carrying a stable code.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// ErrConflict reports a unique-key violation. Adapters return it (or an error
// wrapping it) so callers match with errors.Is regardless of the store.
var ErrConflict = errors.New("domain: unique conflict")
