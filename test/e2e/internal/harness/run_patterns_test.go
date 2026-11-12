//go:build e2e

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// documentedLiveTests are the seven existing live e2e tests a release run must
// select. The test below confirms each one exists in the e2e package source.
var documentedLiveTests = []string{
	"TestE2E_AC01InstalledPipeline",
	"TestE2E_AC02ImmutableAdmission",
	"TestE2E_AC09CleanupAttributable",
	"TestE2E_AC11Materialization",
	"TestE2E_AC12Outputs",
	"TestE2E_AC13OrderedActions",
	"TestE2E_AC16PlannedWait",
}

// documentedSmokeSubtests are the five release-smoke scenarios a default run
// must really execute. A subtest that is deleted or skipped is a missing
// proof, not a pass, so each name is an explicit expected result.
var documentedSmokeSubtests = []string{
	"TestReleaseSmoke/RequestWait",
	"TestReleaseSmoke/OperatorRetry",
	"TestReleaseSmoke/ConversationHandoff",
	"TestReleaseSmoke/ProtectedTask",
	"TestReleaseSmoke/HumanPush",
}

// TestHarnessDocumentedRunPatternsMatchTests proves every documented live
// -run pattern really selects the intended tests. A pattern that matches nothing
// makes `go test` print PASS without executing a test, so it is never acceptable
// release proof.
func TestHarnessDocumentedRunPatternsMatchTests(t *testing.T) {
	root := repoRootForTest(t)
	runbook := readTextFile(t, filepath.Join(root, "docs", "runbooks", "release-smoke.md"))
	source := e2eTestNames(t, filepath.Join(root, "test", "e2e"))
	for _, name := range documentedLiveTests {
		if !source[name] {
			t.Fatalf("documented live test %s does not exist in test/e2e", name)
		}
	}
	// Each documented smoke subtest must still be declared exactly once by the
	// TestReleaseSmoke runner.
	runnerSource := readTextFile(t, filepath.Join(root, "test", "e2e", "release_smoke_test.go"))
	for _, name := range documentedSmokeSubtests {
		subtest := strings.TrimPrefix(name, "TestReleaseSmoke/")
		if !strings.Contains(runnerSource, `t.Run("`+subtest+`"`) {
			t.Fatalf("documented smoke subtest %s is not declared by TestReleaseSmoke", name)
		}
	}

	patternExpression := regexp.MustCompile(`-run '([^']+)'`)
	patterns := patternExpression.FindAllStringSubmatch(runbook, -1)
	if len(patterns) == 0 {
		t.Fatal("the release-smoke runbook documents no -run pattern")
	}
	sawLivePattern := false
	sawPreflightPattern := false
	for _, match := range patterns {
		pattern := match[1]
		if pattern == "^$" {
			continue
		}
		if !strings.Contains(pattern, "TestE2E") {
			continue
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("documented -run pattern %q does not compile: %v", pattern, err)
		}
		var matched []string
		for name := range source {
			if compiled.MatchString(name) {
				matched = append(matched, name)
			}
		}
		if len(matched) == 0 {
			t.Fatalf("documented -run pattern %q matches no test in test/e2e", pattern)
		}
		if strings.HasPrefix(pattern, "^TestE2E_AC") {
			sawLivePattern = true
			for _, name := range documentedLiveTests {
				if !compiled.MatchString(name) {
					t.Fatalf("documented -run pattern %q does not select %s", pattern, name)
				}
			}
			for _, name := range matched {
				if !strings.HasPrefix(name, "TestE2E_AC") || strings.Contains(name, "AC03") ||
					strings.Contains(name, "AC04") || strings.Contains(name, "AC05") ||
					strings.Contains(name, "AC06") || strings.Contains(name, "AC07") ||
					strings.Contains(name, "AC08") || strings.Contains(name, "AC10") ||
					strings.Contains(name, "AC14") || strings.Contains(name, "AC15") ||
					strings.Contains(name, "AC17") || strings.Contains(name, "AC18") ||
					strings.Contains(name, "AC19") || strings.Contains(name, "AC20") {
					t.Fatalf("documented live -run pattern %q also selects %s", pattern, name)
				}
			}
		}
		if pattern == "^TestE2E_InstallationAccess$" {
			sawPreflightPattern = true
			if len(matched) != 1 || matched[0] != "TestE2E_InstallationAccess" {
				t.Fatalf("preflight pattern matched %v", matched)
			}
		}
	}
	if !sawLivePattern {
		t.Fatal("the runbook documents no live acceptance -run pattern")
	}
	if !sawPreflightPattern {
		t.Fatal("the runbook documents no installation preflight -run pattern")
	}
	for _, name := range documentedLiveTests {
		if !strings.Contains(runbook, "--expect "+name) {
			t.Fatalf("the documented release invocation does not expect %s", name)
		}
	}
	for _, name := range documentedSmokeSubtests {
		if !strings.Contains(runbook, "--expect "+name) {
			t.Fatalf("the documented release invocation does not expect %s", name)
		}
	}
	for _, marker := range []string{"verify-go-test.sh", "-json"} {
		if !strings.Contains(runbook, marker) {
			t.Fatalf("the documented release invocation does not use %q", marker)
		}
	}
	// The disposable wrapper is the default smoke gate: it must run the
	// verifying wrapper with every expected top-level test and every expected
	// subtest, so a removed or skipped scenario fails the run.
	smokeScript := readTextFile(t, filepath.Join(root, "deploy", "podman", "smoke.sh"))
	for _, marker := range []string{
		"verify-go-test.sh",
		"--expect",
		"NARROWED SELECTION - NOT RELEASE EVIDENCE",
		"selection=narrowed",
		"selection=full",
		"selection=%s",
		"full selection verified:",
	} {
		if !strings.Contains(smokeScript, marker) {
			t.Fatalf("deploy/podman/smoke.sh does not pin %q", marker)
		}
	}
	for _, name := range append(append([]string{}, documentedLiveTests...), documentedSmokeSubtests...) {
		if !strings.Contains(smokeScript, name) {
			t.Fatalf("deploy/podman/smoke.sh does not expect %s", name)
		}
	}
}

