package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// APIPrefix is the only path prefix the client will address.
const APIPrefix = "/api/v1"

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)

// ErrInvalidRequest marks a local request or configuration validation failure.
// Callers map it to a usage exit without importing the CLI root package.
var ErrInvalidRequest = errors.New("invalid request")

// InvalidRequestError is a usage failure returned before any HTTP call.
type InvalidRequestError struct{ msg string }

func (e *InvalidRequestError) Error() string { return e.msg }

func (e *InvalidRequestError) Unwrap() error { return ErrInvalidRequest }

func invalidRequest(format string, args ...any) error {
	return &InvalidRequestError{msg: fmt.Sprintf(format, args...)}
}

// Scope selects the tenant and optionally the project a request addresses. It
// is routing only: the server authorizes every request independently, and a
// saved context is never authority.
type Scope struct {
	Tenant  domain.TenantID
	Project domain.ProjectID
}

// Request describes one API call. Path holds the segments after the scope
// (for example {"work-items", "wi_1"}); each is escaped as one segment.
type Request struct {
	Method string
	Scope  Scope
	Path   []string
	Query  url.Values
	// Body is marshaled as JSON once; nil sends no body.
	Body any
	// IfVersion, when nonzero, is sent as a strong quoted-integer If-Match.
	IfVersion domain.Version
	// IdempotencyKey is sent unchanged when nonempty.
	IdempotencyKey string
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

// buildPath validates the scope and segments and returns the escaped path
// (without a query string).
func (r Request) buildPath() (string, error) {
	if !allowedMethods[r.Method] {
		return "", fmt.Errorf("client: unsupported method %q", r.Method)
	}
	var b strings.Builder
	b.WriteString(APIPrefix)
	if r.Scope.Tenant == "" && r.Scope.Project != "" {
		return "", fmt.Errorf("client: a project requires a tenant")
	}
	if r.Scope.Tenant != "" {
		if err := r.Scope.Tenant.Validate(); err != nil {
			return "", fmt.Errorf("client: %w", err)
		}
		b.WriteString("/tenants/" + url.PathEscape(string(r.Scope.Tenant)))
	}
	if r.Scope.Project != "" {
		if err := r.Scope.Project.Validate(); err != nil {
			return "", fmt.Errorf("client: %w", err)
		}
		b.WriteString("/projects/" + url.PathEscape(string(r.Scope.Project)))
	}
	for _, seg := range r.Path {
		if err := validSegment(seg); err != nil {
			return "", err
		}
		b.WriteString("/" + url.PathEscape(seg))
	}
	return b.String(), nil
}

func validSegment(seg string) error {
	if seg == "" || seg == "." || seg == ".." || !utf8.ValidString(seg) {
		return invalidRequest("invalid path segment %q", seg)
	}
	for _, c := range seg {
		if c < 0x20 || c == 0x7f {
			return invalidRequest("path segment contains a control character")
		}
	}
	return nil
}

func (r Request) validateHeaders() error {
	if r.IfVersion != 0 {
		if err := r.IfVersion.Validate(); err != nil {
			return invalidRequest("%s", err.Error())
		}
	}
	if r.IdempotencyKey != "" && !idempotencyKeyPattern.MatchString(r.IdempotencyKey) {
		return invalidRequest("idempotency key must match %s", idempotencyKeyPattern)
	}
	return nil
}
