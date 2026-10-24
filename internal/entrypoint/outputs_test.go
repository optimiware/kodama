package entrypoint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var oe *outputError
	if !errors.As(err, &oe) {
		t.Fatalf("expected *outputError, got %T %v", err, err)
	}
	return oe.reason
}

func TestOpenOutputFileRegular(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "report.json"), []byte("{}"), 0o644)
	f, fi, err := openOutputFile(root, "/kodama/outputs/sub/report.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi.Size() != 2 {
		t.Fatalf("size = %d", fi.Size())
	}
}

func TestOpenOutputFileRejections(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	os.WriteFile(filepath.Join(root, "ok"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(root, "dir"), 0o755)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "leaf-link"))
	os.Symlink(outside, filepath.Join(root, "dir-link"))
	os.Symlink("ok", filepath.Join(root, "relative-link"))
	os.Link(filepath.Join(outside, "secret"), filepath.Join(root, "hard"))
	if err := syscallMkfifo(filepath.Join(root, "pipe")); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, declared, reason string }{
		{"missing", "/kodama/outputs/absent", ReasonOutputMissing},
		{"missing parent", "/kodama/outputs/nodir/file", ReasonOutputMissing},
		{"dotdot escape", "/kodama/outputs/../outside", ReasonOutputInvalid},
		{"dotdot inside", "/kodama/outputs/dir/../ok", ReasonOutputInvalid},
		{"foreign prefix", "/etc/passwd", ReasonOutputInvalid},
		{"prefix lookalike", "/kodama/outputs-evil/x", ReasonOutputInvalid},
		{"root itself", "/kodama/outputs", ReasonOutputInvalid},
		{"leaf symlink", "/kodama/outputs/leaf-link", ReasonOutputInvalid},
		{"relative symlink inside root", "/kodama/outputs/relative-link", ReasonOutputInvalid},
		{"symlinked directory", "/kodama/outputs/dir-link/secret", ReasonOutputInvalid},
		{"directory", "/kodama/outputs/dir", ReasonOutputInvalid},
		{"hard link", "/kodama/outputs/hard", ReasonOutputInvalid},
		{"fifo", "/kodama/outputs/pipe", ReasonOutputInvalid},
		{"file as directory", "/kodama/outputs/ok/child", ReasonOutputInvalid},
		{"empty segment", "/kodama/outputs//ok", ReasonOutputInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _, err := openOutputFile(root, c.declared)
			if err == nil {
				f.Close()
				t.Fatalf("%s accepted", c.declared)
			}
			if got := reasonOf(t, err); got != c.reason {
				t.Fatalf("reason = %s, want %s (%v)", got, c.reason, err)
			}
		})
	}
}

