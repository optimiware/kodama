package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	maxCodeLen      = 4096
	maxErrorCodeLen = 64
)

// callbackResult is the single accepted outcome of a loopback callback.
type callbackResult struct {
	Code string
	// Denied is the sanitized provider error code when the provider answered
	// with an error instead of a code.
	Denied string
}

// loopback is the bounded one-shot HTTP listener that receives the OAuth
// redirect. It binds only a literal loopback address. Exactly one callback
// carrying the attempt's state is accepted; mismatched or malformed requests
// are rejected without consuming the attempt, and repeats after acceptance are
// refused. A callback is never turned into a token exchange here.
type loopback struct {
	ln       net.Listener
	srv      *http.Server
	path     string
	state    string
	hostPort string
	deadline time.Time
	now      func() time.Time

	mu       sync.Mutex
	consumed bool
	result   chan callbackResult
	done     chan struct{}
}

// parseRedirect validates a loopback redirect URI and returns its address.
func parseRedirect(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, errors.New("the redirect URI must be an http loopback URL")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("the redirect URI host must be a literal loopback address such as 127.0.0.1 or [::1]")
	}
	if u.Port() == "" {
		return nil, errors.New("the redirect URI must name a port")
	}
	if p, err := strconv.Atoi(u.Port()); err != nil || p < 0 || p > 65535 {
		return nil, errors.New("the redirect URI port is invalid")
	}
	if u.Path == "" || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.ForceQuery {
		return nil, errors.New("the redirect URI must have a callback path and no query, fragment or credentials")
	}
	return u, nil
}

// newLoopback binds the redirect address and starts serving. The returned
// redirect URL carries the actual port (relevant only for port 0).
func newLoopback(redirect *url.URL, state string, deadline time.Time, now func() time.Time) (*loopback, string, error) {
	ln, err := net.Listen("tcp", redirect.Host)
	if err != nil {
		return nil, "", fmt.Errorf("listen on the loopback redirect address: %w", err)
	}
	actual := *redirect
	actual.Host = net.JoinHostPort(redirect.Hostname(), strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	l := &loopback{
		ln: ln, path: redirect.Path, state: state, hostPort: actual.Host, deadline: deadline, now: now,
		result: make(chan callbackResult, 1), done: make(chan struct{}),
	}
	l.srv = &http.Server{
		Handler:           http.HandlerFunc(l.handle),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		defer close(l.done)
		_ = l.srv.Serve(ln)
	}()
	return l, actual.String(), nil
}

func reject(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, msg)
}

func (l *loopback) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != l.path {
		reject(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		reject(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.Host != l.hostPort { // defeats DNS rebinding to the loopback listener
		reject(w, http.StatusBadRequest, "unexpected host")
		return
	}
	if !l.deadline.IsZero() && !l.now().Before(l.deadline) {
		reject(w, http.StatusGone, "this login attempt has expired")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		reject(w, http.StatusBadRequest, "malformed callback")
		return
	}
	states := q["state"]
	if len(states) != 1 || subtle.ConstantTimeCompare([]byte(states[0]), []byte(l.state)) != 1 {
		reject(w, http.StatusBadRequest, "invalid callback")
		return
	}
	codes, errs := q["code"], q["error"]
	var res callbackResult
	switch {
	case len(codes) == 1 && len(errs) == 0 && codes[0] != "" && len(codes[0]) <= maxCodeLen:
		res.Code = codes[0]
	case len(errs) == 1 && len(codes) == 0 && errs[0] != "":
		res.Denied = sanitizeErrorCode(errs[0])
	default:
		reject(w, http.StatusBadRequest, "malformed callback")
		return
	}
	l.mu.Lock()
	if l.consumed {
		l.mu.Unlock()
		reject(w, http.StatusConflict, "this login attempt was already used")
		return
	}
	l.consumed = true
	l.result <- res // buffered; written at most once
	l.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "You can close this window and return to the terminal.")
}

// sanitizeErrorCode keeps only a short RFC 6749 style token so provider text
// cannot inject control characters or long content into CLI output.
func sanitizeErrorCode(s string) string {
	if len(s) == 0 || len(s) > maxErrorCodeLen {
		return "invalid_error"
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.'
		if !ok {
			return "invalid_error"
		}
	}
	return s
}

// Close stops the listener and waits for the server goroutine. It is safe to
// call more than once.
func (l *loopback) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.srv.Shutdown(ctx); err != nil {
		_ = l.srv.Close()
	}
	<-l.done
}
