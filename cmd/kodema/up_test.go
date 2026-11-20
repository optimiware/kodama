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

func TestUpCLISecretRuleFlags(t *testing.T) {
	origUp := runUpStack
	t.Cleanup(func() { runUpStack = origUp })

	var captured up.UpConfig
	runUpStack = func(_ context.Context, cfg up.UpConfig, _ io.Writer) (up.StackDescriptor, error) {
		captured = cfg
		return up.StackDescriptor{Instance: cfg.Instance}, nil
	}

	var out, errBuf bytes.Buffer
	// Default enables local project secrets without exact-name restriction.
	if code := runUpEntry(context.Background(), []string{"--name", "demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("default runUpEntry exit = %d: %s", code, errBuf.String())
	}
	if captured.DenyProjectSecrets || len(captured.AllowSecrets) != 0 {
		t.Fatalf("default secret config = deny:%t allow:%v; want deny:false allow:nil", captured.DenyProjectSecrets, captured.AllowSecrets)
	}

	// Explicit deny-by-default flag.
	errBuf.Reset()
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--allow-project-secrets=false"}, &out, &errBuf); code != 0 {
		t.Fatalf("--allow-project-secrets=false exit = %d: %s", code, errBuf.String())
	}
	if !captured.DenyProjectSecrets {
		t.Fatal("--allow-project-secrets=false did not set DenyProjectSecrets")
	}

	// Environment variable disables project secrets; flag overrides it back on.
	t.Setenv("KODEMA_UP_ALLOW_PROJECT_SECRETS", "false")
	if code := runUpEntry(context.Background(), []string{"--name", "demo"}, &out, &errBuf); code != 0 || !captured.DenyProjectSecrets {
		t.Fatalf("KODEMA_UP_ALLOW_PROJECT_SECRETS=false: exit=%d deny=%t", code, captured.DenyProjectSecrets)
	}
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--allow-project-secrets=true"}, &out, &errBuf); code != 0 || captured.DenyProjectSecrets {
		t.Fatalf("--allow-project-secrets=true override: exit=%d deny=%t", code, captured.DenyProjectSecrets)
	}

	// Explicit repeatable --allow-secret list.
	t.Setenv("KODEMA_UP_ALLOW_PROJECT_SECRETS", "")
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--allow-secret", "API_KEY", "--allow-secret", "DB_PASSWORD"}, &out, &errBuf); code != 0 {
		t.Fatalf("--allow-secret exit = %d: %s", code, errBuf.String())
	}
	if captured.DenyProjectSecrets || len(captured.AllowSecrets) != 2 || captured.AllowSecrets[0] != "API_KEY" || captured.AllowSecrets[1] != "DB_PASSWORD" {
		t.Fatalf("captured allow secrets = %+v", captured)
	}

	// Refusals: reserved KODEMA_TOKEN, invalid name, conflicting flags, bad env.
	for _, tc := range []struct {
		name string
		env  string
		args []string
	}{
		{"reserved KODEMA_TOKEN", "", []string{"--allow-secret", "KODEMA_TOKEN"}},
		{"lowercase secret name", "", []string{"--allow-secret", "my_secret"}},
		{"conflicting deny and allow-secret", "", []string{"--allow-project-secrets=false", "--allow-secret", "API_KEY"}},
		{"invalid env boolean", "maybe", []string{"--name", "demo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KODEMA_UP_ALLOW_PROJECT_SECRETS", tc.env)
			errBuf.Reset()
			if code := runUpEntry(context.Background(), tc.args, &out, &errBuf); code != 2 {
				t.Fatalf("runUpEntry(%v) exit = %d, want 2 (stderr: %s)", tc.args, code, errBuf.String())
			}
		})
	}
}
