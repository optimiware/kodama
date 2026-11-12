package definition

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// TestParseReferenceAcceptsDirectLookups pins the exact S03 §2 grammar:
// inputs.<declared-key> or <earlier-action>.outputs.<declared-key>. Nothing
// else is a reference; there is no interpolation, template or JSON path.
func TestParseReferenceAcceptsDirectLookups(t *testing.T) {
	longAction := strings.Repeat("a", id.MaxNameLength)
	longKey := strings.Repeat("a", id.MaxKeyLength)

	cases := []struct {
		from string
		want Reference
	}{
		{"inputs.repository", Reference{Input: true, Key: "repository"}},
		{"inputs.a", Reference{Input: true, Key: "a"}},
		{"inputs.a1_b-c", Reference{Input: true, Key: "a1_b-c"}},
		{"inputs." + longKey, Reference{Input: true, Key: longKey}},
		{"build.outputs.candidate", Reference{Action: "build", Key: "candidate"}},
		{"build-2.outputs.a1_b-c", Reference{Action: "build-2", Key: "a1_b-c"}},
		{longAction + ".outputs." + longKey, Reference{Action: longAction, Key: longKey}},
	}

	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			got, err := ParseReference(tc.from)
			if err != nil {
				t.Fatalf("ParseReference(%q) error = %v, want nil", tc.from, err)
			}
			if got != tc.want {
				t.Fatalf("ParseReference(%q) = %+v, want %+v", tc.from, got, tc.want)
			}
			if got.Key == "" {
				t.Fatalf("ParseReference(%q) returned an empty key", tc.from)
			}
			if got.Input == (got.Action != "") {
				t.Fatalf("ParseReference(%q) = %+v does not name exactly one source", tc.from, got)
			}
		})
	}
}

// TestParseReferenceRefusesMalformedForms covers expressions, templates,
// extra path segments, mixed forms and unsafe characters.
func TestParseReferenceRefusesMalformedForms(t *testing.T) {
	forms := []string{
		"",
		"inputs",
		"inputs.",
		".inputs.repository",
		"inputs.repository.extra",
		"inputs..repository",
		"build.outputs",
		"build.outputs.",
		"build.outputs.candidate.extra",
		"build.output.candidate",
		"build.Outputs.candidate",
		"build.outputs..candidate",
		"outputs.candidate",
		"repository",
		"inputs.repository/../x",
		"../inputs.repository",
		"inputs.repository\x00suffix",
		"inputs.repository\nsuffix",
		"inputs.Repository",
		"Inputs.repository",
		"1build.outputs.candidate",
		"build-.outputs.candidate",
		"build--x.outputs.candidate",
		"build.outputs.1candidate",
		"build.outputs.candidate ",
		" build.outputs.candidate",
		"build.outputs.candidate\x00",
		"${inputs.repository}",
		"{{build.outputs.candidate}}",
		"inputs.repository | upper",
		"inputs.*",
		"inputs.repository[0]",
		"build.outputs.candidate[0]",
	}

	for _, from := range forms {
		t.Run(strings.ReplaceAll(from, "\x00", "NUL"), func(t *testing.T) {
			got, err := ParseReference(from)
			if err == nil {
				t.Fatalf("ParseReference(%q) = %+v, want refusal", from, got)
			}
			assertInvalidInput(t, err, "reference "+from)
			if got != (Reference{}) {
				t.Fatalf("ParseReference(%q) returned partial value %+v with an error", from, got)
			}
		})
	}
}

// TestParseReferenceRejectsUnsafeNamesAndBounds checks the id grammar and
// the 64-byte bounds at both segments of every accepted form.
func TestParseReferenceRejectsUnsafeNamesAndBounds(t *testing.T) {
	forms := []string{
		"inputs." + strings.Repeat("a", id.MaxKeyLength+1),
		"inputs." + strings.Repeat("a", id.MaxKeyLength) + ".",
		strings.Repeat("a", id.MaxNameLength+1) + ".outputs.candidate",
		strings.Repeat("a", id.MaxNameLength) + "-.outputs.candidate",
		"build.outputs." + strings.Repeat("a", id.MaxKeyLength+1),
		"inputs." + strings.Repeat("A", id.MaxKeyLength),
	}

	for _, from := range forms {
		t.Run(from, func(t *testing.T) {
			if _, err := ParseReference(from); err == nil {
				t.Fatalf("ParseReference(%q) accepted an out-of-bounds or unsafe name", from)
			} else {
				assertInvalidInput(t, err, "reference "+from)
			}
		})
	}
}

// FuzzParseReference asserts the parser is total, always returns a typed
// invalid_input fault on refusal, and only accepts canonical direct lookups
// whose segments satisfy the declared name grammars.
func FuzzParseReference(f *testing.F) {
	for _, seed := range []string{
		"",
		"inputs.repository",
		"build.outputs.candidate",
		"inputs.",
		"inputs.a.b",
		"build.outputs",
		"build.outputs.",
		"build.outputs.a.b",
		"build.output.a",
		"outputs.a",
		"../inputs.repository",
		"inputs.a/b",
		"inputs.A",
		"Inputs.a",
		" inputs.a",
		"inputs.a ",
		"${inputs.a}",
		"{{build.outputs.a}}",
		"inputs.*",
		"build.outputs.a[0]",
		"inputs." + strings.Repeat("a", id.MaxKeyLength+1),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, from string) {
		reference, err := ParseReference(from)
		if err != nil {
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("ParseReference(%q) error = %v, want typed invalid_input", from, err)
			}
			if reference != (Reference{}) {
				t.Fatalf("ParseReference(%q) returned partial value %+v with an error", from, reference)
			}
			return
		}

		if reference.Key == "" {
			t.Fatalf("ParseReference(%q) accepted an empty key", from)
		}
		if reference.Input == (reference.Action != "") {
			t.Fatalf("ParseReference(%q) = %+v does not name exactly one source", from, reference)
		}

		canonical := ""
		if reference.Input {
			if reference.Action != "" {
				t.Fatalf("ParseReference(%q) = %+v mixes input and action sources", from, reference)
			}
			if _, err := id.ParseInputKey(reference.Key); err != nil {
				t.Fatalf("ParseReference(%q) accepted invalid input key %q", from, reference.Key)
			}
			canonical = "inputs." + reference.Key
		} else {
			if _, err := id.ParseActionName(reference.Action); err != nil {
				t.Fatalf("ParseReference(%q) accepted invalid action name %q", from, reference.Action)
			}
			if _, err := id.ParseOutputKey(reference.Key); err != nil {
				t.Fatalf("ParseReference(%q) accepted invalid output key %q", from, reference.Key)
			}
			canonical = reference.Action + ".outputs." + reference.Key
		}

		if from != canonical {
			t.Fatalf("ParseReference(%q) accepted a non-canonical spelling; canonical form is %q", from, canonical)
		}
		reparsed, err := ParseReference(canonical)
		if err != nil {
			t.Fatalf("ParseReference(%q) round-trip error = %v", canonical, err)
		}
		if reparsed != reference {
			t.Fatalf("ParseReference(%q) round-trip = %+v, want %+v", canonical, reparsed, reference)
		}
	})
}
