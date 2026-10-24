package runtimev1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type designFile struct {
	Cases  []fixtureCase `json:"cases"`
	Checks []struct {
		Name         string          `json:"name"`
		Baseline     json.RawMessage `json:"baseline"`
		Report       json.RawMessage `json:"report"`
		Candidate    GitCommit       `json:"candidate"`
		Code         string          `json:"code"`
		Requirements []string        `json:"requirements"`
	} `json:"checks"`
	Vectors []struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		SHA256    Digest `json:"sha256"`
	} `json:"vectors"`
}

var designDecoders = map[string]func([]byte) error{
	"DesignBaseline":    func(b []byte) error { _, err := DecodeRequest[DesignBaseline](b); return err },
	"ConformanceReport": func(b []byte) error { _, err := DecodeRequest[ConformanceReport](b); return err },
}

// TestDesignContextFixtures is the second T-003 acceptance check
// (design_context_fixtures): immutable reference encoding, deterministic
// baseline digest, report disposition and check coverage, and wrong-candidate
// or wrong-baseline reports (AR-14, DQ-14, SD-08).
func TestDesignContextFixtures(t *testing.T) {
	var f designFile
	readFixture(t, "design.json", &f)
	_, compiler := loadSchemaDoc(t)
	covered := map[string]bool{}

	t.Run("payloads", func(t *testing.T) {
		valid, invalid := map[string]bool{}, map[string]bool{}
		for _, c := range f.Cases {
			mark(covered, c.Requirements)
			t.Run(c.Name, func(t *testing.T) {
				err := designDecoders[c.Type](c.Payload)
				if (err == nil) != c.Valid {
					t.Fatalf("Go contract valid=%v, fixture expects %v (%v)", err == nil, c.Valid, err)
				}
				if err != nil && codeOf(err) != c.Code {
					t.Fatalf("code %q, want %q (%v)", codeOf(err), c.Code, err)
				}
				got := schemaVerdict(t, compiler, c.Type, c.Payload)
				if c.SchemaValid == nil || got != *c.SchemaValid {
					t.Fatalf("schema valid=%v, fixture expects %v", got, c.SchemaValid)
				}
				if c.Valid && !got {
					t.Fatal("Go accepts a payload the schema rejects")
				}
			})
			if c.Valid {
				valid[c.Type] = true
			} else {
				invalid[c.Type] = true
			}
		}
		for _, typeName := range []string{"DesignBaseline", "ConformanceReport"} {
			if !valid[typeName] || !invalid[typeName] {
				t.Errorf("%s needs positive and negative fixtures", typeName)
			}
		}
	})

	t.Run("conformance_checks", func(t *testing.T) {
		for _, c := range f.Checks {
			mark(covered, c.Requirements)
			t.Run(c.Name, func(t *testing.T) {
				var baseline DesignBaseline
				var report ConformanceReport
				if err := json.Unmarshal(c.Baseline, &baseline); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(c.Report, &report); err != nil {
					t.Fatal(err)
				}
				err := CheckConformance(report, baseline, c.Candidate)
				if c.Code == "" && err != nil {
					t.Fatalf("unexpected: %v", err)
				}
				if c.Code != "" && codeOf(err) != c.Code {
					t.Fatalf("code %q, want %q (%v)", codeOf(err), c.Code, err)
				}
			})
		}
	})

	// Digests are compared with values derived independently by Python.
	t.Run("digests", func(t *testing.T) {
		for _, v := range f.Vectors {
			t.Run(v.Name, func(t *testing.T) {
				canonical, err := Canonicalize([]byte(v.Input))
				if err != nil || string(canonical) != v.Canonical {
					t.Fatalf("canonical %s (%v), want %s", canonical, err, v.Canonical)
				}
				var got Digest
				switch v.Kind {
				case "baseline":
					var refs baselineReferences
					if err := json.Unmarshal([]byte(v.Input), &refs); err != nil {
						t.Fatal(err)
					}
					got, err = BaselineDigest(DesignBaseline{Requirements: refs.Requirements, Decisions: refs.Decisions, Sources: refs.Sources, RequiredChecks: refs.RequiredChecks})
				case "report":
					var report ConformanceReport
					if err := json.Unmarshal([]byte(v.Input), &report); err != nil {
						t.Fatal(err)
					}
					got, err = ReportDigest(report)
				}
				if err != nil || got != v.SHA256 {
					t.Fatalf("digest %s (%v), want %s", got, err, v.SHA256)
				}
			})
		}
	})

	t.Run("determinism", func(t *testing.T) {
		repo := "https://git.example.invalid/acme/inventory.git"
		rev := GitCommit(strings.Repeat("1", 40))
		sources := []SourceRef{{repo, rev, "docs/specs/software-design.md"}, {repo, rev, "docs/implementation/design-conformance.md"}}
		a, err := NewDesignBaseline([]string{"SD-08", "AR-14"}, []string{"P-16", "C-16"}, sources, []string{"tenant-scope", "module-boundaries"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewDesignBaseline([]string{"AR-14", "SD-08"}, []string{"C-16", "P-16"}, []SourceRef{sources[1], sources[0]}, []string{"module-boundaries", "tenant-scope"})
		if err != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("input order changed the baseline: %v", err)
		}
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
		// The digest field itself is not an input.
		c := a
		c.Digest = Digest("sha256:" + strings.Repeat("0", 64))
		if got, _ := BaselineDigest(c); got != a.Digest {
			t.Fatal("digest depends on its own field")
		}
		// A moved source revision (a branch advancing mid-run) is a different baseline.
		moved := append([]SourceRef{}, sources...)
		moved[0].Revision = GitCommit(strings.Repeat("2", 40))
		d, _ := NewDesignBaseline([]string{"SD-08", "AR-14"}, []string{"P-16", "C-16"}, moved, []string{"tenant-scope", "module-boundaries"})
		if d.Digest == a.Digest {
			t.Fatal("a changed source revision kept the digest")
		}
		if _, err := NewDesignBaseline([]string{"SD-08", "SD-08"}, nil, sources, nil); err == nil {
			t.Fatal("duplicates accepted")
		}
		encoded, _ := json.Marshal(a)
		decoded, err := DecodeRequest[DesignBaseline](encoded)
		if err != nil || !reflect.DeepEqual(decoded, a) {
			t.Fatalf("round trip failed: %v", err)
		}
	})

	t.Run("traceability", func(t *testing.T) {
		for _, id := range []string{"AR-14", "DQ-14", "SD-08"} {
			if !covered[id] {
				t.Errorf("no design fixture covers %s", id)
			}
		}
	})
}
