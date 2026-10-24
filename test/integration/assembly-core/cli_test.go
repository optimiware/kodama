package assembly_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/cli/auth"
	"github.com/optimiware/kodama/internal/cli/root"
)

// Test_core_assembly_cli drives the built command-line client (`app.Main`, the
// process entry point) against the running composition with real OIDC access
// tokens: the supported groups work end to end, authorization failures keep the
// documented exit codes, the groups of the `test` composition's gate, agent,
// environment and deployment routes work, and the commands of capabilities the
// installation does not serve are absent instead of failing with a 404.
func Test_core_assembly_cli(t *testing.T) {
	s := newStack(t, options{})
	s.start()
	w := s.seed(false)

	dir := t.TempDir()
	credDir := filepath.Join(dir, "credentials")
	if err := os.Mkdir(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := auth.NewFileStore(credDir)
	key := root.Context{Factory: s.base()}.CredentialKey()
	save := func(persona string) {
		if err := store.Save(context.Background(), key, auth.Credential{
			AccessToken: s.token(persona), TokenType: "Bearer", ObtainedAt: time.Now(), Expiry: time.Now().Add(10 * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"KODAMA_CONTEXT_FILE": filepath.Join(dir, "contexts.json"), "KODAMA_CREDENTIAL_DIR": credDir, "KODAMA_ALLOW_INSECURE_LOOPBACK": "true",
	}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		code := app.Main(context.Background(), append(args, "--factory", s.base(), "--tenant", TenantA, "--project", w.project, "--credential-store", "file", "--non-interactive", "--output", "json"),
			func(k string) string { return env[k] }, &out, &errb)
		return code, out.String(), errb.String()
	}

	save("engineer")
	specFile := filepath.Join(dir, "spec.md")
	mustWrite(t, specFile, "# Plan\nbody")
	code, out, errs := run("work", "create", "--kind", "story", "--title", "From the CLI", "--spec-file", specFile)
	if code != 0 {
		t.Fatalf("work create: %d %s %s", code, out, errs)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.ID == "" {
		t.Fatalf("work create output %q: %v", out, err)
	}
	code, out, errs = run("work", "list")
	if code != 0 || !strings.Contains(out, created.ID) || !strings.Contains(out, "From the CLI") {
		t.Fatalf("work list: %d %s %s", code, out, errs)
	}
	if code, out, errs = run("run", "list"); code != 0 {
		t.Fatalf("run list: %d %s %s", code, out, errs)
	}
	if code, out, errs = run("variable", "list"); code != 0 {
		t.Fatalf("variable list: %d %s %s", code, out, errs)
	}

	// A viewer reads but cannot write; a caller without bindings sees nothing.
	save("viewer")
	if code, out, errs = run("work", "list"); code != 0 || !strings.Contains(out, created.ID) {
		t.Fatalf("viewer work list: %d %s %s", code, out, errs)
	}
	if code, out, errs = run("work", "create", "--kind", "story", "--title", "Denied"); code != 3 {
		t.Fatalf("viewer work create exited %d: %s %s", code, out, errs)
	}
	save("outsider")
	if code, out, errs = run("work", "list"); code == 0 {
		t.Fatalf("an unbound caller listed work: %s", out)
	}
	// No credential at all: authentication, not a crash.
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = run("work", "list"); code != 3 {
		t.Fatalf("without a credential: exit %d", code)
	}

	// Offline definition validation shares the server's validator.
	doc := filepath.Join(dir, "pipeline.json")
	mustWrite(t, doc, `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"cli"},"spec":{"inputs":{},"steps":[{"id":"t","kind":"container","timeoutSeconds":60,"command":["/bin/true"],"image":"registry.example.invalid/x@sha256:1111111111111111111111111111111111111111111111111111111111111111"}]}}`)
	var vout, verr bytes.Buffer
	if code := app.Main(context.Background(), []string{"pipeline", "validate", "--file", doc, "--tenant", TenantA, "--project", w.project, "--output", "json"}, func(k string) string { return env[k] }, &vout, &verr); code != 0 {
		t.Fatalf("pipeline validate exited %d: %s %s", code, vout.String(), verr.String())
	}

	// The groups of the routes the test composition serves (execution logs,
	// gates, agent profiles, environments and deployments) work end to end.
	save("engineer")
	for _, args := range [][]string{{"agent", "list"}, {"environment", "list"}, {"deployment", "list"}} {
		if code, out, errs := run(args...); code != 0 {
			t.Errorf("%v: exit %d: %s %s", args, code, out, errs)
		}
	}

	// Commands of capabilities this installation does not serve (model
	// settings, MCP registrations, ChatOps destinations, Git triggers) have no
	// CLI group: they are not offered, instead of failing with a 404.
	for _, args := range [][]string{{"model", "list"}, {"mcp", "list"}, {"chatops", "list"}, {"webhook", "list"}} {
		var out, errb bytes.Buffer
		if code := app.Main(context.Background(), args, func(k string) string { return env[k] }, &out, &errb); code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
	var help bytes.Buffer
	if code := app.Main(context.Background(), []string{"help"}, func(string) string { return "" }, &help, &help); code != 0 ||
		!strings.Contains(help.String(), "work, workspace, pipeline, run") || !strings.Contains(help.String(), "gate, environment, deployment") ||
		strings.Contains(help.String(), "chatops") || strings.Contains(help.String(), "mcp") {
		t.Fatalf("help: %d %s", code, help.String())
	}
}
