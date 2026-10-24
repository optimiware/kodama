package entrypoint_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/entrypoint"
)

// cliArgs is the flag form of the fixture's bootstrap configuration.
func (f *fixture) cliArgs() []string {
	c := f.cfg
	args := []string{
		"--server-url", c.ServerURL, "--attempt-id", c.AttemptID,
		"--identity-dir", c.IdentityDir, "--workspace-dir", c.WorkspaceDir, "--outputs-dir", c.OutputsDir,
		"--secrets-dir", c.SecretsDir, "--results-file", c.ResultsFile, "--token-file", c.TokenFile, "--scratch-dir", c.ScratchDir,
		"--job-name", c.Job.Name, "--job-uid", c.Job.UID, "--pod-name", c.Job.PodName, "--pod-uid", c.Job.PodUID,
		"--kill-grace", "300ms", "--finalize-timeout", "30s", "--settle-timeout", "15s",
	}
	if c.GitCAFile != "" {
		args = append(args, "--git-ca-file", c.GitCAFile)
	}
	return args
}

// cliEnv is the KODAMA_STEP_* form of the same configuration.
func (f *fixture) cliEnv() func(string) string {
	c := f.cfg
	env := map[string]string{
		"KODAMA_STEP_SERVER_URL": c.ServerURL, "KODAMA_STEP_ATTEMPT_ID": c.AttemptID,
		"KODAMA_STEP_IDENTITY_DIR": c.IdentityDir, "KODAMA_STEP_WORKSPACE_DIR": c.WorkspaceDir,
		"KODAMA_STEP_OUTPUTS_DIR": c.OutputsDir, "KODAMA_STEP_SECRETS_DIR": c.SecretsDir,
		"KODAMA_STEP_RESULTS_FILE": c.ResultsFile, "KODAMA_STEP_TOKEN_FILE": c.TokenFile, "KODAMA_STEP_SCRATCH_DIR": c.ScratchDir,
		"KODAMA_JOB_NAME": c.Job.Name, "KODAMA_JOB_UID": c.Job.UID, "KODAMA_POD_NAME": c.Job.PodName, "KODAMA_POD_UID": c.Job.PodUID,
		"KODAMA_STEP_GIT_CA_FILE": c.GitCAFile, "KODAMA_KILL_GRACE": "300ms",
		"PATH": os.Getenv("PATH"), "HOME": c.ChildEnv["HOME"],
		"SECRET_PROCESS_ENV": "must-not-reach-the-command-123456",
	}
	return func(k string) string { return env[k] }
}

func runCLI(t *testing.T, args []string, getenv func(string) string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	code = entrypoint.StepCommand().Run(ctx, args, getenv, &out, &errb)
	return code, out.String(), errb.String()
}

