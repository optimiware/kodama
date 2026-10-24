package gate_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// TestStaleGateCLI: a stale subject or version is a conflict (exit 4) that
// keeps the server's code, request ID and details, prints nothing on stdout,
// and is never refreshed or retried by the CLI.
func TestStaleGateCLI(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		details map[string]any
	}{
		{"content changed", 409, "stale_gate_subject", map[string]any{"reason": "content_changed"}},
		{"source candidate changed", 409, "stale_gate_subject", map[string]any{"reason": "candidate_changed"}},
		{"configuration changed", 409, "stale_gate_subject", map[string]any{"reason": "deployment_changed"}},
		{"evidence changed", 409, "stale_gate_subject", map[string]any{"reason": "evidence_changed"}},
		{"expired", 409, "stale_gate_subject", map[string]any{"reason": "expired"}},
		{"invalidated", 409, "stale_gate_subject", map[string]any{"reason": "invalidated"}},
		{"digest mismatch", 409, "stale_gate_subject", map[string]any{"reason": "subject_digest_mismatch"}},
		{"stale version", 412, "version_conflict", map[string]any{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				clienttest.WriteJSON(w, tc.status, map[string]any{"error": map[string]any{
					"code": tc.code, "message": "the gate cannot be decided", "requestId": "req_stale_1", "details": tc.details,
				}}, map[string]string{"X-Request-ID": "req_stale_1"})
			}))
			r := c.run(decideArgs("--output", "json")...)
			if r.code != 4 || r.out != "" {
				t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
			}
			env := decodeJSON(t, r.err)["error"].(map[string]any)
			if env["code"] != tc.code || env["requestId"] != "req_stale_1" {
				t.Fatalf("envelope %v", env)
			}
			details := env["details"].(map[string]any)
			if want, ok := tc.details["reason"]; ok && details["reason"] != want {
				t.Fatalf("details lost the server's reason: %v", details)
			}
			if rec, _ := details["recovery"].(string); !strings.Contains(rec, "gate get") {
				t.Fatalf("no inspect-again guidance: %v", details)
			}
			if n := len(c.srv.Requests()); n != 1 {
				t.Fatalf("the CLI sent %d requests: stale decisions are never refreshed or retried", n)
			}
			h := c.run(decideArgs()...)
			if h.code != 4 || h.out != "" || !strings.Contains(h.err, "req_stale_1") || !strings.Contains(h.err, "inspect the gate again") {
				t.Fatalf("human failure exit %d out %q err %q", h.code, h.out, h.err)
			}
			for _, req := range c.srv.Requests() {
				if req.Method != "POST" {
					t.Fatalf("a %s request was sent: nothing may refresh the gate", req.Method)
				}
			}
		})
	}
}

// TestStaleGateCLIDoesNotRewriteInputs: the stale answer is not used to pick a
// new version or digest.
func TestStaleGateCLIDoesNotRewriteInputs(t *testing.T) {
	c := newCLI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clienttest.WriteError(w, 412, "version_conflict", "stale", "req_1", map[string]string{"ETag": `"9"`})
	}))
	for i := 0; i < 2; i++ {
		if r := c.run(decideArgs()...); r.code != 4 {
			t.Fatalf("exit %d", r.code)
		}
	}
	reqs := c.srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, req := range reqs {
		if req.Header.Get("If-Match") != `"1"` || req.Header.Get("Idempotency-Key") != "decision-01" {
			t.Fatalf("headers %v", req.Header)
		}
	}
}
