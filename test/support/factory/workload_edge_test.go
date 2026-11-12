package factory

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestNewWorkloadEdgeRefusesUnusableHosts proves the edge constructor fails
// closed before any listener exists for every host that is not one concrete
// interface address. An empty host is the net.JoinHostPort("", "0") wildcard
// bind; an unspecified address is the same wildcard spelled out. "localhost" is
// a name, not an address, and net.Listen would happily resolve it to loopback,
// so the constructor must reject it before that resolution can make the
// host-only peer filter compare against a nil host.
func TestNewWorkloadEdgeRefusesUnusableHosts(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "not-an-ip", "localhost"} {
		t.Run("host="+strconv.Quote(host), func(t *testing.T) {
			forwarder, port, err := newWorkloadEdge(host, "127.0.0.1:1")
			if err == nil {
				if forwarder != nil {
					forwarder.Close()
				}
				t.Fatalf("newWorkloadEdge(%q) succeeded with port %d; want an error", host, port)
			}
			if forwarder != nil {
				forwarder.Close()
				t.Fatalf("newWorkloadEdge(%q) returned a forwarder alongside error %v", host, err)
			}
			if port != 0 {
				t.Fatalf("newWorkloadEdge(%q) returned port %d; want 0 on error", host, port)
			}
		})
	}
}

// TestNewWorkloadEdgePropagatesBindFailure proves a well-formed but non-local
// host still fails closed. 192.0.2.0/24 is TEST-NET-1, reserved for
// documentation and not assigned to an interface, so net.Listen reports
// EADDRNOTAVAIL; the constructor must return that error instead of an unusable
// forwarder or a zero port. Without this the bind-error branch could be removed
// and every valid-host test would still pass.
func TestNewWorkloadEdgePropagatesBindFailure(t *testing.T) {
	forwarder, port, err := newWorkloadEdge("192.0.2.10", "127.0.0.1:1")
	if err == nil {
		if forwarder != nil {
			forwarder.Close()
		}
		t.Fatalf("newWorkloadEdge on an unassigned address succeeded with port %d; want a bind error", port)
	}
	if forwarder != nil {
		forwarder.Close()
		t.Fatalf("newWorkloadEdge returned a forwarder alongside bind error %v", err)
	}
	if port != 0 {
		t.Fatalf("newWorkloadEdge returned port %d on bind error; want 0", port)
	}
}

// TestWorkloadEdgeRelaysOnlyBoundHostPeer proves the workload edge built by
// newWorkloadEdge relays a connection sourced from the bound host byte for byte
// and closes a connection sourced from any other loopback address before the
// target sees it. 127.0.0.2 is still on the loopback interface, so this test
// needs no default route; ensureLoopback only raises a DOWN loopback interface
// in a fresh network namespace.
//
// The target is the loopback composition stand-in. The positive exchange is the
// control: without it an edge that refused every peer would pass the negative
// half. A throwaway mutation that removes the hostOnlyListener wrapper relays
// the 127.0.0.2 peer and fails both the read outcome and the target-accept
// assertion.
func TestWorkloadEdgeRelaysOnlyBoundHostPeer(t *testing.T) {
	ensureLoopback(t)

	payload := []byte("edge-byte-for-byte")
	target, accepted := edgeEchoTarget(t, payload)

	forwarder, port, err := newWorkloadEdge("127.0.0.1", target.Addr().String())
	if err != nil {
		t.Fatalf("newWorkloadEdge: %v", err)
	}
	defer forwarder.Close()

	address, ok := forwarder.listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.ParseIP("127.0.0.1")) || address.Port != port {
		t.Fatalf("edge listener address = %v (port %d); want 127.0.0.1:%d", forwarder.listener.Addr(), port, port)
	}

	hostPeer := dialEdge(t, "127.0.0.1", "127.0.0.1", port)
	assertEdgeEcho(t, hostPeer, payload)
	drainAccepted(t, accepted)

	foreignPeer := dialEdge(t, "127.0.0.2", "127.0.0.1", port)
	assertEdgeRefusesPeer(t, foreignPeer, payload, accepted)
}

