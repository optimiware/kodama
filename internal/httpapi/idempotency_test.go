package httpapi_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
)

func Test_idempotency_request_identity(t *testing.T) {
	var got httpapi.RequestIdentity
	var gotErr error
	h := newHarness(t, nil)
	h.handle(httpapi.Route{
		Method: "POST", Path: "/tenants/{tenantId}/projects/{projectId}/runs", Action: "run.create",
		Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
			body, err := httpapi.ReadBody(w, r)
			if err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			got, gotErr = c.Identity(r, body)
			if gotErr != nil {
				httpapi.WriteError(w, r, gotErr)
				return
			}
			w.WriteHeader(204)
		},
	})
	post := func(path, body string, headers ...string) response {
		got, gotErr = httpapi.RequestIdentity{}, nil
		return h.do("POST", path, body, append(bearer("tok-alice"), headers...)...)
	}

	t.Run("canonical route is concrete and the hash is separate", func(t *testing.T) {
		a := post("/api/v1/tenants/t1/projects/p1/runs", `{"a":1}`, "Idempotency-Key", "key-1")
		if a.Code != 204 {
			t.Fatalf("%d %s", a.Code, a.Body)
		}
		first := got
		if first.Route != "/api/v1/tenants/t1/projects/p1/runs" || first.Method != "POST" || first.TenantID != "t1" ||
			first.ProjectID != "p1" || first.Principal.ID != "alice" || first.Key != "key-1" {
			t.Fatalf("identity = %+v", first)
		}
		post("/api/v1/tenants/t1/projects/p2/runs", `{"a":1}`, "Idempotency-Key", "key-1")
		if got.Route == first.Route || got.Hash != first.Hash {
			t.Fatalf("another project: route %q, hashes equal %v", got.Route, got.Hash == first.Hash)
		}
		post("/api/v1/tenants/t1/projects/p1/runs", `{"a":2}`, "Idempotency-Key", "key-1")
		if got.Route != first.Route || got.Key != first.Key || got.Hash == first.Hash {
			t.Fatal("changed body must keep the lookup key and change only the hash")
		}
		post("/api/v1/tenants/t1/projects/p1/runs", `{"a":1}`, "Idempotency-Key", "key-1")
		if got.Hash != first.Hash {
			t.Fatal("hash is not deterministic")
		}
	})

	t.Run("hash covers body bytes, query and If-Match", func(t *testing.T) {
		base := func(body, query string, im ...string) httpapi.RequestHash {
			r := httptest.NewRequest("POST", "/x"+query, strings.NewReader(body))
			for _, v := range im {
				r.Header.Add("If-Match", v)
			}
			return httpapi.HashRequest(r, []byte(body))
		}
		ref := base(`{"a":1}`, "")
		for name, other := range map[string]httpapi.RequestHash{
			"body bytes":   base(`{"a": 1}`, ""),
			"query":        base(`{"a":1}`, "?dry=1"),
			"if-match":     base(`{"a":1}`, "", `"3"`),
			"if-match 2":   base(`{"a":1}`, "", `"3"`, `"4"`),
			"empty body":   base(``, ""),
			"query & body": base(``, "?a=1"),
		} {
			if other == ref {
				t.Errorf("%s does not change the hash", name)
			}
		}
		if base("", "?a=1&b=2") != base("", "?b=2&a=1") {
			t.Error("query parameter order changes the hash")
		}
		if base("", "?a=1&a=2") == base("", "?a=2&a=1") {
			t.Error("query value order is ignored")
		}
		if base("", "?ab=c") == base("", "?a=bc") {
			t.Error("framing ambiguity between query names and values")
		}
	})

	t.Run("key syntax", func(t *testing.T) {
		for _, k := range []string{"a", "A-b_c.d~e:f", strings.Repeat("k", 128)} {
			if rec := post("/api/v1/tenants/t1/projects/p1/runs", `{}`, "Idempotency-Key", k); rec.Code != 204 {
				t.Errorf("key %q rejected: %d", k, rec.Code)
			}
		}
		for _, k := range []string{"", "has space", "slash/key", strings.Repeat("k", 129), "ключ", "a\tb"} {
			rec := post("/api/v1/tenants/t1/projects/p1/runs", `{}`, "Idempotency-Key", k)
			if rec.Code != 400 {
				t.Errorf("key %q accepted: %d", k, rec.Code)
			}
		}
		if rec := post("/api/v1/tenants/t1/projects/p1/runs", `{}`); rec.Code != 400 {
			t.Errorf("missing key: %d", rec.Code)
		}
		if rec := post("/api/v1/tenants/t1/projects/p1/runs", `{}`, "Idempotency-Key", "a", "Idempotency-Key", "b"); rec.Code != 400 {
			t.Errorf("two keys: %d", rec.Code)
		}
	})

	t.Run("principal-level routes cannot be idempotent", func(t *testing.T) {
		var call *httpapi.Call
		h.handle(httpapi.Route{Method: "POST", Path: "/me/things", Action: "x", Handler: func(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
			call = c
			_, gotErr = c.Identity(r, nil)
			httpapi.WriteError(w, r, gotErr)
		}})
		rec := h.do("POST", "/api/v1/me/things", "", append(bearer("tok-alice"), "Idempotency-Key", "k1")...)
		if call == nil || rec.Code != 500 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})
}

func Test_stored_response_validation(t *testing.T) {
	ok := httpapi.StoredResponse{Status: 201, Header: map[string]string{"ETag": `"1"`, "Location": "/x/1"}, Body: []byte(`{"id":"1"}`)}
	if err := ok.Validate(1024); err != nil {
		t.Fatal(err)
	}
	bad := map[string]httpapi.StoredResponse{
		"status 4xx":      {Status: 400, Body: []byte(`{}`)},
		"status 1xx":      {Status: 100},
		"body too large":  {Status: 200, Body: []byte(`{"a":"` + strings.Repeat("x", 2000) + `"}`)},
		"not json":        {Status: 200, Body: []byte(`hello`)},
		"truncated":       {Status: 200, Body: []byte(`{`)},
		"truncated array": {Status: 200, Body: []byte(`[1,2`)},
		"trailing text":   {Status: 200, Body: []byte(`{} trailing`)},
		"trailing value":  {Status: 200, Body: []byte(`{}[]`)},
		"bad member":      {Status: 200, Body: []byte(`{"a":}`)},
		"whitespace only": {Status: 200, Body: []byte(" \n")},
		"cookie header":   {Status: 200, Header: map[string]string{"Set-Cookie": "a=b"}, Body: []byte(`{}`)},
		"header newline":  {Status: 200, Header: map[string]string{"Location": "/x\r\nSet-Cookie: a=b"}, Body: []byte(`{}`)},
		"authorization":   {Status: 200, Header: map[string]string{"Authorization": "Bearer x"}, Body: []byte(`{}`)},
		"request id kept": {Status: 200, Header: map[string]string{"X-Request-ID": "req_x"}, Body: []byte(`{}`)},
	}
	for name, r := range bad {
		if err := r.Validate(1024); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, body := range map[string]string{"padded object": " \n{\"a\":[1,{}]}\n", "array": `[]`} {
		if err := (httpapi.StoredResponse{Status: 200, Body: []byte(body)}).Validate(1024); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	if err := (httpapi.StoredResponse{Status: 204}).Validate(1024); err != nil {
		t.Errorf("empty 204 rejected: %v", err)
	}
	var api *httpapi.APIError
	if err := httpapi.IdempotencyConflictError(); !errors.As(err, &api) || api.Status != 409 || api.Code != "idempotency_conflict" {
		t.Fatalf("conflict error = %v", err)
	}
}
