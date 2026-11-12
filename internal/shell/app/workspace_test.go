package app

// Fresh-only workspace provisioning tests.
//
// The Kernel calls engine.Workspace.Retain before it commits an activation
// (internal/kernel/engine/reconcile.go activate). The Podman adapter's Retain
// refuses an absent volume ("retention never recreates it"), so the composition
// must provision the deterministic owned volume before Retain. It must do so
// only for a never-activated workspace: a missing volume of an execution that
// already ran is evidence of workspace loss and must surface as NotFound, never
// be recreated as an empty workspace.
//
// The engine.Workspace.Retain request carries only the workspace id.
// That is not enough to tell the first activation of a fresh workspace from a
// later activation of a running one, and neither the volume's presence (a
// missing volume is ambiguous) nor process memory (a restart loses it) is a
// safe substitute. The decorator therefore takes one app-owned fact source,
// WorkspaceActivation, whose real implementation must read durable persisted
// execution state and survive a controller restart.
//
// Production API under test (internal/shell/app/workspace.go and compose.go):
//
//	type VolumeProvisioner interface {
//		CreateVolume(ctx context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error)
//	}
//
//	// PreviouslyActivated reports whether the workspace's execution had already
//	// activated an action before the activation that triggered this Retain.
//	// false means the Kernel's first activation of a fresh queued workspace.
//	type WorkspaceActivation interface {
//		PreviouslyActivated(ctx context.Context, workspaceID id.WorkspaceID) (bool, error)
//	}
//
//	// newProvisioningWorkspace validates every dependency and returns a
//	// kernelengine.Workspace (concretely *provisioningWorkspace).
//	func newProvisioningWorkspace(inner kernelengine.Workspace, provisioner VolumeProvisioner, activation WorkspaceActivation) (kernelengine.Workspace, error)
//
//	// Optional composition adapters. A nil VolumeProvisioner means "no
//	// provisioning decorator".
//	Adapters.VolumeProvisioner VolumeProvisioner
//	Adapters.WorkspaceActivation WorkspaceActivation
//
// Decorator contract:
//   - Status and Release delegate unchanged and never provision.
//   - A fresh workspace (PreviouslyActivated == false) calls CreateVolume
//     exactly once with WorkspaceVolumeName(workspaceID) and the single
//     ownership label runtimepodman.LabelWorkspace = string(workspaceID), then
//     delegates Retain. The idempotent primitive is what makes a retry after a
//     lost response safe, so a retry calls CreateVolume again.
//   - An already-activated workspace (PreviouslyActivated == true) never calls
//     CreateVolume; the adapter's Retain surfaces a missing volume as
//     fault.NotFound.
//   - A failed CreateVolume is returned without calling Retain. The
//     drivers/podman ErrAmbiguous sentinel is classified as
//     fault.UncertainExternalEffect; fault classes (Forbidden,
//     DependencyUnavailable, ...) pass through.
//   - A cancelled context performs no external effect.
//   - The decorator never renders the volume or its labels into an error.
//
// Wiring contract: the environment builder installs the decorator around the
// resolved workspace only for ProfileDevelopment/ProfileTest when
// cfg.Installation.Runtime == installation.RuntimePodman and
// Adapters.VolumeProvisioner is non-nil. Every other runtime/profile and a nil
// provisioner leave the workspace adapter untouched. Production uses
// ProductionConfig.Workspace and is never wrapped.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	podmandriver "github.com/optimiware/kodama/internal/shell/app/drivers/podman"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
)

// callLog records the order of the fake external effects.
type callLog struct {
	mu     sync.Mutex
	events []string
}

func (log *callLog) add(event string) {
	log.mu.Lock()
	log.events = append(log.events, event)
	log.mu.Unlock()
}

