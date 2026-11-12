package pgtest

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func writeUnitSource(t *testing.T, root, pkgDir, pkg, source string) {
	t.Helper()
	dir := filepath.Join(root, "internal", "shell", pkgDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "package " + pkg + "\n\n" + source
	if err := os.WriteFile(filepath.Join(dir, "types.go"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestUnitTypesAreQualifiedByPackageAndSkipTestsAndNonStructs(t *testing.T) {
	root := t.TempDir()
	writeUnitSource(t, root, "alpha", "alpha", "type CreateUnit struct{}\ntype NotAStructUnit int\ntype helper struct{}\n")
	writeUnitSource(t, root, "beta", "beta", "type CreateUnit struct{}\n")
	if err := os.WriteFile(filepath.Join(root, "internal", "shell", "alpha", "x_test.go"),
		[]byte("package alpha\n\ntype fakeUnit struct{}\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	got, err := unitTypes(filepath.Join(root, "internal", "shell"))
	if err != nil {
		t.Fatalf("unitTypes: %v", err)
	}
	if want := []string{"alpha.CreateUnit", "beta.CreateUnit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unitTypes = %v; want %v", got, want)
	}
}

func coverageFixture() (types []string, observed map[string][]string, mapping map[string][]string, exempt, nonUnit map[string]string) {
	types = []string{"store.Unit", "task.CreateUnit", "task.StartUnit"}
	observed = map[string][]string{
		"task.create": nil, "task.create.replay": nil, "task.create.loser": nil,
		"task.start": nil, "engine.commit": nil,
	}
	mapping = map[string][]string{
		"task.CreateUnit": {"task.create"},
		"task.StartUnit":  {"task.start"},
	}
	exempt = map[string]string{"store.Unit": "generic envelope"}
	nonUnit = map[string]string{"engine.commit": "engine port"}
	return
}

func TestCoverageGateAcceptsAConsistentFixture(t *testing.T) {
	types, observed, mapping, exempt, nonUnit := coverageFixture()
	if err := unitStateCoverageFor(types, observed, mapping, exempt, nonUnit); err != nil {
		t.Fatalf("consistent fixture rejected: %v", err)
	}
}

func TestCoverageGateFailsEveryKindOfGap(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(types *[]string, observed map[string][]string, mapping map[string][]string, exempt, nonUnit map[string]string)
		wantErr string
	}{
		{"unmapped source type", func(types *[]string, _ map[string][]string, _ map[string][]string, _, _ map[string]string) {
			*types = append(*types, "task.AskUnit")
		}, "task.AskUnit has no mapped"},
		{"mapped state never observed", func(_ *[]string, observed map[string][]string, _ map[string][]string, _, _ map[string]string) {
			delete(observed, "task.start")
		}, `state "task.start" was never observed`},
		{"stale mapping for a removed type", func(_ *[]string, _ map[string][]string, mapping map[string][]string, _, _ map[string]string) {
			mapping["task.GoneUnit"] = []string{"task.gone"}
		}, "task.GoneUnit does not exist in product source"},
		{"stale exemption", func(_ *[]string, _ map[string][]string, _ map[string][]string, exempt, _ map[string]string) {
			exempt["task.GoneUnit"] = "reason"
		}, "exempt unit type task.GoneUnit does not exist"},
		{"exemption without a reason", func(_ *[]string, _ map[string][]string, _ map[string][]string, exempt, _ map[string]string) {
			exempt["store.Unit"] = "  "
		}, "store.Unit has no reason"},
		{"observed state nobody maps", func(_ *[]string, observed map[string][]string, _ map[string][]string, _, _ map[string]string) {
			observed["task.mystery"] = nil
		}, `observed state "task.mystery" is not mapped`},
		{"replay of an unmapped base", func(_ *[]string, observed map[string][]string, _ map[string][]string, _, _ map[string]string) {
			observed["task.mystery.replay"] = nil
		}, `observed state "task.mystery.replay" is not mapped`},
		{"loser of an unmapped base", func(_ *[]string, observed map[string][]string, _ map[string][]string, _, _ map[string]string) {
			observed["task.mystery.loser"] = nil
		}, `observed state "task.mystery.loser" is not mapped`},
		{"non-unit state never observed", func(_ *[]string, _ map[string][]string, _ map[string][]string, _, nonUnit map[string]string) {
			nonUnit["engine.other"] = "reason"
		}, `non-unit state "engine.other" was never observed`},
		{"non-unit state without a reason", func(_ *[]string, _ map[string][]string, _ map[string][]string, _, nonUnit map[string]string) {
			nonUnit["engine.commit"] = ""
		}, `non-unit state "engine.commit" has no reason`},
		{"state claimed twice", func(_ *[]string, _ map[string][]string, mapping map[string][]string, _, _ map[string]string) {
			mapping["task.StartUnit"] = []string{"task.start", "task.create"}
		}, `state "task.create" is claimed by both`},
		{"type both mapped and exempt", func(_ *[]string, _ map[string][]string, _ map[string][]string, exempt, _ map[string]string) {
			exempt["task.StartUnit"] = "reason"
		}, "task.StartUnit is both mapped and exempt"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			types, observed, mapping, exempt, nonUnit := coverageFixture()
			testCase.mutate(&types, observed, mapping, exempt, nonUnit)
			err := unitStateCoverageFor(types, observed, mapping, exempt, nonUnit)
			if err == nil {
				t.Fatal("gate passed a gap")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("gate error = %v; want it to mention %q", err, testCase.wantErr)
			}
		})
	}
}

func TestCoverageGateReadsSourceAndRefusesAnEmptyRegistry(t *testing.T) {
	root := t.TempDir()
	writeUnitSource(t, root, "example", "example", "type ImaginaryUnit struct{ ID string }\n")
	if err := unitStateCoverageError(root, map[string][]string{"task.start": nil}); err == nil ||
		!strings.Contains(err.Error(), "example.ImaginaryUnit has no mapped") {
		t.Fatalf("gate = %v; want it to name example.ImaginaryUnit", err)
	}
	if err := unitStateCoverageError(root, map[string][]string{}); err == nil {
		t.Fatal("gate passed an empty registry")
	}
	if err := unitStateCoverageError(filepath.Join(root, "missing"), map[string][]string{"x": nil}); err == nil {
		t.Fatal("gate passed an unreadable source root")
	}
}

// TestUnitTypeStatesMatchProductSource keeps the table honest against the real
// product tree: the qualified *Unit types parsed from internal/shell must equal
// the mapped keys plus the exempt ones, so a new unit type cannot slip in.
func TestUnitTypeStatesMatchProductSource(t *testing.T) {
	parsed, err := unitTypes(filepath.Join("..", "..", "..", "..", "internal", "shell"))
	if err != nil {
		t.Fatalf("unitTypes: %v", err)
	}
	var tabled []string
	for name := range unitTypeStates {
		tabled = append(tabled, name)
	}
	for name := range exemptUnitTypes {
		tabled = append(tabled, name)
	}
	sort.Strings(tabled)
	if !reflect.DeepEqual(parsed, tabled) {
		t.Fatalf("product *Unit types %v\ndo not equal the table %v", parsed, tabled)
	}
	for name, states := range unitTypeStates {
		if len(states) == 0 {
			t.Errorf("%s maps no states", name)
		}
	}
}
