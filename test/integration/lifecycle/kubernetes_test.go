//go:build integration

package lifecycle

// The first real Kubernetes pipeline.
//
// This is the end-to-end proof that the composed factory server runs a
// workload as a Kubernetes Job on a PersistentVolumeClaim workspace in a real
// rootless kind cluster, through the public API, with real PostgreSQL and NATS
// behind it:
//
//   - pipeline.apply and pipeline.start are API-posted (no inline YAML);
//   - the first container action authenticates its mounted execution credential
//     against KODAMA_URL from inside the pod, writes the shared PVC workspace
//     and submits a declared output;
//   - the second container action reads the same PVC workspace and the earlier
//     action's materialized output, then submits the final output;
//   - the execution succeeds, both outputs are promoted, and the retained logs
//     are readable through execution.logs.read after both Jobs are gone; the
//     retained log of the first action carries the redaction marker where the
//     action printed its mounted credential, and no captured credential value
//     appears in it;
//   - kubectl-verified facts: two Jobs exist in the tenant namespace with the
//     digest-pinned image and the same workspace claim; the claim has the
//     pinned storage spec and ownership labels; both Jobs mount it; and the
//     per-action activation ConfigMaps and credential Secrets exist while the
//     workload is live (the positive control for cleanup);
//   - terminal cleanup is "complete": Jobs, Pods, Secrets and ConfigMaps
//     labelled with the execution are gone and the PVC is released (Release is
//     eventually consistent with the pvc-protection finalizer);
//   - the workload credential authenticates while the execution is live (the
//     positive control for revocation) and is revoked at the terminal (the old
//     token is refused); the secret value never appears in logs or events, and
//     the event scan proves it read this execution's committed events; no
//     run-labelled Podman workload leftovers remain.
//
// The test deliberately does not credit a StorageClass/size the cluster cannot
// satisfy: that negative control is out of scope for this test.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	kubeSeed     = "kube-seed"
	kubeArtifact = "built:" + kubeSeed
	kubeFinal    = "consumed:" + kubeArtifact

	// kubeActionTimeout keeps the action deadline clear of the Kubernetes Job
	// runtime while a cluster is being scheduled.
	kubeActionTimeout = "1h"
	// kubeWorkloadTimeout bounds the kubectl barriers for Job/Pod creation and
	// terminal Job status. The first Job pull/start is a few seconds; the bound
	// is a deadlock guard, not a target.
	kubeWorkloadTimeout = 5 * time.Minute
	// kubeCleanupTimeout bounds the terminal cleanup barrier (execution terminal
	// plus the composition's eventually consistent Release).
	kubeCleanupTimeout = 3 * time.Minute
	// kubeDeleteTimeout bounds the observable removal of objects the adapter
	// deletes synchronously during Remove. It is shorter than the Job's 120s
	// finished-Job TTL so a skipped adapter delete cannot be masked by the TTL
	// controller.
	kubeDeleteTimeout = 30 * time.Second
	// kubeReleaseTimeout bounds the observable PVC removal after the adapter's
	// Release reported success (pvc-protection finalizer).
	kubeReleaseTimeout = 2 * time.Minute
	// kubePollInterval is the short poll between kubectl/API observations.
	kubePollInterval = 100 * time.Millisecond
	// kubeCleanupRetryClockStep is the factory-clock advance applied while a
	// fired cleanup attempt is waiting out its durable retry backoff. The
	// factory clock is frozen, so the persisted next_retry_at (commit.At + the
	// first backoff, 1s) only becomes due once the harness clock moves past it.
	// One step clears the first backoff; later attempts back off further and
	// keep advancing under the existing kubeCleanupTimeout bound.
	kubeCleanupRetryClockStep = 2 * time.Second
)