func (log *callLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

// recordingWorkspace is a kernelengine.Workspace fake with no host effect.
type recordingWorkspace struct {
	log *callLog

	mu           sync.Mutex
	statusCalls  int
	retainCalls  int
	releaseCalls int
	observation  kernelengine.WorkspaceObservation
	statusErr    error
	retainErr    error
	releaseErr   error
}

func (workspace *recordingWorkspace) record(event string) {
	if workspace.log != nil {
		workspace.log.add(event)
	}
}

func (workspace *recordingWorkspace) Status(ctx context.Context, workspaceID id.WorkspaceID) (kernelengine.WorkspaceObservation, error) {
	workspace.mu.Lock()
	workspace.statusCalls++
	observation := workspace.observation
	err := workspace.statusErr
	workspace.mu.Unlock()
	if err != nil {
		return kernelengine.WorkspaceObservation{}, err
	}
	if observation.WorkspaceID == "" {
		observation.WorkspaceID = workspaceID
	}
	return observation, nil
}

func (workspace *recordingWorkspace) Retain(ctx context.Context, workspaceID id.WorkspaceID) error {
	workspace.mu.Lock()
	workspace.retainCalls++
	err := workspace.retainErr
	workspace.mu.Unlock()
	workspace.record("retain")
	return err
}

func (workspace *recordingWorkspace) Release(ctx context.Context, workspaceID id.WorkspaceID) error {
	workspace.mu.Lock()
	workspace.releaseCalls++
	err := workspace.releaseErr
	workspace.mu.Unlock()
	workspace.record("release")
	return err
}

func (workspace *recordingWorkspace) retainCount() int {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.retainCalls
}

func (workspace *recordingWorkspace) statusCount() int {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.statusCalls
}

func (workspace *recordingWorkspace) releaseCount() int {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.releaseCalls
}

// recordingProvisioner is a VolumeProvisioner fake that records every requested
// volume and delegates to an optional create function.
type recordingProvisioner struct {
	log *callLog

	mu      sync.Mutex
	volumes []runtimepodman.Volume
	create  func(context.Context, runtimepodman.Volume) (runtimepodman.Volume, error)
}

func (provisioner *recordingProvisioner) CreateVolume(ctx context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	provisioner.mu.Lock()
	provisioner.volumes = append(provisioner.volumes, volume)
	create := provisioner.create
	provisioner.mu.Unlock()
	if provisioner.log != nil {
		provisioner.log.add("create")
	}
	if create != nil {
		return create(ctx, volume)
	}
	return volume, nil
}

func (provisioner *recordingProvisioner) count() int {
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	return len(provisioner.volumes)
}

func (provisioner *recordingProvisioner) requested() []runtimepodman.Volume {
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	return append([]runtimepodman.Volume(nil), provisioner.volumes...)
}

// fakeActivation is a WorkspaceActivation fake keyed by workspace identity.
//
// It also reports the durable owning tenant the Kubernetes claim decorator
// matches against its trusted claim template. The Podman decorator never calls
// OwningTenant, so the zero tenant is harmless there.
type fakeActivation struct {
	mu          sync.Mutex
	previously  map[id.WorkspaceID]bool
	err         error
	calls       []id.WorkspaceID
	tenant      id.TenantID
	tenants     map[id.WorkspaceID]id.TenantID
	tenantErr   error
	tenantCalls []id.WorkspaceID
}

func (activation *fakeActivation) PreviouslyActivated(ctx context.Context, workspaceID id.WorkspaceID) (bool, error) {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	activation.calls = append(activation.calls, workspaceID)
	if activation.err != nil {
		return false, activation.err
	}
	return activation.previously[workspaceID], nil
}

// OwningTenant reports the durable owning tenant of the workspace's execution.
// A per-workspace value wins over the single default tenant.
func (activation *fakeActivation) OwningTenant(ctx context.Context, workspaceID id.WorkspaceID) (id.TenantID, error) {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	activation.tenantCalls = append(activation.tenantCalls, workspaceID)
	if activation.tenantErr != nil {
		return "", activation.tenantErr
	}
	if tenant, ok := activation.tenants[workspaceID]; ok {
		return tenant, nil
	}
	return activation.tenant, nil
}

func (activation *fakeActivation) count() int {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	return len(activation.calls)
}

func (activation *fakeActivation) tenantCount() int {
	activation.mu.Lock()
	defer activation.mu.Unlock()
	return len(activation.tenantCalls)
}

// newTestProvisioningWorkspace builds the decorator under test.
func newTestProvisioningWorkspace(t *testing.T, inner kernelengine.Workspace, provisioner VolumeProvisioner, activation WorkspaceActivation) kernelengine.Workspace {
	t.Helper()
	decorated, err := newProvisioningWorkspace(inner, provisioner, activation)
	if err != nil {
		t.Fatalf("newProvisioningWorkspace: %v", err)
	}
	return decorated
}

// expectedWorkspaceVolume is the exact volume the adapter's Retain validates:
// the deterministic name and the single workspace ownership label.
func expectedWorkspaceVolume(t *testing.T, workspaceID id.WorkspaceID) runtimepodman.Volume {
	t.Helper()
	name, err := runtimepodman.WorkspaceVolumeName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceVolumeName(%q): %v", workspaceID, err)
	}
	return runtimepodman.Volume{
		Name:   name,
		Labels: map[string]string{runtimepodman.LabelWorkspace: string(workspaceID)},
	}
}

