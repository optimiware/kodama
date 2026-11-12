//go:build component

package kubernetesconformance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

// TestCreateClaimRetain proves the seam-external claim provisioning primitive
// composes with the real adapter on a real kind cluster: the driver
// creates exactly the workspace PVC the adapter's Retain/Status accept, a
// repeated create adopts it without rewriting the retention marker, and a
// foreign pre-existing claim at the same name is refused and left untouched.
//
// The kind StorageClass is WaitForFirstConsumer, so the claim legitimately
// stays Pending until a Job mounts it. This test never waits for Bound; it
// proves metadata-level ownership and retention, which is what the adapter
// consumes, and releases the claim through the adapter's eventually-consistent
// Release path.
func TestCreateClaimRetain(t *testing.T) {
	t.Parallel()

	workspaceID := id.WorkspaceID("workspace-kube-14")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	spec := driverkube.ClaimSpec{
		Namespace:    testNamespace,
		TenantID:     testTenant(),
		WorkspaceID:  workspaceID,
		StorageClass: kubesupport.StorageClassName,
		Size:         "64Mi",
		AccessMode:   "ReadWriteOnce",
	}

	// The driver creates the claim the adapter can never create.
	claim, err := fx.Client.CreateClaim(ctx, spec)
	if err != nil {
		t.Fatalf("CreateClaim: %v", err)
	}
	if claim.Name != claimName || claim.Namespace != testNamespace {
		t.Fatalf("created claim = %s/%s; want %s/%s", claim.Namespace, claim.Name, testNamespace, claimName)
	}
	if claim.Labels[runtimekube.LabelTenant] != string(testTenant()) ||
		claim.Labels[runtimekube.LabelWorkspace] != string(workspaceID) {
		t.Fatalf("created claim labels = %v; want the adapter's ownership labels", claim.Labels)
	}
	if claim.Annotations[runtimekube.AnnotationRetained] != "" ||
		claim.Labels[runtimekube.LabelRetained] != "" {
		t.Fatalf("created claim %+v already carries a retention marker; Retain owns it", claim)
	}

	// The real adapter accepts the provisioned claim: Retain succeeds and Status
	// reports the workspace available.
	if err := fx.Adapter.Retain(ctx, workspaceID); err != nil {
		t.Fatalf("Retain on a driver-created claim: %v", err)
	}
	status, err := fx.Adapter.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Available {
		t.Fatalf("workspace status = %+v; want available after Retain", status)
	}
	retained, err := fx.Client.GetClaim(ctx, testNamespace, claimName)
	if err != nil {
		t.Fatalf("GetClaim after Retain: %v", err)
	}
	marked := retained.Annotations[runtimekube.AnnotationRetained] == "true" ||
		retained.Labels[runtimekube.LabelRetained] == "true"
	if !marked {
		t.Fatalf("retained claim %+v lost its retention marker", retained)
	}

	// The real API server holds exactly the requested storage spec. It is
	// Pending under WaitForFirstConsumer; that is expected and not asserted
	// against Bound.
	document, err := fx.Cluster.GetJSON(ctx, "get", "persistentvolumeclaim", claimName, "-n", testNamespace)
	if err != nil {
		t.Fatalf("kubectl get pvc: %v", err)
	}
	assertClaimStorageSpec(t, document, spec)

	// A second create is idempotent: it adopts the retained claim and never
	// rewrites the product-owned retention marker or ownership labels. The
	// resourceVersion is deliberately not compared: the PVC protection
	// controller adds and removes its finalizer asynchronously, which changes
	// the resourceVersion without the driver touching the claim.
	again, err := fx.Client.CreateClaim(ctx, spec)
	if err != nil {
		t.Fatalf("idempotent CreateClaim: %v", err)
	}
	if again.Name != claimName || again.Namespace != testNamespace {
		t.Fatalf("adopted claim = %s/%s; want %s/%s", again.Namespace, again.Name, testNamespace, claimName)
	}
	if again.Annotations[runtimekube.AnnotationRetained] != "true" {
		t.Fatalf("adopted claim %+v lost the retention annotation", again)
	}
	if again.Labels[runtimekube.LabelRetained] != "true" {
		t.Fatalf("adopted claim %+v lost the retention label", again)
	}
	if again.Labels[runtimekube.LabelTenant] != string(testTenant()) ||
		again.Labels[runtimekube.LabelWorkspace] != string(workspaceID) {
		t.Fatalf("adopted claim labels = %v; want the driver's ownership labels", again.Labels)
	}

	// A foreign claim that already holds the deterministic name for another
	// workspace is refused Forbidden and is never overwritten or adopted.
	foreignWorkspace := id.WorkspaceID("workspace-kube-15")
	foreignName, err := runtimekube.WorkspaceClaimName(foreignWorkspace)
	if err != nil {
		t.Fatalf("foreign WorkspaceClaimName: %v", err)
	}
	if _, err := fx.Cluster.Apply(ctx, foreignClaimYAML(foreignName, foreignWorkspace)); err != nil {
		t.Fatalf("create foreign claim: %v", err)
	}
	if _, err := waitForClaim(ctx, fx.Client, foreignName); err != nil {
		t.Fatalf("foreign claim never appeared: %v", err)
	}
	_, err = fx.Client.CreateClaim(ctx, driverkube.ClaimSpec{
		Namespace:    testNamespace,
		TenantID:     testTenant(),
		WorkspaceID:  foreignWorkspace,
		StorageClass: kubesupport.StorageClassName,
		Size:         "64Mi",
		AccessMode:   "ReadWriteOnce",
	})
	requireFaultClass(t, err, fault.Forbidden)
	if errors.Is(err, runtimekube.ErrAlreadyExists) {
		t.Fatalf("foreign claim refusal %v leaked ErrAlreadyExists as the outcome", err)
	}
	// The refusal must not relabel, adopt or retain the foreign claim. The
	// resourceVersion is deliberately not compared: the PVC protection
	// controller may add its finalizer between the two reads asynchronously.
	foreignAfter, err := fx.Client.GetClaim(ctx, testNamespace, foreignName)
	if err != nil {
		t.Fatalf("GetClaim after refused create: %v", err)
	}
	if foreignAfter.Labels[runtimekube.LabelTenant] != "tenant-other" ||
		foreignAfter.Labels[runtimekube.LabelWorkspace] != string(foreignWorkspace) {
		t.Fatalf("foreign claim labels = %v; the refused create must not relabel it", foreignAfter.Labels)
	}
	if foreignAfter.Annotations[runtimekube.AnnotationRetained] != "" ||
		foreignAfter.Labels[runtimekube.LabelRetained] != "" {
		t.Fatalf("refused create retained the foreign claim: %+v", foreignAfter)
	}

	// Cleanup: the adapter releases the provisioned workspace through its
	// eventually-consistent Release, and the foreign claim is deleted directly.
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("released claim is still present: %v", err)
	}
	if _, err := fx.Cluster.Kubectl(ctx, "delete", "persistentvolumeclaim", foreignName,
		"-n", testNamespace, "--ignore-not-found"); err != nil {
		t.Fatalf("delete foreign claim: %v", err)
	}
	if err := waitForClaimGone(ctx, fx.Client, foreignName); err != nil {
		t.Fatalf("foreign claim is still present: %v", err)
	}
}

