package logshttp_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/logs/logstest"
)

type failingIdentity struct{ err error }

func (f failingIdentity) Authorize(context.Context, identity.Actor, identity.Target, identity.Permission) error {
	return f.err
}

type failingAuthority struct{ err error }

func (f failingAuthority) AuthorizeRead(context.Context, string, domain.Scope) error { return f.err }

// TestAuthorizationAndBackendFailures: revoked membership denies reconnects and
// ends a follow; authority and provider failures disclose neither content nor
// credentials; mid-stream failures are explicit and close every resource.
// Real PostgreSQL and identity; controlled HTTP failures for Kubernetes.
func TestAuthorizationAndBackendFailures(t *testing.T) {
	e := logstest.New(t)
	a := e.NewAttempt(logstest.ScopeA, true)
	q := "attemptId=" + string(a.ID)

	t.Run("a revoked membership denies reconnects", func(t *testing.T) {
		e.Grant(logstest.ScopeA, "u023_temp", identity.RoleViewer)
		r := newRig(t, a, e, rigOptions{})
		if res := r.read("u023_temp", a.Scope, a.Run, q); res.status != http.StatusOK {
			t.Fatalf("a member reads: %d %s", res.status, trunc(res.raw))
		}
		e.Revoke(logstest.ScopeA, "u023_temp")
		before := len(r.kube.Requests())
		res := r.read("u023_temp", a.Scope, a.Run, q)
		if res.status != http.StatusNotFound || strings.Contains(res.raw, "line") {
			t.Fatalf("after revocation: %d %s", res.status, trunc(res.raw))
		}
		if len(r.kube.Requests()) != before {
			t.Fatal("a revoked caller reached Kubernetes")
		}
	})

	t.Run("a revocation ends a long follow", func(t *testing.T) {
		e.Grant(logstest.ScopeA, "u023_follow", identity.RoleViewer)
		lim := logstest.Limits()
		lim.ReauthInterval = time.Second
		lim.MaxDuration = 30 * time.Second
		r := newRig(t, a, e, rigOptions{limits: lim})
		gone := make(chan struct{})
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, req *http.Request) {
				defer close(gone)
				for req.Context().Err() == nil {
					fmt.Fprint(w, "tick\n")
					w.(http.Flusher).Flush()
					time.Sleep(100 * time.Millisecond)
				}
			}
		})
		resp := r.get("u023_follow", a.Scope, a.Run, q+"&follow=true")
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		if !sc.Scan() { // the meta record: the stream is open
			t.Fatalf("no first record: %v", sc.Err())
		}
		e.Revoke(logstest.ScopeA, "u023_follow")
		type outcome struct {
			last string
			n    int
		}
		done := make(chan outcome, 1)
		go func() {
			var o outcome
			for sc.Scan() {
				o.last = sc.Text()
				o.n++
			}
			done <- o
		}()
		select {
		case o := <-done:
			if !strings.Contains(o.last, `"type":"summary"`) || !strings.Contains(o.last, `"stop":"authorization_changed"`) {
				t.Fatalf("last record = %s", o.last)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a revoked caller kept streaming")
		}
		waitClosed(t, gone, "the upstream request")
	})

	t.Run("an authority outage is unavailable, never content", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{auth: failingAuthority{errors.New("pq: password authentication failed for user kodama_runtime host=10.1.2.3")}})
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		for _, leak := range []string{"password", "kodama_runtime", "10.1.2.3", "line"} {
			if strings.Contains(res.raw, leak) {
				t.Fatalf("the response leaks %q: %s", leak, res.raw)
			}
		}
		if len(r.kube.Requests()) != 0 {
			t.Fatal("an unavailable authority must not be treated as access")
		}
		// A refusal by the module's authority is a 403, an unbound principal 404.
		r2 := newRig(t, a, e, rigOptions{auth: failingAuthority{logs.ErrDenied}})
		if res := r2.read(logstest.Engineer, a.Scope, a.Run, q); res.status != http.StatusForbidden {
			t.Fatalf("%d", res.status)
		}
	})

	t.Run("an identity authority outage at the route denies", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{identity: failingIdentity{errors.New("pq: connection refused 10.5.5.5")}})
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if res.status != http.StatusServiceUnavailable || strings.Contains(res.raw, "10.5.5.5") || len(r.kube.Requests()) != 0 {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
	})

	t.Run("provider failures disclose nothing", func(t *testing.T) {
		for name, setup := range map[string]func(r *rig){
			"kubelet error": func(r *rig) { r.kube.Set(func(k *kubefakeAPI) { k.LogCode = http.StatusInternalServerError }) },
			"forbidden by the API": func(r *rig) {
				r.kube.Set(func(k *kubefakeAPI) {
					k.LogHandler = func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "token=abc", http.StatusForbidden) }
				})
			},
			"job recreated under a new uid": func(r *rig) {
				r.kube.Set(func(k *kubefakeAPI) {
					k.Job["metadata"].(map[string]any)["uid"] = "99999999-2222-3333-4444-555555555555"
				})
			},
			"oversized object": func(r *rig) { r.kube.Set(func(k *kubefakeAPI) { k.JobBytes = 8 << 20 }) },
		} {
			t.Run(name, func(t *testing.T) {
				r := newRig(t, a, e, rigOptions{})
				setup(r)
				res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&source=live")
				if res.status != http.StatusServiceUnavailable {
					t.Fatalf("%d %s", res.status, trunc(res.raw))
				}
				for _, leak := range []string{"token=abc", "credentials", r.kube.URL(), "uid", "one\ntwo"} {
					if strings.Contains(res.raw, leak) {
						t.Fatalf("the response leaks %q: %s", leak, res.raw)
					}
				}
			})
		}
		t.Run("an unreachable API", func(t *testing.T) {
			r := newRig(t, a, e, rigOptions{})
			base := r.kube.URL()
			dead := httptest.NewServer(http.NotFoundHandler())
			url := dead.URL
			dead.Close()
			_ = base
			r2 := newRigWithEndpoint(t, a, e, url)
			res := r2.read(logstest.Engineer, a.Scope, a.Run, q+"&source=live")
			if res.status != http.StatusServiceUnavailable || strings.Contains(res.raw, url) || strings.Contains(res.raw, "connection refused") {
				t.Fatalf("%d %s", res.status, trunc(res.raw))
			}
		})
	})

	t.Run("a missing workload falls back to the retained log, or is a plain 404", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{})
		r.kube.Set(func(k *kubefakeAPI) { k.Job = nil })
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		if res.status != http.StatusNotFound || strings.Contains(res.raw, "secret detail") {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		e.WrapperLog(a, "kept by the wrapper\n", contract.Complete)
		res = r.read(logstest.Engineer, a.Scope, a.Run, q)
		if res.status != http.StatusOK || res.meta(t)["origin"] != "wrapper" || res.lines()[0] != "kept by the wrapper" {
			t.Fatalf("a retained log serves when the workload is gone: %d %s", res.status, trunc(res.raw))
		}
		if live := r.read(logstest.Engineer, a.Scope, a.Run, q+"&source=live"); live.status != http.StatusNotFound {
			t.Fatalf("an explicit live read of a missing workload: %d", live.status)
		}
	})

	t.Run("an unobserved Job has no live log and no similarly named workload is read", func(t *testing.T) {
		pending := e.NewAttempt(logstest.ScopeA, false)
		r := newRig(t, pending, e, rigOptions{})
		res := r.read(logstest.Engineer, pending.Scope, pending.Run, "attemptId="+string(pending.ID))
		if res.status != http.StatusNotFound {
			t.Fatalf("%d %s", res.status, trunc(res.raw))
		}
		if len(r.kube.Requests()) != 0 {
			t.Fatalf("an unobserved Job must not be looked up by name: %v", r.kube.Requests())
		}
	})

	t.Run("a mid-stream failure is explicit and closes the stream", func(t *testing.T) {
		r := newRig(t, a, e, rigOptions{})
		r.kube.Set(func(k *kubefakeAPI) {
			k.LogHandler = func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, "partial line one\npartial line two\n")
				w.(http.Flusher).Flush()
				// Cut the connection without finishing the chunked response.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					if tcp, ok := conn.(*net.TCPConn); ok {
						_ = tcp.SetLinger(0)
					}
					conn.Close()
				}
			}
		})
		res := r.read(logstest.Engineer, a.Scope, a.Run, q)
		s := res.summary(t)
		if s["stop"] != "interrupted" || s["truncated"] != false {
			t.Fatalf("summary = %v", s)
		}
		if got := res.lines(); len(got) < 1 || got[0] != "partial line one" {
			t.Fatalf("lines before the failure are kept: %v", got)
		}
		if strings.Contains(res.raw, "reset") || strings.Contains(res.raw, "EOF") || strings.Contains(res.raw, "127.0.0.1") {
			t.Fatalf("raw provider text in the stream: %s", res.raw)
		}
	})

	t.Run("failures release every resource", func(t *testing.T) {
		live := &logstest.FakeLive{Body: strings.Repeat("abcdefghij\n", 50), FailAt: 100, FailErr: errors.New("connection reset by peer 10.0.0.7")}
		r := newRig(t, a, e, rigOptions{live: live, limits: func() logs.Limits { l := logstest.Limits(); l.MaxStreams = 1; return l }()})
		for i := 0; i < 5; i++ {
			res := r.read(logstest.Engineer, a.Scope, a.Run, q)
			if res.status != http.StatusOK || res.summary(t)["stop"] != "interrupted" || strings.Contains(res.raw, "10.0.0.7") {
				t.Fatalf("round %d: %d %s", i, res.status, trunc(res.raw))
			}
		}
		if live.Opened() != 5 || live.Closed() != 5 {
			t.Fatalf("opened %d closed %d: every upstream must be closed", live.Opened(), live.Closed())
		}
		// The single stream slot was released each time.
		live.FailAt = 0
		if res := r.read(logstest.Engineer, a.Scope, a.Run, q); res.status != http.StatusOK {
			t.Fatalf("slot leaked: %d", res.status)
		}
	})

	t.Run("an open failure also releases the slot", func(t *testing.T) {
		live := &logstest.FakeLive{Err: errors.New("dial tcp 10.0.0.9:6443: i/o timeout")}
		r := newRig(t, a, e, rigOptions{live: live, limits: func() logs.Limits { l := logstest.Limits(); l.MaxStreams = 1; return l }()})
		for i := 0; i < 3; i++ {
			res := r.read(logstest.Engineer, a.Scope, a.Run, q+"&source=live")
			if res.status != http.StatusServiceUnavailable || strings.Contains(res.raw, "10.0.0.9") {
				t.Fatalf("round %d: %d %s", i, res.status, trunc(res.raw))
			}
		}
	})
}
