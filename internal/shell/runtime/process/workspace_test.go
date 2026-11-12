// Workspace retention, availability and release schedules.
//
// The process adapter owns the engine.Workspace port over the same confined
// roots and injected Files seam as the workload port. These tests pin the
// declared Kernel rules: a retained workspace survives request waits and
// adapter reconstruction, a missing or symlinked workspace is never recreated
// or followed, release is refused while a live workload identity still
// references the workspace but never requires or is blocked by the retention
// marker, and a partial delete stays visible instead of becoming a fabricated
// success.
package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// TestAdapterImplementsEngineWorkspacePort is a compile-time proof that the
// Process adapter exposes the declared Kernel workspace port.
var _ engine.Workspace = (*Adapter)(nil)

func otherWorkspaceID() id.WorkspaceID {
	return "workspace-2"
}

func markerPath(fx *fixture, workspaceID id.WorkspaceID) string {
	return fx.adapter.retentionPath(workspaceID)
}

func assertMarker(t *testing.T, fx *fixture, workspaceID id.WorkspaceID, want bool) {
	t.Helper()
	_, err := os.Lstat(markerPath(fx, workspaceID))
	if want {
		if err != nil {
			t.Fatalf("retention marker for %q is absent: %v", workspaceID, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("retention marker for %q exists; want it absent", workspaceID)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("stat retention marker for %q: %v", workspaceID, err)
	}
}

func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %q (ok=%v), error = %v; want %q", class, ok, err, want)
	}
}

// TestWorkspaceStatusReportsPresentRealDirectory proves the present workspace
// is reported available without changing it.
func TestWorkspaceStatusReportsPresentRealDirectory(t *testing.T) {
	fx := newFixture(t)

	observation, err := fx.adapter.Status(context.Background(), testWorkspaceID())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if observation.WorkspaceID != testWorkspaceID() {
		t.Fatalf("Status workspace = %q; want %q", observation.WorkspaceID, testWorkspaceID())
	}
	if !observation.Available {
		t.Fatalf("Status = %+v; want available", observation)
	}
	if strings.TrimSpace(observation.Detail) == "" {
		t.Fatal("Status omitted its availability detail")
	}
	// Status never creates a marker; retention is Retain's decision alone.
	assertMarker(t, fx, testWorkspaceID(), false)
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("workspace sentinel = %q; want %q", got, workspaceSentinelData)
	}
}

// TestWorkspaceStatusReportsMissingWithoutRecreating proves a lost workspace
// is an unavailable observation, not permission to create scratch state.
func TestWorkspaceStatusReportsMissingWithoutRecreating(t *testing.T) {
	fx := newFixture(t)
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}

	observation, err := fx.adapter.Status(context.Background(), testWorkspaceID())
	if err != nil {
		t.Fatalf("Status for a missing workspace returned error: %v", err)
	}
	if observation.Available {
		t.Fatalf("Status = %+v; want unavailable", observation)
	}
	if !strings.Contains(observation.Detail, "missing") {
		t.Fatalf("Status detail = %q; want a missing-workspace explanation", observation.Detail)
	}
	assertNoPath(t, fx.workspaceDir)
}

// TestWorkspaceStatusRefusesSymlinkWithoutFollowing proves a symlinked
// workspace is refused and its target is never inspected or changed.
func TestWorkspaceStatusRefusesSymlinkWithoutFollowing(t *testing.T) {
	fx := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("creating outside directory: %v", err)
	}
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}
	if err := os.Symlink(outside, fx.workspaceDir); err != nil {
		t.Fatalf("creating workspace symlink: %v", err)
	}

	observation, err := fx.adapter.Status(context.Background(), testWorkspaceID())
	if err != nil {
		t.Fatalf("Status for a symlinked workspace returned error: %v", err)
	}
	if observation.Available {
		t.Fatalf("Status = %+v; want unavailable", observation)
	}
	if !strings.Contains(observation.Detail, "symlink") {
		t.Fatalf("Status detail = %q; want a symlink refusal", observation.Detail)
	}
	if _, err := os.Lstat(outside); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
}

