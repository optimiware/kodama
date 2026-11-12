//go:build component

package kubernetesconformance

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestAdapterActionEndToEnd proves one complete adapter action on a real
// cluster: materialized inputs/outputs/task/secret mounts, the reserved
// environment, redacted logs, the pinned Job security policy, retained cleanup
// and release.
func TestAdapterActionEndToEnd(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-9")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	identity := testIdentity("exec-e2e", "action-e2e", "workload-e2e", "idem-e2e")
	commands := []string{
		`cat /kodama/inputs/input-a > /workspace/input-copy.txt`,
		`cat /kodama/task.json > /workspace/task-copy.json`,
		`cat /kodama/outputs/earlier/out-a > /workspace/output-copy.txt`,
		`cat /kodama/secrets/API_KEY`,
		`cat /kodama/secrets/KODAMA_TOKEN`,
		`printf '\n'`,
		`printf 'stdout-line\n'`,
		`printf 'stderr-line\n' >&2`,
		`env | sort > /workspace/env.txt`,
	}
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "task-e2e",
		testContainer(commands, []definition.EnvVar{{Name: "BUILD_MODE", Value: "release"}}),
		[]engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t-value"}},
		"tok-e2e",
	))
	observation := waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	if code, ok := observation.ExitStatus(); !ok || code != 0 {
		t.Fatalf("action exit = %d/%v; want 0/true (%+v)", code, ok, observation)
	}

	job := oneJob(t, ctx, fx.Client, identity)
	assertJobSecurityPolicy(t, ctx, fx, job.Name)

	stdout, stderr := readAllLogs(t, ctx, fx.Adapter, identity, 1<<20)
	if !strings.Contains(string(stdout), "[REDACTED][REDACTED]") {
		t.Fatalf("stdout = %q; want the configured values redacted", stdout)
	}
	if strings.Contains(string(stdout), "s3cr3t-value") || strings.Contains(string(stdout), "tok-e2e") {
		t.Fatalf("stdout leaked credential material: %q", stdout)
	}
	if !containsLine(string(stdout), "stdout-line") {
		t.Fatalf("stdout = %q; want the workload line", stdout)
	}
	// Stock clusters cannot split the streams: stderr output arrives in the one
	// combined log, attributed to stdout.
	if !containsLine(string(stdout), "stderr-line") || len(stderr) != 0 {
		t.Fatalf("combined log = %q, stderr = %q; want the stderr line in the combined log only", stdout, stderr)
	}

	readerOutput := runReader(t, ctx, fx, workspaceID, "e2e-read", []string{
		`printf 'INPUT:'`,
		`cat /workspace/input-copy.txt`,
		`printf 'TASK:'`,
		`cat /workspace/task-copy.json`,
		`printf '\n'`,
		`printf 'OUTPUT:'`,
		`cat /workspace/output-copy.txt`,
		`printf '\n'`,
		`grep -E '^KODAMA_(URL|EXECUTION|ACTION|TASK)=' /workspace/env.txt | sort | sed 's/^/ENV:/'`,
	})
	for _, want := range []string{
		"INPUT:line1\nline2\n",
		"TASK:{\"id\":\"task-1\"}",
		"OUTPUT:earlier-output",
		"ENV:KODAMA_ACTION=build",
		"ENV:KODAMA_EXECUTION=exec-e2e",
		"ENV:KODAMA_TASK=task-e2e",
		"ENV:KODAMA_URL=" + testKodamaURL,
	} {
		if !strings.Contains(readerOutput, want) {
			t.Fatalf("reader output is missing %q:\n%s", want, readerOutput)
		}
	}
	if strings.Contains(readerOutput, "s3cr3t-value") || strings.Contains(readerOutput, "tok-e2e") {
		t.Fatalf("secret material reached the workspace environment:\n%s", readerOutput)
	}

	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed || !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("removal = %+v; want removed with the retained claim named", removal)
	}
	reader := testIdentity("exec-e2e-read", "action-e2e-read", "workload-e2e-read", "idem-e2e-read")
	if _, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: reader, WorkspaceID: workspaceID}); err != nil {
		t.Fatalf("Remove reader: %v", err)
	}
	waitForWorkspaceUnreferenced(t, ctx, fx.Client, workspaceID)

	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("released claim is still present: %v", err)
	}
}

