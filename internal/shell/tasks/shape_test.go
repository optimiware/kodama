package tasks

import (
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Test_unit_tasks_no_state_machine pins what the task tracker is not: it has no
// operation that moves a task between states, parks it, blocks it on another task
// or marks it resolved, and no field that would hold such a state. Execution
// belongs to the run; the task projects it.
func Test_unit_tasks_no_state_machine(t *testing.T) {
	t.Run("the use cases are exactly the documented ones", func(t *testing.T) {
		var got []string
		typ := reflect.TypeOf(&Service{})
		for i := range typ.NumMethod() {
			got = append(got, typ.Method(i).Name)
		}
		want := []string{"Apply", "Create", "Edit", "Get", "Inbox", "List", "Rebuild", "RebuildProject", "Rerun", "Resolve", "Start"}
		if !slices.Equal(got, want) {
			t.Fatalf("service methods = %v, want %v", got, want)
		}
	})

	t.Run("the store has no transition, park, block or resolution-marker operation", func(t *testing.T) {
		var got []string
		typ := reflect.TypeOf((*Store)(nil)).Elem()
		for i := range typ.NumMethod() {
			got = append(got, typ.Method(i).Name)
		}
		want := []string{"Advance", "Create", "Get", "List", "Select", "TaskOfRun", "UpdateContent", "Waiting"}
		if !slices.Equal(got, want) {
			t.Fatalf("store methods = %v, want %v", got, want)
		}
	})

	t.Run("a task has no state, parking, blocking or resolution fields", func(t *testing.T) {
		banned := []string{"state", "parked", "resume", "blocked", "resolution", "resolved", "transition", "workflow"}
		var walk func(reflect.Type, string)
		walk = func(typ reflect.Type, path string) {
			for i := range typ.NumField() {
				f := typ.Field(i)
				name := strings.ToLower(f.Name)
				for _, b := range banned {
					if strings.Contains(name, b) {
						t.Errorf("%s.%s looks like workflow state", path, f.Name)
					}
				}
				if f.Anonymous && f.Type.Kind() == reflect.Struct {
					walk(f.Type, path+"."+f.Name)
				}
			}
		}
		walk(reflect.TypeOf(Task{}), "Task")
		walk(reflect.TypeOf(Progress{}), "Progress")
	})

	t.Run("the package does not depend on the legacy workflow engine or the blocking runner", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				p := strings.Trim(imp.Path.Value, `"`)
				if strings.HasSuffix(p, "/internal/tasks") || strings.Contains(p, "/internal/tasks/") ||
					strings.Contains(p, "/internal/kernel/runner") || strings.Contains(p, "/internal/shell/api") {
					t.Errorf("%s imports %s", e.Name(), p)
				}
			}
		}
	})
}