// Test_cli_happy_paths drives a reader and a writer through the registered
// `step` command (flags, then the KODAMA_STEP_* environment) the way the Job
// Pod does: bootstrap files, real Git over TLS, the contract server.
func Test_cli_happy_paths(t *testing.T) {
	if c := entrypoint.StepCommand(); c.Name != "step" || c.Run == nil || c.Summary == "" {
		t.Fatalf("command = %+v", c)
	}

	t.Run("reader_with_flags", func(t *testing.T) {
		r := newRepo(t)
		f := newFixture(t, `cat a.txt > "$KODAMA_OUTPUTS_DIR/seen.txt"
printf '{"outcome":"completed","outputs":{"lines":1}}' > "$KODAMA_RESULTS_FILE"
echo "env: ${SECRET_PROCESS_ENV:-absent}" >&2
echo scribble > a.txt`)
		f.withRepo(r, runtimev1.AccessRead, r.c1)
		f.declareFile("seen", "/kodama/outputs/seen.txt")
		f.declare("lines", runtimev1.OutputInteger)
		code, stdout, stderr := runCLI(t, f.cliArgs(), func(k string) string {
			return map[string]string{"PATH": os.Getenv("PATH"), "HOME": f.cfg.ChildEnv["HOME"], "SECRET_PROCESS_ENV": "must-not-reach-the-command-123456"}[k]
		})
		if code != entrypoint.ExitOK {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit != nil || len(res.Outputs) != 2 {
			t.Fatalf("result = %+v", res)
		}
		outs := f.srv.Artifacts(runtimev1.RoleOutput)
		if len(outs) != 1 || strings.TrimSpace(string(outs[0].bytes)) != "one" {
			t.Fatalf("outputs = %+v", outs)
		}
		diag := f.diagnostics(res)["command.log"]
		if !strings.Contains(diag, "env: absent") {
			t.Fatalf("the process environment leaked into the command: %q", diag)
		}
		if f.srv.Count("proposeSource") != 0 {
			t.Fatal("a reader proposed source")
		}
		if !strings.Contains(stderr, "result acknowledged") {
			t.Fatalf("stderr lacks the acknowledgment log:\n%s", stderr)
		}
		token, _ := os.ReadFile(f.cfg.TokenFile)
		for _, n := range []string{gitPass, f.srv.admission, string(token)} {
			if strings.Contains(stderr, n) || strings.Contains(stdout, n) {
				t.Fatalf("credential %q... in the CLI output", n[:4])
			}
		}
	})

	t.Run("writer_with_environment", func(t *testing.T) {
		f, r := newWriter(t, `echo "$KODAMA_STEP_ID" > step.txt
printf '{"outcome":"completed","outputs":{"n":3}}' > "$KODAMA_RESULTS_FILE"`)
		f.declare("n", runtimev1.OutputInteger)
		code, stdout, stderr := runCLI(t, nil, f.cliEnv())
		if code != entrypoint.ExitOK {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		res := f.acceptedResult()
		if res.Outcome != runtimev1.OutcomeCompleted || res.SourceCommit == nil || string(*res.SourceCommit) != r.tip() || r.tip() == r.c2 {
			t.Fatalf("result = %+v, tip %s", res, r.tip())
		}
		if got := gitRun(t, r.bare, "show", branch+":step.txt"); got != "build" {
			t.Fatalf("published step.txt = %q", got)
		}
		order := f.srv.Order()
		if len(order) != 2 || order[0] != "source_accepted" || order[1] != "result" {
			t.Fatalf("order = %v", order)
		}
		f.credentialsAbsent(gitPass)
	})

	t.Run("exit_codes", func(t *testing.T) {
		code, _, stderr := runCLI(t, nil, func(string) string { return "" })
		if code != entrypoint.ExitUsage || !strings.Contains(stderr, "--server-url") {
			t.Fatalf("missing flags: exit %d, stderr %q", code, stderr)
		}
		code, _, _ = runCLI(t, []string{"--no-such-flag"}, func(string) string { return "" })
		if code != entrypoint.ExitUsage {
			t.Fatalf("unknown flag: exit %d", code)
		}
		code, _, _ = runCLI(t, []string{"positional"}, func(string) string { return "" })
		if code != entrypoint.ExitUsage {
			t.Fatalf("positional argument: exit %d", code)
		}
		code, stdout, _ := runCLI(t, []string{"--help"}, func(string) string { return "" })
		if code != entrypoint.ExitOK || !strings.Contains(stdout, "-server-url") || !strings.Contains(stdout, "KODAMA_STEP_SERVER_URL") {
			t.Fatalf("help: exit %d, stdout %q", code, stdout)
		}

		f := newFixture(t, markRun+`echo hi`)
		f.srv.Revoke()
		if code, _, _ := runCLI(t, f.cliArgs(), f.cliEnv()); code != entrypoint.ExitRevoked || f.launches() != 0 {
			t.Fatalf("revoked admission: exit %d", code)
		}
		g := newFixture(t, markRun+`echo hi`)
		g.srv.unsupported = true
		if code, _, _ := runCLI(t, g.cliArgs(), g.cliEnv()); code != entrypoint.ExitAdmission || g.launches() != 0 {
			t.Fatalf("unsupported profile: exit %d", code)
		}
		h := newFixture(t, `echo hi`)
		h.srv.preAccept(runtimev1.Digest("sha256:" + strings.Repeat("c", 64)))
		if code, _, _ := runCLI(t, h.cliArgs(), h.cliEnv()); code != entrypoint.ExitResultConflict {
			t.Fatalf("result conflict: exit %d", code)
		}
	})

	t.Run("flags_override_environment_and_plain_http_is_loopback_only", func(t *testing.T) {
		var out bytes.Buffer
		if _, err := entrypoint.ParseConfig([]string{"--server-url", "http://example.com", "--attempt-id", "att_it01"}, func(string) string { return "" }, &out); err == nil {
			t.Fatal("plain http to a non-loopback host was accepted")
		}
	})
}
