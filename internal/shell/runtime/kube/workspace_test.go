// Retained-workspace behavior for the Kubernetes adapter.
//
// These tests drive the declared engine.Workspace port over the same
// deterministic Cluster seam as the workload tests. Availability never
// recreates a claim, retention is durable, release is conditional on the
// absence of every Job/Pod reference, an uncertain deletion never reports
// success, and an accepted deletion that is still in progress is reported as
// released because Kubernetes completes it asynchronously.
package kube

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %q (present=%v) for %v; want %q", class, ok, err, want)
	}
}

func workspacePod(workspaceID id.WorkspaceID, pod Pod) Pod {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelWorkspace] = string(workspaceID)
	return pod
}

func workspaceJob(workspaceID id.WorkspaceID) Job {
	return Job{
		Name:      "other-job",
		Namespace: testNamespace,
		Labels: map[string]string{
			LabelTenant:    string(testTenant()),
			LabelWorkspace: string(workspaceID),
		},
	}
}

func workspaceClaim(t *testing.T, fake *fakeCluster, workspaceID id.WorkspaceID) Claim {
	t.Helper()
	claim, ok := fake.claimSnapshot(testNamespace, testClaimName(t, workspaceID))
	if !ok {
		t.Fatal("workspace claim is missing")
	}
	return claim
}

// TestWorkspaceStatusRequiresOwnedClaim proves Status reports availability only
// for an owned claim in the adapter's own tenant namespace, and that it never
// recreates or adopts a missing, foreign or unlabelled claim.
func TestWorkspaceStatusRequiresOwnedClaim(t *testing.T) {
	t.Run("owned claim is available", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if !observation.Available {
			t.Fatalf("observation = %+v; want an available retained workspace", observation)
		}
		if observation.WorkspaceID != workspaceID {
			t.Fatalf("observation workspace = %q; want %q", observation.WorkspaceID, workspaceID)
		}
		if observation.Detail == "" {
			t.Fatal("available observation has no detail")
		}
		if fake.callCount("UpdateClaim") != 0 || fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Status mutated the claim")
		}
	})

	t.Run("missing claim is unavailable and never recreated", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.removeClaim(testNamespace, claimName)

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error for a missing claim: %v", err)
		}
		if observation.Available {
			t.Fatal("missing claim was reported available")
		}
		if !strings.Contains(strings.ToLower(observation.Detail), "missing") {
			t.Fatalf("missing detail = %q; want an explicit missing explanation", observation.Detail)
		}
		if fake.claimExists(testNamespace, claimName) {
			t.Fatal("Status recreated a missing claim")
		}
		if fake.callCount("UpdateClaim") != 0 || fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Status attempted to create or delete a claim")
		}
	})

	t.Run("foreign tenant claim never counts", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Labels[LabelTenant] = "tenant-other"
		})

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error for a foreign claim: %v", err)
		}
		if observation.Available {
			t.Fatal("foreign tenant claim was reported available")
		}
		if observation.Detail == "" {
			t.Fatal("foreign claim detail is empty")
		}
	})

	t.Run("foreign workspace claim never counts", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Labels[LabelWorkspace] = "workspace-other"
		})

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error for a foreign claim: %v", err)
		}
		if observation.Available {
			t.Fatal("claim bound to another workspace was reported available")
		}
	})

	t.Run("unlabelled claim never counts", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Labels = nil
		})

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error for an unlabelled claim: %v", err)
		}
		if observation.Available {
			t.Fatal("unlabelled claim was reported available")
		}
	})

	t.Run("terminating claim is unavailable", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
		})

		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if observation.Available {
			t.Fatal("terminating claim was reported available")
		}
		if !strings.Contains(strings.ToLower(observation.Detail), "terminat") {
			t.Fatalf("terminating detail = %q; want a terminating explanation", observation.Detail)
		}
	})

	t.Run("foreign tenant namespace is refused", func(t *testing.T) {
		fake := newFakeCluster()
		fake.addNamespace(Namespace{Name: testNamespace, Labels: map[string]string{LabelTenant: "tenant-other"}})
		addTestClaim(t, fake, testWorkspaceID())
		adapter := newTestAdapter(t, fake, newFakeClock())

		_, err := adapter.Status(context.Background(), testWorkspaceID())
		requireFaultClass(t, err, fault.Forbidden)
		if fake.callCount("UpdateClaim") != 0 || fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Status touched a foreign tenant namespace")
		}
	})

	t.Run("claim in another namespace is untouched", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		const otherNamespace = "tenant-other"
		fake.addNamespace(Namespace{Name: otherNamespace, Labels: map[string]string{LabelTenant: "tenant-other"}})
		otherClaimName := testClaimName(t, testWorkspaceID())
		fake.addClaim(Claim{
			Name:      otherClaimName,
			Namespace: otherNamespace,
			Labels:    map[string]string{LabelTenant: "tenant-other", LabelWorkspace: string(testWorkspaceID())},
		})
		fake.removeClaim(testNamespace, otherClaimName)

		observation, err := adapter.Status(context.Background(), testWorkspaceID())
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if observation.Available {
			t.Fatal("cross-tenant claim was reported available")
		}
		if !fake.claimExists(otherNamespace, otherClaimName) {
			t.Fatal("Status touched a claim in another tenant namespace")
		}
	})
}