// TestKubernetesPipeline is the first real Kubernetes pipeline proof.
func TestKubernetesPipeline(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t)
	ctx := t.Context()

	if harness.Cluster == nil || harness.KubernetesClient == nil || harness.KubernetesAdapter == nil {
		t.Fatalf("Kubernetes harness did not expose its cluster, client and adapter")
	}
	namespace := harness.KubernetesNamespace
	if namespace != factory.KubernetesNamespace {
		t.Fatalf("Kubernetes namespace = %q; want %q", namespace, factory.KubernetesNamespace)
	}
	// The image is resolved from the real Podman image store and is the
	// canonical docker.io digest-pinned reference the kind node pulls.
	if !strings.HasPrefix(harness.Image.Reference, "docker.io/library/alpine@sha256:") {
		t.Fatalf("workload image reference %q is not the digest-pinned docker.io alpine reference", harness.Image.Reference)
	}
	t.Logf("kubernetes pipeline workload image %s", harness.Image.Reference)

	document := marshalPipeline(t, pipelineDoc{
		Inputs: []string{"seed"},
		Actions: []pipelineActionDoc{
			{
				Name:    "prepare",
				Image:   harness.Image.Reference,
				Outputs: []string{"artifact"},
				Timeout: kubeActionTimeout,
				Commands: []string{
					// The mounted execution credential is non-empty and the
					// submission below authenticates it against KODAMA_URL
					// through BusyBox nc from inside the pod.
					`test -s /kodama/secrets/KODAMA_TOKEN`,
					// The workload prints its mounted credential so the retained
					// log proves the Logs path redacts it. The test asserts the
					// marker and the absence of the captured value.
					`printf 'credential:%s\n' "$(cat /kodama/secrets/KODAMA_TOKEN)"`,
					`printf 'shared workspace\n' > /workspace/marker`,
					`ARTIFACT="built:$(cat /kodama/inputs/seed)"`,
					`echo "prepare: credential accepted and marker written"`,
					submitScript("artifact", "ARTIFACT"),
				},
			},
			{
				Name:    "consume",
				Image:   harness.Image.Reference,
				Outputs: []string{"final"},
				Timeout: kubeActionTimeout,
				Commands: []string{
					// Same PVC workspace and the ordered materialization of the
					// earlier succeeded output.
					`test "$(cat /workspace/marker)" = "shared workspace"`,
					fmt.Sprintf(`test "$(cat /kodama/outputs/prepare/artifact)" = %q`, kubeArtifact),
					`FINAL="consumed:$(cat /kodama/outputs/prepare/artifact)"`,
					`echo "consume: workspace and earlier output verified"`,
					// The action holds for a few seconds while it is observably
					// running, so the kubectl facts (image, claim mount,
					// scheduled pods) and the workload Secrets can be observed
					// before the composition removes the finished Jobs. The
					// test still polls for each fact; this is not a test-side
					// settle sleep.
					`sleep 5`,
					submitScript("final", "FINAL"),
				},
			},
		},
	})

	applied := applyPipeline(t, harness, "kubernetes-pipeline", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": kubeSeed}, "start-kubernetes-pipeline")
	if started.ExecutionID == "" {
		t.Fatalf("pipeline.start returned an empty execution id: %+v", started)
	}

	// The durable workspace identity is the only public-adjacent source of the
	// deterministic claim name; the API exposes it only in a cleanup record, so
	// the harness reads the authoritative execution row.
	workspaceID := waitWorkspaceID(t, harness, started.ExecutionID)
	claimName, err := runtimekube.WorkspaceClaimName(id.WorkspaceID(workspaceID))
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}

	// The claim is provisioned before the adapter's Retain, so it exists before
	// the first Job starts.
	claim := waitClaim(t, harness, claimName)
	assertClaimSpec(t, harness, claim, workspaceID, claimName)

	// Both Jobs are created before the second action finishes; capture them,
	// their pods, the workload credentials and the activation snapshots while
	// they still exist. The Secrets and ConfigMaps are the positive control for
	// the cleanup assertion below: they were observed and must be gone later.
	jobs, pods := waitJobsAndPods(t, harness, started.ExecutionID, 2)
	actions := jobActions(t, jobs)
	secrets := captureLabelledObjects(t, harness, "secrets", started.ExecutionID, actions)
	configMaps := captureLabelledObjects(t, harness, "configmaps", started.ExecutionID, actions)
	for _, configMap := range configMaps {
		// Activation bytes travel as binaryData so arbitrary content round-trips
		// exactly; a non-empty snapshot proves this is the materialized
		// activation, not an empty placeholder.
		if len(nestedMap(t, configMap, "binaryData")) == 0 {
			t.Fatalf("activation ConfigMap %v carries no snapshot data",
				nestedMap(t, configMap, "metadata")["name"])
		}
	}
	tokens := captureWorkloadTokens(t, harness, secrets)
	if len(tokens) != len(jobs) {
		t.Fatalf("captured %d workload credential(s); want %d", len(tokens), len(jobs))
	}
	for index, job := range jobs {
		assertJob(t, harness, job, started.ExecutionID, workspaceID, claimName, index)
	}
	for _, pod := range pods {
		assertPod(t, harness, pod, started.ExecutionID, workspaceID, claimName)
	}

	// Positive control for the terminal revocation: a captured credential
	// authenticates while the execution is still live. The refusal after the
	// terminal is therefore a real revocation, not an unauthenticated probe.
	liveAction, liveToken := liveWorkloadToken(t, pods, tokens)
	liveResult, liveError := harness.REST(liveToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: started.ExecutionID,
	}, nil, "")
	if liveError.Code != "" {
		t.Fatalf("captured workload credential for action %s was refused while the execution was live: %s (%s)",
			liveAction, liveError.Code, liveError.Message)
	}
	var liveInspect api.ExecutionInspectResult
	harness.Decode(liveResult, &liveInspect)
	if liveInspect.ExecutionID != started.ExecutionID {
		t.Fatalf("live credential inspected execution %q; want %q", liveInspect.ExecutionID, started.ExecutionID)
	}

	execution := waitKubernetesExecution(t, harness, started.ExecutionID)
	requireSucceeded(t, execution)
	if execution.TaskID != "" {
		t.Fatalf("task-free execution reported task_id %q", execution.TaskID)
	}
	if execution.Cleanup.State != "complete" {
		t.Fatalf("terminal cleanup state = %q; want complete (%+v)", execution.Cleanup.State, execution.Cleanup)
	}
	if len(execution.Actions) != 2 {
		t.Fatalf("execution has %d actions; want 2", len(execution.Actions))
	}
	prepare := actionByName(t, execution, "prepare")
	consume := actionByName(t, execution, "consume")
	if prepare.State != "succeeded" || consume.State != "succeeded" {
		t.Fatalf("action states prepare=%q consume=%q; want succeeded/succeeded", prepare.State, consume.State)
	}
	if prepare.Kind != "container" || consume.Kind != "container" {
		t.Fatalf("action kinds prepare=%q consume=%q; want container/container", prepare.Kind, consume.Kind)
	}
	if prepare.Outputs["artifact"] != kubeArtifact {
		t.Fatalf("prepare artifact = %q; want %q", prepare.Outputs["artifact"], kubeArtifact)
	}
	if consume.Outputs["final"] != kubeFinal {
		t.Fatalf("consume final = %q; want %q", consume.Outputs["final"], kubeFinal)
	}
	// The adapter observed both pods terminate with exit code 0: the two Jobs
	// actually ran to completion on the shared claim.
	for _, action := range []api.ExecutionAction{prepare, consume} {
		if action.Stop == nil || action.Stop.State != "confirmed" {
			t.Fatalf("action %s stop = %+v; want confirmed termination evidence", action.Name, action.Stop)
		}
		if !strings.Contains(action.Stop.Detail, "exit code 0") {
			t.Fatalf("action %s stop detail = %q; want a zero exit code", action.Name, action.Stop.Detail)
		}
	}

	// Terminal cleanup removed both Jobs and released the PVC. Release is
	// eventually consistent with the pvc-protection finalizer, so both are
	// bounded polls, not fixed sleeps. Pods are garbage-collected with their
	// Job; the adapter-owned Secrets and ConfigMaps have no garbage collector,
	// so a Remove that skips either leaves them observable here.
	waitJobsGone(t, harness, started.ExecutionID)
	waitExecutionEphemeraGone(t, harness, started.ExecutionID)
	waitClaimGone(t, harness, claimName)

	// The retained logs are readable through the public operation after the
	// Jobs are gone: the composition drains them before it removes a workload.
	prepareLogs := readActionLog(t, harness, harness.ProjectToken, started.ExecutionID, prepare.ActionID, "stdout")
	consumeLogs := readActionLog(t, harness, harness.ProjectToken, started.ExecutionID, consume.ActionID, "stdout")
	if !strings.Contains(prepareLogs, "prepare: credential accepted and marker written") {
		t.Fatalf("prepare retained stdout does not contain its marker:\n%s", prepareLogs)
	}
	if !strings.Contains(consumeLogs, "consume: workspace and earlier output verified") {
		t.Fatalf("consume retained stdout does not contain its marker:\n%s", consumeLogs)
	}
	// The prepare action printed its mounted credential. The retained log must
	// carry the product's redaction marker on that line, which proves the
	// Kubernetes Logs path redacted a value that was actually present. The
	// token itself is asserted absent below; never print the log here.
	if !strings.Contains(prepareLogs, "credential:"+runtimekube.Redacted) {
		t.Fatalf("prepare retained stdout does not carry the redacted credential line; the Logs path did not redact the mounted credential")
	}

	// The workload credential is revoked at the terminal: every credential a
	// pod received is now refused by the served edge.
	for action, token := range tokens {
		_, publicError := harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: started.ExecutionID,
		}, nil, "")
		if publicError.Code != api.CodeUnauthenticated {
			t.Fatalf("terminal credential for action %s was not refused: code=%q message=%q",
				action, publicError.Code, publicError.Message)
		}
	}

	// The secret value never appears in the composition log, the retained
	// logs, the execution inspection result or any committed event.
	assertTokensAbsent(t, harness, started.ExecutionID, tokens,
		harness.Logs(), prepareLogs, consumeLogs, executionJSON(t, harness, started.ExecutionID))

	// No run-labelled Podman workload leftovers remain. The infrastructure
	// containers belong to the harness and stay until Close.
	containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
	if err != nil {
		t.Fatalf("list run-labelled containers: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("execution %s left run-labelled Podman containers %v", started.ExecutionID, containers)
	}
	present, err := harness.WorkspaceVolumePresent(ctx, workspaceID)
	if err != nil {
		t.Fatalf("probe Podman workspace volume: %v", err)
	}
	if present {
		t.Fatalf("execution %s left a Podman workspace volume for %s", started.ExecutionID, workspaceID)
	}
}

