// Package clienttest provides a recording fake factory API for CLI tests. It
// is a test helper: production code must not import it.
package clienttest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Recorded is one request as received by the fake.
type Recorded struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Server is an httptest server that records every request before handing it to
// the scripted handler.
type Server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []Recorded
}

// NewServer starts a loopback server closed with the test.
func NewServer(t testing.TB, h http.Handler) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, Recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), body})
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

// Requests returns a copy of the recorded requests.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.reqs...)
}

// Tokens is a fixed AccessTokenSource.
type Tokens string

// AccessToken implements client.AccessTokenSource.
func (t Tokens) AccessToken(context.Context) (string, error) { return string(t), nil }

// WriteJSON writes a JSON response with the standard headers.
func WriteJSON(w http.ResponseWriter, status int, v any, header map[string]string) {
	b, _ := json.Marshal(v)
	for k, val := range header {
		w.Header().Set(k, val)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, status int, code, message, requestID string, header map[string]string) {
	WriteJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": message, "requestId": requestID, "details": map[string]any{},
	}}, header)
}
