//go:build component

package kubernetesconformance

// Component proof: a workload prepared but never started owns partial
// activation resources and an unconsumed workspace claim. Remove deletes only
// that identity's resources without requesting a stop, and Release is the sole
// owner of the claim.

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestPreparationFailureReleasesUnconsumedClaimAndPartialResources prepares one
// identity without starting it, proves its activation ConfigMap and credential
// Secret exist, removes them through the adapter's never-started path, and
// releases the still-unconsumed claim.
func TestPreparationFailureReleasesUnconsumedClaimAndPartialResources(t *testing.T) {
	t.Parallel()

	workspaceID := id.WorkspaceID("workspace-kube-prep-failure")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	if err := fx.Adapter.Retain(ctx, workspaceID); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	identity := testIdentity("exec-prep-failure", "action-prep-failure", "workload-prep-failure", "idem-prep-failure")
	request := prepareRequest(identity, workspaceID, "denied", "", testContainer([]string{"true"}, nil), nil, "tok-prep-failure")
	if _, err := fx.Adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Positive control: preparing materialized partial resources even though no
	// Start was requested.
	if got := labelledCount(t, ctx, fx, "configmaps", string(identity.WorkloadID)); got != 1 {
		t.Fatalf("activation ConfigMaps = %d; want 1 partial resource", got)
	}
	if got := labelledCount(t, ctx, fx, "secrets", string(identity.WorkloadID)); got != 1 {
		t.Fatalf("credential Secrets = %d; want 1 partial resource", got)
	}
	if got := labelledCount(t, ctx, fx, "jobs", string(identity.WorkloadID)); got != 0 {
		t.Fatalf("Jobs = %d; want none before Start", got)
	}

	// Remove is sufficient for an affirmatively never-started identity: it
	// deletes the partial resources without a stop request and never touches the
	// retained claim.
	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want removed", removal)
	}
	if got := labelledCount(t, ctx, fx, "configmaps", string(identity.WorkloadID)); got != 0 {
		t.Fatalf("activation ConfigMaps after Remove = %d; want 0", got)
	}
	if got := labelledCount(t, ctx, fx, "secrets", string(identity.WorkloadID)); got != 0 {
		t.Fatalf("credential Secrets after Remove = %d; want 0", got)
	}

	// The claim is still retained: only Release may delete it.
	status, err := fx.Adapter.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status after Remove: %v", err)
	}
	if !status.Available {
		t.Fatalf("workspace status after Remove = %+v; want still available", status)
	}

	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
	if _, err := fx.Client.GetClaim(ctx, testNamespace, claimName); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("unconsumed claim read after Release = %v; want ErrNotFound", err)
	}
	status, err = fx.Adapter.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status after Release: %v", err)
	}
	if status.Available {
		t.Fatalf("released workspace is still available: %+v", status)
	}
}

// labelledCount lists one resource kind by the workload identity label.
func labelledCount(t *testing.T, ctx context.Context, fx *fixture, kind, workload string) int {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", kind, "-n", testNamespace,
		"-l", runtimekube.LabelWorkload+"="+workload)
	if err != nil {
		t.Fatalf("kubectl get %s by workload label: %v", kind, err)
	}
	items, ok := document["items"].([]any)
	if !ok {
		t.Fatalf("kubectl get %s items is %T", kind, document["items"])
	}
	return len(items)
}
