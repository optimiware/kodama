package configuration_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

func Test_variable_create_preserves_scalar_types(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantBody string
	}{
		{[]string{"--value", "42"}, `{"name":"limit","value":"42"}`},
		{[]string{"--value-json", "42"}, `{"name":"limit","value":42}`},
		{[]string{"--value-json", "9007199254740991"}, `{"name":"limit","value":9007199254740991}`},
		{[]string{"--value-json", "-9007199254740991"}, `{"name":"limit","value":-9007199254740991}`},
		{[]string{"--value-json", "true"}, `{"name":"limit","value":true}`},
		{[]string{"--value-json", `"true"`}, `{"name":"limit","value":"true"}`},
		{[]string{"--value", "", "--description", "d"}, `{"description":"d","name":"limit","value":""}`},
	} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 201, variableDoc("limit", "string", "x", 1, 1), map[string]string{"ETag": `"1"`, "Idempotent-Replayed": "false"})
		}))
		args := append([]string{"variable", "create", "limit", "--idempotency-key", "var-1", "--output", "json"}, tc.args...)
		r := run(t, srv, strings.NewReader(""), t.TempDir(), args...)
		if r.code != 0 {
			t.Fatalf("%v: exit %d %s", tc.args, r.code, r.err)
		}
		req := srv.Requests()[0]
		if got := strings.TrimSpace(string(req.Body)); got != tc.wantBody {
			t.Errorf("%v: body %s, want %s", tc.args, got, tc.wantBody)
		}
		if req.Path != base+"/variables" || req.Header.Get("Idempotency-Key") != "var-1" || req.Header.Get("If-Match") != "" {
			t.Errorf("request %+v", req)
		}
	}
}

func Test_variable_read_update_delete(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "DELETE":
			w.WriteHeader(204)
		case "PATCH":
			clienttest.WriteJSON(w, 200, variableDoc("limit", "integer", 9007199254740991, 4, 6), nil)
		default:
			if strings.HasSuffix(r.URL.Path, "/variables") {
				clienttest.WriteJSON(w, 200, map[string]any{"items": []any{variableDoc("limit", "integer", 7, 3, 5)}, "nextCursor": "n.c"}, nil)
				return
			}
			clienttest.WriteJSON(w, 200, variableDoc("limit", "integer", 7, 3, 5), nil)
		}
	}))
	d := t.TempDir()
	g := run(t, srv, strings.NewReader(""), d, "variable", "get", "var_1")
	if g.code != 0 || !strings.Contains(g.out, "value version: 3") || !strings.Contains(g.out, "resource version: 5") {
		t.Fatalf("get: exit %d out %q", g.code, g.out)
	}
	u := run(t, srv, strings.NewReader(""), d, "variable", "update", "var_1", "--if-version", "5", "--value-json", "9007199254740991", "--output", "json")
	if u.code != 0 || !strings.Contains(u.out, `"value":9007199254740991`) {
		t.Fatalf("update: exit %d out %q err %q", u.code, u.out, u.err)
	}
	reqs := srv.Requests()
	if reqs[1].Method != "PATCH" || reqs[1].Header.Get("If-Match") != `"5"` || strings.TrimSpace(string(reqs[1].Body)) != `{"value":9007199254740991}` {
		t.Fatalf("update request %+v", reqs[1])
	}
	del := run(t, srv, strings.NewReader(""), d, "variable", "delete", "var_1", "--if-version", "6", "--output", "json")
	if del.code != 0 || strings.TrimSpace(del.out) != `{"deleted":true,"id":"var_1"}` {
		t.Fatalf("delete: exit %d out %q err %q", del.code, del.out, del.err)
	}
	if reqs = srv.Requests(); reqs[2].Method != "DELETE" || reqs[2].Header.Get("If-Match") != `"6"` {
		t.Fatalf("delete request %+v", reqs[2])
	}
	l := run(t, srv, strings.NewReader(""), d, "variable", "list", "--limit", "10", "--cursor", "a.b", "--output", "json")
	if l.code != 0 || !strings.Contains(l.out, `"nextCursor":"n.c"`) {
		t.Fatalf("list: exit %d out %q", l.code, l.out)
	}
	if q := srv.Requests()[3].RawQuery; !strings.Contains(q, "limit=10") || !strings.Contains(q, "cursor=a.b") {
		t.Fatalf("query %q", q)
	}
}

func Test_variable_stale_update_is_reported_not_retried(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 412, "version_conflict", "stale", "req_v1", nil)
	}))
	r := run(t, srv, strings.NewReader(""), t.TempDir(), "variable", "update", "var_1", "--if-version", "1", "--description", "x")
	if r.code != 4 || !strings.Contains(r.err, "req_v1") || !strings.Contains(r.err, "variable get") || len(srv.Requests()) != 1 {
		t.Fatalf("exit %d err %q requests %d", r.code, r.err, len(srv.Requests()))
	}
}

