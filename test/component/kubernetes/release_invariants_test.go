package kubernetesconformance

// Hermetic invariants for Release's accepted-deletion-in-progress path.
//
// The component test drives the real adapter against a real kind cluster; these
// cases model the only part of that observation the product does not own. While
// a claim's deletion is held open by the test-owned finalizer, the cluster's own
// controllers (notably the PVC protection controller) add and remove their
// finalizers asynchronously. The product owes: the claim is still present with
// the same deletion timestamp, the test-owned hold finalizer is untouched, the
// claim is still owned by the workspace and tenant, and it is unretained.
// Nothing else about the object is Release's to preserve.

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// holdFinalizer is the test-owned finalizer the workspace component test places
// on its claim so the deletion stays in progress for the whole assertion. The
// product must never remove it: that is what proves Release did not clear state
// or force the deletion.
const holdFinalizer = "kodama.test/hold"

// terminatingReleaseHeldError reports whether after still satisfies the Release
// contract for an ownership-preserving, unretained deletion in progress that was
// observed as before. It deliberately ignores controller-owned finalizers other
// than the test's hold, and every resourceVersion, generation, status and
// timestamp other than the deletion timestamp the test already observed.
func terminatingReleaseHeldError(before, after runtimekube.Claim, workspaceID id.WorkspaceID, tenant id.TenantID) error {
	if after.DeletionTimestamp == nil {
		return fmt.Errorf("terminating claim %q lost its deletion timestamp", after.Name)
	}
	if before.DeletionTimestamp == nil || !after.DeletionTimestamp.Equal(*before.DeletionTimestamp) {
		return fmt.Errorf("terminating claim %q changed its deletion timestamp: %v -> %v", after.Name, before.DeletionTimestamp, after.DeletionTimestamp)
	}
	// Only the test-owned hold finalizer is the product's to preserve. Every
	// other finalizer belongs to a cluster controller that may add or remove it
	// asynchronously, so the set itself is never compared.
	if !slices.Contains(after.Finalizers, holdFinalizer) {
		return fmt.Errorf("terminating claim %q lost the test-owned hold finalizer %q: finalizers = %v", after.Name, holdFinalizer, after.Finalizers)
	}
	if after.Labels[runtimekube.LabelWorkspace] != string(workspaceID) {
		return fmt.Errorf("terminating claim %q changed its workspace label: labels = %v", after.Name, after.Labels)
	}
	if after.Labels[runtimekube.LabelTenant] != string(tenant) {
		return fmt.Errorf("terminating claim %q changed its tenant label: labels = %v", after.Name, after.Labels)
	}
	if _, retained := after.Annotations[runtimekube.AnnotationRetained]; retained {
		return fmt.Errorf("terminating claim %q was retained: annotations = %v", after.Name, after.Annotations)
	}
	if _, retained := after.Labels[runtimekube.LabelRetained]; retained {
		return fmt.Errorf("terminating claim %q was retained: labels = %v", after.Name, after.Labels)
	}
	return nil
}

