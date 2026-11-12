package examples_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// examplesRoot is the directory this package tests. `go test ./examples/...`
// runs with that directory as the working directory.
const examplesRoot = "."

// skillMaxBodyBytes mirrors the Markdown knowledge body bound (1 MiB)
// declared by the Userland skill contract. It is duplicated here so this
// package does not import the Userland skill package; the value is a contract
// constant, not behavior.
const skillMaxBodyBytes = 1 << 20

// examplePath resolves one root-relative example path.
func examplePath(rel string) string {
	return filepath.Join(examplesRoot, filepath.FromSlash(rel))
}

// readExample reads one required example file, failing with the missing asset
// name so an absent implementation is unambiguous.
func readExample(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(examplePath(rel))
	if err != nil {
		t.Fatalf("read example %s: %v", rel, err)
	}
	return data
}

// decodeExample decodes one shipped pipeline example through the real admission
// parser. A decode refusal is a fixture defect, not an expected red state.
func decodeExample(t *testing.T, rel string) definition.Pipeline {
	t.Helper()
	decoded, err := decodeRaw(t, rel)
	if err != nil {
		t.Fatalf("decode example pipeline %s: %v", rel, err)
	}
	return decoded
}

// decodeRaw decodes one YAML document without failing, so negative fixtures can
// assert the parser's typed refusal.
func decodeRaw(t *testing.T, rel string) (definition.Pipeline, error) {
	t.Helper()
	return pipeline.Decode(readExample(t, rel), pipeline.DefaultLimits)
}

// readFile reads one resolved example path.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// sortedDir returns the sorted file names with the supplied extension in dir.
// A missing directory fails: examples are required assets.
func sortedDir(t *testing.T, dir, extension string) []string {
	t.Helper()
	return sortedFilesWithExtensions(t, examplePath(dir), extension)
}

// sortedExampleYAML returns every shipped pipeline YAML in dir, accepting both
// the .yaml and .yml spellings the real `kodactl pipeline apply` inventories.
// The inventory must never silently skip one spelling: that would let an
// example escape admission, schema and CLI validation.
func sortedExampleYAML(t *testing.T, dir string) []string {
	t.Helper()
	return sortedFilesWithExtensions(t, examplePath(dir), ".yaml", ".yml")
}

