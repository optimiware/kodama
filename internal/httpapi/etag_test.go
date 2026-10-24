package httpapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
)

func Test_etag_validation(t *testing.T) {
	t.Run("format and parse round trip at the boundaries", func(t *testing.T) {
		for _, v := range []domain.Version{1, 2, 9, 10, 4096, domain.MaxSafeInteger} {
			tag, err := httpapi.FormatETag(v)
			if err != nil || tag != `"`+strconv.FormatInt(int64(v), 10)+`"` {
				t.Fatalf("FormatETag(%d) = %q, %v", v, tag, err)
			}
			if got, err := httpapi.ParseETag(tag); err != nil || got != v {
				t.Fatalf("ParseETag(%q) = %d, %v", tag, got, err)
			}
		}
		for _, v := range []domain.Version{0, -1, domain.MaxSafeInteger + 1} {
			if _, err := httpapi.FormatETag(v); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("FormatETag(%d) accepted", v)
			}
		}
	})

	rr := func(values ...string) *http.Request {
		r := httptest.NewRequest("PATCH", "/x", nil)
		for _, v := range values {
			r.Header.Add("If-Match", v)
		}
		return r
	}

	t.Run("missing header is 428", func(t *testing.T) {
		for _, r := range []*http.Request{rr(), rr("")} {
			_, err := httpapi.RequireIfMatch(r)
			var api *httpapi.APIError
			if !errors.As(err, &api) || api.Status != 428 || api.Code != domain.CodeVersionRequired {
				t.Fatalf("err = %v", err)
			}
		}
	})

	t.Run("valid header", func(t *testing.T) {
		if v, err := httpapi.RequireIfMatch(rr(`"7"`)); err != nil || v != 7 {
			t.Fatalf("got %d, %v", v, err)
		}
	})

	t.Run("malformed weak wildcard and multiple are 400", func(t *testing.T) {
		bad := [][]string{
			{`7`}, {`W/"7"`}, {`*`}, {`"7", "8"`}, {`"7","8"`}, {`"7"`, `"8"`}, {`""`}, {`"0"`}, {`"-1"`}, {`"+1"`}, {`"07"`},
			{`"7 "`}, {` "7"`}, {`"abc"`}, {`"9007199254740992"`}, {`"99999999999999999999"`}, {`"7`}, {`7"`}, {`'7'`}, {`"1e3"`}, {`"٣"`},
		}
		for _, vs := range bad {
			_, err := httpapi.RequireIfMatch(rr(vs...))
			var api *httpapi.APIError
			if !errors.As(err, &api) || api.Status != 400 || api.Code != domain.CodeInvalidRequest {
				t.Errorf("%q: err = %v", vs, err)
			}
		}
	})

	t.Run("max safe integer is accepted", func(t *testing.T) {
		if v, err := httpapi.RequireIfMatch(rr(`"9007199254740991"`)); err != nil || v != domain.MaxSafeInteger {
			t.Fatalf("got %d, %v", v, err)
		}
	})

	t.Run("stale version is 412", func(t *testing.T) {
		if err := httpapi.CheckVersion(3, 3); err != nil {
			t.Fatal(err)
		}
		var api *httpapi.APIError
		if err := httpapi.CheckVersion(3, 4); !errors.As(err, &api) || api.Status != 412 || api.Code != domain.CodeVersionConflict {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("SetETag", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := httpapi.SetETag(w, 5); err != nil || w.Header().Get("ETag") != `"5"` {
			t.Fatalf("%v %v", err, w.Header())
		}
		if err := httpapi.SetETag(w, 0); err == nil {
			t.Fatal("version 0 accepted")
		}
	})
}