// TestWorkloadEdgeBindsRequestedHost proves newWorkloadEdge binds the host it
// was given instead of a hardcoded loopback address, and that the peer filter
// compares against that same host. Binding 127.0.0.2 and accepting only
// 127.0.0.2 sources fails if the constructor ignores listenHost, if it binds a
// different address, or if the wrapper's host disagrees with the bound address.
func TestWorkloadEdgeBindsRequestedHost(t *testing.T) {
	ensureLoopback(t)

	payload := []byte("requested-host-edge")
	target, accepted := edgeEchoTarget(t, payload)

	forwarder, port, err := newWorkloadEdge("127.0.0.2", target.Addr().String())
	if err != nil {
		t.Fatalf("newWorkloadEdge: %v", err)
	}
	defer forwarder.Close()

	address, ok := forwarder.listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.ParseIP("127.0.0.2")) || address.Port != port {
		t.Fatalf("edge listener address = %v (port %d); want 127.0.0.2:%d", forwarder.listener.Addr(), port, port)
	}

	hostPeer := dialEdge(t, "127.0.0.2", "127.0.0.2", port)
	assertEdgeEcho(t, hostPeer, payload)
	drainAccepted(t, accepted)

	foreignPeer := dialEdge(t, "127.0.0.1", "127.0.0.2", port)
	assertEdgeRefusesPeer(t, foreignPeer, payload, accepted)
}

// edgeEchoTarget starts a loopback target that reports every accepted
// connection and echoes exactly len(payload) bytes back before closing it.
func edgeEchoTarget(t *testing.T, payload []byte) (net.Listener, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen edge target: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- connection
			go func(connection net.Conn) {
				defer func() { _ = connection.Close() }()
				request := make([]byte, len(payload))
				if _, err := io.ReadFull(connection, request); err != nil {
					return
				}
				_, _ = connection.Write(request)
			}(connection)
		}
	}()
	return listener, accepted
}

// dialEdge dials the edge from one explicit loopback source address. The source
// is what the host-only filter inspects, so it must be selectable.
func dialEdge(t *testing.T, localHost, remoteHost string, port int) net.Conn {
	t.Helper()
	dialer := net.Dialer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(localHost)},
		Timeout:   5 * time.Second,
	}
	connection, err := dialer.Dial("tcp", net.JoinHostPort(remoteHost, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial edge from %s to %s:%d: %v", localHost, remoteHost, port, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// assertEdgeEcho requires the connection to relay payload unchanged.
func assertEdgeEcho(t *testing.T, connection net.Conn, payload []byte) {
	t.Helper()
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write edge request: %v", err)
	}
	response := make([]byte, len(payload))
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("read edge response: %v", err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatalf("edge response = %q; want %q", response, payload)
	}
}

// drainAccepted waits for the positive exchange's target connection and closes
// it, so the following negative assertion observes only new connections.
func drainAccepted(t *testing.T, accepted <-chan net.Conn) {
	t.Helper()
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(5 * time.Second):
		t.Fatalf("host peer was not relayed to the target")
	}
}

// assertEdgeRefusesPeer requires the edge to close a non-host peer before any
// byte reaches the target. A relayed peer would leave the client waiting for a
// response (timeout) and would show up on the target's accept channel.
func assertEdgeRefusesPeer(t *testing.T, connection net.Conn, payload []byte, accepted <-chan net.Conn) {
	t.Helper()
	// A rejected peer may see the connection close before or during the write;
	// the invariant under test is the read outcome, not the write.
	_, _ = connection.Write(payload)

	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	buffer := make([]byte, len(payload))
	count, err := connection.Read(buffer)
	if err == nil {
		t.Fatalf("foreign peer was relayed: read %q", buffer[:count])
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("foreign peer was not closed; the read timed out")
	}

	select {
	case relayed := <-accepted:
		_ = relayed.Close()
		t.Fatalf("foreign peer reached the loopback target")
	case <-time.After(time.Second):
	}
}
