// Package httpapi_test pins the REST edge API and the behavior these tests
// require.
//
// # Exported API
//
//	type Router interface {
//	    Invoke(context.Context, ops.Invocation) (api.Result, error)
//	}
//	func NewEdge(Router) (*Edge, error)
//	func (*Edge) ServeHTTP(http.ResponseWriter, *http.Request)
//
// # Pinned behavior
//
// The REST edge is a thin transport over the shared router. It matches the
// declared method/path table (including the six product-fixed S08 endpoints),
// extracts the bearer credential, decodes exactly one bounded api.Request JSON
// body for every method (GET and DELETE included), and passes one
// ops.Invocation to the router. It never authenticates, authorizes, looks up a
// resource or performs an effect itself.
//
// A successful invocation is HTTP 200 with Content-Type application/json and
// the JSON api.Result body; the result's operation, body and Replayed fact are
// preserved. A refusal is the JSON api.Error body with the declared
// api.HTTPStatus mapping; a fault's public code/message is preserved while a
// private cause is never rendered. An unknown route is 404 not_found, a query
// string, malformed envelope or a body over the matched operation's
// api.MaxEnvelopeBytes bound (2 MiB for the three YAML-carrying operations,
// 1 MiB otherwise) is 400 invalid_input, and an encoded result over
// api.MaxResponseBytes or an invalid router result is 500 internal_failure with
// a sanitized message.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

type fakeRouter struct {
	mu    sync.Mutex
	fn    func(context.Context, ops.Invocation) (api.Result, error)
	calls int
	last  ops.Invocation
}

func (fake *fakeRouter) Invoke(ctx context.Context, invocation ops.Invocation) (api.Result, error) {
	fake.mu.Lock()
	fake.calls++
	fake.last = invocation
	fn := fake.fn
	fake.mu.Unlock()
	if fn != nil {
		return fn(ctx, invocation)
	}
	return api.Result{
		Operation: string(invocation.Operation),
		Body:      json.RawMessage(`{"ok":true}`),
	}, nil
}

func (fake *fakeRouter) callCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.calls
}

func (fake *fakeRouter) lastInvocation() ops.Invocation {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.last
}

// recorder is an in-process http.ResponseWriter; these tests never start a
// server or use the httptest package.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newRecorder() *recorder {
	return &recorder{header: make(http.Header)}
}

func (recorder *recorder) Header() http.Header { return recorder.header }

func (recorder *recorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
}

func (recorder *recorder) Write(data []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.body.Write(data)
}

func perform(t *testing.T, edge *httpapi.Edge, method, path string, body []byte, authorization string) *recorder {
	t.Helper()
	request, err := http.NewRequest(method, path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http.NewRequest(%s %s): %v", method, path, err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := newRecorder()
	edge.ServeHTTP(response, request)
	return response
}

func decodePublicError(t *testing.T, response *recorder) api.Error {
	t.Helper()
	var publicError api.Error
	if err := json.Unmarshal(response.body.Bytes(), &publicError); err != nil {
		t.Fatalf("decode api.Error from %q: %v", response.body.String(), err)
	}
	if err := publicError.Validate(); err != nil {
		t.Fatalf("invalid api.Error body %q: %v", response.body.String(), err)
	}
	return publicError
}

// requestBodyOfSize builds a valid api.Request body padded with insignificant
// JSON whitespace to an exact byte size.
func requestBodyOfSize(t *testing.T, size int) []byte {
	t.Helper()
	base := []byte(`{"input":{}}`)
	if size < len(base) {
		t.Fatalf("request size %d is smaller than the minimal body %d", size, len(base))
	}
	body := make([]byte, size)
	copy(body, base)
	for index := len(base); index < size; index++ {
		body[index] = ' '
	}
	return body
}

// resultOfEncodedSize builds an api.Result whose exact JSON encoding is size
// bytes.
func resultOfEncodedSize(t *testing.T, operation string, size int) api.Result {
	t.Helper()
	result := api.Result{Operation: operation, Body: json.RawMessage(`{"pad":""}`)}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal base result: %v", err)
	}
	if size < len(raw) {
		t.Fatalf("result size %d is smaller than the minimal encoding %d", size, len(raw))
	}
	padding := size - len(raw)
	result.Body = json.RawMessage(fmt.Sprintf(`{"pad":"%s"}`, strings.Repeat("x", padding)))
	raw, err = json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal padded result: %v", err)
	}
	if len(raw) != size {
		t.Fatalf("encoded result = %d bytes; want %d", len(raw), size)
	}
	return result
}
