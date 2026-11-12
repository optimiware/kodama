//go:build component

package kubernetesconformance

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// TestWorkspacePersistsAcrossTwoJobsAndRelease proves a retained workspace
// survives two separate Jobs (with the WaitForFirstConsumer StorageClass) and is
// deleted only by Release.
func TestWorkspacePersistsAcrossTwoJobsAndRelease(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-6")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}

	writer := testIdentity("exec-k6a", "action-k6a", "workload-k6a", "idem-k6a")
	startPrepared(t, ctx, fx, prepareRequest(
		writer, workspaceID, "build", "",
		testContainer([]string{`printf 'persisted\n' > /workspace/persist.txt`}, nil), nil, "tok-k6a",
	))
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: writer, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	removal, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: writer, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Remove writer: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("writer removal = %+v; want removed", removal)
	}
	if !strings.Contains(removal.Detail, claimName) {
		t.Fatalf("writer removal detail %q does not name the retained claim %q", removal.Detail, claimName)
	}
	waitForWorkspaceUnreferenced(t, ctx, fx.Client, workspaceID)

	content := runReader(t, ctx, fx, workspaceID, "k6b", []string{`cat /workspace/persist.txt`})
	if content != "persisted\n" {
		t.Fatalf("second Job read %q; want the first Job's persisted bytes", content)
	}
	reader := testIdentity("exec-k6b", "action-k6b", "workload-k6b", "idem-k6b")
	if _, err := fx.Adapter.Remove(ctx, engine.RemoveRequest{Identity: reader, WorkspaceID: workspaceID}); err != nil {
		t.Fatalf("Remove reader: %v", err)
	}
	waitForWorkspaceUnreferenced(t, ctx, fx.Client, workspaceID)

	status, err := fx.Adapter.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Available {
		t.Fatalf("workspace status = %+v; want available before release", status)
	}
	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("released claim is still present: %v", err)
	}
	status, err = fx.Adapter.Status(ctx, workspaceID)
	if err != nil {
		t.Fatalf("Status after release: %v", err)
	}
	if status.Available {
		t.Fatalf("released workspace is still available: %+v", status)
	}
	if _, err := fx.Client.GetClaim(ctx, testNamespace, claimName); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("released claim read = %v; want ErrNotFound", err)
	}
}

