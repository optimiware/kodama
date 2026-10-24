package qa_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
)

type reportCase struct {
	Name        string `json:"name"`
	File        string `json:"file"`
	Valid       bool   `json:"valid"`
	SchemaValid *bool  `json:"schemaValid"`
	Suffix      string `json:"suffix"`
	Edits       []struct {
		Old string `json:"old"`
		New string `json:"new"`
	} `json:"edits"`
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadCases(t *testing.T) []reportCase {
	t.Helper()
	var doc struct {
		Cases []reportCase `json:"cases"`
	}
	if err := json.Unmarshal(readFile(t, "cases.json"), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func (c reportCase) payload(t *testing.T) []byte {
	t.Helper()
	s := string(readFile(t, c.File))
	for _, e := range c.Edits {
		if n := strings.Count(s, e.Old); n != 1 {
			t.Fatalf("%s: edit %q matches %d times, want exactly once", c.Name, e.Old, n)
		}
		s = strings.Replace(s, e.Old, e.New, 1)
	}
	return []byte(s + c.Suffix)
}

func schemaOf(t *testing.T) *jsonschema.Schema {
	t.Helper()
	const url = "https://kodama.invalid/qa/report-v1.schema.json"
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(qa.Schema))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(url)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReportContractFixtures: the strict decoder and the published schema agree
// with the published cases. A payload the Go decoder accepts must pass the
// schema; the cases name the rest.
func TestReportContractFixtures(t *testing.T) {
	schema := schemaOf(t)
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			data := c.payload(t)
			_, err := qa.Decode(data)
			if c.Valid && err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !c.Valid {
				if err == nil {
					t.Fatal("decode accepted an invalid report")
				}
				if !errors.Is(err, qa.ErrInvalid) {
					t.Fatalf("decode error is not ErrInvalid: %v", err)
				}
			}
			inst, perr := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if perr != nil {
				return // not parseable JSON: only the strict parser judges it
			}
			got := schema.Validate(inst) == nil
			if c.SchemaValid != nil && got != *c.SchemaValid {
				t.Fatalf("schema verdict %v, want %v", got, *c.SchemaValid)
			}
			if c.Valid && !got {
				t.Fatal("a report the Go contract accepts must pass the schema")
			}
		})
	}
}

func TestReportCanonicalIsFormattingIndependent(t *testing.T) {
	r1, err := qa.Decode(readFile(t, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(readFile(t, "report.json"), &tree); err != nil {
		t.Fatal(err)
	}
	compact, _ := json.Marshal(tree) // reordered keys, no whitespace
	r2, err := qa.Decode(compact)
	if err != nil {
		t.Fatal(err)
	}
	d1, err := r1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := r2.Digest()
	if d1 != d2 || !strings.HasPrefix(string(d1), "sha256:") {
		t.Fatalf("digests differ: %s %s", d1, d2)
	}
	c1, _ := r1.Canonical()
	if bytes.ContainsAny(c1, " \n") && !strings.Contains(string(c1), "Explored") {
		t.Fatal("canonical form keeps insignificant whitespace")
	}
	if !strings.Contains(string(c1), `"note":null`) || !strings.Contains(string(c1), `"criterionId":null`) {
		t.Fatalf("canonical form must keep explicit nulls: %s", c1)
	}
	// A changed field changes the digest.
	r1.Summary += "!"
	d3, _ := r1.Digest()
	if d3 == d1 {
		t.Fatal("digest ignores content")
	}
}

func TestReportBounds(t *testing.T) {
	base, err := qa.Decode(readFile(t, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(r *qa.Report)) error {
		r := base
		r.Observations = append([]qa.Observation(nil), base.Observations...)
		f(&r)
		return r.Validate()
	}
	if err := mutate(func(r *qa.Report) {
		for len(r.Observations) <= qa.MaxObservations {
			o := r.Observations[0]
			o.Title = strings.Repeat("x", len(r.Observations)+1)
			r.Observations = append(r.Observations, o)
		}
	}); !errors.Is(err, qa.ErrInvalid) {
		t.Fatalf("too many observations: %v", err)
	}
	if err := mutate(func(r *qa.Report) { r.Observations[0].Title = strings.Repeat("é", qa.MaxTitleChars+1) }); !errors.Is(err, qa.ErrInvalid) {
		t.Fatalf("long title: %v", err)
	}
	if err := mutate(func(r *qa.Report) { r.Observations[0].Actual = "bell\x07" }); !errors.Is(err, qa.ErrInvalid) {
		t.Fatalf("control character: %v", err)
	}
	if err := mutate(func(r *qa.Report) { r.Observations[0].Actual = "line one\nline two\tindented" }); err != nil {
		t.Fatalf("newline and tab are allowed in multi-line text: %v", err)
	}
	if err := mutate(func(r *qa.Report) { r.Observations = nil }); !errors.Is(err, qa.ErrInvalid) {
		t.Fatalf("a null observations array must be refused: %v", err)
	}
	if err := mutate(func(r *qa.Report) {
		for i := 0; i < qa.MaxEvidencePerFinding; i++ {
			o := r.Observations[0]
			o.Evidence = append(append([]qa.EvidenceRef(nil), o.Evidence...), qa.EvidenceRef{ArtifactID: domain.ArtifactID("art_more" + strings.Repeat("x", i+1)), Digest: o.Evidence[0].Digest, Kind: qa.EvidenceLog})
			r.Observations[0] = o
		}
	}); !errors.Is(err, qa.ErrInvalid) {
		t.Fatalf("too much evidence: %v", err)
	}
}
