package native

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, handler Handler) *SocketServer {
	t.Helper()
	return serveSocket(t, SocketConfig{Handler: handler, Timeout: 2 * time.Second, RemoveDirectory: true})
}

// serveSocket listens on a fresh private socket path with config and serves it
// until the test ends. Serve and Close must both end cleanly.
func serveSocket(t *testing.T, config SocketConfig) *SocketServer {
	t.Helper()
	config.Path = filepath.Join(t.TempDir(), "hook", "hook.sock")
	server, err := NewSocketServer(config)
	if err != nil {
		t.Fatalf("NewSocketServer: %v", err)
	}
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		if err := server.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return server
}

func validRequest(phase Phase) Request {
	request := Request{
		Phase:       phase,
		ActorRef:    "kah_valid",
		Repository:  "shop/web",
		ResourceID:  "res-1",
		SourceEvent: "event-1",
	}
	switch phase {
	case PhaseUpdate:
		request.Ref = "refs/heads/main"
		request.Old = strings.Repeat("1", 40)
		request.New = strings.Repeat("2", 40)
	default:
		request.Lines = []string{strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " refs/heads/main"}
	}
	return request
}

func TestCallRoundTripAndPrivatePermissions(t *testing.T) {
	server := testServer(t, HandlerFunc(func(_ context.Context, request Request) Response {
		if request.Phase != PhasePreReceive {
			return rejectResponse()
		}
		return Response{ExitCode: 0}
	}))
	info, err := os.Stat(filepath.Dir(server.Path()))
	if err != nil {
		t.Fatalf("Stat directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("socket directory mode = %04o; want 0700", mode)
	}
	info, err = os.Stat(server.Path())
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode = %04o; want 0600", mode)
	}
	response, err := Call(context.Background(), server.Path(), validRequest(PhasePreReceive), 2*time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !response.Accepted() {
		t.Fatalf("response = %+v; want accept", response)
	}
}

func TestRawRequestRefusals(t *testing.T) {
	server := testServer(t, HandlerFunc(func(context.Context, Request) Response { return Response{ExitCode: 0} }))
	cases := []struct {
		name string
		body string
	}{
		{"unknown phase", `{"phase":"evil","actor_ref":"kah_x","repository":"shop/web","resource_id":"res-1","source_event":"event-1"}`},
		{"unknown member", `{"phase":"pre-receive","actor_ref":"kah_x","repository":"shop/web","resource_id":"res-1","source_event":"event-1","lines":["a"],"extra":1}`},
		{"trailing value", `{"phase":"pre-receive","actor_ref":"kah_x","repository":"shop/web","resource_id":"res-1","source_event":"event-1","lines":["a"]} {"phase":"pre-receive"}`},
		{"missing environment", `{"phase":"pre-receive","actor_ref":"kah_x","lines":["a"]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := rawCall(t, server.Path(), testCase.body)
			if response.Accepted() {
				t.Fatalf("raw request %q was accepted", testCase.body)
			}
			if response.Message != RejectionMessage {
				t.Fatalf("message = %q; want the uniform rejection", response.Message)
			}
		})
	}
}

func TestOversizedRequestRefused(t *testing.T) {
	path := serveSocket(t, SocketConfig{
		Handler:         HandlerFunc(func(context.Context, Request) Response { return Response{ExitCode: 0} }),
		Timeout:         time.Second,
		MaxRequestBytes: 64,
	}).Path()
	response := rawCall(t, path, strings.Repeat("x", 200))
	if response.Accepted() {
		t.Fatal("an oversized request was accepted")
	}
}

func TestSlowHandlerIsRejectedByTimeout(t *testing.T) {
	path := serveSocket(t, SocketConfig{
		Handler: HandlerFunc(func(ctx context.Context, _ Request) Response {
			<-ctx.Done()
			return Response{ExitCode: 0}
		}),
		Timeout: 50 * time.Millisecond,
	}).Path()
	response, err := Call(context.Background(), path, validRequest(PhasePreReceive), 2*time.Second)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if response.Accepted() {
		t.Fatal("a timed-out decision was accepted")
	}
}

func TestPeerCredentialMismatchRefused(t *testing.T) {
	path := serveSocket(t, SocketConfig{
		Handler: HandlerFunc(func(context.Context, Request) Response { return Response{ExitCode: 0} }),
		Timeout: time.Second,
		PeerUID: func(*net.UnixConn) (uint32, error) {
			return uint32(os.Getuid()) + 1, nil
		},
	}).Path()
	if _, err := Call(context.Background(), path, validRequest(PhasePreReceive), time.Second); err == nil {
		t.Fatal("a different-uid peer was served")
	}
}

func TestPeerCredentialErrorRefused(t *testing.T) {
	path := serveSocket(t, SocketConfig{
		Handler: HandlerFunc(func(context.Context, Request) Response { return Response{ExitCode: 0} }),
		Timeout: time.Second,
		PeerUID: func(*net.UnixConn) (uint32, error) {
			return 0, errors.New("no credentials")
		},
	}).Path()
	if _, err := Call(context.Background(), path, validRequest(PhasePreReceive), time.Second); err == nil {
		t.Fatal("a peer with unreadable credentials was served")
	}
}

func TestCallFailsClosedWhenSocketMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "hook.sock")
	if _, err := Call(context.Background(), path, validRequest(PhasePreReceive), time.Second); err == nil {
		t.Fatal("Call reached a missing socket")
	}
}

func TestListenRefusesPublicDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "public")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	server, err := NewSocketServer(SocketConfig{
		Path:    filepath.Join(directory, "hook.sock"),
		Handler: HandlerFunc(func(context.Context, Request) Response { return Response{ExitCode: 0} }),
	})
	if err != nil {
		t.Fatalf("NewSocketServer: %v", err)
	}
	if err := server.Listen(); err == nil {
		_ = server.Close()
		t.Fatal("Listen accepted a world-reachable directory")
	}
}

func TestSocketPathIsUniqueAndBounded(t *testing.T) {
	base, err := os.MkdirTemp("", "k-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(base)
	first, err := SocketPath(base, "tenant-1")
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	second, err := SocketPath(base, "tenant-1")
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if first == second {
		t.Fatal("two socket paths are identical")
	}
	if len(first) > MaxSocketPathBytes {
		t.Fatalf("socket path length = %d; want at most %d", len(first), MaxSocketPathBytes)
	}
	if !strings.HasPrefix(first, base) {
		t.Fatalf("socket path %q is outside %q", first, base)
	}
}

// rawCall writes one unframed body and decodes the response, bypassing the
// client-side validation so the server-side strict decode is exercised.
func rawCall(t *testing.T, path, body string) Response {
	t.Helper()
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if unix, ok := connection.(*net.UnixConn); ok {
		_ = unix.CloseWrite()
	}
	raw := make([]byte, 4096)
	read, err := connection.Read(raw)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	response, err := decodeResponse(raw[:read])
	if err != nil {
		t.Fatalf("decodeResponse(%q): %v", raw[:read], err)
	}
	return response
}
