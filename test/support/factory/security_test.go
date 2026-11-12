package factory

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// TestHarnessTokensAreFreshAndStrong proves the harness credentials are
// generated per call, independent, and carry at least 128 bits of entropy. It
// also proves a generated value never equals a retired published literal.
func TestHarnessTokensAreFreshAndStrong(t *testing.T) {
	first := newHarnessToken(t)
	second := newHarnessToken(t)
	if first == second {
		t.Fatalf("two token generations returned the same value")
	}
	for _, token := range []string{first, second} {
		material, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			t.Fatalf("token %q is not unpadded base64url: %v", token, err)
		}
		if len(material) < 16 {
			t.Fatalf("token carries %d bytes (%d bits); want at least 16 bytes (128 bits)",
				len(material), len(material)*8)
		}
		for _, retired := range retiredFixtureTokens {
			if token == retired {
				t.Fatalf("generated token equals the retired published literal %q", retired)
			}
		}
	}

	admin, project := newHarnessCredentials(t)
	if admin == project {
		t.Fatalf("generated admin and project credentials are identical")
	}
}

// TestFactorySourcesNeverBindWildcard is the static guard against a wildcard
// bind: no non-test source in this package may name an unspecified wildcard or
// join an empty host, so a listener can only ever bind a specific address. The
// constructor also refuses such a host at runtime; this check catches a literal
// that would bypass it.
func TestFactorySourcesNeverBindWildcard(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	forbidden := []struct {
		literal string
		reason  string
	}{
		{"0.0.0.0", "names the IPv4 unspecified wildcard"},
		{`net.Listen("tcp", ":`, "binds an empty host, which is the wildcard"},
		{`JoinHostPort("",`, "joins an empty host, which is the wildcard"},
		{`"::"`, "names the IPv6 unspecified wildcard"},
		{`"[::]"`, "names the IPv6 unspecified wildcard"},
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, rule := range forbidden {
			if strings.Contains(string(source), rule.literal) {
				t.Fatalf("%s %s; the factory edge must bind one host address", name, rule.reason)
			}
		}
	}
}

// TestWorkloadBindAddressIsSpecific proves the bind host is one concrete
// interface address, that a listener bound to it reports that exact address,
// and that it is neither the wildcard nor loopback (pasta does not forward the
// host alias to host loopback; see workloadListenHost).
//
// The probe needs an IPv4 default route. A host without one cannot serve the
// workload path at all, so the default build-tag-free unit baseline skips this
// test honestly instead of failing; KODAMA_REQUIRE_INFRA=1 turns that skip into
// a failure, exactly like the shared infrastructure policy.
func TestWorkloadBindAddressIsSpecific(t *testing.T) {
	host, err := defaultRouteHost()
	if err != nil {
		if errors.Is(err, errNoDefaultRoute) {
			harness.SkipOrFail(t, os.Getenv("KODAMA_REQUIRE_INFRA") == "1",
				fmt.Errorf("workload bind address probe: %w", err))
		}
		t.Fatalf("factory: %v", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("bind host %q is not an IP address", host)
	}
	if ip.IsUnspecified() {
		t.Fatalf("bind host %q is the unspecified wildcard", host)
	}
	if ip.IsLoopback() {
		t.Fatalf("bind host %q is loopback, which the pasta host alias does not reach", host)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	defer func() { _ = listener.Close() }()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(ip) {
		t.Fatalf("listener address %v is not the bound host %q", listener.Addr(), host)
	}
}

// TestHostOnlyListenerRejectsForeignPeers proves the peer filter accepts only
// the bound host address, so a remote network peer is refused before the HTTP
// server sees the connection.
func TestHostOnlyListenerRejectsForeignPeers(t *testing.T) {
	host := net.ParseIP("192.0.2.10")
	if !peerAllowed(&net.TCPAddr{IP: host, Port: 1234}, host) {
		t.Fatalf("host peer was refused")
	}
	if peerAllowed(&net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 1234}, host) {
		t.Fatalf("foreign peer was accepted")
	}
	if peerAllowed(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}, host) {
		t.Fatalf("loopback peer was accepted")
	}
	if peerAllowed(nil, host) || peerAllowed(&net.TCPAddr{IP: host, Port: 1}, nil) {
		t.Fatalf("an absent peer or host was accepted")
	}
}

