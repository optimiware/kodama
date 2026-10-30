package pipeline_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const fixturesV3 = "../../contracts/pipeline/v1alpha3/fixtures"

func readFixtureV3(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesV3, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newEnvV3 is a validator environment with kodama/v1alpha3 enabled and
// kodama/v1alpha2 left at its default (off): the two flags are independent.
func newEnvV3(t testing.TB) *env {
	t.Helper()
	e := newEnv(t)
	v, err := pipeline.NewValidator(pipeline.Config{Capabilities: e.caps, Profiles: e.profiles, References: e.refs, EnableV1Alpha3: true})
	if err != nil {
		t.Fatal(err)
	}
	e.v = v
	return e
}

// v3Base is a valid kodama/v1alpha3 document with a writer, a reader and two
// sources; every invalid-document case mutates it by one path.
const v3Base = `{
 "apiVersion": "kodama/v1alpha3", "kind": "Pipeline", "metadata": {"name": "base"},
 "spec": {
  "timeoutSeconds": 1800,
  "sources": [
   {"repository": "backend", "path": "backend", "access": "write"},
   {"repository": "frontend", "path": "frontend", "access": "read"}
  ],
  "steps": [
   {"id": "build", "kind": "container", "image": "registry.example.invalid/tools@` + v2Digest1 + `",
    "command": ["/bin/sh", "-ec"], "args": ["true"], "timeoutSeconds": 600, "workspace": {"access": "write"}},
   {"id": "check", "kind": "container", "needs": ["build"], "image": "registry.example.invalid/tools@` + v2Digest1 + `",
    "command": ["/bin/sh", "-ec"], "args": ["true"], "timeoutSeconds": 600, "workspace": {"access": "read"}}
  ]
 }
}`

func mutateV3(t testing.TB, edits ...any) []byte { return mutateDoc(t, v3Base, edits...) }

var v3Goldens = []string{"service-test", "two-repositories", "read-only-sources", "single-root-source", "agent-gate-sources", "default-repository"}

func src(repository, path, access string) map[string]any {
	return map[string]any{"repository": repository, "path": path, "access": access}
}

// Test_unit_pipeline_sources_declaration: the kodama/v1alpha3 format declares a
// run's source set as spec.sources: names, safe unique non-overlapping paths
// and accesses, with a document that omits them equivalent to one default
// repository at ".". The format is off by default and independent of
// kodama/v1alpha2; every rejection has a stable code, path and stage and echoes
// no document content. PX-23, GR-06.
func Test_unit_pipeline_sources_declaration(t *testing.T) {
	t.Run("goldens: canonical bytes, digest, stored round trip", func(t *testing.T) {
		e := newEnvV3(t)
		for _, name := range v3Goldens {
			t.Run(name, func(t *testing.T) {
				v := e.mustValidate(t, string(readFixtureV3(t, "valid/"+name+".yaml")))
				canonPath := filepath.Join(fixturesV3, "canonical", name+".json")
				digestPath := filepath.Join(fixturesV3, "canonical", name+".digest")
				if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
					if err := os.WriteFile(canonPath, v.Canonical(), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(digestPath, []byte(v.Digest()+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want := readFixtureV3(t, "canonical/"+name+".json")
				if !bytes.Equal(v.Canonical(), want) {
					t.Fatalf("canonical bytes changed\n got: %s\nwant: %s", v.Canonical(), want)
				}
				wantDigest := strings.TrimSpace(string(readFixtureV3(t, "canonical/"+name+".digest")))
				if string(v.Digest()) != wantDigest || pipeline.DigestOf(want) != v.Digest() {
					t.Fatalf("digest %s, want %s", v.Digest(), wantDigest)
				}
				stored, err := pipeline.NewValidatedFromCanonical(want, pipeline.Digest(wantDigest))
				if err != nil {
					t.Fatalf("stored content rejected: %v", err)
				}
				doc, err := stored.Document()
				if err != nil || !doc.IsV1Alpha3() || !doc.IsRunShared() || doc.IsV1Alpha2() {
					t.Fatalf("stored document: %v %v", doc.APIVersion, err)
				}
				if !strings.HasPrefix(string(want), `{"apiVersion":"kodama/v1alpha3","kind":"Pipeline"`) || strings.ContainsAny(string(want), "\n\t") {
					t.Fatalf("canonical form is not sorted and compact: %s", want)
				}
			})
		}
	})

	t.Run("reference vector: sources are the only addition to kodama/v1alpha2", func(t *testing.T) {
		e := newEnvV3(t)
		v := e.mustValidate(t, string(readFixtureV3(t, "valid/service-test.yaml")))
		// Derived independently with Python json/hashlib from the published
		// kodama/v1alpha2 vector with the apiVersion replaced.
		const vector = "sha256:4ac7d72393f64a79ae72d17bdfe1e8962c4ac5b521b6c1cf369e5f9b938267c2"
		if string(v.Digest()) != vector {
			t.Fatalf("digest %s, want the independently derived %s", v.Digest(), vector)
		}
		v2canon, err := os.ReadFile(filepath.Join(fixturesV2, "canonical", "service-test.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Replace(string(v2canon), "kodama/v1alpha2", "kodama/v1alpha3", 1); got != string(v.Canonical()) {
			t.Fatalf("without sources the canonical form is the v1alpha2 form\n got: %s\nwant: %s", v.Canonical(), got)
		}
		asJSON := e.mustValidateJSON(t, readFixtureV3(t, "valid/service-test.json"))
		if asJSON.Digest() != v.Digest() {
			t.Fatal("YAML and JSON spellings must converge")
		}
	})

	t.Run("sources are canonical in declaration order and never filled", func(t *testing.T) {
		e := newEnvV3(t)
		two := e.mustValidateJSON(t, mutateV3(t))
		c := string(two.Canonical())
		if !strings.Contains(c, `"sources":[{"access":"write","path":"backend","repository":"backend"},{"access":"read","path":"frontend","repository":"frontend"}]`) {
			t.Fatalf("sources missing or reordered: %s", c)
		}
		swapped := e.mustValidateJSON(t, mutateV3(t, "spec.sources", []any{src("frontend", "frontend", "read"), src("backend", "backend", "write")}))
		if swapped.Digest() == two.Digest() {
			t.Fatal("declaration order is part of the canonical form")
		}
		none := e.mustValidateJSON(t, mutateV3(t, "spec.sources", nil, "spec.steps.0.workspace", nil))
		if strings.Contains(string(none.Canonical()), "sources") {
			t.Fatalf("an absent sources is not filled with the default source: %s", none.Canonical())
		}
	})

	t.Run("a document without sources is one default repository at the root", func(t *testing.T) {
		e := newEnvV3(t)
		doc := func(t *testing.T, name string) pipeline.Document {
			v := e.mustValidate(t, string(readFixtureV3(t, "valid/"+name+".yaml")))
			d, err := v.Document()
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
		got, implicit := pipeline.EffectiveSources(doc(t, "default-repository"))
		if !implicit || len(got) != 1 || got[0] != (pipeline.Source{Path: ".", Access: pipeline.SourceWrite}) {
			t.Fatalf("a writer without sources: %+v %v", got, implicit)
		}
		got, implicit = pipeline.EffectiveSources(doc(t, "service-test"))
		if !implicit || len(got) != 1 || got[0].Path != "." || got[0].Access != pipeline.SourceRead || got[0].Repository != "" {
			t.Fatalf("a reader without sources: %+v %v", got, implicit)
		}
		got, implicit = pipeline.EffectiveSources(doc(t, "two-repositories"))
		if implicit || len(got) != 2 || got[0].Repository != "backend" || got[1].Access != pipeline.SourceRead {
			t.Fatalf("declared sources: %+v %v", got, implicit)
		}
		got[0].Path = "mutated"
		if again, _ := pipeline.EffectiveSources(doc(t, "two-repositories")); again[0].Path != "backend" {
			t.Fatal("EffectiveSources must copy")
		}
	})

	t.Run("a declared root source is the single-repository case", func(t *testing.T) {
		e := newEnvV3(t)
		e.mustValidate(t, string(readFixtureV3(t, "valid/single-root-source.yaml")))
		explicit := e.mustValidateJSON(t, mutateV3(t, "spec.sources", []any{src("inventory", ".", "write")}))
		d, _ := explicit.Document()
		if got, implicit := pipeline.EffectiveSources(d); implicit || len(got) != 1 || got[0].Path != "." {
			t.Fatalf("%+v %v", got, implicit)
		}
	})

	t.Run("the format is default off and independent of kodama/v1alpha2", func(t *testing.T) {
		off := newEnv(t)
		if off.v.FormatEnabled(pipelinev1alpha3.APIVersion) || !off.v.FormatEnabled("kodama/v1alpha1") {
			t.Fatal("the default validator enables kodama/v1alpha1 only")
		}
		raw := readFixtureV3(t, "valid/two-repositories.yaml")
		_, err := off.v.ValidateYAML(t.Context(), scope, actor, raw)
		expectCode(t, err, pipeline.CodeFormatDisabled)
		if stageOf(err) != pipeline.StageSemantic || len(codes(err)) != 1 || off.refs.calls != 0 || off.profiles.calls != 0 {
			t.Fatalf("format_disabled must be the single semantic finding and run no lookup: %v", err)
		}
		// v1alpha2 on, v1alpha3 off: still refused.
		two := newEnvV2(t)
		if two.v.FormatEnabled(pipelinev1alpha3.APIVersion) {
			t.Fatal("enabling kodama/v1alpha2 must not enable kodama/v1alpha3")
		}
		_, err = two.v.ValidateYAML(t.Context(), scope, actor, raw)
		expectCode(t, err, pipeline.CodeFormatDisabled)
		// v1alpha3 on, v1alpha2 off: v1alpha2 is still refused.
		three := newEnvV3(t)
		if three.v.FormatEnabled("kodama/v1alpha2") || !three.v.FormatEnabled(pipelinev1alpha3.APIVersion) {
			t.Fatal("the flags are independent")
		}
		_, err = three.v.ValidateYAML(t.Context(), scope, actor, readFixtureV2(t, "valid/service-test.yaml"))
		expectCode(t, err, pipeline.CodeFormatDisabled)
		// Reading is independent of the flag: a disabled validator still parses.
		if _, err := off.v.ParseYAML(raw); err != nil {
			t.Fatalf("a disabled validator must still parse: %v", err)
		}
		// A syntactically invalid document is not hidden behind the flag.
		_, err = off.validateJSON(mutateV3(t, "spec.sources.0.commit", "1111"))
		expectCode(t, err, pipeline.CodeUnknownField)
	})

	t.Run("registration needs the opt-in and dedupes on the canonical form", func(t *testing.T) {
		raw := readFixtureV3(t, "valid/two-repositories.yaml")
		build := func(e *env) (*pipeline.Service, *pipelinetest.MemRepo) {
			repo := &pipelinetest.MemRepo{}
			svc, err := pipeline.NewService(pipeline.ServiceConfig{Repository: repo, Authority: newAuthority(), Validator: e.v})
			if err != nil {
				t.Fatal(err)
			}
			return svc, repo
		}
		register := func(svc *pipeline.Service, format pipeline.Format, body []byte) (pipeline.RegisterResult, error) {
			return svc.Register(t.Context(), pipeline.RegisterCommand{Scope: scope, Caller: pipeline.Caller{PrincipalID: "u_engineer"}, Format: format, Body: body})
		}
		off, offRepo := build(newEnv(t))
		_, err := register(off, pipeline.FormatYAML, raw)
		expectCode(t, err, pipeline.CodeFormatDisabled)
		if defs, _ := offRepo.List(t.Context(), scope, pipeline.ListQuery{Limit: 10}); len(defs) != 0 {
			t.Fatal("a refused format must persist nothing")
		}
		on, _ := build(newEnvV3(t))
		first, err := register(on, pipeline.FormatYAML, raw)
		if err != nil || !first.Created {
			t.Fatalf("opted-in registration: %+v %v", first, err)
		}
		again, err := register(on, pipeline.FormatJSON, first.Definition.Content.Canonical())
		if err != nil || again.Created || again.Definition.ID != first.Definition.ID {
			t.Fatalf("the canonical form must dedupe: %+v %v", again, err)
		}
	})

	t.Run("earlier formats never accept sources", func(t *testing.T) {
		both := newEnvV2(t)
		_, err := both.validateJSON(mutateDoc(t, v2Base, "spec.sources", []any{src("backend", ".", "read")}))
		expectCode(t, err, pipeline.CodeUnknownField)
		if !hasDiag(err, pipeline.CodeUnknownField, "$.spec.sources") {
			t.Fatalf("unknown_field must locate spec.sources: %v", err)
		}
		_, err = both.validate(strings.Replace(doc(container("a", "workspace: {access: read, from: run}")), "spec:\n", "spec:\n  sources: [{repository: a, path: ., access: read}]\n", 1))
		expectCode(t, err, pipeline.CodeUnknownField)

		// A typed document of an earlier version carrying sources is refused, not
		// silently reduced.
		v := both.mustValidateJSON(t, mutateDoc(t, v2Base))
		typed, _ := v.Document()
		typed.Spec.Sources = []pipeline.Source{{Repository: "backend", Path: ".", Access: pipeline.SourceRead}}
		_, err = both.v.Validate(t.Context(), scope, actor, typed)
		expectCode(t, err, pipeline.CodeUnknownField)
		v1 := newEnv(t).mustValidate(t, doc(container("a")))
		typed1, _ := v1.Document()
		typed1.Spec.Sources = []pipeline.Source{{Repository: "backend", Path: ".", Access: pipeline.SourceRead}}
		_, err = both.v.Validate(t.Context(), scope, actor, typed1)
		expectCode(t, err, pipeline.CodeUnknownField)
	})

	t.Run("a typed kodama/v1alpha3 document goes through every rule", func(t *testing.T) {
		e := newEnvV3(t)
		v := e.mustValidateJSON(t, mutateV3(t))
		typed, _ := v.Document()
		typed.Spec.Sources = []pipeline.Source{{Repository: "Backend", Path: "../x", Access: "admin"}}
		_, err := e.v.Validate(t.Context(), scope, actor, typed)
		for _, code := range []string{pipeline.CodeInvalidFormat, pipeline.CodeInvalidSourcePath, pipeline.CodeInvalidValue} {
			if !hasCode(err, code) {
				t.Errorf("typed document skipped %s: %v", code, codes(err))
			}
		}
		typed.Spec.Sources = []pipeline.Source{{Repository: "a", Path: "a", Access: pipeline.SourceWrite}, {Repository: "a", Path: "b", Access: pipeline.SourceRead}}
		_, err = e.v.Validate(t.Context(), scope, actor, typed)
		expectCode(t, err, pipeline.CodeDuplicateSourceRepository)
		typed.Spec.Sources = []pipeline.Source{{Repository: "a", Path: "a", Access: pipeline.SourceWrite}, {Repository: "b", Path: "a/b", Access: pipeline.SourceRead}}
		_, err = e.v.Validate(t.Context(), scope, actor, typed)
		expectCode(t, err, pipeline.CodeSourcePathOverlap)
		typed.Spec.Sources = make([]pipeline.Source, 33)
		for i := range typed.Spec.Sources {
			typed.Spec.Sources[i] = pipeline.Source{Repository: "r" + strings.Repeat("x", i), Path: "p" + strings.Repeat("x", i), Access: pipeline.SourceRead}
		}
		_, err = e.v.Validate(t.Context(), scope, actor, typed)
		expectCode(t, err, pipeline.CodeLimitExceeded)
	})

	t.Run("invalid documents have stable codes, paths and stages", func(t *testing.T) {
		e := newEnvV3(t)
		thirtyThree := make([]any, 33)
		for i := range thirtyThree {
			thirtyThree[i] = src("r"+strings.Repeat("x", i), "p"+strings.Repeat("x", i), "read")
		}
		thirtyTwo := make([]any, 32)
		for i := range thirtyTwo {
			thirtyTwo[i] = src("r"+strings.Repeat("x", i), "p"+strings.Repeat("x", i), "read")
		}
		cases := []invalidCase{
			// Valid controls.
			{name: "control: thirty-two sources", edits: []any{"spec.sources", thirtyTwo, "spec.steps.0.workspace", map[string]any{"access": "read"}}},
			{name: "control: nested names that merely share a prefix", edits: []any{"spec.sources", []any{src("a", "app", "write"), src("b", "app2", "read"), src("c", "app-two", "read")}}},
			{name: "control: deep path", edits: []any{"spec.sources", []any{src("a", "services/payments/api", "write")}}},
			{name: "control: underscore component", edits: []any{"spec.sources", []any{src("a", "_vendor", "write")}}},

			// Shape: the list.
			sx("empty sources", pipeline.CodeInvalidValue, "$.spec.sources", "spec.sources", []any{}),
			sx("sources not a list", pipeline.CodeInvalidType, "$.spec.sources", "spec.sources", map[string]any{}),
			sx("thirty-three sources", pipeline.CodeLimitExceeded, "$.spec.sources", "spec.sources", thirtyThree),
			sx("source is not an object", pipeline.CodeInvalidType, "sources[0]", "spec.sources.0", "backend"),

			// Shape: members.
			sx("source without repository", pipeline.CodeMissingField, "sources[0].repository", "spec.sources.0.repository", nil),
			sx("source without path", pipeline.CodeMissingField, "sources[0].path", "spec.sources.0.path", nil),
			sx("source without access", pipeline.CodeMissingField, "sources[0].access", "spec.sources.0.access", nil),
			sx("repository with uppercase", pipeline.CodeInvalidFormat, "sources[0].repository", "spec.sources.0.repository", "Backend"),
			sx("repository starting with a digit", pipeline.CodeInvalidFormat, "sources[0].repository", "spec.sources.0.repository", "1backend"),
			sx("repository with a slash", pipeline.CodeInvalidFormat, "sources[0].repository", "spec.sources.0.repository", "acme/backend"),
			sx("repository too long", pipeline.CodeInvalidFormat, "sources[0].repository", "spec.sources.0.repository", "a"+strings.Repeat("b", 63)),
			sx("repository as a number", pipeline.CodeInvalidType, "sources[0].repository", "spec.sources.0.repository", 7),
			sx("access admin", pipeline.CodeInvalidValue, "sources[0].access", "spec.sources.0.access", "admin"),
			sx("access none", pipeline.CodeInvalidValue, "sources[0].access", "spec.sources.0.access", "none"),
			sx("access upper case", pipeline.CodeInvalidValue, "sources[0].access", "spec.sources.0.access", "Write"),
			sx("source commit is not a document field", pipeline.CodeUnknownField, "sources[0].commit", "spec.sources.0.commit", "1111111111111111111111111111111111111111"),
			sx("source branch is not a document field", pipeline.CodeUnknownField, "sources[0].branch", "spec.sources.0.branch", "refs/heads/x"),
			sx("source ref is not a document field", pipeline.CodeUnknownField, "sources[0].ref", "spec.sources.0.ref", "main"),
			sx("source url is not a document field", pipeline.CodeUnknownField, "sources[0].url", "spec.sources.0.url", "https://git.example.invalid/x.git"),
			sx("source repository id is not a document field", pipeline.CodeUnknownField, "sources[0].repositoryId", "spec.sources.0.repositoryId", "repo_01"),

			// Shape: paths.
			sx("absolute path", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "/backend"),
			sx("parent traversal", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "../backend"),
			sx("inner parent traversal", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a/../b"),
			sx("dot-dot alone", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", ".."),
			sx("dot git", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", ".git"),
			sx("nested dot git", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a/.git"),
			sx("hidden component", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", ".hidden"),
			sx("empty component", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a//b"),
			sx("trailing slash", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a/"),
			sx("dot component", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a/./b"),
			sx("dot slash prefix", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "./a"),
			sx("backslash", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a\\b"),
			sx("space", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "a b"),
			sx("empty path", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", ""),
			sx("dash first", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", "-rf"),
			sx("path too long", pipeline.CodeInvalidSourcePath, "sources[0].path", "spec.sources.0.path", strings.Repeat("a", 256)),
			sx("path not a string", pipeline.CodeInvalidType, "sources[0].path", "spec.sources.0.path", 5),

			// Shape: relations between sources.
			sx("duplicate path", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources.1.path", "backend"),
			sx("nested path", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources.1.path", "backend/web"),
			sx("enclosing path", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources", []any{src("backend", "a/b", "write"), src("frontend", "a", "read")}),
			sx("root next to another source", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources", []any{src("backend", ".", "write"), src("frontend", "web", "read")}),
			sx("another source before the root", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources", []any{src("backend", "web", "write"), src("frontend", ".", "read")}),
			sx("two roots", pipeline.CodeSourcePathOverlap, "sources[1].path", "spec.sources", []any{src("backend", ".", "write"), src("frontend", ".", "read")}),
			sx("duplicate repository", pipeline.CodeDuplicateSourceRepository, "sources[1].repository", "spec.sources.1.repository", "backend"),

			// Semantic: sources and writers.
			sm("a writer without a write source", pipeline.CodeSourceWriteUndeclared, "steps[0].workspace.access", "spec.sources.0.access", "read"),
			sm("a write source without a writer", pipeline.CodeSourceWriteUnused, "sources[0].access", "spec.steps.0.workspace", map[string]any{"access": "read"}),
			sm("a write source and steps with no workspace", pipeline.CodeSourceWriteUnused, "sources[0].access", "spec.steps.0.workspace", nil),
			sm("parallel writers stay forbidden", pipeline.CodeParallelWriters, "steps[1]", "spec.steps.1.needs", []any{}, "spec.steps.1.workspace", map[string]any{"access": "write"}),
			sx("workspace.from stays removed", pipeline.CodeWorkspaceFromRemoved, "steps[0].workspace.from", "spec.steps.0.workspace", map[string]any{"access": "write", "from": "run"}),
		}
		sentinels := []string{"backend", "frontend", "registry.example.invalid", "1backend", "acme", "../", "refs/heads"}
		type publishedCase struct {
			Name        string `json:"name"`
			Format      string `json:"format"`
			SchemaValid bool   `json:"schemaValid"`
			Stage       string `json:"stage"`
			Code        string `json:"code"`
			Path        string `json:"path"`
			Document    string `json:"document"`
		}
		var published []publishedCase
		sch, err := pipelinev1alpha3.Schema()
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range cases {
			if seen[c.name] {
				t.Fatalf("duplicate case %q", c.name)
			}
			seen[c.name] = true
			t.Run(c.name, func(t *testing.T) {
				raw := mutateV3(t, c.edits...)
				_, err := e.validateJSON(raw)
				if c.code == "" {
					if err != nil {
						t.Fatalf("control must be valid: %v", err)
					}
					return
				}
				inst, jerr := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
				if jerr != nil {
					t.Fatal(jerr)
				}
				published = append(published, publishedCase{Name: c.name, Format: "json", SchemaValid: sch.Validate(inst) == nil, Stage: string(c.stage), Code: c.code, Path: c.path, Document: string(raw)})
				expectCode(t, err, c.code)
				if !hasDiag(err, c.code, c.path) {
					t.Fatalf("code %s is not reported at a path containing %q: %v", c.code, c.path, err.(*pipeline.ValidationError).Diagnostics)
				}
				if got := stageOf(err); got != c.stage {
					t.Fatalf("stage %s, want %s", got, c.stage)
				}
				for _, d := range err.(*pipeline.ValidationError).Diagnostics {
					if !strings.HasPrefix(d.Path, "$") || d.Message == "" {
						t.Fatalf("malformed diagnostic %+v", d)
					}
					for _, s := range sentinels {
						if strings.Contains(d.Message, s) {
							t.Fatalf("message %q echoes document content %q", d.Message, s)
						}
					}
				}
			})
		}
		body, err := json.MarshalIndent(published, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, '\n')
		golden := filepath.Join(fixturesV3, "cases.json")
		if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(golden, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if want := readFixtureV3(t, "cases.json"); !bytes.Equal(want, body) {
			t.Fatal("contracts/pipeline/v1alpha3/fixtures/cases.json is out of date; regenerate with KODAMA_UPDATE_GOLDEN=1")
		}
	})
}

// frozenTrees are the SHA-256 of every frozen contract file of an earlier
// format (the relative path and the content digest of each file, in path
// order). A change to any schema, valid document, canonical golden, digest or
// rejected case of kodama/v1alpha1 or kodama/v1alpha2 changes the value.
var frozenTrees = map[string]string{
	"../../contracts/pipeline/v1":       "ddff858d9a99dd85a736c8ed1cc60c3bb58366f5a25e6528a52020cd4839a102",
	"../../contracts/pipeline/v1alpha2": "667d91efc3e68f4a7eaa52fa98244ad152b7d999acc4757bfcc157879b111098",
}

func treeDigest(t testing.TB, root string, include func(rel string) bool) string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if include(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		h.Write([]byte(rel + "\x00" + hex.EncodeToString(sum[:]) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Test_unit_pipeline_old_formats_unchanged: adding kodama/v1alpha3 changes
// nothing of kodama/v1alpha1 or kodama/v1alpha2: their schemas, fixtures,
// canonical bytes and digests are byte-identical, every golden still validates
// to the same bytes and digest with every format enabled, and a document of one
// version is never judged by another version's rules. PX-21.
func Test_unit_pipeline_old_formats_unchanged(t *testing.T) {
	isGoSource := func(rel string) bool { return !strings.HasSuffix(rel, ".go") }
	for root, want := range frozenTrees {
		if got := treeDigest(t, root, isGoSource); got != want {
			t.Errorf("%s: the frozen contract files changed (tree digest %s)", root, got)
		}
	}

	base := newEnv(t)
	base.refs.secrets["test-db-password"] = true
	all, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: base.caps, Profiles: base.profiles, References: base.refs,
		EnableV1Alpha2: true, EnableV1Alpha3: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, dir, name string) {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(dir, "valid", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		v, err := all.ValidateYAML(t.Context(), scope, actor, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		canon, _ := os.ReadFile(filepath.Join(dir, "canonical", name+".json"))
		digest, _ := os.ReadFile(filepath.Join(dir, "canonical", name+".digest"))
		if !bytes.Equal(v.Canonical(), canon) || string(v.Digest()) != strings.TrimSpace(string(digest)) {
			t.Errorf("%s: canonical bytes or digest changed with every format enabled", name)
		}
	}
	for _, name := range []string{"minimal-test", "build-and-publish", "m2-agent-gate"} {
		t.Run("v1alpha1/"+name, func(t *testing.T) { check(t, "../../contracts/pipeline/v1/fixtures", name) })
	}
	for _, name := range v2Goldens {
		t.Run("v1alpha2/"+name, func(t *testing.T) { check(t, fixturesV2, name) })
	}

	t.Run("published vectors", func(t *testing.T) {
		v := newEnvV2(t).mustValidate(t, string(readFixtureV2(t, "valid/service-test.yaml")))
		if string(v.Digest()) != "sha256:368c9636851797bb0812589cae5297122b3d799fc96522194764de5a574910e2" {
			t.Fatalf("the kodama/v1alpha2 reference vector changed: %s", v.Digest())
		}
	})

	t.Run("a version is judged by its own rules", func(t *testing.T) {
		// The v1alpha2 service-test document, re-labelled, is a different
		// document with a different digest; neither is reinterpreted.
		on := newEnvV3(t)
		a := newEnvV2(t).mustValidate(t, string(readFixtureV2(t, "valid/service-test.yaml")))
		b := on.mustValidate(t, string(readFixtureV3(t, "valid/service-test.yaml")))
		if a.Digest() == b.Digest() {
			t.Fatal("the digest includes the apiVersion")
		}
		// A kodama/v1alpha1 document can never carry run_shared fields or sources.
		_, err := all.ValidateYAML(t.Context(), scope, actor, []byte(strings.Replace(doc(container("a")), "spec:\n", "spec:\n  sources: []\n", 1)))
		expectCode(t, err, pipeline.CodeUnknownField)
		// An unknown version is still not a version.
		_, err = all.ValidateYAML(t.Context(), scope, actor, []byte(strings.Replace(doc(container("a")), "v1alpha1", "v1alpha4", 1)))
		if err == nil || all.FormatEnabled("kodama/v1alpha4") {
			t.Fatal("an unknown apiVersion must not validate")
		}
	})
}
