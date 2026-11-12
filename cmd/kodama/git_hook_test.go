package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

type capturedRequest struct {
	mutex    sync.Mutex
	requests []native.Request
	response native.Response
}

func (capture *capturedRequest) handle(_ context.Context, request native.Request) native.Response {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()
	capture.requests = append(capture.requests, request)
	return capture.response
}

func (capture *capturedRequest) last() (native.Request, bool) {
	capture.mutex.Lock()
	defer capture.mutex.Unlock()
	if len(capture.requests) == 0 {
		return native.Request{}, false
	}
	return capture.requests[len(capture.requests)-1], true
}

func startHookSocket(t *testing.T, capture *capturedRequest) string {
	t.Helper()
	base, err := os.MkdirTemp("", "k-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	server, err := native.NewSocketServer(native.SocketConfig{
		Path:            filepath.Join(base, "hook", "hook.sock"),
		Handler:         native.HandlerFunc(capture.handle),
		Timeout:         2 * time.Second,
		RemoveDirectory: true,
	})
	if err != nil {
		os.RemoveAll(base)
		t.Fatalf("NewSocketServer: %v", err)
	}
	if err := server.Listen(); err != nil {
		os.RemoveAll(base)
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = server.Close()
		os.RemoveAll(base)
	})
	return server.Path()
}

func setHookEnvironment(t *testing.T, socket string) {
	t.Helper()
	t.Setenv(native.EnvHookSocket, socket)
	t.Setenv(git.EnvActorRef, "kah_handle")
	t.Setenv(git.EnvRepository, "shop/web")
	t.Setenv(git.EnvResourceID, "res-1")
	t.Setenv(git.EnvSourceEvent, "event-1")
}

func TestGitHookForwardsPreReceiveAndPrintsTheDecision(t *testing.T) {
	capture := &capturedRequest{response: native.Response{ExitCode: 1, Message: native.RejectionMessage}}
	socket := startHookSocket(t, capture)
	setHookEnvironment(t, socket)
	stdin := strings.NewReader(strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " refs/heads/main\n")
	var stderr bytes.Buffer
	code := runGitHook(context.Background(), []string{"pre-receive"}, stdin, io.Discard, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), native.RejectionMessage) {
		t.Fatalf("stderr = %q; want the rejection message", stderr.String())
	}
	request, ok := capture.last()
	if !ok {
		t.Fatal("the hook never reached the socket")
	}
	if request.Phase != native.PhasePreReceive || request.ActorRef != "kah_handle" ||
		request.Repository != "shop/web" || request.ResourceID != "res-1" ||
		request.SourceEvent != "event-1" || len(request.Lines) != 1 {
		t.Fatalf("forwarded request = %+v", request)
	}
}

func TestGitHookForwardsUpdateArgv(t *testing.T) {
	capture := &capturedRequest{response: native.Response{ExitCode: 0}}
	socket := startHookSocket(t, capture)
	setHookEnvironment(t, socket)
	old := strings.Repeat("1", 40)
	candidate := strings.Repeat("2", 40)
	var stderr bytes.Buffer
	code := runGitHook(context.Background(), []string{"update", "refs/heads/main", old, candidate}, strings.NewReader(""), io.Discard, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%q", code, stderr.String())
	}
	request, ok := capture.last()
	if !ok {
		t.Fatal("the update hook never reached the socket")
	}
	if request.Phase != native.PhaseUpdate || request.Ref != "refs/heads/main" || request.Old != old || request.New != candidate {
		t.Fatalf("forwarded request = %+v", request)
	}
}

func TestGitHookFailsClosedWithoutSocket(t *testing.T) {
	setHookEnvironment(t, "")
	var stderr bytes.Buffer
	code := runGitHook(context.Background(), []string{"pre-receive"},
		strings.NewReader(strings.Repeat("1", 40)+" "+strings.Repeat("2", 40)+" refs/heads/main\n"),
		io.Discard, &stderr)
	if code == 0 {
		t.Fatal("a missing socket accepted the hook")
	}
	if !strings.Contains(stderr.String(), native.RejectionMessage) {
		t.Fatalf("stderr = %q; want the rejection message", stderr.String())
	}
}

func TestGitHookFailsClosedWhenSocketUnreachable(t *testing.T) {
	setHookEnvironment(t, filepath.Join(t.TempDir(), "missing", "hook.sock"))
	var stderr bytes.Buffer
	code := runGitHook(context.Background(), []string{"update", "refs/heads/main", strings.Repeat("1", 40), strings.Repeat("2", 40)},
		strings.NewReader(""), io.Discard, &stderr)
	if code == 0 {
		t.Fatal("an unreachable socket accepted the hook")
	}
}

func TestGitHookRefusesUnknownPhase(t *testing.T) {
	var stderr bytes.Buffer
	if code := runGitHook(context.Background(), []string{"evil"}, strings.NewReader(""), io.Discard, &stderr); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
}

func TestGitHookNeverForwardsAmbientSecrets(t *testing.T) {
	capture := &capturedRequest{response: native.Response{ExitCode: 0}}
	socket := startHookSocket(t, capture)
	setHookEnvironment(t, socket)
	const canary = "kodama-secret-canary-value"
	t.Setenv("KODAMA_ACTOR_CREDENTIAL", canary)
	t.Setenv("GIT_HTTP_AUTHORIZATION", canary)
	var stderr bytes.Buffer
	code := runGitHook(context.Background(), []string{"update", "refs/heads/main", strings.Repeat("1", 40), strings.Repeat("2", 40)},
		strings.NewReader(""), io.Discard, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	request, _ := capture.last()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), canary) || strings.Contains(stderr.String(), canary) {
		t.Fatal("the hook forwarded an ambient secret")
	}
}

func TestGitHookDispatch(t *testing.T) {
	called := false
	deps := deps{gitHook: func(context.Context, []string, io.Writer, io.Writer) int {
		called = true
		return 0
	}}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"git", "hook", "pre-receive"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !called {
		t.Fatal("git hook did not dispatch to its runner")
	}
}
