// Package api declares the public operation envelopes, routes, events and
// verification/report schemas shared by the REST edge, the MCP edge and the
// Userland client.
//
// The package is a declaration-only schema package. It imports only the
// standard library and never a product package, so the Shell, Kernel and
// Userland can share these values without a dependency on an implementation.
// Nothing here starts a process, opens a socket, reads a database or performs
// an effect; the decode/validate helpers only shape and refuse values.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Version is the public REST API version segment.
const Version = "v1"

// MediaType is the only public request and response content type.
const MediaType = "application/json"

// Public bounds. They bound a request/response, one event payload, an
// idempotency key, an opaque cursor, a page, one model body, one subject and one
// forwarded credential.
const (
	// MaxRequestBytes bounds one public request body (1 MiB).
	MaxRequestBytes = 1 << 20
	// MaxResponseBytes bounds one public response body (8 MiB).
	MaxResponseBytes = 8 << 20
	// MaxInlineYAMLBytes bounds one inline pipeline YAML document (1 MiB).
	MaxInlineYAMLBytes = 1 << 20
	// MaxYAMLRequestBytes bounds one request envelope that may carry inline
	// YAML (2 MiB): a 1 MiB document expands to about 1.4 MiB of base64.
	MaxYAMLRequestBytes = 2 << 20
	// MaxInlineYAMLEncodedBytes bounds the base64 encoding of one inline YAML
	// document, which is what the wire member advertises.
	MaxInlineYAMLEncodedBytes = (MaxInlineYAMLBytes + 2) / 3 * 4
	// MaxEventDataBytes bounds one event payload (64 KiB).
	MaxEventDataBytes = 64 << 10
	// MaxIdempotencyKeyBytes bounds one caller-supplied idempotency key.
	MaxIdempotencyKeyBytes = 128
	// MaxCursorBytes bounds one opaque pagination cursor.
	MaxCursorBytes = 512
	// DefaultPageLimit is the page size used when a caller omits a limit.
	DefaultPageLimit = 50
	// MaxPageLimit is the largest accepted page size.
	MaxPageLimit = 200
	// MaxModelBodyBytes bounds one OpenAI-compatible request body (1 MiB).
	MaxModelBodyBytes = 1 << 20
	// MaxModelMessages bounds one model request message list.
	MaxModelMessages = 128
	// MaxCompletionTokens bounds requested completion tokens.
	MaxCompletionTokens = 32768
	// MaxSubjectFields bounds one typed verification subject.
	MaxSubjectFields = 64
	// MaxSubjectValueBytes bounds one typed subject value.
	MaxSubjectValueBytes = 4096
	// MaxCredentialBytes bounds one forwarded actor credential.
	MaxCredentialBytes = 4096
)

// maxIDLength mirrors the neutral identity bound without importing it.
const maxIDLength = 128

// maxReportTypeLength mirrors the declared report-type bound.
const maxReportTypeLength = 128

// maxCursorSortKeyBytes bounds the sort key a cursor may carry.
const maxCursorSortKeyBytes = 256

// cursorVersion is the version prefix inside every opaque cursor.
const cursorVersion = "v1"

// Public error codes. They are exactly the product fault codes; api repeats the
// vocabulary so the schema package stays independent of Shell internals.
const (
	// CodeInvalidInput reports a refused request shape or value.
	CodeInvalidInput = "invalid_input"
	// CodeUnauthenticated reports a missing or unverifiable principal.
	CodeUnauthenticated = "unauthenticated"
	// CodeForbidden reports an authenticated principal without authority.
	CodeForbidden = "forbidden"
	// CodeNotFound reports a missing entity visible to the caller's scope.
	CodeNotFound = "not_found"
	// CodeVersionConflict reports a stale expected version.
	CodeVersionConflict = "version_conflict"
	// CodeChangedIdempotencyContent reports a retry whose content changed.
	CodeChangedIdempotencyContent = "changed_idempotency_content"
	// CodeDeadlineExpired reports work refused at or past its deadline.
	CodeDeadlineExpired = "deadline_expired"
	// CodeUnsupportedCapability reports admitted work the adapter cannot run.
	CodeUnsupportedCapability = "unsupported_capability"
	// CodeDependencyUnavailable reports an unavailable external dependency.
	CodeDependencyUnavailable = "dependency_unavailable"
	// CodeUncertainExternalEffect reports an effect whose outcome is unknown.
	CodeUncertainExternalEffect = "uncertain_external_effect"
	// CodeInternalFailure reports an unexpected internal failure.
	CodeInternalFailure = "internal_failure"
)