// TestEdgeForwarderRelaysHostTraffic proves the workload-facing edge relays a
// host-sourced connection byte-for-byte to the loopback-bound composition API
// and shuts down without leaving a relay goroutine behind.
//
// The edge listens on loopback and the client therefore connects from
// loopback, so this test needs no default route and is not gated on the host
// network configuration. ensureLoopback only raises a DOWN loopback interface
// (a fresh network namespace) and never changes a route.
func TestEdgeForwarderRelaysHostTraffic(t *testing.T) {
	ensureLoopback(t)

	// Pin the scheduler to one P for the goroutine-dump checks below. With a
	// single P the test goroutine cannot be descheduled between Close returning
	// and the snapshot, so a forwarder goroutine that Close failed to join is
	// still on the run queue and therefore still in the dump. Without the pin
	// the unjoined goroutines could exit on another P before the snapshot and
	// mask the defect.
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	// Close must join a relay that is still blocked. A real net.Conn.Close
	// unblocks the relay's copies, so an unjoined relay can finish before a
	// post-Close snapshot and hide the defect; the gated connection used here
	// keeps the relay parked until Close has had to join it.
	assertEdgeForwarderCloseJoins(t)

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		connection, err := target.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		request := make([]byte, 4)
		if _, err := io.ReadFull(connection, request); err != nil {
			return
		}
		_, _ = connection.Write([]byte("pong"))
	}()

	edge, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen edge: %v", err)
	}
	forwarder := newEdgeForwarder(hostOnlyListener{Listener: edge, host: net.ParseIP("127.0.0.1")}, target.Addr().String())
	defer forwarder.Close()

	connection, err := net.Dial("tcp", edge.Addr().String())
	if err != nil {
		t.Fatalf("dial edge: %v", err)
	}
	defer func() { _ = connection.Close() }()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(response) != "pong" {
		t.Fatalf("response = %q; want pong", response)
	}

	// Close must still bound the real relay and close the client connection.
	// The deterministic join proof is assertEdgeForwarderCloseJoins above.
	closed := make(chan struct{})
	go func() {
		forwarder.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("forwarder Close did not join the active relay")
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatalf("client connection still delivered bytes after forwarder Close")
	}
}

// forwarderAcceptFrame and forwarderRelayFrame are the exact method-frame
// symbols that appear in a goroutine dump while the forwarder's goroutines are
// alive. The trailing "(" excludes the "created by ...accept in goroutine" /
// "created by ...relay in goroutine" lines, so a surviving relay does not
// masquerade as a surviving accept goroutine.
const (
	forwarderAcceptFrame = "(*edgeForwarder).accept("
	forwarderRelayFrame  = "(*edgeForwarder).relay("
)

// forwarderGoroutineDump snapshots every goroutine stack, growing the buffer
// until the runtime reports that the whole dump fit.
func forwarderGoroutineDump() string {
	size := 64 << 10
	for {
		buffer := make([]byte, size)
		written := runtime.Stack(buffer, true)
		if written < len(buffer) {
			return string(buffer[:written])
		}
		size *= 2
	}
}

// joinReleaseDelay is the gated connection's read deadline: it is how long the
// fake read stays blocked while Close joins the relay. It is a fake-connection
// deadline, not a settling delay for the assertion. The post-Close snapshot
// runs immediately after Close returns; a Close that omits the join fails while
// the relay is still parked on this deadline, so the check does not depend on
// the delay elapsing.
const joinReleaseDelay = 250 * time.Millisecond

