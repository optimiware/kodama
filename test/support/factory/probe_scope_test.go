package factory

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// installFakePodman puts a deterministic fake `podman` first on PATH. The
// factory's production runner is harness.ExecRunner with an explicit
// environment, so a unit test can observe the exact argument vector without a
// container runtime. The caller's script receives the real argv as "$@".
func installFakePodman(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func requirePodmanArgs(t *testing.T, args []string, want ...string) {
	t.Helper()
	joined := "\x00" + strings.Join(args, "\x00") + "\x00"
	for index := 0; index+1 < len(want); index += 2 {
		pair := "\x00" + want[index] + "\x00" + want[index+1] + "\x00"
		if !strings.Contains(joined, pair) {
			t.Fatalf("podman arguments %v are missing %q %q", args, want[index], want[index+1])
		}
	}
}

// TestProbeContainerCarriesFixtureLabel proves the positive-control probe is
// a fixture resource: it must carry the fixture label next to the process
// cleanup labels. Starting it without the fixture label would make it invisible
// to fixture-scoped enumeration and removal, so one fixture's close could leave
// its probe behind (and a sibling probe would look like this fixture's).
func TestProbeContainerCarriesFixtureLabel(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "probe-args")
	installFakePodman(t, "printf '%s\\n' \"$@\" > '"+argsFile+"'")

	scope := fixtureScopeWithID("probe-fixture-id")
	h := &Harness{
		T:       t,
		fixture: scope,
		Image: runtimesupport.PinnedImage{
			Reference: "localhost/kodama-fixture-probe:image",
		},
	}
	if err := h.StartProbeContainer(context.Background(), "probe-unit", "kodama.dev/execution", "execution-7"); err != nil {
		t.Fatalf("StartProbeContainer: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read fake podman arguments: %v", err)
	}
	args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	requirePodmanArgs(t, args,
		"--name", "probe-unit",
		"--label", FixtureLabel+"="+scope.ID(),
		"--label", harness.LabelTest+"=1",
		"--label", harness.LabelOwnerPID+"="+runtimesupport.RunOwnerPID(),
		"--label", "kodama.dev/execution=execution-7",
	)
	if !strings.Contains("\x00"+strings.Join(args, "\x00")+"\x00", "\x00"+h.Image.Reference+"\x00") {
		t.Fatalf("probe arguments %v do not name the harness image %q", args, h.Image.Reference)
	}
}

// TestProbeContainerNameIsRunScoped proves the probe container name carries
// its run scope suffix and that two independent run scopes derive different
// names, so concurrent lifecycle runs cannot force-remove each other's probe.
func TestProbeContainerNameIsRunScoped(t *testing.T) {
	const base = "i04-b1-running-probe"

	name := ProbeContainerName(base)
	if name == base {
		t.Fatalf("ProbeContainerName(%q) = %q; want a run-scoped name", base, name)
	}
	if want := base + "-" + runtimesupport.RunScope().Suffix(); name != want {
		t.Fatalf("ProbeContainerName(%q) = %q; want %q", base, name, want)
	}

	firstScope := runtimesupport.NewScope()
	secondScope := runtimesupport.NewScope()
	first := probeContainerNameFor(firstScope, base)
	second := probeContainerNameFor(secondScope, base)
	if first == second {
		t.Fatalf("two run scopes derive one probe container name %q", first)
	}
	if want := firstScope.ID(base); first != want {
		t.Fatalf("first scoped probe name = %q; want %q", first, want)
	}
	if want := secondScope.ID(base); second != want {
		t.Fatalf("second scoped probe name = %q; want %q", second, want)
	}
	if first == base || second == base {
		t.Fatalf("run-scoped probe name stayed at the literal base %q (first %q, second %q)", base, first, second)
	}
}

// TestLifecycleProbeContainerNamesAreRunScoped is a source-level guard for the
// lifecycle suite's probe-container positive control. StartProbeContainer and
// RemoveProbeContainer act on one exact Podman name, so a literal name shared
// by two concurrent suite runs lets one run force-remove the other's probe (or
// makes `podman run --name` fail with "name already in use"). The guard parses
// the lifecycle package instead of importing it because that package is
// integration-tagged, and it rejects both a direct string literal and an
// identifier bound to a string literal in the same file.
func TestLifecycleProbeContainerNamesAreRunScoped(t *testing.T) {
	directory := filepath.Join("..", "..", "integration", "lifecycle")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read lifecycle package: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		// Identifiers bound to a plain string literal are literal names even
		// when the call site passes the identifier. A run-scoped value is a
		// call (ProbeContainerName), never a literal, so it is not collected.
		literalNames := map[string]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.ValueSpec:
				for index, name := range typed.Names {
					if index < len(typed.Values) && isStringLiteral(typed.Values[index]) {
						literalNames[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for index, target := range typed.Lhs {
					if index >= len(typed.Rhs) || !isStringLiteral(typed.Rhs[index]) {
						continue
					}
					if name, ok := target.(*ast.Ident); ok {
						literalNames[name.Name] = true
					}
				}
			}
			return true
		})

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			argument, guarded := probeNameArgumentIndex(call)
			if !guarded || argument >= len(call.Args) {
				return true
			}
			expression := ast.Unparen(call.Args[argument])
			switch typed := expression.(type) {
			case *ast.BasicLit:
				if typed.Kind == token.STRING {
					t.Errorf("%s: literal Podman name %s passed as a probe container name; use ProbeContainerName so concurrent runs cannot collide",
						files.Position(typed.Pos()), typed.Value)
				}
			case *ast.Ident:
				if literalNames[typed.Name] {
					t.Errorf("%s: %s is bound to a string literal and passed as a probe container name; use ProbeContainerName so concurrent runs cannot collide",
						files.Position(typed.Pos()), typed.Name)
				}
			}
			return true
		})
	}
}

// probeNameArgumentIndex reports whether call is one of the exact-name probe
// helpers and, if so, which argument is the container name.
func probeNameArgumentIndex(call *ast.CallExpr) (int, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return 0, false
	}
	switch selector.Sel.Name {
	case "StartProbeContainer", "RemoveProbeContainer", "RemoveContainerByName":
		return 1, true
	default:
		return 0, false
	}
}

func isStringLiteral(expression ast.Expr) bool {
	literal, ok := ast.Unparen(expression).(*ast.BasicLit)
	return ok && literal.Kind == token.STRING
}
