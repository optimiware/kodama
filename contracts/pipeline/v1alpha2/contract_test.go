package pipelinev1alpha2_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
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
	sch, err := pipelinev1alpha2.Schema()
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

// Test_v1alpha2_contract_fixtures: the embedded schema accepts every valid
// fixture and its golden canonical form, and judges every published rejected
// document as the fixture records. The schema alone decides structure;
// edge-to-service references, probe-to-port references, source defaults and the
// format capability are the validator's (internal/pipeline), so a case the
// validator rejects at its syntax stage may still be schema-valid. PX-17, PX-20,
// IT-14.
func Test_v1alpha2_contract_fixtures(t *testing.T) {
	valid, err := filepath.Glob("fixtures/valid/*")
	if err != nil || len(valid) < 6 {
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
	if len(cases) < 100 {
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
	if schemaRejected < 80 {
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

// Test_v1alpha2_schema_is_a_separate_version: a v1alpha2 document is never
// valid under the v1alpha1 schema and the reverse, so no document is reinterpreted.
// PX-21.
func Test_v1alpha2_schema_is_a_separate_version(t *testing.T) {
	v2 := readFixture(t, "valid/service-test.yaml")
	v1sch, err := pipelinev1.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if v1sch.Validate(instance(t, "yaml", v2)) == nil {
		t.Fatal("the v1alpha1 schema accepted a v1alpha2 document")
	}
	v1doc := `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"steps":[{"id":"g","kind":"gate"}]}}`
	if validate(t, "json", v1doc) == nil {
		t.Fatal("the v1alpha2 schema accepted a v1alpha1 document")
	}
	for _, mutated := range []string{
		strings.Replace(v2, "kodama/v1alpha2", "kodama/v1alpha1", 1),
		strings.Replace(v2, "kodama/v1alpha2", "kodama/v1alpha3", 1),
	} {
		if validate(t, "yaml", mutated) == nil {
			t.Fatal("the schema accepted another apiVersion")
		}
	}
	if pipelinev1alpha2.APIVersion != "kodama/v1alpha2" || pipelinev1alpha2.Kind != "Pipeline" {
		t.Fatal("document identity changed")
	}
}

// Test_v1alpha2_schema_states_the_contract_bounds pins the numbers the prose
// contract fixes, so the schema, the Go shape checks and the document cannot
// drift apart silently. PX-17, IT-14.
func Test_v1alpha2_schema_states_the_contract_bounds(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal(pipelinev1alpha2.SchemaJSON(), &s); err != nil {
		t.Fatal(err)
	}
	defs := s["$defs"].(map[string]any)
	num := func(def, prop, key string) float64 {
		t.Helper()
		m := defs[def].(map[string]any)["properties"].(map[string]any)[prop].(map[string]any)
		return m[key].(float64)
	}
	for _, c := range []struct {
		def, prop string
		lo, hi    float64
	}{
		{"serviceResources", "cpuMillis", 50, 16000}, {"serviceResources", "memoryMiB", 16, 65536},
		{"serviceResources", "ephemeralMiB", 16, 262144}, {"serviceResources", "shmMiB", 0, 4096},
		{"readiness", "timeoutSeconds", 5, 600}, {"readiness", "periodSeconds", 1, 60},
		{"readiness", "probeTimeoutSeconds", 1, 60}, {"readiness", "lossSeconds", 5, 600},
		{"port", "port", 1, 65535}, {"storage", "sizeMiB", 1, 262144}, {"runWorkspace", "sizeMiB", 64, 1048576},
	} {
		if num(c.def, c.prop, "minimum") != c.lo || num(c.def, c.prop, "maximum") != c.hi {
			t.Errorf("%s.%s: bounds are not [%v, %v]", c.def, c.prop, c.lo, c.hi)
		}
	}
	spec := s["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	if tm := spec["timeoutSeconds"].(map[string]any); tm["minimum"].(float64) != 60 || tm["maximum"].(float64) != 604800 {
		t.Error("spec.timeoutSeconds must be 60..604800")
	}
	if spec["services"].(map[string]any)["maxProperties"].(float64) != 8 {
		t.Error("at most 8 services")
	}
	if defs["network"].(map[string]any)["properties"].(map[string]any)["edges"].(map[string]any)["maxItems"].(float64) != 32 {
		t.Error("at most 32 edges")
	}
	if defs["egress"].(map[string]any)["maxItems"].(float64) != 8 {
		t.Error("at most 8 egress names")
	}
	svc := defs["service"].(map[string]any)["properties"].(map[string]any)
	if svc["command"].(map[string]any)["minItems"].(float64) != 1 || svc["command"].(map[string]any)["maxItems"].(float64) != 64 ||
		svc["args"].(map[string]any)["maxItems"].(float64) != 256 || svc["storage"].(map[string]any)["maxItems"].(float64) != 4 ||
		svc["env"].(map[string]any)["maxProperties"].(float64) != 64 || svc["secretFiles"].(map[string]any)["maxProperties"].(float64) != 16 ||
		svc["ports"].(map[string]any)["minProperties"].(float64) != 1 || svc["ports"].(map[string]any)["maxProperties"].(float64) != 8 {
		t.Error("a service bound differs from the contract")
	}
	if defs["serviceName"].(map[string]any)["pattern"] != `^[a-z][a-z0-9-]{0,29}$` || defs["portName"].(map[string]any)["pattern"] != `^[a-z][a-z0-9-]{0,14}$` {
		t.Error("service and port name patterns differ from the contract")
	}
	if _, has := defs["workspace"].(map[string]any)["properties"].(map[string]any)["from"]; has {
		t.Error("workspace has no from in kodama/v1alpha2")
	}
	for _, forbidden := range []string{`"hostPort"`, `"privileged"`, `"securityContext"`} {
		if bytes.Contains(pipelinev1alpha2.SchemaJSON(), []byte(forbidden)) {
			t.Errorf("the schema must not define %s", forbidden)
		}
	}
}

func Test_v1alpha2_schema_is_embedded_and_copied(t *testing.T) {
	a := pipelinev1alpha2.SchemaJSON()
	a[0] = 'X'
	if bytes.Equal(a, pipelinev1alpha2.SchemaJSON()) {
		t.Fatal("SchemaJSON must return a copy")
	}
	onDisk, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, pipelinev1alpha2.SchemaJSON()) {
		t.Fatal("the embedded schema differs from schema.json")
	}
	var m map[string]any
	if err := json.Unmarshal(onDisk, &m); err != nil || m["$id"] != pipelinev1alpha2.SchemaID {
		t.Fatalf("schema id: %v %v", m["$id"], err)
	}
}
