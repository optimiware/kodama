package pipelinev1alpha3_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
)

type testCase struct {
	Name        string `json:"name"`
	Format      string `json:"format"`
	SchemaValid bool   `json:"schemaValid"`
	Stage       string `json:"stage"`
	Code        string `json:"code"`
	Path        string `json:"path"`
	Document    string `json:"document"`
}

func instance(t testing.TB, format, doc string) any {
	t.Helper()
	if format == "yaml" {
		var v any
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("fixture is not YAML: %v", err)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		doc = string(raw)
	}
	out, err := jsonschema.UnmarshalJSON(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return out
}

func validate(t testing.TB, format, doc string) error {
	t.Helper()
	sch, err := pipelinev1alpha3.Schema()
	if err != nil {
		t.Fatal(err)
	}
	return sch.Validate(instance(t, format, doc))
}

func readFixture(t testing.TB, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Test_v1alpha3_contract_fixtures: the embedded schema accepts every valid
// fixture and its golden canonical form, and judges every published rejected
// document as the fixture records. The schema alone decides structure;
// edge-to-service references, probe-to-port references, source defaults and the
// format capability are the validator's (internal/pipeline), so a case the
// validator rejects at its syntax stage may still be schema-valid. PX-17, PX-20,
// IT-14.
func Test_v1alpha3_contract_fixtures(t *testing.T) {
	valid, err := filepath.Glob("fixtures/valid/*")
	if err != nil || len(valid) < 7 {
		t.Fatalf("valid fixtures: %v %v", valid, err)
	}
	for _, f := range valid {
		format := "yaml"
		if strings.HasSuffix(f, ".json") {
			format = "json"
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := validate(t, format, string(raw)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		if _, err := os.Stat(filepath.Join("fixtures", "canonical", base+".json")); err != nil {
			t.Errorf("%s has no canonical golden", f)
		}
	}

	var cases []testCase
	if err := json.Unmarshal([]byte(readFixture(t, "cases.json")), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 40 {
		t.Fatalf("only %d cases", len(cases))
	}
	seen := map[string]bool{}
	var schemaRejected int
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate case name %q", c.Name)
		}
		seen[c.Name] = true
		if c.Code == "" || c.Stage == "" || c.Path == "" {
			t.Fatalf("%s: a case names its code, path and stage", c.Name)
		}
		err := validate(t, c.Format, c.Document)
		if c.SchemaValid && err != nil {
			t.Errorf("%s: the schema must accept a case only the validator rejects (%s): %v", c.Name, c.Code, err)
		}
		if !c.SchemaValid {
			schemaRejected++
			if err == nil {
				t.Errorf("%s: the schema must reject %s", c.Name, c.Code)
			}
			if c.Stage != "syntax" {
				t.Errorf("%s: a schema-rejected case fails at the syntax stage", c.Name)
			}
		}
	}
	if schemaRejected < 30 {
		t.Fatalf("only %d schema-rejected cases", schemaRejected)
	}

	t.Run("canonical goldens are schema-valid, compact and match their digests", func(t *testing.T) {
		goldens, _ := filepath.Glob("fixtures/canonical/*.json")
		if len(goldens) < 6 {
			t.Fatalf("goldens: %v", goldens)
		}
		for _, g := range goldens {
			raw, _ := os.ReadFile(g)
			if err := validate(t, "json", string(raw)); err != nil {
				t.Errorf("%s: canonical form violates the schema: %v", g, err)
			}
			if bytes.ContainsAny(raw, "\n\t") {
				t.Errorf("%s: canonical form is not compact", g)
			}
			d := strings.TrimSpace(readFixture(t, "canonical/"+strings.TrimSuffix(filepath.Base(g), ".json")+".digest"))
			if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
				t.Errorf("%s: malformed digest %q", g, d)
			}
		}
	})
}

// Test_v1alpha3_schema_is_a_separate_version: a v1alpha3 document is never
// valid under another version's schema and the reverse, so no document is
// reinterpreted. PX-21, PX-23.
func Test_v1alpha3_schema_is_a_separate_version(t *testing.T) {
	v3 := readFixture(t, "valid/two-repositories.yaml")
	for name, load := range map[string]func() (*jsonschema.Schema, error){"v1alpha1": pipelinev1.Schema, "v1alpha2": pipelinev1alpha2.Schema} {
		sch, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if sch.Validate(instance(t, "yaml", v3)) == nil {
			t.Fatalf("the %s schema accepted a v1alpha3 document", name)
		}
	}
	v2doc, err := os.ReadFile("../v1alpha2/fixtures/valid/service-test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if validate(t, "yaml", string(v2doc)) == nil {
		t.Fatal("the v1alpha3 schema accepted a v1alpha2 document")
	}
	v1doc := `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"steps":[{"id":"g","kind":"gate"}]}}`
	if validate(t, "json", v1doc) == nil {
		t.Fatal("the v1alpha3 schema accepted a v1alpha1 document")
	}
	// The v1alpha2 schema rejects sources: it is not an extension flag.
	sch2, _ := pipelinev1alpha2.Schema()
	withSources := strings.Replace(string(v2doc), "spec:\n", "spec:\n  sources: [{repository: a, path: ., access: read}]\n", 1)
	if sch2.Validate(instance(t, "yaml", withSources)) == nil {
		t.Fatal("the v1alpha2 schema accepted spec.sources")
	}
	for _, mutated := range []string{
		strings.Replace(v3, "kodama/v1alpha3", "kodama/v1alpha2", 1),
		strings.Replace(v3, "kodama/v1alpha3", "kodama/v1alpha4", 1),
	} {
		if validate(t, "yaml", mutated) == nil {
			t.Fatal("the schema accepted another apiVersion")
		}
	}
	if pipelinev1alpha3.APIVersion != "kodama/v1alpha3" || pipelinev1alpha3.Kind != "Pipeline" {
		t.Fatal("document identity changed")
	}
}

// Test_v1alpha3_schema_adds_only_sources: apart from the version identity, the
// description and spec.sources (with the three definitions it uses), the schema
// is the kodama/v1alpha2 schema, so the run_shared contract has one meaning in
// both versions. PX-21, PX-23.
func Test_v1alpha3_schema_adds_only_sources(t *testing.T) {
	var a, b map[string]any
	if err := json.Unmarshal(pipelinev1alpha3.SchemaJSON(), &a); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile("../v1alpha2/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(old, &b); err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]any{a, b} {
		for _, k := range []string{"$id", "title", "description"} {
			delete(m, k)
		}
		m["properties"].(map[string]any)["apiVersion"] = nil
		defs := m["$defs"].(map[string]any)
		delete(defs, "workspace") // only its description differs
		spec := m["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
		delete(spec, "sources")
		for _, k := range []string{"source", "sourcePath", "repositoryName"} {
			delete(defs, k)
		}
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if !bytes.Equal(x, y) {
		t.Fatal("the kodama/v1alpha3 schema differs from kodama/v1alpha2 beyond spec.sources")
	}
}

// Test_v1alpha3_schema_states_the_source_bounds pins the numbers the Git
// contract fixes so the schema and the Go validator cannot drift apart.
// PX-23.
func Test_v1alpha3_schema_states_the_source_bounds(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(pipelinev1alpha3.SchemaJSON(), &s); err != nil {
		t.Fatal(err)
	}
	defs := s["$defs"].(map[string]any)
	spec := s["properties"].(map[string]any)["spec"].(map[string]any)
	if r := spec["required"].([]any); len(r) != 2 || r[0] != "timeoutSeconds" || r[1] != "steps" {
		t.Errorf("sources must stay optional: %v", r)
	}
	srcs := spec["properties"].(map[string]any)["sources"].(map[string]any)
	if srcs["minItems"].(float64) != 1 || srcs["maxItems"].(float64) != float64(gitv1.MaxSources) {
		t.Errorf("sources are 1..%d", gitv1.MaxSources)
	}
	source := defs["source"].(map[string]any)
	if source["additionalProperties"] != false {
		t.Error("a source rejects unknown members: it carries no commit, branch or URL")
	}
	if got := source["required"].([]any); len(got) != 3 {
		t.Errorf("a source requires repository, path and access: %v", got)
	}
	acc := source["properties"].(map[string]any)["access"].(map[string]any)["enum"].([]any)
	if len(acc) != 2 || acc[0] != "read" || acc[1] != "write" {
		t.Errorf("source access is read or write: %v", acc)
	}
	if defs["repositoryName"].(map[string]any)["pattern"] != gitv1.Patterns["RepositoryName"] {
		t.Error("the repository name pattern differs from the Git contract")
	}
	path := defs["sourcePath"].(map[string]any)
	if path["pattern"] != gitv1.Patterns["SourcePath"] || path["maxLength"].(float64) != float64(gitv1.MaxPathBytes) {
		t.Error("the source path pattern or bound differs from the Git contract")
	}
}

func Test_v1alpha3_schema_is_embedded_and_copied(t *testing.T) {
	a := pipelinev1alpha3.SchemaJSON()
	a[0] = 'X'
	if bytes.Equal(a, pipelinev1alpha3.SchemaJSON()) {
		t.Fatal("SchemaJSON must return a copy")
	}
	onDisk, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, pipelinev1alpha3.SchemaJSON()) {
		t.Fatal("the embedded schema differs from schema.json")
	}
	var m map[string]any
	if err := json.Unmarshal(onDisk, &m); err != nil || m["$id"] != pipelinev1alpha3.SchemaID {
		t.Fatalf("schema id: %v %v", m["$id"], err)
	}
}