// waitWorkspaceID waits for the durable execution row that names the retained
// workspace.
func waitWorkspaceID(t *testing.T, harness *factory.Harness, executionID string) string {
	t.Helper()
	var lastErr error
	var workspaceID string
	waitForKubernetes(t, harness, "execution workspace row", 2*time.Minute, func(ctx context.Context) (bool, error) {
		found, err := harness.ExecutionWorkspaceID(ctx, executionID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if found == "" {
			lastErr = errors.New("workspace id is empty")
			return false, nil
		}
		workspaceID = found
		return true, nil
	}, func() string { return fmt.Sprintf("last error: %v", lastErr) })
	return workspaceID
}

// waitClaim waits until the deterministic workspace claim is observable and
// carries the durable retention mark. The decorator creates the claim and then
// delegates Retain, which writes the mark; the mark is part of the claim's
// observed state, so the barrier waits for it instead of assuming one call is
// synchronous.
func waitClaim(t *testing.T, harness *factory.Harness, claimName string) runtimekube.Claim {
	t.Helper()
	var claim runtimekube.Claim
	var lastErr error
	waitForKubernetes(t, harness, "retained workspace claim "+claimName, 2*time.Minute, func(ctx context.Context) (bool, error) {
		found, err := harness.KubernetesClient.GetClaim(ctx, harness.KubernetesNamespace, claimName)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if found.Labels[runtimekube.LabelRetained] != "true" ||
			found.Annotations[runtimekube.AnnotationRetained] != "true" {
			lastErr = fmt.Errorf("claim exists but is not durably retained: labels=%v annotations=%v",
				found.Labels, found.Annotations)
			return false, nil
		}
		claim = found
		return true, nil
	}, func() string { return fmt.Sprintf("last error: %v", lastErr) })
	return claim
}

// assertClaimSpec pins the trusted claim template and ownership labels.
func assertClaimSpec(t *testing.T, harness *factory.Harness, claim runtimekube.Claim, workspaceID, claimName string) {
	t.Helper()
	if claim.Name != claimName {
		t.Fatalf("claim name = %q; want %q", claim.Name, claimName)
	}
	if claim.Namespace != factory.KubernetesNamespace {
		t.Fatalf("claim namespace = %q; want %q", claim.Namespace, factory.KubernetesNamespace)
	}
	if claim.Labels[runtimekube.LabelTenant] != factory.TenantID {
		t.Fatalf("claim tenant label = %q; want %q", claim.Labels[runtimekube.LabelTenant], factory.TenantID)
	}
	if claim.Labels[runtimekube.LabelWorkspace] != workspaceID {
		t.Fatalf("claim workspace label = %q; want %q", claim.Labels[runtimekube.LabelWorkspace], workspaceID)
	}
	// Retain marks the claim durably; the mark is part of the claim's observed
	// state, never a precondition the test sets.
	if claim.Labels[runtimekube.LabelRetained] != "true" {
		t.Fatalf("claim retained label = %q; want true", claim.Labels[runtimekube.LabelRetained])
	}
	if claim.Annotations[runtimekube.AnnotationRetained] != "true" {
		t.Fatalf("claim retained annotation = %q; want true", claim.Annotations[runtimekube.AnnotationRetained])
	}

	// The authoritative spec is verified with kubectl, including the values the
	// engine seam does not expose.
	document := kubectlJSON(t, harness, "get", "persistentvolumeclaim", claimName,
		"-n", factory.KubernetesNamespace)
	spec := nestedMap(t, document, "spec")
	if got := nestedString(t, spec, "storageClassName"); got != factory.KubernetesStorageClass {
		t.Fatalf("claim storageClassName = %q; want %q", got, factory.KubernetesStorageClass)
	}
	if got := nestedString(t, spec, "volumeMode"); got != "Filesystem" {
		t.Fatalf("claim volumeMode = %q; want Filesystem", got)
	}
	modes := nestedSlice(t, spec, "accessModes")
	if len(modes) != 1 || modes[0] != factory.KubernetesWorkspaceAccessMode {
		t.Fatalf("claim accessModes = %v; want [%s]", modes, factory.KubernetesWorkspaceAccessMode)
	}
	requests := nestedMap(t, spec, "resources", "requests")
	if got := nestedString(t, requests, "storage"); got != factory.KubernetesWorkspaceSize {
		t.Fatalf("claim storage request = %q; want %q", got, factory.KubernetesWorkspaceSize)
	}
}

// waitJobsAndPods waits until both workload Jobs and both workload Pods exist
// and returns their kubectl JSON documents.
func waitJobsAndPods(t *testing.T, harness *factory.Harness, executionID string, want int) ([]map[string]any, []map[string]any) {
	t.Helper()
	var jobs, pods []map[string]any
	var lastErr error
	waitForKubernetes(t, harness, "workload Jobs and Pods", kubeWorkloadTimeout, func(ctx context.Context) (bool, error) {
		jobItems, err := kubectlItems(ctx, harness, "jobs", executionID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		podItems, err := kubectlItems(ctx, harness, "pods", executionID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		if len(jobItems) != want || len(podItems) != want {
			lastErr = fmt.Errorf("%d Job(s) and %d Pod(s) observed; want %d each", len(jobItems), len(podItems), want)
			return false, nil
		}
		for index, pod := range podItems {
			spec := nestedMap(t, pod, "spec")
			if nestedString(t, spec, "nodeName") == "" {
				lastErr = fmt.Errorf("pod %d is not scheduled", index)
				return false, nil
			}
			status, _ := pod["status"].(map[string]any)
			phase, _ := status["phase"].(string)
			if phase != "Running" && phase != "Succeeded" {
				lastErr = fmt.Errorf("pod %d phase = %q; want Running or Succeeded", index, phase)
				return false, nil
			}
		}
		jobs, pods = jobItems, podItems
		return true, nil
	}, func() string {
		return fmt.Sprintf("last error: %v\njobs:\n%s\npods:\n%s", lastErr,
			kubectlDump(t, harness, "jobs", "-l", runtimekube.LabelExecution+"="+executionID),
			kubectlDump(t, harness, "pods", "-l", runtimekube.LabelExecution+"="+executionID))
	})
	return jobs, pods
}

// assertJob pins one generated Job: tenant namespace, ownership labels, the
// digest-pinned image and the shared workspace claim mount.
func assertJob(t *testing.T, harness *factory.Harness, job map[string]any, executionID, workspaceID, claimName string, index int) {
	t.Helper()
	metadata := nestedMap(t, job, "metadata")
	if got := nestedString(t, metadata, "namespace"); got != factory.KubernetesNamespace {
		t.Fatalf("job %d namespace = %q; want %q", index, got, factory.KubernetesNamespace)
	}
	labels := nestedMap(t, metadata, "labels")
	if got := labels[runtimekube.LabelTenant]; got != factory.TenantID {
		t.Fatalf("job %d tenant label = %v; want %q", index, got, factory.TenantID)
	}
	if got := labels[runtimekube.LabelExecution]; got != executionID {
		t.Fatalf("job %d execution label = %v; want %q", index, got, executionID)
	}
	if got := labels[runtimekube.LabelWorkspace]; got != workspaceID {
		t.Fatalf("job %d workspace label = %v; want %q", index, got, workspaceID)
	}
	if labels[runtimekube.LabelAction] == "" || labels[runtimekube.LabelWorkload] == "" {
		t.Fatalf("job %d lacks action/workload identity labels: %v", index, labels)
	}

	podSpec := nestedMap(t, job, "spec", "template", "spec")
	containers := nestedSlice(t, podSpec, "containers")
	if len(containers) != 1 {
		t.Fatalf("job %d has %d containers; want 1", index, len(containers))
	}
	container := kubeMap(t, containers[0], "job container")
	if got := nestedString(t, container, "image"); got != harness.Image.Reference {
		t.Fatalf("job %d image = %q; want the pinned digest %q", index, got, harness.Image.Reference)
	}
	mounts := nestedSlice(t, container, "volumeMounts")
	mountName := ""
	for _, raw := range mounts {
		mount := kubeMap(t, raw, "volume mount")
		if nestedString(t, mount, "mountPath") == "/workspace" {
			mountName = nestedString(t, mount, "name")
		}
	}
	if mountName == "" {
		t.Fatalf("job %d has no /workspace mount: %v", index, mounts)
	}
	volumes := nestedSlice(t, podSpec, "volumes")
	claimMount := ""
	for _, raw := range volumes {
		volume := kubeMap(t, raw, "pod volume")
		if nestedString(t, volume, "name") != mountName {
			continue
		}
		claim := nestedMap(t, volume, "persistentVolumeClaim")
		claimMount = nestedString(t, claim, "claimName")
	}
	if claimMount != claimName {
		t.Fatalf("job %d workspace volume claim = %q; want %q", index, claimMount, claimName)
	}
}

// assertPod pins one workload Pod to the same tenant, workspace and claim.
func assertPod(t *testing.T, harness *factory.Harness, pod map[string]any, executionID, workspaceID, claimName string) {
	t.Helper()
	metadata := nestedMap(t, pod, "metadata")
	if got := nestedString(t, metadata, "namespace"); got != factory.KubernetesNamespace {
		t.Fatalf("pod namespace = %q; want %q", got, factory.KubernetesNamespace)
	}
	labels := nestedMap(t, metadata, "labels")
	if labels[runtimekube.LabelExecution] != executionID ||
		labels[runtimekube.LabelWorkspace] != workspaceID ||
		labels[runtimekube.LabelTenant] != factory.TenantID {
		t.Fatalf("pod identity labels = %v; want execution=%s workspace=%s tenant=%s",
			labels, executionID, workspaceID, factory.TenantID)
	}
	volumes := nestedSlice(t, nestedMap(t, pod, "spec"), "volumes")
	found := false
	for _, raw := range volumes {
		volume := kubeMap(t, raw, "pod volume")
		claim := volume["persistentVolumeClaim"]
		if claim == nil {
			continue
		}
		if nestedString(t, kubeMap(t, claim, "persistentVolumeClaim"), "claimName") == claimName {
			found = true
		}
	}
	if !found {
		t.Fatalf("pod does not mount claim %q: %v", claimName, volumes)
	}
}

// debugKubernetesExecution renders the execution inspection, Jobs, Pods and pod
// logs for a failure path. It never fails the test itself.
func debugKubernetesExecution(t *testing.T, harness *factory.Harness, executionID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var builder strings.Builder
	if result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, ""); publicError.Code == "" {
		_, _ = builder.WriteString("inspect: ")
		_, _ = builder.WriteString(factory.SanitizeDiagnostic(string(result.Body)))
		_, _ = builder.WriteString("\n")
	} else {
		_, _ = fmt.Fprintf(&builder, "inspect refused: %s (%s)\n", publicError.Code, publicError.Message)
	}
	jobs, err := harness.KubernetesClient.ListJobs(ctx, harness.KubernetesNamespace,
		map[string]string{runtimekube.LabelExecution: executionID})
	if err != nil {
		_, _ = fmt.Fprintf(&builder, "list jobs: %v\n", err)
	}
	for _, job := range jobs {
		_, _ = fmt.Fprintf(&builder, "job %s labels=%v status=%+v image=%s\n", job.Name, job.Labels, job.Status, job.Spec.Image)
	}
	pods, err := harness.KubernetesClient.ListPods(ctx, harness.KubernetesNamespace,
		map[string]string{runtimekube.LabelExecution: executionID})
	if err != nil {
		_, _ = fmt.Fprintf(&builder, "list pods: %v\n", err)
	}
	// A workload prints its mounted credential to prove the Logs path redacts
	// it. This diagnostic reads raw pod logs, so it must redact the same values
	// itself or it would leak the secret into the test output. If the values
	// cannot be read, the logs are withheld rather than printed raw.
	redactions, redactionErr := executionSecretValues(ctx, harness, executionID)
	for _, pod := range pods {
		_, _ = fmt.Fprintf(&builder, "pod %s phase=%s node=%s containers=%+v\n", pod.Name, pod.Phase, pod.NodeName, pod.Containers)
		if redactionErr != nil {
			_, _ = fmt.Fprintf(&builder, "pod %s logs withheld: redaction values unavailable: %v\n", pod.Name, redactionErr)
			continue
		}
		for _, stream := range []string{"stdout", "stderr"} {
			page, err := harness.KubernetesClient.ReadPodLogs(ctx, harness.KubernetesNamespace, pod.Name,
				runtimekube.PodLogRequest{Stream: stream, Limit: runtimekube.MaxLogBytes})
			if err != nil {
				_, _ = fmt.Fprintf(&builder, "pod %s %s logs: %v\n", pod.Name, stream, err)
				continue
			}
			_, _ = fmt.Fprintf(&builder, "pod %s %s logs:\n%s\n", pod.Name, stream,
				factory.SanitizeDiagnostic(string(page.Data), redactions...))
		}
	}
	return builder.String()
}

// executionSecretValues returns every value stored in the execution's workload
// Secrets so a diagnostic can redact raw pod logs. It never returns the values
// to a caller that logs them.
func executionSecretValues(ctx context.Context, harness *factory.Harness, executionID string) ([]string, error) {
	items, err := kubectlItems(ctx, harness, "secrets", executionID)
	if err != nil {
		return nil, err
	}
	var values []string
	for _, item := range items {
		data, ok := item["data"].(map[string]any)
		if !ok {
			continue
		}
		for _, encoded := range data {
			text, ok := encoded.(string)
			if !ok {
				return nil, fmt.Errorf("secret data is %T; want a base64 string", encoded)
			}
			decoded, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				return nil, err
			}
			if len(decoded) > 0 {
				values = append(values, string(decoded))
			}
		}
	}
	return values, nil
}

// jobActions returns the action id every captured Job carries. The activation
// snapshot and credential Secret of a workload carry the same action label, so
// the sets are directly comparable.
func jobActions(t *testing.T, jobs []map[string]any) map[string]struct{} {
	t.Helper()
	actions := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		labels := nestedMap(t, job, "metadata", "labels")
		action, _ := labels[runtimekube.LabelAction].(string)
		if action == "" {
			t.Fatalf("job %v carries no action label", nestedMap(t, job, "metadata")["name"])
		}
		actions[action] = struct{}{}
	}
	return actions
}

