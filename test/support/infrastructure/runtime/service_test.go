package runtimesupport

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git/app/native"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// TestServiceOutputConcurrentRead pins the capture invariant: the service
// output reader and the subprocess copy goroutines never touch the capture
// buffer concurrently. The child writes over a short interval so the copy
// goroutine and the reader genuinely overlap; an unsynchronized builder is
// reported by the race detector, not by a timing-sensitive string assertion.
func TestServiceOutputConcurrentRead(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c",
		`i=0; while [ "$i" -lt 100 ]; do printf 'line-%03d\n' "$i"; i=$((i+1)); sleep 0.01; done`)
	service := newServiceForTest(filepath.Join(dir, "service.sock"), cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start output command: %v", err)
	}
	t.Cleanup(service.Stop)

	probe := func(context.Context) error {
		if strings.Contains(service.Output(), "line-099") {
			return nil
		}
		return errors.New("service output is incomplete")
	}
	if err := harness.WaitReady(context.Background(), harness.RealClock{}, "service output", probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for service output: %v (output %q)", err, service.Output())
	}
}

// TestServiceSocketPathGuard pins the Podman fixture's pre-effect socket
// guard: a private API socket path beyond the product's conservative sun_path
// bound is refused with a named cause before `podman system service` starts,
// instead of failing later with an opaque listen error.
func TestServiceSocketPathGuard(t *testing.T) {
	overlong := strings.Repeat("a", native.MaxSocketPathBytes+1)
	err := validateServiceSocketPath(overlong)
	if err == nil {
		t.Fatalf("accepted a %d-byte service socket path", len(overlong))
	}
	if !errors.Is(err, ErrSocketPathTooLong) {
		t.Fatalf("overlong service socket error = %v; want ErrSocketPathTooLong", err)
	}
	accepted := filepath.Join(t.TempDir(), "podman.sock")
	if err := validateServiceSocketPath(accepted); err != nil {
		t.Fatalf("refused the short service socket path %q: %v", accepted, err)
	}
}

// gatedWriter blocks the first subprocess write until the test releases it. It
// lets the test observe whether Stop waits for the output copy goroutines
// instead of relying on a timing window.
type gatedWriter struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	data    bytes.Buffer
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}

func (w *gatedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

// TestServiceStopJoinsOutput pins the shutdown invariant: Stop returns only
// after the subprocess output copy pipeline has finished, so no write into the
// captured output (or a driver-supplied writer) survives Stop. Process.Wait
// reaps the process without joining those goroutines and fails this test.
func TestServiceStopJoinsOutput(t *testing.T) {
	dir := t.TempDir()
	writer := newGatedWriter()
	cmd := exec.Command("sh", "-c", `printf 'complete-output\n'`)
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		t.Fatalf("start output command: %v", err)
	}
	// The writer is supplied by the test and Stop does not read it, so only the
	// command and socket are needed here.
	service := &Service{socket: filepath.Join(dir, "service.sock"), cmd: cmd}
	select {
	case <-writer.started:
	case <-time.After(10 * time.Second):
		t.Fatal("subprocess output never reached the capture writer")
	}

	stopped := make(chan struct{})
	go func() {
		service.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		close(writer.release)
		t.Fatal("Stop returned before the subprocess output copy finished")
	case <-time.After(200 * time.Millisecond):
	}
	close(writer.release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the subprocess output copy finished")
	}
	if got := writer.String(); got != "complete-output\n" {
		t.Fatalf("captured output after Stop = %q; want complete-output", got)
	}
}
