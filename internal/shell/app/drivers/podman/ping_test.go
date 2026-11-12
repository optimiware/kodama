package podman

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPingProbesTheAPIAndCloseKeepsTheDaemon pins the readiness probe the
// durable test profile uses: Ping is one bounded GET of the libpod _ping
// endpoint, a non-200 answer or a dead socket is a failure, and Close only
// releases the client's idle connections: it sends no request, so it can
// never stop the external daemon or a workload.
func TestPingProbesTheAPIAndCloseKeepsTheDaemon(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	var requests atomic.Int32
	var lastPath atomic.Value
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		lastPath.Store(r.Method + " " + r.URL.Path)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte("OK"))
	}))
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := lastPath.Load(); got != "GET /v5.0.0/libpod/_ping" {
		t.Fatalf("Ping request = %v; want GET /v5.0.0/libpod/_ping", got)
	}
	status.Store(http.StatusInternalServerError)
	if err := client.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Ping on a failing daemon = %v; want a status failure", err)
	}
	before := requests.Load()
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if requests.Load() != before {
		t.Fatal("Close sent a request to the daemon")
	}

	dead, err := NewLibpod(LibpodConfig{SocketPath: "/nonexistent/podman.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if err := dead.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against a missing socket")
	}
}
