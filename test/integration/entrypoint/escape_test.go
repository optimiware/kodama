package entrypoint_test

import (
	"os"
	"path/filepath"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/entrypoint"
)

const (
	secretValue = "s3cr3t-erp-token-91d7a0"
	envSecret   = "env-pass-5b2c8f1e44"
)

// Test_path_escape covers PX-10 (reject path and symlink escape) and VS-11
// (secret and identity files are never collected): an output that resolves
// outside the outputs root, or to a secret or identity file, fails the step
// with output_invalid and nothing foreign is uploaded.
func Test_path_escape(t *testing.T) {
	newEscapeFixture := func(t *testing.T, script string) *fixture {
		f := newFixture(t, script)
		f.secret("erp-token", "secret://project/erp-token", secretValue, runtimev1.DeliveryFile)
		f.secret("db-pass", "secret://project/db-pass", envSecret, runtimev1.DeliveryEnv)
		f.declareFile("report", "/kodama/outputs/report.json")
		return f
	}
	assertRefused := func(t *testing.T, f *fixture) {
		t.Helper()
		f.wantExit(f.run(), entrypoint.ExitOK)
		r := f.acceptedResult()
		f.wantFailed(r, "output_invalid")
		if outs := f.srv.Artifacts(runtimev1.RoleOutput); len(outs) != 0 {
			t.Fatalf("an escaping output was uploaded: %+v", outs)
		}
		token := ""
		if b, err := os.ReadFile(f.cfg.TokenFile); err == nil {
			token = string(b)
		}
		f.credentialsAbsent(secretValue, envSecret, token)
	}

	t.Run("symlink_to_secret_file", func(t *testing.T) {
		f := newEscapeFixture(t, `ln -s "$KODAMA_SECRETS_DIR/erp-token" "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, f)
	})

	t.Run("symlink_to_identity_token", func(t *testing.T) {
		f := newEscapeFixture(t, `ln -s "`+"$IDENTITY"+`/token" "$KODAMA_OUTPUTS_DIR/report.json"`)
		f.srv.ctx.Command.Env["IDENTITY"] = f.path("identity")
		assertRefused(t, f)
	})

	t.Run("symlink_to_dotdot_target", func(t *testing.T) {
		f := newEscapeFixture(t, `echo outside > "$KODAMA_OUTPUTS_DIR/../escaped.txt"; ln -s ../escaped.txt "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, f)
	})

	t.Run("symlinked_directory_component", func(t *testing.T) {
		f := newEscapeFixture(t, `ln -s "$KODAMA_SECRETS_DIR" "$KODAMA_OUTPUTS_DIR/linked"`)
		f.srv.ctx.Outputs = nil
		f.declareFile("report", "/kodama/outputs/linked/erp-token")
		assertRefused(t, f)
	})

	t.Run("hard_link_to_secret_file", func(t *testing.T) {
		f := newEscapeFixture(t, `ln "$KODAMA_SECRETS_DIR/erp-token" "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, f)
	})

	t.Run("fifo_and_directory_outputs", func(t *testing.T) {
		f := newEscapeFixture(t, `mkfifo "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, f)
		g := newEscapeFixture(t, `mkdir "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, g)
	})

	t.Run("replaced_output_root_symlink_inside", func(t *testing.T) {
		// A real file is fine; the same name swapped for a symlink after the
		// command's own write is not.
		f := newEscapeFixture(t, `echo ok > "$KODAMA_OUTPUTS_DIR/real.json"; rm -f "$KODAMA_OUTPUTS_DIR/real.json"; ln -s /etc/hosts "$KODAMA_OUTPUTS_DIR/report.json"`)
		assertRefused(t, f)
	})

	t.Run("manifest_names_file_or_undeclared_output", func(t *testing.T) {
		f := newEscapeFixture(t, `echo ok > "$KODAMA_OUTPUTS_DIR/report.json"
printf '{"outcome":"completed","outputs":{"report":"/kodama/secrets/erp-token"}}' > "$KODAMA_RESULTS_FILE"`)
		assertRefused(t, f)
		g := newEscapeFixture(t, `echo ok > "$KODAMA_OUTPUTS_DIR/report.json"
printf '{"outcome":"completed","outputs":{"stowaway":"x"}}' > "$KODAMA_RESULTS_FILE"`)
		assertRefused(t, g)
	})

	t.Run("manifest_symlink_is_refused", func(t *testing.T) {
		f := newEscapeFixture(t, `echo ok > "$KODAMA_OUTPUTS_DIR/report.json"; echo '{"outcome":"completed","outputs":{}}' > "$KODAMA_OUTPUTS_DIR/m.json"; ln -s "$KODAMA_OUTPUTS_DIR/m.json" "$KODAMA_RESULTS_FILE"`)
		assertRefused(t, f)
	})

	// A declared path that escapes the outputs root (or names the secrets or
	// identity directory) is not a valid context: the entrypoint refuses to
	// start the command and reports nothing.
	t.Run("declared_path_outside_outputs_root_never_launches", func(t *testing.T) {
		for _, p := range []string{
			"/kodama/outputs/../secrets/erp-token",
			"/kodama/outputs/../identity/token",
			"/kodama/secrets/erp-token",
			"/kodama/identity/token",
			"/etc/passwd",
			"/kodama/outputs/./report.json",
		} {
			marker := filepath.Join(t.TempDir(), "ran")
			f := newEscapeFixture(t, `touch "$MARKER"`)
			f.srv.ctx.Command.Env["MARKER"] = marker
			// A hostile or buggy control plane: bypass the contract encoder.
			f.srv.Intercept(func(op string, n int, body []byte) action { return action{} })
			f.srv.with(func() {
				path := p
				f.srv.ctx.Outputs[0].Path = &path
			})
			code := f.run()
			f.wantExit(code, entrypoint.ExitAdmission)
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("%s: the command was launched", p)
			}
			if f.srv.Accepted() != nil {
				t.Fatalf("%s: a result was accepted", p)
			}
		}
	})

	t.Run("overlapping_secrets_and_outputs_layout_is_refused", func(t *testing.T) {
		f := newEscapeFixture(t, `exit 0`)
		f.cfg.SecretsDir = filepath.Join(f.cfg.OutputsDir, "secrets")
		f.wantExit(f.run(), entrypoint.ExitUsage)
		if f.srv.Count("admit") != 0 {
			t.Fatal("the entrypoint contacted the server with an unsafe layout")
		}
	})

	t.Run("secrets_never_reach_outputs_or_artifacts", func(t *testing.T) {
		// The command legitimately uses its secrets and produces a clean output.
		f := newEscapeFixture(t, `test "$(cat "$KODAMA_SECRETS_DIR/erp-token")" = "`+secretValue+`" && test "$DB_PASS" = "`+envSecret+`" && echo ok > "$KODAMA_OUTPUTS_DIR/report.json"`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		r := f.acceptedResult()
		if r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v\n%s", r, f.logs.String())
		}
		f.credentialsAbsent(secretValue, envSecret)
		if _, err := os.Stat(filepath.Join(f.cfg.SecretsDir, "erp-token")); err == nil {
			t.Fatal("secret file left behind after the step")
		}
		if got := f.srv.Count("resolveSecret"); got != 2 {
			t.Fatalf("resolved %d secrets, want exactly the 2 declared", got)
		}
	})
}