// errorCodes is the declaration order of the public codes.
var errorCodes = []string{
	CodeInvalidInput,
	CodeUnauthenticated,
	CodeForbidden,
	CodeNotFound,
	CodeVersionConflict,
	CodeChangedIdempotencyContent,
	CodeDeadlineExpired,
	CodeUnsupportedCapability,
	CodeDependencyUnavailable,
	CodeUncertainExternalEffect,
	CodeInternalFailure,
}

// ErrorCodes returns every public error code in declaration order.
func ErrorCodes() []string {
	return append([]string(nil), errorCodes...)
}

// IsErrorCode reports whether code is one of the declared public codes.
func IsErrorCode(code string) bool {
	for _, candidate := range errorCodes {
		if candidate == code {
			return true
		}
	}
	return false
}

// Error is the stable public error body. It never carries a private cause.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// Error renders the public code and message.
func (e Error) Error() string {
	switch {
	case e.Code == "":
		return e.Message
	case e.Message == "":
		return e.Code
	default:
		return e.Code + ": " + e.Message
	}
}

// Validate checks that the body carries a declared code and a public message.
func (e Error) Validate() error {
	if !IsErrorCode(e.Code) {
		return invalid("unknown public error code %q", e.Code)
	}
	if e.Message == "" {
		return invalid("public error message is empty")
	}
	return nil
}

// invalid builds a stable public invalid_input refusal.
func invalid(format string, arguments ...any) error {
	return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf(format, arguments...)}
}

// Method is one supported HTTP method.
type Method string

const (
	// MethodGet reads a representation.
	MethodGet Method = "GET"
	// MethodPost creates a resource or submits an operation.
	MethodPost Method = "POST"
	// MethodPut replaces a representation.
	MethodPut Method = "PUT"
	// MethodPatch updates part of a representation.
	MethodPatch Method = "PATCH"
	// MethodDelete removes a representation.
	MethodDelete Method = "DELETE"
)

// methods is the declaration order.
var methods = []Method{MethodGet, MethodPost, MethodPut, MethodPatch, MethodDelete}

// Valid reports whether the method is one of the supported methods.
func (method Method) Valid() bool {
	for _, candidate := range methods {
		if candidate == method {
			return true
		}
	}
	return false
}

// Route is one public operation's REST and MCP binding.
type Route struct {
	Operation    string `json:"operation"`
	Method       Method `json:"method"`
	Path         string `json:"path"`
	MCPTool      string `json:"mcp_tool"`
	InputSchema  string `json:"input_schema"`
	ResultSchema string `json:"result_schema"`
}

// Request is the single transport representation of one operation input.
type Request struct {
	Input           json.RawMessage `json:"input"`
	ExpectedVersion *uint64         `json:"expected_version"`
	IdempotencyKey  string          `json:"idempotency_key"`
}

// Validate checks the expected version and idempotency key and that a present
// input is a JSON object. The empty request is valid.
func (request Request) Validate() error {
	if request.ExpectedVersion != nil && *request.ExpectedVersion == 0 {
		return invalid("expected_version must be a positive version")
	}
	if len(request.IdempotencyKey) > MaxIdempotencyKeyBytes {
		return invalid("idempotency_key is %d bytes; at most %d are allowed", len(request.IdempotencyKey), MaxIdempotencyKeyBytes)
	}
	if request.IdempotencyKey != "" && !validIdempotencyKey(request.IdempotencyKey) {
		return invalid("idempotency_key must match [A-Za-z0-9._:-]")
	}
	if !optionalJSONObject(request.Input) {
		return invalid("input must be a JSON object")
	}
	return nil
}

// Result is the single transport representation of one operation result.
type Result struct {
	Operation string          `json:"operation"`
	Body      json.RawMessage `json:"body,omitempty"`
	Replayed  bool            `json:"replayed,omitempty"`
}

// Validate checks the operation name and that a present body is a JSON object.
func (result Result) Validate() error {
	if _, ok := LookupRoute(result.Operation); !ok {
		return invalid("unknown operation %q", result.Operation)
	}
	if !optionalJSONObject(result.Body) {
		return invalid("result body must be a JSON object")
	}
	return nil
}

// Invocation is one decoded operation call shared by the REST and MCP edges.
type Invocation struct {
	Operation string  `json:"operation"`
	Request   Request `json:"request"`
}