// captureLabelledObjects waits until exactly one object of kind carries the
// execution label for every action of the captured Jobs, and returns the
// kubectl JSON documents. It is the positive control for cleanup: these objects
// existed while the workload was live and must be gone after the terminal.
func captureLabelledObjects(t *testing.T, harness *factory.Harness, kind, executionID string, actions map[string]struct{}) []map[string]any {
	t.Helper()
	var captured []map[string]any
	var lastErr error
	waitForKubernetes(t, harness, "workload "+kind, time.Minute, func(ctx context.Context) (bool, error) {
		items, err := kubectlItems(ctx, harness, kind, executionID)
		if err != nil {
			lastErr = err
			return false, nil
		}
		seen := make(map[string]struct{}, len(items))
		for _, item := range items {
			labels := nestedMap(t, item, "metadata", "labels")
			action, _ := labels[runtimekube.LabelAction].(string)
			if _, ok := actions[action]; !ok {
				lastErr = fmt.Errorf("%s %v carries action %q outside the captured Jobs",
					kind, nestedMap(t, item, "metadata")["name"], action)
				return false, nil
			}
			if _, duplicate := seen[action]; duplicate {
				lastErr = fmt.Errorf("%d %s object(s) for action %q; want exactly one", len(items), kind, action)
				return false, nil
			}
			seen[action] = struct{}{}
		}
		if len(seen) != len(actions) {
			lastErr = fmt.Errorf("%d %s object(s) for %d action(s); want exactly one per action",
				len(seen), kind, len(actions))
			return false, nil
		}
		captured = items
		return true, nil
	}, func() string {
		return fmt.Sprintf("last error: %v\n%s:\n%s", lastErr, kind,
			kubectlDump(t, harness, kind, "-l", runtimekube.LabelExecution+"="+executionID))
	})
	return captured
}