// TestFreshWorkspaceOnly pins the central safety rule: provision a volume only
// for a never-activated workspace; never recreate the lost workspace
// of an execution that already ran.
func TestFreshWorkspaceOnly(t *testing.T) {
	const workspaceID = id.WorkspaceID("workspace-fresh-1")

	t.Run("fresh workspace is provisioned exactly once then retained", func(t *testing.T) {
		log := &callLog{}
		inner := &recordingWorkspace{log: log}
		provisioner := &recordingProvisioner{log: log}
		activation := &fakeActivation{}
		decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

		if err := decorated.Retain(context.Background(), workspaceID); err != nil {
			t.Fatalf("Retain: %v", err)
		}
		if got := provisioner.count(); got != 1 {
			t.Fatalf("CreateVolume calls = %d; want exactly 1", got)
		}
		want := expectedWorkspaceVolume(t, workspaceID)
		if got := provisioner.requested(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("CreateVolume volumes = %#v; want exactly %#v", got, want)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want exactly 1", got)
		}
		if got := log.snapshot(); !reflect.DeepEqual(got, []string{"create", "retain"}) {
			t.Fatalf("effect order = %v; want the volume created before Retain is delegated", got)
		}
		if got := activation.count(); got != 1 {
			t.Fatalf("activation lookups = %d; want exactly 1", got)
		}
	})

	t.Run("already-activated workspace with a missing volume is never recreated", func(t *testing.T) {
		inner := &recordingWorkspace{
			retainErr: fault.New(fault.NotFound,
				"retained workspace volume is missing; retention never recreates it"),
		}
		provisioner := &recordingProvisioner{}
		activation := &fakeActivation{previously: map[id.WorkspaceID]bool{workspaceID: true}}
		decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

		err := decorated.Retain(context.Background(), workspaceID)
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("Retain error = %v; want not_found", err)
		}
		if got := provisioner.count(); got != 0 {
			t.Fatalf("CreateVolume calls = %d; a previously activated workspace must never be recreated", got)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want the adapter's missing-volume refusal to surface", got)
		}
	})

	t.Run("already-activated workspace with a present volume retains without provisioning", func(t *testing.T) {
		inner := &recordingWorkspace{}
		provisioner := &recordingProvisioner{}
		activation := &fakeActivation{previously: map[id.WorkspaceID]bool{workspaceID: true}}
		decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

		if err := decorated.Retain(context.Background(), workspaceID); err != nil {
			t.Fatalf("Retain: %v", err)
		}
		if got := provisioner.count(); got != 0 {
			t.Fatalf("CreateVolume calls = %d; want 0 for an already-activated workspace", got)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want 1", got)
		}
	})
}

// TestProvisioningWorkspaceCancellationBeforeEffect proves a cancelled context
// performs no external effect: no CreateVolume and no Retain.
func TestProvisioningWorkspaceCancellationBeforeEffect(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := decorated.Retain(ctx, id.WorkspaceID("workspace-cancelled"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Retain error = %v; want context.Canceled", err)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateVolume calls = %d; a cancelled Retain must not provision", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; a cancelled Retain must not delegate", got)
	}
}

// TestProvisioningWorkspaceActivationFactFailure proves the decorator fails
// closed when the durable activation fact cannot be read: no create, no retain.
func TestProvisioningWorkspaceActivationFactFailure(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{err: fault.New(fault.DependencyUnavailable,
		"workspace activation state is unreachable")}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-unknown"))
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("Retain error = %v; want dependency_unavailable", err)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateVolume calls = %d; an unknown activation fact must not provision", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; an unknown activation fact must not delegate", got)
	}
}

