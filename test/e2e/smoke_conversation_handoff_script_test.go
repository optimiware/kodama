//go:build e2e

package e2e

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a stdout/stderr capture the test can read while the fixture
// script is still running.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestConversationHandoffReadinessMarkerPrecedesPublish proves the S3 fixture
// prints its readiness marker before it publishes the event. A consumer can
// observe the committed event and cancel the action while the publish request
// is still in flight, so the event only proves readiness when the marker is
// already on stdout at request arrival.
//
// The endpoint withholds its response until after the test has inspected the
// captured stdout, so the wrong ordering (publish, check the response, then
// print) cannot pass. The fixture is cancelled before the response is released.
func TestConversationHandoffReadinessMarkerPrecedesPublish(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is unavailable; the workload images provide it", tool)
		}
	}

	workspace := t.TempDir()
	taskFile := filepath.Join(workspace, "task.json")
	tokenFile := filepath.Join(workspace, "KODAMA_TOKEN")
	writeFile(t, taskFile, "[]\n")
	writeFile(t, tokenFile, "hermetic-execution-token\n")

	const (
		marker  = "ready-hermetic"
		project = "project-hermetic"
	)
	var (
		once        sync.Once
		releaseOnce sync.Once
		request     struct {
			path string
			auth string
			body []byte
		}
	)
	arrived := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request.path = r.URL.Path
		request.auth = r.Header.Get("Authorization")
		request.body = body
		once.Do(func() { close(arrived) })
		// Hold the response until the fixture has been cancelled; a consumer
		// can cancel in exactly this window.
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"event_id":"event-hermetic"}`)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})

	command := exec.Command("/bin/sh", "-c", conversationHandoffLiveScript)
	command.Env = mergeEnv(os.Environ(),
		"SMOKE_ANSWER=answer-hermetic",
		"SMOKE_RESTORATION_MARKER=restored-hermetic",
		"SMOKE_READY_MARKER="+marker,
		"SMOKE_PROJECT="+project,
		"SMOKE_READY_KEY=key-hermetic",
		"SMOKE_WORKSPACE="+workspace,
		"SMOKE_TASK_FILE="+taskFile,
		"SMOKE_TOKEN_PATH="+tokenFile,
		"KODAMA_EXECUTION=execution-hermetic",
		"KODAMA_URL="+server.URL,
	)
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	command.Stdout = stdout
	command.Stderr = stderr
	// The script runs curl as a child; kill the whole group so the withheld
	// response cannot leave a process behind.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start the conversation-handoff fixture: %v", err)
	}
	var waitOnce sync.Once
	stop := func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		waitOnce.Do(func() { _ = command.Wait() })
	}
	t.Cleanup(stop)

	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatalf("the fixture never published its readiness event; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	// The publish request is blocked in the endpoint. Anything the script
	// prints after its response arrives cannot count, so the marker must
	// already be on stdout now.
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(stdout.String(), marker+"\n") == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("readiness marker %q was not printed before the publish request; stdout=%q stderr=%q",
				marker, stdout.String(), stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := stdout.String()
	if count := strings.Count(got, marker+"\n"); count != 1 {
		t.Fatalf("stdout carries %d readiness marker lines before publication; want exactly 1: %q", count, got)
	}
	if request.path != "/api/v1/projects/"+project+"/events" {
		t.Fatalf("publish request path = %q; want the project events endpoint", request.path)
	}
	if request.auth != "Bearer hermetic-execution-token" {
		t.Fatalf("publish request authorization = %q; want the mounted execution credential", request.auth)
	}
	if !bytes.Contains(request.body, []byte(`"type":"observation.custom"`)) ||
		!bytes.Contains(request.body, []byte(`"subject_id":"`+marker+`"`)) {
		t.Fatalf("publish request body = %s; want the readiness observation", request.body)
	}

	// Cancel the fixture before releasing the withheld response.
	stop()
	releaseOnce.Do(func() { close(release) })
}
