package client

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

var (
	// ErrNotAuthenticated is returned (wrapped) by an AccessTokenSource that
	// holds no usable credential. The CLI maps it to exit code 3 and never
	// answers it by starting a login.
	ErrNotAuthenticated = errors.New("client: no usable credential")
	// ErrMalformedResponse marks a response that is not the complete JSON
	// document the caller expected. After a mutation the outcome is unknown.
	ErrMalformedResponse = errors.New("client: malformed response")
	// ErrResponseTooLarge marks a response body above the configured bound.
	ErrResponseTooLarge = errors.New("client: response too large")
)

// RemoteError is a non-2xx answer from the factory. Unknown codes and
// additive envelope fields are accepted: Code is whatever the server sent.
// A body that is not the standard envelope yields Malformed with an empty Code.
type RemoteError struct {
	Status     int
	Code       string
	Message    string
	RequestID  string
	Details    map[string]any
	RetryAfter time.Duration // zero when the server gave no guidance
	Malformed  bool
	// Mutation is true for a non-GET request: a retry must reread the resource
	// or replay the identical keyed request, never a modified one.
	Mutation bool
}

func (e *RemoteError) Error() string {
	if e.Malformed {
		return fmt.Sprintf("remote %d: unexpected response", e.Status)
	}
	s := fmt.Sprintf("remote %d %s: %s", e.Status, e.Code, e.Message)
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// DetailKeys returns the detail names in deterministic order.
func (e *RemoteError) DetailKeys() []string {
	keys := make([]string, 0, len(e.Details))
	for k := range e.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TransportError is a failure to obtain any HTTP response, including
// cancellation and timeout. For a mutation the request may have been applied.
type TransportError struct {
	Method string
	Path   string
	// Uncertain is true for a mutation: the server may have applied it.
	Uncertain bool
	Err       error
}

func (e *TransportError) Error() string {
	s := fmt.Sprintf("client: %s %s: %v", e.Method, e.Path, e.Err)
	if e.Uncertain {
		s += " (outcome unknown: reread the resource, or replay the identical keyed request)"
	}
	return s
}

func (e *TransportError) Unwrap() error { return e.Err }

func isMutation(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}
