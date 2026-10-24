package gate_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// TestGateCommandValidation: a decision missing any explicit input, or with a
// malformed one, is a usage failure before any HTTP call.
func TestGateCommandValidation(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	long := strings.Repeat("r", 2049)
	cases := map[string][]string{
		"missing decision":    without(decideArgs(), "decision"),
		"missing digest":      without(decideArgs(), "subject-digest"),
		"missing reason":      without(decideArgs(), "reason"),
		"missing version":     without(decideArgs(), "if-version"),
		"missing key":         without(decideArgs(), "idempotency-key"),
		"unknown decision":    withFlag(decideArgs(), "decision", "maybe"),
		"empty decision":      withFlag(decideArgs(), "decision", ""),
		"blank reason":        withFlag(decideArgs(), "reason", "   "),
		"reason too long":     withFlag(decideArgs(), "reason", long),
		"short digest":        withFlag(decideArgs(), "subject-digest", "sha256:abc"),
		"uppercase digest":    withFlag(decideArgs(), "subject-digest", "sha256:"+strings.Repeat("A", 64)),
		"zero version":        withFlag(decideArgs(), "if-version", "0"),
		"negative version":    withFlag(decideArgs(), "if-version", "-1"),
		"fractional version":  withFlag(decideArgs(), "if-version", "1.5"),
		"huge version":        withFlag(decideArgs(), "if-version", "9007199254740992"),
		"bad key":             withFlag(decideArgs(), "idempotency-key", "has space"),
		"long key":            withFlag(decideArgs(), "idempotency-key", strings.Repeat("k", 129)),
		"missing gate":        {"gate", "decide", "--decision", "approve"},
		"extra argument":      append(decideArgs(), "extra"),
		"get without id":      {"gate", "get"},
		"get extra arguments": {"gate", "get", gateID, "another"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if len(args) < 2 || args[0] != "gate" {
				t.Fatal("bad case")
			}
			if r := c.run(args...); r.code != 2 || r.out != "" || r.err == "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
		})
	}
	if n := len(c.srv.Requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func TestGateCommandValidRequests(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteJSON(w, 200, gateJSON(decision+"d", 4), map[string]string{"ETag": `"4"`})
			}))
			args := withFlag(decideArgs("--output", "json"), "decision", decision)
			args = withFlag(args, "if-version", "3")
			args = withFlag(args, "reason", "Tést — \"quoted\" <reason>")
			if r := c.run(args...); r.code != 0 || r.err != "" {
				t.Fatalf("exit %d err %q", r.code, r.err)
			}
			req := c.srv.Requests()[0]
			if req.Method != "POST" || req.Path != decides || req.Header.Get("If-Match") != `"3"` || req.Header.Get("Idempotency-Key") != "decision-01" {
				t.Fatalf("request %+v", req)
			}
			b := decodeJSON(t, string(req.Body))
			if len(b) != 3 || b["decision"] != decision || b["subjectDigest"] != digest || b["reason"] != "Tést — \"quoted\" <reason>" {
				t.Fatalf("the body must hold exactly the explicit inputs: %v", b)
			}
			for _, forbidden := range []string{"actor", "subject", "decidedBy", "version"} {
				if _, ok := b[forbidden]; ok {
					t.Fatalf("body carries server-owned %q", forbidden)
				}
			}
			if len(c.srv.Requests()) != 1 {
				t.Fatalf("decision must not prefetch the gate: %d requests", len(c.srv.Requests()))
			}
		})
	}
}

// TestCommandErrors: remote failures keep their code, request ID and exit
// class, with stdout and stderr separated and no success output.
func TestCommandErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		exit   int
		args   []string
		hdr    map[string]string
	}{
		{"unauthenticated", 401, "unauthenticated", 3, decideArgs(), nil},
		{"forbidden", 403, "forbidden", 3, decideArgs(), nil},
		{"hidden gate", 404, "not_found", 1, decideArgs(), nil},
		{"inspect hidden gate", 404, "not_found", 1, []string{"gate", "get", gateID}, nil},
		{"inspect forbidden", 403, "forbidden", 3, []string{"gate", "get", gateID}, nil},
		{"already decided", 422, "invalid_transition", 1, decideArgs(), nil},
		{"conformance denial", 422, "conformance_blocked", 1, decideArgs(), nil},
		{"version required", 428, "version_required", 2, decideArgs(), nil},
		{"throttled", 429, "rate_limited", 1, decideArgs(), map[string]string{"Retry-After": "7"}},
		{"unavailable", 503, "unavailable", 1, decideArgs(), map[string]string{"Retry-After": "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteError(w, tc.status, tc.code, "refused", "req_err_1", tc.hdr)
			}))
			r := c.run(append(append([]string{}, tc.args...), "--output", "json")...)
			if int(r.code) != tc.exit || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			env := decodeJSON(t, r.err)["error"].(map[string]any)
			if env["code"] != tc.code || env["requestId"] != "req_err_1" {
				t.Fatalf("envelope %v", env)
			}
			if n := len(c.srv.Requests()); n != 1 {
				t.Fatalf("%d requests", n)
			}
		})
	}
}

func TestCommandErrorsMalformedAndTimeout(t *testing.T) {
	t.Run("malformed decision response is uncertain", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":`))
		}))
		r := c.run(decideArgs("--output", "json")...)
		if r.code == 0 || r.out != "" {
			t.Fatalf("exit %d out %q", r.code, r.out)
		}
		details := decodeJSON(t, r.err)["error"].(map[string]any)["details"].(map[string]any)
		if details["outcomeUncertain"] != true || !strings.Contains(details["recovery"].(string), "gate get") {
			t.Fatalf("details %v", details)
		}
		if len(c.srv.Requests()) != 1 {
			t.Fatal("must not retry")
		}
	})
	t.Run("gateway page for a decision is uncertain", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<html>bad gateway</html>", 502)
		}))
		r := c.run(decideArgs("--output", "json")...)
		if r.code != 1 || r.out != "" || !strings.Contains(r.err, "identical command") {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
	t.Run("timeout is exit 5 with decision guidance", func(t *testing.T) {
		release := make(chan struct{})
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer close(release)
		r := c.run(decideArgs("--timeout", "200ms", "--output", "json")...)
		if r.code != 5 || r.out != "" {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
		details := decodeJSON(t, r.err)["error"].(map[string]any)["details"].(map[string]any)
		if details["outcomeUncertain"] != true || !strings.Contains(details["recovery"].(string), "--idempotency-key") {
			t.Fatalf("details %v", details)
		}
		if len(c.srv.Requests()) != 1 {
			t.Fatal("must not retry")
		}
	})
	t.Run("malformed gate read", func(t *testing.T) {
		c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clienttest.WriteJSON(w, 200, []any{1}, nil)
		}))
		if r := c.run("gate", "get", gateID); r.code != 1 || r.out != "" || !strings.Contains(r.err, "malformed_response") {
			t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
		}
	})
}

func TestGateRequiresProjectScope(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request")
	}))
	// Overriding with an empty project leaves routing unselected.
	r := c.run("gate", "get", gateID, "--project", "")
	if r.code == 0 {
		t.Fatalf("expected failure: out %q", r.out)
	}
	if len(c.srv.Requests()) != 0 {
		t.Fatal("request sent without a project")
	}
}
