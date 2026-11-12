// Loopback redirect listener vector.
//
// The browser flow needs a real loopback callback listener: bound to the
// numeric loopback host 127.0.0.1 with an ephemeral port, serving exactly the
// fixed /callback path once. This vector uses an in-process HTTP client against
// the listener only; it starts no external process or service.
package publiccontract

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestLoopbackListenerBindsLoopbackAndAcceptsOneRequest pins the listener
// contract.
func TestLoopbackListenerBindsLoopbackAndAcceptsOneRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	listener := cli.NewLoopbackListener()
	address, wait, closeListener, err := listener.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := closeListener(); err != nil {
			t.Errorf("close listener: %v", err)
		}
	}()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("callback address %q is not host:port: %v", address, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("callback host = %q; want the numeric loopback 127.0.0.1", host)
	}
	if port == "" || port == "0" {
		t.Errorf("callback port = %q; want a bound ephemeral port", port)
	}

	type callbackResult struct {
		params cli.CallbackParams
		err    error
	}
	results := make(chan callbackResult, 1)
	go func() {
		params, err := wait(ctx)
		results <- callbackResult{params: params, err: err}
	}()

	client := &http.Client{Timeout: 2 * time.Second}

	// A different path is not the callback and must not consume the one
	// accepted request.
	wrong, err := client.Get("http://" + address + "/not-the-callback?code=wrong")
	if err == nil {
		io.Copy(io.Discard, wrong.Body)
		wrong.Body.Close()
		if wrong.StatusCode >= 200 && wrong.StatusCode < 300 {
			t.Errorf("listener accepted the wrong callback path with status %d", wrong.StatusCode)
		}
	}

	first, err := client.Get("http://" + address + cli.OAuthCallbackPath + "?code=CODE-CANARY&state=STATE-CANARY")
	if err != nil {
		t.Fatalf("first callback request: %v", err)
	}
	io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode < 200 || first.StatusCode >= 300 {
		t.Fatalf("first callback status = %d; want a 2xx acknowledgement", first.StatusCode)
	}

	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("wait after the first callback: %v", result.err)
		}
		if result.params.Code != "CODE-CANARY" || result.params.State != "STATE-CANARY" {
			t.Fatalf("callback params = %+v; want the first request values", result.params)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the first callback")
	}

	// A second callback request is refused: the listener is done and must not
	// hand out another authorization response.
	second, err := client.Get("http://" + address + cli.OAuthCallbackPath + "?code=SECOND-CALLBACK")
	if err == nil {
		io.Copy(io.Discard, second.Body)
		second.Body.Close()
		if second.StatusCode >= 200 && second.StatusCode < 300 {
			t.Errorf("listener accepted a second callback with status %d", second.StatusCode)
		}
	}
	if len(results) != 0 {
		t.Error("listener produced a second callback result")
	}
}

// TestLoopbackListenerCloseCancelsWait pins that closing the listener unblocks
// a pending wait without a callback.
func TestLoopbackListenerCloseCancelsWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener := cli.NewLoopbackListener()
	_, wait, closeListener, err := listener.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := wait(ctx)
		done <- err
	}()
	if err := closeListener(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("wait returned success after the listener closed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the listener closed")
	}
}
