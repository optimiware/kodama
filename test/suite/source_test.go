package suite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// repositoryRoot is the module root, two levels above this package.
var repositoryRoot = filepath.Join("..", "..")

// testFunctions parses every _test.go file of one repository directory and
// returns its top-level Test functions by name. The files are parsed, not
// compiled, so build tags do not hide them.
func testFunctions(t *testing.T, directory string) map[string]*ast.FuncDecl {
	t.Helper()
	path := filepath.Join(repositoryRoot, directory)
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	functions := map[string]*ast.FuncDecl{}
	files := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, filepath.Join(path, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", directory, entry.Name(), err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && strings.HasPrefix(function.Name.Name, "Test") {
				functions[function.Name.Name] = function
			}
		}
	}
	if len(functions) == 0 {
		t.Fatalf("%s declares no test", directory)
	}
	return functions
}

// startsWith reports whether the function's first statement calls the named
// package function.
func startsWith(function *ast.FuncDecl, name string) bool {
	if function.Body == nil || len(function.Body.List) == 0 {
		return false
	}
	statement, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

// calls reports whether the function calls name anywhere: a package function,
// or a selector such as factory.StartKubernetes or t.Parallel.
func calls(function *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(function, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch callee := call.Fun.(type) {
		case *ast.Ident:
			found = found || callee.Name == name
		case *ast.SelectorExpr:
			found = found || callee.Sel.Name == name
		}
		return !found
	})
	return found
}

// TestPortableE2ESelectionMatchesThePackage proves the run pattern, the
// expected-test list and test/e2e agree, and that every selected proof made an
// explicit concurrency decision: it enters the e2e gate first, or it is listed
// as serial and is never parallel.
func TestPortableE2ESelectionMatchesThePackage(t *testing.T) {
	functions := testFunctions(t, "test/e2e")
	pattern := regexp.MustCompile(PortableE2ERunPattern)
	var selected []string
	for name := range functions {
		if pattern.MatchString(name) {
			selected = append(selected, name)
		}
	}
	sort.Strings(selected)
	if !slices.Equal(selected, PortableE2ETests) {
		t.Fatalf("PortableE2ERunPattern selects\n  %v\nbut PortableE2ETests lists\n  %v", selected, PortableE2ETests)
	}
	for _, excluded := range []string{"TestReleaseSmoke", "TestE2E_WorkloadScriptsKeepTokensOutOfArgv"} {
		if functions[excluded] == nil {
			t.Errorf("test/e2e no longer declares %s; update this exclusion check", excluded)
		}
		if pattern.MatchString(excluded) {
			t.Errorf("PortableE2ERunPattern selects %s", excluded)
		}
	}
	for _, name := range SerialE2ETests {
		if !slices.Contains(PortableE2ETests, name) {
			t.Errorf("SerialE2ETests names %s, which is not a portable proof", name)
		}
	}
	for _, name := range PortableE2ETests {
		function := functions[name]
		if slices.Contains(SerialE2ETests, name) {
			if calls(function, "e2eParallel") || calls(function, "Parallel") {
				t.Errorf("%s is listed as serial but runs in parallel", name)
			}
			continue
		}
		if !startsWith(function, "e2eParallel") {
			t.Errorf("%s must start with e2eParallel(t), or be listed in SerialE2ETests with the project-wide state it asserts on", name)
		}
	}
}

// TestLifecycleKindTestsFollowTheLaneSplit proves the -run/-skip ^TestKubernetes
// split of the lifecycle package is exact: every test that starts a kind
// cluster carries the prefix, matches KindLifecycleTests so the verifying
// wrapper checks it, and takes a kind slot first.
func TestLifecycleKindTestsFollowTheLaneSplit(t *testing.T) {
	functions := testFunctions(t, "test/integration/lifecycle")
	pattern := regexp.MustCompile("^TestKubernetes")
	var selected []string
	for name, function := range functions {
		matchesPrefix := pattern.MatchString(name)
		startsCluster := calls(function, "StartKubernetes")
		if matchesPrefix {
			selected = append(selected, name)
		}
		if startsCluster && !matchesPrefix {
			t.Errorf("%s starts a kind cluster; name it TestKubernetes* so the kind lane selects it and the stack lane skips it", name)
		}
		if matchesPrefix && !startsCluster {
			t.Errorf("%s matches ^TestKubernetes without starting a kind cluster", name)
		}
		if startsCluster && !startsWith(function, "acquireLifecycleKindSlot") {
			t.Errorf("%s starts a kind cluster; its first statement must be acquireLifecycleKindSlot(t)", name)
		}
	}
	sort.Strings(selected)
	if len(selected) == 0 {
		t.Fatal("test/integration/lifecycle starts no kind cluster; the kind lane would select nothing")
	}
	if !slices.Equal(selected, KindLifecycleTests) {
		t.Fatalf("^TestKubernetes selects\n  %v\nbut KindLifecycleTests lists\n  %v", selected, KindLifecycleTests)
	}
}