// Validate checks the registered operation and the request envelope.
func (invocation Invocation) Validate() error {
	if _, ok := LookupRoute(invocation.Operation); !ok {
		return invalid("unknown operation %q", invocation.Operation)
	}
	return invocation.Request.Validate()
}

// Handler is the shared operation boundary both public edges call.
type Handler interface {
	Invoke(ctx context.Context, invocation Invocation) (Result, error)
}

// Page is the bounded pagination input shared by list operations.
type Page struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// Validate checks the page limit and cursor bound.
func (page Page) Validate() error {
	if page.Limit < 0 || page.Limit > MaxPageLimit {
		return invalid("page limit %d is outside 0..%d", page.Limit, MaxPageLimit)
	}
	if len(page.Cursor) > MaxCursorBytes {
		return invalid("page cursor is %d bytes; at most %d are allowed", len(page.Cursor), MaxCursorBytes)
	}
	return nil
}

// EffectiveLimit returns the limit a caller actually asked for.
func (page Page) EffectiveLimit() int {
	if page.Limit == 0 {
		return DefaultPageLimit
	}
	return page.Limit
}

// PageInfo is the pagination continuation of one list result.
type PageInfo struct {
	NextCursor string `json:"next_cursor,omitempty"`
}

// EncodeCursor turns a sort key into an opaque, versioned, unpadded base64url
// token. The token carries no scope and no secret.
func EncodeCursor(sortKey string) (string, error) {
	if sortKey == "" {
		return "", invalid("cursor sort key is empty")
	}
	if len(sortKey) > maxCursorSortKeyBytes {
		return "", invalid("cursor sort key is %d bytes; at most %d are allowed", len(sortKey), maxCursorSortKeyBytes)
	}
	if strings.ContainsAny(sortKey, "\x00\n\r") {
		return "", invalid("cursor sort key contains a control character")
	}
	payload := cursorVersion + "\n" + sortKey
	cursor := base64.RawURLEncoding.EncodeToString([]byte(payload))
	if len(cursor) > MaxCursorBytes {
		return "", invalid("encoded cursor exceeds %d bytes", MaxCursorBytes)
	}
	return cursor, nil
}

// DecodeCursor validates an opaque cursor and returns its sort key.
func DecodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", invalid("cursor is empty")
	}
	if len(cursor) > MaxCursorBytes {
		return "", invalid("cursor is %d bytes; at most %d are allowed", len(cursor), MaxCursorBytes)
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", invalid("cursor is not unpadded base64url")
	}
	if base64.RawURLEncoding.EncodeToString(payload) != cursor {
		return "", invalid("cursor is not canonical base64url")
	}
	separator := strings.IndexByte(string(payload), '\n')
	if separator < 0 {
		return "", invalid("cursor payload has no version separator")
	}
	version := string(payload[:separator])
	sortKey := string(payload[separator+1:])
	if version != cursorVersion {
		return "", invalid("cursor version %q is not supported", version)
	}
	if sortKey == "" {
		return "", invalid("cursor sort key is empty")
	}
	if len(sortKey) > maxCursorSortKeyBytes {
		return "", invalid("cursor sort key is too long")
	}
	if strings.ContainsAny(sortKey, "\x00\n\r") {
		return "", invalid("cursor sort key contains a control character")
	}
	return sortKey, nil
}

// Output is one declared pipeline output. A reference is an ordinary string; it
// is never an artifact entity.
type Output struct {
	Name      string `json:"name"`
	Reference string `json:"reference"`
}

// Validate checks that the output has a name and a non-empty reference.
func (output Output) Validate() error {
	if output.Name == "" {
		return invalid("output name is empty")
	}
	if output.Reference == "" {
		return invalid("output reference is empty")
	}
	return nil
}

// validIdempotencyKey reports whether key matches [A-Za-z0-9._:-].
func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > MaxIdempotencyKeyBytes {
		return false
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '.', character == '_', character == ':', character == '-':
		default:
			return false
		}
	}
	return true
}

// validID reports whether value is a bounded ASCII identity payload.
func validID(value string) bool {
	if value == "" || len(value) > maxIDLength {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// optionalJSONObject reports whether value is absent, JSON null or a JSON
// object.
func optionalJSONObject(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || trimmed == "null" {
		return true
	}
	if !json.Valid([]byte(trimmed)) {
		return false
	}
	return strings.HasPrefix(trimmed, "{")
}

// jsonObject reports whether value is a JSON object.
func jsonObject(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return false
	}
	return strings.HasPrefix(trimmed, "{")
}
