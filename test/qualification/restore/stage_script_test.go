package restorequal

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageDrillScriptTakesTheRDBLock proves scripts/q03-stage-drill serializes
// on the dedicated RDB lock, not on the cluster lock, so a drill never queues
// behind a cluster suite. The script runs against a recording launcher and
// stub go/scw binaries; it never reaches a host slot or Scaleway.
func TestStageDrillScriptTakesTheRDBLock(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "q03-stage-drill"))
	if err != nil {
		t.Fatalf("read scripts/q03-stage-drill: %v", err)
	}
	scratch := t.TempDir()
	scripts := filepath.Join(scratch, "scripts")
	bin := filepath.Join(scratch, "bin")
	recorded := filepath.Join(scratch, "launcher-arguments")
	writeExecutable(t, filepath.Join(scripts, "q03-stage-drill"), string(script))
	writeExecutable(t, filepath.Join(scripts, "infra-slot.sh"),
		"#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >\"$Q03_TEST_RECORD\"\n")
	for _, tool := range []string{"go", "scw"} {
		writeExecutable(t, filepath.Join(bin, tool), "#!/usr/bin/env bash\nexit 0\n")
	}

	run := func(extra ...string) (arguments []string, output string, code int) {
		t.Helper()
		_ = os.Remove(recorded)
		command := exec.Command("bash", filepath.Join(scripts, "q03-stage-drill"), "plan")
		command.Env = append([]string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"Q03_TEST_RECORD=" + recorded,
			"KODAMA_Q03_STAGE_INSTANCE_ID=11111111-2222-3333-4444-555555555555",
			"KODAMA_Q03_STAGE_SOURCE_DATABASE=kodama",
			"KODAMA_Q03_STAGE_MANAGED_DATABASE=kodama",
		}, extra...)
		combined, err := command.CombinedOutput()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit):
			code = exit.ExitCode()
		default:
			t.Fatalf("run q03-stage-drill: %v", err)
		}
		if data, err := os.ReadFile(recorded); err == nil {
			arguments = strings.Split(strings.TrimSpace(string(data)), "\n")
		}
		return arguments, string(combined), code
	}

	arguments, output, code := run()
	want := []string{"--exclusive", "scw-stage-rdb", "stack", "--", "go", "run", "./test/qualification/restore/stagedrill"}
	if code != 0 || strings.Join(arguments, " ") != strings.Join(want, " ") {
		t.Fatalf("default launcher arguments = %v (exit %d, output %q); want %v", arguments, code, output, want)
	}

	arguments, output, code = run("KODAMA_Q03_STAGE_LOCK=scw-stage")
	if code != 0 || len(arguments) < 2 || arguments[1] != "scw-stage" {
		t.Fatalf("overridden launcher arguments = %v (exit %d, output %q); want the lock scw-stage", arguments, code, output)
	}

	arguments, output, code = run("KODAMA_Q03_STAGE_LOCK=../bad lock")
	if code == 0 || arguments != nil || !strings.Contains(output, "KODAMA_Q03_STAGE_LOCK") {
		t.Fatalf("unsafe lock name = exit %d, launcher arguments %v, output %q; want a refusal naming KODAMA_Q03_STAGE_LOCK before the launcher runs", code, arguments, output)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