// sortedFilesWithExtensions returns the sorted non-directory file names in
// directory that end with any supplied extension.
func sortedFilesWithExtensions(t *testing.T, directory string, extensions ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read example directory %s: %v", directory, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		for _, extension := range extensions {
			if strings.HasSuffix(entry.Name(), extension) {
				names = append(names, entry.Name())
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// exampleMissing returns the entries of expected that actual does not contain.
// The inventory tests use it in both directions so one deleted or unexpected
// asset is reported by name.
func exampleMissing(expected, actual []string) []string {
	present := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		present[name] = struct{}{}
	}
	var missing []string
	for _, name := range expected {
		if _, ok := present[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// requireSameExampleInventory fails with the exact missing and unexpected
// assets. Deletions are missing entries; a new spelling or an unrelated file is
// an unexpected entry.
func requireSameExampleInventory(t *testing.T, label string, got, want []string) {
	t.Helper()
	if missing := exampleMissing(want, got); len(missing) != 0 {
		t.Fatalf("%s is missing %v; a deleted example is an inventory change", label, missing)
	}
	if extra := exampleMissing(got, want); len(extra) != 0 {
		t.Fatalf("%s has unexpected %v; add it to the inventory or remove the file", label, extra)
	}
}

// actionKind names the single declared kind of one action.
func actionKind(t *testing.T, rel string, action definition.Action) string {
	t.Helper()
	switch {
	case action.Container != nil && action.Request != nil:
		t.Fatalf("%s action %q declares both kinds", rel, action.Name)
	case action.Container != nil:
		return "container"
	case action.Request != nil:
		return "request"
	default:
		t.Fatalf("%s action %q declares no kind", rel, action.Name)
	}
	return ""
}

// assertKinds checks the exact ordered action kinds of one example.
func assertKinds(t *testing.T, rel string, decoded definition.Pipeline, want ...string) {
	t.Helper()
	if len(decoded.Actions) != len(want) {
		t.Fatalf("%s declares %d actions; want %d (%v)", rel, len(decoded.Actions), len(want), want)
	}
	for index, action := range decoded.Actions {
		if got := actionKind(t, rel, action); got != want[index] {
			t.Fatalf("%s action %d (%q) kind = %s; want %s", rel, index, action.Name, got, want[index])
		}
	}
}

// assertPinnedImages requires every container image in one example to carry an
// immutable sha256 digest. Examples demonstrate pinned references; a mutable tag
// is never a pinned example.
func assertPinnedImages(t *testing.T, rel string, decoded definition.Pipeline) {
	t.Helper()
	for _, action := range decoded.Actions {
		if action.Container == nil {
			continue
		}
		if _, err := pipeline.ParseImageReference(action.Container.Image); err != nil {
			t.Fatalf("%s action %q image %q is not digest-pinned: %v", rel, action.Name, action.Container.Image, err)
		}
	}
}

// assertAssignment checks the request assignment grammar: exactly
// role/<current human role> or service/<service id>.
func assertAssignment(t *testing.T, rel string, action definition.Action) {
	t.Helper()
	if action.Request == nil {
		t.Fatalf("%s action %q is not a request", rel, action.Name)
	}
	value := action.Request.Assignment
	if strings.Count(value, "/") != 1 {
		t.Fatalf("%s action %q assignment %q must be exactly role/<role> or service/<id>", rel, action.Name, value)
	}
	prefix, name, _ := strings.Cut(value, "/")
	switch prefix {
	case "role":
		role := tenancy.Role(name)
		human := slices.Contains(tenancy.HumanRoles(), role)
		if !human {
			t.Fatalf("%s action %q assignment %q does not name a current human role", rel, action.Name, value)
		}
	case "service":
		if _, err := service.ParseServiceID(name); err != nil {
			t.Fatalf("%s action %q assignment %q names an invalid service id: %v", rel, action.Name, value, err)
		}
	default:
		t.Fatalf("%s action %q assignment %q must start with role/ or service/", rel, action.Name, value)
	}
}

// subjectField returns one immutable subject field of a request action.
func subjectField(t *testing.T, rel string, action definition.Action, name string) definition.SubjectValue {
	t.Helper()
	if action.Request == nil {
		t.Fatalf("%s action %q is not a request", rel, action.Name)
	}
	fields := action.Request.Subject.Fields()
	value, ok := fields[name]
	if !ok {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		t.Fatalf("%s action %q subject has no %q field; fields = %v", rel, action.Name, name, keys)
	}
	return value
}

// literalField requires one subject field to be a literal and returns it.
func literalField(t *testing.T, rel string, action definition.Action, name string) string {
	t.Helper()
	value := subjectField(t, rel, action, name)
	if value.Kind != definition.SubjectLiteral {
		t.Fatalf("%s action %q subject field %q must be a literal; got kind %s", rel, action.Name, name, value.Kind)
	}
	if value.Literal == "" {
		t.Fatalf("%s action %q subject field %q literal is empty", rel, action.Name, name)
	}
	return value.Literal
}

// referenceField requires one subject field to be a direct lookup and returns
// its parsed reference.
func referenceField(t *testing.T, rel string, action definition.Action, name string) definition.Reference {
	t.Helper()
	value := subjectField(t, rel, action, name)
	if value.Kind != definition.SubjectReference {
		t.Fatalf("%s action %q subject field %q must be a direct lookup; got kind %s", rel, action.Name, name, value.Kind)
	}
	return value.Reference
}

// outputReference requires one subject field to reference a declared output of
// an earlier action.
func outputReference(t *testing.T, rel string, action definition.Action, name, wantAction, wantKey string) {
	t.Helper()
	reference := referenceField(t, rel, action, name)
	if reference.Input {
		t.Fatalf("%s action %q subject field %q must reference an action output, not an input", rel, action.Name, name)
	}
	if string(reference.Action) != wantAction || string(reference.Key) != wantKey {
		t.Fatalf("%s action %q subject field %q references %s.outputs.%s; want %s.outputs.%s",
			rel, action.Name, name, reference.Action, reference.Key, wantAction, wantKey)
	}
}

// hasOutput reports whether one action declares an output key.
func hasOutput(action definition.Action, key string) bool {
	var outputs []string
	switch {
	case action.Container != nil:
		for _, output := range action.Container.Outputs {
			outputs = append(outputs, string(output))
		}
	case action.Request != nil:
		for _, output := range action.Request.Outputs {
			outputs = append(outputs, string(output))
		}
	}
	return slices.Contains(outputs, key)
}

// inputKeys renders the declared pipeline inputs.
func inputKeys(decoded definition.Pipeline) []string {
	keys := make([]string, 0, len(decoded.Inputs))
	for _, key := range decoded.Inputs {
		keys = append(keys, string(key))
	}
	return keys
}

// assertInputs checks the exact ordered declared input keys of one example.
func assertInputs(t *testing.T, rel string, decoded definition.Pipeline, want ...string) {
	t.Helper()
	got := inputKeys(decoded)
	if len(got) != len(want) {
		t.Fatalf("%s inputs = %v; want %v", rel, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s inputs = %v; want %v", rel, got, want)
		}
	}
}

// assertResourceID checks a subject literal is a registered-resource identity,
// not a Git path.
func assertResourceID(t *testing.T, rel, name, value string) {
	t.Helper()
	if _, err := resource.ParseResourceID(value); err != nil {
		t.Fatalf("%s subject %q = %q is not a registered resource id: %v", rel, name, value, err)
	}
}

// walkExampleFiles returns every example file with one of the supplied
// extensions, sorted, so checks are deterministic.
func walkExampleFiles(t *testing.T, extensions ...string) []string {
	t.Helper()
	allowed := make(map[string]bool, len(extensions))
	for _, extension := range extensions {
		allowed[extension] = true
	}
	var paths []string
	err := filepath.WalkDir(examplesRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if allowed[filepath.Ext(entry.Name())] {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
	sort.Strings(paths)
	return paths
}

// secretPatterns mirrors the Userland knowledge credential refusal gate
// (`skill.ContainsSecret`). Examples must already satisfy the loader that
// will read them, but this package does not import the Userland skill package.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)authorization:[ \t]*bearer[ \t]+\S+`),
	regexp.MustCompile(`(?i)\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?im)(^|[^A-Za-z0-9])([A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key(?:[_-]?id)?|private[_-]?key)(?:[_-](?:access[_-]?key|key|id|token|secret|value))?)(["']?[ \t]*[=:][ \t]*)((?:'[^'\r\n]*[^\s'\r\n][^'\r\n]*'|"[^"\r\n]*[^\s"\r\n][^"\r\n]*"|"[^"\r\n]*$|'[^'\r\n]*$|[^\s"',;][^\s,;]*))`),
}

// containsCredential reports whether the bytes carry credential material.
func containsCredential(data []byte) bool {
	for _, pattern := range secretPatterns {
		if pattern.Match(data) {
			return true
		}
	}
	return false
}

var (
	mdFence      = regexp.MustCompile("^\\s*(```|~~~)")
	mdInlineCode = regexp.MustCompile("`[^`]*`")
	mdInlineLink = regexp.MustCompile(`!?\[[^\]]*\]\(([^()\s]+)(?:\s+"[^"]*")?\)`)
	mdHeading    = regexp.MustCompile(`^(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	mdLinkText   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdCodeSpan   = regexp.MustCompile("`([^`]*)`")
	mdHTMLTag    = regexp.MustCompile(`<[^>]*>`)
	externalLink = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// markdownHeadings returns the heading text of one Markdown file.
func markdownHeadings(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read markdown %s: %v", path, err)
	}
	var headings []string
	inFence := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if mdFence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		match := mdHeading.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		headings = append(headings, match[2])
	}
	return headings
}

// markdownAnchors returns the GitHub-style anchors of one Markdown file.
func markdownAnchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	anchors := make(map[string]bool)
	used := make(map[string]int)
	for _, heading := range markdownHeadings(t, path) {
		base := slugify(heading)
		if base == "" {
			continue
		}
		count := used[base]
		used[base] = count + 1
		if count == 0 {
			anchors[base] = true
		} else {
			anchors[fmt.Sprintf("%s-%d", base, count)] = true
		}
	}
	return anchors
}

// slugify mirrors the repository's GitHub-style heading slug.
func slugify(text string) string {
	text = mdLinkText.ReplaceAllString(text, "$1")
	text = mdCodeSpan.ReplaceAllString(text, "$1")
	text = mdHTMLTag.ReplaceAllString(text, "")
	text = strings.ToLower(text)
	var builder strings.Builder
	for _, character := range text {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			builder.WriteRune(character)
		case character == '-' || character == '_':
			builder.WriteRune(character)
		case unicode.IsSpace(character):
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

// assertMarkdownLinksResolve checks every relative link in one Markdown file.
func assertMarkdownLinksResolve(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read markdown %s: %v", path, err)
	}
	rel, err := filepath.Rel(examplesRoot, path)
	if err != nil {
		rel = path
	}
	inFence := false
	lineNumber := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		lineNumber++
		if mdFence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = mdInlineCode.ReplaceAllString(line, "")
		for _, match := range mdInlineLink.FindAllStringSubmatch(line, -1) {
			target := match[1]
			if target == "" || externalLink.MatchString(target) || strings.HasPrefix(target, "//") {
				continue
			}
			pathPart, anchor, _ := strings.Cut(target, "#")
			targetPath := path
			if pathPart != "" {
				decoded, err := url.PathUnescape(pathPart)
				if err != nil {
					t.Fatalf("%s:%d link %q has an invalid path: %v", rel, lineNumber, target, err)
				}
				targetPath = filepath.Join(filepath.Dir(path), filepath.FromSlash(decoded))
			}
			if _, err := os.Stat(targetPath); err != nil {
				t.Fatalf("%s:%d link %q does not resolve: %v", rel, lineNumber, target, err)
			}
			if anchor != "" && strings.EqualFold(filepath.Ext(targetPath), ".md") {
				if !markdownAnchors(t, targetPath)[anchor] {
					t.Fatalf("%s:%d link %q names a missing heading anchor", rel, lineNumber, target)
				}
			}
		}
	}
}
