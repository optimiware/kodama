package clientcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestKodemaDispatchesOnlyTheCLI(t *testing.T) {
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
	if !filepath.IsAbs(gotDeps.CommandPath) {
		t.Fatalf("command path = %q; want the absolute path of this executable", gotDeps.CommandPath)
	}
}

func TestKodemaWithoutServerURLStartsNoTransport(t *testing.T) {
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
		t.Fatal("kodema built a transport without a server URL")
	}
}

func TestKodemaRefusesUnknownFlags(t *testing.T) {
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

func TestKodemaPassesEmptyCommandToUsage(t *testing.T) {
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

// TestMountedExecutionCredentialIsTheDefaultInsideAnAction pins that a client
// command inside a container action authenticates with the mounted execution
// credential without any flag, that an explicit credential file still wins and
// that a workstation, which has no such mount, keeps the human session.
func TestMountedExecutionCredentialIsTheDefaultInsideAnAction(t *testing.T) {
	isolateUserConfig(t)
	mounted := filepath.Join(t.TempDir(), "KODEMA_TOKEN")
	if err := os.WriteFile(mounted, []byte("kodex.exec-1.signature"), 0o444); err != nil {
		t.Fatal(err)
	}
	configured := func(t *testing.T, args ...string) cli.Config {
		t.Helper()
		var got cli.Config
		deps := cliDeps{runCLI: func(_ context.Context, config cli.Config, _ []string, _ cli.Ports, _ cli.CommandDeps) int {
			got = config
			return 0
		}}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), append(args, "whoami"), &stdout, &stderr, deps); code != 0 {
			t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
		}
		return got
	}

	previous := mountedCredentialFile
	t.Cleanup(func() { mountedCredentialFile = previous })

	mountedCredentialFile = mounted
	t.Run("runtime context wins over workstation defaults in the image", func(t *testing.T) {
		t.Setenv("KODEMA_URL", "https://workload.example")
		t.Setenv("KODEMA_SERVER", "https://workstation.example")
		t.Setenv("KODEMA_CREDENTIAL_FILE", "/workstation/credential")
		if got := configured(t); got.ServerURL != "https://workload.example" || got.CredentialFile != mounted {
			t.Fatalf("workload default = %q %q", got.ServerURL, got.CredentialFile)
		}
		if got := configured(t, "--server", "https://explicit.example", "--credential-file", "/run/secrets/service"); got.ServerURL != "https://explicit.example" || got.CredentialFile != "/run/secrets/service" {
			t.Fatalf("explicit flags lost: %q %q", got.ServerURL, got.CredentialFile)
		}
	})

	mountedCredentialFile = filepath.Join(t.TempDir(), "absent")
	if got := configured(t).CredentialFile; got != "" {
		t.Fatalf("credential file = %q; want none on a workstation", got)
	}
}

// TestGlobalFlagsAcceptedAfterSubcommand pins that global client flags such as
// --output, --project, and --session work both before and after the subcommand,
// while approve/reject `--output k=v` pairs stay with the subcommand.
func TestGlobalFlagsAcceptedAfterSubcommand(t *testing.T) {
	isolateUserConfig(t)
	var gotConfig cli.Config
	var gotArgs []string
	deps := cliDeps{runCLI: func(_ context.Context, config cli.Config, args []string, _ cli.Ports, _ cli.CommandDeps) int {
		gotConfig = config
		gotArgs = append([]string(nil), args...)
		return 0
	}}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"call", "pipeline.list", "--input", "{}", "--output", "json", "--project", "proj-after", "--server", "https://factory.example",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if gotConfig.Output != cli.OutputJSON || gotConfig.Project != "proj-after" || gotConfig.ServerURL != "https://factory.example" {
		t.Fatalf("config = %+v; want output=json project=proj-after server=https://factory.example", gotConfig)
	}
	wantArgs := []string{"call", "pipeline.list", "--input", "{}"}
	if strings.Join(gotArgs, "|") != strings.Join(wantArgs, "|") {
		t.Fatalf("args = %v; want %v", gotArgs, wantArgs)
	}

	// On approve, `--output k=v` stays with approve while `--output json` is hoisted.
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"approve", "exec-1", "--output", "approved=yes", "--output", "json",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("approve exit = %d; stderr=%s", code, stderr.String())
	}
	if gotConfig.Output != cli.OutputJSON {
		t.Fatalf("approve config.Output = %q; want json", gotConfig.Output)
	}
	wantApproveArgs := []string{"approve", "exec-1", "--output", "approved=yes"}
	if strings.Join(gotArgs, "|") != strings.Join(wantApproveArgs, "|") {
		t.Fatalf("approve args = %v; want %v", gotArgs, wantApproveArgs)
	}

	// On repo create, `--trusted-origin` stays with repo create while `--project` is hoisted.
	stdout.Reset()
	stderr.Reset()
	code = run(context.Background(), []string{
		"repo", "create", "shop/web", "--project", "proj-repo", "--trusted-origin", "https://git.example",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("repo create exit = %d; stderr=%s", code, stderr.String())
	}
	if gotConfig.Project != "proj-repo" || len(gotConfig.TrustedOrigins) != 0 {
		t.Fatalf("repo create config = %+v; want project=proj-repo and no hoisted trusted origins", gotConfig)
	}
	wantRepoArgs := []string{"repo", "create", "shop/web", "--trusted-origin", "https://git.example"}
	if strings.Join(gotArgs, "|") != strings.Join(wantRepoArgs, "|") {
		t.Fatalf("repo create args = %v; want %v", gotArgs, wantRepoArgs)
	}
}