// TestWorkspacePortRejectsInvalidIdentity proves every workspace operation
// refuses an unusable identity before any host call.
func TestWorkspacePortRejectsInvalidIdentity(t *testing.T) {
	fx := newFixture(t)
	invalid := id.WorkspaceID("../escape")
	ctx := context.Background()

	if _, err := fx.adapter.Status(ctx, invalid); err == nil {
		t.Fatal("Status accepted an invalid workspace identity")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if err := fx.adapter.Retain(ctx, invalid); err == nil {
		t.Fatal("Retain accepted an invalid workspace identity")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if err := fx.adapter.Release(ctx, invalid); err == nil {
		t.Fatal("Release accepted an invalid workspace identity")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	assertNoPath(t, filepath.Join(fx.root, "retention"))
}

// TestWorkspaceRetainIsIdempotentAndDurableAcrossReconstruction proves
// retention is recorded durably and a fresh adapter observes it.
func TestWorkspaceRetainIsIdempotentAndDurableAcrossReconstruction(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	assertMarker(t, fx, testWorkspaceID(), true)
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("second Retain returned error: %v", err)
	}
	assertMarker(t, fx, testWorkspaceID(), true)

	reconstructed := newTestAdapter(t, fx.config())
	observation, err := reconstructed.Status(ctx, testWorkspaceID())
	if err != nil {
		t.Fatalf("reconstructed Status returned error: %v", err)
	}
	if !observation.Available {
		t.Fatalf("reconstructed Status = %+v; want available", observation)
	}
	if err := reconstructed.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("reconstructed Retain returned error: %v", err)
	}
	assertMarker(t, fx, testWorkspaceID(), true)
	if got := readHostFile(t, filepath.Join(fx.workspaceDir, workspaceSentinel)); got != workspaceSentinelData {
		t.Fatalf("retention changed workspace bytes: %q", got)
	}

	// The durable marker is load-bearing for a reconstructed adapter: release
	// still finds the retained workspace and removes it.
	if err := reconstructed.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("reconstructed Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceRetainNeverRecreatesMissingWorkspace proves Retain reports a
// lost workspace and does not create it or a marker.
func TestWorkspaceRetainNeverRecreatesMissingWorkspace(t *testing.T) {
	fx := newFixture(t)
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}

	err := fx.adapter.Retain(context.Background(), testWorkspaceID())
	assertFaultClass(t, err, fault.NotFound)
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceRetainRefusesSymlinkedWorkspace proves Retain never follows a
// symlinked workspace or writes through it.
func TestWorkspaceRetainRefusesSymlinkedWorkspace(t *testing.T) {
	fx := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("creating outside directory: %v", err)
	}
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}
	if err := os.Symlink(outside, fx.workspaceDir); err != nil {
		t.Fatalf("creating workspace symlink: %v", err)
	}

	err := fx.adapter.Retain(context.Background(), testWorkspaceID())
	assertFaultClass(t, err, fault.NotFound)
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("reading outside directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("Retain wrote through the symlink: %v", entries)
	}
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceRetentionMarkerRefusesSymlinkedDirectory proves the durable
// marker can never be written outside the adapter root.
func TestWorkspaceRetentionMarkerRefusesSymlinkedDirectory(t *testing.T) {
	fx := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("creating outside directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(fx.root, "retention")); err != nil {
		t.Fatalf("creating retention symlink: %v", err)
	}

	err := fx.adapter.Retain(context.Background(), testWorkspaceID())
	assertFaultClass(t, err, fault.Forbidden)
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("reading outside directory: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("Retain escaped the adapter root: %v", entries)
	}
}

// TestWorkspaceReleaseRefusedWhileLiveWorkloadReferencesIt proves a prepared
// workload keeps the workspace and its retention marker; release succeeds only
// after that identity is removed.
func TestWorkspaceReleaseRefusedWhileLiveWorkloadReferencesIt(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	prepareTestWorkload(t, fx.adapter, fx.request)
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}

	err := fx.adapter.Release(ctx, testWorkspaceID())
	assertFaultClass(t, err, fault.Forbidden)
	assertNoPathError := func() {
		if _, statErr := os.Lstat(fx.workspaceDir); statErr != nil {
			t.Fatalf("refused Release removed the workspace: %v", statErr)
		}
	}
	assertNoPathError()
	assertMarker(t, fx, testWorkspaceID(), true)

	removal, err := fx.adapter.Remove(ctx, engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("Remove = %+v; want removed", removal)
	}

	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Release after removal returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceReleaseDropsWorkspaceWithoutRetentionMarker proves Release does
// not require a retention marker. The Kernel calls Release as the single final
// drop after cleanup, so a lost marker must not leak the workspace, and the
// release stays idempotent.
func TestWorkspaceReleaseDropsWorkspaceWithoutRetentionMarker(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	assertMarker(t, fx, testWorkspaceID(), false)

	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Release without a retention marker returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("second Release returned error: %v", err)
	}
}

// TestWorkspaceReleaseAfterReconstructionWithoutMarker proves a reconstructed
// adapter releases a workspace whose retention marker was lost, once no live
// workload identity references it.
func TestWorkspaceReleaseAfterReconstructionWithoutMarker(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	reconstructed := newTestAdapter(t, fx.config())
	if err := os.RemoveAll(markerPath(fx, testWorkspaceID())); err != nil {
		t.Fatalf("removing the retention marker: %v", err)
	}
	assertMarker(t, fx, testWorkspaceID(), false)

	if err := reconstructed.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("reconstructed Release without a marker returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceReleaseRefusesLiveWorkloadWithoutMarker proves the marker is not
// what protects a live workspace: a non-tombstoned workload manifest refuses
// Release even when no retention marker exists.
func TestWorkspaceReleaseRefusesLiveWorkloadWithoutMarker(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	prepareTestWorkload(t, fx.adapter, fx.request)
	assertMarker(t, fx, testWorkspaceID(), false)

	err := fx.adapter.Release(ctx, testWorkspaceID())
	assertFaultClass(t, err, fault.Forbidden)
	if _, statErr := os.Lstat(fx.workspaceDir); statErr != nil {
		t.Fatalf("refused Release removed the workspace: %v", statErr)
	}
}

// TestWorkspaceReleaseFollowsKernelCleanupOrder pins the exact Kernel order:
// Retain at activation, Prepare/Start, confirmed whole-group stop, Remove (which
// tombstones the workload manifest) and only then Release as the single final
// drop. Release must succeed without requiring the marker, remove the
// workspace, clear the marker and stay idempotent.
func TestWorkspaceReleaseFollowsKernelCleanupOrder(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()

	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("activation Retain returned error: %v", err)
	}
	prepareTestWorkload(t, fx.adapter, fx.request)
	start := startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	if start.Outcome != engine.StartAccepted {
		t.Fatalf("Start outcome = %q; want accepted", start.Outcome)
	}
	assertMarker(t, fx, testWorkspaceID(), true)

	fx.observer.markLeaderExited(fx.groupID(t), 0, true)
	evidence, err := fx.adapter.Stop(ctx, engine.StopRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed whole-group termination", evidence)
	}

	removal, err := fx.adapter.Remove(ctx, engine.RemoveRequest{
		Identity:    fx.request.Identity,
		WorkspaceID: fx.request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Remove returned error: %v", err)
	}
	if !removal.Removed {
		t.Fatalf("removal = %+v; want the owned resources removed", removal)
	}
	assertNoPath(t, fx.resourceRoot(t))
	if !pathExists(t, fx.adapter.tombstonePath(fx.request.Identity.WorkloadID)) {
		t.Fatal("Remove did not tombstone the workload manifest")
	}

	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("final Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("second final Release returned error: %v", err)
	}
}

// TestWorkspaceReleaseNeverFollowsEscapedRetentionDirectory proves clearing the
// marker never follows a symlinked retention directory out of the adapter root.
// The unconfined marker is left untouched but never blocks the workspace drop.
func TestWorkspaceReleaseNeverFollowsEscapedRetentionDirectory(t *testing.T) {
	fx := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(outside, string(testWorkspaceID())), 0o755); err != nil {
		t.Fatalf("creating outside directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(fx.root, "retention")); err != nil {
		t.Fatalf("creating retention symlink: %v", err)
	}

	if err := fx.adapter.Release(context.Background(), testWorkspaceID()); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	if _, statErr := os.Lstat(filepath.Join(outside, string(testWorkspaceID()))); statErr != nil {
		t.Fatalf("Release escaped the adapter root: %v", statErr)
	}
}

// TestWorkspaceReleaseIsIdempotentForAbsentWorkspace proves an already-absent
// workspace is a clean release and a retry stays a clean release.
func TestWorkspaceReleaseIsIdempotentForAbsentWorkspace(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	if err := os.RemoveAll(fx.workspaceDir); err != nil {
		t.Fatalf("removing workspace: %v", err)
	}

	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Release of an absent workspace returned error: %v", err)
	}
	assertMarker(t, fx, testWorkspaceID(), false)
	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("second Release of an absent workspace returned error: %v", err)
	}
}

// TestWorkspaceReleaseSurfacesPartialDelete proves a failed removal is an
// error with the workspace and marker still observable, and a retry completes.
func TestWorkspaceReleaseSurfacesPartialDelete(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	workspaceSuffix := filepath.Join("workspaces", string(testWorkspaceID()))
	fx.files.injectBefore("RemoveAll", workspaceSuffix, errors.New("partial delete"))

	err := fx.adapter.Release(ctx, testWorkspaceID())
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if _, statErr := os.Lstat(fx.workspaceDir); statErr != nil {
		t.Fatalf("failed Release removed the workspace: %v", statErr)
	}
	assertMarker(t, fx, testWorkspaceID(), true)
	observation, statusErr := fx.adapter.Status(ctx, testWorkspaceID())
	if statusErr != nil {
		t.Fatalf("Status after a failed Release returned error: %v", statusErr)
	}
	if !observation.Available {
		t.Fatalf("Status after a failed Release = %+v; want available", observation)
	}

	fx.files.clear("RemoveAll", workspaceSuffix)
	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("retried Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceReleaseNeverTouchesOtherWorkspaces proves release is confined to
// the named workspace and its own retention marker.
func TestWorkspaceReleaseNeverTouchesOtherWorkspaces(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	otherDir, err := WorkspaceDir(fx.workspaceRoot, otherWorkspaceID())
	if err != nil {
		t.Fatalf("WorkspaceDir returned error: %v", err)
	}
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatalf("creating other workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "other.txt"), []byte("other"), 0o644); err != nil {
		t.Fatalf("seeding other workspace: %v", err)
	}
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	if err := fx.adapter.Retain(ctx, otherWorkspaceID()); err != nil {
		t.Fatalf("Retain other returned error: %v", err)
	}

	if err := fx.adapter.Release(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	if got := readHostFile(t, filepath.Join(otherDir, "other.txt")); got != "other" {
		t.Fatalf("other workspace bytes = %q; want %q", got, "other")
	}
	assertMarker(t, fx, otherWorkspaceID(), true)
	observation, statusErr := fx.adapter.Status(ctx, otherWorkspaceID())
	if statusErr != nil {
		t.Fatalf("Status for other workspace returned error: %v", statusErr)
	}
	if !observation.Available {
		t.Fatalf("Status for other workspace = %+v; want available", observation)
	}
}

// TestWorkspaceDetailsCarryNoSecrets proves status and refusal details never
// echo the execution credential or declared secret values.
func TestWorkspaceDetailsCarryNoSecrets(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	prepareTestWorkload(t, fx.adapter, fx.request)
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}

	observation, err := fx.adapter.Status(ctx, testWorkspaceID())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	releaseErr := fx.adapter.Release(ctx, testWorkspaceID())
	if releaseErr == nil {
		t.Fatal("Release succeeded while a live workload references the workspace")
	}
	for _, detail := range []string{observation.Detail, releaseErr.Error()} {
		for _, secret := range []string{"s3cr3t", "tok-abc"} {
			if strings.Contains(detail, secret) {
				t.Fatalf("workspace detail %q leaks secret material %q", detail, secret)
			}
		}
	}
}

// TestWorkspaceConcurrentRetainReleaseSchedule proves a Retain and a Release
// sharing the adapter lock never leave a falsely released workspace.
func TestWorkspaceConcurrentRetainReleaseSchedule(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	gate := fx.files.newGate("Rename")

	retained := make(chan error, 1)
	go func() {
		retained <- fx.adapter.Retain(ctx, testWorkspaceID())
	}()
	waitFor(t, gate.entered, "the retention marker rename barrier")

	released := make(chan error, 1)
	go func() {
		released <- fx.adapter.Release(ctx, testWorkspaceID())
	}()
	close(gate.release)

	if err := <-retained; err != nil {
		t.Fatalf("concurrent Retain returned error: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("concurrent Release returned error: %v", err)
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceConcurrentDuplicateRelease proves duplicate releases converge on
// one clean outcome and never fabricate a second deletion.
func TestWorkspaceConcurrentDuplicateRelease(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	gate := fx.files.newGate("RemoveAll")

	const workers = 2
	results := make([]error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index] = fx.adapter.Release(ctx, testWorkspaceID())
		}(index)
	}
	waitFor(t, gate.entered, "the workspace removal barrier")
	close(gate.release)
	wait.Wait()

	for index, err := range results {
		if err != nil {
			t.Fatalf("worker %d Release returned error: %v", index, err)
		}
	}
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}

// TestWorkspaceReleaseWaitsForInFlightStartThenRefuses proves workspace locking
// is shared with Start: a release cannot slip past a start in progress.
func TestWorkspaceReleaseWaitsForInFlightStartThenRefuses(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	prepareTestWorkload(t, fx.adapter, fx.request)
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("Retain returned error: %v", err)
	}
	gate := fx.executor.newGate("Spawn")

	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	started := make(chan startOutcome, 1)
	go func() {
		result, err := fx.adapter.Start(ctx, engine.StartRequest{
			Identity:    fx.request.Identity,
			WorkspaceID: fx.request.WorkspaceID,
		})
		started <- startOutcome{result: result, err: err}
	}()
	waitFor(t, gate.entered, "the spawn barrier")

	released := make(chan error, 1)
	go func() {
		released <- fx.adapter.Release(ctx, testWorkspaceID())
	}()
	close(gate.release)

	outcome := <-started
	if outcome.err != nil {
		t.Fatalf("Start returned error: %v", outcome.err)
	}
	if outcome.result.Outcome != engine.StartAccepted {
		t.Fatalf("Start outcome = %q; want accepted", outcome.result.Outcome)
	}
	releaseErr := <-released
	assertFaultClass(t, releaseErr, fault.Forbidden)
	if _, statErr := os.Lstat(fx.workspaceDir); statErr != nil {
		t.Fatalf("Release dropped a started workload's workspace: %v", statErr)
	}
	assertMarker(t, fx, testWorkspaceID(), true)
}

// TestWorkspaceRetainRacingReleaseFailsWithoutFalseSuccess proves a Retain that
// loses the race to a completed Release reports the lost workspace instead of
// recreating it.
func TestWorkspaceRetainRacingReleaseFailsWithoutFalseSuccess(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	if err := fx.adapter.Retain(ctx, testWorkspaceID()); err != nil {
		t.Fatalf("initial Retain returned error: %v", err)
	}
	gate := fx.files.newGate("RemoveAll")

	released := make(chan error, 1)
	go func() {
		released <- fx.adapter.Release(ctx, testWorkspaceID())
	}()
	waitFor(t, gate.entered, "the workspace removal barrier")

	retained := make(chan error, 1)
	go func() {
		retained <- fx.adapter.Retain(ctx, testWorkspaceID())
	}()
	close(gate.release)

	if err := <-released; err != nil {
		t.Fatalf("Release returned error: %v", err)
	}
	err := <-retained
	assertFaultClass(t, err, fault.NotFound)
	assertNoPath(t, fx.workspaceDir)
	assertMarker(t, fx, testWorkspaceID(), false)
}
