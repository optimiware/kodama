package gate_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// replayServer models the public decision contract at the HTTP boundary only:
// it stores the first response per Idempotency-Key and body. It is not a claim
// of database replay qualification, which belongs to the gates module.
type replayServer struct {
	mu       sync.Mutex
	stored   map[string][]byte // key -> body of the first request
	revoked  bool
	dropNext bool // lose the answer of the next fresh decision
}

func (s *replayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		clienttest.WriteError(w, 403, "forbidden", "the caller may not decide this gate", "req_revoked", nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(r.Body)
	if first, used := s.stored[key]; used {
		if !bytes.Equal(first, body.Bytes()) {
			clienttest.WriteError(w, 409, "idempotency_conflict", "the key was used with a different request", "req_conflict", nil)
			return
		}
		clienttest.WriteJSON(w, 200, gateJSON("approved", 2), map[string]string{"ETag": `"2"`, "Idempotent-Replayed": "true", "X-Request-ID": "req_replay"})
		return
	}
	s.stored[key] = append([]byte(nil), body.Bytes()...)
	if s.dropNext {
		s.dropNext = false
		conn, _, err := w.(http.Hijacker).Hijack() // decision recorded, answer lost
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	clienttest.WriteJSON(w, 200, gateJSON("approved", 2), map[string]string{"ETag": `"2"`, "X-Request-ID": "req_first"})
}

// TestDecisionRetry: after a lost response the same command is rerun by the
// caller with the same scope, route, body bytes, version and key; the CLI sends
// one POST per invocation and reports the stored result as a replay.
func TestDecisionRetry(t *testing.T) {
	srv := &replayServer{stored: map[string][]byte{}, dropNext: true}
	c := newCLI(t, srv)

	lost := c.run(decideArgs("--output", "json")...)
	if lost.code != 1 || lost.out != "" {
		t.Fatalf("lost response: exit %d out %q err %q", lost.code, lost.out, lost.err)
	}
	env := decodeJSON(t, lost.err)["error"].(map[string]any)
	details := env["details"].(map[string]any)
	if details["outcomeUncertain"] != true {
		t.Fatalf("an unknown outcome must be flagged: %v", env)
	}
	rec := details["recovery"].(string)
	for _, want := range []string{"gate get", "identical command", "--idempotency-key", "never use a different key"} {
		if !strings.Contains(rec, want) {
			t.Fatalf("guidance %q lacks %q", rec, want)
		}
	}
	if n := len(c.srv.Requests()); n != 1 {
		t.Fatalf("the CLI sent %d requests for one invocation: a mutation is never retried automatically", n)
	}

	retry := c.run(decideArgs("--output", "json")...)
	if retry.code != 0 || retry.err != "" {
		t.Fatalf("retry: exit %d err %q", retry.code, retry.err)
	}
	g := decodeJSON(t, retry.out)
	if g["status"] != "approved" || g["version"].(interface{ String() string }).String() != "2" {
		t.Fatalf("stored result %v", g)
	}
	human := c.run(decideArgs()...)
	if human.code != 0 || !strings.Contains(human.out, "replayed") {
		t.Fatalf("replay not visible: %q", human.out)
	}

	reqs := c.srv.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want exactly one POST per invocation", len(reqs))
	}
	for i, req := range reqs {
		if req.Method != "POST" || req.Path != decides {
			t.Fatalf("request %d: %s %s", i, req.Method, req.Path)
		}
		if req.Header.Get("If-Match") != `"1"` || req.Header.Get("Idempotency-Key") != "decision-01" ||
			req.Header.Get("Authorization") != "Bearer tok-1" {
			t.Fatalf("request %d headers: %v", i, req.Header)
		}
		if !bytes.Equal(req.Body, reqs[0].Body) {
			t.Fatalf("request %d body changed: %s vs %s", i, req.Body, reqs[0].Body)
		}
	}
	if got := string(reqs[0].Body); got != `{"decision":"approve","subjectDigest":"`+digest+`","reason":"Reviewed the identified source and test evidence."}` {
		t.Fatalf("body %s", got)
	}
}

func TestDecisionRetryChangedBodyIsConflict(t *testing.T) {
	srv := &replayServer{stored: map[string][]byte{}}
	c := newCLI(t, srv)
	if r := c.run(decideArgs()...); r.code != 0 {
		t.Fatalf("first: exit %d %s", r.code, r.err)
	}
	r := c.run(withFlag(decideArgs("--output", "json"), "decision", "reject")...)
	if r.code != 4 || r.out != "" {
		t.Fatalf("exit %d out %q", r.code, r.out)
	}
	env := decodeJSON(t, r.err)["error"].(map[string]any)
	if env["code"] != "idempotency_conflict" || env["requestId"] != "req_conflict" {
		t.Fatalf("envelope %v", env)
	}
	if !strings.Contains(env["details"].(map[string]any)["recovery"].(string), "idempotency key was already used") {
		t.Fatalf("no key guidance: %v", env)
	}
}

func TestDecisionRetryRevokedReviewer(t *testing.T) {
	srv := &replayServer{stored: map[string][]byte{}}
	c := newCLI(t, srv)
	if r := c.run(decideArgs()...); r.code != 0 {
		t.Fatalf("first: exit %d %s", r.code, r.err)
	}
	srv.revoked = true
	r := c.run(decideArgs("--output", "json")...)
	if r.code != 3 || r.out != "" {
		t.Fatalf("a revoked reviewer cannot replay: exit %d out %q", r.code, r.out)
	}
	if env := decodeJSON(t, r.err)["error"].(map[string]any); env["code"] != "forbidden" || env["requestId"] != "req_revoked" {
		t.Fatalf("envelope %v", env)
	}
}
