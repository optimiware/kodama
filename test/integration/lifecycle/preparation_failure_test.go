//go:build integration

package lifecycle

// Regression: a Kubernetes preparation failure must not strand the
// unconsumed workspace claim. The workspace is retained (and its PVC created)
// before the workload is prepared; when preparation fails before Start is
// issued, cleanup must remove the failed identity's partial resources, release
// the claim and record the attribution instead of waiting forever on an absent
// object and an unconfirmed Stop.
//
// This test runs on the real kind composition and is intentionally not part of
// the unit baseline.

import (
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestKubernetesPreparationFailureCleanup proves that boundary on a real
// kind cluster: an activation secret refused during preparation fails the
// execution, and cleanup converges to complete with the failed identity named,
// the unconsumed claim released and no workload ephemera left behind.
func TestKubernetesPreparationFailureCleanup(t *testing.T) {
	acquireLifecycleKindSlot(t)
	harness := factory.StartKubernetes(t,
		factory.WithSecretRules(secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(factory.TenantID),
				ProjectID: id.ProjectID(factory.ProjectID),
			},
			Names: []id.SecretName{pipelineSecretName},
		}),
		factory.WithSecrets(map[string]string{
			pipelineSecretName: pipelineSecretValue,
			deniedSecretName:   deniedSecretValue,
		}),
	)

	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "denied",
			Image:    harness.Image.Reference,
			Secrets:  []string{deniedSecretName},
			Commands: []string{"true"},
			Timeout:  kubeActionTimeout,
		}},
	})
	applied := applyPipeline(t, harness, "preparation-failure", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-preparation-failure")
	if started.ExecutionID == "" {
		t.Fatalf("pipeline.start returned an empty execution id: %+v", started)
	}

	// The workspace is retained before preparation, so its deterministic claim
	// name is available from the durable execution row even though the workload
	// never started.
	workspaceID := waitWorkspaceID(t, harness, started.ExecutionID)
	claimName, err := runtimekube.WorkspaceClaimName(id.WorkspaceID(workspaceID))
	if err != nil {
		t.Fatalf("WorkspaceClaimName(%q): %v", workspaceID, err)
	}

	execution := waitKubernetesExecution(t, harness, started.ExecutionID)
	if execution.State != "failed" {
		t.Fatalf("execution state = %q; want failed", execution.State)
	}
	if execution.Failure == nil || execution.Failure.Kind != "preparation" {
		t.Fatalf("failure = %+v; want preparation", execution.Failure)
	}
	if execution.Cleanup.State != "complete" {
		t.Fatalf("cleanup state = %q; want complete (%+v)", execution.Cleanup.State, execution.Cleanup)
	}
	if execution.Cleanup.WorkspaceID != workspaceID {
		t.Fatalf("cleanup workspace = %q; want %q", execution.Cleanup.WorkspaceID, workspaceID)
	}
	if execution.Cleanup.WorkloadID == "" {
		t.Fatal("cleanup did not name the failed identity; the record is not attributable")
	}

	// The failed action never started, so no confirmed stop evidence exists and
	// nothing may claim a workload ran.
	action := actionByName(t, execution, "denied")
	if action.State != "failed" {
		t.Fatalf("failed action state = %q; want failed", action.State)
	}
	if action.Stop != nil && action.Stop.State == "confirmed" {
		t.Fatalf("preparation failure fabricated stop evidence: %+v", action.Stop)
	}

	// No partial Job, Pod, Secret or ConfigMap survives the failed identity, and
	// the unconsumed claim is gone. Both are bounded observable barriers.
	waitJobsGone(t, harness, started.ExecutionID)
	waitExecutionEphemeraGone(t, harness, started.ExecutionID)
	waitClaimGone(t, harness, claimName)
	if _, err := harness.KubernetesClient.GetClaim(t.Context(), factory.KubernetesNamespace, claimName); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("unconsumed workspace claim read = %v; want ErrNotFound", err)
	}
}