// TestWorkspaceRetainIsIdempotentAndDurable proves retention survives adapter
// reconstruction and repeated Retain calls without recreating a missing claim.
func TestWorkspaceRetainIsIdempotentAndDurable(t *testing.T) {
	fake, clock, adapter := testSetup(t)
	workspaceID := testWorkspaceID()
	claimName := testClaimName(t, workspaceID)

	if err := adapter.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	claim := workspaceClaim(t, fake, workspaceID)
	if claim.Annotations[AnnotationRetained] != "true" {
		t.Fatalf("retained annotation = %q; want true", claim.Annotations[AnnotationRetained])
	}
	if claim.Labels[LabelRetained] != "true" {
		t.Fatalf("retained label = %q; want true", claim.Labels[LabelRetained])
	}
	if updates := fake.callCount("UpdateClaim"); updates != 1 {
		t.Fatalf("UpdateClaim calls = %d; want exactly one durable update", updates)
	}

	if err := adapter.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("repeated Retain returned error: %v", err)
	}
	if updates := fake.callCount("UpdateClaim"); updates != 1 {
		t.Fatalf("repeated Retain wrote again; UpdateClaim calls = %d", updates)
	}

	reconstructed := newTestAdapter(t, fake, clock)
	observation, err := reconstructed.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("reconstructed Status returned error: %v", err)
	}
	if !observation.Available {
		t.Fatalf("reconstructed observation = %+v; want the retained workspace available", observation)
	}
	if err := reconstructed.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("reconstructed Retain returned error: %v", err)
	}
	if updates := fake.callCount("UpdateClaim"); updates != 1 {
		t.Fatalf("reconstructed Retain wrote again; UpdateClaim calls = %d", updates)
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("retention disappeared")
	}
}

func TestWorkspaceRetainRefusesMissingAndForeignClaims(t *testing.T) {
	t.Run("missing claim is never recreated", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.removeClaim(testNamespace, claimName)

		err := adapter.Retain(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.NotFound)
		if fake.claimExists(testNamespace, claimName) {
			t.Fatal("Retain recreated a missing workspace claim")
		}
		if fake.callCount("UpdateClaim") != 0 {
			t.Fatal("Retain wrote a missing claim")
		}
	})

	t.Run("foreign claim is refused", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Labels[LabelTenant] = "tenant-other"
		})

		err := adapter.Retain(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		claim := workspaceClaim(t, fake, workspaceID)
		if claim.Annotations[AnnotationRetained] != "" {
			t.Fatal("Retain adopted a foreign claim")
		}
		if fake.callCount("UpdateClaim") != 0 {
			t.Fatal("Retain wrote a foreign claim")
		}
	})

	t.Run("cancelled context has no effect", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := adapter.Retain(ctx, testWorkspaceID())
		if err == nil {
			t.Fatal("Retain succeeded with a cancelled context")
		}
		if fake.callCount("UpdateClaim") != 0 {
			t.Fatal("cancelled Retain reached the API")
		}
	})
}

