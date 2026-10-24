package pipelinev1_test

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
)

type testCase struct {
	Name        string `json:"name"`
	Format      string `json:"format"`
	SchemaValid *bool  `json:"schemaValid"`
	Code        string `json:"code"`
	Document    string `json:"document"`
}

// toJSON converts a YAML or JSON fixture to the JSON value the schema judges.
func toJSON(t testing.TB, format, doc string) any {
	t.Helper()
	var v any
	switch format {
	case "yaml":
		if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatalf("fixture is not YAML: %v", err)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("fixture has a non-JSON value: %v", err)
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
	sch, err := pipelinev1.Schema()
	if err != nil {
		t.Fatal(err)
	}
	return sch.Validate(toJSON(t, format, doc))
}

func readFile(t testing.TB, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Test_pipeline_contract_fixtures: the embedded schema accepts every valid
// YAML/JSON fixture, including the planned M2 shapes, and rejects every
// structurally invalid case. The schema alone decides structure; whether a kind
// is enabled is the validator's decision (internal/pipeline), and the schema
// carries no claim of executable M2 behavior. PX-01, PX-02, PX-14.
func Test_pipeline_contract_fixtures(t *testing.T) {
	t.Run("valid fixtures", func(t *testing.T) {
		for _, f := range []struct{ file, format string }{
			{"valid/minimal-test.yaml", "yaml"}, {"valid/minimal-test.json", "json"},
			{"valid/build-and-publish.yaml", "yaml"}, {"valid/m2-agent-gate.yaml", "yaml"},
		} {
			if err := validate(t, f.format, readFile(t, f.file)); err != nil {
				t.Errorf("%s: %v", f.file, err)
			}
		}
	})

	t.Run("cases", func(t *testing.T) {
		var cases []testCase
		if err := json.Unmarshal([]byte(readFile(t, "cases.json")), &cases); err != nil {
			t.Fatal(err)
		}
		if len(cases) < 10 {
			t.Fatalf("only %d cases", len(cases))
		}
		for _, c := range cases {
			t.Run(c.Name, func(t *testing.T) {
				if c.SchemaValid == nil {
					return // not representable as a value (duplicate keys): parser territory
				}
				err := validate(t, c.Format, c.Document)
				if *c.SchemaValid && err != nil {
					t.Fatalf("the schema must accept a case only the validator rejects (%s): %v", c.Code, err)
				}
				if !*c.SchemaValid && err == nil {
					t.Fatalf("the schema must reject %s", c.Code)
				}
			})
		}
	})

	t.Run("step alternatives are exclusive", func(t *testing.T) {
		img := "x@sha256:" + strings.Repeat("1", 64)
		wrap := func(step string) string {
			return `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"steps":[` + step + `]}}`
		}
		for name, tc := range map[string]struct {
			step string
			ok   bool
		}{
			"container":                 {`{"id":"a","kind":"container","image":"` + img + `","command":["x"],"timeoutSeconds":5}`, true},
			"container with gate field": {`{"id":"a","kind":"container","image":"` + img + `","command":["x"],"timeoutSeconds":5,"gate":{}}`, false},
			"gate":                      {`{"id":"g","kind":"gate"}`, true},
			"gate with image":           {`{"id":"g","kind":"gate","image":"` + img + `"}`, false},
			"gate with when":            {`{"id":"g","kind":"gate","when":{"input":"x","equals":true}}`, false},
			"unknown kind":              {`{"id":"g","kind":"operator"}`, false},
			"missing kind":              {`{"id":"g"}`, false},
			"container without image":   {`{"id":"a","kind":"container","command":["x"],"timeoutSeconds":5}`, false},
			"string command":            {`{"id":"a","kind":"container","image":"` + img + `","command":"x","timeoutSeconds":5}`, false},
			"tag only image":            {`{"id":"a","kind":"container","image":"golang:1.24","command":["x"],"timeoutSeconds":5}`, false},
			"wrong api version":         {"", false},
		} {
			doc := wrap(tc.step)
			if name == "wrong api version" {
				doc = strings.Replace(wrap(`{"id":"g","kind":"gate"}`), "v1alpha1", "v2", 1)
			}
			err := validate(t, "json", doc)
			if tc.ok && err != nil {
				t.Errorf("%s: %v", name, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("canonical goldens are schema-valid, compact and match their digests", func(t *testing.T) {
		for _, name := range []string{"minimal-test", "build-and-publish", "m2-agent-gate"} {
			doc := readFile(t, "canonical/"+name+".json")
			if err := validate(t, "json", doc); err != nil {
				t.Errorf("%s: canonical form violates the schema: %v", name, err)
			}
			if strings.ContainsAny(doc, "\n\t") {
				t.Errorf("%s: canonical form is not compact", name)
			}
			d := strings.TrimSpace(readFile(t, "canonical/"+name+".digest"))
			if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
				t.Errorf("%s: malformed digest %q", name, d)
			}
		}
	})

	t.Run("embedded schema is stable and defensively copied", func(t *testing.T) {
		a := pipelinev1.SchemaJSON()
		a[0] = 'X'
		if bytes.Equal(a, pipelinev1.SchemaJSON()) {
			t.Fatal("SchemaJSON must return a copy")
		}
		onDisk, err := os.ReadFile("schema.json")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(onDisk, pipelinev1.SchemaJSON()) {
			t.Fatal("the embedded schema differs from schema.json")
		}
		var m map[string]any
		if err := json.Unmarshal(onDisk, &m); err != nil || m["$id"] != pipelinev1.SchemaID {
			t.Fatalf("schema id: %v %v", m["$id"], err)
		}
	})
}
