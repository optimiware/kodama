package pipeline_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

const fixtures = "../../contracts/pipeline/v1/fixtures"

func readFixture(t testing.TB, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func Test_canonical_digest(t *testing.T) {
	e := newEnv(t)

	t.Run("golden bytes and digests", func(t *testing.T) {
		for _, name := range []string{"minimal-test", "build-and-publish", "m2-agent-gate"} {
			v := e.mustValidate(t, string(readFixture(t, "valid/"+name+".yaml")))
			canonPath := filepath.Join(fixtures, "canonical", name+".json")
			digestPath := filepath.Join(fixtures, "canonical", name+".digest")
			if os.Getenv("KODAMA_UPDATE_GOLDEN") == "1" {
				if err := os.MkdirAll(filepath.Dir(canonPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(canonPath, v.Canonical(), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(digestPath, []byte(v.Digest()+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want := readFixture(t, "canonical/"+name+".json")
			if !bytes.Equal(v.Canonical(), want) {
				t.Errorf("%s: canonical bytes changed\n got: %s\nwant: %s", name, v.Canonical(), want)
			}
			wantDigest := strings.TrimSpace(string(readFixture(t, "canonical/"+name+".digest")))
			if string(v.Digest()) != wantDigest || pipeline.DigestOf(want) != v.Digest() {
				t.Errorf("%s: digest %s, want %s", name, v.Digest(), wantDigest)
			}
		}
	})

	t.Run("canonical form is sorted, compact and unescaped", func(t *testing.T) {
		v := e.mustValidate(t, string(readFixture(t, "valid/minimal-test.yaml")))
		c := string(v.Canonical())
		if strings.ContainsAny(c, "\n\t") || strings.Contains(c, ": ") || strings.Contains(c, `\u00`) {
			t.Fatalf("not compact or unexpectedly escaped: %s", c)
		}
		if !strings.HasPrefix(c, `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":`) {
			t.Fatalf("keys are not sorted: %s", c)
		}
		if !strings.Contains(c, "&& go test -json ./... > /kodama") {
			t.Fatal("shell metacharacters must be preserved verbatim")
		}
	})

	t.Run("YAML, JSON, key order and formatting converge", func(t *testing.T) {
		yamlV := e.mustValidate(t, string(readFixture(t, "valid/minimal-test.yaml")))
		jsonV, err := e.v.ValidateJSON(t.Context(), scope, actor, readFixture(t, "valid/minimal-test.json"))
		if err != nil {
			t.Fatal(err)
		}
		if yamlV.Digest() != jsonV.Digest() || !bytes.Equal(yamlV.Canonical(), jsonV.Canonical()) {
			t.Fatalf("equivalent documents differ:\n%s\n%s", yamlV.Canonical(), jsonV.Canonical())
		}
		reordered := e.mustValidate(t, `
metadata:
    name: test
spec:
    steps:
        - timeoutSeconds: 600
          outputs: {report: {path: /kodama/outputs/tests.json, type: artifact}}
          workspace: {from: run, access: read}
          args:
            - "cd /workspace && go test -json ./... > /kodama/outputs/tests.json"
          command:
            - /bin/sh
            - -ec
          image: `+img+`
          kind: container
          id: test
kind: Pipeline
apiVersion: kodama/v1alpha1
`)
		if reordered.Digest() != yamlV.Digest() {
			t.Fatal("key order and formatting must not change the digest")
		}
	})

	t.Run("documented defaults normalize and nothing else does", func(t *testing.T) {
		minimal := e.mustValidate(t, doc(container("a")))
		explicit := e.mustValidate(t, docWith("{}", container("a",
			"needs: []", "args: []", "env: {}", "secretFiles: {}", "outputs: {}",
			"workspace: {access: none}", "retry: {maxAttempts: 1, backoffSeconds: 0, on: []}")))
		if minimal.Digest() != explicit.Digest() {
			t.Fatalf("explicit defaults must equal omitted defaults:\n%s\n%s", minimal.Canonical(), explicit.Canonical())
		}
		noInputs := e.mustValidate(t, strings.Replace(doc(container("a")), "  inputs: {}\n", "", 1))
		if noInputs.Digest() != minimal.Digest() {
			t.Fatal("absent inputs equal empty inputs")
		}
		withRequired := e.mustValidate(t, docWith("{a: {type: string}}", container("a")))
		explicitRequired := e.mustValidate(t, docWith("{a: {type: string, required: false}}", container("a")))
		if withRequired.Digest() != explicitRequired.Digest() {
			t.Fatal("required defaults to false")
		}
		requiredTrue := e.mustValidate(t, docWith("{a: {type: string, required: true}}", container("a")))
		if requiredTrue.Digest() == withRequired.Digest() {
			t.Fatal("a non-default value must change the digest")
		}
		if e.mustValidate(t, doc(container("a", "retry: {maxAttempts: 2, on: [transient]}"))).Digest() == minimal.Digest() {
			t.Fatal("a non-default retry must change the digest")
		}
		// An expression is content: its spelling is preserved, so the digest differs.
		a := e.mustValidate(t, docWith("{s: {type: string}}", container("a", "env: {A: '${{ inputs.s }}'}")))
		b := e.mustValidate(t, docWith("{s: {type: string}}", container("a", "env: {A: '${{inputs.s}}'}")))
		if a.Digest() == b.Digest() {
			t.Fatal("expression text is not normalized")
		}
	})

	t.Run("array order is meaningful", func(t *testing.T) {
		ab := e.mustValidate(t, doc(container("a"), container("b")))
		ba := e.mustValidate(t, doc(container("b"), container("a")))
		if ab.Digest() == ba.Digest() {
			t.Fatal("step order is preserved and changes the digest")
		}
		x := e.mustValidate(t, doc(container("a", "args: [one, two]")))
		y := e.mustValidate(t, doc(container("a", "args: [two, one]")))
		if x.Digest() == y.Digest() {
			t.Fatal("argument order is preserved and changes the digest")
		}
		n1 := e.mustValidate(t, doc(container("a"), container("b"), container("c", "needs: [a, b]")))
		n2 := e.mustValidate(t, doc(container("a"), container("b"), container("c", "needs: [b, a]")))
		if n1.Digest() == n2.Digest() {
			t.Fatal("dependency order is preserved and changes the digest")
		}
	})

	t.Run("the name is part of the digest", func(t *testing.T) {
		one := e.mustValidate(t, doc(container("a")))
		two := e.mustValidate(t, strings.Replace(doc(container("a")), "name: sample", "name: other", 1))
		if one.Digest() == two.Digest() || one.Name() != "sample" || two.Name() != "other" {
			t.Fatal("two names must not share a digest")
		}
	})

	t.Run("multiline commands keep their exact text", func(t *testing.T) {
		src := doc(container("a", `args: ["line one\nline two\ttabbed \"quoted\" <tag> & more\n"]`))
		v := e.mustValidate(t, src)
		d, err := v.Document()
		if err != nil {
			t.Fatal(err)
		}
		if got := d.Spec.Steps[0].Args[0]; got != "line one\nline two\ttabbed \"quoted\" <tag> & more\n" {
			t.Fatalf("argument text changed: %q", got)
		}
		if strings.Contains(string(v.Canonical()), `\u003c`) {
			t.Fatal("HTML escaping must be off")
		}
		literal := e.mustValidate(t, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: sample}\nspec:\n  steps:\n    - id: a\n      kind: container\n      image: "+img+"\n      command: [/bin/sh, -ec]\n      args:\n        - |\n          set -e\n          echo hi\n      timeoutSeconds: 60\n")
		ld, _ := literal.Document()
		if ld.Spec.Steps[0].Args[0] != "set -e\necho hi\n" {
			t.Fatalf("block scalar text changed: %q", ld.Spec.Steps[0].Args[0])
		}
	})

	t.Run("canonical content is a fixed point and round-trips", func(t *testing.T) {
		for _, name := range []string{"minimal-test", "build-and-publish", "m2-agent-gate"} {
			v := e.mustValidate(t, string(readFixture(t, "valid/"+name+".yaml")))
			again, err := e.v.ValidateJSON(t.Context(), scope, actor, v.Canonical())
			if err != nil {
				t.Fatalf("%s: canonical content must validate: %v", name, err)
			}
			if !bytes.Equal(again.Canonical(), v.Canonical()) || again.Digest() != v.Digest() {
				t.Fatalf("%s: canonicalization is not idempotent", name)
			}
			stored, err := pipeline.NewValidatedFromCanonical(v.Canonical(), v.Digest())
			if err != nil || stored.Digest() != v.Digest() || stored.Name() != v.Name() {
				t.Fatalf("%s: stored content did not reload: %v", name, err)
			}
		}
	})

	t.Run("stored content must match its digest and be canonical", func(t *testing.T) {
		v := e.mustValidate(t, doc(container("a")))
		tampered := bytes.Replace(v.Canonical(), []byte(`"timeoutSeconds":60`), []byte(`"timeoutSeconds":61`), 1)
		if _, err := pipeline.NewValidatedFromCanonical(tampered, v.Digest()); err == nil {
			t.Fatal("tampered content must not load")
		}
		pretty := []byte(strings.Replace(string(v.Canonical()), `{"apiVersion"`, "{ \"apiVersion\"", 1))
		if _, err := pipeline.NewValidatedFromCanonical(pretty, pipeline.DigestOf(pretty)); err == nil {
			t.Fatal("non-canonical content must not load even with a matching digest")
		}
		if _, err := pipeline.NewValidatedFromCanonical(v.Canonical(), "sha256:zz"); err == nil {
			t.Fatal("a malformed digest must not load")
		}
	})

	t.Run("accessors return defensive copies", func(t *testing.T) {
		v := e.mustValidate(t, doc(container("a")))
		before := v.Digest()
		c := v.Canonical()
		c[0] = 'X'
		if v.Canonical()[0] != '{' {
			t.Fatal("Canonical exposes internal storage")
		}
		d1, err := v.Document()
		if err != nil {
			t.Fatal(err)
		}
		d1.Spec.Steps[0].Command[0] = "mutated"
		d1.Spec.Steps[0].ID = "mutated"
		d1.Spec.Inputs["x"] = pipeline.Input{Type: pipeline.TypeString}
		d2, _ := v.Document()
		if d2.Spec.Steps[0].Command[0] != "/bin/sh" || d2.Spec.Steps[0].ID != "a" || len(d2.Spec.Inputs) != 0 {
			t.Fatal("Document shares state with the validated value")
		}
		if v.Digest() != before {
			t.Fatal("digest changed")
		}
	})

	t.Run("validation does not mutate or alias the submitted document", func(t *testing.T) {
		d, err := e.v.ParseYAML([]byte(doc(container("a"), container("b", "needs: [a]"))))
		if err != nil {
			t.Fatal(err)
		}
		v, err := e.v.Validate(t.Context(), scope, actor, d)
		if err != nil {
			t.Fatal(err)
		}
		if d.Spec.Steps[0].Workspace != nil || d.Spec.Steps[0].Retry != nil {
			t.Fatal("Validate must not apply defaults to the caller's document")
		}
		d.Spec.Steps[1].Needs[0] = "mutated"
		d.Spec.Steps[0].Command[0] = "mutated"
		after, _ := v.Document()
		if after.Spec.Steps[1].Needs[0] != "a" || after.Spec.Steps[0].Command[0] != "/bin/sh" {
			t.Fatal("the validated value aliases the caller's document")
		}
	})
}