// TestWorkspaceRetainSurvivesLostResponse proves an uncertain retention write is
// observable and a retry converges on the persisted marker.
func TestWorkspaceRetainSurvivesLostResponse(t *testing.T) {
	fake, _, adapter := testSetup(t)
	workspaceID := testWorkspaceID()
	fake.injectAfter("UpdateClaim", errors.New("connection reset after write"))

	err := adapter.Retain(context.Background(), workspaceID)
	requireFaultClass(t, err, fault.UncertainExternalEffect)
	claim := workspaceClaim(t, fake, workspaceID)
	if claim.Annotations[AnnotationRetained] != "true" {
		t.Fatal("lost retention response did not persist the durable marker")
	}

	fake.clearInjection("UpdateClaim")
	if err := adapter.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain retry returned error: %v", err)
	}
	if updates := fake.callCount("UpdateClaim"); updates != 1 {
		t.Fatalf("retry wrote again; UpdateClaim calls = %d", updates)
	}
}

// TestWorkspaceReleaseRequiresNoWorkloadReference proves release refuses while
// any Job or a live/uncertain Pod still references the claim.
func TestWorkspaceReleaseRequiresNoWorkloadReference(t *testing.T) {
	t.Run("releases an unreferenced claim", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()

		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release returned error: %v", err)
		}
		if fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release left an unreferenced claim behind")
		}
		if fake.callCount("DeleteClaim") != 1 {
			t.Fatalf("DeleteClaim calls = %d; want 1", fake.callCount("DeleteClaim"))
		}
	})

	t.Run("clears retention before deleting", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		if err := adapter.Retain(context.Background(), workspaceID); err != nil {
			t.Fatalf("Retain returned error: %v", err)
		}

		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release returned error: %v", err)
		}
		if fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release left a retained claim behind after clearing retention")
		}
		if updates := fake.callCount("UpdateClaim"); updates != 2 {
			t.Fatalf("UpdateClaim calls = %d; want one Retain and one release clear", updates)
		}
	})

	t.Run("refuses while a Job references the workspace", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.addJob(workspaceJob(workspaceID))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release deleted a claim referenced by a Job")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release attempted to delete a referenced claim")
		}
	})

	t.Run("refuses while a running pod references the workspace", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.addPod(workspacePod(workspaceID, testRunningPod(testIdentity())))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release deleted a claim referenced by a running pod")
		}
	})

	t.Run("refuses while a terminating pod is uncertain", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.addPod(workspacePod(workspaceID, testTerminatingPod(testIdentity())))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release deleted a claim while a pod might still write")
		}
	})

	t.Run("allows release after affirmatively terminated pod", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.addPod(workspacePod(workspaceID, testTerminatedPod(testIdentity(), "Succeeded", 0)))

		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release returned error: %v", err)
		}
		if fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("Release left the claim behind after confirmed pod termination")
		}
	})

	t.Run("accepts an owned terminating claim as released in progress", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
		})

		// The claim is owned, unreferenced and unretained, and its deletion is
		// already running: a PVC keeps the kubernetes.io/pvc-protection
		// finalizer until the API server finishes the volume teardown. That is
		// an accepted deletion in progress, not a failed cleanup attempt:
		// Release returns nil, so the Kernel cleanup loop completes instead of
		// spending one of its bounded attempts (internal/kernel/engine
		// removeOwned counts every non-nil Release error except NotFound).
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release of an owned terminating claim returned error: %v", err)
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatalf("DeleteClaim calls = %d; want no redundant delete for a claim already terminating", fake.callCount("DeleteClaim"))
		}
		claim, ok := fake.claimSnapshot(testNamespace, claimName)
		if !ok {
			t.Fatal("terminating claim disappeared")
		}
		if claim.DeletionTimestamp == nil {
			t.Fatal("terminating claim lost its deletion timestamp")
		}
		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if observation.Available {
			t.Fatal("terminating claim was reported available after an accepted release")
		}
	})

	t.Run("refuses a terminating claim referenced by a Job", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
		})
		fake.addJob(workspaceJob(workspaceID))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("referenced terminating claim disappeared")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release issued a delete for a referenced terminating claim")
		}
	})

	t.Run("refuses a terminating claim referenced by an uncertain pod", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
		})
		fake.addPod(workspacePod(workspaceID, testTerminatingPod(testIdentity())))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("terminating claim referenced by an uncertain pod disappeared")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release deleted a terminating claim while a pod might still write")
		}
	})

	t.Run("clears retention on a terminating claim and accepts it in progress", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
			claim.Annotations = map[string]string{AnnotationRetained: retainedValue}
			claim.Labels[LabelRetained] = retainedValue
		})

		// An external deletion of a retained claim is a lost workspace. Its
		// deletion is already running and cannot be reversed, so Release clears
		// the durable retention marker with the conditional metadata update the
		// live path uses and accepts the in-progress deletion. It still never
		// reports success while the marker is present, and it issues no
		// redundant delete.
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release of a retained terminating claim returned error: %v", err)
		}
		if fake.callCount("UpdateClaim") != 1 {
			t.Fatalf("UpdateClaim calls = %d; want one conditional retention clear", fake.callCount("UpdateClaim"))
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatalf("Release issued a delete for a retained terminating claim")
		}
		claim, ok := fake.claimSnapshot(testNamespace, claimName)
		if !ok {
			t.Fatal("retained terminating claim disappeared")
		}
		if claimRetained(claim) {
			t.Fatalf("retained terminating claim kept its marker: labels=%v annotations=%v",
				claim.Labels, claim.Annotations)
		}
		if claim.DeletionTimestamp == nil {
			t.Fatal("terminating claim lost its deletion timestamp")
		}
		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if observation.Available {
			t.Fatal("terminating claim was reported available after an accepted release")
		}
	})

	t.Run("refuses a foreign terminating claim", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			deleted := testDeadline
			claim.DeletionTimestamp = &deleted
			claim.Labels[LabelTenant] = "tenant-other"
		})

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("foreign terminating claim disappeared")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release issued a delete for a foreign terminating claim")
		}
	})

	t.Run("releases a claim carrying the pvc-protection finalizer", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
		})
		before := workspaceClaim(t, fake, workspaceID)

		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release of a finalizer-protected claim returned error: %v", err)
		}
		if fake.claimExists(testNamespace, claimName) {
			t.Fatal("Release left the finalizer-protected claim behind")
		}
		if fake.callCount("DeleteClaim") != 1 {
			t.Fatalf("DeleteClaim calls = %d; want 1", fake.callCount("DeleteClaim"))
		}
		if version := fake.lastClaimDeleteVersion(); version != before.ResourceVersion {
			t.Fatalf("conditional delete version = %q; want %q", version, before.ResourceVersion)
		}
	})

	t.Run("refuses a foreign claim", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
			claim.Labels[LabelTenant] = "tenant-other"
		})

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.Forbidden)
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("Release deleted a foreign claim")
		}
	})

	t.Run("is idempotent when already absent", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.removeClaim(testNamespace, testClaimName(t, workspaceID))

		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release of an absent claim returned error: %v", err)
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release issued a delete for an absent claim")
		}
	})
}

