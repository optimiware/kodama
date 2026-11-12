package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeMountsThePublicHandlerAndShutsDown(t *testing.T) {
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

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	eventually(t, 5*time.Second, func() bool {
		response, err := http.Post("http://"+listener.Addr().String()+"/mcp", "application/json", bytes.NewReader(body))
		if err != nil {
			return false
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode == http.StatusOK
	})

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	if err := app.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestRunJoinsOnCancellation(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