// captureWorkloadTokens decodes the execution credential from every workload
// credential Secret, keyed by the action label. The values are test evidence
// only: they are never logged, and the test asserts they do not appear in logs
// or events.
func captureWorkloadTokens(t *testing.T, harness *factory.Harness, secrets []map[string]any) map[string]string {
	t.Helper()
	tokens := make(map[string]string, len(secrets))
	for _, secret := range secrets {
		action := nestedString(t, nestedMap(t, secret, "metadata", "labels"), runtimekube.LabelAction)
		data := nestedMap(t, secret, "data")
		encoded, ok := data[id.SecretTokenName]
		if !ok {
			t.Fatalf("workload credential Secret %v has no %s entry",
				nestedMap(t, secret, "metadata")["name"], id.SecretTokenName)
		}
		text, ok := encoded.(string)
		if !ok {
			t.Fatalf("workload credential Secret data is %T; want a base64 string", encoded)
		}
		value, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			t.Fatalf("decode workload credential: %v", err)
		}
		if len(value) == 0 {
			t.Fatalf("workload credential Secret carries an empty token")
		}
		if _, duplicate := tokens[action]; duplicate {
			t.Fatalf("action %q has more than one workload credential Secret", action)
		}
		tokens[action] = string(value)
	}
	return tokens
}

