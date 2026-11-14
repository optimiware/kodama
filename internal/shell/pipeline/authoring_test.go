package pipeline_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

func pushRefDocument(ref string) []byte {
	return []byte(fmt.Sprintf(`repository: repo-1
service: git-service
on:
  push:
    - %q
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`, ref))
}

func TestDecodeAcceptsExactRefsAndTrailingSlashPrefixes(t *testing.T) {
	accepted := []string{
		"refs/heads/main",
		"refs/heads/work/task-42",
		"refs/tags/v1.0.0",
		"refs/heads/",
		"refs/heads/work/",
		"refs/heads/feature/sub/",
		"refs/tags/",
	}
	for _, ref := range accepted {
		t.Run(ref, func(t *testing.T) {
			if _, err := pipeline.Decode(pushRefDocument(ref), pipeline.DefaultLimits); err != nil {
				t.Fatalf("Decode(%q) error = %v; want nil", ref, err)
			}
		})
	}
}

func TestDecodeRefusesInvalidPushRefsAndGlobPatterns(t *testing.T) {
	refused := []struct {
		name string
		ref  string
	}{
		{"empty", ""},
		{"bare refs slash", "refs/"},
		{"bare refs", "refs"},
		{"two-segment without slash", "refs/heads"},
		{"unanchored branch", "heads/main"},
		{"unanchored prefix", "work/"},
		{"glob star", "refs/heads/*"},
		{"glob prefix star", "refs/heads/work/*"},
		{"glob question", "refs/heads/task-?"},
		{"glob bracket open", "refs/heads/[a-z]"},
		{"glob bracket close", "refs/heads/a]"},
		{"backslash", `refs/heads/work\task`},
		{"double dot traversal", "refs/heads/../main"},
		{"double dot range", "refs/heads/a..b"},
		{"dot segment", "refs/heads/./main"},
		{"dot prefix segment", "refs/heads/./"},
		{"empty middle segment", "refs//heads/main"},
		{"trailing double slash", "refs/heads//"},
		{"space", "refs/heads/my branch"},
		{"tab", "refs/heads/my\tbranch"},
		{"newline", "refs/heads/my\nbranch"},
		{"control char", "refs/heads/my\x00branch"},
		{"delete char", "refs/heads/my\x7fbranch"},
		{"overlong", "refs/heads/" + strings.Repeat("a", 512)},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := pipeline.Decode(pushRefDocument(testCase.ref), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}
