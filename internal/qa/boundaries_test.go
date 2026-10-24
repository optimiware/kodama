package qa_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func importsOf(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, imp := range parsed.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		out = append(out, p)
	}
	return out
}

func sources(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("%s: %v", dir, err)
	}
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	return out
}

// TestModuleBoundaries enforces the hexagonal direction and the authority
// separation. The domain and application package imports only the standard
// library, the shared identifiers and the runtime contract: no driver, transport
// or peer module. Peer modules appear only in adapters (and the test fixture).
// No package of this module, tests included, may import the gate module, and
// neither the use case nor the adapters import the identity module: QA cannot
// approve anything because nothing here reaches a decision (DQ-06, AR-12). The
// work module's authority is injected into the adapters as a work.Authority; the
// qatest fixture alone plays the composition root and wires the real identity
// authority into it (it is test wiring, not product code).
func TestModuleBoundaries(t *testing.T) {
	allowedRoot := []string{"github.com/optimiware/kodama/contracts/runtime/v1", "github.com/optimiware/kodama/internal/domain"}
	for _, f := range sources(t, ".") {
		for _, p := range importsOf(t, f) {
			std := !strings.Contains(strings.Split(p, "/")[0], ".") && !strings.HasPrefix(p, "github.com/optimiware/kodama/")
			if !std && !slices.Contains(allowedRoot, p) {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
	forbiddenAdapters := []string{"net/http", "os/exec", "k8s.io/", "github.com/optimiware/kodama/internal/storage", "github.com/optimiware/kodama/internal/httpapi", "github.com/optimiware/kodama/internal/gates",
		"github.com/optimiware/kodama/internal/identity", "github.com/optimiware/kodama/internal/runtimeauth", "github.com/optimiware/kodama/internal/secrets", "github.com/optimiware/kodama/internal/delivery/postgres", "github.com/optimiware/kodama/internal/delivery/http",
		"github.com/optimiware/kodama/internal/execution/postgres", "github.com/optimiware/kodama/internal/artifacts/postgres", "github.com/optimiware/kodama/internal/artifacts/s3"}
	for _, f := range sources(t, "adapters") {
		for _, p := range importsOf(t, f) {
			for _, bad := range forbiddenAdapters {
				if strings.HasPrefix(p, bad) {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
		// The adapters build only create/edit/read work commands: no transition,
		// completion or cancellation vocabulary of the work module appears.
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, word := range []string{"TransitionCommand", ".Transition(", "StateDone", "StateReview", "PermComplete", "PermWork", "PermCancel", "PermReopen", "CompletionRecord", "PermGateDecide"} {
			if strings.Contains(string(src), word) {
				t.Errorf("%s mentions %s: QA adapters never move work", f, word)
			}
		}
	}
	for _, dir := range []string{".", "adapters", "qatest"} {
		forbidden := []string{"github.com/optimiware/kodama/internal/gates", "github.com/optimiware/kodama/internal/identity"}
		if dir == "qatest" {
			forbidden = []string{"github.com/optimiware/kodama/internal/gates"}
		}
		for _, f := range sources(t, dir) {
			for _, p := range importsOf(t, f) {
				for _, bad := range forbidden {
					if strings.HasPrefix(p, bad) {
						t.Errorf("%s imports %s", f, p)
					}
				}
			}
		}
	}
}
