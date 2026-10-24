package run_test

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
)

// keyedFactory is a minimal keyed run-creation endpoint: it stores the first
// body per key, replays it for an identical body, conflicts for a different one
// and refuses a revoked principal even for a stored key (AC-02).
type keyedFactory struct {
	mu      sync.Mutex
	stored  map[string][]byte
	created int
	lose    int  // answers to drop (connection closed after applying)
	revoked bool // the caller's run:manage permission is gone
}

func (k *keyedFactory) handler(t *testing.T) http.Handler {
	k.stored = map[string][]byte{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/runs") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		if k.revoked {
			clienttest.WriteError(w, 403, "forbidden", "run:manage was revoked", "req_403", nil)
			return
		}
		prev, seen := k.stored[key]
		switch {
		case seen && !bytes.Equal(prev, body.Bytes()):
			clienttest.WriteError(w, 409, "idempotency_conflict", "the key was used with a different request", "req_409", nil)
			return
		case !seen:
			k.stored[key] = body.Bytes()
			k.created++
		}
		if k.lose > 0 {
			k.lose--
			c, _, _ := w.(http.Hijacker).Hijack()
			_ = c.Close()
			return
		}
		hdr := map[string]string{}
		if seen {
			hdr["Idempotent-Replayed"] = "true"
		}
		clienttest.WriteJSON(w, 201, runDoc("queued", 1), hdr)
	})
}

// Test_mutation_recovery: a lost answer is never resent by the CLI; the manual
// identical keyed replay carries the same body and key and converges on one
// run; changed content under the key conflicts; a revoked permission still
// fails a replay; and cancellation stays a request.
func Test_mutation_recovery(t *testing.T) {
	k := &keyedFactory{lose: 1}
	srv := clienttest.NewServer(t, k.handler(t))

	first := runCLI(t, srv, startArgs("--input", "region=eu", "--output", "json")...)
	if first.code != 1 || first.out != "" {
		t.Fatalf("lost answer: exit %d out %q err %q", first.code, first.out, first.err)
	}
	e := errorOf(t, first.err)
	d, _ := e["details"].(map[string]any)
	if d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "identical --idempotency-key") {
		t.Fatalf("details %v", d)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d requests: a lost answer must not be resent automatically", n)
	}

	// Manual replay: identical command, identical key and body.
	second := runCLI(t, srv, startArgs("--input", "region=eu", "--output", "json")...)
	if second.code != 0 {
		t.Fatalf("replay: exit %d err %q", second.code, second.err)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 || !bytes.Equal(reqs[0].Body, reqs[1].Body) || reqs[0].Header.Get("Idempotency-Key") != reqs[1].Header.Get("Idempotency-Key") {
		t.Fatalf("the replay must repeat the exact body and key: %+v", reqs)
	}
	if k.created != 1 {
		t.Fatalf("%d runs created", k.created)
	}

	// Changed content under the same key conflicts and is not retried.
	third := runCLI(t, srv, startArgs("--input", "region=us", "--output", "json")...)
	if third.code != 4 || errorOf(t, third.err)["code"] != "idempotency_conflict" || len(srv.Requests()) != 3 {
		t.Fatalf("changed body: exit %d err %q", third.code, third.err)
	}
	if !strings.Contains(third.err, "new key creates another run") {
		t.Fatalf("no key guidance: %s", third.err)
	}

	// A revoked permission fails the replay of a stored key.
	k.mu.Lock()
	k.revoked = true
	k.mu.Unlock()
	fourth := runCLI(t, srv, startArgs("--input", "region=eu", "--output", "json")...)
	if fourth.code != 3 || fourth.out != "" || errorOf(t, fourth.err)["code"] != "forbidden" {
		t.Fatalf("revoked: exit %d out %q err %q", fourth.code, fourth.out, fourth.err)
	}
	if k.created != 1 {
		t.Fatalf("%d runs created", k.created)
	}
}

func Test_mutation_recovery_cancel_lost_response_is_not_resent(t *testing.T) {
	var posts atomic.Int32
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	}))
	r := runCLI(t, srv, "run", "cancel", runID, "--output", "json")
	if r.code != 1 || r.out != "" || posts.Load() != 1 {
		t.Fatalf("exit %d out %q posts %d", r.code, r.out, posts.Load())
	}
	d := errorOf(t, r.err)["details"].(map[string]any)
	if d["outcomeUncertain"] != true || !strings.Contains(d["recovery"].(string), "run get") || strings.Contains(d["recovery"].(string), "idempotency-key") {
		t.Fatalf("details %v", d)
	}
}

func Test_mutation_recovery_deadline_keeps_uncertainty(t *testing.T) {
	block := make(chan struct{})
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer close(block)
	r := runCLI(t, srv, append(startArgs("--output", "json"), "--timeout", "200ms")...)
	if r.code != 5 || r.out != "" {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if d := errorOf(t, r.err)["details"].(map[string]any); d["outcomeUncertain"] != true {
		t.Fatalf("a timed-out mutation is still uncertain: %v", d)
	}
}