// assertEdgeForwarderCloseJoins proves Close joins both forwarder goroutines.
//
// The accepted connection is a gate that Close cannot unblock, so a Close that
// returns without waiting on the WaitGroup leaves the relay goroutine parked
// and visible in the goroutine dump. A real connection cannot make this check
// deterministic: net.Conn.Close synchronizes with a blocked read, so an
// unjoined relay can finish before the snapshot is taken.
//
// The positive control snapshots the dump while the relay is open and requires
// the accept and relay frames to be present. Without it a rename of the methods
// would silently turn the post-Close assertion into a no-op.
func assertEdgeForwarderCloseJoins(t *testing.T) {
	t.Helper()

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen join target: %v", err)
	}
	defer func() { _ = target.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := target.Accept()
		if err != nil {
			return
		}
		accepted <- connection
	}()

	incoming := newGatedConn()
	forwarder := newEdgeForwarder(newGatedListener(incoming), target.Addr().String())
	defer forwarder.Close()

	select {
	case <-incoming.readStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("relay did not start reading the accepted connection")
	}
	select {
	case connection := <-accepted:
		defer func() { _ = connection.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatalf("relay did not dial the join target")
	}

	dump := forwarderGoroutineDump()
	var missing []string
	for _, frame := range []string{forwarderRelayFrame, forwarderAcceptFrame} {
		if !strings.Contains(dump, frame) {
			missing = append(missing, frame)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("open forwarder goroutine dump does not contain %v; the post-Close assertion would be vacuous:\n%s", missing, dump)
	}

	closed := make(chan struct{})
	go func() {
		forwarder.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("forwarder Close did not join the blocked relay")
	}

	dump = forwarderGoroutineDump()
	var remaining []string
	for _, frame := range []string{forwarderRelayFrame, forwarderAcceptFrame} {
		if strings.Contains(dump, frame) {
			remaining = append(remaining, frame)
		}
	}
	if len(remaining) > 0 {
		t.Fatalf("forwarder goroutines %v still running after Close returned:\n%s", remaining, dump)
	}
}

// gatedListener serves one gated connection and then blocks until Close.
type gatedListener struct {
	conn      net.Conn
	mu        sync.Mutex
	served    bool
	closeOnce sync.Once
	closed    chan struct{}
}

func newGatedListener(conn net.Conn) *gatedListener {
	return &gatedListener{conn: conn, closed: make(chan struct{})}
}

func (listener *gatedListener) Accept() (net.Conn, error) {
	listener.mu.Lock()
	if !listener.served {
		listener.served = true
		connection := listener.conn
		listener.mu.Unlock()
		return connection, nil
	}
	listener.mu.Unlock()
	<-listener.closed
	return nil, net.ErrClosed
}

func (listener *gatedListener) Close() error {
	listener.closeOnce.Do(func() { close(listener.closed) })
	return nil
}

func (listener *gatedListener) Addr() net.Addr { return gatedAddr{} }

// gatedConn is an accepted connection whose read Close cannot unblock. Read
// signals when the relay has reached its copy loop, then stays blocked for
// joinReleaseDelay so the post-Close dump can observe a relay that Close failed
// to join.
type gatedConn struct {
	readStarted chan struct{}
	startOnce   sync.Once
}

func newGatedConn() *gatedConn {
	return &gatedConn{readStarted: make(chan struct{})}
}

func (connection *gatedConn) Read(buffer []byte) (int, error) {
	connection.startOnce.Do(func() { close(connection.readStarted) })
	<-time.After(joinReleaseDelay)
	return 0, io.EOF
}

func (connection *gatedConn) Write(buffer []byte) (int, error) { return len(buffer), nil }

// Close deliberately does not unblock Read: the relay must stay parked so a
// Close that omits the join cannot finish it before the snapshot.
func (connection *gatedConn) Close() error { return nil }

func (connection *gatedConn) LocalAddr() net.Addr  { return gatedAddr{} }
func (connection *gatedConn) RemoteAddr() net.Addr { return gatedAddr{} }

func (connection *gatedConn) SetDeadline(time.Time) error      { return nil }
func (connection *gatedConn) SetReadDeadline(time.Time) error  { return nil }
func (connection *gatedConn) SetWriteDeadline(time.Time) error { return nil }

type gatedAddr struct{}

func (gatedAddr) Network() string { return "gated" }
func (gatedAddr) String() string  { return "gated" }

// TestHostOnlyListenerRejectsMismatchedHostConnection is the negative control
// for the relay test: the peer filter is what makes relaying safe, so a peer
// whose source address is not the configured host must be closed before any
// byte reaches the target.
//
// The target listener is deliberately never accepted. If the filter accepted
// every peer, the relay would dial the target, copy the request into the
// kernel backlog and then wait for a response, so the client read would time
// out instead of observing a closed connection. A throwaway mutation that
// makes peerAllowed return true therefore fails this control.
func TestHostOnlyListenerRejectsMismatchedHostConnection(t *testing.T) {
	ensureLoopback(t)

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer func() { _ = target.Close() }()

	edge, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen edge: %v", err)
	}
	// The listener is on loopback but the filter expects a different host, so
	// the loopback client is a foreign peer.
	forwarder := newEdgeForwarder(hostOnlyListener{Listener: edge, host: net.ParseIP("192.0.2.10")}, target.Addr().String())
	defer forwarder.Close()

	connection, err := net.Dial("tcp", edge.Addr().String())
	if err != nil {
		t.Fatalf("dial edge: %v", err)
	}
	defer func() { _ = connection.Close() }()
	// A rejected peer may see the connection close before or during the write;
	// the invariant under test is the read outcome, not the write.
	_, _ = connection.Write([]byte("ping"))

	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, 4)
	count, err := connection.Read(buffer)
	if err == nil {
		t.Fatalf("mismatched-host connection was relayed: read %q", buffer[:count])
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("mismatched-host connection was not closed; the read timed out")
	}
}

