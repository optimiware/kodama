package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/userland/up"
)

func TestUpDownStatusCLICommands(t *testing.T) {
	origUp, origDown, origStatus := runUpStack, runDownStack, runStatusStack
	t.Cleanup(func() {
		runUpStack = origUp
		runDownStack = origDown
		runStatusStack = origStatus
	})

	var capturedUp up.UpConfig
	runUpStack = func(_ context.Context, cfg up.UpConfig, out io.Writer) (up.StackDescriptor, error) {
		capturedUp = cfg
		_, _ = io.WriteString(out, "up-ok\n")
		return up.StackDescriptor{Instance: cfg.Instance}, nil
	}
	var capturedDown up.DownConfig
	runDownStack = func(_ context.Context, cfg up.DownConfig, out io.Writer) error {
		capturedDown = cfg
		_, _ = io.WriteString(out, "down-ok\n")
		return nil
	}
	runStatusStack = func(_ context.Context, cfg up.StatusConfig) (up.StatusReport, error) {
		return up.StatusReport{
			Instance:     cfg.Instance,
			GatewayAlive: true,
			Ready:        true,
			Containers: map[string]up.ContainerInfo{
				"postgres": {Running: true, Status: "running"},
				"nats":     {Running: true, Status: "running"},
				"server":   {Running: true, Status: "running"},
				"git":      {Running: true, Status: "running"},
			},
		}, nil
	}

	var out, errBuf bytes.Buffer
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--listen", "127.0.0.1:9443"}, &out, &errBuf); code != 0 {
		t.Fatalf("runUpEntry exit = %d: %s", code, errBuf.String())
	}
	if capturedUp.Instance != "demo" || capturedUp.ListenAddr != "127.0.0.1:9443" {
		t.Fatalf("capturedUp = %+v", capturedUp)
	}

	// KODEMA_SOCKET names the Podman socket of every command, and --socket wins.
	t.Setenv("KODEMA_SOCKET", "/env/podman.sock")
	if code := runUpEntry(context.Background(), []string{"--name", "demo"}, &out, &errBuf); code != 0 || capturedUp.Socket != "/env/podman.sock" {
		t.Fatalf("runUpEntry with KODEMA_SOCKET: exit %d, socket %q", code, capturedUp.Socket)
	}
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--socket", "/flag/podman.sock"}, &out, &errBuf); code != 0 || capturedUp.Socket != "/flag/podman.sock" {
		t.Fatalf("runUpEntry with --socket: exit %d, socket %q", code, capturedUp.Socket)
	}

	out.Reset()
	if code := runStatusEntry(context.Background(), []string{"--name", "demo", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("runStatusEntry exit = %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"instance": "demo"`) {
		t.Fatalf("unexpected status JSON: %s", out.String())
	}

	out.Reset()
	if code := runDownEntry(context.Background(), []string{"--name", "demo", "--purge"}, &out, &errBuf); code != 0 {
		t.Fatalf("runDownEntry exit = %d: %s", code, errBuf.String())
	}
	if !capturedDown.Purge || capturedDown.Instance != "demo" || capturedDown.Socket != "/env/podman.sock" {
		t.Fatalf("capturedDown = %+v", capturedDown)
	}
}
