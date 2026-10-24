package entrypoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func outputReservations(u *unit) []runtimev1.ArtifactReserveRequest {
	var out []runtimev1.ArtifactReserveRequest
	for _, q := range u.rt.reserved {
		if q.Role == runtimev1.RoleOutput {
			out = append(out, q)
		}
	}
	return out
}

func Test_artifact_output_name_is_declared_name(t *testing.T) {
	u := newUnit(t, `echo '{"ok":true}' > "$KODAMA_OUTPUTS_DIR/quality.json"`)
	u.rt.ctx.Outputs = []runtimev1.OutputDecl{{Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/quality.json")}}
	u.wantExit(u.run(), ExitOK)
	r := u.lastResult()
	if r.Outcome != runtimev1.OutcomeCompleted {
		t.Fatalf("result = %+v\nlogs:\n%s", r, u.logs.String())
	}
	got := outputReservations(u)
	if len(got) != 1 || got[0].Name != "report" || got[0].Kind != runtimev1.KindFile {
		t.Fatalf("output reservations = %+v", got)
	}
	id, ok := r.Outputs["report"]
	if !ok || !id.IsString() {
		t.Fatalf("outputs = %+v", r.Outputs)
	}
	var idStr string
	if err := json.Unmarshal(mustJSON(t, id), &idStr); err != nil {
		t.Fatal(err)
	}
	if body := u.up.object(domain.ArtifactID(idStr)); string(body) != "{\"ok\":true}\n" {
		t.Fatalf("finalized object = %q", body)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func Test_artifact_output_path_basename_free(t *testing.T) {
	t.Run("same base name in different directories", func(t *testing.T) {
		u := newUnit(t, `mkdir -p "$KODAMA_OUTPUTS_DIR/a" "$KODAMA_OUTPUTS_DIR/b"
echo one > "$KODAMA_OUTPUTS_DIR/a/report.json"
echo two > "$KODAMA_OUTPUTS_DIR/b/report.json"`)
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{
			{Name: "first", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/a/report.json")},
			{Name: "second", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/b/report.json")},
		}
		u.wantExit(u.run(), ExitOK)
		r := u.lastResult()
		if r.Outcome != runtimev1.OutcomeCompleted || len(r.Outputs) != 2 {
			t.Fatalf("result = %+v\nlogs:\n%s", r, u.logs.String())
		}
		got := outputReservations(u)
		if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
			t.Fatalf("output reservations = %+v", got)
		}
	})
	t.Run("declared name collision is refused before any transfer", func(t *testing.T) {
		u := newUnit(t, `echo one > "$KODAMA_OUTPUTS_DIR/a.json"; echo two > "$KODAMA_OUTPUTS_DIR/b.json"`)
		u.rt.ctx.Outputs = []runtimev1.OutputDecl{
			{Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/a.json")},
			{Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/b.json")},
		}
		// The context contract already refuses a repeated declared name, so the
		// command never launches and nothing is reserved.
		u.wantExit(u.run(), ExitAdmission)
		if got := outputReservations(u); len(got) != 0 {
			t.Fatalf("output artifacts reserved: %+v", got)
		}
	})
	t.Run("collect refuses a repeated output name before any transfer", func(t *testing.T) {
		outputs := t.TempDir()
		for _, f := range []string{"a.json", "b.json"} {
			if err := os.WriteFile(filepath.Join(outputs, f), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rt := &fakeRuntime{}
		a := &attempt{
			s: NewStep(Config{OutputsDir: outputs, ResultsFile: filepath.Join(t.TempDir(), "results.json")}, rt, nil),
			c: runtimev1.Context{Outputs: []runtimev1.OutputDecl{
				{Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/a.json")},
				{Name: "report", Type: runtimev1.OutputArtifact, Path: ptr("/kodama/outputs/b.json")},
			}, Limits: runtimev1.Limits{MaxOutputBytes: 1 << 20, MaxOutputFiles: 4}},
			parent: context.Background(), runCtx: context.Background(),
			artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}, names: map[string]bool{},
		}
		if _, err := a.collect(nil); err == nil || reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("artifactName reserves each declared name once", func(t *testing.T) {
		taken := map[string]bool{}
		if n, err := artifactName("report", taken); err != nil || n != "report" {
			t.Fatalf("name = %q err = %v", n, err)
		}
		if _, err := artifactName("report", taken); err == nil || reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("duplicate accepted: %v", err)
		}
		if _, err := artifactName("bad name", taken); err == nil || reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("unsafe name accepted: %v", err)
		}
	})
}
