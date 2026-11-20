package docs

import (
	"strings"
	"testing"
)

func TestCatalogInvariants(t *testing.T) {
	topics := Topics()
	if len(topics) == 0 {
		t.Fatal("Topics() returned no embedded documentation topics")
	}

	seenSlugs := make(map[string]bool, len(topics))
	seenURIs := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if topic.Slug == "" {
			t.Fatal("topic has empty slug")
		}
		if seenSlugs[topic.Slug] {
			t.Fatalf("duplicate topic slug %q", topic.Slug)
		}
		seenSlugs[topic.Slug] = true

		wantURI := URIScheme + topic.Slug
		if topic.URI != wantURI {
			t.Fatalf("topic %q URI = %q; want %q", topic.Slug, topic.URI, wantURI)
		}
		if seenURIs[topic.URI] {
			t.Fatalf("duplicate topic URI %q", topic.URI)
		}
		seenURIs[topic.URI] = true

		if strings.TrimSpace(topic.Title) == "" {
			t.Fatalf("topic %q has empty title", topic.Slug)
		}
		if strings.TrimSpace(topic.Description) == "" {
			t.Fatalf("topic %q has empty description", topic.Slug)
		}
		if topic.MIMEType != MIMETypeMarkdown {
			t.Fatalf("topic %q mime type = %q; want %q", topic.Slug, topic.MIMEType, MIMETypeMarkdown)
		}
		if strings.TrimSpace(topic.Content) == "" {
			t.Fatalf("topic %q has empty content", topic.Slug)
		}
		if !strings.HasPrefix(topic.Content, "# ") {
			t.Fatalf("topic %q content does not start with a Markdown heading", topic.Slug)
		}

		bySlug, ok := Lookup(strings.ToUpper(topic.Slug))
		if !ok || bySlug.URI != topic.URI {
			t.Fatalf("Lookup(%q) = (%+v, %t); want match", topic.Slug, bySlug, ok)
		}
		byURI, ok := LookupByURI(topic.URI)
		if !ok || byURI.Slug != topic.Slug {
			t.Fatalf("LookupByURI(%q) = (%+v, %t); want match", topic.URI, byURI, ok)
		}
	}

	// Mutating the returned slice must not affect subsequent calls.
	topics[0].Slug = "mutated"
	if fresh := Topics(); fresh[0].Slug == "mutated" {
		t.Fatal("Topics() returned a shared slice")
	}

	if _, ok := Lookup("nonexistent"); ok {
		t.Fatal("Lookup accepted unknown slug")
	}
	if _, ok := LookupByURI("kodema://docs/nonexistent"); ok {
		t.Fatal("LookupByURI accepted unknown URI")
	}
}

func TestMCPInstructionsAndSkillDocument(t *testing.T) {
	instructions := MCPInstructions()
	if strings.TrimSpace(instructions) == "" {
		t.Fatal("MCPInstructions() is empty")
	}
	if len(instructions) > 4096 {
		t.Fatalf("MCPInstructions() is %d bytes; keep it concise (<= 4096 bytes)", len(instructions))
	}
	for _, topic := range Topics() {
		if !strings.Contains(instructions, topic.URI) {
			t.Errorf("MCPInstructions() does not reference topic URI %q", topic.URI)
		}
	}

	skillDoc := SkillDocument()
	if !strings.HasPrefix(skillDoc, "---\nname: kodema\ndescription: "+SkillDescription+"\n---") {
		t.Fatalf("SkillDocument() missing portable YAML frontmatter:\n%s", skillDoc)
	}
	if strings.Contains(skillDoc, URIScheme) {
		t.Fatalf("SkillDocument() should reference local references/<slug>.md rather than MCP URIs:\n%s", skillDoc)
	}
	for _, topic := range Topics() {
		ref := "references/" + topic.Slug + ".md"
		if !strings.Contains(skillDoc, ref) {
			t.Errorf("SkillDocument() missing reference %q", ref)
		}
	}

	all := AllMarkdown()
	for _, topic := range Topics() {
		if !strings.Contains(all, "# "+topic.Title) {
			t.Errorf("AllMarkdown() missing title for %q", topic.Slug)
		}
	}
}
