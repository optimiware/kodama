package agents_test

import (
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
)

func TestContentValidation(t *testing.T) {
	good := agentstest.Draft(t, "coding")
	if _, err := agents.NewContent(good); err != nil {
		t.Fatalf("a valid draft: %v", err)
	}

	paths := func(err error) []string {
		var ve *agents.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("expected a validation error, got %v", err)
		}
		var out []string
		for _, d := range ve.Diagnostics {
			out = append(out, d.Path)
		}
		return out
	}
	bad := func(name string, mutate func(*agents.Draft), path string) {
		t.Run(name, func(t *testing.T) {
			d := agentstest.Draft(t, "coding")
			mutate(&d)
			_, err := agents.NewContent(d)
			if !errors.Is(err, agents.ErrInvalid) {
				t.Fatalf("expected ErrInvalid, got %v", err)
			}
			found := false
			for _, p := range paths(err) {
				found = found || p == path
			}
			if !found {
				t.Fatalf("diagnostic at %s missing: %v", path, paths(err))
			}
			if strings.Contains(err.Error(), "Implement the work item") {
				t.Fatal("an error must never echo instruction content")
			}
		})
	}
	neg := int64(-1)
	zero := int64(0)
	bad("moving image tag", func(d *agents.Draft) { d.Image = "registry.example.invalid/agent:latest" }, "image")
	bad("bad name", func(d *agents.Draft) { d.Name = "Coding" }, "name")
	bad("empty instructions", func(d *agents.Draft) { d.Instructions = "" }, "instructions")
	bad("oversized instructions", func(d *agents.Draft) { d.Instructions = strings.Repeat("a", agents.MaxInstructionBytes+1) }, "instructions")
	bad("NUL in instructions", func(d *agents.Draft) { d.Instructions = "a\x00b" }, "instructions")
	bad("endpoint with credentials", func(d *agents.Draft) { d.Model.Endpoint = "https://user:secret@gateway.example.invalid/v1" }, "model.endpoint")
	bad("endpoint with query", func(d *agents.Draft) { d.Model.Endpoint = "https://gateway.example.invalid/v1?key=abc" }, "model.endpoint")
	bad("plain http endpoint", func(d *agents.Draft) { d.Model.Endpoint = "http://gateway.example.invalid/v1" }, "model.endpoint")
	bad("wall time zero", func(d *agents.Draft) { d.Limits.WallSeconds = 0 }, "limits.wallSeconds")
	bad("wall time too large", func(d *agents.Draft) { d.Limits.WallSeconds = agents.MaxWallSeconds + 1 }, "limits.wallSeconds")
	bad("negative request limit", func(d *agents.Draft) { d.Limits.MaxRequests = &neg }, "limits.maxRequests")
	bad("zero token limit", func(d *agents.Draft) { d.Limits.MaxTokens = &zero }, "limits.maxTokens")
	bad("duplicate tool", func(d *agents.Draft) { d.Tools = []string{"repo.read", "repo.read"} }, "tools[1]")
	bad("invalid tool", func(d *agents.Draft) { d.Tools = []string{"bad tool"} }, "tools[0]")
	bad("expression in command", func(d *agents.Draft) { d.Wrapper.Command = []string{"${{ inputs.x }}"} }, "wrapper.command[0]")
	bad("no command", func(d *agents.Draft) { d.Wrapper.Command = nil }, "wrapper.command")
	bad("bad wrapper version", func(d *agents.Draft) { d.Wrapper.Version = "1 2" }, "wrapper.version")
	bad("moving design reference", func(d *agents.Draft) {
		d.DesignBaseline.Sources = []runtimev1.SourceRef{{Repository: "https://git.example.invalid/k.git", Revision: "main", Path: "docs/x.md"}}
	}, "designBaseline")
	bad("no design requirements", func(d *agents.Draft) { d.DesignBaseline.Requirements = nil }, "designBaseline")
	bad("mismatched baseline digest", func(d *agents.Draft) {
		d.DesignBaseline.Digest = runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
	}, "designBaseline.digest")

	t.Run("too many tools", func(t *testing.T) {
		d := agentstest.Draft(t, "coding")
		for i := 0; i <= agents.MaxTools; i++ {
			d.Tools = append(d.Tools, "tool."+strings.Repeat("a", 3)+string(rune('a'+i%26))+strings.Repeat("b", i/26))
		}
		if _, err := agents.NewContent(d); !errors.Is(err, agents.ErrInvalid) {
			t.Fatalf("expected ErrInvalid, got %v", err)
		}
	})
}

