package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

// fakeAuth authenticates bearer tokens from a table: token -> principal ID.
type fakeAuth struct {
	mu     sync.Mutex
	tokens map[string]string
	err    error
	calls  int
}

func (f *fakeAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return httpapi.Principal{}, f.err
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	id, ok := f.tokens[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

type authzCall struct {
	Principal httpapi.Principal
	Action    httpapi.Action
	Target    httpapi.Target
}

// fakeAuthz allows what the policy function allows and records every call.
type fakeAuthz struct {
	mu     sync.Mutex
	policy func(authzCall) error
	calls  []authzCall
}

func (f *fakeAuthz) Authorize(_ context.Context, p httpapi.Principal, a httpapi.Action, t httpapi.Target) error {
	c := authzCall{p, a, t}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	policy := f.policy
	f.mu.Unlock()
	if policy == nil {
		return nil
	}
	return policy(c)
}

func (f *fakeAuthz) set(policy func(authzCall) error) {
	f.mu.Lock()
	f.policy = policy
	f.mu.Unlock()
}

func (f *fakeAuthz) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeLimiter struct {
	mu    sync.Mutex
	keys  []httpapi.RateKey
	allow func(httpapi.RateKey) (httpapi.RateDecision, error)
}

func (f *fakeLimiter) Allow(_ context.Context, k httpapi.RateKey) (httpapi.RateDecision, error) {
	f.mu.Lock()
	f.keys = append(f.keys, k)
	allow := f.allow
	f.mu.Unlock()
	if allow == nil {
		return httpapi.RateDecision{Allowed: true}, nil
	}
	return allow(k)
}

type harness struct {
	t       *testing.T
	auth    *fakeAuth
	authz   *fakeAuthz
	limiter *fakeLimiter
	router  *httpapi.Router
}

func newHarness(t *testing.T, cfg func(*httpapi.RouterConfig)) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		auth:    &fakeAuth{tokens: map[string]string{"tok-alice": "alice", "tok-bob": "bob"}},
		authz:   &fakeAuthz{},
		limiter: &fakeLimiter{},
	}
	rc := httpapi.RouterConfig{Authenticator: h.auth, Authorizer: h.authz, RateLimiter: h.limiter, Prefix: "/api/v1"}
	if cfg != nil {
		cfg(&rc)
	}
	rt, err := httpapi.NewRouter(rc)
	if err != nil {
		t.Fatal(err)
	}
	h.router = rt
	return h
}

func (h *harness) handle(route httpapi.Route) {
	h.t.Helper()
	if err := h.router.Handle(route); err != nil {
		h.t.Fatal(err)
	}
}

type response struct {
	*httptest.ResponseRecorder
}

func (r response) envelope(t *testing.T) httpapi.ErrorEnvelope {
	t.Helper()
	var e httpapi.ErrorEnvelope
	dec := json.NewDecoder(strings.NewReader(r.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("body is not an error envelope: %v: %q", err, r.Body.String())
	}
	return e
}

func (h *harness) do(method, target string, body string, headers ...string) response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	if body != "" && len(req.Header.Values("Content-Type")) == 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return response{rec}
}

func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newResponse() response { return response{httptest.NewRecorder()} }

func newRequest(method, target string) *http.Request { return httptest.NewRequest(method, target, nil) }

func fmtSscan(s string, n *int) (int, error) { return fmt.Sscan(s, n) }

func asAPI(err error, target **httpapi.APIError) bool { return errors.As(err, target) }
