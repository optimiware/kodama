package entrypoint_test

import (
	"os"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/entrypoint"
)

// Test_exit_zero_missing_output covers PX-10 and AR-06: a command that exits
// zero without producing the declared outputs never completes, its diagnostics
// stay readable, and nothing is published as output.
func Test_exit_zero_missing_output(t *testing.T) {
	t.Run("declared_file_not_produced", func(t *testing.T) {
		f := newFixture(t, `echo "all green"; exit 0`)
		f.declareFile("report", "/kodama/outputs/report.json")
		f.wantExit(f.run(), entrypoint.ExitOK)

		r := f.acceptedResult()
		f.wantFailed(r, "output_invalid")
		if r.ExitCode == nil || *r.ExitCode != 0 {
			t.Fatalf("exit code = %v, want the observed 0", r.ExitCode)
		}
		diag := f.diagnostics(r)
		if !strings.Contains(diag["command.log"], "all green") {
			t.Fatalf("command log not retained: %v", diag)
		}
		if !strings.Contains(diag["entrypoint.log"], "report.json") {
			t.Fatalf("the entrypoint did not explain the failure: %v", diag)
		}
		if outs := f.srv.Artifacts(runtimev1.RoleOutput); len(outs) != 0 {
			t.Fatalf("output artifacts were reserved: %v", outs)
		}
	})

	t.Run("declared_scalar_without_manifest", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		f.declare("count", runtimev1.OutputInteger)
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "output_invalid")
	})

	t.Run("manifest_lacks_declared_output", func(t *testing.T) {
		f := newFixture(t, `printf '{"outcome":"completed","outputs":{}}' > "$KODAMA_RESULTS_FILE"`)
		f.declare("count", runtimev1.OutputInteger)
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "output_invalid")
	})

	t.Run("wrong_type_and_invalid_manifest", func(t *testing.T) {
		for name, manifest := range map[string]string{
			"wrong type":      `{"outcome":"completed","outputs":{"count":"3"}}`,
			"float":           `{"outcome":"completed","outputs":{"count":1.5}}`,
			"not json":        `exit zero but {not json`,
			"unknown outcome": `{"outcome":"great","outputs":{"count":1}}`,
			"duplicate key":   `{"outcome":"completed","outputs":{"count":1,"count":2}}`,
		} {
			f := newFixture(t, `cat > "$KODAMA_RESULTS_FILE" <<'EOF'`+"\n"+manifest+"\nEOF\n")
			f.declare("count", runtimev1.OutputInteger)
			f.wantExit(f.run(), entrypoint.ExitOK)
			r := f.acceptedResult()
			if r.Outcome == runtimev1.OutcomeCompleted {
				t.Fatalf("%s: completed with an invalid manifest", name)
			}
			f.wantFailed(r, "output_invalid")
		}
	})

	t.Run("failure_keeps_log_and_exit_code", func(t *testing.T) {
		f := newFixture(t, `echo "boom: tests failed" >&2; exit 3`)
		f.declareFile("report", "/kodama/outputs/report.json")
		f.wantExit(f.run(), entrypoint.ExitOK)
		r := f.acceptedResult()
		f.wantFailed(r, entrypoint.ReasonExitNonzero)
		if r.ExitCode == nil || *r.ExitCode != 3 {
			t.Fatalf("exit code = %v", r.ExitCode)
		}
		if !strings.Contains(f.diagnostics(r)["command.log"], "boom: tests failed") {
			t.Fatal("failure log not retained")
		}
	})

	t.Run("command_reported_failure_and_needs_input", func(t *testing.T) {
		f := newFixture(t, `printf '{"outcome":"failed","reason":"lint_errors"}' > "$KODAMA_RESULTS_FILE"`)
		f.declare("count", runtimev1.OutputInteger)
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "lint_errors")

		g := newFixture(t, `printf '{"outcome":"needs_input","question":"Which database?"}' > "$KODAMA_RESULTS_FILE"`)
		g.declare("count", runtimev1.OutputInteger)
		g.wantExit(g.run(), entrypoint.ExitOK)
		r := g.acceptedResult()
		if r.Outcome != runtimev1.OutcomeNeedsInput || r.Question == nil || *r.Question != "Which database?" || len(r.Outputs) != 0 {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("control_produced_outputs_complete", func(t *testing.T) {
		// Not vacuous: the same step completes when every declared output exists.
		f := newFixture(t, `echo '{"ok":true}' > "$KODAMA_OUTPUTS_DIR/report.json"
printf '{"outcome":"completed","outputs":{"count":7,"label":"x","flag":true}}' > "$KODAMA_RESULTS_FILE"`)
		f.declareFile("report", "/kodama/outputs/report.json")
		f.declare("count", runtimev1.OutputInteger)
		f.declare("label", runtimev1.OutputString)
		f.declare("flag", runtimev1.OutputBoolean)
		f.wantExit(f.run(), entrypoint.ExitOK)
		r := f.acceptedResult()
		if r.Outcome != runtimev1.OutcomeCompleted || r.ExitCode == nil || *r.ExitCode != 0 || r.Reason != nil || r.SourceCommit != nil {
			t.Fatalf("result = %+v", r)
		}
		if len(r.Outputs) != 4 || !r.Outputs["count"].IsInteger() || !r.Outputs["flag"].IsBool() || !r.Outputs["label"].IsString() {
			t.Fatalf("outputs = %+v", r.Outputs)
		}
		outs := f.srv.Artifacts(runtimev1.RoleOutput)
		if len(outs) != 1 || !outs[0].usable || string(outs[0].bytes) != "{\"ok\":true}\n" || outs[0].req.Name != "report.json" {
			t.Fatalf("output artifacts = %+v", outs)
		}
		if string(r.Outputs["report"].String()) != string(outs[0].id) {
			t.Fatalf("output report = %v, artifact %s", r.Outputs["report"], outs[0].id)
		}
	})

	t.Run("oversize_output_is_refused_before_upload", func(t *testing.T) {
		f := newFixture(t, `head -c 2000000 /dev/zero > "$KODAMA_OUTPUTS_DIR/big.bin"`)
		f.declareFile("big", "/kodama/outputs/big.bin")
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "output_invalid")
		if outs := f.srv.Artifacts(runtimev1.RoleOutput); len(outs) != 0 {
			t.Fatalf("an oversize output was reserved: %+v", outs)
		}
	})

	t.Run("stale_manifest_from_before_launch_is_ignored", func(t *testing.T) {
		f := newFixture(t, `exit 0`)
		f.declare("count", runtimev1.OutputInteger)
		if err := os.WriteFile(f.cfg.ResultsFile, []byte(`{"outcome":"completed","outputs":{"count":1}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		f.wantExit(f.run(), entrypoint.ExitOK)
		f.wantFailed(f.acceptedResult(), "output_invalid")
	})
}
