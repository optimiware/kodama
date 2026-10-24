package agent_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// TestProfileCommands: scoped, authenticated list queries; exact registration
// payload; 201 and 200 handling; no version or idempotency headers.
func TestProfileCommands(t *testing.T) {
	t.Run("list one page with filters", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{profileJSON(2), profileJSON(1)}, "nextCursor": "abc.def"}, nil)
		}))
		r := c.run("agent", "list", "--name", "coding", "--limit", "25", "--cursor", "opaque.cursor", "--output", "json")
		if r.code != 0 || r.err != "" {
			t.Fatalf("exit %d err %q", r.code, r.err)
		}
		reqs := c.srv.Requests()
		if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != profiles {
			t.Fatalf("requests %+v", reqs)
		}
		if reqs[0].RawQuery != "cursor=opaque.cursor&limit=25&name=coding" {
			t.Fatalf("query %q", reqs[0].RawQuery)
		}
		if reqs[0].Header.Get("Authorization") != "Bearer tok-1" || len(reqs[0].Body) != 0 {
			t.Fatalf("request %+v", reqs[0])
		}
		out := decodeJSON(t, r.out)
		if out["nextCursor"] != "abc.def" || len(out["items"].([]any)) != 2 {
			t.Fatalf("output %v", out)
		}
		h := c.run("agent", "list")
		if h.code != 0 || !strings.Contains(h.out, "coding@2") || !strings.Contains(h.out, "next page: --cursor abc.def") {
			t.Fatalf("human output %q", h.out)
		}
	})

	t.Run("list last page and empty", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, map[string]any{"items": []any{}, "nextCursor": nil}, nil)
		}))
		r := c.run("agent", "list")
		if r.code != 0 || !strings.Contains(r.out, "no agent profiles") || !strings.Contains(r.out, "no further pages") {
			t.Fatalf("exit %d out %q", r.code, r.out)
		}
	})

	t.Run("list rejects bad flags before any request", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request") }))
		for _, args := range [][]string{
			{"agent", "list", "--limit", "0"}, {"agent", "list", "--limit", "x"}, {"agent", "list", "--cursor", ""},
			{"agent", "list", "--name", " "}, {"agent", "list", "extra"},
		} {
			if r := c.run(args...); r.code != 2 || r.out != "" {
				t.Errorf("%v: exit %d out %q", args, r.code, r.out)
			}
		}
	})

	for _, tc := range []struct {
		status  int
		version int
		want    string
	}{{201, 1, "registered a new profile version"}, {200, 1, "an identical profile was already registered"}} {
		t.Run("register "+http.StatusText(tc.status), func(t *testing.T) {
			c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteJSON(w, tc.status, profileJSON(tc.version), map[string]string{"ETag": `"1"`, "X-Request-ID": "req_1"})
			}))
			file := writeFile(t, "profile.json", string(fixture(t)))
			r := c.run("agent", "register", "--file", file, "--output", "json")
			if r.code != 0 || r.err != "" {
				t.Fatalf("exit %d err %q", r.code, r.err)
			}
			reqs := c.srv.Requests()
			if len(reqs) != 1 || reqs[0].Method != "POST" || reqs[0].Path != profiles {
				t.Fatalf("requests %+v", reqs)
			}
			for _, h := range []string{"If-Match", "Idempotency-Key"} {
				if v := reqs[0].Header.Get(h); v != "" {
					t.Fatalf("%s = %q: registration is content-addressed", h, v)
				}
			}
			var sent, want any
			if err := json.Unmarshal(reqs[0].Body, &sent); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(fixture(t), &want)
			if !reflect.DeepEqual(sent, want) {
				t.Fatalf("payload differs from the file:\n%s", reqs[0].Body)
			}
			if out := decodeJSON(t, r.out); out["reference"] != "coding@1" || out["digest"] == nil {
				t.Fatalf("output %v", out)
			}
			h := c.run("agent", "register", "--file", file)
			for _, want := range []string{tc.want, "coding@1", "version: 1", "digest: sha256:2222", "model: example-model", "tools: [\"repo.read\",\"repo.search\"]"} {
				if !strings.Contains(h.out, want) {
					t.Errorf("human output lacks %q: %s", want, h.out)
				}
			}
		})
	}
}