func TestContentIsCanonicalAndDeterministic(t *testing.T) {
	a := agentstest.Draft(t, "coding")
	b := agentstest.Draft(t, "coding")
	b.Tools = []string{"repo.search", "repo.read"} // order is not intent
	ca, cb := agentstest.Content(t, a), agentstest.Content(t, b)
	if ca.Digest() != cb.Digest() || string(ca.Canonical()) != string(cb.Canonical()) {
		t.Fatal("tool order must not change the canonical content")
	}
	c := agentstest.Draft(t, "coding")
	c.Instructions += "!"
	if agentstest.Content(t, c).Digest() == ca.Digest() {
		t.Fatal("changed instructions must change the digest")
	}
	d := agentstest.Draft(t, "coding")
	d.Name = "other"
	if agentstest.Content(t, d).Digest() == ca.Digest() {
		t.Fatal("the name is part of the content")
	}

	// A caller cannot alias the stored content through the input or the accessor.
	in := agentstest.Draft(t, "coding")
	content := agentstest.Content(t, in)
	in.Tools[0] = "mutated"
	in.Wrapper.Command[0] = "mutated"
	got := content.Draft()
	got.Tools[0] = "mutated"
	got.Wrapper.Command[0] = "mutated"
	if content.Draft().Tools[0] == "mutated" || content.Draft().Wrapper.Command[0] == "mutated" {
		t.Fatal("content must be immutable")
	}
	canonical := content.Canonical()
	canonical[0] = 'x'
	if content.Canonical()[0] == 'x' {
		t.Fatal("the canonical bytes must be a copy")
	}

	t.Run("claims follow the content", func(t *testing.T) {
		d := agentstest.Draft(t, "coding")
		if got := agentstest.Content(t, d).Claims(); len(got) != len(agents.Controls()) {
			t.Fatalf("a fully claiming draft claims every control: %v", got)
		}
		d.Limits.MaxRequests, d.Limits.MaxTokens, d.Limits.MaxToolCalls = nil, nil, nil
		d.Tools = nil
		d.Transcript.Record = false
		got := agentstest.Content(t, d).Claims()
		if len(got) != 1 || got[0] != agents.ControlModel {
			t.Fatalf("a minimal draft claims only the model: %v", got)
		}
	})
}

func TestContentFromCanonicalRejectsCorruption(t *testing.T) {
	c := agentstest.Content(t, agentstest.Draft(t, "coding"))
	if got, err := agents.ContentFromCanonical(c.Canonical(), c.Digest()); err != nil || got.Digest() != c.Digest() {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := agents.ContentFromCanonical(c.Canonical(), agents.Digest("sha256:"+strings.Repeat("0", 64))); !errors.Is(err, agents.ErrConflict) {
		t.Fatalf("a wrong digest must be a conflict: %v", err)
	}
	tampered := strings.Replace(string(c.Canonical()), "wallSeconds\":1800", "wallSeconds\":1801", 1)
	if _, err := agents.ContentFromCanonical([]byte(tampered), c.Digest()); !errors.Is(err, agents.ErrConflict) {
		t.Fatalf("tampered content must be a conflict: %v", err)
	}
	if _, err := agents.ContentFromCanonical([]byte(`{"name":"x"}`), c.Digest()); !errors.Is(err, agents.ErrConflict) {
		t.Fatalf("partial content must be a conflict: %v", err)
	}
	if _, err := agents.DecodeDraft([]byte(`{"name":"a","unknown":1}`)); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("unknown fields must be rejected: %v", err)
	}
	if _, err := agents.DecodeDraft(append(c.Canonical(), []byte(` {}`)...)); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("trailing data must be rejected: %v", err)
	}
}

func TestReferences(t *testing.T) {
	name, version, err := agents.ParseReference("coding@3")
	if err != nil || name != "coding" || version != 3 {
		t.Fatalf("parse: %q %d %v", name, version, err)
	}
	if got := agents.Ref("coding", domain.Version(3)); got != "coding@3" {
		t.Fatal(got)
	}
	for _, ref := range []string{"coding", "coding@", "coding@0", "coding@03", "coding@-1", "coding@latest", "@1", "Coding@1", "coding@1@2", "coding@+1",
		"coding@99999999999999999", "coding@1 ", "coding@main"} {
		if _, _, err := agents.ParseReference(ref); !errors.Is(err, agents.ErrInvalid) {
			t.Errorf("%q must be rejected as a moving or malformed reference: %v", ref, err)
		}
	}
}

func TestCatalogue(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	e := agentstest.Entry(d)
	cat, err := agents.NewCatalogue(e)
	if err != nil {
		t.Fatal(err)
	}
	q, err := cat.Qualify(t.Context(), e.Tuple)
	if err != nil || q.Evidence == "" || !q.Supports(agents.ControlTokens) {
		t.Fatalf("qualify: %+v %v", q, err)
	}
	for name, mutate := range map[string]func(*agents.Tuple){
		"image":           func(t *agents.Tuple) { t.Image = "registry.example.invalid/other@" + agentstest.Digest1 },
		"wrapper id":      func(t *agents.Tuple) { t.WrapperID = "other" },
		"wrapper version": func(t *agents.Tuple) { t.WrapperVersion = "9.9.9" },
		"command":         func(t *agents.Tuple) { t.Command = []string{"/bin/sh"} },
		"harness":         func(t *agents.Tuple) { t.HarnessID = "other" },
		"harness version": func(t *agents.Tuple) { t.HarnessVersion = "9.9.9" },
	} {
		tup := e.Tuple
		tup.Command = append([]string{}, tup.Command...)
		mutate(&tup)
		if _, err := cat.Qualify(t.Context(), tup); !errors.Is(err, agents.ErrUnsupported) {
			t.Errorf("a different %s must not qualify: %v", name, err)
		}
	}
	empty, err := agents.NewCatalogue()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Qualify(t.Context(), e.Tuple); !errors.Is(err, agents.ErrUnsupported) {
		t.Fatalf("an empty catalogue qualifies nothing: %v", err)
	}
	noEvidence := e
	noEvidence.Evidence = ""
	if _, err := agents.NewCatalogue(noEvidence); err == nil {
		t.Fatal("an entry without evidence is not support")
	}
	badControl := e
	badControl.Controls = []agents.Control{"telepathy"}
	if _, err := agents.NewCatalogue(badControl); err == nil {
		t.Fatal("an unknown control must be refused")
	}
	if _, err := agents.NewCatalogue(e, e); err == nil {
		t.Fatal("a duplicate tuple must be refused")
	}
	var nilCat *agents.Catalogue
	if _, err := nilCat.Qualify(t.Context(), e.Tuple); err == nil {
		t.Fatal("a nil catalogue must not qualify")
	}
}