// TestHarnessVerifyGoTestRejectsAnEmptySelection proves the completion check
// fails on zero executed tests, a missing expected test, a skipped selected
// test and a failed selected test.
func TestHarnessVerifyGoTestRejectsAnEmptySelection(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable; the workload images provide it")
	}
	verifier := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "verify-go-test.py")
	cases := []struct {
		name     string
		events   []string
		expected []string
		wants    string
		wantCode int
	}{
		{
			name:     "pass",
			events:   []string{jsonEvent("run", "TestE2E_AC12Outputs"), jsonEvent("pass", "TestE2E_AC12Outputs")},
			expected: []string{"TestE2E_AC12Outputs"},
			wantCode: 0,
		},
		{
			name: "subtest pass",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/RequestWait"),
				jsonEvent("pass", "TestReleaseSmoke/RequestWait"),
				jsonEvent("run", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("pass", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/RequestWait", "TestReleaseSmoke/ProtectedTask"},
			wantCode: 0,
		},
		{
			// A parent-only expectation must not hide a skipped subtest: the
			// scenario did not run, so the release proof is missing.
			name: "skipped child under expected parent",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/HumanPush"),
				jsonEvent("skip", "TestReleaseSmoke/HumanPush"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke"},
			wants:    "skipped",
			wantCode: 1,
		},
		{
			// A parent-only expectation must not hide a failed subtest either.
			name: "failed child under expected parent",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("fail", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("fail", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke"},
			wants:    "failed",
			wantCode: 1,
		},
		{
			name: "expected subtest ran",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/HumanPush"),
				jsonEvent("pass", "TestReleaseSmoke/HumanPush"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/HumanPush"},
			wantCode: 0,
		},
		{
			name: "expected subtest did not run",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/HumanPush"},
			wants:    "did not run",
			wantCode: 1,
		},
		{
			name: "subtest and parent both expected",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/RequestWait"),
				jsonEvent("pass", "TestReleaseSmoke/RequestWait"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke", "TestReleaseSmoke/RequestWait"},
			wantCode: 0,
		},
		{
			name: "missing expected subtest",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/RequestWait"),
				jsonEvent("pass", "TestReleaseSmoke/RequestWait"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/RequestWait", "TestReleaseSmoke/ProtectedTask"},
			wants:    "did not run",
			wantCode: 1,
		},
		{
			name: "skipped expected subtest",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/HumanPush"),
				jsonEvent("skip", "TestReleaseSmoke/HumanPush"),
				jsonEvent("pass", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/HumanPush"},
			wants:    "skipped",
			wantCode: 1,
		},
		{
			name: "failed expected subtest",
			events: []string{
				jsonEvent("run", "TestReleaseSmoke"),
				jsonEvent("run", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("fail", "TestReleaseSmoke/ProtectedTask"),
				jsonEvent("fail", "TestReleaseSmoke"),
			},
			expected: []string{"TestReleaseSmoke/ProtectedTask"},
			wants:    "failed",
			wantCode: 1,
		},
		{
			name:     "no tests executed",
			events:   []string{`{"Action":"pass","Package":"github.com/optimiware/kodama/test/e2e","Elapsed":1.0}`},
			expected: []string{"TestE2E_AC12Outputs"},
			wants:    "zero tests",
			wantCode: 1,
		},
		{
			// The real go test -json stream for a -run that matches nothing:
			// a warning output line and a passing package, no test events.
			name: "go test reports no tests to run",
			events: []string{
				`{"Action":"start","Package":"github.com/optimiware/kodama/test/e2e"}`,
				`{"Action":"output","Package":"github.com/optimiware/kodama/test/e2e","Output":"testing: warning: no tests to run\n"}`,
				`{"Action":"output","Package":"github.com/optimiware/kodama/test/e2e","Output":"PASS\n","OutputType":"frame"}`,
				`{"Action":"output","Package":"github.com/optimiware/kodama/test/e2e","Output":"ok  \tgithub.com/optimiware/kodama/test/e2e\t0.003s [no tests to run]\n"}`,
				`{"Action":"pass","Package":"github.com/optimiware/kodama/test/e2e","Elapsed":0.004}`,
			},
			expected: []string{"TestE2E_AC12Outputs"},
			wants:    "zero tests",
			wantCode: 1,
		},
		{
			name:     "missing expected test",
			events:   []string{jsonEvent("run", "TestOther"), jsonEvent("pass", "TestOther")},
			expected: []string{"TestE2E_AC12Outputs"},
			wants:    "did not run",
			wantCode: 1,
		},
		{
			name:     "skipped selected test",
			events:   []string{jsonEvent("run", "TestE2E_AC12Outputs"), jsonEvent("skip", "TestE2E_AC12Outputs")},
			expected: []string{"TestE2E_AC12Outputs"},
			wants:    "skipped",
			wantCode: 1,
		},
		{
			name:     "failed selected test",
			events:   []string{jsonEvent("run", "TestE2E_AC12Outputs"), jsonEvent("fail", "TestE2E_AC12Outputs")},
			expected: []string{"TestE2E_AC12Outputs"},
			wants:    "failed",
			wantCode: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "go-test.json")
			if err := os.WriteFile(path, []byte(strings.Join(testCase.events, "\n")+"\n"), 0o600); err != nil {
				t.Fatalf("write go test result: %v", err)
			}
			arguments := append([]string{verifier, path}, testCase.expected...)
			command := exec.Command(python, arguments...)
			output, err := command.CombinedOutput()
			code := 0
			if err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("run verifier: %v", err)
				}
				code = exit.ExitCode()
			}
			if code != testCase.wantCode {
				t.Fatalf("verifier exit = %d, want %d; output=%s", code, testCase.wantCode, output)
			}
			if testCase.wants != "" && !strings.Contains(strings.ToLower(string(output)), testCase.wants) {
				t.Fatalf("verifier output %q does not name %q", output, testCase.wants)
			}
		})
	}
}

func jsonEvent(action, test string) string {
	return `{"Action":"` + action + `","Package":"github.com/optimiware/kodama/test/e2e","Test":"` + test + `"}`
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// e2eTestNames lists the top-level Test functions declared in test/e2e.
func e2eTestNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read e2e package: %v", err)
	}
	declaration := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	names := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range declaration.FindAllStringSubmatch(string(data), -1) {
			names[match[1]] = true
		}
	}
	return names
}