func writeManifest(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadManifestMissingIsNil(t *testing.T) {
	m, err := readManifest(filepath.Join(t.TempDir(), "none.json"))
	if m != nil || err != nil {
		t.Fatalf("m=%v err=%v", m, err)
	}
}

func TestReadManifestValid(t *testing.T) {
	m, err := readManifest(writeManifest(t, `{"outcome":"completed","reason":null,"question":null,"outputs":{"n":3}}`))
	if err != nil || m == nil || *m.Outcome != "completed" || len(m.Outputs) != 1 {
		t.Fatalf("m=%+v err=%v", m, err)
	}
	m, err = readManifest(writeManifest(t, `{"outcome":"needs_input","question":"which env?"}`))
	if err != nil || *m.Question != "which env?" {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

func TestReadManifestStrict(t *testing.T) {
	bad := map[string]string{
		"unknown field":      `{"outcome":"completed","extra":1}`,
		"duplicate key":      `{"outcome":"completed","outcome":"failed"}`,
		"missing outcome":    `{"outputs":{}}`,
		"unknown outcome":    `{"outcome":"succeeded"}`,
		"float":              `{"outcome":"completed","outputs":{"n":1.5}}`,
		"completed+reason":   `{"outcome":"completed","reason":"x"}`,
		"failed+question":    `{"outcome":"failed","reason":"x","question":"q"}`,
		"failed+outputs":     `{"outcome":"failed","reason":"x","outputs":{"a":"b"}}`,
		"needs_input blank":  `{"outcome":"needs_input","question":"  "}`,
		"needs_input none":   `{"outcome":"needs_input"}`,
		"needs_input output": `{"outcome":"needs_input","question":"q","outputs":{"a":1}}`,
		"not json":           `outcome=completed`,
		"trailing data":      `{"outcome":"completed"} {}`,
		"array":              `[]`,
		"too deep":           strings.Repeat("[", 100),
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			m, err := readManifest(writeManifest(t, doc))
			if err == nil {
				t.Fatalf("accepted: %+v", m)
			}
			if got := reasonOf(t, err); got != ReasonManifestInvalid {
				t.Fatalf("reason = %s", got)
			}
		})
	}
}

func TestReadManifestRejectsOversizeAndNonRegular(t *testing.T) {
	big := writeManifest(t, `{"outcome":"completed","outputs":{"a":"`+strings.Repeat("a", maxManifestBytes)+`"}}`)
	if _, err := readManifest(big); err == nil || reasonOf(t, err) != ReasonManifestInvalid {
		t.Fatalf("oversize: %v", err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	os.WriteFile(target, []byte(`{"outcome":"completed"}`), 0o644)
	link := filepath.Join(dir, "results.json")
	os.Symlink(target, link)
	if _, err := readManifest(link); err == nil || reasonOf(t, err) != ReasonManifestInvalid {
		t.Fatalf("symlinked manifest: %v", err)
	}
}

func TestScalarValueTypes(t *testing.T) {
	ok := []struct {
		t   runtimev1.OutputType
		raw string
	}{
		{runtimev1.OutputString, `"x"`}, {runtimev1.OutputBoolean, `true`}, {runtimev1.OutputInteger, `-4`},
	}
	for _, c := range ok {
		if _, err := scalarValue("o", c.t, []byte(c.raw)); err != nil {
			t.Errorf("%s %s: %v", c.t, c.raw, err)
		}
	}
	bad := []struct {
		t   runtimev1.OutputType
		raw string
	}{
		{runtimev1.OutputString, `1`}, {runtimev1.OutputString, `true`}, {runtimev1.OutputBoolean, `"true"`},
		{runtimev1.OutputInteger, `"1"`}, {runtimev1.OutputInteger, `1.0`}, {runtimev1.OutputInteger, `9007199254740993`},
		{runtimev1.OutputString, `null`}, {runtimev1.OutputString, `{}`},
	}
	for _, c := range bad {
		_, err := scalarValue("o", c.t, []byte(c.raw))
		if err == nil || reasonOf(t, err) != ReasonOutputInvalid {
			t.Errorf("%s %s accepted (err=%v)", c.t, c.raw, err)
		}
	}
}

func TestOCIValue(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	v, err := ociValueOf("img", []byte(`{"digest":"`+digest+`","registryRef":"registry.example/app"}`))
	if err != nil || string(v.Digest) != digest {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	for _, raw := range []string{
		`"registry.example/app"`,
		`{"digest":"` + digest + `"}`,
		`{"digest":"sha256:short","registryRef":"r"}`,
		`{"digest":"` + digest + `","registryRef":"r","extra":1}`,
		`{"digest":"` + digest + `","registryRef":"has space"}`,
	} {
		if _, err := ociValueOf("img", []byte(raw)); err == nil || reasonOf(t, err) != ReasonOutputInvalid {
			t.Errorf("%s accepted (err=%v)", raw, err)
		}
	}
}

func TestCollectNeverCompletesWithMissingOrInvalidOutputs(t *testing.T) {
	outputs := t.TempDir()
	path := func(p string) *string { return &p }
	newAttempt := func(decls ...runtimev1.OutputDecl) (*attempt, *fakeRuntime) {
		rt := &fakeRuntime{}
		s := NewStep(Config{OutputsDir: outputs, ResultsFile: filepath.Join(t.TempDir(), "results.json")}, rt, nil)
		return &attempt{
			s: s, parent: context.Background(), runCtx: context.Background(),
			c:         runtimev1.Context{Outputs: decls, Limits: runtimev1.Limits{MaxOutputBytes: 1 << 20, MaxOutputFiles: 4}},
			artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}, names: map[string]bool{},
		}, rt
	}
	file := runtimev1.OutputDecl{Name: "report", Type: runtimev1.OutputArtifact, Path: path("/kodama/outputs/report.json")}
	scalar := runtimev1.OutputDecl{Name: "count", Type: runtimev1.OutputInteger}

	t.Run("declared file never written (exit zero)", func(t *testing.T) {
		a, _ := newAttempt(file)
		_, err := a.collect(nil)
		if reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("declared scalar without manifest", func(t *testing.T) {
		a, _ := newAttempt(scalar)
		_, err := a.collect(nil)
		if reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("declared scalar absent from manifest", func(t *testing.T) {
		a, _ := newAttempt(scalar)
		_, err := a.collect(&manifestDoc{Outputs: map[string]json.RawMessage{}})
		if reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("wrong scalar type", func(t *testing.T) {
		a, _ := newAttempt(scalar)
		_, err := a.collect(&manifestDoc{Outputs: map[string]json.RawMessage{"count": json.RawMessage(`"3"`)}})
		if reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("undeclared and file outputs in manifest", func(t *testing.T) {
		a, _ := newAttempt(scalar, file)
		for _, name := range []string{"extra", "report"} {
			_, err := a.collect(&manifestDoc{Outputs: map[string]json.RawMessage{"count": json.RawMessage(`1`), name: json.RawMessage(`"x"`)}})
			if reasonOf(t, err) != ReasonOutputInvalid {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
	})
	t.Run("oversize file is refused before any reservation", func(t *testing.T) {
		os.WriteFile(filepath.Join(outputs, "report.json"), make([]byte, 2<<20), 0o644)
		defer os.Remove(filepath.Join(outputs, "report.json"))
		a, rt := newAttempt(file)
		_, err := a.collect(nil)
		if reasonOf(t, err) != ReasonOutputInvalid || len(rt.resolved) != 0 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many files", func(t *testing.T) {
		var decls []runtimev1.OutputDecl
		for i := 0; i < 5; i++ {
			n := "f" + strconv.Itoa(i)
			decls = append(decls, runtimev1.OutputDecl{Name: n, Type: runtimev1.OutputArtifact, Path: path("/kodama/outputs/" + n)})
		}
		a, _ := newAttempt(decls...)
		if _, err := a.collect(nil); reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("symlink escape in declared path", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "secret")
		os.WriteFile(outside, []byte("s"), 0o644)
		os.Symlink(outside, filepath.Join(outputs, "report.json"))
		defer os.Remove(filepath.Join(outputs, "report.json"))
		a, _ := newAttempt(file)
		if _, err := a.collect(nil); reasonOf(t, err) != ReasonOutputInvalid {
			t.Fatalf("err = %v", err)
		}
	})
}
