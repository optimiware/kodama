package client

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEarlierConstantsCompile references the enum constants consumers wrote
// against before the business-loop composition (T-075) renamed them in the
// generated code, with their types: if this stops compiling, a source-level
// break reached the public client.
func TestEarlierConstantsCompile(t *testing.T) {
	states := []WorkState{Backlog, Ready, Active, Review, Done, Cancelled}
	for i, want := range []string{"backlog", "ready", "active", "review", "done", "cancelled"} {
		if string(states[i]) != want {
			t.Errorf("state %d is %q, want %q", i, states[i], want)
		}
	}
	events := []ObservableEventType{RunCreated, RunStateChanged, StepAttemptStateChanged, WorkItemCreated, WorkItemTransitioned, WorkItemUpdated}
	for i, want := range []string{"run.created", "run.state_changed", "step_attempt.state_changed", "work_item.created", "work_item.transitioned", "work_item.updated"} {
		if string(events[i]) != want {
			t.Errorf("event %d is %q, want %q", i, events[i], want)
		}
	}
	actors := []RunsActorType{RunsActorTypeOperator, RunsActorTypeService, RunsActorTypeUser, RunsActorTypeWorkload}
	for i, want := range []string{"operator", "service", "user", "workload"} {
		if string(actors[i]) != want {
			t.Errorf("actor %d is %q, want %q", i, actors[i], want)
		}
	}
	spaces := []WorkspaceState{WorkspaceStateProvisioning, WorkspaceStateAvailable, WorkspaceStateArchived}
	for i, want := range []string{"provisioning", "available", "archived"} {
		if string(spaces[i]) != want {
			t.Errorf("workspace state %d is %q, want %q", i, spaces[i], want)
		}
	}
}

// TestExportedSurfaceOnlyGrows pins every exported top-level identifier of the
// client as released with the M2 composition (testdata/exported-baseline.txt,
// one "kind name" per line): types, functions, methods, constants and variables.
// A later fragment may add identifiers; removing or renaming one (the schema
// compatibility check does not see enum constant names) fails here. Regenerating
// the baseline is never the fix: add an alias to compat.go.
func TestExportedSurfaceOnlyGrows(t *testing.T) {
	have := exported(t)
	f, err := os.Open(filepath.Join("testdata", "exported-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var missing []string
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n++
		if !have[line] {
			missing = append(missing, line)
		}
	}
	if n < 1000 {
		t.Fatalf("the baseline lists only %d identifiers", n)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("exported identifiers of the released client are gone (add aliases to compat.go):\n%s", strings.Join(missing, "\n"))
	}
}

func exported(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					out["func "+d.Name.Name] = true
					continue
				}
				var recv string
				switch r := d.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					recv = r.X.(*ast.Ident).Name
				case *ast.Ident:
					recv = r.Name
				}
				out["method "+recv+"."+d.Name.Name] = true
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							out["type "+s.Name.Name] = true
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								out[d.Tok.String()+" "+n.Name] = true
							}
						}
					}
				}
			}
		}
	}
	return out
}
