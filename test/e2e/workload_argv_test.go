//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// TestE2E_WorkloadScriptsKeepTokensOutOfArgv proves every generated workload
// script passes the execution token through a file or a stdin curl config, not
// through a command argument visible in /proc/<pid>/cmdline under rootless
// Podman.
func TestE2E_WorkloadScriptsKeepTokensOutOfArgv(t *testing.T) {
	preflight := strings.Join(preflightCommand(
		"tenant-a",
		"https://host.containers.internal:18445",
		"smoke/run-1",
		"/workspace/w49-ca.crt",
	), "\n")
	submission, err := harness.OutputSubmissionScript(map[string]string{"artifact": "ok"}, "/workspace/w49-ca.crt")
	if err != nil {
		t.Fatalf("build output submission: %v", err)
	}
	for name, script := range map[string]string{
		"installation preflight": preflight,
		"output submission":      submission,
	} {
		for _, forbidden := range []string{
			`-H "Authorization: Bearer $(`,
			`--header "Authorization: Bearer $(`,
			`Authorization: Bearer $KODAMA`,
			`Authorization: Bearer ${`,
		} {
			if strings.Contains(script, forbidden) {
				t.Fatalf("%s puts the execution token in a command argument (%s)", name, forbidden)
			}
		}
		if strings.Contains(script, "cat /kodama/secrets/KODAMA_TOKEN") &&
			!strings.Contains(script, "GIT_ASKPASS") && !strings.Contains(script, "printf") {
			t.Fatalf("%s reads the token without a safe file/askpass/stdin path", name)
		}
	}
	if !strings.Contains(preflight, "-K -") ||
		!strings.Contains(preflight, `printf 'header = "Authorization: Bearer %s"`) {
		t.Fatal("the API probe does not pass its Authorization header through a curl stdin config")
	}
	if strings.Contains(preflight, "/workspace/token") || strings.Contains(preflight, "cp ") {
		t.Fatal("the preflight copies the execution token into the workspace")
	}
}
