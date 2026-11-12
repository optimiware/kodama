package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDryRunPrintsThePlanAndRunsNothing proves --dry-run renders each target's
// waves, lanes and settings, and that a plan that cannot be built is a usage
// error.
func TestDryRunPrintsThePlanAndRunsNothing(t *testing.T) {
	for _, name := range []string{"Q01_INSTALLATION_FILE", "KODAMA_LOCAL_HTTPS_PORT"} {
		t.Setenv(name, "")
	}
	for _, target := range []struct {
		arguments []string
		want      []string
	}{
		{[]string{"--target=local-all", "--dry-run"}, []string{
			"target local-all: 1 wave(s)", "wave-1-local: 9 concurrent lane(s)",
			"scripts/infra-slot.sh kind -- test/e2e/internal/harness/verify-go-test.sh --expect TestKubernetesCancellationStopsAdoptedWorkload",
			"-race -count=1 -timeout 45m -tags=integration -json -run ^TestKubernetes ./test/integration/lifecycle/",
			"KODAMA_Q03_CONCURRENCY=4",
		}},
		{[]string{"--target=kind", "--dry-run", "--race=false", "--evidence-dir=/evidence"}, []string{
			"wave-1-kind: 2 concurrent lane(s)", "go test -count=1 -timeout 40m -tags=component",
		}},
		{[]string{"--target=podman", "--installation=/state/installation.json", "--evidence-dir=/evidence", "--concurrency=2", "--dry-run"}, []string{
			"Q01_INSTALLATION_FILE=/state/installation.json", "Q01_EVIDENCE_DIR=/evidence/portable-e2e-podman",
			"KODAMA_E2E_CONCURRENCY=2", "-parallel 2",
		}},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(target.arguments, &stdout, &stderr); code != 0 {
			t.Fatalf("testsuite %v exited %d: %s", target.arguments, code, stderr.String())
		}
		for _, want := range target.want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("testsuite %v output lacks %q:\n%s", target.arguments, want, stdout.String())
			}
		}
	}

	for _, refused := range []struct {
		arguments []string
		message   string
	}{
		{[]string{"--target=cloud", "--dry-run"}, "unknown target"},
		{[]string{"--target=stage", "--dry-run"}, "installation descriptor"},
		{[]string{"--target=podman", "--dry-run"}, "KODAMA_LOCAL_HTTPS_PORT"},
		{[]string{"--target=local-all", "--evidence-dir=relative", "--dry-run"}, "absolute"},
		{[]string{"--target=local-all", "extra"}, "unexpected arguments"},
		{[]string{"--no-such-flag"}, "no-such-flag"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(refused.arguments, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), refused.message) {
			t.Errorf("testsuite %v exited %d with %q; want exit 2 naming %q", refused.arguments, code, stderr.String(), refused.message)
		}
	}
}
