package repochk

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type taskRunResult struct {
	code           int
	stdout, stderr string
	tempDir        string
}

// runTaskFixture writes one planned task whose check is the given shell
// script and runs it with a minimal environment.
func runTaskFixture(t *testing.T, check string, acceptance []string, extraEnv ...string) taskRunResult {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	names := "[" + strings.Join(acceptance, ", ") + "]"
	indented := "  " + strings.ReplaceAll(strings.TrimSpace(check), "\n", "\n  ")
	text := "---\nid: T-126\nmilestone: production\nstatus: planned\ndepends_on: []\ndecision_blockers: []\nowns: [internal/x/]\ncheck: |-\n" + indented + "\nacceptance: " + names + "\n---\n# T-126\n"
	if err := os.WriteFile(filepath.Join(directory, "T-126-example.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	var stdout, stderr bytes.Buffer
	environment := append([]string{"PATH=" + os.Getenv("PATH"), "OUT=" + filepath.Join(root, "out.txt")}, extraEnv...)
	code := TaskRun{Root: root, TaskID: "T-126", Environ: environment, TempDir: temp, Stdout: &stdout, Stderr: &stderr}.Run()
	return taskRunResult{code: code, stdout: stdout.String(), stderr: stderr.String(), tempDir: temp}
}

func (result taskRunResult) kept(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(result.tempDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

const aggregated = `printf '{"passed":["unit_a","Test_unit_b","unitb"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"`

func TestUnitCheckTaskPassesAggregatedResults(t *testing.T) {
	result := runTaskFixture(t, aggregated, []string{"unit_a", "unit_b"})
	if result.code != 0 || !strings.Contains(result.stdout, "PASS T-126") {
		t.Fatalf("code = %d stdout = %q stderr = %q", result.code, result.stdout, result.stderr)
	}
	if result.kept(t) != 0 {
		t.Fatal("a passing run left its results directory behind")
	}
}

func TestUnitCheckTaskScopeAndPrefix(t *testing.T) {
	script := `echo "$KODAMA_TASK_ID $KODAMA_RESOURCE_PREFIX $DEV_SCOPE" > "$OUT"; test -n "$FORGE_RESULTS" && test ! -e "$FORGE_RESULTS"`
	// The fixture's OUT lives in its root; recover it from the check output.
	root := t.TempDir()
	out := filepath.Join(root, "env.txt")
	result := runTaskFixture(t, strings.ReplaceAll(script, `"$OUT"`, `"`+out+`"`), nil)
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "T-126 t126 t126" {
		t.Fatalf("environment = %q", got)
	}
	result = runTaskFixture(t, strings.ReplaceAll(script, `"$OUT"`, `"`+out+`"`), nil, "DEV_SCOPE=custom")
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
	data, _ = os.ReadFile(out)
	if got := strings.TrimSpace(string(data)); got != "T-126 t126 custom" {
		t.Fatalf("explicit scope was replaced: %q", got)
	}
}

func TestUnitCheckTaskRunsFromRepositoryRoot(t *testing.T) {
	result := runTaskFixture(t, `test -f docs/tasks/T-126-example.md`, nil)
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
}

func TestUnitCheckTaskAcceptanceFailures(t *testing.T) {
	cases := map[string]struct {
		script, want string
	}{
		"missing name":   {`printf '{"passed":["unit_a"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"`, "not passed (missing from the results): unit_b"},
		"failed name":    {`printf '{"passed":["unit_a","unit_b"],"failed":["unit_b"],"skipped":[]}' > "$FORGE_RESULTS"`, "failed: unit_b"},
		"skipped name":   {`printf '{"passed":["unit_a","unit_b"],"failed":[],"skipped":["unitb"]}' > "$FORGE_RESULTS"`, "skipped: unit_b"},
		"no results":     {`true`, "wrote no FORGE_RESULTS"},
		"empty results":  {`: > "$FORGE_RESULTS"`, "empty"},
		"malformed list": {`printf '{"passed":"unit_a","failed":[],"skipped":[]}' > "$FORGE_RESULTS"`, "passed: must be a list of strings"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			result := runTaskFixture(t, test.script, []string{"unit_a", "unit_b"})
			if result.code != 1 || !strings.Contains(result.stderr, test.want) {
				t.Fatalf("code = %d stderr = %q, want %q", result.code, result.stderr, test.want)
			}
			if result.kept(t) != 1 {
				t.Fatal("a failing run must keep its results directory for diagnosis")
			}
		})
	}
}

func TestUnitCheckTaskIgnoresStaleResults(t *testing.T) {
	stale := filepath.Join(t.TempDir(), "stale.json")
	if err := os.WriteFile(stale, []byte(`{"passed":["unit_a","unit_b"],"failed":[],"skipped":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runTaskFixture(t, `true`, []string{"unit_a", "unit_b"}, "FORGE_RESULTS="+stale)
	if result.code != 1 || !strings.Contains(result.stderr, "wrote no FORGE_RESULTS") {
		t.Fatalf("stale inherited results counted: code = %d stderr = %q", result.code, result.stderr)
	}
	// A command that writes nothing new never inherits an earlier run's file.
	result = runTaskFixture(t, `test ! -e "$FORGE_RESULTS" && true`, []string{"unit_a"})
	if result.code != 1 {
		t.Fatalf("code = %d", result.code)
	}
}

func TestUnitCheckTaskGoTestJSON(t *testing.T) {
	events := `{"Package":"p","Action":"run","Test":"Test_unit_a"}
{"Package":"p","Action":"pass","Test":"Test_unit_a"}
{"Package":"p","Action":"pass","Test":"Test_unit_b"}
{"Package":"p","Action":"skip","Test":"Test_unit_c"}
{"Package":"p","Action":"fail","Test":"Test_unit_d"}
{"Package":"p","Action":"pass"}
`
	script := "cat > \"$FORGE_RESULTS\" <<'EOF'\n" + events + "EOF"
	result := runTaskFixture(t, script, []string{"unit_a", "unit_b"})
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
	result = runTaskFixture(t, script, []string{"unit_a", "unit_c"})
	if result.code != 1 || !strings.Contains(result.stderr, "skipped: unit_c") {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
	result = runTaskFixture(t, script, []string{"unit_d"})
	if result.code != 1 || !strings.Contains(result.stderr, "failed: unit_d") {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
	result = runTaskFixture(t, script, []string{"unit_a", "unit_missing"})
	if result.code != 1 || !strings.Contains(result.stderr, "unit_missing") {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
}

func TestUnitCheckTaskRejectsFailedOrIncompletePackages(t *testing.T) {
	cases := map[string]string{
		"failed package after passing acceptance": `{"Package":"p","Action":"pass","Test":"Test_unit_a"}
{"Package":"p","Action":"fail"}
`,
		"incomplete package": `{"Package":"p","Action":"run","Test":"Test_unit_a"}
{"Package":"p","Action":"pass","Test":"Test_unit_a"}
`,
		"one package fails, another passes": `{"Package":"p","Action":"pass","Test":"Test_unit_a"}
{"Package":"p","Action":"pass"}
{"Package":"q","Action":"pass","Test":"Test_unit_b"}
{"Package":"q","Action":"fail"}
`,
		"whole package skipped": `{"Package":"p","Action":"pass","Test":"Test_unit_a"}
{"Package":"p","Action":"skip"}
`,
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			// The shell masks the go test status, as a wrapper might.
			script := "cat > \"$FORGE_RESULTS\" <<'EOF'\n" + events + "EOF\nexit 0"
			result := runTaskFixture(t, script, []string{"unit_a"})
			if result.code != 1 || !strings.Contains(result.stderr, "failed or did not complete") || strings.Contains(result.stdout, "PASS") {
				t.Fatalf("code = %d stdout = %q stderr = %q", result.code, result.stdout, result.stderr)
			}
		})
	}
	// A completed package plus a package without test files passes.
	events := `{"Package":"p","Action":"pass","Test":"Test_unit_a"}
{"Package":"p","Action":"pass"}
{"Package":"q","Action":"output","Output":"?   \tq\t[no test files]\n"}
{"Package":"q","Action":"skip"}
`
	result := runTaskFixture(t, "cat > \"$FORGE_RESULTS\" <<'EOF'\n"+events+"EOF", []string{"unit_a"})
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %q", result.code, result.stderr)
	}
}

func TestUnitCheckTaskFailingCommandNeverPasses(t *testing.T) {
	script := aggregated + "\nexit 3"
	result := runTaskFixture(t, script, []string{"unit_a", "unit_b"})
	if result.code != 3 || !strings.Contains(result.stderr, "FAIL") || strings.Contains(result.stdout, "PASS") {
		t.Fatalf("code = %d stdout = %q stderr = %q", result.code, result.stdout, result.stderr)
	}
}

func TestUnitCheckTaskInfrastructureUnavailable(t *testing.T) {
	result := runTaskFixture(t, "exit 75", []string{"unit_a"})
	if result.code != ExitTempFail || !strings.Contains(result.stderr, "infrastructure unavailable") {
		t.Fatalf("code = %d stderr = %q", result.code, result.stderr)
	}
	// make turns a recipe's exit 75 into 2; the infra_pause record survives.
	pause := `printf '{"infra_pause":"missing dev scope t126","passed":[],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"; exit 2`
	result = runTaskFixture(t, pause, []string{"unit_a"})
	if result.code != ExitTempFail || !strings.Contains(result.stderr, "infrastructure unavailable: missing dev scope t126") {
		t.Fatalf("code = %d stderr = %q", result.code, result.stderr)
	}
	if result.kept(t) != 0 {
		t.Fatal("an infrastructure pause left a results directory behind")
	}
	// An ordinary code failure is not an infrastructure pause.
	result = runTaskFixture(t, `printf '{"passed":[],"failed":["unit_a"],"skipped":[]}' > "$FORGE_RESULTS"; exit 1`, []string{"unit_a"})
	if result.code != 1 {
		t.Fatalf("code = %d", result.code)
	}
}

func TestUnitCheckTaskZeroExitInfrastructureReceipt(t *testing.T) {
	receipt := `printf '{"infra_pause":"missing dev scope t126","passed":["unit_a"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"; exit 0`
	for name, acceptance := range map[string][]string{"with acceptance": {"unit_a"}, "without acceptance": nil} {
		t.Run(name, func(t *testing.T) {
			result := runTaskFixture(t, receipt, acceptance)
			if result.code != ExitTempFail || !strings.Contains(result.stderr, "infrastructure unavailable: missing dev scope t126") || strings.Contains(result.stdout, "PASS") {
				t.Fatalf("code = %d stdout = %q stderr = %q", result.code, result.stdout, result.stderr)
			}
			if result.kept(t) != 0 {
				t.Fatal("an infrastructure pause left a results directory behind")
			}
		})
	}
	// An empty reason is not a receipt.
	result := runTaskFixture(t, `printf '{"infra_pause":"  ","passed":["unit_a"],"failed":[],"skipped":[]}' > "$FORGE_RESULTS"`, []string{"unit_a"})
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %q", result.code, result.stderr)
	}
}

func TestUnitCheckTaskWithoutAcceptanceNeedsNoResults(t *testing.T) {
	result := runTaskFixture(t, `true`, nil)
	if result.code != 0 {
		t.Fatalf("code = %d stderr = %s", result.code, result.stderr)
	}
}

func TestUnitCheckTaskRejectsLegacyOverrides(t *testing.T) {
	for _, name := range []string{"LAYER", "STAGE"} {
		result := runTaskFixture(t, `touch "$OUT"`, nil, name+"=full")
		if result.code != 2 || !strings.Contains(result.stderr, name+" is no longer supported") {
			t.Fatalf("%s: code = %d stderr = %q", name, result.code, result.stderr)
		}
	}
}

func TestUnitCheckTaskContractErrors(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "docs", "tasks")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	record := "---\nid: T-130\nmilestone: production\nstatus: done\ndepends_on: []\ndecision_blockers: []\nowns: [docs/]\n---\n"
	if err := os.WriteFile(filepath.Join(directory, "T-130-nocheck.md"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"T-130": "declares no check", "T-131": "no record", "t-1": "invalid task ID"} {
		var stderr bytes.Buffer
		code := TaskRun{Root: root, TaskID: id, Environ: []string{"PATH=" + os.Getenv("PATH")}, Stderr: &stderr}.Run()
		if code != 2 || !strings.Contains(stderr.String(), want) {
			t.Fatalf("%s: code = %d stderr = %q", id, code, stderr.String())
		}
	}
}

func TestUnitCheckTaskReadsRepoctlGoResults(t *testing.T) {
	// The aggregated object GoCheck writes, including normalized aliases.
	path := filepath.Join(t.TempDir(), "results.json")
	if err := writeResults(path, map[string]any{
		"passed": []string{"Test_unit_a", "unita"}, "failed": []string{}, "skipped": []string{},
	}); err != nil {
		t.Fatal(err)
	}
	if problem := readTaskResults(path).verify([]string{"unit_a"}); problem != "" {
		t.Fatal(problem)
	}
}
