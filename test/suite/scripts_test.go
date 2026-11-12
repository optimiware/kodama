package suite

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScript runs one command with exactly the given environment (plus PATH and
// HOME) and returns its combined output and exit code.
func runScript(t *testing.T, directory string, environment []string, argv ...string) (string, int) {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, environment...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return output.String(), 0
	case errors.As(err, &exit):
		return output.String(), exit.ExitCode()
	default:
		t.Fatalf("run %v: %v", argv, err)
		return "", 0
	}
}

// TestSlotLauncherNestsANamedLockInsideASlot proves the stage session can run
// the RDB drill: a command inside a slot that asks for a named lock gets it
// and keeps its parent's slot, even on a host with one stack slot.
func TestSlotLauncherNestsANamedLockInsideASlot(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skipf("flock is not installed: %v", err)
	}
	launcher, err := filepath.Abs(filepath.Join(repositoryRoot, slotLauncher))
	if err != nil {
		t.Fatalf("resolve the launcher: %v", err)
	}
	host := []string{
		"KODAMA_SLOT_DIR=" + t.TempDir(),
		"KODAMA_STACK_SLOTS=1",
		"KODAMA_SLOT_WAIT=1",
		"KODAMA_LOAD_MAX=1000000",
		"TMPDIR=" + t.TempDir(),
	}
	output, code := runScript(t, "", host, launcher, "--exclusive", "stage", "stack", "--",
		launcher, "--exclusive", "rdb", "stack", "--", "sh", "-c", `echo "inner slot=$KODAMA_SLOT"`)
	if code != 0 || !strings.Contains(output, "inner slot=stack-1") {
		t.Fatalf("a named lock nested in the only slot exited %d:\n%s", code, output)
	}
}

// scratchRepository copies scripts/test-suite next to a recording launcher and
// a recording go command, so the wrapper's routing can be observed without a
// slot, a cluster or a build.
func scratchRepository(t *testing.T) (root, record string) {
	t.Helper()
	root = t.TempDir()
	record = filepath.Join(root, "recorded")
	wrapper, err := os.ReadFile(filepath.Join(repositoryRoot, "scripts", "test-suite"))
	if err != nil {
		t.Fatalf("read scripts/test-suite: %v", err)
	}
	recorder := "#!/usr/bin/env bash\n" +
		`{ printf '%s\n' "$(basename "$0")" "$@"; printf 'descriptor=%s generated=%s\n' "${Q01_INSTALLATION_FILE:-}" "${Q01_STAGE_DESCRIPTOR:-}"; } >"$SUITE_TEST_RECORD"` + "\n"
	for path, content := range map[string]string{
		"scripts/test-suite":    string(wrapper),
		"scripts/infra-slot.sh": recorder,
		"bin/go":                recorder,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root, record
}

// TestSuiteWrapperOpensOneStageSession proves scripts/test-suite routes the
// stage target through the stage lock and the stage runner, generates the
// descriptor only when none was supplied, and runs every other target and
// every dry run directly.
func TestSuiteWrapperOpensOneStageSession(t *testing.T) {
	root, record := scratchRepository(t)
	run := func(environment []string, arguments ...string) (recorded []string, output string, code int) {
		t.Helper()
		_ = os.Remove(record)
		environment = append([]string{
			"PATH=" + filepath.Join(root, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
			"SUITE_TEST_RECORD=" + record,
		}, environment...)
		output, code = runScript(t, t.TempDir(), environment, append([]string{"bash", filepath.Join(root, "scripts", "test-suite")}, arguments...)...)
		if data, err := os.ReadFile(record); err == nil {
			recorded = strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		return recorded, output, code
	}
	session := []string{
		"infra-slot.sh", "--exclusive", "scw-stage", "stack", "--",
		"test/e2e/internal/harness/run-stage.sh",
		"go", "run", "./test/suite/cmd/testsuite", "--token-refresh=5m",
	}
	joined := func(words ...string) string { return strings.Join(words, " ") }

	recorded, output, code := run(nil, "--target=stage", "--skip-chaos")
	if want := append(append([]string(nil), session...), "--target=stage", "--skip-chaos", "descriptor= generated=1"); code != 0 || joined(recorded...) != joined(want...) {
		t.Fatalf("stage session = %v (exit %d, output %q); want %v", recorded, code, output, want)
	}

	recorded, _, code = run([]string{"KODAMA_STAGE_LOCK=other-stage"}, "-target", "stage", "--installation", "/stage/installation.json")
	if code != 0 || recorded[2] != "other-stage" || recorded[len(recorded)-1] != "descriptor=/stage/installation.json generated=" {
		t.Fatalf("stage session with a descriptor = %v (exit %d); want the named lock and no generated descriptor", recorded, code)
	}

	recorded, _, code = run([]string{"Q01_INSTALLATION_FILE=/exported/installation.json"}, "--target=stage")
	if code != 0 || recorded[len(recorded)-1] != "descriptor=/exported/installation.json generated=" {
		t.Fatalf("stage session with an exported descriptor = %v (exit %d)", recorded, code)
	}

	recorded, output, code = run([]string{"KODAMA_STAGE_LOCK=../lock"}, "--target=stage")
	if code == 0 || recorded != nil {
		t.Fatalf("an unsafe lock name exited %d and ran %v; want a refusal before any command:\n%s", code, recorded, output)
	}

	direct := []string{"go", "run", "./test/suite/cmd/testsuite"}
	for _, arguments := range [][]string{
		{"--target=local-all", "--keep-going"},
		{"--target=stage", "--dry-run"},
		{},
	} {
		recorded, output, code = run(nil, arguments...)
		if want := append(append(append([]string(nil), direct...), arguments...), "descriptor= generated="); code != 0 || joined(recorded...) != joined(want...) {
			t.Fatalf("test-suite %v = %v (exit %d, output %q); want %v", arguments, recorded, code, output, want)
		}
	}
}