// TestWorkspaceKernelOrderReleasesFinalizedClaim proves the declared Kernel
// order Retain -> Prepare -> Start -> Stop -> Remove -> Release completes for a
// claim carrying the real pvc-protection finalizer: Remove never deletes the
// claim, Release performs the conditional delete, and the workspace ends
// absent.
func TestWorkspaceKernelOrderReleasesFinalizedClaim(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	claimName := testClaimName(t, request.WorkspaceID)
	fake.mutateClaim(testNamespace, claimName, func(claim *Claim) {
		claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	})

	if err := adapter.Retain(context.Background(), request.WorkspaceID); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	fake.addPod(testTerminatedPod(request.Identity, "Succeeded", 0))

	evidence, err := adapter.Stop(context.Background(), engine.StopRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed termination", evidence)
	}
	// TTL cleanup removes the finished Pod; Remove then drops the remaining
	// owned resources without touching the claim.
	fake.setPods(nil)

	removal, err := adapter.Remove(context.Background(), engine.RemoveRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the owned resources removed", removal)
	}
	if fake.callCount("DeleteClaim") != 0 {
		t.Fatal("Remove attempted to delete the workspace claim")
	}
	if !fake.claimExists(testNamespace, claimName) {
		t.Fatal("Remove deleted the workspace claim before Release")
	}

	if err := adapter.Release(context.Background(), request.WorkspaceID); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	if fake.claimExists(testNamespace, claimName) {
		t.Fatal("Release left the finalizer-protected claim behind")
	}
	if fake.jobCount() != 0 {
		t.Fatalf("jobs = %d; want none left", fake.jobCount())
	}
	if pods := fake.podsSnapshot(); len(pods) != 0 {
		t.Fatalf("pods = %d; want none left", len(pods))
	}
	if fake.callCount("DeleteClaim") != 1 {
		t.Fatalf("DeleteClaim calls = %d; want exactly Release's delete", fake.callCount("DeleteClaim"))
	}
}

