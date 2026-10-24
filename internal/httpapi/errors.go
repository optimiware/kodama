package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Codes owned by this transport layer in addition to the shared codes in
// package domain. They are typed as domain.ErrorCode so every module speaks one
// code type, but they are not added to the shared set: they have no meaning
// outside HTTP.
const (
	CodeInvalidCursor domain.ErrorCode = "invalid_cursor"
	CodeInternal      domain.ErrorCode = "internal_error"
)

// statusByCode maps stable codes to HTTP statuses (docs/contracts/http-api.md).
var statusByCode = map[domain.ErrorCode]int{
	domain.CodeInvalidRequest:      http.StatusBadRequest,
	CodeInvalidCursor:              http.StatusBadRequest,
	domain.CodeInvalidCredential:   http.StatusUnauthorized,
	domain.CodeForbidden:           http.StatusForbidden,
	domain.CodeNotFound:            http.StatusNotFound,
	domain.CodeIdempotencyConflict: http.StatusConflict,
	domain.CodeVersionConflict:     http.StatusPreconditionFailed,
	domain.CodeVersionRequired:     http.StatusPreconditionRequired,
	domain.CodeRateLimited:         http.StatusTooManyRequests,
	domain.CodeUnavailable:         http.StatusServiceUnavailable,
	CodeInternal:                   http.StatusInternalServerError,
}

// StatusFor returns the HTTP status of a shared code.
func StatusFor(code domain.ErrorCode) (int, bool) {
	s, ok := statusByCode[code]
	return s, ok
}

// APIError is an error that carries its HTTP rendering. Modules may return it
// from handlers for module-defined codes (for example 409 stale_gate_subject);
// WriteError renders it unchanged. Cause is for logs only and is never sent.
type APIError struct {
	Status  int
	Code    domain.ErrorCode
	Message string
	Details map[string]any
	Header  http.Header
	Cause   error
}

func (e *APIError) Error() string {
	if e.Cause != nil {
		return string(e.Code) + ": " + e.Message + ": " + e.Cause.Error()
	}
	return string(e.Code) + ": " + e.Message
}

// Unwrap exposes the cause to errors.Is/As.
func (e *APIError) Unwrap() error { return e.Cause }

// NewError builds an APIError for a module-defined status and code.
func NewError(status int, code domain.ErrorCode, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// WithDetails returns a copy of e with details attached. Details must hold only
// non-sensitive, client-safe values.
func (e *APIError) WithDetails(details map[string]any) *APIError {
	c := *e
	c.Details = details
	return &c
}

// BadRequestError is 400 invalid_request.
func BadRequestError(message string) *APIError {
	return NewError(http.StatusBadRequest, domain.CodeInvalidRequest, message)
}

// UnauthorizedError is 401 invalid_credential.
func UnauthorizedError() *APIError {
	e := NewError(http.StatusUnauthorized, domain.CodeInvalidCredential, "authentication is required")
	e.Header = http.Header{"Www-Authenticate": {`Bearer`}}
	return e
}

// ForbiddenError is 403 forbidden.
func ForbiddenError() *APIError {
	return NewError(http.StatusForbidden, domain.CodeForbidden, "the operation is not permitted")
}

// NotFoundError is 404 not_found. It is also used for hidden references, so
// the message is identical whether the resource is absent or not disclosable.
func NotFoundError() *APIError {
	return NewError(http.StatusNotFound, domain.CodeNotFound, "the resource was not found")
}

// VersionRequiredError is 428 version_required.
func VersionRequiredError() *APIError {
	return NewError(http.StatusPreconditionRequired, domain.CodeVersionRequired, "an If-Match header with the current version is required")
}

// VersionConflictError is 412 version_conflict.
func VersionConflictError() *APIError {
	return NewError(http.StatusPreconditionFailed, domain.CodeVersionConflict, "the resource changed; reread it and retry with the current version")
}

// IdempotencyConflictError is 409 idempotency_conflict.
func IdempotencyConflictError() *APIError {
	return NewError(http.StatusConflict, domain.CodeIdempotencyConflict, "the Idempotency-Key was already used for a different request")
}

// RateLimitedError is 429 rate_limited with Retry-After.
func RateLimitedError(retryAfter time.Duration) *APIError {
	e := NewError(http.StatusTooManyRequests, domain.CodeRateLimited, "too many requests; retry later")
	e.Header = http.Header{"Retry-After": {retryAfterSeconds(retryAfter)}}
	return e
}

// UnavailableError is 503 unavailable. retryAfter <= 0 omits Retry-After. The
// cause is retained for logs only.
func UnavailableError(retryAfter time.Duration, cause error) *APIError {
	e := NewError(http.StatusServiceUnavailable, domain.CodeUnavailable, "a required service is unavailable; retry later")
	e.Cause = cause
	if retryAfter > 0 {
		e.Header = http.Header{"Retry-After": {retryAfterSeconds(retryAfter)}}
	}
	return e
}

// InternalServerError is 500 internal_error with a fixed message.
func InternalServerError(cause error) *APIError {
	e := NewError(http.StatusInternalServerError, CodeInternal, "an internal error occurred")
	e.Cause = cause
	return e
}

// retryAfterSeconds renders whole seconds, rounded up, never below 1.
func retryAfterSeconds(d time.Duration) string {
	secs := int64(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}

// ResolveError converts any error into the APIError that will be sent. Unknown
// errors, including database driver errors, become a generic 500 so internals
// never reach the client.
func ResolveError(err error) *APIError {
	var api *APIError
	if errors.As(err, &api) {
		return api
	}
	var de *domain.Error
	if errors.As(err, &de) {
		if status, ok := StatusFor(de.Code); ok {
			return &APIError{Status: status, Code: de.Code, Message: de.Message, Cause: err}
		}
		return InternalServerError(err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return UnavailableError(time.Second, err)
	}
	return InternalServerError(err)
}

// WriteError renders err as the standard envelope with the request's ID. It
// never includes the cause. Server-side failures are logged with the request ID
// and the error's type chain, not its text or any request data.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	e := ResolveError(err)
	rid := RequestIDFrom(r.Context())
	if e.Status >= 500 {
		loggerFrom(r.Context()).LogAttrs(r.Context(), slog.LevelError, "request failed",
			slog.String("requestId", rid), slog.Int("status", e.Status),
			slog.String("code", string(e.Code)), slog.String("cause", describeCause(e)))
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	body, merr := json.Marshal(ErrorEnvelope{Error: Error{
		Code: string(e.Code), Message: e.Message, RequestId: rid, Details: details,
	}})
	if merr != nil { // details held an unmarshalable value: send a safe fallback.
		e = InternalServerError(merr)
		body, _ = json.Marshal(ErrorEnvelope{Error: Error{
			Code: string(e.Code), Message: e.Message, RequestId: rid, Details: map[string]any{},
		}})
	}
	h := w.Header()
	for k, vs := range e.Header {
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	setJSONHeaders(h)
	w.WriteHeader(e.Status)
	_, _ = w.Write(body)
}

// describeCause names the types in the error chain and any SQLSTATE, without
// error text that could contain driver or credential detail.
func describeCause(e *APIError) string {
	out := ""
	for err := error(e); err != nil; err = errors.Unwrap(err) {
		if out != "" {
			out += " <- "
		}
		out += fmt.Sprintf("%T", err)
		var st interface{ SQLState() string }
		if errors.As(err, &st) {
			out += "(sqlstate " + st.SQLState() + ")"
		}
	}
	return out
}
