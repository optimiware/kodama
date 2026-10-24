package chatopstest

import (
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
)

// Request is one request a receiver observed.
type Request struct {
	N      int
	Header http.Header
	Body   []byte
	Path   string
}

// Behavior answers the nth request (1-based). It runs after the request was
// recorded, so a behavior that withholds the response (LoseResponse, Hang) still
// leaves the receiver having "processed" the notification, exactly the
// uncertainty a lost acknowledgment creates.
type Behavior func(n int, w http.ResponseWriter, r *http.Request)

// Receiver is an isolated local HTTPS receiver. It records every request and
// counts one *effect* per distinct Idempotency-Key, which is what a
// deduplicating receiver would apply; Requests counts every arrival.
type Receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	behavior Behavior
	reqs     []Request
	effects  map[string]int
}

// NewReceiver starts a TLS receiver (closed on cleanup). A nil behavior accepts.
func NewReceiver(t testing.TB, b Behavior) *Receiver {
	t.Helper()
	rc := &Receiver{behavior: b, effects: map[string]int{}}
	rc.srv = httptest.NewTLSServer(http.HandlerFunc(rc.serve))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *Receiver) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, Request{N: len(rc.reqs) + 1, Header: r.Header.Clone(), Body: body, Path: r.URL.Path})
	n := len(rc.reqs)
	if key := r.Header.Get("Idempotency-Key"); key != "" && rc.effects[key] == 0 {
		rc.effects[key] = 1
	}
	b := rc.behavior
	rc.mu.Unlock()
	if b == nil {
		Accept("rcpt-1")(n, w, r)
		return
	}
	b(n, w, r)
}

// SetBehavior replaces the behavior (the outage test flips it).
func (rc *Receiver) SetBehavior(b Behavior) {
	rc.mu.Lock()
	rc.behavior = b
	rc.mu.Unlock()
}

// URL is the hook endpoint.
func (rc *Receiver) URL() string { return rc.srv.URL + "/hook" }

// AddrPort is the exact listening address.
func (rc *Receiver) AddrPort() netip.AddrPort {
	return netip.MustParseAddrPort(rc.srv.Listener.Addr().String())
}

// Pool trusts the receiver's certificate.
func (rc *Receiver) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(rc.srv.Certificate())
	return pool
}

// Requests returns a copy of every request seen.
func (rc *Receiver) Requests() []Request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]Request(nil), rc.reqs...)
}

// Count is the number of requests seen.
func (rc *Receiver) Count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.reqs)
}

// Effects returns the number of distinct idempotency keys seen: the effects a
// deduplicating receiver applied.
func (rc *Receiver) Effects() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.effects)
}

// EffectOf reports whether the key was applied.
func (rc *Receiver) EffectOf(key string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.effects[key] > 0
}

// Accept answers 200 with an optional receipt header.
func Accept(receipt string) Behavior {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		if receipt != "" {
			w.Header().Set("X-Receipt-Id", receipt)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// Status answers the status with a short body.
func Status(code int) Behavior {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte("receiver diagnostic that must never be stored"))
	}
}

// LoseResponse closes the connection after the request was recorded, without
// answering: acceptance followed by a lost response.
func LoseResponse() Behavior {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	}
}

// Hang never answers until the request context ends.
func Hang() Behavior {
	return func(_ int, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
}

// Then answers the first n requests with first and every later one with rest.
func Then(n int, first, rest Behavior) Behavior {
	return func(i int, w http.ResponseWriter, r *http.Request) {
		if i <= n {
			first(i, w, r)
			return
		}
		rest(i, w, r)
	}
}
