//go:build component

package runtimeconformance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestPodmanRuntimeWorkspacePersistenceAndCredentialRefresh proves a retained
// workspace volume survives container recreation and that a credential refresh
// is visible to the running workload without a restart.
func TestPodmanRuntimeWorkspacePersistenceAndCredentialRefresh(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-2")

	first := testIdentity("exec-c2a", "action-c2a", "workload-c2a", "idem-c2a")
	firstRequest := fixture.request(first, []string{
		`echo persisted > /workspace/data`,
		`echo first >> /workspace/launch-count`,
	}, nil, nil, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, firstRequest); err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: first, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: first, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: first, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("first Remove = %+v, %v; want removed", removal, err)
	}
	if !runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("workspace volume disappeared with the first container")
	}

	second := testIdentity("exec-c2b", "action-c2b", "workload-c2b", "idem-c2b")
	secondRequest := fixture.request(second, []string{
		`cat /workspace/data > /workspace/readback`,
		`echo second >> /workspace/launch-count`,
	}, nil, nil, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, secondRequest); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: second, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: second, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	if got := string(readVolume(t, ctx, fixture, "readback")); got != "persisted\n" {
		t.Fatalf("workspace data after container recreation = %q; want persisted", got)
	}
	if got := strings.TrimSpace(string(readVolume(t, ctx, fixture, "launch-count"))); got != "first\nsecond" {
		t.Fatalf("launch count = %q; want first then second", got)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: second, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("second Remove = %+v, %v; want removed", removal, err)
	}

	third := testIdentity("exec-c2c", "action-c2c", "workload-c2c", "idem-c2c")
	thirdRequest := fixture.request(third, []string{
		`i=0`,
		`while [ ! -f /workspace/stop ]; do`,
		`  cat /kodama/secrets/KODAMA_TOKEN > /workspace/token-seen`,
		`  i=$((i+1))`,
		`  if [ "$i" -ge 60 ]; then break; fi`,
		`  sleep 1`,
		`done`,
	}, nil, nil, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, thirdRequest); err != nil {
		t.Fatalf("third Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: third, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("third Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: third, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)
	waitForVolumeContent(t, ctx, fixture, "token-seen", "tok-abc")
	name, err := podman.ContainerName(third.WorkloadID)
	if err != nil {
		t.Fatalf("ContainerName: %v", err)
	}
	before, err := fixture.libpod.InspectContainer(ctx, name)
	if err != nil {
		t.Fatalf("inspect before refresh: %v", err)
	}
	if _, err := fixture.adapter.RefreshCredential(ctx, engine.CredentialRequest{Identity: third, Credential: "tok-new"}); err != nil {
		t.Fatalf("RefreshCredential: %v", err)
	}
	waitForVolumeContent(t, ctx, fixture, "token-seen", "tok-new")
	credentialPath := filepath.Join(fixture.root, string(third.WorkloadID), "kodama", "secrets", "KODAMA_TOKEN")
	credential, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatalf("read replaced credential: %v", err)
	}
	if string(credential) != "tok-new" {
		t.Fatalf("replaced credential = %q; want the whole tok-new value", credential)
	}
	if info, err := os.Stat(credentialPath); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("replaced credential mode = %v, %v; want 444", info, err)
	}
	after, err := fixture.libpod.InspectContainer(ctx, name)
	if err != nil {
		t.Fatalf("inspect after refresh: %v", err)
	}
	if before.ID != after.ID || after.Status != podman.ContainerStatusRunning {
		t.Fatalf("credential refresh restarted the container: before %+v after %+v", before, after)
	}
	if evidence := waitForPodmanStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: third, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: third, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("third Remove = %+v, %v; want removed", removal, err)
	}
	if err := fixture.adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "workspace-persistence-and-credential-refresh",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// TestPodmanRuntimeNoReplayAcrossControllerRestart proves a lost start response