// TestClaimFinalizerPendingAndStaleDelete proves a finalizer-protected claim
// stays observable after an accepted delete and a stale conditional delete is
// refused instead of dropping it.
func TestClaimFinalizerPendingAndStaleDelete(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-7")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true, claimFinalizer: holdFinalizer})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	claim, err := fx.Client.GetClaim(ctx, testNamespace, claimName)
	if err != nil {
		t.Fatalf("GetClaim: %v", err)
	}
	holds := false
	for _, finalizer := range claim.Finalizers {
		if finalizer == holdFinalizer {
			holds = true
		}
	}
	if !holds {
		t.Fatalf("claim finalizers = %v; want the hold finalizer", claim.Finalizers)
	}
	staleVersion := claim.ResourceVersion

	if err := fx.Client.DeleteClaim(ctx, testNamespace, claimName); err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	terminating, err := waitForClaimTerminating(ctx, fx.Client, claimName)
	if err != nil {
		t.Fatalf("claim never showed a pending deletion: %v", err)
	}
	if terminating.DeletionTimestamp == nil {
		t.Fatalf("claim = %+v; want a deletion timestamp", terminating)
	}

	if err := fx.Client.DeleteClaimVersioned(ctx, testNamespace, claimName, staleVersion); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("stale conditional delete = %v; want ErrConflict", err)
	}
	if _, err := fx.Client.GetClaim(ctx, testNamespace, claimName); err != nil {
		t.Fatalf("stale delete dropped the terminating claim: %v", err)
	}

	if _, err := fx.Cluster.Kubectl(ctx, "patch", "persistentvolumeclaim", claimName, "-n", testNamespace,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`); err != nil {
		t.Fatalf("clear hold finalizer: %v", err)
	}
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("claim did not disappear after finalizer removal: %v", err)
	}
}

// deleteGatedCluster interposes on DeleteClaimVersioned so a concurrent Retain
// can execute while Release's versioned delete is in flight with a pre-retain
// ResourceVersion.
type deleteGatedCluster struct {
	runtimekube.Cluster
	deleteEntered chan struct{}
	deleteRelease chan struct{}
	once          sync.Once
	mu            sync.Mutex
	lastErr       error
}

func (c *deleteGatedCluster) DeleteClaimVersioned(ctx context.Context, namespace, name, resourceVersion string) error {
	c.once.Do(func() {
		close(c.deleteEntered)
		<-c.deleteRelease
	})
	err := c.Cluster.DeleteClaimVersioned(ctx, namespace, name, resourceVersion)
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
	return err
}

func (c *deleteGatedCluster) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// TestRetainBarrierAndStaleVersionedDelete proves a concurrent Retain that
// advances the resource version makes an in-flight stale conditional delete
// fail, so a retained workspace is never dropped.
func TestRetainBarrierAndStaleVersionedDelete(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-8")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	if _, err := fx.Client.GetClaim(ctx, testNamespace, claimName); err != nil {
		t.Fatalf("GetClaim: %v", err)
	}

	gated := &deleteGatedCluster{
		Cluster:       fx.Client,
		deleteEntered: make(chan struct{}),
		deleteRelease: make(chan struct{}),
	}
	releaseAdapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   gated,
		Namespace: testNamespace,
		TenantID:  testTenant(),
		KodamaURL: testKodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("build release adapter: %v", err)
	}

	released := make(chan error, 1)
	go func() {
		released <- releaseAdapter.Release(ctx, workspaceID)
	}()
	<-gated.deleteEntered

	retainedDone := make(chan error, 1)
	go func() {
		retainedDone <- fx.Adapter.Retain(ctx, workspaceID)
	}()
	if err := <-retainedDone; err != nil {
		close(gated.deleteRelease)
		t.Fatalf("Retain while DeleteClaimVersioned is in flight: %v", err)
	}
	close(gated.deleteRelease)

	releaseErr := <-released
	requireFaultClass(t, releaseErr, fault.VersionConflict)
	if !errors.Is(gated.err(), runtimekube.ErrConflict) {
		t.Fatalf("in-flight DeleteClaimVersioned = %v; want ErrConflict", gated.err())
	}

	retained, err := fx.Client.GetClaim(ctx, testNamespace, claimName)
	if err != nil {
		t.Fatalf("retained claim disappeared: %v", err)
	}
	marked := retained.Annotations[runtimekube.AnnotationRetained] == "true" ||
		retained.Labels[runtimekube.LabelRetained] == "true"
	if !marked {
		t.Fatalf("claim %+v lost its retention marker", retained)
	}
	// The marker's presence is the evidence that Retain wrote the claim. The
	// resourceVersion is deliberately not compared in either direction: the
	// real cluster's controllers update a claim asynchronously, so a controller
	// write can satisfy or break an equality that says nothing about Retain.

	// Retain is idempotent: a second call on an already-retained claim returns
	// nil and leaves the product-owned marker and ownership intact.
	if err := fx.Adapter.Retain(ctx, workspaceID); err != nil {
		t.Fatalf("second Retain: %v", err)
	}
	again, err := fx.Client.GetClaim(ctx, testNamespace, claimName)
	if err != nil {
		t.Fatalf("GetClaim after idempotent retain: %v", err)
	}
	markedAgain := again.Annotations[runtimekube.AnnotationRetained] == "true" ||
		again.Labels[runtimekube.LabelRetained] == "true"
	if !markedAgain {
		t.Fatalf("idempotent Retain cleared the retention marker: %+v", again)
	}
	if again.Labels[runtimekube.LabelWorkspace] != string(workspaceID) ||
		again.Labels[runtimekube.LabelTenant] != string(testTenant()) {
		t.Fatalf("idempotent Retain changed claim ownership: labels = %v", again.Labels)
	}

	releaseWorkspaceEventually(t, ctx, fx, workspaceID)
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("released claim is still present: %v", err)
	}

	// Also exercise an unserialized race between Retain and DeleteClaimVersioned
	// starting from the same pre-retain ResourceVersion: whichever linearizes
	// first, a claim reported retained must never be lost.
	raceWorkspaceID := id.WorkspaceID("workspace-kube-8-race")
	raceClaimName, err := runtimekube.WorkspaceClaimName(raceWorkspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName(race): %v", err)
	}
	if _, err := fx.Cluster.Apply(ctx, claimYAML(raceClaimName, raceWorkspaceID, "")); err != nil {
		t.Fatalf("create race workspace claim: %v", err)
	}
	if _, err := waitForClaim(ctx, fx.Client, raceClaimName); err != nil {
		t.Fatalf("race workspace claim never appeared: %v", err)
	}
	raceClaim, err := fx.Client.GetClaim(ctx, testNamespace, raceClaimName)
	if err != nil {
		t.Fatalf("GetClaim(race): %v", err)
	}

	start := make(chan struct{})
	retainErrCh := make(chan error, 1)
	deleteErrCh := make(chan error, 1)
	go func() {
		<-start
		retainErrCh <- fx.Adapter.Retain(ctx, raceWorkspaceID)
	}()
	go func() {
		<-start
		deleteErrCh <- fx.Client.DeleteClaimVersioned(ctx, testNamespace, raceClaimName, raceClaim.ResourceVersion)
	}()
	close(start)

	retainErr := <-retainErrCh
	deleteErr := <-deleteErrCh
	if retainErr == nil && deleteErr == nil {
		t.Fatal("both Retain and stale DeleteClaimVersioned succeeded; retained claim could be lost")
	}
	switch {
	case retainErr == nil:
		if !errors.Is(deleteErr, runtimekube.ErrConflict) {
			t.Fatalf("DeleteClaimVersioned = %v when Retain succeeded; want ErrConflict", deleteErr)
		}
		surviving, err := fx.Client.GetClaim(ctx, testNamespace, raceClaimName)
		if err != nil {
			t.Fatalf("retained claim disappeared in unserialized race: %v", err)
		}
		if surviving.DeletionTimestamp != nil {
			t.Fatalf("retained claim is terminating after unserialized race: %+v", surviving)
		}
		if surviving.Annotations[runtimekube.AnnotationRetained] != "true" &&
			surviving.Labels[runtimekube.LabelRetained] != "true" {
			t.Fatalf("retained claim lost its retention marker in unserialized race: %+v", surviving)
		}
		releaseWorkspaceEventually(t, ctx, fx, raceWorkspaceID)
		if err := waitForClaimGone(ctx, fx.Client, raceClaimName); err != nil {
			t.Fatalf("released race claim is still present: %v", err)
		}
	case deleteErr == nil:
		if err := waitForClaimGone(ctx, fx.Client, raceClaimName); err != nil {
			t.Fatalf("deleted race claim is still present: %v", err)
		}
	default:
		if !errors.Is(deleteErr, runtimekube.ErrConflict) {
			t.Fatalf("DeleteClaimVersioned = %v; want ErrConflict", deleteErr)
		}
		requireFaultClass(t, retainErr, fault.VersionConflict)
		surviving, err := fx.Client.GetClaim(ctx, testNamespace, raceClaimName)
		if err != nil || surviving.DeletionTimestamp != nil {
			t.Fatalf("claim was lost despite conflicting operations: claim=%+v err=%v", surviving, err)
		}
		releaseWorkspaceEventually(t, ctx, fx, raceWorkspaceID)
		if err := waitForClaimGone(ctx, fx.Client, raceClaimName); err != nil {
			t.Fatalf("released race claim is still present: %v", err)
		}
	}
}

// TestReleaseAcceptsTerminatingClaimAsInProgress pins the Release contract the
// Kernel cleanup loop relies on: an accepted versioned delete is not a confirmed
// removal on its own, but it is not a failed attempt either. While the claim
// carries a deletion timestamp (the real kubernetes.io/pvc-protection finalizer,
// held deterministically here by an explicit finalizer), Release reports the
// accepted deletion in progress as success so cleanup converges without
// spending an attempt; the claim itself stays observable until the API server
// completes the deletion. Once the claim is gone, Release converges to nil as
// before.
func TestReleaseAcceptsTerminatingClaimAsInProgress(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-13")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true, claimFinalizer: holdFinalizer})
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}

	// The first Release issues an accepted versioned delete. The claim stays
	// terminating under its finalizer, which is the accepted in-progress state,
	// not a cleanup failure.
	first := fx.Adapter.Release(ctx, workspaceID)
	if first != nil {
		t.Fatalf("first Release = %v; want nil for an accepted deletion in progress", first)
	}
	terminating, err := waitForClaimTerminating(ctx, fx.Client, claimName)
	if err != nil {
		t.Fatalf("claim never showed a pending deletion: %v", err)
	}
	if terminating.DeletionTimestamp == nil {
		t.Fatalf("claim = %+v; want a deletion timestamp", terminating)
	}
	if !slices.Contains(terminating.Finalizers, holdFinalizer) {
		t.Fatalf("claim finalizers = %v; want the test-owned hold finalizer %q", terminating.Finalizers, holdFinalizer)
	}

	// A Release while the claim is terminating is the same in-progress success:
	// it must not issue another delete, clear the state or report a failure.
	// The invariants are the ones the product owns: the claim is still present
	// with the same deletion timestamp, the test-owned hold finalizer is still
	// there, and ownership and the unretained marker are unchanged. Controller
	// finalizers, resourceVersion, generation, labels, annotations and status
	// are not compared: the real cluster's controllers own and change them
	// asynchronously (the PVC protection controller removing its finalizer is
	// exactly such drift).
	second := fx.Adapter.Release(ctx, workspaceID)
	if second != nil {
		t.Fatalf("Release during a terminating claim = %v; want nil for the accepted deletion in progress", second)
	}
	current, err := fx.Client.GetClaim(ctx, testNamespace, claimName)
	if err != nil {
		t.Fatalf("terminating claim disappeared during Release: %v", err)
	}
	if err := terminatingReleaseHeldError(terminating, current, workspaceID, testTenant()); err != nil {
		t.Fatal(err)
	}

	// Clearing the hold finalizer lets the API server remove the claim; Release
	// then observes ErrNotFound and confirms absence.
	if _, err := fx.Cluster.Kubectl(ctx, "patch", "persistentvolumeclaim", claimName, "-n", testNamespace,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`); err != nil {
		t.Fatalf("clear hold finalizer: %v", err)
	}
	if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
		t.Fatalf("claim did not disappear after finalizer removal: %v", err)
	}
	if err := fx.Adapter.Release(ctx, workspaceID); err != nil {
		t.Fatalf("Release after the claim was removed = %v; want nil", err)
	}
}