// liveWorkloadToken returns the credential of the action whose pod is still
// running, falling back to any captured credential when both actions have
// already exited. Credentials are revoked only at the execution terminal, so a
// captured credential is a pre-revocation positive control either way.
func liveWorkloadToken(t *testing.T, pods []map[string]any, tokens map[string]string) (string, string) {
	t.Helper()
	for _, pod := range pods {
		status, _ := pod["status"].(map[string]any)
		phase, _ := status["phase"].(string)
		if phase != "Running" {
			continue
		}
		action := nestedString(t, nestedMap(t, pod, "metadata", "labels"), runtimekube.LabelAction)
		if token, ok := tokens[action]; ok {
			return action, token
		}
	}
	for action, token := range tokens {
		return action, token
	}
	t.Fatalf("no captured workload credential is available for a live authentication")
	return "", ""
}

// waitKubernetesExecution waits for a terminal execution whose cleanup is
// "complete". A "none" cleanup record is not accepted: this test proves the
// Kubernetes-owned resources were removed and the claim released, so it waits
// for the product's explicit complete record.
//
// The factory clock is frozen while cleanup retries pace themselves on the
// persisted durable schedule (next_retry_at = commit.At + backoff), so a failed
// attempt would never become due. Advance the harness clock by a fixed step on
// each poll only while the inspected cleanup is pending with a spent failed
// attempt; advancing unconditionally would also move action deadlines and
// credential lifetimes during preparation. The wait stays bounded by
// kubeCleanupTimeout.
func waitKubernetesExecution(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	deadline := time.Now().Add(kubeCleanupTimeout)
	for {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		inspected := false
		if publicError.Code == "" {
			harness.Decode(result, &last)
			inspected = true
			switch last.State {
			case "succeeded", "failed", "cancelled":
				if last.Cleanup.State == "complete" {
					return last
				}
			}
		}
		if !time.Now().Before(deadline) {
			t.Logf("execution %s last inspect: state=%q cleanup=%+v failure=%+v actions=%+v",
				executionID, last.State, last.Cleanup, last.Failure, last.Actions)
			dumpExecutionLogs(t, harness, harness.ProjectToken, executionID, last)
			t.Logf("composition log:\n%s", factory.SanitizeDiagnostic(harness.Logs()))
			t.Logf("%s", debugKubernetesExecution(t, harness, executionID))
			t.Fatalf("execution %s did not reach a terminal state with complete cleanup within %s: state=%q cleanup=%+v",
				executionID, kubeCleanupTimeout, last.State, last.Cleanup)
		}
		if inspected && last.Cleanup.State == "pending" && last.Cleanup.Attempts > 0 {
			harness.Clock.Advance(kubeCleanupRetryClockStep)
		}
		time.Sleep(kubePollInterval)
	}
}

