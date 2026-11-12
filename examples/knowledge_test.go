package examples_test

import (
	"strings"
	"testing"
)

// knowledgeFiles is the required Markdown knowledge example set: the context
// repository conventions plus the reusable skills the delivery recipes name.
var knowledgeFiles = []string{
	"knowledge/README.md",
	"knowledge/conventions.md",
	"knowledge/skills/build-and-test.md",
	"knowledge/skills/promote-and-verify.md",
}

// TestKnowledgeExamplesExistAndAreBounded checks that every knowledge example
// exists, is nonempty and stays inside the 1 MiB Markdown body bound.
func TestKnowledgeExamplesExistAndAreBounded(t *testing.T) {
	for _, rel := range knowledgeFiles {
		t.Run(rel, func(t *testing.T) {
			data := readExample(t, rel)
			if len(data) == 0 {
				t.Fatalf("%s is empty", rel)
			}
			if len(data) > skillMaxBodyBytes {
				t.Fatalf("%s is %d bytes; at most %d are allowed", rel, len(data), skillMaxBodyBytes)
			}
			if strings.TrimSpace(string(data)) == "" {
				t.Fatalf("%s has no content", rel)
			}
		})
	}
}

// TestSkillExampleNamesAreValidSlugs checks that skill files are named with the
// lowercase slug grammar, so the knowledge loader can address them by name.
func TestSkillExampleNamesAreValidSlugs(t *testing.T) {
	names := sortedDir(t, "knowledge/skills", ".md")
	if len(names) == 0 {
		t.Fatalf("examples/knowledge/skills has no skill examples; want %v", knowledgeFiles)
	}
	for _, name := range names {
		slug := strings.TrimSuffix(name, ".md")
		t.Run(slug, func(t *testing.T) {
			if !validSkillSlug(slug) {
				t.Fatalf("skill file %q does not use the lowercase slug grammar", name)
			}
		})
	}
}

// TestKnowledgeReadmeExplainsContextRepositories checks that the knowledge
// README explains where knowledge lives and what a skill is.
func TestKnowledgeReadmeExplainsContextRepositories(t *testing.T) {
	const rel = "knowledge/README.md"
	body := strings.ToLower(string(readExample(t, rel)))
	for _, phrase := range []string{"context", "skill", "markdown"} {
		if !strings.Contains(body, phrase) {
			t.Fatalf("%s must explain %q for context-repository knowledge", rel, phrase)
		}
	}
}

// TestKnowledgeExamplesCarryNoCredentialMaterial checks that no knowledge
// example carries credential material. The same refusal gate the Userland
// loader applies is mirrored here so examples cannot ship a secret.
func TestKnowledgeExamplesCarryNoCredentialMaterial(t *testing.T) {
	for _, rel := range knowledgeFiles {
		data := readExample(t, rel)
		if containsCredential(data) {
			t.Fatalf("%s carries credential material; knowledge declares no secret values", rel)
		}
	}
}

// validSkillSlug mirrors the lowercase skill-name grammar: bounded,
// lowercase alphanumeric segments separated by single hyphens, not starting
// with a digit or hyphen.
func validSkillSlug(value string) bool {
	const maxNameLength = 64
	if value == "" || len(value) > maxNameLength {
		return false
	}
	previousHyphen := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
			previousHyphen = false
		case character >= '0' && character <= '9':
			if index == 0 {
				return false
			}
			previousHyphen = false
		case character == '-':
			if index == 0 || previousHyphen {
				return false
			}
			previousHyphen = true
		default:
			return false
		}
	}
	return !previousHyphen
}