// TestWorkspaceReleaseSurfacesUncertainDeletion proves a failed or lost delete
// is reported as an error with observable state, never as success.
func TestWorkspaceReleaseSurfacesUncertainDeletion(t *testing.T) {
	t.Run("failed delete keeps the claim", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		fake.injectBefore("DeleteClaim", errors.New("volume still attached"))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.UncertainExternalEffect)
		if !fake.claimExists(testNamespace, claimName) {
			t.Fatal("failed delete removed the claim")
		}

		fake.clearInjection("DeleteClaim")
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release retry returned error: %v", err)
		}
		if fake.claimExists(testNamespace, claimName) {
			t.Fatal("Release retry left the claim behind")
		}
	})

	t.Run("lost delete response is not success", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		fake.injectAfter("DeleteClaim", errors.New("connection reset after delete"))

		err := adapter.Release(context.Background(), workspaceID)
		requireFaultClass(t, err, fault.UncertainExternalEffect)
		if fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
			t.Fatal("fake lost-response injection did not apply the delete")
		}
		observation, statusErr := adapter.Status(context.Background(), workspaceID)
		if statusErr != nil {
			t.Fatalf("Status after lost response returned error: %v", statusErr)
		}
		if observation.Available {
			t.Fatal("lost delete response reported an available workspace")
		}

		fake.clearInjection("DeleteClaim")
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release retry returned error: %v", err)
		}
	})

	t.Run("finalizer-held deletion is accepted in progress", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		workspaceID := testWorkspaceID()
		claimName := testClaimName(t, workspaceID)
		before := workspaceClaim(t, fake, workspaceID)
		fake.setKeepDeletedClaims(true)

		// Our own versioned delete is accepted, but the finalizer keeps the
		// claim present with a deletion timestamp. That is the same accepted
		// deletion in progress: Release returns nil and spends no cleanup
		// attempt, and the claim stays observable until Kubernetes removes it.
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release of a finalizer-held claim returned error: %v", err)
		}
		if fake.callCount("DeleteClaim") != 1 {
			t.Fatalf("DeleteClaim calls = %d; want exactly one versioned delete", fake.callCount("DeleteClaim"))
		}
		if version := fake.lastClaimDeleteVersion(); version != before.ResourceVersion {
			t.Fatalf("conditional delete version = %q; want %q", version, before.ResourceVersion)
		}
		claim, ok := fake.claimSnapshot(testNamespace, claimName)
		if !ok {
			t.Fatal("finalizer-held claim disappeared")
		}
		if claim.DeletionTimestamp == nil {
			t.Fatal("finalizer-held claim is present without a deletion timestamp")
		}
		observation, err := adapter.Status(context.Background(), workspaceID)
		if err != nil {
			t.Fatalf("Status returned error: %v", err)
		}
		if observation.Available {
			t.Fatal("terminating claim was reported available after an accepted release")
		}

		// A later observation confirms the same in-progress state without a
		// second delete, and absence still releases idempotently.
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("repeated Release during termination returned error: %v", err)
		}
		if fake.callCount("DeleteClaim") != 1 {
			t.Fatalf("repeated Release issued another delete; DeleteClaim calls = %d", fake.callCount("DeleteClaim"))
		}
		fake.removeClaim(testNamespace, claimName)
		if err := adapter.Release(context.Background(), workspaceID); err != nil {
			t.Fatalf("Release after the claim disappeared returned error: %v", err)
		}
	})

	t.Run("reference listing failure keeps the claim", func(t *testing.T) {
		for _, operation := range []string{"ListJobs", "ListPods"} {
			t.Run(operation, func(t *testing.T) {
				fake, _, adapter := testSetup(t)
				workspaceID := testWorkspaceID()
				fake.injectBefore(operation, errors.New("api server unreachable"))

				err := adapter.Release(context.Background(), workspaceID)
				requireFaultClass(t, err, fault.DependencyUnavailable)
				if !fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
					t.Fatal("Release deleted the claim without reference evidence")
				}
				if fake.callCount("DeleteClaim") != 0 {
					t.Fatal("Release deleted the claim without reference evidence")
				}
			})
		}
	})
}