// TestStageWavesClassifyEveryLiveProbe proves the stage wave lists and
// test/qualification/production agree under the default build tags, so baseline
// checks catch a renamed or removed TestStage* probe without -tags=qualification.
func TestStageWavesClassifyEveryLiveProbe(t *testing.T) {
	const (
		wave1 = "StageWave1Probes"
		wave2 = "StageWave2ChaosProbes"
		none  = "StageUnscheduledProbes"
	)
	classified := map[string]string{}
	for _, group := range []struct {
		list  string
		names []string
	}{
		{wave1, StageWave1Probes},
		{wave2, StageWave2ChaosProbes},
		{none, StageUnscheduledProbes},
	} {
		for _, name := range group.names {
			if other, ok := classified[name]; ok {
				t.Errorf("%s is listed in %s and %s", name, other, group.list)
			}
			classified[name] = group.list
		}
	}

	functions := testFunctions(t, "test/qualification/production")
	for name, function := range functions {
		if !strings.HasPrefix(name, "TestStage") {
			continue
		}
		switch classified[name] {
		case wave1:
			if !startsWith(function, "stageWave1Parallel") {
				t.Errorf("%s is a wave 1 probe; its first statement must be stageWave1Parallel(t)", name)
			}
		case wave2, none:
			if calls(function, "stageWave1Parallel") || calls(function, "Parallel") {
				t.Errorf("%s is listed in %s and must never run in parallel", name, classified[name])
			}
		default:
			t.Errorf("%s is in no stage wave: add it to %s or %s", name, wave1, wave2)
		}
	}
	for name, list := range classified {
		if functions[name] == nil {
			t.Errorf("%s names %s, which test/qualification/production does not declare", list, name)
		}
	}
}

// TestRestoreDrillsTakeASlotBeforeTheirTimeline proves every local restore
// drill enters the restore gate first, so queueing is never measured as RTO.
func TestRestoreDrillsTakeASlotBeforeTheirTimeline(t *testing.T) {
	drills := 0
	for name, function := range testFunctions(t, "test/qualification/restore") {
		if !calls(function, "runLocalDrill") && !calls(function, "runCoordinatedDrill") && !calls(function, "prepareCoordinatedBackup") {
			continue
		}
		drills++
		if !startsWith(function, "acquireRestoreSlot") {
			t.Errorf("%s runs a restore drill; its first statement must be acquireRestoreSlot(t)", name)
		}
	}
	if drills == 0 {
		t.Fatal("test/qualification/restore runs no local drill")
	}
}

// TestLocalAllCoversTheMakefileSuites proves the local-all plan runs every
// package of the Makefile's sequential suites-stack and suites-kind rows with
// the same tags in the same launcher pool, so the two entry points cannot
// drift apart.
func TestLocalAllCoversTheMakefileSuites(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repositoryRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	row := regexp.MustCompile(`(?m)^SUITE_[a-z-]+ = \$\(SLOT\) (stack|kind) -- \$\(SUITE_TEST\) (.*)$`)
	type selection struct{ pool, tags, target string }
	want := map[selection]bool{}
	for _, match := range row.FindAllStringSubmatch(string(makefile), -1) {
		tags := ""
		for _, word := range strings.Fields(match[2]) {
			switch {
			case strings.HasPrefix(word, "-tags="):
				tags = strings.TrimPrefix(word, "-tags=")
			case strings.HasPrefix(word, "./"):
				want[selection{match[1], tags, word}] = true
			}
		}
	}
	if len(want) < 10 {
		t.Fatalf("parsed only %d package selections from the Makefile suite rows", len(want))
	}

	plan, err := BuildPlan(Options{Target: TargetLocalAll, EvidenceDir: "/evidence", Race: true}, noEnvironment)
	if err != nil {
		t.Fatalf("BuildPlan(local-all): %v", err)
	}
	got := map[selection]bool{}
	for _, lane := range plan.Waves[0].Lanes {
		tags := ""
		for _, word := range lane.Argv {
			if strings.HasPrefix(word, "-tags=") {
				tags = strings.TrimPrefix(word, "-tags=")
			}
		}
		for _, word := range lane.Argv[3:] {
			if strings.HasPrefix(word, "./") {
				got[selection{lane.Argv[1], tags, word}] = true
			}
		}
	}
	for selected := range want {
		// The lifecycle package is split across both pools by test name.
		if selected.target == "./test/integration/lifecycle/" {
			for _, pool := range []string{"stack", "kind"} {
				if !got[selection{pool, selected.tags, selected.target}] {
					t.Errorf("local-all runs no %s lane for %s", pool, selected.target)
				}
			}
			continue
		}
		if !got[selected] {
			t.Errorf("local-all does not run %s with -tags=%s in the %s pool", selected.target, selected.tags, selected.pool)
		}
	}
}