// TestTerminatingReleaseInvariantsAllowControllerFinalizerDrift pins the
// contract the real component run asserts. The set-equality case guards
// against a false failure: the PVC protection controller removes its finalizer
// while the test's hold remains, and Release has done nothing wrong.
func TestTerminatingReleaseInvariantsAllowControllerFinalizerDrift(t *testing.T) {
	t.Parallel()

	const (
		workspace = id.WorkspaceID("workspace-kube-13")
		tenant    = id.TenantID("tenant-acme")
	)
	deletedAt := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
	otherTime := deletedAt.Add(time.Second)
	held := runtimekube.Claim{
		Name:      "kodama-workspace",
		Namespace: string(tenant),
		Labels: map[string]string{
			runtimekube.LabelWorkspace: string(workspace),
			runtimekube.LabelTenant:    string(tenant),
		},
		Finalizers:        []string{"kubernetes.io/pvc-protection", holdFinalizer},
		DeletionTimestamp: &deletedAt,
	}
	withLabels := func(base map[string]string, extra map[string]string) map[string]string {
		copied := make(map[string]string, len(base)+len(extra))
		maps.Copy(copied, base)
		maps.Copy(copied, extra)
		return copied
	}

	cases := []struct {
		name    string
		after   runtimekube.Claim
		wantErr bool
	}{
		{
			name:  "controller removes its own finalizer",
			after: runtimekube.Claim{Name: held.Name, Namespace: held.Namespace, Labels: held.Labels, Finalizers: []string{holdFinalizer}, DeletionTimestamp: &deletedAt},
		},
		{
			name:  "controller adds another finalizer",
			after: runtimekube.Claim{Name: held.Name, Namespace: held.Namespace, Labels: held.Labels, Finalizers: []string{holdFinalizer, "example.com/other"}, DeletionTimestamp: &deletedAt},
		},
		{
			name: "controller changes other labels and annotations",
			after: runtimekube.Claim{
				Name: held.Name, Namespace: held.Namespace,
				Labels:            withLabels(held.Labels, map[string]string{"example.com/controller": "owned"}),
				Annotations:       map[string]string{"example.com/controller": "owned"},
				Finalizers:        []string{holdFinalizer},
				DeletionTimestamp: &deletedAt,
			},
		},
		{
			name:    "hold finalizer removed",
			after:   runtimekube.Claim{Name: held.Name, Namespace: held.Namespace, Labels: held.Labels, Finalizers: []string{"kubernetes.io/pvc-protection"}, DeletionTimestamp: &deletedAt},
			wantErr: true,
		},
		{
			name:    "deletion timestamp cleared",
			after:   runtimekube.Claim{Name: held.Name, Namespace: held.Namespace, Labels: held.Labels, Finalizers: []string{holdFinalizer}},
			wantErr: true,
		},
		{
			name:    "deletion timestamp changed",
			after:   runtimekube.Claim{Name: held.Name, Namespace: held.Namespace, Labels: held.Labels, Finalizers: []string{holdFinalizer}, DeletionTimestamp: &otherTime},
			wantErr: true,
		},
		{
			name: "workspace ownership changed",
			after: runtimekube.Claim{
				Name: held.Name, Namespace: held.Namespace,
				Labels:            withLabels(held.Labels, map[string]string{runtimekube.LabelWorkspace: "other-workspace"}),
				Finalizers:        []string{holdFinalizer},
				DeletionTimestamp: &deletedAt,
			},
			wantErr: true,
		},
		{
			name: "tenant ownership changed",
			after: runtimekube.Claim{
				Name: held.Name, Namespace: held.Namespace,
				Labels:            withLabels(held.Labels, map[string]string{runtimekube.LabelTenant: "tenant-other"}),
				Finalizers:        []string{holdFinalizer},
				DeletionTimestamp: &deletedAt,
			},
			wantErr: true,
		},
		{
			name: "retention annotation reintroduced",
			after: runtimekube.Claim{
				Name: held.Name, Namespace: held.Namespace,
				Labels:            held.Labels,
				Annotations:       map[string]string{runtimekube.AnnotationRetained: "true"},
				Finalizers:        []string{holdFinalizer},
				DeletionTimestamp: &deletedAt,
			},
			wantErr: true,
		},
		{
			name: "retention label reintroduced",
			after: runtimekube.Claim{
				Name: held.Name, Namespace: held.Namespace,
				Labels:            withLabels(held.Labels, map[string]string{runtimekube.LabelRetained: "true"}),
				Finalizers:        []string{holdFinalizer},
				DeletionTimestamp: &deletedAt,
			},
			wantErr: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := terminatingReleaseHeldError(held, testCase.after, workspace, tenant)
			if testCase.wantErr && err == nil {
				t.Fatal("invariant check accepted a claim Release must not leave behind")
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("invariant check rejected controller-owned drift: %v", err)
			}
		})
	}
}
