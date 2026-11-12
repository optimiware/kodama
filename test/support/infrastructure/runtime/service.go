// Package runtimesupport starts and drives the real Podman API service used by
// the Process/Podman runtime conformance suite.
//
// It is test support code under test/, never product code. It starts one
// `podman system service` per test on a private Unix socket, resolves the pinned
// Alpine workload image to a digest, creates the retained workspace volumes the
// adapter attaches and records image/version/results evidence. It never starts
// a workload itself; the runtime adapter does that.
package runtimesupport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git/app/native"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// ErrSocketPathTooLong reports a Podman API socket path beyond the product's
// conservative Unix-socket bound. It is a fixture configuration failure with a
// named cause, not an opaque listen error from the service process.
var ErrSocketPathTooLong = errors.New("podman service socket path exceeds the unix socket bound")

// validateServiceSocketPath is the pre-effect guard for the private API socket.
// It runs before the service process starts so an overlong TMPDIR reports the
// real cause instead of a later, opaque listen failure inside Podman.
func validateServiceSocketPath(socket string) error {
	if len(socket) > native.MaxSocketPathBytes {
		return fmt.Errorf("%w: %q is %d bytes; at most %d are allowed",
			ErrSocketPathTooLong, socket, len(socket), native.MaxSocketPathBytes)
	}
	return nil
}

// PodmanAPIVersion is the pinned libpod REST version the driver targets.
const PodmanAPIVersion = "v5.0.0"

// Service is one private Podman API service process.
type Service struct {
	socket string
	podman *harness.Podman
	output *outputBuffer

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	joined  bool
}

// outputBuffer is the combined stdout/stderr capture. The subprocess copy
// goroutines and diagnostics readers run concurrently, so every access is
// guarded by one mutex instead of sharing an unguarded strings.Builder.
type outputBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *outputBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(payload)
}

func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

// StartService starts `podman system service` on a private socket under the
// test's temp directory, waits for the API to answer and registers bounded
// cleanup. It fails or skips through the harness infrastructure policy when
// Podman is unavailable.
func StartService(t testing.TB) *Service {
	t.Helper()
	dir, err := os.MkdirTemp("", "kodama-podman-")
	if err != nil {
		t.Fatalf("create the podman service directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "podman.sock")
	if err := validateServiceSocketPath(socket); err != nil {
		t.Fatalf("podman API service: %v", err)
	}
	service := newService(socket)
	service.podman = harness.NewPodman(
		harness.WithLogger(t.Logf),
		harness.WithEvidenceDir(os.Getenv("KODAMA_EVIDENCE_DIR")),
	)
	if err := service.cmd.Start(); err != nil {
		harness.SkipOrFail(t, requireInfra(), fmt.Errorf("start podman system service: %w", err))
	}
	t.Cleanup(service.Stop)

	if err := waitForAPI(service, "podman API service"); err != nil {
		service.Stop()
		harness.SkipOrFail(t, requireInfra(), fmt.Errorf("podman API service: %w: %s", err, service.Output()))
	}
	return service
}

// newService builds the service process and its capture without starting it.
func newService(socket string) *Service {
	output := &outputBuffer{}
	return &Service{
		socket: socket,
		output: output,
		cmd:    newServiceCommand(socket, output),
	}
}

// newServiceCommand builds the pinned `podman system service` invocation. Both
// stdout and stderr go through the synchronized capture; cmd.Wait joins the copy
// goroutines that feed it.
func newServiceCommand(socket string, output *outputBuffer) *exec.Cmd {
	cmd := exec.Command("podman", "system", "service", "--time=0", "unix://"+socket)
	cmd.Env = harness.BaseEnv()
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd
}

// newServiceForTest wires an already-created command to a Service so the
// capture and shutdown invariants can be exercised without Podman. The caller
// starts the command.
func newServiceForTest(socket string, cmd *exec.Cmd) *Service {
	output := &outputBuffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	return &Service{socket: socket, output: output, cmd: cmd}
}

// Socket is the private API socket path.
func (s *Service) Socket() string { return s.socket }

// Podman is the CLI client that shares this user's Podman storage.
func (s *Service) Podman() *harness.Podman { return s.podman }

// Output returns the captured service output for diagnostics.
func (s *Service) Output() string {
	s.mu.Lock()
	output := s.output
	s.mu.Unlock()
	if output == nil {
		return ""
	}
	return output.String()
}

// Stop kills the service process and joins it, including the stdout/stderr copy
// goroutines. It is idempotent.
func (s *Service) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cmd := s.cmd
	joined := s.joined
	s.joined = true
	s.mu.Unlock()

	if !joined {
		killAndJoin(cmd)
	}
	_ = os.Remove(s.socket)
}

// Restart stops the current service and starts a fresh one on the same socket,
// so containers and volumes in the shared storage remain observable while a
// call is interrupted. It is used only by the ambiguity scenarios. Captured
// output is appended rather than discarded so diagnostics keep the interrupted
// service's last output.
func (s *Service) Restart(t testing.TB) {
	t.Helper()
	s.mu.Lock()
	stopped := s.stopped
	cmd := s.cmd
	joined := s.joined
	s.stopped = true
	s.joined = true
	s.mu.Unlock()

	if !joined {
		killAndJoin(cmd)
	}
	_ = os.Remove(s.socket)
	if stopped {
		return
	}

	restarted := newServiceCommand(s.socket, s.output)
	if err := restarted.Start(); err != nil {
		t.Fatalf("restart podman system service: %v", err)
	}
	s.mu.Lock()
	s.cmd = restarted
	s.stopped = false
	s.joined = false
	s.mu.Unlock()

	if err := waitForAPI(s, "restarted podman API service"); err != nil {
		t.Fatalf("restarted podman API service: %v: %s", err, s.Output())
	}
}

// Kill interrupts the service without a clean shutdown. It is the
// mid-call-connection-loss primitive. The process and its copy goroutines are
// still joined, so a later Restart never races their output against a new
// service.
func (s *Service) Kill() {
	s.mu.Lock()
	cmd := s.cmd
	joined := s.joined
	s.joined = true
	s.mu.Unlock()

	if !joined {
		killAndJoin(cmd)
	}
}

// killAndJoin kills one command and waits for it with Cmd.Wait, which joins the
// stdout/stderr copy goroutines. Process.Wait would reap only the process and
// leave those goroutines writing into the capture after Stop returns.
func killAndJoin(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// waitForAPI probes the libpod ping endpoint on the service's private socket.
func waitForAPI(service *Service, name string) error {
	probe := func(ctx context.Context) error {
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := &net.Dialer{}
				return dialer.DialContext(ctx, "unix", service.socket)
			},
		}}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://d/"+PodmanAPIVersion+"/libpod/_ping", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("podman API ping returned %d", response.StatusCode)
		}
		return nil
	}
	return harness.WaitReady(context.Background(), harness.RealClock{}, name, probe, harness.DefaultWaitOptions())
}

// requireInfra reports whether missing infrastructure must fail rather than
// skip.
func requireInfra() bool { return os.Getenv("KODAMA_REQUIRE_INFRA") == "1" }