// waitJobsGone waits until no Job carries the execution label.
func waitJobsGone(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	var lastErr error
	waitForKubernetes(t, harness, "workload Jobs deleted", kubeDeleteTimeout, func(ctx context.Context) (bool, error) {
		jobs, err := harness.KubernetesClient.ListJobs(ctx, harness.KubernetesNamespace,
			map[string]string{runtimekube.LabelExecution: executionID})
		if err != nil {
			lastErr = err
			return false, nil
		}
		if len(jobs) != 0 {
			lastErr = fmt.Errorf("%d Job(s) still present", len(jobs))
			return false, nil
		}
		return true, nil
	}, func() string { return fmt.Sprintf("last error: %v", lastErr) })
}

// waitExecutionEphemeraGone waits until no Pod, Secret or ConfigMap carries the
// execution label. Pods are garbage-collected with their Job; the adapter-owned
// Secrets and ConfigMaps have no Kubernetes garbage collector, so this is where
// a Remove that skips either is observable. The Secrets and ConfigMaps were
// captured while the workload was live, so this is not a vacuous "nothing was
// ever created" check.
func waitExecutionEphemeraGone(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	for _, kind := range []string{"pods", "secrets", "configmaps"} {
		var lastErr error
		waitForKubernetes(t, harness, "workload "+kind+" deleted", kubeDeleteTimeout, func(ctx context.Context) (bool, error) {
			items, err := kubectlItems(ctx, harness, kind, executionID)
			if err != nil {
				lastErr = err
				return false, nil
			}
			if len(items) != 0 {
				lastErr = fmt.Errorf("%d %s still present", len(items), kind)
				return false, nil
			}
			return true, nil
		}, func() string {
			return fmt.Sprintf("last error: %v\n%s:\n%s", lastErr, kind,
				kubectlDump(t, harness, kind, "-l", runtimekube.LabelExecution+"="+executionID))
		})
	}
}

// waitClaimGone waits until the retained workspace claim is observably absent.
// The adapter's Release may report success a moment before the
// pvc-protection finalizer completes; this is the bounded observable barrier.
func waitClaimGone(t *testing.T, harness *factory.Harness, claimName string) {
	t.Helper()
	var lastErr error
	waitForKubernetes(t, harness, "workspace claim release", kubeReleaseTimeout, func(ctx context.Context) (bool, error) {
		_, err := harness.KubernetesClient.GetClaim(ctx, harness.KubernetesNamespace, claimName)
		if errors.Is(err, runtimekube.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			lastErr = err
			return false, nil
		}
		lastErr = fmt.Errorf("claim %s is still present", claimName)
		return false, nil
	}, func() string {
		return fmt.Sprintf("last error: %v\nclaim:\n%s", lastErr,
			kubectlDump(t, harness, "persistentvolumeclaim", claimName))
	})
}

// readActionLog reads one retained stream through the public operation and
// concatenates the chunks. It fails on a refusal or a missing stream.
func readActionLog(t *testing.T, harness *factory.Harness, token, executionID, actionID, stream string) string {
	t.Helper()
	result, publicError := harness.REST(token, "execution.logs.read", api.ExecutionLogsReadRequest{
		ExecutionID: executionID,
		ActionID:    actionID,
		Stream:      stream,
	}, nil, "")
	if publicError.Code != "" {
		t.Fatalf("read retained %s logs of action %s: %s (%s)", stream, actionID, publicError.Code, publicError.Message)
	}
	var read api.ExecutionLogsReadResult
	harness.Decode(result, &read)
	var builder strings.Builder
	for _, chunk := range read.Chunks {
		_, _ = builder.Write(chunk.Data)
	}
	return builder.String()
}

// executionJSON returns the raw public inspection body as text so a secret
// assertion covers what the API actually exposes.
func executionJSON(t *testing.T, harness *factory.Harness, executionID string) string {
	t.Helper()
	result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	if publicError.Code != "" {
		t.Fatalf("inspect execution for secret scan: %s (%s)", publicError.Code, publicError.Message)
	}
	return string(result.Body)
}

