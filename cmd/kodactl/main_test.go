package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/userland/cli"
)

func TestKodactlDispatchesOnlyTheCLI(t *testing.T) {
	isolateUserConfig(t)
	calls := 0
	var gotConfig cli.Config
	var gotArgs []string
	var gotPorts cli.Ports
	var gotDeps cli.CommandDeps
	deps := cliDeps{runCLI: func(_ context.Context, config cli.Config, args []string, ports cli.Ports, commandDeps cli.CommandDeps) int {
		calls++
		gotConfig = config
		gotArgs = append([]string(nil), args...)
		gotPorts = ports
		gotDeps = commandDeps
		return 0
	}}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--server-url", "https://factory.example",
		"--credential-file", "/run/secrets/token",
		"call", "task.read", "--input={\"task_id\":\"task-1\"}",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if calls != 1 {
		t.Fatalf("CLI calls = %d; want 1", calls)
	}
	if gotConfig.ServerURL != "https://factory.example" || gotConfig.CredentialFile != "/run/secrets/token" {
		t.Fatalf("config = %+v", gotConfig)
	}
	if len(gotArgs) != 3 || gotArgs[0] != "call" || gotArgs[1] != "task.read" || gotArgs[2] != "--input={\"task_id\":\"task-1\"}" {
		t.Fatalf("args = %v", gotArgs)
	}
	if gotPorts.Client == nil || gotPorts.Files == nil || gotPorts.Session == nil || gotPorts.Credential == nil {
		t.Fatalf("ports = %+v", gotPorts)
	}
	if gotDeps.Login.OIDC == nil || gotDeps.Login.Store == nil || gotDeps.Login.Lock == nil ||
		gotDeps.Login.Browser == nil || gotDeps.Login.Loopback == nil || gotDeps.Login.Random == nil ||
		gotDeps.Login.Clock == nil || gotDeps.Login.Environment == nil {
		t.Fatalf("the real login runtime was not wired: %+v", gotDeps.Login)
	}
}

func TestKodactlWithoutServerURLStartsNoTransport(t *testing.T) {
	isolateUserConfig(t)
	var gotPorts cli.Ports
	deps := cliDeps{runCLI: func(_ context.Context, _ cli.Config, _ []string, ports cli.Ports, _ cli.CommandDeps) int {
		gotPorts = ports
		return 0
	}}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"call", "task.read"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if gotPorts.Client != nil {
		t.Fatal("kodactl built a transport without a server URL")
	}
}

func TestKodactlRefusesUnknownFlags(t *testing.T) {
	called := false
	deps := cliDeps{runCLI: func(context.Context, cli.Config, []string, cli.Ports, cli.CommandDeps) int {
		called = true
		return 0
	}}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--nope"}, &stdout, &stderr, deps)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if called {
		t.Fatal("an unknown flag reached the CLI command logic")
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestKodactlPassesEmptyCommandToUsage(t *testing.T) {
	var gotArgs []string
	deps := cliDeps{runCLI: func(_ context.Context, _ cli.Config, args []string, _ cli.Ports, _ cli.CommandDeps) int {
		gotArgs = append([]string(nil), args...)
		return 2
	}}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr, deps); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if len(gotArgs) != 0 {
		t.Fatalf("args = %v; want empty", gotArgs)
	}
}