// TestWorkspaceStaleReleaseCannotDropRetainedClaim proves a Release that read
// the claim before another controller retained it is refused rather than
// deleting the retained workspace.
func TestWorkspaceStaleReleaseCannotDropRetainedClaim(t *testing.T) {
	fake, clock, releaser := testSetup(t)
	workspaceID := testWorkspaceID()
	retainer := newTestAdapter(t, fake, clock)
	gate := fake.newGate("ListPods")

	result := make(chan error, 1)
	go func() {
		result <- releaser.Release(context.Background(), workspaceID)
	}()

	select {
	case <-gate.entered:
	case <-context.Background().Done():
		t.Fatal("Release never reached the reference barrier")
	}

	if err := retainer.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("concurrent Retain returned error: %v", err)
	}
	close(gate.release)

	err := <-result
	requireFaultClass(t, err, fault.VersionConflict)
	if !fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
		t.Fatal("stale Release deleted a retained workspace")
	}
	claim := workspaceClaim(t, fake, workspaceID)
	if claim.Annotations[AnnotationRetained] != "true" {
		t.Fatal("stale Release cleared the durable retention marker")
	}
}

// TestWorkspaceConcurrentRetainIsIdempotent proves concurrent durable retains
// converge on one retained claim instead of conflicting failures.
func TestWorkspaceConcurrentRetainIsIdempotent(t *testing.T) {
	fake, clock, first := testSetup(t)
	second := newTestAdapter(t, fake, clock)
	workspaceID := testWorkspaceID()
	gate := fake.newGate("UpdateClaim")

	const workers = 2
	errs := make([]error, workers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for index, adapter := range []*Adapter{first, second} {
		wait.Add(1)
		go func(index int, adapter *Adapter) {
			defer wait.Done()
			<-start
			errs[index] = adapter.Retain(context.Background(), workspaceID)
		}(index, adapter)
	}
	close(start)
	select {
	case <-gate.entered:
	case <-context.Background().Done():
		t.Fatal("Retain never reached the update barrier")
	}
	close(gate.release)
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d Retain returned error: %v", index, err)
		}
	}
	claim := workspaceClaim(t, fake, workspaceID)
	if claim.Annotations[AnnotationRetained] != "true" {
		t.Fatal("concurrent Retain did not persist the durable marker")
	}
}

// TestWorkspaceReleaseSerializesWithStart proves a claim is never deleted while
// Start is creating the Job that mounts it.
func TestWorkspaceReleaseSerializesWithStart(t *testing.T) {
	t.Run("Start first keeps the claim", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)
		gate := fake.newGate("CreateJob")

		started := make(chan error, 1)
		go func() {
			_, err := adapter.Start(context.Background(), engine.StartRequest{
				Identity:    request.Identity,
				WorkspaceID: request.WorkspaceID,
			})
			started <- err
		}()
		select {
		case <-gate.entered:
		case <-context.Background().Done():
			t.Fatal("Start never reached the CreateJob barrier")
		}

		released := make(chan error, 1)
		go func() {
			released <- adapter.Release(context.Background(), request.WorkspaceID)
		}()
		close(gate.release)

		if err := <-started; err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
		if err := <-released; err == nil {
			t.Fatal("Release succeeded while a Job referenced the workspace")
		}
		if fake.jobCount() != 1 {
			t.Fatalf("jobs = %d; want the one started Job", fake.jobCount())
		}
		if !fake.claimExists(testNamespace, testClaimName(t, request.WorkspaceID)) {
			t.Fatal("Release deleted the claim under a live Job")
		}
		if fake.callCount("DeleteClaim") != 0 {
			t.Fatal("Release issued a delete while a Job existed")
		}
	})

	t.Run("Release first makes Start fail closed", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)
		gate := fake.newGate("ListJobs")

		released := make(chan error, 1)
		go func() {
			released <- adapter.Release(context.Background(), request.WorkspaceID)
		}()
		select {
		case <-gate.entered:
		case <-context.Background().Done():
			t.Fatal("Release never reached the Job listing barrier")
		}

		started := make(chan error, 1)
		go func() {
			result, err := adapter.Start(context.Background(), engine.StartRequest{
				Identity:    request.Identity,
				WorkspaceID: request.WorkspaceID,
			})
			if err == nil && result.Outcome == engine.StartAccepted {
				err = errors.New("Start reported accepted without a claim")
			}
			started <- err
		}()
		close(gate.release)

		if err := <-released; err != nil {
			t.Fatalf("Release returned error: %v", err)
		}
		if err := <-started; err == nil {
			t.Fatal("Start succeeded after the retained workspace was released")
		}
		if fake.jobCount() != 0 {
			t.Fatalf("jobs = %d; Start created a Job without a claim", fake.jobCount())
		}
	})
}

