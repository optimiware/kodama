package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
)

func Test_json_bounds(t *testing.T) {
	type payload struct {
		Title string `json:"title"`
	}
	run := func(t *testing.T, cfg func(*httpapi.RouterConfig), body string, headers ...string) response {
		h := newHarness(t, cfg)
		h.handle(httpapi.Route{Method: "POST", Path: "/echo", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			var p payload
			if err := httpapi.DecodeJSON(w, r, &p); err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			httpapi.WriteJSON(w, r, 200, p)
		}})
		return h.do("POST", "/api/v1/echo", body, append(bearer("tok-alice"), headers...)...)
	}

	t.Run("valid", func(t *testing.T) {
		if rec := run(t, nil, `{"title":"x"}`); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"title":"x"}` {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("rejections never echo the body", func(t *testing.T) {
		for name, body := range map[string]string{
			"unknown field": `{"title":"x","secret-field":"hunter2"}`,
			"trailing":      `{"title":"x"} {"title":"hunter2"}`,
			"array":         `["hunter2"]`,
			"wrong type":    `{"title":5}`,
			"truncated":     `{"title":"hunter2`,
		} {
			rec := run(t, nil, body)
			if rec.Code != 400 || rec.envelope(t).Error.Code != "invalid_request" || strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
		if rec := run(t, nil, ""); rec.Code != 415 && rec.Code != 400 {
			t.Errorf("empty body: %d", rec.Code)
		}
	})
	t.Run("oversized body is 413", func(t *testing.T) {
		body := `{"title":"` + strings.Repeat("a", 200) + `"}`
		rec := run(t, func(c *httpapi.RouterConfig) { c.MaxRequestBytes = 64 }, body)
		if rec.Code != 413 || rec.envelope(t).Error.Code != "invalid_request" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if rec := run(t, func(c *httpapi.RouterConfig) { c.MaxRequestBytes = int64(len(body)) }, body); rec.Code != 200 {
			t.Fatalf("body at the bound rejected: %d", rec.Code)
		}
	})
	t.Run("content type", func(t *testing.T) {
		h := newHarness(t, nil)
		h.handle(httpapi.Route{Method: "POST", Path: "/echo", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			var p payload
			if err := httpapi.DecodeJSON(w, r, &p); err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			w.WriteHeader(204)
		}})
		for ct, want := range map[string]int{"application/json": 204, "application/json; charset=utf-8": 204, "text/plain": 415, "": 415, "application/jsonx": 415} {
			rec := h.do("POST", "/api/v1/echo", `{"title":"x"}`, append(bearer("tok-alice"), "Content-Type", ct)...)
			if rec.Code != want {
				t.Errorf("%q: %d", ct, rec.Code)
			}
		}
	})
	t.Run("oversized response is a clean 500", func(t *testing.T) {
		h := newHarness(t, func(c *httpapi.RouterConfig) { c.MaxResponseBytes = 32 })
		h.handle(httpapi.Route{Method: "GET", Path: "/big", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, _ *httpapi.Call) {
			httpapi.WriteJSON(w, r, 200, map[string]string{"v": strings.Repeat("a", 100)})
		}})
		rec := h.do("GET", "/api/v1/big", "", bearer("tok-alice")...)
		if rec.Code != 500 || rec.envelope(t).Error.Code != "internal_error" {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("invalid router configuration", func(t *testing.T) {
		for _, p := range []string{"api", "/api/", "/{x}"} {
			_, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: &fakeAuth{}, Authorizer: &fakeAuthz{}, RateLimiter: httpapi.Unlimited(), Prefix: p})
			if err == nil {
				t.Errorf("prefix %q accepted", p)
			}
		}
		_, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: &fakeAuth{}, Authorizer: &fakeAuthz{}, RateLimiter: httpapi.Unlimited(), MaxRequestBytes: -1})
		if err == nil {
			t.Error("negative bound accepted")
		}
	})
}
