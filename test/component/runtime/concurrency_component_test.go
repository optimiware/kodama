//go:build component

package runtimeconformance

import (
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestRuntimeIdentityNamespaces proves the Podman adapter's deterministic
// names are run-scoped. Its premise is the regression: two runs that both use
// the literal workspace/workload ids derive exactly one container and volume
// name, so concurrent runs collide. Mapping every literal through one run scope
// keeps the names deterministic inside a run and distinct between runs.
func TestRuntimeIdentityNamespaces(t *testing.T) {
	const workspaceBase = "workspace-podman-1"
	const workloadBase = "workload-c1"

	first := runtimesupport.NewScope()
	second := runtimesupport.NewScope()
	if first.Suffix() == second.Suffix() {
		t.Fatalf("two run scopes share the suffix %q", first.Suffix())
	}

	// A literal is not run-scoped: every run maps it to the same product name.
	literalVolume, err := podman.WorkspaceVolumeName(id.WorkspaceID(workspaceBase))
	if err != nil {
		t.Fatalf("literal WorkspaceVolumeName: %v", err)
	}
	literalVolumeAgain, err := podman.WorkspaceVolumeName(id.WorkspaceID(workspaceBase))
	if err != nil {
		t.Fatalf("literal WorkspaceVolumeName: %v", err)
	}
	if literalVolume != literalVolumeAgain {
		t.Fatalf("literal workspace ids no longer collide deterministically: %q vs %q", literalVolume, literalVolumeAgain)
	}

	firstWorkspace := id.WorkspaceID(first.ID(workspaceBase))
	secondWorkspace := id.WorkspaceID(second.ID(workspaceBase))
	for _, workspace := range []id.WorkspaceID{firstWorkspace, secondWorkspace} {
		if _, err := id.ParseWorkspaceID(string(workspace)); err != nil {
			t.Fatalf("run-scoped workspace id %q is invalid: %v", workspace, err)
		}
	}
	firstVolume, err := podman.WorkspaceVolumeName(firstWorkspace)
	if err != nil {
		t.Fatalf("first WorkspaceVolumeName: %v", err)
	}
	secondVolume, err := podman.WorkspaceVolumeName(secondWorkspace)
	if err != nil {
		t.Fatalf("second WorkspaceVolumeName: %v", err)
	}
	if firstVolume == secondVolume {
		t.Fatalf("two run scopes derive one workspace volume name %q", firstVolume)
	}
	if firstVolume == literalVolume || secondVolume == literalVolume {
		t.Fatalf("run-scoped volume name did not move off the literal name %q", literalVolume)
	}

	// The full identity is scoped once, so every derived workload name and the
	// complete effect identity differ between runs too.
	firstIdentity := testIdentityInScope(first, "exec-c1", "action-c1", workloadBase, "idem-c1")
	secondIdentity := testIdentityInScope(second, "exec-c1", "action-c1", workloadBase, "idem-c1")
	for _, identity := range []engine.EffectIdentity{firstIdentity, secondIdentity} {
		if _, err := id.ParseExecutionID(string(identity.ExecutionID)); err != nil {
			t.Fatalf("run-scoped execution id %q is invalid: %v", identity.ExecutionID, err)
		}
		if _, err := id.ParseActionID(string(identity.ActionID)); err != nil {
			t.Fatalf("run-scoped action id %q is invalid: %v", identity.ActionID, err)
		}
		if _, err := id.ParseWorkloadID(string(identity.WorkloadID)); err != nil {
			t.Fatalf("run-scoped workload id %q is invalid: %v", identity.WorkloadID, err)
		}
	}
	firstContainer, err := podman.ContainerName(firstIdentity.WorkloadID)
	if err != nil {
		t.Fatalf("first ContainerName: %v", err)
	}
	secondContainer, err := podman.ContainerName(secondIdentity.WorkloadID)
	if err != nil {
		t.Fatalf("second ContainerName: %v", err)
	}
	if firstContainer == secondContainer {
		t.Fatalf("two run scopes derive one workload container name %q", firstContainer)
	}
	if firstIdentity.ExecutionID == secondIdentity.ExecutionID ||
		firstIdentity.ActionID == secondIdentity.ActionID ||
		firstIdentity.IdempotencyKey == secondIdentity.IdempotencyKey {
		t.Fatalf("identity kinds are not run-scoped: %+v vs %+v", firstIdentity, secondIdentity)
	}
}

// TestRuntimeConcurrentTwinIsolation proves two runs that share the literal
// fixture ids stay independent: both own live containers and retained volume
// content at the same time, and closing one run removes only its own resources.
// The twin runs use distinct scopes, exactly as two concurrent test processes
// do, so the fixture-level collision is exercised without a second process.
func TestRuntimeConcurrentTwinIsolation(t *testing.T) {
	ctx := t.Context()
	firstScope := runtimesupport.NewScope()
	secondScope := runtimesupport.NewScope()
	firstTwin := newPodmanFixtureInScope(t, firstScope, "workspace-podman-1")
	secondTwin := newPodmanFixtureInScope(t, secondScope, "workspace-podman-1")
	if firstTwin.workspaceID == secondTwin.workspaceID {
		t.Fatalf("twins share workspace id %q", firstTwin.workspaceID)
	}
	if firstTwin.volumeName == secondTwin.volumeName {
		t.Fatalf("twins share workspace volume %q", firstTwin.volumeName)
	}

	firstIdentity := testIdentityInScope(firstScope, "exec-twin", "action-twin", "workload-twin", "idem-twin")
	secondIdentity := testIdentityInScope(secondScope, "exec-twin", "action-twin", "workload-twin", "idem-twin")
	launch := func(fixture *podmanFixture, identity engine.EffectIdentity) {
		t.Helper()
		request := fixture.request(identity, []string{`echo alive > /workspace/alive`}, nil, nil, "tok-abc")
		if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
			t.Fatalf("Prepare %q: %v", identity.WorkloadID, err)
		}
		start, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
		if err != nil {
			t.Fatalf("Start %q: %v", identity.WorkloadID, err)
		}
		if start.Outcome != engine.StartAccepted {
			t.Fatalf("start outcome for %q = %q; want accepted", identity.WorkloadID, start.Outcome)
		}
		waitForPodmanObservation(t, ctx, fixture.adapter,
			engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	}
	launch(firstTwin, firstIdentity)
	launch(secondTwin, secondIdentity)

	// Positive observations before cleanup: each run owns one container and its
	// own retained workspace bytes, while the twin is still alive.
	assertOwned := func(fixture *podmanFixture, identity engine.EffectIdentity, want string) {
		t.Helper()
		name, err := podman.ContainerName(identity.WorkloadID)
		if err != nil {
			t.Fatalf("ContainerName %q: %v", identity.WorkloadID, err)
		}
		names := podmanContainerNames(t, ctx, fixture.workspaceID)
		if len(names) != 1 || names[0] != name {
			t.Fatalf("workspace %q containers = %v; want exactly %q", fixture.workspaceID, names, name)
		}
		if got := string(readVolume(t, ctx, fixture, "alive")); got != want {
			t.Fatalf("workspace %q content = %q; want %q", fixture.workspaceID, got, want)
		}
	}
	assertOwned(firstTwin, firstIdentity, "alive\n")
	assertOwned(secondTwin, secondIdentity, "alive\n")

	// Close the first run exactly as the suite does at test end.
	if removal, err := firstTwin.adapter.Remove(ctx, engine.RemoveRequest{Identity: firstIdentity, WorkspaceID: firstTwin.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("first twin Remove = %+v, %v; want removed", removal, err)
	}
	if err := firstTwin.adapter.Release(ctx, firstTwin.workspaceID); err != nil {
		t.Fatalf("first twin Release: %v", err)
	}
	if err := runtimesupport.RemoveRunContainersInWorkspace(ctx, string(firstTwin.workspaceID)); err != nil {
		t.Fatalf("first twin container sweep: %v", err)
	}
	if err := runtimesupport.RemoveWorkspaceVolume(ctx, firstTwin.volumeName); err != nil {
		t.Fatalf("first twin volume sweep: %v", err)
	}

	// The closed run is gone; the twin keeps its container and its bytes.
	if names := podmanContainerNames(t, ctx, firstTwin.workspaceID); len(names) != 0 {
		t.Fatalf("closed run still has containers: %v", names)
	}
	if runtimesupport.VolumeExists(ctx, firstTwin.volumeName) {
		t.Fatalf("closed run volume %q still exists", firstTwin.volumeName)
	}
	assertOwned(secondTwin, secondIdentity, "alive\n")

	// Clean the surviving twin so the run-scoped sweeps have nothing left.
	if removal, err := secondTwin.adapter.Remove(ctx, engine.RemoveRequest{Identity: secondIdentity, WorkspaceID: secondTwin.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("second twin Remove = %+v, %v; want removed", removal, err)
	}
	if err := secondTwin.adapter.Release(ctx, secondTwin.workspaceID); err != nil {
		t.Fatalf("second twin Release: %v", err)
	}
}
