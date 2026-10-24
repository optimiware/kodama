package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

func nextCursor(t *testing.T, rec response) string {
	t.Helper()
	var page struct {
		Items      []string `json:"items"`
		NextCursor *string  `json:"nextCursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page.NextCursor == nil {
		t.Fatalf("no next cursor: %v %s", err, rec.Body)
	}
	return *page.NextCursor
}

func Test_pagination_and_throttling(t *testing.T) {
	codec, _ := httpapi.NewCursorCodec(testKey)
	scope := baseScope()
	get := func(q string) *http.Request { return httptest.NewRequest("GET", "/x?"+q, nil) }

	t.Run("limits", func(t *testing.T) {
		for q, want := range map[string]int{"": 50, "limit=1": 1, "limit=50": 50, "limit=200": 200} {
			if got, err := httpapi.ParseLimit(get(q)); err != nil || got != want {
				t.Errorf("%q: %d, %v", q, got, err)
			}
		}
		for _, q := range []string{"limit=0", "limit=-1", "limit=201", "limit=abc", "limit=", "limit=1.5", "limit=01", "limit=+5", "limit=1&limit=2", "limit=99999999999999999999"} {
			_, err := httpapi.ParseLimit(get(q))
			var api *httpapi.APIError
			if err == nil || !asAPI(err, &api) || api.Status != 400 {
				t.Errorf("%q accepted: %v", q, err)
			}
		}
	})

	t.Run("unknown and repeated parameters are rejected", func(t *testing.T) {
		if err := httpapi.CheckQuery(get("state=a&limit=2&cursor=x"), "state"); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"owner=me", "cursor=a&cursor=b"} {
			if err := httpapi.CheckQuery(get(q), "state"); err == nil {
				t.Errorf("%q accepted", q)
			}
		}
	})

	t.Run("stable continuation and exhausted shape", func(t *testing.T) {
		all := []string{"a", "b", "c", "d", "e"}
		pos := func(s string) ([]byte, error) { return []byte(s), nil }
		fetch := func(req httpapi.PageRequest) []string {
			start := 0
			if req.Position != nil {
				for i, s := range all {
					if s == string(req.Position) {
						start = i + 1
					}
				}
			}
			end := min(len(all), start+req.FetchLimit())
			return all[start:end]
		}
		var seen []string
		var req httpapi.PageRequest
		req.Limit = 2
		pages := 0
		for {
			page, err := httpapi.BuildPage(codec, scope, req, fetch(req), pos)
			if err != nil {
				t.Fatal(err)
			}
			pages++
			seen = append(seen, page.Items...)
			if page.NextCursor == nil {
				break
			}
			r, err := codec.ParsePage(get("limit=2&cursor="+*page.NextCursor), scope)
			if err != nil {
				t.Fatal(err)
			}
			req = r
		}
		if strings.Join(seen, "") != "abcde" || pages != 3 {
			t.Fatalf("seen %v in %d pages", seen, pages)
		}

		empty, err := httpapi.BuildPage[string](codec, scope, httpapi.PageRequest{Limit: 5}, nil, pos)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(empty)
		if string(b) != `{"items":[],"nextCursor":null}` {
			t.Fatalf("exhausted list = %s", b)
		}
		exact, _ := httpapi.BuildPage(codec, scope, httpapi.PageRequest{Limit: 2}, []string{"a", "b"}, pos)
		if exact.NextCursor != nil || len(exact.Items) != 2 {
			t.Fatalf("exactly-full final page = %+v", exact)
		}
		if _, err := httpapi.BuildPage(codec, scope, httpapi.PageRequest{Limit: 1}, []string{"a", "b", "c"}, pos); err == nil {
			t.Fatal("module returning too many rows accepted")
		}
		if _, err := httpapi.BuildPage(codec, scope, httpapi.PageRequest{Limit: 0}, []string{"a"}, pos); err == nil {
			t.Fatal("invalid limit accepted")
		}
	})

	t.Run("429 carries scoped key and retry guidance", func(t *testing.T) {
		h := newHarness(t, nil)
		called := 0
		registerWork(h, &called)
		h.limiter.allow = func(k httpapi.RateKey) (httpapi.RateDecision, error) {
			if k.TenantID == "t1" {
				return httpapi.RateDecision{Allowed: false, RetryAfter: 1500 * time.Millisecond}, nil
			}
			return httpapi.RateDecision{Allowed: true}, nil
		}
		rec := h.do("GET", "/api/v1/tenants/t1/projects/p1/work-items/w1", "", bearer("tok-alice")...)
		if rec.Code != 429 || rec.Header().Get("Retry-After") != "2" || rec.envelope(t).Error.Code != "rate_limited" {
			t.Fatalf("throttled: %d %v %s", rec.Code, rec.Header(), rec.Body)
		}
		if called != 0 || h.authz.count() != 0 {
			t.Fatal("throttled request reached authorization or the handler")
		}
		want := httpapi.RateKey{Principal: httpapi.Principal{ID: "alice"}, TenantID: "t1", ProjectID: "p1", Action: "work.read"}
		if h.limiter.keys[0] != want {
			t.Fatalf("limiter key = %+v", h.limiter.keys[0])
		}
		if rec := h.do("GET", "/api/v1/tenants/t2/projects/p1/work-items/w1", "", bearer("tok-alice")...); rec.Code != 200 {
			t.Fatalf("other tenant throttled: %d", rec.Code)
		}
	})

	t.Run("retry after is a positive integer", func(t *testing.T) {
		for _, d := range []time.Duration{0, -time.Second, time.Millisecond, time.Second, 90 * time.Second} {
			e := httpapi.RateLimitedError(d)
			var n int
			if _, err := fmtSscan(e.Header.Get("Retry-After"), &n); err != nil || n < 1 {
				t.Errorf("Retry-After for %v = %q", d, e.Header.Get("Retry-After"))
			}
		}
	})

	t.Run("limiter is deliberate", func(t *testing.T) {
		d, err := httpapi.Unlimited().Allow(withTimeout(t), httpapi.RateKey{})
		if err != nil || !d.Allowed {
			t.Fatalf("Unlimited = %+v, %v", d, err)
		}
	})
}
