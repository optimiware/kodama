package httpapi_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

var testKey = bytes.Repeat([]byte{7}, 32)

func baseScope() httpapi.CursorScope {
	return httpapi.CursorScope{
		TenantID: "t1", ProjectID: "p1",
		Endpoint: "GET /api/v1/tenants/{tenantId}/projects/{projectId}/work-items",
		Filters:  map[string][]string{"state": {"backlog", "ready"}, "kind": {"story"}},
		Sort:     "createdAt:asc,id:asc",
	}
}

func isInvalidCursor(err error) bool {
	var api *httpapi.APIError
	return errors.As(err, &api) && api.Status == 400 && api.Code == httpapi.CodeInvalidCursor && errors.Is(err, httpapi.ErrInvalidCursor)
}

func Test_cursor_scope(t *testing.T) {
	codec, err := httpapi.NewCursorCodec(testKey)
	if err != nil {
		t.Fatal(err)
	}
	pos := []byte(`{"c":"2026-01-01T00:00:00Z","id":"wi_9"}`)
	token, err := codec.Encode(baseScope(), pos)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("round trip", func(t *testing.T) {
		got, err := codec.Decode(baseScope(), token)
		if err != nil || !bytes.Equal(got, pos) {
			t.Fatalf("got %q, %v", got, err)
		}
		// Filter map order and a different map instance do not matter.
		s := baseScope()
		s.Filters = map[string][]string{"kind": {"story"}, "state": {"backlog", "ready"}}
		if _, err := codec.Decode(s, token); err != nil {
			t.Fatalf("equivalent scope rejected: %v", err)
		}
	})

	t.Run("a copied cursor cannot change tenant project endpoint filters or sort", func(t *testing.T) {
		mutations := map[string]func(*httpapi.CursorScope){
			"tenant":         func(s *httpapi.CursorScope) { s.TenantID = "t2" },
			"project":        func(s *httpapi.CursorScope) { s.ProjectID = "p2" },
			"endpoint":       func(s *httpapi.CursorScope) { s.Endpoint = "GET /api/v1/tenants/{tenantId}/projects/{projectId}/runs" },
			"sort":           func(s *httpapi.CursorScope) { s.Sort = "createdAt:desc,id:desc" },
			"filter value":   func(s *httpapi.CursorScope) { s.Filters = map[string][]string{"state": {"backlog"}, "kind": {"story"}} },
			"filter order":   func(s *httpapi.CursorScope) { s.Filters["state"] = []string{"ready", "backlog"} },
			"filter removed": func(s *httpapi.CursorScope) { delete(s.Filters, "kind") },
			"filter added":   func(s *httpapi.CursorScope) { s.Filters["owner"] = []string{"me"} },
			"filter renamed": func(s *httpapi.CursorScope) {
				s.Filters = map[string][]string{"state": {"backlog", "ready"}, "type": {"story"}}
			},
			"field shifting": func(s *httpapi.CursorScope) { s.TenantID, s.ProjectID = "t1p", "1" },
		}
		for name, mutate := range mutations {
			s := baseScope()
			mutate(&s)
			if _, err := codec.Decode(s, token); !isInvalidCursor(err) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})

	t.Run("tampering and malformed tokens fail", func(t *testing.T) {
		body, sig, _ := strings.Cut(token, ".")
		flip := func(s string) string {
			b := []byte(s)
			if b[3] == 'A' {
				b[3] = 'B'
			} else {
				b[3] = 'A'
			}
			return string(b)
		}
		bad := []string{
			"", ".", "x", "x.y", token + "x", token[:len(token)-2], flip(body) + "." + sig, body + "." + flip(sig),
			sig + "." + body, body + ".", "." + sig, token + ".extra", strings.Repeat("A", httpapi.MaxCursorBytes+1),
			"!!!." + sig, body + ".!!!",
		}
		for _, b := range bad {
			if _, err := codec.Decode(baseScope(), b); !isInvalidCursor(err) {
				t.Errorf("%.40q: err = %v", b, err)
			}
		}
	})

	t.Run("another key cannot mint or read cursors", func(t *testing.T) {
		other, _ := httpapi.NewCursorCodec(bytes.Repeat([]byte{9}, 32))
		forged, _ := other.Encode(baseScope(), pos)
		if _, err := codec.Decode(baseScope(), forged); !isInvalidCursor(err) {
			t.Fatalf("forged cursor accepted: %v", err)
		}
	})

	t.Run("key rotation keeps issued cursors valid", func(t *testing.T) {
		rotated, err := httpapi.NewCursorCodec(bytes.Repeat([]byte{9}, 32), testKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rotated.Decode(baseScope(), token); err != nil {
			t.Fatalf("old cursor rejected after rotation: %v", err)
		}
		fresh, _ := rotated.Encode(baseScope(), pos)
		if _, err := codec.Decode(baseScope(), fresh); !isInvalidCursor(err) {
			t.Fatal("new-key cursor accepted by old-key codec")
		}
	})

	t.Run("position and key bounds", func(t *testing.T) {
		if _, err := httpapi.NewCursorCodec([]byte("short")); err == nil {
			t.Error("short key accepted")
		}
		if _, err := httpapi.NewCursorCodec(testKey, []byte("short")); err == nil {
			t.Error("short previous key accepted")
		}
		for _, p := range [][]byte{nil, {}, make([]byte, httpapi.MaxCursorPositionBytes+1)} {
			if _, err := codec.Encode(baseScope(), p); err == nil {
				t.Errorf("position of %d bytes accepted", len(p))
			}
		}
		max := bytes.Repeat([]byte{'a'}, httpapi.MaxCursorPositionBytes)
		tok, err := codec.Encode(baseScope(), max)
		if err != nil || len(tok) > httpapi.MaxCursorBytes {
			t.Fatalf("max position: %v len %d", err, len(tok))
		}
		if got, err := codec.Decode(baseScope(), tok); err != nil || !bytes.Equal(got, max) {
			t.Fatalf("max position round trip: %v", err)
		}
	})

	t.Run("an unconfigured codec never accepts", func(t *testing.T) {
		var nilCodec *httpapi.CursorCodec
		if _, err := nilCodec.Decode(baseScope(), token); err == nil {
			t.Fatal("nil codec accepted a cursor")
		}
		if _, err := nilCodec.Encode(baseScope(), pos); err == nil {
			t.Fatal("nil codec issued a cursor")
		}
	})

	t.Run("a valid cursor grants no permission", func(t *testing.T) {
		h := newHarness(t, nil)
		scopeFor := func(c *httpapi.Call, r *http.Request) httpapi.CursorScope {
			return c.CursorScope(map[string][]string{"state": r.URL.Query()["state"]}, "id:asc")
		}
		h.handle(httpapi.Route{
			Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/work-items", Action: "work.list",
			Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
				req, err := codec.ParsePage(r, scopeFor(c, r))
				if err != nil {
					httpapi.WriteError(w, r, err)
					return
				}
				page, err := httpapi.BuildPage(codec, scopeFor(c, r), req, []string{"a", "b", "c"}[:min(3, req.FetchLimit())], func(s string) ([]byte, error) { return []byte(s), nil })
				if err != nil {
					httpapi.WriteError(w, r, err)
					return
				}
				httpapi.WriteJSON(w, r, 200, page)
			},
		})
		first := h.do("GET", "/api/v1/tenants/t1/projects/p1/work-items?state=ready&limit=2", "", bearer("tok-alice")...)
		if first.Code != 200 {
			t.Fatalf("first page: %d %s", first.Code, first.Body)
		}
		next := nextCursor(t, first)
		url := "/api/v1/tenants/t1/projects/p1/work-items?state=ready&limit=2&cursor=" + next
		if rec := h.do("GET", url, "", bearer("tok-alice")...); rec.Code != 200 {
			t.Fatalf("continuation: %d %s", rec.Code, rec.Body)
		}
		before := h.authz.count()
		// Revocation between pages blocks continuation, and the check is made
		// again for the continuation request.
		h.authz.set(func(c authzCall) error {
			if c.Principal.ID == "alice" {
				return httpapi.ErrDenied
			}
			return nil
		})
		rec := h.do("GET", url, "", bearer("tok-alice")...)
		if rec.Code != 403 || strings.Contains(rec.Body.String(), `"items"`) {
			t.Fatalf("revoked continuation: %d %s", rec.Code, rec.Body)
		}
		if h.authz.count() != before+1 {
			t.Fatal("continuation did not reauthorize")
		}
		// Copying the cursor to another project or filter set fails even for an
		// authorized principal; to another tenant is 400 as well.
		h.authz.set(nil)
		for _, u := range []string{
			"/api/v1/tenants/t1/projects/p2/work-items?state=ready&limit=2&cursor=" + next,
			"/api/v1/tenants/t2/projects/p1/work-items?state=ready&limit=2&cursor=" + next,
			"/api/v1/tenants/t1/projects/p1/work-items?state=done&limit=2&cursor=" + next,
		} {
			rec := h.do("GET", u, "", bearer("tok-alice")...)
			if rec.Code != 400 || rec.envelope(t).Error.Code != "invalid_cursor" {
				t.Errorf("%s: %d %s", u, rec.Code, rec.Body)
			}
		}
	})
}

func Test_cursor_scope_binds_path_identifiers(t *testing.T) {
	codec, err := httpapi.NewCursorCodec(testKey)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil)
	h.handle(httpapi.Route{
		Method: "GET", Path: "/tenants/{tenantId}/projects/{projectId}/runs/{runId}/steps", Action: "step.list",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
			scope := c.CursorScope(nil, "seq:asc")
			req, err := codec.ParsePage(r, scope)
			if err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			page, err := httpapi.BuildPage(codec, scope, req, []string{"a", "b", "c"}[:min(3, req.FetchLimit())], func(s string) ([]byte, error) { return []byte(s), nil })
			if err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			httpapi.WriteJSON(w, r, 200, page)
		},
	})
	const base = "/api/v1/tenants/t1/projects/p1/runs/"
	first := h.do("GET", base+"r1/steps?limit=2", "", bearer("tok-alice")...)
	if first.Code != 200 {
		t.Fatalf("first page: %d %s", first.Code, first.Body)
	}
	next := nextCursor(t, first)
	if rec := h.do("GET", base+"r1/steps?limit=2&cursor="+next, "", bearer("tok-alice")...); rec.Code != 200 {
		t.Fatalf("same-resource continuation: %d %s", rec.Code, rec.Body)
	}
	// The caller may be authorized for both runs; the cursor still belongs to r1.
	rec := h.do("GET", base+"r2/steps?limit=2&cursor="+next, "", bearer("tok-alice")...)
	if rec.Code != 400 || rec.envelope(t).Error.Code != "invalid_cursor" {
		t.Fatalf("cursor copied to another run: %d %s", rec.Code, rec.Body)
	}
	// And the first page of r2 issues a cursor that r1 rejects.
	other := nextCursor(t, h.do("GET", base+"r2/steps?limit=2", "", bearer("tok-alice")...))
	if other == next {
		t.Fatal("two runs issued identical cursors")
	}
	if rec := h.do("GET", base+"r1/steps?limit=2&cursor="+other, "", bearer("tok-alice")...); rec.Code != 400 {
		t.Fatalf("reverse copy: %d %s", rec.Code, rec.Body)
	}
}

var _ = httptest.NewRecorder
var _ = domain.CodeNotFound
