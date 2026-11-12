package app

// A slowloris client opens a connection and dribbles request headers forever.
// The served public listener must bound the header read, otherwise one cheap
// client per connection can pin a server goroutine and a file descriptor
// indefinitely. The production server sets ReadHeaderTimeout, so this test
// drives the real Serve path and requires the connection to be cut off.

import (
	"context"
	"net"
	"testing"
	"time"
)

// slowHeaderBudget is the longest this test waits for the server to end a slow
// header exchange. It is the production 10s ReadHeaderTimeout plus a scheduling
// margin: a server without the bound keeps the connection open and the test
// fails at this deadline instead of hanging.
const slowHeaderBudget = 15 * time.Second

func TestServeCutsOffSlowRequestHeaders(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- app.Serve(ctx, listener)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
		if err := app.Wait(); err != nil {
			t.Errorf("Wait: %v", err)
		}
	}
	defer stop()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send a partial request and one header line, then dribble a new header
	// byte at a time. A complete request never arrives, so only the header
	// deadline can end the exchange.
	if _, err := conn.Write([]byte("GET /mcp HTTP/1.1\r\nHost: slowloris.example\r\n")); err != nil {
		t.Fatalf("write the partial request: %v", err)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		buffer := make([]byte, 256)
		for {
			if _, err := conn.Read(buffer); err != nil {
				return
			}
		}
	}()

	deadline := time.NewTimer(slowHeaderBudget)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-closed:
			// The server ended the exchange before the request was complete.
			return
		case <-deadline.C:
			t.Fatalf("the server kept a slow header connection open for %s; want the 10s ReadHeaderTimeout to cut it off", slowHeaderBudget)
		case <-ticker.C:
			if _, err := conn.Write([]byte("X-Slow: x\r\n")); err != nil {
				// The server closed the connection between reads; that is the
				// cut-off this test requires.
				return
			}
		}
	}
}
