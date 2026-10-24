package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

func newClient(t *testing.T, srv *clienttest.Server, mutate func(*client.Config)) *client.Client {
	t.Helper()
	cfg := client.Config{BaseURL: srv.URL, AllowInsecureLoopback: true, Tokens: clienttest.Tokens("tok-1")}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func scope() client.Scope { return client.Scope{Tenant: "t017_a", Project: "p017_a"} }

func Test_transport_contract(t *testing.T) {
	t.Run("version and key headers, scoped path, additive fields", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"id": "wi_1", "title": "x", "futureField": []int{1}},
				map[string]string{"ETag": `"4"`, "X-Request-ID": "req_abc", "Idempotent-Replayed": "true"})
		}))
		c := newClient(t, srv, nil)
		var out struct {
			ID string `json:"id"`
		}
		resp, err := c.Do(context.Background(), client.Request{
			Method: http.MethodPatch, Scope: scope(), Path: []string{"work-items", "wi_1"},
			Body: map[string]string{"spec": "s"}, IfVersion: 3, IdempotencyKey: "key-1",
		}, &out)
		if err != nil {
			t.Fatal(err)
		}
		if out.ID != "wi_1" || resp.Version != 4 || resp.RequestID != "req_abc" || !resp.Replayed || resp.ETag != `"4"` {
			t.Fatalf("metadata: %+v out=%+v", resp, out)
		}
		got := srv.Requests()[0]
		if got.Path != "/api/v1/tenants/t017_a/projects/p017_a/work-items/wi_1" || got.Method != "PATCH" {
			t.Fatalf("request line: %s %s", got.Method, got.Path)
		}
		if got.Header.Get("If-Match") != `"3"` || got.Header.Get("Idempotency-Key") != "key-1" ||
			got.Header.Get("Authorization") != "Bearer tok-1" || got.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("headers: %v", got.Header)
		}
		if string(got.Body) != `{"spec":"s"}` {
			t.Fatalf("body %q", got.Body)
		}
	})

	t.Run("invalid versions, keys and scopes are rejected before sending", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		c := newClient(t, srv, nil)
		bad := []client.Request{
			{Method: "GET", IfVersion: -1},
			{Method: "GET", IfVersion: domain.MaxSafeInteger + 1},
			{Method: "POST", IdempotencyKey: "has space"},
			{Method: "POST", IdempotencyKey: strings.Repeat("a", 129)},
			{Method: "GET", Scope: client.Scope{Project: "p"}},
			{Method: "GET", Scope: client.Scope{Tenant: "bad tenant"}},
			{Method: "TRACE"},
		}
		for i, r := range bad {
			if _, err := c.Do(context.Background(), r, nil); err == nil {
				t.Errorf("case %d: expected error", i)
			}
		}
		if n := len(srv.Requests()); n != 0 {
			t.Fatalf("%d requests were sent", n)
		}
	})

	t.Run("remote errors keep codes, request ids, details and retry guidance", func(t *testing.T) {
		cases := []struct {
			status int
			code   string
			header map[string]string
			retry  time.Duration
		}{
			{412, "version_conflict", nil, 0},
			{428, "version_required", nil, 0},
			{409, "idempotency_conflict", nil, 0},
			{429, "rate_limited", map[string]string{"Retry-After": "7"}, 7 * time.Second},
			{503, "unavailable", map[string]string{"Retry-After": "2"}, 2 * time.Second},
			{422, "a_code_added_in_the_future", nil, 0},
		}
		for _, tc := range cases {
			srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteError(w, tc.status, tc.code, "msg", "req_9", tc.header)
			}))
			c := newClient(t, srv, nil)
			resp, err := c.Do(context.Background(), client.Request{Method: "PUT", Scope: scope(), Path: []string{"x"}, IfVersion: 1}, nil)
			var re *client.RemoteError
			if !errors.As(err, &re) {
				t.Fatalf("%d: %v", tc.status, err)
			}
			if re.Status != tc.status || re.Code != tc.code || re.RequestID != "req_9" || re.RetryAfter != tc.retry || !re.Mutation || re.Malformed {
				t.Errorf("%d: %+v", tc.status, re)
			}
			if resp == nil || resp.RetryAfter != tc.retry {
				t.Errorf("%d: response metadata missing", tc.status)
			}
		}
	})

	t.Run("malformed and oversized responses are classified safely", func(t *testing.T) {
		type tc struct {
			name  string
			serve http.HandlerFunc
			check func(t *testing.T, err error)
		}
		isMalformed := func(t *testing.T, err error) {
			t.Helper()
			if !errors.Is(err, client.ErrMalformedResponse) {
				t.Fatalf("want malformed, got %v", err)
			}
		}
		for _, c := range []tc{
			{"truncated JSON", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"a"`))
			}, isMalformed},
			{"trailing data", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"a"} {}`))
			}, isMalformed},
			{"html success", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(`<html>`))
			}, isMalformed},
			{"empty success body", func(w http.ResponseWriter, r *http.Request) {}, isMalformed},
			{"oversized", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"` + strings.Repeat("a", 4096) + `"}`))
			}, func(t *testing.T, err error) {
				if !errors.Is(err, client.ErrResponseTooLarge) {
					t.Fatalf("got %v", err)
				}
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				srv := clienttest.NewServer(t, c.serve)
				cl := newClient(t, srv, func(cfg *client.Config) { cfg.MaxResponseBytes = 1024 })
				var out map[string]any
				_, err := cl.Do(context.Background(), client.Request{Method: "POST", Scope: scope(), Path: []string{"x"}}, &out)
				c.check(t, err)
				var te *client.TransportError
				if !errors.As(err, &te) || !te.Uncertain {
					t.Fatalf("mutation outcome must be flagged uncertain: %v", err)
				}
			})
		}

		t.Run("non-JSON error body", func(t *testing.T) {
			srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "<h1>bad gateway</h1>", 502)
			}))
			_, err := newClient(t, srv, nil).Do(context.Background(), client.Request{Method: "GET", Scope: scope(), Path: []string{"x"}}, nil)
			var re *client.RemoteError
			if !errors.As(err, &re) || !re.Malformed || re.Status != 502 || re.Code != "" {
				t.Fatalf("got %+v", err)
			}
		})
	})

	t.Run("cancellation and timeout return transport errors", func(t *testing.T) {
		release := make(chan struct{})
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer close(release)
		c := newClient(t, srv, nil)

		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		_, err := c.Do(ctx, client.Request{Method: "POST", Scope: scope(), Path: []string{"x"}}, nil)
		var te *client.TransportError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &te) || !te.Uncertain {
			t.Fatalf("cancel: %v", err)
		}

		ctx, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel2()
		_, err = c.Do(ctx, client.Request{Method: "GET", Scope: scope(), Path: []string{"x"}}, nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout: %v", err)
		}
		if errors.As(err, &te) && te.Uncertain {
			t.Fatal("a read must not be flagged uncertain")
		}
	})

	t.Run("a failed mutation is never replayed automatically", func(t *testing.T) {
		var n atomic.Int32
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			clienttest.WriteError(w, 503, "unavailable", "later", "req_1", map[string]string{"Retry-After": "1"})
		}))
		c := newClient(t, srv, nil)
		_, err := c.Do(context.Background(), client.Request{Method: "POST", Scope: scope(), Path: []string{"runs"}, IdempotencyKey: "k", Body: map[string]string{"a": "b"}}, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		time.Sleep(1200 * time.Millisecond) // longer than the retry guidance
		if n.Load() != 1 {
			t.Fatalf("server saw %d requests", n.Load())
		}
	})

	t.Run("a connection reset after sending is an uncertain transport error", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}))
		_, err := newClient(t, srv, nil).Do(context.Background(), client.Request{Method: "POST", Scope: scope(), Path: []string{"x"}}, nil)
		var te *client.TransportError
		if !errors.As(err, &te) || !te.Uncertain {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a lost answer on a reused connection never replays the mutation", func(t *testing.T) {
		cases := []struct {
			name string
			req  client.Request
		}{
			{"keyed with body", client.Request{Method: "POST", Scope: scope(), Path: []string{"runs"}, IdempotencyKey: "key-1", Body: map[string]string{"a": "b"}}},
			{"keyed without body", client.Request{Method: "POST", Scope: scope(), Path: []string{"runs", "r1", "cancel"}, IdempotencyKey: "key-2"}},
			{"keyed delete", client.Request{Method: "DELETE", Scope: scope(), Path: []string{"runs", "r1"}, IdempotencyKey: "key-3", IfVersion: 2}},
			{"unkeyed with body", client.Request{Method: "PUT", Scope: scope(), Path: []string{"runs", "r1"}, IfVersion: 2, Body: map[string]string{"a": "b"}}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var mutations atomic.Int32
				srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						clienttest.WriteJSON(w, 200, map[string]any{}, nil)
						return
					}
					mutations.Add(1)
					conn, _, err := w.(http.Hijacker).Hijack() // the mutation was received; drop the answer
					if err == nil {
						_ = conn.Close()
					}
				}))
				transport := &http.Transport{MaxIdleConnsPerHost: 1}
				t.Cleanup(transport.CloseIdleConnections)
				c := newClient(t, srv, func(cfg *client.Config) { cfg.HTTP = &http.Client{Transport: transport} })

				// Warm a keep-alive connection so the mutation reuses it, the
				// case in which net/http considers a resend.
				if _, err := c.Do(context.Background(), client.Request{Method: "GET", Scope: scope(), Path: []string{"runs"}}, nil); err != nil {
					t.Fatal(err)
				}
				_, err := c.Do(context.Background(), tc.req, nil)
				var te *client.TransportError
				if !errors.As(err, &te) || !te.Uncertain {
					t.Fatalf("an uncertain outcome must reach the caller: %v", err)
				}
				if n := mutations.Load(); n != 1 {
					t.Fatalf("the server received the mutation %d times", n)
				}
				if got := srv.Requests(); len(got) != 2 {
					t.Fatalf("server saw %d requests", len(got))
				}
			})
		}
	})

	t.Run("a keyed bodyless mutation still carries its key on the wire", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{}, nil)
		}))
		_, err := newClient(t, srv, nil).Do(context.Background(), client.Request{Method: "POST", Scope: scope(), Path: []string{"x"}, IdempotencyKey: "key-9"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := srv.Requests()[0].Header.Get("Idempotency-Key"); got != "key-9" {
			t.Fatalf("key = %q", got)
		}
	})

	t.Run("token source failures stop the request", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		c := newClient(t, srv, func(cfg *client.Config) { cfg.Tokens = failingTokens{} })
		_, err := c.Do(context.Background(), client.Request{Method: "GET", Scope: scope()}, nil)
		if !errors.Is(err, client.ErrNotAuthenticated) || len(srv.Requests()) != 0 {
			t.Fatalf("err=%v requests=%d", err, len(srv.Requests()))
		}
		for _, tok := range []string{"", "a b", "a\r\nX-Evil: 1"} {
			c := newClient(t, srv, func(cfg *client.Config) { cfg.Tokens = clienttest.Tokens(tok) })
			if _, err := c.Do(context.Background(), client.Request{Method: "GET", Scope: scope()}, nil); !errors.Is(err, client.ErrNotAuthenticated) {
				t.Errorf("token %q: %v", tok, err)
			}
		}
	})
}

type failingTokens struct{}

func (failingTokens) AccessToken(context.Context) (string, error) {
	return "", errors.Join(client.ErrNotAuthenticated, errors.New("none"))
}

func Test_token_destination_isolation(t *testing.T) {
	var otherHits atomic.Int32
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()

	for _, status := range []int{301, 302, 307, 308} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", status)
		}))
		_, err := newClient(t, srv, nil).Do(context.Background(), client.Request{Method: "GET", Scope: scope(), Path: []string{"x"}}, nil)
		var re *client.RemoteError
		if !errors.As(err, &re) || !re.Malformed || re.Status != status {
			t.Fatalf("status %d: %v", status, err)
		}
	}
	if otherHits.Load() != 0 {
		t.Fatalf("redirect was followed; credential header %v", leaked.Load())
	}

	t.Run("segments are escaped exactly once on the wire", func(t *testing.T) {
		type seen struct{ raw, decoded string }
		var got []seen
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{}, nil)
		}))
		c := newClient(t, srv, nil)
		cases := []struct{ seg, wire string }{
			{"a b", "a%20b"},
			{"caf\u00e9", "caf%C3%A9"},
			{"100%", "100%25"},
			{"a%20b", "a%2520b"},
			{"a/b", "a%2Fb"},
			{"x?y=1#z", "x%3Fy=1%23z"},
			{"plain-._~", "plain-._~"},
		}
		for _, tc := range cases {
			if _, err := c.Do(context.Background(), client.Request{Method: "GET", Scope: scope(), Path: []string{"work-items", tc.seg}}, nil); err != nil {
				t.Fatalf("%q: %v", tc.seg, err)
			}
		}
		for _, r := range srv.Requests() {
			parts := strings.Split(r.Path, "/")
			last := parts[len(parts)-1]
			dec, err := url.PathUnescape(last)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, seen{last, dec})
			if r.RawQuery != "" {
				t.Errorf("a path segment leaked into the query: %q", r.RawQuery)
			}
		}
		for i, tc := range cases {
			if got[i].raw != tc.wire || got[i].decoded != tc.seg {
				t.Errorf("segment %q: wire %q (want %q), server-decoded %q", tc.seg, got[i].raw, tc.wire, got[i].decoded)
			}
		}
	})

	t.Run("segments cannot escape the API prefix or the host", func(t *testing.T) {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{}, nil)
		}))
		c := newClient(t, srv, nil)
		for _, seg := range []string{"..", ".", "", "a\nb"} {
			if _, err := c.Do(context.Background(), client.Request{Method: "GET", Path: []string{seg}}, nil); err == nil {
				t.Errorf("segment %q accepted", seg)
			}
		}
		for _, seg := range []string{"a/../../admin", "http://evil.invalid/x", "%2e%2e", "x?y=1#z"} {
			if _, err := c.Do(context.Background(), client.Request{Method: "GET", Path: []string{seg}}, nil); err != nil {
				t.Fatalf("escaped segment %q: %v", seg, err)
			}
		}
		for _, r := range srv.Requests() {
			if !strings.HasPrefix(r.Path, "/api/v1/") || strings.Count(r.Path, "/") != 3 {
				t.Errorf("segment escaped as multiple path elements: %q", r.Path)
			}
		}
	})

	t.Run("base URL must be a bare https origin", func(t *testing.T) {
		for _, raw := range []string{
			"", "factory.example.invalid", "http://factory.example.invalid", "ftp://x", "https://",
			"https://u:p@factory.example.invalid", "https://factory.example.invalid/api/v1",
			"https://factory.example.invalid?x=1", "https://factory.example.invalid#f",
		} {
			if _, err := client.New(client.Config{BaseURL: raw, Tokens: clienttest.Tokens("t")}); err == nil {
				t.Errorf("accepted %q", raw)
			}
		}
		if _, err := client.New(client.Config{BaseURL: "http://factory.example.invalid", AllowInsecureLoopback: true, Tokens: clienttest.Tokens("t")}); err == nil {
			t.Error("insecure exception must be limited to loopback")
		}
		if _, err := client.New(client.Config{BaseURL: "https://factory.example.invalid/", Tokens: clienttest.Tokens("t")}); err != nil {
			t.Errorf("bare https origin rejected: %v", err)
		}
		if _, err := client.New(client.Config{BaseURL: "https://factory.example.invalid"}); err == nil {
			t.Error("missing token source accepted")
		}
	})
}

// Test_authenticated_scope_denial uses T-004's separate authentication and
// authorization ports: a valid credential does not bypass membership denial.
// It is boundary evidence, not T-010 verifier qualification.
func Test_authenticated_scope_denial(t *testing.T) {
	router, err := httpapi.NewRouter(httpapi.RouterConfig{
		Authenticator: tokenAuth{"tok-1": "alice"},
		Authorizer:    memberAuthz{"alice": {"t017_a": true}},
		RateLimiter:   httpapi.Unlimited(),
		Prefix:        client.APIPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(httpapi.Route{
		Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/things", Action: "thing.list",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
			httpapi.WriteJSON(w, r, 200, map[string]any{"items": []any{}, "nextCursor": nil})
		},
	}); err != nil {
		t.Fatal(err)
	}
	srv := clienttest.NewServer(t, router)
	do := func(tok string, sc client.Scope) error {
		c := newClient(t, srv, func(cfg *client.Config) { cfg.Tokens = clienttest.Tokens(tok) })
		_, err := c.Do(context.Background(), client.Request{Method: "GET", Scope: sc, Path: []string{"things"}}, &map[string]any{})
		return err
	}

	if err := do("tok-1", client.Scope{Tenant: "t017_a", Project: "p1"}); err != nil {
		t.Fatalf("member: %v", err)
	}
	var re *client.RemoteError
	// Valid credential, but another tenant: membership is not implied by the token or saved scope.
	if err := do("tok-1", client.Scope{Tenant: "t017_b", Project: "p1"}); !errors.As(err, &re) || re.Status != 403 || re.Code != "forbidden" || re.RequestID == "" {
		t.Fatalf("cross-tenant: %v", err)
	}
	if err := do("bad", client.Scope{Tenant: "t017_a", Project: "p1"}); !errors.As(err, &re) || re.Status != 401 || re.Code != "invalid_credential" {
		t.Fatalf("bad credential: %v", err)
	}
}

type tokenAuth map[string]string

func (a tokenAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id, ok := a[tok]
	if !ok {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: id}, nil
}

type memberAuthz map[string]map[string]bool

func (m memberAuthz) Authorize(_ context.Context, p httpapi.Principal, _ httpapi.Action, t httpapi.Target) error {
	if m[p.ID][string(t.TenantID)] {
		return nil
	}
	return httpapi.ErrDenied
}