// TestProvisioningWorkspaceRefusesForeignVolume proves a foreign-owner refusal
// propagates as Forbidden and Retain is not called.
func TestProvisioningWorkspaceRefusesForeignVolume(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{create: func(context.Context, runtimepodman.Volume) (runtimepodman.Volume, error) {
		return runtimepodman.Volume{}, fault.New(fault.Forbidden,
			"podman volume create found an existing volume with different ownership labels")
	}}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-foreign"))
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("Retain error = %v; want forbidden", err)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; a refused provision must not be retained", got)
	}
}

// TestProvisioningWorkspaceUncertainCreateIsNotRetained proves the driver's
// ambiguous transport outcome stays uncertain and is never collapsed into a
// successful retention.
func TestProvisioningWorkspaceUncertainCreateIsNotRetained(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{create: func(context.Context, runtimepodman.Volume) (runtimepodman.Volume, error) {
		return runtimepodman.Volume{}, fmt.Errorf("%w: response was lost", podmandriver.ErrAmbiguous)
	}}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-uncertain"))
	if !errors.Is(err, podmandriver.ErrAmbiguous) {
		t.Fatalf("Retain error = %v; want the driver's ErrAmbiguous to stay reachable", err)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
		t.Fatalf("Retain error class = %v/%v; want uncertain_external_effect", class, ok)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; an uncertain create must not be treated as retained", got)
	}
}

// TestProvisioningWorkspaceIdempotentRetryAfterLostResponse proves a retry
// after a lost response provisions again through the idempotent CreateVolume
// primitive (same name and ownership label) and only then retains.
func TestProvisioningWorkspaceIdempotentRetryAfterLostResponse(t *testing.T) {
	const workspaceID = id.WorkspaceID("workspace-retry")
	inner := &recordingWorkspace{}
	attempts := 0
	provisioner := &recordingProvisioner{create: func(_ context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
		attempts++
		if attempts == 1 {
			return runtimepodman.Volume{}, fmt.Errorf("%w: response was lost", podmandriver.ErrAmbiguous)
		}
		return volume, nil
	}}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	firstErr := decorated.Retain(context.Background(), workspaceID)
	if !errors.Is(firstErr, podmandriver.ErrAmbiguous) {
		t.Fatalf("first Retain error = %v; want ErrAmbiguous", firstErr)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateVolume calls after lost response = %d; want 1", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls after lost response = %d; want 0", got)
	}

	if err := decorated.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("retry Retain: %v", err)
	}
	if got := provisioner.count(); got != 2 {
		t.Fatalf("CreateVolume calls after retry = %d; want the idempotent primitive called again", got)
	}
	want := expectedWorkspaceVolume(t, workspaceID)
	for index, volume := range provisioner.requested() {
		if !reflect.DeepEqual(volume, want) {
			t.Fatalf("CreateVolume[%d] = %#v; want the same deterministic owned volume %#v", index, volume, want)
		}
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("Retain calls after retry = %d; want exactly 1", got)
	}
}

// TestProvisioningWorkspaceAddsNoVolumeFieldsToErrors proves the decorator does
// not render the volume or its labels into an error and keeps the cause
// reachable for diagnostics.
func TestProvisioningWorkspaceAddsNoVolumeFieldsToErrors(t *testing.T) {
	const workspaceID = id.WorkspaceID("workspace-secret-free")
	cause := errors.New("volume create failed")
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{create: func(context.Context, runtimepodman.Volume) (runtimepodman.Volume, error) {
		return runtimepodman.Volume{}, cause
	}}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)

	err := decorated.Retain(context.Background(), workspaceID)
	if err == nil {
		t.Fatal("Retain succeeded despite a failed create")
	}
	if !errors.Is(err, cause) {
		t.Fatalf("Retain error = %v; want the driver cause reachable", err)
	}
	want := expectedWorkspaceVolume(t, workspaceID)
	if strings.Contains(err.Error(), runtimepodman.LabelWorkspace) {
		t.Fatalf("error renders the workspace ownership label: %v", err)
	}
	if strings.Contains(err.Error(), want.Name) {
		t.Fatalf("error renders the workspace volume name: %v", err)
	}
}