// foreignClaimYAML builds a same-named claim owned by another tenant, as an
// operator would have created it with kubectl. Only the tenant label differs
// from the driver's ownership labels, so the conflict must be refused.
func foreignClaimYAML(name string, workspaceID id.WorkspaceID) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  labels:
    %s: tenant-other
    %s: %s
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: %s
  resources:
    requests:
      storage: 64Mi
`, name, testNamespace, runtimekube.LabelTenant, runtimekube.LabelWorkspace, workspaceID, kubesupport.StorageClassName)
}

// assertClaimStorageSpec proves the API server persisted exactly the requested
// storage specification.
func assertClaimStorageSpec(t *testing.T, document map[string]any, spec driverkube.ClaimSpec) {
	t.Helper()
	claimSpec, ok := document["spec"].(map[string]any)
	if !ok {
		t.Fatalf("persistent volume claim has no spec: %v", document)
	}
	if got := claimSpec["storageClassName"]; got != spec.StorageClass {
		t.Fatalf("storageClassName = %v; want %q", got, spec.StorageClass)
	}
	if got := claimSpec["volumeMode"]; got != "Filesystem" {
		t.Fatalf("volumeMode = %v; want Filesystem", got)
	}
	modes, ok := claimSpec["accessModes"].([]any)
	if !ok || len(modes) != 1 || modes[0] != spec.AccessMode {
		t.Fatalf("accessModes = %v; want [%s]", claimSpec["accessModes"], spec.AccessMode)
	}
	resources, ok := claimSpec["resources"].(map[string]any)
	if !ok {
		t.Fatalf("claim spec has no resources: %v", claimSpec)
	}
	requests, ok := resources["requests"].(map[string]any)
	if !ok {
		t.Fatalf("claim resources have no requests: %v", resources)
	}
	if got := requests["storage"]; got != spec.Size {
		t.Fatalf("storage request = %v; want %q", got, spec.Size)
	}
}