// TestWorkspaceCancellationAtBarrier proves a cancelled Retain/Release never
// reaches the effect.
func TestWorkspaceCancellationAtBarrier(t *testing.T) {
	t.Run("Retain", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		gate := fake.newGate("UpdateClaim")
		ctx, cancel := context.WithCancel(context.Background())

		result := make(chan error, 1)
		go func() { result <- adapter.Retain(ctx, testWorkspaceID()) }()
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("Retain never reached the update barrier")
		}
		cancel()
		close(gate.release)

		if err := <-result; err == nil {
			t.Fatal("cancelled Retain hid the cancellation")
		}
		claim := workspaceClaim(t, fake, testWorkspaceID())
		if claim.Annotations[AnnotationRetained] != "" {
			t.Fatal("cancelled Retain applied the retention marker")
		}
	})

	t.Run("Release", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		gate := fake.newGate("DeleteClaim")
		ctx, cancel := context.WithCancel(context.Background())

		result := make(chan error, 1)
		go func() { result <- adapter.Release(ctx, testWorkspaceID()) }()
		select {
		case <-gate.entered:
		case <-ctx.Done():
			t.Fatal("Release never reached the delete barrier")
		}
		cancel()
		close(gate.release)

		if err := <-result; err == nil {
			t.Fatal("cancelled Release hid the cancellation")
		}
		if !fake.claimExists(testNamespace, testClaimName(t, testWorkspaceID())) {
			t.Fatal("cancelled Release deleted the claim")
		}
	})
}

// TestWorkspaceDetailsCarryNoSecrets proves workspace evidence never leaks
// configured secret values.
func TestWorkspaceDetailsCarryNoSecrets(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	fake.removeClaim(testNamespace, testClaimName(t, request.WorkspaceID))

	observation, err := adapter.Status(context.Background(), request.WorkspaceID)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if strings.Contains(observation.Detail, "s3cr3t") {
		t.Fatalf("Status detail leaked a secret: %q", observation.Detail)
	}
	if err := adapter.Retain(context.Background(), request.WorkspaceID); err == nil {
		t.Fatal("Retain of a missing claim succeeded")
	} else if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("Retain error leaked a secret: %v", err)
	}
}

// TestWorkspaceConcurrentRetainReleaseIsRaceFree exercises mixed retention and
// release schedules under the race detector. Every outcome is an explicit
// observation or a typed fault; no panic or fabricated availability occurs.
func TestWorkspaceConcurrentRetainReleaseIsRaceFree(t *testing.T) {
	fake, _, adapter := testSetup(t)
	workspaceID := testWorkspaceID()

	const workers = 8
	var wait sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, workers)
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			if index%2 == 0 {
				errs[index] = adapter.Retain(context.Background(), workspaceID)
				return
			}
			errs[index] = adapter.Release(context.Background(), workspaceID)
		}(index)
	}
	close(start)
	wait.Wait()

	for index, err := range errs {
		if err == nil {
			continue
		}
		if _, ok := fault.ClassOf(err); !ok {
			t.Fatalf("worker %d returned an untyped error: %v", index, err)
		}
	}

	observation, err := adapter.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if observation.Available {
		claim := workspaceClaim(t, fake, workspaceID)
		if claim.Annotations[AnnotationRetained] != "true" {
			t.Fatal("available workspace has no durable retention marker")
		}
	} else if fake.claimExists(testNamespace, testClaimName(t, workspaceID)) {
		claim := workspaceClaim(t, fake, workspaceID)
		if claim.DeletionTimestamp == nil {
			t.Fatal("unavailable claim is neither retained nor terminating")
		}
	}
}