func TestProfileRegisterRequiresFile(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request") }))
	for _, args := range [][]string{{"agent", "register"}, {"agent", "register", "--file", ""}, {"agent", "register", "--file", "x", "extra"}} {
		if r := c.run(args...); r.code != 2 || r.out != "" {
			t.Errorf("%v: exit %d out %q", args, r.code, r.out)
		}
	}
}

// TestCommandErrors: remote failures keep code, request ID, details and exit
// class; stdout and stderr stay separate; registration guidance never claims an
// idempotency key.
func TestCommandErrors(t *testing.T) {
	file := func(t *testing.T) string { return writeFile(t, "profile.json", string(fixture(t))) }
	cases := []struct {
		name   string
		status int
		code   string
		exit   int
		reg    bool
	}{
		{"unauthenticated", 401, "unauthenticated", 3, true},
		{"forbidden", 403, "forbidden", 3, true},
		{"hidden project", 404, "not_found", 1, true},
		{"invalid body", 400, "invalid_request", 1, true},
		{"too large", 413, "payload_too_large", 1, true},
		{"invalid profile", 422, "invalid_profile", 1, true},
		{"unsupported profile", 422, "profile_unsupported", 1, true},
		{"profile conflict", 409, "profile_conflict", 4, true},
		{"throttled", 429, "rate_limited", 1, true},
		{"unavailable", 503, "unavailable", 1, true},
		{"list forbidden", 403, "forbidden", 3, false},
		{"list unavailable", 503, "unavailable", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteJSON(w, tc.status, map[string]any{"error": map[string]any{
					"code": tc.code, "message": "refused", "requestId": "req_err_1",
					"details": map[string]any{"diagnostics": []any{map[string]any{"code": "unapproved_tool", "path": "tools[1]"}}},
				}}, map[string]string{"Retry-After": "2"})
			}))
			args := []string{"agent", "list", "--output", "json"}
			if tc.reg {
				args = []string{"agent", "register", "--file", file(t), "--output", "json"}
			}
			r := c.run(args...)
			if int(r.code) != tc.exit || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			env := decodeJSON(t, r.err)["error"].(map[string]any)
			if env["code"] != tc.code || env["requestId"] != "req_err_1" {
				t.Fatalf("envelope %v", env)
			}
			if !strings.Contains(r.err, "unapproved_tool") {
				t.Fatalf("server diagnostics were dropped: %s", r.err)
			}
			if strings.Contains(r.err, "idempotency") {
				t.Fatalf("profile registration has no idempotency key: %s", r.err)
			}
			if n := len(c.srv.Requests()); n != 1 {
				t.Fatalf("%d requests: the CLI never retries", n)
			}
		})
	}
}

func TestCommandErrorsUncertainRegistration(t *testing.T) {
	t.Run("lost answer", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		r := c.run("agent", "register", "--file", writeFile(t, "p.json", string(fixture(t))), "--output", "json")
		if r.code != 1 || r.out != "" {
			t.Fatalf("exit %d out %q", r.code, r.out)
		}
		d := decodeJSON(t, r.err)["error"].(map[string]any)["details"].(map[string]any)
		if d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "agent list") || strings.Contains(d["recovery"].(string), "idempotency") {
			t.Fatalf("details %v", d)
		}
		if len(c.srv.Requests()) != 1 {
			t.Fatal("must not retry")
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":`))
		}))
		r := c.run("agent", "register", "--file", writeFile(t, "p.json", string(fixture(t))))
		if r.code == 0 || r.out != "" || !strings.Contains(r.err, "agent list") {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer close(release)
		r := c.run("agent", "register", "--file", writeFile(t, "p.json", string(fixture(t))), "--timeout", "200ms", "--output", "json")
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("malformed list", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, []any{}, nil)
		}))
		if r := c.run("agent", "list"); r.code != 1 || r.out != "" || !strings.Contains(r.err, "malformed_response") {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
}
