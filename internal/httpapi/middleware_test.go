package httpapi_test

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

const workPath = "/tenants/{tenantId}/projects/{projectId}/work-items/{id}"

var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

func registerWork(h *harness, called *int) {
	h.handle(httpapi.Route{
		Method: http.MethodGet, Path: workPath, Action: "work.read",
		ResourceKind: "work-item", ResourceParam: "id",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
			*called++
			httpapi.WriteJSON(w, r, http.StatusOK, map[string]string{"id": c.Target.Resource.ID, "tenant": string(c.Target.TenantID)})
		},
	})
}

func Test_transport_denials(t *testing.T) {
	url := "/api/v1/tenants/t1/projects/p1/work-items/w1"

	t.Run("absent wiring never serves", func(t *testing.T) {
		ok := httpapi.RouterConfig{Authenticator: &fakeAuth{}, Authorizer: &fakeAuthz{}, RateLimiter: httpapi.Unlimited()}
		for name, mutate := range map[string]func(*httpapi.RouterConfig){
			"authenticator": func(c *httpapi.RouterConfig) { c.Authenticator = nil },
			"authorizer":    func(c *httpapi.RouterConfig) { c.Authorizer = nil },
			"limiter":       func(c *httpapi.RouterConfig) { c.RateLimiter = nil },
		} {
			cfg := ok
			mutate(&cfg)
			if rt, err := httpapi.NewRouter(cfg); err == nil || rt != nil {
				t.Errorf("router without %s was created", name)
			}
		}
		var zero httpapi.Router
		rec := newResponse()
		zero.ServeHTTP(rec.ResponseRecorder, newRequest(http.MethodGet, url))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("zero router status = %d", rec.Code)
		}
		if err := zero.Handle(httpapi.Route{Method: "GET", Path: "/x", Action: "a", Handler: func(http.ResponseWriter, *http.Request, *httpapi.Call) {}}); err == nil {
			t.Error("zero router accepted a route")
		}
	})

	t.Run("route metadata is mandatory", func(t *testing.T) {
		h := newHarness(t, nil)
		noop := func(http.ResponseWriter, *http.Request, *httpapi.Call) {}
		for name, r := range map[string]httpapi.Route{
			"no action":                 {Method: "GET", Path: "/a", Handler: noop},
			"no handler":                {Method: "GET", Path: "/a", Action: "x"},
			"bad method":                {Method: "TRACE", Path: "/a", Action: "x", Handler: noop},
			"project without tenant":    {Method: "GET", Path: "/p/{projectId}", Action: "x", Handler: noop},
			"resource kind only":        {Method: "GET", Path: "/a/{id}", Action: "x", ResourceKind: "k", Handler: noop},
			"resource wildcard missing": {Method: "GET", Path: "/a/{id}", Action: "x", ResourceKind: "k", ResourceParam: "other", Handler: noop},
			"rest wildcard":             {Method: "GET", Path: "/a/{rest...}", Action: "x", Handler: noop},
			"duplicate wildcard":        {Method: "GET", Path: "/a/{id}/{id}", Action: "x", Handler: noop},
		} {
			if err := h.router.Handle(r); err == nil {
				t.Errorf("%s: route accepted", name)
			}
		}
		h.handle(httpapi.Route{Method: "GET", Path: "/dup", Action: "x", Handler: noop})
		if err := h.router.Handle(httpapi.Route{Method: "GET", Path: "/dup", Action: "x", Handler: noop}); err == nil {
			t.Error("conflicting pattern accepted")
		}
		if err := h.router.HandlePublic("POST", "/health", func(http.ResponseWriter, *http.Request) {}); err == nil {
			t.Error("public POST accepted")
		}
	})

	t.Run("identity and authorization gate the handler", func(t *testing.T) {
		type tc struct {
			name    string
			headers []string
			setup   func(*harness)
			status  int
			code    domain.ErrorCode
		}
		cases := []tc{
			{"missing credential", nil, nil, 401, domain.CodeInvalidCredential},
			{"invalid credential", bearer("nope"), nil, 401, domain.CodeInvalidCredential},
			{"authenticator outage", bearer("tok-alice"), func(h *harness) { h.auth.err = errors.New("db password=hunter2 down") }, 503, domain.CodeUnavailable},
			{"authorization denied", bearer("tok-alice"), func(h *harness) {
				h.authz.set(func(authzCall) error { return httpapi.ErrDenied })
			}, 403, domain.CodeForbidden},
			{"hidden reference", bearer("tok-alice"), func(h *harness) {
				h.authz.set(func(authzCall) error { return httpapi.ErrHidden })
			}, 404, domain.CodeNotFound},
			{"authorizer outage", bearer("tok-alice"), func(h *harness) {
				h.authz.set(func(authzCall) error { return errors.New("pg: password=hunter2 refused") })
			}, 503, domain.CodeUnavailable},
			{"limiter outage fails closed", bearer("tok-alice"), func(h *harness) {
				h.limiter.allow = func(httpapi.RateKey) (httpapi.RateDecision, error) {
					return httpapi.RateDecision{}, errors.New("redis down")
				}
			}, 503, domain.CodeUnavailable},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				h := newHarness(t, nil)
				called := 0
				registerWork(h, &called)
				if c.setup != nil {
					c.setup(h)
				}
				rec := h.do("GET", url, "", c.headers...)
				if rec.Code != c.status {
					t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body)
				}
				if called != 0 {
					t.Fatal("protected handler ran")
				}
				env := rec.envelope(t)
				if env.Error.Code != string(c.code) {
					t.Errorf("code = %q", env.Error.Code)
				}
				if env.Error.RequestId != rec.Header().Get("X-Request-ID") || !requestIDPattern.MatchString(env.Error.RequestId) {
					t.Errorf("request id = %q header %q", env.Error.RequestId, rec.Header().Get("X-Request-ID"))
				}
				if strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "redis") {
					t.Errorf("error leaked internals: %s", rec.Body)
				}
				if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
					t.Errorf("headers = %v", rec.Header())
				}
			})
		}
	})

	t.Run("no authorization before authentication", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		h.do("GET", url, "")
		if h.authz.count() != 0 || len(h.limiter.keys) != 0 {
			t.Fatal("authorizer or limiter consulted for an unauthenticated request")
		}
	})

	t.Run("hidden reference is indistinguishable from not found", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		h.authz.set(func(authzCall) error { return httpapi.ErrHidden })
		hidden := h.do("GET", url, "", bearer("tok-alice")...).envelope(t).Error
		missing := h.do("GET", "/api/v1/nowhere", "", bearer("tok-alice")...).envelope(t).Error
		hidden.RequestId, missing.RequestId = "", ""
		if hidden.Code != missing.Code || hidden.Message != missing.Message {
			t.Fatalf("hidden %+v differs from missing %+v", hidden, missing)
		}
	})

	t.Run("authorizer receives verified principal and scope", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		rec := h.do("GET", url, "", bearer("tok-bob")...)
		if rec.Code != 200 || called != 1 {
			t.Fatalf("status = %d called = %d: %s", rec.Code, called, rec.Body)
		}
		got := h.authz.calls[0]
		want := authzCall{
			Principal: httpapi.Principal{ID: "bob"}, Action: "work.read",
			Target: httpapi.Target{TenantID: "t1", ProjectID: "p1", Resource: httpapi.Reference{Kind: "work-item", ID: "w1"}},
		}
		if got != want {
			t.Fatalf("authorize call = %+v, want %+v", got, want)
		}
	})

	t.Run("invalid identifiers are rejected before authorization", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		for _, bad := range []string{"/api/v1/tenants/-t/projects/p1/work-items/w1", "/api/v1/tenants/t1/projects/p%2F1/work-items/w1", "/api/v1/tenants/t1/projects/p1/work-items/w.1"} {
			rec := h.do("GET", bad, "", bearer("tok-alice")...)
			if rec.Code != 400 {
				t.Errorf("%s: status %d", bad, rec.Code)
			}
		}
		if h.authz.count() != 0 || called != 0 {
			t.Fatal("authorizer or handler reached with invalid identifiers")
		}
	})

	t.Run("unmatched routes use the envelope", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		rec := h.do("GET", "/api/v1/unknown", "", bearer("tok-alice")...)
		if rec.Code != 404 || rec.envelope(t).Error.Code != "not_found" {
			t.Errorf("404 case: %d %s", rec.Code, rec.Body)
		}
		rec = h.do("DELETE", url, "", bearer("tok-alice")...)
		if rec.Code != 405 || rec.Header().Get("Allow") == "" || rec.envelope(t).Error.Code != "invalid_request" {
			t.Errorf("405 case: %d %v %s", rec.Code, rec.Header(), rec.Body)
		}
		if h.auth.calls != 0 {
			t.Error("authenticator called for an unmatched route")
		}
	})

	t.Run("request ids are server generated and unique", func(t *testing.T) {
		h := newHarness(t, nil)
		a := h.do("GET", "/api/v1/x", "", "X-Request-ID", "client-chosen")
		b := h.do("GET", "/api/v1/x", "")
		ida, idb := a.Header().Get("X-Request-ID"), b.Header().Get("X-Request-ID")
		if ida == "client-chosen" || ida == idb || !requestIDPattern.MatchString(ida) {
			t.Fatalf("ids %q %q", ida, idb)
		}
	})

	t.Run("handler panics and unknown errors become safe 500", func(t *testing.T) {
		h := newHarness(t, nil)
		h.handle(httpapi.Route{Method: "GET", Path: "/boom", Action: "x", Handler: func(http.ResponseWriter, *http.Request, *httpapi.Call) {
			panic("secret panic payload")
		}})
		h.handle(httpapi.Route{Method: "GET", Path: "/err", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			httpapi.WriteError(w, r, errors.New(`pq: connection to "postgres://u:hunter2@db" failed`))
		}})
		for _, p := range []string{"/api/v1/boom", "/api/v1/err"} {
			rec := h.do("GET", p, "", bearer("tok-alice")...)
			if rec.Code != 500 || rec.envelope(t).Error.Code != "internal_error" || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("%s: %d %s", p, rec.Code, rec.Body)
			}
		}
	})

	t.Run("module errors render with their code", func(t *testing.T) {
		h := newHarness(t, nil)
		h.handle(httpapi.Route{Method: "GET", Path: "/stale", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			httpapi.WriteError(w, r, httpapi.NewError(409, "stale_gate_subject", "subject changed"))
		}})
		h.handle(httpapi.Route{Method: "GET", Path: "/domain", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			httpapi.WriteError(w, r, &domain.Error{Code: domain.CodeVersionConflict, Message: "wi_1 changed"})
		}})
		if rec := h.do("GET", "/api/v1/stale", "", bearer("tok-alice")...); rec.Code != 409 || rec.envelope(t).Error.Code != "stale_gate_subject" {
			t.Errorf("module error: %d %s", rec.Code, rec.Body)
		}
		if rec := h.do("GET", "/api/v1/domain", "", bearer("tok-alice")...); rec.Code != 412 {
			t.Errorf("domain error: %d %s", rec.Code, rec.Body)
		}
	})

	t.Run("public routes are explicit", func(t *testing.T) {
		h := newHarness(t, nil)
		if err := h.router.HandlePublic("GET", "/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }); err != nil {
			t.Fatal(err)
		}
		if rec := h.do("GET", "/api/v1/healthz", ""); rec.Code != 204 || rec.Header().Get("X-Request-ID") == "" {
			t.Fatalf("public: %d %v", rec.Code, rec.Header())
		}
	})
}
