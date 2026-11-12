package examples_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// deliveryPhrases are the statements the delivery example must make: external
// registry ownership, digest pinning and attribution, retry without rebuilding,
// receipt/idempotency convergence, the unshadowable KODAMA_* context, and
// readiness as an observed result rather than a command exit.
var deliveryPhrases = []string{
	"external registry",
	"pinned",
	"sha256",
	"without rebuilding",
	"idempotenc",
	"receipt",
	"readiness",
	"not a kodama entity",
	"KODAMA_URL",
	"KODAMA_EXECUTION",
	"KODAMA_ACTION",
	"KODAMA_TASK",
	"KODAMA_TOKEN",
}

// deliveryHeadings are the required delivery README sections.
var deliveryHeadings = []string{
	"registry ownership",
	"promotion",
	"rollback",
	"readiness",
}

// TestDeliveryReadmeDocumentsRegistryAndRetries checks the delivery README for
// the pinned-image, external-registry, attribution, retry, idempotency,
// KODAMA_* and readiness statements.
func TestDeliveryReadmeDocumentsRegistryAndRetries(t *testing.T) {
	const rel = "delivery/README.md"
	data := readExample(t, rel)
	body := strings.ToLower(string(data))
	for _, phrase := range deliveryPhrases {
		if !strings.Contains(body, strings.ToLower(phrase)) {
			t.Fatalf("%s must state %q", rel, phrase)
		}
	}
	headings := markdownHeadings(t, examplePath(rel))
	for _, want := range deliveryHeadings {
		found := false
		for _, heading := range headings {
			if strings.Contains(strings.ToLower(heading), want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s must have a %q section; headings = %v", rel, want, headings)
		}
	}
}

// TestDeliveryReadmeQualifiesExecutionContext checks the delivery README's
// execution-context statement precisely: KODAMA_ACTION is the current action
// slug, and KODAMA_TASK is present only when the execution has an associated
// task and unset for task-free runs. The README must not claim KODAMA_TASK is
// supplied to every action.
func TestDeliveryReadmeQualifiesExecutionContext(t *testing.T) {
	const rel = "delivery/README.md"
	section := markdownSection(t, rel, "Unshadowable execution context")
	if section == "" {
		t.Fatalf("%s must document the unshadowable execution context", rel)
	}
	actionSentence := sentenceContaining(section, "KODAMA_ACTION")
	if actionSentence == "" {
		t.Fatalf("%s must describe KODAMA_ACTION", rel)
	}
	if !strings.Contains(actionSentence, "action slug") {
		t.Fatalf("%s must state KODAMA_ACTION is the current action slug; sentence = %q", rel, actionSentence)
	}
	taskSentence := sentenceContaining(section, "KODAMA_TASK")
	if taskSentence == "" {
		t.Fatalf("%s must describe KODAMA_TASK", rel)
	}
	for _, want := range []string{"associated task", "unset", "task-free"} {
		if !strings.Contains(taskSentence, want) {
			t.Fatalf("%s KODAMA_TASK statement must state %q; sentence = %q", rel, want, taskSentence)
		}
	}
	if strings.Contains(taskSentence, "every action") {
		t.Fatalf("%s must not claim KODAMA_TASK is supplied to every action; sentence = %q", rel, taskSentence)
	}
	if !strings.Contains(section, "plain.yaml") {
		t.Fatalf("%s must name a task-free example pipeline; section = %q", rel, section)
	}
}

// markdownSection returns the normalized body of one heading section, or "" when
// the heading is absent. Fenced code blocks cannot open or close a section.
func markdownSection(t *testing.T, rel, heading string) string {
	t.Helper()
	var body strings.Builder
	inSection := false
	inFence := false
	for line := range strings.SplitSeq(string(readExample(t, rel)), "\n") {
		if mdFence.MatchString(line) {
			inFence = !inFence
		}
		if !inFence {
			if match := mdHeading.FindStringSubmatch(line); match != nil {
				if inSection {
					break
				}
				inSection = strings.EqualFold(strings.TrimSpace(match[2]), heading)
				continue
			}
		}
		if inSection {
			body.WriteString(line)
			body.WriteString(" ")
		}
	}
	return strings.Join(strings.Fields(body.String()), " ")
}

// sentenceContaining returns the normalized sentence that contains marker, or ""
// when no sentence does.
func sentenceContaining(text, marker string) string {
	for sentence := range strings.SplitSeq(text, ". ") {
		if strings.Contains(sentence, marker) {
			return strings.TrimSpace(sentence)
		}
	}
	return ""
}

// TestDeliveryExamplesNameTheExampleRegistry checks internal consistency: the
// registry host used by the build example and the registered deploy resource
// used by promotion/rollback are the ones the delivery README documents.
func TestDeliveryExamplesNameTheExampleRegistry(t *testing.T) {
	readme := strings.ToLower(string(readExample(t, "delivery/README.md")))

	build := decodeExample(t, "pipelines/build.yaml")
	image := build.Actions[0].Container.Image
	reference, err := pipeline.ParseImageReference(image)
	if err != nil {
		t.Fatalf("build example image %q is not pinned: %v", image, err)
	}
	host := reference.Reference
	if index := strings.Index(host, "/"); index >= 0 {
		host = host[:index]
	}
	if host == "" {
		t.Fatalf("build example image %q has no registry host", image)
	}
	if !strings.Contains(readme, strings.ToLower(host)) {
		t.Fatalf("delivery README must document the external registry %q used by the build example", host)
	}

	promote := decodeExample(t, "pipelines/promote.yaml")
	approval := promote.Actions[1]
	deploy := literalField(t, "pipelines/promote.yaml", approval, "resource")
	if !strings.Contains(readme, strings.ToLower(deploy)) {
		t.Fatalf("delivery README must document the registered deploy resource %q", deploy)
	}
}

// artifactEntityPattern matches an artifact table/registration API, which the
// examples must not invent. Artifacts are output values only.
var artifactEntityPattern = regexp.MustCompile(`(?i)artifact[_-]id|/api/v1/artifact`)

// TestDeliveryExamplesCarryNoArtifactEntity checks that no example invents an
// artifact entity, table or registration API.
func TestDeliveryExamplesCarryNoArtifactEntity(t *testing.T) {
	for _, path := range walkExampleFiles(t, ".md", ".yaml", ".json") {
		data, err := readFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if artifactEntityPattern.Match(data) {
			t.Fatalf("%s references an artifact entity or artifact API; artifacts are output values only", path)
		}
	}
}

// TestExamplesCarryNoCredentialMaterial checks every example text file against
// the credential refusal gate. Examples declare secret names only.
func TestExamplesCarryNoCredentialMaterial(t *testing.T) {
	for _, path := range walkExampleFiles(t, ".md", ".yaml", ".json") {
		data, err := readFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if containsCredential(data) {
			t.Fatalf("%s carries credential material; examples declare secret names only", path)
		}
	}
}

// TestExamplesMarkdownLinksResolve checks that every relative Markdown link in
// the examples resolves, including heading anchors. The repository docs check
// does not walk examples/, so this package checks them.
func TestExamplesMarkdownLinksResolve(t *testing.T) {
	paths := walkExampleFiles(t, ".md")
	if len(paths) == 0 {
		t.Fatalf("examples has no Markdown files")
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assertMarkdownLinksResolve(t, path)
		})
	}
}
