package mcphttp

import (
	"net/http"
	"testing"
)

// WrapClientTransportForTest wraps the private client transport New already
// installed with observe. External tests use it to observe the request the
// driver hands the HTTP stack, the response-body closure and headers injected
// just before the real transport runs. Production New refuses every
// caller-supplied non-*http.Transport, so this test-only seam cannot be reached
// through Config and the trace-based dispatch evidence cannot be bypassed by an
// opaque RoundTripper.
func WrapClientTransportForTest(t *testing.T, transport *Transport, observe func(http.RoundTripper) http.RoundTripper) {
	t.Helper()
	if transport == nil || transport.client == nil {
		t.Fatal("test instrumentation received a nil transport")
	}
	base, ok := transport.client.Transport.(*http.Transport)
	if !ok || base == nil {
		t.Fatalf("private client transport = %T; want *http.Transport", transport.client.Transport)
	}
	transport.client.Transport = observe(base)
}

// CloseIdleConnectionsForTest closes the idle connections of the driver's
// private transport. New clones a caller-supplied transport, so the caller's
// template pool is not the pool the driver used and tests must clean the clone.
func CloseIdleConnectionsForTest(t *testing.T, transport *Transport) {
	t.Helper()
	if transport == nil || transport.client == nil {
		t.Fatal("test instrumentation received a nil transport")
	}
	base, ok := transport.client.Transport.(*http.Transport)
	if !ok || base == nil {
		t.Fatalf("private client transport = %T; want *http.Transport", transport.client.Transport)
	}
	base.CloseIdleConnections()
}
