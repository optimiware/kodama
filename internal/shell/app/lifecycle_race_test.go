package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// TestCloseConcurrentWithRunStartup drives Close against Run startup so the
// race detector sees the shared cancel and WaitGroup state, and every caller
// still returns.
func TestCloseConcurrentWithRunStartup(t *testing.T) {
	for iteration := range 20 {
		environment := newTestEnvironment(t)
		app, err := New(context.Background(), environment.config)
		if err != nil {
			t.Fatalf("iteration %d New: %v", iteration, err)
		}
		runResult := make(chan error, 1)
		closeResult := make(chan error, 1)
		go func() {
			runResult <- app.Run(context.Background())
		}()
		go func() {
			closeResult <- app.Close(context.Background())
		}()
		select {
		case err := <-closeResult:
			if err != nil {
				t.Fatalf("iteration %d Close: %v", iteration, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d Close did not return", iteration)
		}
		select {
		case <-runResult:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d Run did not return", iteration)
		}
	}
}

// TestCloseConcurrentWithServeStartup drives Close against Serve startup.
func TestCloseConcurrentWithServeStartup(t *testing.T) {
	for iteration := range 20 {
		environment := newTestEnvironment(t)
		app, err := New(context.Background(), environment.config)
		if err != nil {
			t.Fatalf("iteration %d New: %v", iteration, err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("iteration %d listen: %v", iteration, err)
		}
		serveResult := make(chan error, 1)
		closeResult := make(chan error, 1)
		go func() {
			serveResult <- app.Serve(context.Background(), listener)
		}()
		go func() {
			closeResult <- app.Close(context.Background())
		}()
		select {
		case err := <-closeResult:
			if err != nil {
				t.Fatalf("iteration %d Close: %v", iteration, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d Close did not return", iteration)
		}
		select {
		case <-serveResult:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d Serve did not return", iteration)
		}
		_ = listener.Close()
	}
}

// TestRunAndServeAfterCloseLaunchNothing proves a start after Close fails
// before any loop or workload starts.
func TestRunAndServeAfterCloseLaunchNothing(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := app.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	baseline := runtime.NumGoroutine()
	runResult := make(chan error, 1)
	go func() {
		runResult <- app.Run(context.Background())
	}()
	select {
	case err := <-runResult:
		if err == nil {
			t.Fatal("Run succeeded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run after Close did not return")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- app.Serve(context.Background(), listener)
	}()
	select {
	case err := <-serveResult:
		if err == nil {
			t.Fatal("Serve succeeded after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve after Close did not return")
	}
	if environment.runtime.startCount() != 0 {
		t.Fatalf("a start after Close launched %d workloads", environment.runtime.startCount())
	}
	eventually(t, 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

// TestExternalCloseStopsServing proves Close while Serve is serving makes Serve
// return and stops the HTTP server instead of leaving it serving against closed
// stores.
func TestExternalCloseStopsServing(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- app.Serve(context.Background(), listener)
	}()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	client := &http.Client{Timeout: time.Second}
	eventually(t, 5*time.Second, func() bool {
		response, err := client.Post("http://"+listener.Addr().String()+"/mcp", "application/json", bytes.NewReader(body))
		if err != nil {
			return false
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode == http.StatusOK
	})

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("Serve after external Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept serving after an external Close")
	}
	if err := app.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	response, err := client.Post("http://"+listener.Addr().String()+"/mcp", "application/json", bytes.NewReader(body))
	if err == nil {
		response.Body.Close()
		t.Fatal("the HTTP server still served a request after Close")
	}
}
