package factory

import (
	"io"
	"net"
	"sync"
	"time"
)

// relayDialTimeout bounds the dial to the loopback composition target. The
// target is local, so a healthy dial is immediate; the bound exists so Close
// can always join a relay goroutine even if the target listener is unreachable.
const relayDialTimeout = 5 * time.Second

// edgeForwarder relays the workload-facing edge to the loopback-bound
// composition API. The composition itself never listens on a routable
// interface: the only listener on the host's routable address is this
// forwarder, it accepts only host-sourced peers (hostOnlyListener) and it
// forwards the byte stream unchanged, preserving HTTP half-close semantics.
type edgeForwarder struct {
	listener net.Listener
	target   string

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// newEdgeForwarder starts relaying accepted connections to target. The caller
// owns listener and must Close the forwarder to join every goroutine.
func newEdgeForwarder(listener net.Listener, target string) *edgeForwarder {
	forwarder := &edgeForwarder{
		listener: listener,
		target:   target,
		conns:    make(map[net.Conn]struct{}),
	}
	forwarder.wg.Add(1)
	go forwarder.accept()
	return forwarder
}

// Close stops accepting, closes every relayed connection and joins the
// forwarder goroutines. It is idempotent.
func (forwarder *edgeForwarder) Close() {
	forwarder.mu.Lock()
	if forwarder.closed {
		forwarder.mu.Unlock()
		return
	}
	forwarder.closed = true
	connections := make([]net.Conn, 0, len(forwarder.conns))
	for connection := range forwarder.conns {
		connections = append(connections, connection)
	}
	forwarder.mu.Unlock()

	_ = forwarder.listener.Close()
	for _, connection := range connections {
		_ = connection.Close()
	}
	forwarder.wg.Wait()
}

// accept relays every accepted connection until the listener closes.
//
// A listener Accept error is terminal: the harness owns a private listener
// whose only expected error is net.ErrClosed during Close, and any other error
// fails the workload edge closed rather than retrying forever. Close stays
// bounded because every relay dial has a timeout and every tracked connection
// is closed before wg.Wait.
func (forwarder *edgeForwarder) accept() {
	defer forwarder.wg.Done()
	for {
		incoming, err := forwarder.listener.Accept()
		if err != nil {
			return
		}
		forwarder.wg.Add(1)
		go forwarder.relay(incoming)
	}
}

// relay copies one accepted connection to and from the loopback target.
func (forwarder *edgeForwarder) relay(incoming net.Conn) {
	defer forwarder.wg.Done()
	outgoing, err := net.DialTimeout("tcp", forwarder.target, relayDialTimeout)
	if err != nil {
		_ = incoming.Close()
		return
	}
	if !forwarder.track(incoming, outgoing) {
		_ = incoming.Close()
		_ = outgoing.Close()
		return
	}
	defer func() {
		forwarder.untrack(incoming, outgoing)
		_ = incoming.Close()
		_ = outgoing.Close()
	}()

	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(outgoing, incoming)
		closeWrite(outgoing)
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(incoming, outgoing)
		closeWrite(incoming)
	}()
	copies.Wait()
}

// track registers the pair for Close. It reports false once the forwarder is
// closed so a late connection is not relayed after shutdown began.
func (forwarder *edgeForwarder) track(incoming, outgoing net.Conn) bool {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	if forwarder.closed {
		return false
	}
	forwarder.conns[incoming] = struct{}{}
	forwarder.conns[outgoing] = struct{}{}
	return true
}

func (forwarder *edgeForwarder) untrack(incoming, outgoing net.Conn) {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	delete(forwarder.conns, incoming)
	delete(forwarder.conns, outgoing)
}

// closeWrite sends FIN on one direction without dropping the reverse stream, so
// an HTTP client half-close is relayed exactly like a direct connection.
func closeWrite(connection net.Conn) {
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