func Test_secret_metadata_commands(t *testing.T) {
	retention := map[string]any{"pinnedVersions": 2, "activePins": 1, "pendingConsumerHandling": 1, "backupRetention": "unresolved"}
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/revoke"):
			clienttest.WriteJSON(w, 200, map[string]any{
				"secret": secretMeta(4), "consumerHandling": map[string]any{"total": 3, "pending": 2},
				"version": map[string]any{"secretId": "sec_1", "version": 2, "algorithm": "a", "keyGeneration": 1, "createdAt": "2026-10-19T10:00:00Z", "createdBy": "u", "revokedAt": "2026-10-19T11:00:00Z"},
			}, nil)
		case r.Method == "DELETE":
			m := secretMeta(5)
			m["deletedAt"] = "2026-10-19T12:00:00Z"
			m["retention"] = retention
			clienttest.WriteJSON(w, 200, map[string]any{"secret": m, "retention": retention}, nil)
		case strings.HasSuffix(p, "/versions"):
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{map[string]any{"secretId": "sec_1", "version": 1, "algorithm": "a", "keyGeneration": 1, "createdAt": "2026-10-19T10:00:00Z", "createdBy": "u", "revokedAt": nil}}, "nextCursor": nil}, nil)
		case strings.HasSuffix(p, "/secrets"):
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{secretMeta(2)}, "nextCursor": nil}, nil)
		default:
			clienttest.WriteJSON(w, 200, secretMeta(2), nil)
		}
	}))
	d := t.TempDir()
	in := strings.NewReader("")

	rv := run(t, srv, in, d, "secret", "revoke", "sec_1", "--version", "2", "--reason", "leaked in CI log")
	if rv.code != 0 || !strings.Contains(rv.out, "cannot recall") || !strings.Contains(rv.out, "2 of 3") {
		t.Fatalf("revoke: exit %d out %q err %q", rv.code, rv.out, rv.err)
	}
	reqs := srv.Requests()
	if reqs[0].Path != base+"/secrets/sec_1/versions/2/revoke" || reqs[0].Header.Get("If-Match") != "" || strings.TrimSpace(string(reqs[0].Body)) != `{"reason":"leaked in CI log"}` {
		t.Fatalf("revoke request %+v", reqs[0])
	}
	run(t, srv, in, d, "secret", "revoke", "sec_1", "--version", "2")
	if reqs = srv.Requests(); len(reqs[1].Body) != 0 {
		t.Fatalf("revoke without a reason must be bodyless: %q", reqs[1].Body)
	}

	del := run(t, srv, in, d, "secret", "delete", "sec_1", "--if-version", "4")
	if del.code != 0 || !strings.Contains(del.out, "unresolved") || !strings.Contains(del.out, "purges nothing") || srv.Requests()[2].Header.Get("If-Match") != `"4"` {
		t.Fatalf("delete: exit %d out %q", del.code, del.out)
	}
	if g := run(t, srv, in, d, "secret", "get", "sec_1", "--output", "json"); g.code != 0 || strings.Contains(g.out, "\"value\"") {
		t.Fatalf("get: exit %d out %q", g.code, g.out)
	}
	if v := run(t, srv, in, d, "secret", "versions", "sec_1"); v.code != 0 || !strings.Contains(v.out, "v1") {
		t.Fatalf("versions: exit %d out %q", v.code, v.out)
	}
	if l := run(t, srv, in, d, "secret", "list"); l.code != 0 || !strings.Contains(l.out, "erp-read-token") {
		t.Fatalf("list: exit %d out %q", l.code, l.out)
	}
}

func Test_scope_cursor_output_contract(t *testing.T) {
	for _, tc := range []struct{ status, exit int }{{401, 3}, {403, 3}, {404, 1}, {409, 4}, {412, 4}, {428, 2}, {429, 1}, {503, 1}} {
		srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteError(w, tc.status, "denied", "m", "req_c", map[string]string{"Retry-After": "9"})
		}))
		r := run(t, srv, strings.NewReader(""), t.TempDir(), "variable", "delete", "var_1", "--if-version", "2", "--output", "json")
		if int(r.code) != tc.exit || r.out != "" || !strings.Contains(r.err, `"requestId":"req_c"`) {
			t.Errorf("%d: exit %d out %q err %q", tc.status, r.code, r.out, r.err)
		}
		if (tc.status == 429 || tc.status == 503) && !strings.Contains(r.err, `"retryAfterSeconds":9`) {
			t.Errorf("%d: retry guidance lost", tc.status)
		}
	}
	// A foreign scope is addressed exactly as written; the server decides.
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 404, "not_found", "m", "req_f", nil)
	}))
	run(t, srv, strings.NewReader(""), t.TempDir(), "variable", "get", "var_1", "--tenant", "t_other", "--project", "p_other")
	if got := srv.Requests()[0].Path; got != "/api/v1/tenants/t_other/projects/p_other/variables/var_1" {
		t.Fatalf("path %s", got)
	}
}
