package messaging

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// DropProxy is a bounded TCP proxy in front of the broker. It forwards client
// traffic to the server normally, but once armed it drops the next
// server-to-client frame and closes both sides. That models a broker that
// stored a publish whose acknowledgement never reached the client.
type DropProxy struct {
	listener net.Listener
	upstream string

	mu     sync.Mutex
	armed  bool
	closed bool
	conns  []net.Conn
}

// NewDropProxy starts a proxy in front of upstream (host:port).
func NewDropProxy(t *testing.T, upstream string) *DropProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen drop proxy: %v", err)
	}
	proxy := &DropProxy{listener: listener, upstream: upstream}
	go proxy.acceptLoop()
	t.Cleanup(proxy.Close)
	return proxy
}

// Addr returns the proxy's loopback address.
func (proxy *DropProxy) Addr() string { return proxy.listener.Addr().String() }

// URL returns the nats:// URL of the proxy.
func (proxy *DropProxy) URL() string { return "nats://" + proxy.Addr() }

// ArmDropNextServerFrame drops the next server-to-client frame on whichever
// proxied connection produces one and closes that connection.
func (proxy *DropProxy) ArmDropNextServerFrame() {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	proxy.armed = true
}

// Close stops the listener and every proxied connection.
func (proxy *DropProxy) Close() {
	proxy.mu.Lock()
	if proxy.closed {
		proxy.mu.Unlock()
		return
	}
	proxy.closed = true
	conns := append([]net.Conn(nil), proxy.conns...)
	proxy.mu.Unlock()
	_ = proxy.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (proxy *DropProxy) acceptLoop() {
	for {
		client, err := proxy.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", proxy.upstream, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		proxy.mu.Lock()
		if proxy.closed {
			proxy.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			return
		}
		proxy.conns = append(proxy.conns, client, server)
		proxy.mu.Unlock()
		go proxy.forwardRequests(client, server)
		go proxy.forwardResponses(client, server)
	}
}

func (proxy *DropProxy) forwardRequests(client, server net.Conn) {
	_, _ = io.Copy(server, client)
	_ = client.Close()
	_ = server.Close()
}

func (proxy *DropProxy) forwardResponses(client, server net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		read, err := server.Read(buffer)
		if read > 0 {
			proxy.mu.Lock()
			drop := proxy.armed
			if drop {
				proxy.armed = false
			}
			proxy.mu.Unlock()
			if drop {
				// The server frame (the acknowledgement) is discarded and
				// the connection is torn down, so the client cannot report a
				// successful publish.
				_ = client.Close()
				_ = server.Close()
				return
			}
			if _, writeErr := client.Write(buffer[:read]); writeErr != nil {
				_ = client.Close()
				_ = server.Close()
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				_ = client.Close()
			}
			_ = server.Close()
			return
		}
	}
}