// TestOutputMaterializationsSeesMaterialization is the positive control for
// Harness.OutputMaterializations: the helper must report a directory laid out
// exactly like the adapter's <workloadRoot>/<workload>/kodama/outputs/<action>.
// Without it an empty result in the integration suite would be indistinguishable
// from a helper that can never see anything.
func TestOutputMaterializationsSeesMaterialization(t *testing.T) {
	root := t.TempDir()
	harnessUnderTest := &Harness{T: t, runtimeRoot: root}

	found, err := harnessUnderTest.OutputMaterializations("produce")
	if err != nil {
		t.Fatalf("inspect empty runtime root: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("empty runtime root reported materializations %v; want none", found)
	}

	materialized := filepath.Join(root, "workload-one", "kodama", "outputs", "produce")
	if err := os.MkdirAll(materialized, 0o755); err != nil {
		t.Fatalf("create materialization: %v", err)
	}
	found, err = harnessUnderTest.OutputMaterializations("produce")
	if err != nil {
		t.Fatalf("inspect materialized output: %v", err)
	}
	if len(found) != 1 || found[0] != materialized {
		t.Fatalf("materializations = %v; want [%s]", found, materialized)
	}

	// A different action is not reported by this action's query.
	found, err = harnessUnderTest.OutputMaterializations("consume")
	if err != nil {
		t.Fatalf("inspect unrelated action: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("unrelated action reported materializations %v; want none", found)
	}

	// A missing runtime root is an empty result, not an error.
	absent := &Harness{T: t, runtimeRoot: filepath.Join(root, "absent")}
	found, err = absent.OutputMaterializations("produce")
	if err != nil {
		t.Fatalf("inspect absent runtime root: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("absent runtime root reported materializations %v; want none", found)
	}
}