// TestProvisioningWorkspaceDelegatesStatusAndRelease proves the non-Retain
// workspace surface is untouched and never provisions.
func TestProvisioningWorkspaceDelegatesStatusAndRelease(t *testing.T) {
	inner := &recordingWorkspace{observation: kernelengine.WorkspaceObservation{Available: true}}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{}
	decorated := newTestProvisioningWorkspace(t, inner, provisioner, activation)
	workspaceID := id.WorkspaceID("workspace-delegated")

	observation, err := decorated.Status(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !observation.Available || observation.WorkspaceID != workspaceID {
		t.Fatalf("Status observation = %+v; want available for %q", observation, workspaceID)
	}
	if err := decorated.Release(context.Background(), workspaceID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := inner.statusCount(); got != 1 {
		t.Fatalf("inner Status calls = %d; want 1", got)
	}
	if got := inner.releaseCount(); got != 1 {
		t.Fatalf("inner Release calls = %d; want 1", got)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateVolume calls = %d; Status/Release must never provision", got)
	}
	if got := activation.count(); got != 0 {
		t.Fatalf("activation lookups = %d; Status/Release must not consult activation", got)
	}
}

// TestProvisioningWorkspaceRequiresDependencies proves an incomplete decorator
// is refused at construction rather than failing open later.
func TestProvisioningWorkspaceRequiresDependencies(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{}
	cases := []struct {
		name        string
		inner       kernelengine.Workspace
		provisioner VolumeProvisioner
		activation  WorkspaceActivation
	}{
		{name: "missing workspace", inner: nil, provisioner: provisioner, activation: activation},
		{name: "missing provisioner", inner: inner, provisioner: nil, activation: activation},
		{name: "missing activation fact", inner: inner, provisioner: provisioner, activation: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := newProvisioningWorkspace(testCase.inner, testCase.provisioner, testCase.activation); err == nil {
				t.Fatal("constructor accepted an incomplete decorator")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("constructor error = %v; want invalid_input", err)
			}
		})
	}
}

// TestProvisioningWorkspaceWiringForPodmanProfile proves the composition
// installs the decorator around the resolved workspace for the Podman test
// profile: a fresh Retain provisions before the adapter's Retain is reached.
func TestProvisioningWorkspaceWiringForPodmanProfile(t *testing.T) {
	config := fullTestProfileConfig(t)
	config.Installation.Environment = installation.EnvironmentTest
	config.Installation.Runtime = installation.RuntimePodman
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{}
	config.Adapters.Workspace = inner
	config.Adapters.VolumeProvisioner = provisioner
	config.Adapters.WorkspaceActivation = activation

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	workspaceID := id.WorkspaceID("workspace-wired")
	if err := environment.workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain through the wired decorator: %v", err)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateVolume calls = %d; the Podman profile must install the provisioning decorator", got)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want the decorator to delegate after provisioning", got)
	}
}

// TestProvisioningWorkspaceNotInstalledForProcessProfile proves a non-Podman
// profile is untouched even when a provisioner is supplied: the workspace
// adapter is used directly.
func TestProvisioningWorkspaceNotInstalledForProcessProfile(t *testing.T) {
	config := fullTestProfileConfig(t) // Installation.Runtime is the process adapter.
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	activation := &fakeActivation{}
	config.Adapters.Workspace = inner
	config.Adapters.VolumeProvisioner = provisioner
	config.Adapters.WorkspaceActivation = activation

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if err := environment.workspace.Retain(context.Background(), id.WorkspaceID("workspace-process")); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateVolume calls = %d; the process profile must not be wrapped", got)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want the process workspace used directly", got)
	}
}
