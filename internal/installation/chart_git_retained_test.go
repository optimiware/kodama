package installation

// Retained-commit chart wiring. The control plane must mount every Git tenant's served
// repository directory and enable the keeper root when the chart also deploys
// the Git service. With the block disabled the default render is unchanged.

import (
	"strings"
	"testing"
)

func TestChartControlPlaneMountsGitRepositoriesForKeepRefs(t *testing.T) {
	body := readText(t, chartPath(t, "templates/deployment.yaml"))
	for _, required := range []string{
		"KODAMA_GIT_STORAGE_ROOT",
		`value: "/var/lib/kodama/git-repos"`,
		"mountPath: /var/lib/kodama/git-repos/{{ $entry.tenant }}",
		"subPath: repos/{{ $entry.tenant }}",
		"claimName: kodama-git-{{ $entry.tenant }}-data",
		"name: prepare-git-repos",
		`mkdir -p "/data/{{ $entry.tenant }}/repos/{{ $entry.tenant }}"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("deployment template does not carry %q", required)
		}
	}
	// The whole block must be gated on the Git service: a default render keeps
	// the control plane unchanged.
	const gate = "{{- if and .Values.gitService .Values.gitService.enabled }}"
	if count := strings.Count(body, gate); count < 4 {
		t.Fatalf("deployment gates the Git mounts on %q %d times; want the environment, init container, mounts and volumes", gate, count)
	}
}

func TestChartGitServiceValuesDocumentRetainedCommits(t *testing.T) {
	values := readText(t, chartPath(t, "values.yaml"))
	for _, required := range []string{"KODAMA_GIT_STORAGE_ROOT", "refs/kodama/keep/<sha>"} {
		if !strings.Contains(values, required) {
			t.Fatalf("values.yaml does not document %q", required)
		}
	}
}