// is StartUnknown, a reconstructed controller reuses the same stable container,
// and an attempted-but-gone container is never recreated.
func TestPodmanRuntimeNoReplayAcrossControllerRestart(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-3")
	identity := testIdentity("exec-c3", "action-c3", "workload-c3", "idem-c3")
	wrapper := &lostStartLibpod{Libpod: fixture.libpod, lose: true}
	_, _, adapter := newPodmanAdapter(t, fixture, wrapper)
	request := fixture.request(identity, []string{
		`echo launched >> /workspace/launch-count`,
	}, nil, nil, "tok-abc")
	if _, err := adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	start, err := adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err == nil || start.Outcome != engine.StartUnknown {
		t.Fatalf("lost start = %q/%v; want unknown", start.Outcome, err)
	}
	if wrapper.startCalls() != 1 {
		t.Fatalf("start calls = %d; want 1", wrapper.startCalls())
	}

	recovered := newPodmanAdapterOnly(t, fixture)
	recoveredStart, err := recovered.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("recovered Start: %v", err)
	}
	if recoveredStart.Outcome != engine.StartAccepted {
		t.Fatalf("recovered start outcome = %q; want accepted", recoveredStart.Outcome)
	}
	if wrapper.startCalls() != 1 {
		t.Fatalf("recovered start issued another start: %d calls", wrapper.startCalls())
	}
	if names := podmanContainerNames(t, ctx, fixture.workspaceID); len(names) != 1 {
		t.Fatalf("workspace containers = %v; want exactly one", names)
	}
	if got := strings.Count(string(readVolume(t, ctx, fixture, "launch-count")), "launched"); got != 1 {
		t.Fatalf("workload launched %d times; want 1", got)
	}
	waitForPodmanObservation(t, ctx, recovered,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	recoveredStart, err = recovered.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil || recoveredStart.Outcome != engine.StartAccepted {
		t.Fatalf("start after exit = %q/%v; want accepted", recoveredStart.Outcome, err)
	}
	if got := strings.Count(string(readVolume(t, ctx, fixture, "launch-count")), "launched"); got != 1 {
		t.Fatalf("workload relaunched after exit: %d", got)
	}

	// An attempted start whose container is gone is never recreated.
	goneIdentity := testIdentity("exec-c3b", "action-c3b", "workload-c3b", "idem-c3b")
	goneWrapper := &lostStartLibpod{Libpod: fixture.libpod, lose: true}
	_, _, goneAdapter := newPodmanAdapter(t, fixture, goneWrapper)
	goneRequest := fixture.request(goneIdentity, []string{`echo gone >> /workspace/gone`}, nil, nil, "tok-abc")
	if _, err := goneAdapter.Prepare(ctx, goneRequest); err != nil {
		t.Fatalf("gone Prepare: %v", err)
	}
	if start, err := goneAdapter.Start(ctx, engine.StartRequest{Identity: goneIdentity, WorkspaceID: fixture.workspaceID}); err == nil || start.Outcome != engine.StartUnknown {
		t.Fatalf("gone lost start = %q/%v; want unknown", start.Outcome, err)
	}
	goneName, err := podman.ContainerName(goneIdentity.WorkloadID)
	if err != nil {
		t.Fatalf("gone ContainerName: %v", err)
	}
	if err := fixture.libpod.RemoveContainer(ctx, goneName); err != nil && !errors.Is(err, podman.ErrNotFound) {
		t.Fatalf("remove attempted container: %v", err)
	}
	createdBefore := goneWrapper.createCalls()
	retry, err := goneAdapter.Start(ctx, engine.StartRequest{Identity: goneIdentity, WorkspaceID: fixture.workspaceID})
	if err == nil || retry.Outcome != engine.StartUnknown {
		t.Fatalf("start of a gone attempted container = %q/%v; want unknown", retry.Outcome, err)
	}
	if goneWrapper.createCalls() != createdBefore {
		t.Fatalf("a gone attempted container was recreated: %d -> %d", createdBefore, goneWrapper.createCalls())
	}
	if _, err := goneAdapter.Remove(ctx, engine.RemoveRequest{Identity: goneIdentity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("gone Remove: %v", err)
	}
	observation, err := goneAdapter.Observe(ctx, engine.ObserveRequest{Identity: goneIdentity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("gone Observe: %v", err)
	}
	if observation.Kind != kernel.ObservationAbsent {
		t.Fatalf("removed observation = %+v; want absent", observation)
	}
	if evidence := waitForPodmanStop(t, ctx, recovered, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	if removal, err := recovered.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if err := recovered.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "no-replay-across-controller-restart",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// TestPodmanRuntimeUncertainStartAndStop interrupts the real Podman service
// mid-call and asserts the uncertain classification; after recovery the
// stop barrier confirms only real whole-container termination.
func TestPodmanRuntimeUncertainStartAndStop(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-4")

	startIdentity := testIdentity("exec-c4a", "action-c4a", "workload-c4a", "idem-c4a")
	gated := newGatedLibpod(fixture.libpod)
	_, _, startAdapter := newPodmanAdapter(t, fixture, gated)
	startRequest := fixture.request(startIdentity, []string{`sleep 30`}, nil, nil, "tok-abc")
	if _, err := startAdapter.Prepare(ctx, startRequest); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	type startOutcome struct {
		result engine.StartResult
		err    error
	}
	started := make(chan startOutcome, 1)
	go func() {
		result, err := startAdapter.Start(ctx, engine.StartRequest{Identity: startIdentity, WorkspaceID: fixture.workspaceID})
		started <- startOutcome{result: result, err: err}
	}()
	<-gated.startEntered
	fixture.service.Kill()
	close(gated.startRelease)
	outcome := <-started
	if outcome.err == nil || outcome.result.Outcome != engine.StartUnknown {
		t.Fatalf("interrupted start = %q/%v; want unknown", outcome.result.Outcome, outcome.err)
	}
	fixture.service.Restart(t)
	observation, err := startAdapter.Observe(ctx, engine.ObserveRequest{Identity: startIdentity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Observe after restart: %v", err)
	}
	if observation.Kind == kernel.ObservationNeverStarted || observation.ProofOfNonexecution() {
		t.Fatalf("ambiguous start collapsed into never-started: %+v", observation)
	}
	// Clean the ambiguous created container; it may never have started. The
	// removal also clears the durable start-attempt marker so the workspace can
	// be released after the rest of the test.
	if err := runtimesupport.RemoveRunContainersInWorkspace(ctx, string(fixture.workspaceID)); err != nil {
		t.Fatalf("remove ambiguous container: %v", err)
	}
	if removal, err := startAdapter.Remove(ctx, engine.RemoveRequest{Identity: startIdentity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("clean ambiguous workload = %+v, %v; want removed", removal, err)
	}

	stopIdentity := testIdentity("exec-c4b", "action-c4b", "workload-c4b", "idem-c4b")
	stopRequest := fixture.request(stopIdentity, []string{`sleep 30`}, nil, nil, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, stopRequest); err != nil {
		t.Fatalf("stop Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: stopIdentity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("stop Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: stopIdentity, WorkspaceID: fixture.workspaceID}, kernel.ObservationRunning)

	stopGate := newGatedLibpod(fixture.libpod)
	_, _, stopAdapter := newPodmanAdapter(t, fixture, stopGate)
	type stopOutcome struct {
		evidence kernel.StopEvidence
		err      error
	}
	stopped := make(chan stopOutcome, 1)
	go func() {
		evidence, err := stopAdapter.Stop(ctx, engine.StopRequest{Identity: stopIdentity, WorkspaceID: fixture.workspaceID})
		stopped <- stopOutcome{evidence: evidence, err: err}
	}()
	<-stopGate.stopEntered
	fixture.service.Kill()
	close(stopGate.stopRelease)
	pending := <-stopped
	assertFaultClass(t, pending.err, fault.UncertainExternalEffect)
	if pending.evidence.IsConfirmed() {
		t.Fatalf("interrupted stop confirmed termination: %+v", pending.evidence)
	}
	fixture.service.Restart(t)
	if evidence := waitForPodmanStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: stopIdentity, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("recovered stop evidence = %+v; want confirmed", evidence)
	}
	stopName, err := podman.ContainerName(stopIdentity.WorkloadID)
	if err != nil {
		t.Fatalf("ContainerName: %v", err)
	}
	state, err := fixture.libpod.InspectContainer(ctx, stopName)
	if err != nil {
		t.Fatalf("inspect after confirmed stop: %v", err)
	}
	if state.Status != podman.ContainerStatusExited || !state.WholeStopped {
		t.Fatalf("confirmed stop state = %+v; want exited with whole-container evidence", state)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: stopIdentity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if err := fixture.adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "uncertain-start-and-stop",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// TestPodmanRuntimeCleanupAndWorkspaceExclusivity proves a workspace is
// released only when no labelled container owns it, removal failures stay
// pending, and two controllers contend for exactly one owner.
func TestPodmanRuntimeCleanupAndWorkspaceExclusivity(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-5")
	identity := testIdentity("exec-c5", "action-c5", "workload-c5", "idem-c5")
	request := fixture.request(identity, []string{`echo done > /workspace/done`}, nil, nil, "tok-abc")
	if _, err := fixture.adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := fixture.adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, fixture.adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	if evidence := waitForPodmanStop(t, ctx, fixture.adapter, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	// A second controller sees the labelled container as the owner and cannot
	// release the workspace while it exists.
	second := newPodmanAdapterOnly(t, fixture)
	if err := second.Release(ctx, fixture.workspaceID); err == nil {
		t.Fatal("released a workspace still owned by a labelled container")
	} else {
		assertFaultClass(t, err, fault.Forbidden)
	}
	if removal, err := fixture.adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil || !removal.Removed {
		t.Fatalf("Remove = %+v, %v; want removed", removal, err)
	}
	if names := podmanContainerNames(t, ctx, fixture.workspaceID); len(names) != 0 {
		t.Fatalf("containers remain after Remove: %v", names)
	}
	if err := second.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release after Remove: %v", err)
	}
	if runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("workspace volume still exists after release")
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "cleanup-and-workspace-exclusivity",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// TestPodmanRuntimeRemovalFailureStaysPending proves a bounded removal failure
// keeps the owned container and fence visible, and a retry completes cleanup
// without touching the retained workspace.
func TestPodmanRuntimeRemovalFailureStaysPending(t *testing.T) {
	ctx := t.Context()
	fixture := newPodmanFixture(t, "workspace-podman-6")
	identity := testIdentity("exec-c6", "action-c6", "workload-c6", "idem-c6")
	failing := &failingRemoveLibpod{Libpod: fixture.libpod, failures: 3}
	_, _, adapter := newPodmanAdapter(t, fixture, failing)
	request := fixture.request(identity, []string{`echo done > /workspace/done`}, nil, nil, "tok-abc")
	if _, err := adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForPodmanObservation(t, ctx, adapter,
		engine.ObserveRequest{Identity: identity, WorkspaceID: fixture.workspaceID}, kernel.ObservationExited)
	if evidence := waitForPodmanStop(t, ctx, adapter, engine.StopRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); !evidence.IsConfirmed() {
		t.Fatalf("stop evidence = %+v; want confirmed", evidence)
	}
	removal, err := adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removal.Removed {
		t.Fatalf("removal with exhausted attempts = %+v; want retained", removal)
	}
	name, _ := podman.ContainerName(identity.WorkloadID)
	if !strings.Contains(removal.Detail, name) {
		t.Fatalf("removal detail does not name the retained container: %q", removal.Detail)
	}
	if _, err := adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: fixture.workspaceID}); err == nil {
		t.Fatal("a fenced identity was started again")
	}
	failing.clearFailures()
	removal, err = adapter.Remove(ctx, engine.RemoveRequest{Identity: identity, WorkspaceID: fixture.workspaceID})
	if err != nil || !removal.Removed {
		t.Fatalf("retried Remove = %+v, %v; want removed", removal, err)
	}
	if !runtimesupport.VolumeExists(ctx, fixture.volumeName) {
		t.Fatal("Remove touched the retained workspace volume")
	}
	if err := adapter.Release(ctx, fixture.workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	runtimesupport.RecordResult(t, runtimesupport.ResultRecord{
		Adapter: "podman", Scenario: "removal-failure-stays-pending",
		ImageTag: fixture.image.Tag, ImageDigest: fixture.image.Digest,
		Podman: runtimesupport.PodmanVersion(ctx),
	})
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// lostStartLibpod loses exactly one start response after the real effect.
type lostStartLibpod struct {
	podman.Libpod
	mu      sync.Mutex
	lose    bool
	starts  int
	creates int
}

func (l *lostStartLibpod) CreateContainer(ctx context.Context, spec podman.ContainerSpec) (podman.ContainerState, error) {
	l.mu.Lock()
	l.creates++
	l.mu.Unlock()
	return l.Libpod.CreateContainer(ctx, spec)
}

func (l *lostStartLibpod) StartContainer(ctx context.Context, name string) error {
	l.mu.Lock()
	l.starts++
	lose := l.lose
	l.lose = false
	l.mu.Unlock()
	if lose {
		if err := l.Libpod.StartContainer(ctx, name); err != nil {
			return err
		}
		return errors.New("injected lost start response")
	}
	return l.Libpod.StartContainer(ctx, name)
}

func (l *lostStartLibpod) startCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts
}

func (l *lostStartLibpod) createCalls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.creates
}

// gatedLibpod blocks the first start and stop call until the test releases it.
type gatedLibpod struct {
	podman.Libpod
	startEntered chan struct{}
	startRelease chan struct{}
	stopEntered  chan struct{}
	stopRelease  chan struct{}
	startOnce    sync.Once
	stopOnce     sync.Once
}

func newGatedLibpod(inner podman.Libpod) *gatedLibpod {
	return &gatedLibpod{
		Libpod:       inner,
		startEntered: make(chan struct{}),
		startRelease: make(chan struct{}),
		stopEntered:  make(chan struct{}),
		stopRelease:  make(chan struct{}),
	}
}

func (g *gatedLibpod) StartContainer(ctx context.Context, name string) error {
	block := false
	g.startOnce.Do(func() {
		block = true
		close(g.startEntered)
	})
	if block {
		select {
		case <-g.startRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Libpod.StartContainer(ctx, name)
}

func (g *gatedLibpod) StopContainer(ctx context.Context, name string) error {
	block := false
	g.stopOnce.Do(func() {
		block = true
		close(g.stopEntered)
	})
	if block {
		select {
		case <-g.stopRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Libpod.StopContainer(ctx, name)
}

// failingRemoveLibpod fails the first N container removals before the effect.
type failingRemoveLibpod struct {
	podman.Libpod
	mu       sync.Mutex
	failures int
}

func (f *failingRemoveLibpod) RemoveContainer(ctx context.Context, name string) error {
	f.mu.Lock()
	fail := f.failures > 0
	if fail {
		f.failures--
	}
	f.mu.Unlock()
	if fail {
		return errors.New("injected removal failure")
	}
	return f.Libpod.RemoveContainer(ctx, name)
}

func (f *failingRemoveLibpod) clearFailures() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = 0
}

// newPodmanAdapterOnly builds a fresh adapter over the fixture's real client
// and root.
func newPodmanAdapterOnly(t *testing.T, fixture *podmanFixture) *podman.Adapter {
	t.Helper()
	_, _, adapter := newPodmanAdapter(t, fixture, nil)
	return adapter
}

// waitForVolumeContent waits for a file in the retained volume to contain want.
func waitForVolumeContent(t *testing.T, ctx context.Context, fixture *podmanFixture, logical, want string) {
	t.Helper()
	probe := func(context.Context) error {
		data, found := runtimesupport.ReadVolumeFile(ctx, t, fixture.volumeName, logical)
		if !found {
			return errors.New("volume file is missing")
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(want) {
			return errors.New("volume content does not match yet")
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "volume content "+logical, probe, harness.DefaultWaitOptions()); err != nil {
		t.Fatalf("wait for volume content %q = %q: %v", logical, want, err)
	}
}