// assertTokensAbsent fails if any captured workload credential appears in the
// supplied evidence. It never prints a token. The event scan is a positive
// control: it must contain a committed event naming this execution, so the
// absence check cannot pass over an empty scan.
func assertTokensAbsent(t *testing.T, harness *factory.Harness, executionID string, tokens map[string]string, evidence ...string) {
	t.Helper()
	events, eventsJSON := readAllEvents(t, harness)
	named := false
	for _, event := range events {
		if event.SubjectID == executionID {
			named = true
			break
		}
	}
	if !named {
		t.Fatalf("no committed event names execution %s; the secret scan read no execution evidence", executionID)
	}
	all := append(append([]string(nil), evidence...), string(eventsJSON))
	for action, token := range tokens {
		for evidenceIndex, text := range all {
			if strings.Contains(text, token) {
				t.Fatalf("workload credential for action %s appears in evidence item %d (execution %s)",
					action, evidenceIndex, executionID)
			}
		}
	}
}

// readAllEvents reads every committed event page and returns the decoded
// envelopes plus their JSON encoding for text scans.
func readAllEvents(t *testing.T, harness *factory.Harness) ([]api.Event, []byte) {
	t.Helper()
	var events []api.Event
	cursor := ""
	for range 1000 {
		request := api.EventReadRequest{
			ProjectID: factory.ProjectID,
			Page:      &api.Page{Limit: api.MaxPageLimit},
		}
		if cursor != "" {
			request.Page.Cursor = cursor
		}
		result, publicError := harness.REST(harness.ProjectToken, "event.read", request, nil, "")
		if publicError.Code != "" {
			t.Fatalf("read committed events: %s (%s)", publicError.Code, publicError.Message)
		}
		var read api.EventReadResult
		harness.Decode(result, &read)
		events = append(events, read.Events...)
		if !read.HasMore {
			break
		}
		if read.Next == "" || read.Next == cursor {
			t.Fatalf("event page did not advance: next=%q", read.Next)
		}
		cursor = read.Next
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("encode events: %v", err)
	}
	return events, encoded
}

// waitForKubernetes polls one condition with a bounded deadline and reports
// the last observation on failure.
func waitForKubernetes(t *testing.T, harness *factory.Harness, what string, timeout time.Duration, condition func(context.Context) (bool, error), dump func() string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	var lastErr error
	for {
		ok, err := condition(ctx)
		if err != nil {
			lastErr = err
		}
		if ok {
			return
		}
		if ctx.Err() != nil {
			if dump != nil {
				t.Logf("%s: %s", what, dump())
			}
			t.Logf("composition log:\n%s", factory.SanitizeDiagnostic(harness.Logs()))
			t.Fatalf("condition %q was not satisfied within %s (last error %v)", what, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
		case <-time.After(kubePollInterval):
		}
	}
}

// kubectlItems lists one resource kind by execution label.
func kubectlItems(ctx context.Context, harness *factory.Harness, kind, executionID string) ([]map[string]any, error) {
	result, err := harness.Cluster.Kubectl(ctx, "get", kind, "-n", factory.KubernetesNamespace,
		"-l", runtimekube.LabelExecution+"="+executionID, "-o", "json")
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(result.Stdout, &document); err != nil {
		return nil, fmt.Errorf("decode kubectl %s JSON: %w", kind, err)
	}
	raw, ok := document["items"]
	if !ok {
		return nil, fmt.Errorf("kubectl %s output has no items", kind)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("kubectl %s items is %T", kind, raw)
	}
	decoded := make([]map[string]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("kubectl %s item is %T", kind, item)
		}
		decoded = append(decoded, object)
	}
	return decoded, nil
}

// kubectlJSON runs one kubectl get with -o json and decodes the document.
func kubectlJSON(t *testing.T, harness *factory.Harness, args ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	document, err := harness.Cluster.GetJSON(ctx, args...)
	if err != nil {
		t.Fatalf("kubectl %v: %v", args, err)
	}
	return document
}

// kubectlDump renders one kubectl get as diagnostic text. A failure is
// returned as text, never as a test failure: it is only used in a failure path.
// A Secret kind is rendered as a name list so no payload is ever requested, and
// the result passes through the shared diagnostic sanitizer.
func kubectlDump(t *testing.T, harness *factory.Harness, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	full := factory.DiagnosticKubectlGetArgs(args...)
	full = append(full, "-n", factory.KubernetesNamespace)
	result, err := harness.Cluster.Kubectl(ctx, full...)
	if err != nil {
		return fmt.Sprintf("kubectl %v failed: %v", full, err)
	}
	return factory.SanitizeDiagnostic(string(result.Stdout))
}

// kubeMap asserts one decoded JSON object.
func kubeMap(t *testing.T, value any, what string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T; want an object", what, value)
	}
	return object
}

// nestedMap walks object keys and returns the final object.
func nestedMap(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()
	value := nested(t, object, path...)
	return kubeMap(t, value, strings.Join(path, "."))
}

// nestedSlice walks object keys and returns the final array.
func nestedSlice(t *testing.T, object map[string]any, path ...string) []any {
	t.Helper()
	value := nested(t, object, path...)
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T; want an array", strings.Join(path, "."), value)
	}
	return items
}

// nestedString walks object keys and returns the final string.
func nestedString(t *testing.T, object map[string]any, path ...string) string {
	t.Helper()
	value := nested(t, object, path...)
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s is %T; want a string", strings.Join(path, "."), value)
	}
	return text
}

// nested walks decoded JSON object keys.
func nested(t *testing.T, object map[string]any, path ...string) any {
	t.Helper()
	current := any(object)
	for index, key := range path {
		asObject, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s is %T; want an object", strings.Join(path[:index], "."), current)
		}
		next, ok := asObject[key]
		if !ok {
			t.Fatalf("%s is absent", strings.Join(path[:index+1], "."))
		}
		current = next
	}
	return current
}