// assertJobSecurityPolicy reads the live Job through kubectl and proves the
// no-replay, no-token, no-host-network and bounded-resource policy survived the
// API server.
func assertJobSecurityPolicy(t *testing.T, ctx context.Context, fx *fixture, jobName string) {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "job", jobName, "-n", testNamespace)
	if err != nil {
		t.Fatalf("read live Job: %v", err)
	}
	spec, _ := document["spec"].(map[string]any)
	if value, _ := spec["backoffLimit"].(float64); value != 0 {
		t.Fatalf("backoffLimit = %v; want 0 (never retried)", spec["backoffLimit"])
	}
	if value, _ := spec["podReplacementPolicy"].(string); value != "Failed" {
		t.Fatalf("podReplacementPolicy = %q; want Failed", value)
	}
	if value, _ := spec["ttlSecondsAfterFinished"].(float64); value != 120 {
		t.Fatalf("ttlSecondsAfterFinished = %v; want 120", spec["ttlSecondsAfterFinished"])
	}
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	if value, _ := podSpec["restartPolicy"].(string); value != "Never" {
		t.Fatalf("restartPolicy = %q; want Never", value)
	}
	if automount, ok := podSpec["automountServiceAccountToken"]; !ok || automount != false {
		t.Fatalf("automountServiceAccountToken = %v (present %v); want an explicit false", automount, ok)
	}
	if hostNetwork, ok := podSpec["hostNetwork"]; ok && hostNetwork != false {
		t.Fatalf("hostNetwork = %v; want false", hostNetwork)
	}
	if serviceAccount, ok := podSpec["serviceAccountName"]; ok && serviceAccount != "" {
		t.Fatalf("serviceAccountName = %v; want the default account", serviceAccount)
	}

	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("containers = %d; want exactly 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	if name, _ := container["name"].(string); name != "workload" {
		t.Fatalf("container name = %q; want workload", name)
	}
	resources, _ := container["resources"].(map[string]any)
	limits, _ := resources["limits"].(map[string]any)
	if cpu, _ := limits["cpu"].(string); cpu != "200m" {
		t.Fatalf("cpu limit = %q; want 200m", cpu)
	}
	if memory, _ := limits["memory"].(string); memory != "128Mi" {
		t.Fatalf("memory limit = %q; want 128Mi", memory)
	}

	volumes, _ := podSpec["volumes"].([]any)
	var hasClaim, hasConfigMap, hasSecret bool
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		name, _ := volume["name"].(string)
		if strings.HasPrefix(name, "kube-api-access-") {
			t.Fatalf("Job has a service-account token volume %q", name)
		}
		switch name {
		case "workspace":
			claim, _ := volume["persistentVolumeClaim"].(map[string]any)
			if claimName, _ := claim["claimName"].(string); claimName == "" {
				t.Fatalf("workspace volume has no claim name: %v", volume)
			}
			hasClaim = true
		case "activation":
			if configMap, ok := volume["configMap"].(map[string]any); ok {
				if name, _ := configMap["name"].(string); name != "" {
					hasConfigMap = true
				}
			}
		case "credential":
			if secret, ok := volume["secret"].(map[string]any); ok {
				if name, _ := secret["secretName"].(string); name != "" {
					hasSecret = true
				}
			}
		}
	}
	if !hasClaim || !hasConfigMap || !hasSecret {
		t.Fatalf("Job volumes = %v; want the workspace claim, activation snapshot and credential secret", volumes)
	}
}
