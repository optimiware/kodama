package app

// Fresh-only workspace claim provisioning for the Kubernetes runtime (sibling
// of the Podman provisioning in workspace_test.go).
//
// The Kubernetes runtime adapter never creates a PVC: Prepare and Retain refuse
// a missing claim so a lost workspace of an execution that already ran fails
// visibly. The Kernel calls engine.Workspace.Retain before it commits the
// triggering activation, so the composition must create the deterministic
// workspace claim before Retain for a never-activated workspace.
//
// The driver exposes the seam-external primitive
//
//	func (c *driverkube.Cluster) CreateClaim(ctx context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error)
//
// which is idempotent, adopts only an exactly-owned existing claim, refuses a
// foreign claim as Forbidden and reports an uncertain outcome as
// driverkube.ErrAmbiguous. The composition consumes it through an app-owned
// port so the app package never imports a concrete cluster.
//
// Production API under test (internal/shell/app/workspace_kube.go, config.go
// and compose.go):
//
//	type ClaimProvisioner interface {
//	    CreateClaim(ctx context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error)
//	}
//	var _ ClaimProvisioner = (*driverkube.Cluster)(nil)
//
//	// WorkspacePVC is the trusted claim template. Every value comes from
//	// composition configuration and is never derived from a request.
//	type WorkspacePVC struct {
//	    Namespace    string
//	    TenantID     id.TenantID
//	    StorageClass string
//	    Size         string
//	    AccessMode   string
//	}
//
//	Config.WorkspacePVC        *WorkspacePVC
//	Adapters.ClaimProvisioner  ClaimProvisioner
//
//	// WorkspaceActivation reports the durable owning tenant of the
//	// workspace's execution. The store-backed implementation reads
//	// executions.tenant_id; any missing or unreadable fact is an error.
//	OwningTenant(ctx context.Context, workspaceID id.WorkspaceID) (id.TenantID, error)
//
//	func newKubeProvisioningWorkspace(inner kernelengine.Workspace, provisioner ClaimProvisioner, activation WorkspaceActivation, claim WorkspacePVC) (kernelengine.Workspace, error)
//
// Decorator contract:
//   - Status and Release delegate unchanged and never provision, never consult
//     the activation fact and never read the durable tenant.
//   - A fresh workspace (PreviouslyActivated == false) checks the durable owning
//     tenant equals the trusted template's tenant, calls CreateClaim exactly
//     once with the exact spec derived from the template plus the workspace id,
//     then delegates Retain. The decorator never waits for the claim to become
//     Bound: a WaitForFirstConsumer StorageClass binds it when the Job runs.
//   - An already-activated workspace never calls CreateClaim and never reads the
//     tenant: the adapter's Retain surfaces a missing claim as not_found.
//   - A durable tenant mismatch is refused as forbidden before any external
//     effect and Retain is not called.
//   - A failed CreateClaim is returned without calling Retain. A forbidden
//     foreign claim and a terminating-claim version conflict propagate. The
//     driver's ErrAmbiguous stays reachable and is classified as
//     fault.UncertainExternalEffect.
//   - A cancelled context performs no external effect.
//   - An unknown activation or tenant fact fails closed: no create, no retain.
//   - No error renders the namespace, tenant, storage class, size, access mode
//     or derived claim name.
//
// Wiring contract: the environment builder installs the decorator only for
// ProfileDevelopment/ProfileTest when Installation.Runtime is
// installation.RuntimeKubernetes and Adapters.ClaimProvisioner is non-nil, and
// then Config.WorkspacePVC is required. Podman, Process and production are
// untouched even when both provisioners are injected. Production refuses the
// ClaimProvisioner adapter through hasDevelopmentAdapter and refuses a
// Config.WorkspacePVC template, because production claim provisioning (a
// ProductionConfig field plus cmd/** construction) is a documented non-goal.
//
// Design notes:
//   - The tenant check lives in the decorator and reads the durable execution
//     row through WorkspaceActivation.OwningTenant. The app cannot observe the
//     runtime adapter's bound namespace (the engine.Workspace seam
//     exposes no namespace), so "the template namespace equals the adapter's
//     configured namespace" is an operator configuration invariant; only the
//     durable tenant is checkable here.
//   - A nil ClaimProvisioner leaves the workspace unwrapped even when a
//     template is configured, mirroring nil VolumeProvisioner: the claims are
//     then provisioned externally (test fixtures) and Retain still fails
//     visibly for a missing claim.
//   - AccessMode must be one of the driver's four modes: ReadWriteOnce,
//     ReadOnlyMany, ReadWriteMany, ReadWriteOncePod.

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
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/internal/shell/store"
)

const (
	kubeTestTenant       = id.TenantID("tenant-1")
	kubeTestNamespace    = "tenant-namespace-1"
	kubeTestStorageClass = "standard"
	kubeTestSize         = "64Mi"
	kubeTestAccessMode   = "ReadWriteOnce"
)

// kubeTestTemplate is the trusted claim template the composition configures.
func kubeTestTemplate() WorkspacePVC {
	return WorkspacePVC{
		Namespace:    kubeTestNamespace,
		TenantID:     kubeTestTenant,
		StorageClass: kubeTestStorageClass,
		Size:         kubeTestSize,
		AccessMode:   kubeTestAccessMode,
	}
}

// expectedKubeClaimSpec is the exact request the decorator must send: every
// field from trusted configuration plus the requested workspace identity.
func expectedKubeClaimSpec(workspaceID id.WorkspaceID) driverkube.ClaimSpec {
	template := kubeTestTemplate()
	return driverkube.ClaimSpec{
		Namespace:    template.Namespace,
		TenantID:     template.TenantID,
		WorkspaceID:  workspaceID,
		StorageClass: template.StorageClass,
		Size:         template.Size,
		AccessMode:   template.AccessMode,
	}
}

// recordingClaimProvisioner is a ClaimProvisioner fake that records every
// requested spec and delegates to an optional create function.
type recordingClaimProvisioner struct {
	log *callLog

	mu     sync.Mutex
	specs  []driverkube.ClaimSpec
	create func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error)
}

func (provisioner *recordingClaimProvisioner) CreateClaim(ctx context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error) {
	provisioner.mu.Lock()
	provisioner.specs = append(provisioner.specs, spec)
	create := provisioner.create
	provisioner.mu.Unlock()
	if provisioner.log != nil {
		provisioner.log.add("create-claim")
	}
	if create != nil {
		return create(ctx, spec)
	}
	return runtimekube.Claim{Name: "unbound-workspace-claim", Namespace: spec.Namespace}, nil
}

func (provisioner *recordingClaimProvisioner) count() int {
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	return len(provisioner.specs)
}

func (provisioner *recordingClaimProvisioner) requested() []driverkube.ClaimSpec {
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	return append([]driverkube.ClaimSpec(nil), provisioner.specs...)
}

// newTestKubeProvisioningWorkspace builds the decorator under test.
func newTestKubeProvisioningWorkspace(t *testing.T, inner kernelengine.Workspace, provisioner ClaimProvisioner, activation WorkspaceActivation, template WorkspacePVC) kernelengine.Workspace {
	t.Helper()
	decorated, err := newKubeProvisioningWorkspace(inner, provisioner, activation, template)
	if err != nil {
		t.Fatalf("newKubeProvisioningWorkspace: %v", err)
	}
	return decorated
}

// kubeWiringConfig is a fully injected test composition with the Kubernetes
// runtime. The test profile does not run installation preflight, so an injected
// runtime/workspace pair is the supported shape here.
func kubeWiringConfig(t *testing.T) Config {
	t.Helper()
	config := fullTestProfileConfig(t)
	config.Installation.Environment = installation.EnvironmentTest
	config.Installation.Runtime = installation.RuntimeKubernetes
	return config
}

// TestKubernetesFreshWorkspaceOnly pins the central safety rule: create the
// workspace claim only for a never-activated workspace; never
// recreate the lost claim of an execution that already ran.
func TestKubernetesFreshWorkspaceOnly(t *testing.T) {
	const workspaceID = id.WorkspaceID("workspace-kube-fresh-1")

	t.Run("fresh workspace is provisioned exactly once then retained", func(t *testing.T) {
		log := &callLog{}
		inner := &recordingWorkspace{log: log}
		provisioner := &recordingClaimProvisioner{log: log}
		activation := &fakeActivation{tenant: kubeTestTenant}
		decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

		if err := decorated.Retain(context.Background(), workspaceID); err != nil {
			t.Fatalf("Retain: %v", err)
		}
		if got := provisioner.count(); got != 1 {
			t.Fatalf("CreateClaim calls = %d; want exactly 1", got)
		}
		want := expectedKubeClaimSpec(workspaceID)
		if got := provisioner.requested(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("CreateClaim specs = %#v; want exactly %#v", got, want)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want exactly 1", got)
		}
		if got := log.snapshot(); !reflect.DeepEqual(got, []string{"create-claim", "retain"}) {
			t.Fatalf("effect order = %v; want the claim created before Retain is delegated", got)
		}
		if got := activation.count(); got != 1 {
			t.Fatalf("activation lookups = %d; want exactly 1", got)
		}
		if got := activation.tenantCount(); got != 1 {
			t.Fatalf("tenant lookups = %d; want exactly 1", got)
		}
	})

	t.Run("already-activated workspace with a missing claim is never recreated", func(t *testing.T) {
		inner := &recordingWorkspace{
			retainErr: fault.New(fault.NotFound,
				"retained workspace claim is missing; it is never recreated"),
		}
		provisioner := &recordingClaimProvisioner{}
		activation := &fakeActivation{
			previously: map[id.WorkspaceID]bool{workspaceID: true},
			tenant:     kubeTestTenant,
		}
		decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

		err := decorated.Retain(context.Background(), workspaceID)
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("Retain error = %v; want not_found", err)
		}
		if got := provisioner.count(); got != 0 {
			t.Fatalf("CreateClaim calls = %d; a previously activated workspace must never be recreated", got)
		}
		if got := activation.tenantCount(); got != 0 {
			t.Fatalf("tenant lookups = %d; an already-activated workspace must not read the tenant", got)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want the adapter's missing-claim refusal to surface", got)
		}
	})

	t.Run("already-activated workspace with a present claim retains without provisioning", func(t *testing.T) {
		inner := &recordingWorkspace{}
		provisioner := &recordingClaimProvisioner{}
		activation := &fakeActivation{
			previously: map[id.WorkspaceID]bool{workspaceID: true},
			tenant:     kubeTestTenant,
		}
		decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

		if err := decorated.Retain(context.Background(), workspaceID); err != nil {
			t.Fatalf("Retain: %v", err)
		}
		if got := provisioner.count(); got != 0 {
			t.Fatalf("CreateClaim calls = %d; want 0 for an already-activated workspace", got)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("Retain calls = %d; want 1", got)
		}
	})
}

// TestKubernetesProvisioningDoesNotWaitForClaimBinding proves the decorator
// treats the create response as a provisioned claim and delegates Retain
// immediately. A WaitForFirstConsumer StorageClass only binds the claim when
// the workload Job runs, so waiting for Bound would deadlock activation.
func TestKubernetesProvisioningDoesNotWaitForClaimBinding(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{create: func(_ context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error) {
		// A freshly created claim has no binding evidence: no resource version,
		// no annotations, no deletion timestamp.
		return runtimekube.Claim{Name: "kodama-workspace-unbound", Namespace: spec.Namespace}, nil
	}}
	activation := &fakeActivation{tenant: kubeTestTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

	if err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-kube-unbound")); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateClaim calls = %d; want exactly 1 with no polling", got)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("Retain calls = %d; want the claim retained without waiting for binding", got)
	}
}

// TestKubernetesProvisioningCancellationBeforeEffect proves a cancelled context
// performs no external effect and reads no fact.
func TestKubernetesProvisioningCancellationBeforeEffect(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := decorated.Retain(ctx, id.WorkspaceID("workspace-kube-cancelled"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Retain error = %v; want context.Canceled", err)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateClaim calls = %d; a cancelled Retain must not provision", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; a cancelled Retain must not delegate", got)
	}
	if got := activation.count(); got != 0 {
		t.Fatalf("activation lookups = %d; a cancelled Retain must not read facts", got)
	}
}

// TestKubernetesProvisioningUnknownFactsFailClosed proves an unreadable
// activation or tenant fact never provisions and never retains.
func TestKubernetesProvisioningUnknownFactsFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		activation *fakeActivation
	}{
		{
			name: "activation fact is unreadable",
			activation: &fakeActivation{
				tenant: kubeTestTenant,
				err: fault.New(fault.DependencyUnavailable,
					"workspace activation state is unreachable"),
			},
		},
		{
			name: "tenant fact is unreadable",
			activation: &fakeActivation{
				tenant: kubeTestTenant,
				tenantErr: fault.New(fault.DependencyUnavailable,
					"workspace execution tenant is unreachable"),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inner := &recordingWorkspace{}
			provisioner := &recordingClaimProvisioner{}
			decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, testCase.activation, kubeTestTemplate())

			err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-kube-unknown"))
			if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
				t.Fatalf("Retain error = %v; want dependency_unavailable", err)
			}
			if got := provisioner.count(); got != 0 {
				t.Fatalf("CreateClaim calls = %d; an unknown fact must not provision", got)
			}
			if got := inner.retainCount(); got != 0 {
				t.Fatalf("Retain calls = %d; an unknown fact must not delegate", got)
			}
		})
	}
}

// TestKubernetesProvisioningTenantMismatchRefused proves a durable execution
// tenant that differs from the trusted template tenant is refused before any
// external effect, and no configured value reaches the error.
func TestKubernetesProvisioningTenantMismatchRefused(t *testing.T) {
	const foreignTenant = id.TenantID("tenant-foreign")
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: foreignTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

	err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-kube-tenant"))
	if err == nil {
		t.Fatal("Retain succeeded for a workspace owned by another tenant")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
		t.Fatalf("Retain error = %v; want forbidden", err)
	}
	if got := provisioner.count(); got != 0 {
		t.Fatalf("CreateClaim calls = %d; a tenant mismatch must not provision", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; a tenant mismatch must not delegate", got)
	}
	for _, forbidden := range []string{string(foreignTenant), string(kubeTestTenant), kubeTestNamespace} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error renders a tenant or namespace value %q: %v", forbidden, err)
		}
	}
}

// TestKubernetesProvisioningRefusalsDoNotRetain proves a foreign claim and a
// terminating-claim conflict propagate and are never collapsed into a retained
// workspace.
func TestKubernetesProvisioningRefusalsDoNotRetain(t *testing.T) {
	cases := []struct {
		name   string
		create func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error)
		class  fault.Class
		cause  error
	}{
		{
			name: "foreign claim is forbidden",
			create: func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error) {
				return runtimekube.Claim{}, fault.New(fault.Forbidden,
					"kubernetes workspace claim is already held by another owner")
			},
			class: fault.Forbidden,
		},
		{
			name: "terminating claim is a version conflict",
			create: func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error) {
				return runtimekube.Claim{}, fault.Wrap(fault.VersionConflict,
					"kubernetes workspace claim is terminating and cannot be adopted",
					runtimekube.ErrConflict)
			},
			class: fault.VersionConflict,
			cause: runtimekube.ErrConflict,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inner := &recordingWorkspace{}
			provisioner := &recordingClaimProvisioner{create: testCase.create}
			activation := &fakeActivation{tenant: kubeTestTenant}
			decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

			err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-kube-refused"))
			if class, ok := fault.ClassOf(err); !ok || class != testCase.class {
				t.Fatalf("Retain error = %v; want class %v", err, testCase.class)
			}
			if testCase.cause != nil && !errors.Is(err, testCase.cause) {
				t.Fatalf("Retain error = %v; want cause %v reachable", err, testCase.cause)
			}
			if got := inner.retainCount(); got != 0 {
				t.Fatalf("Retain calls = %d; a refused provision must not be retained", got)
			}
		})
	}
}

// TestKubernetesProvisioningUncertainCreateIsNotRetained proves the driver's
// ambiguous transport outcome stays uncertain and never becomes a successful
// retention.
func TestKubernetesProvisioningUncertainCreateIsNotRetained(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{create: func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error) {
		return runtimekube.Claim{}, fmt.Errorf("%w: response was lost", driverkube.ErrAmbiguous)
	}}
	activation := &fakeActivation{tenant: kubeTestTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

	err := decorated.Retain(context.Background(), id.WorkspaceID("workspace-kube-uncertain"))
	if !errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("Retain error = %v; want the driver's ErrAmbiguous to stay reachable", err)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.UncertainExternalEffect {
		t.Fatalf("Retain error class = %v/%v; want uncertain_external_effect", class, ok)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls = %d; an uncertain create must not be treated as retained", got)
	}
}

// TestKubernetesProvisioningIdempotentRetryAfterLostResponse proves a retry
// after a lost response sends the identical spec through the idempotent
// CreateClaim primitive and only then retains.
func TestKubernetesProvisioningIdempotentRetryAfterLostResponse(t *testing.T) {
	const workspaceID = id.WorkspaceID("workspace-kube-retry")
	inner := &recordingWorkspace{}
	attempts := 0
	provisioner := &recordingClaimProvisioner{create: func(_ context.Context, spec driverkube.ClaimSpec) (runtimekube.Claim, error) {
		attempts++
		if attempts == 1 {
			return runtimekube.Claim{}, fmt.Errorf("%w: response was lost", driverkube.ErrAmbiguous)
		}
		return runtimekube.Claim{Name: "kodama-workspace-retry", Namespace: spec.Namespace}, nil
	}}
	activation := &fakeActivation{tenant: kubeTestTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())

	firstErr := decorated.Retain(context.Background(), workspaceID)
	if !errors.Is(firstErr, driverkube.ErrAmbiguous) {
		t.Fatalf("first Retain error = %v; want ErrAmbiguous", firstErr)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateClaim calls after lost response = %d; want 1", got)
	}
	if got := inner.retainCount(); got != 0 {
		t.Fatalf("Retain calls after lost response = %d; want 0", got)
	}

	if err := decorated.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("retry Retain: %v", err)
	}
	if got := provisioner.count(); got != 2 {
		t.Fatalf("CreateClaim calls after retry = %d; want the idempotent primitive called again", got)
	}
	want := expectedKubeClaimSpec(workspaceID)
	for index, spec := range provisioner.requested() {
		if !reflect.DeepEqual(spec, want) {
			t.Fatalf("CreateClaim[%d] = %#v; want the same deterministic spec %#v", index, spec, want)
		}
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("Retain calls after retry = %d; want exactly 1", got)
	}
}

// TestKubernetesProvisioningAddsNoClaimFieldsToErrors proves the decorator does
// not render the template or the derived claim name into an error and keeps the
// driver cause reachable.
func TestKubernetesProvisioningAddsNoClaimFieldsToErrors(t *testing.T) {
	template := WorkspacePVC{
		Namespace:    "namespace-detail-9",
		TenantID:     "tenant-detail-9",
		StorageClass: "class-detail-9",
		Size:         "999Mi",
		AccessMode:   "ReadWriteMany",
	}
	const workspaceID = id.WorkspaceID("workspace-kube-detail-free")
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	cause := errors.New("claim create failed")
	cases := []struct {
		name   string
		create func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error)
		cause  error
	}{
		{
			name: "plain driver cause",
			create: func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error) {
				return runtimekube.Claim{}, cause
			},
			cause: cause,
		},
		{
			name: "ambiguous outcome",
			create: func(context.Context, driverkube.ClaimSpec) (runtimekube.Claim, error) {
				return runtimekube.Claim{}, fmt.Errorf("%w: response was lost", driverkube.ErrAmbiguous)
			},
			cause: driverkube.ErrAmbiguous,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inner := &recordingWorkspace{}
			provisioner := &recordingClaimProvisioner{create: testCase.create}
			activation := &fakeActivation{tenant: template.TenantID}
			decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, template)

			err := decorated.Retain(context.Background(), workspaceID)
			if err == nil {
				t.Fatal("Retain succeeded despite a failed create")
			}
			if !errors.Is(err, testCase.cause) {
				t.Fatalf("Retain error = %v; want the driver cause reachable", err)
			}
			for _, forbidden := range []string{
				template.Namespace,
				string(template.TenantID),
				template.StorageClass,
				template.Size,
				template.AccessMode,
				claimName,
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error renders claim detail %q: %v", forbidden, err)
				}
			}
		})
	}
}

// TestKubernetesProvisioningStatusAndReleaseNeverProvision proves the non-Retain
// workspace surface is untouched and consults no fact.
func TestKubernetesProvisioningStatusAndReleaseNeverProvision(t *testing.T) {
	inner := &recordingWorkspace{observation: kernelengine.WorkspaceObservation{Available: true}}
	provisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	decorated := newTestKubeProvisioningWorkspace(t, inner, provisioner, activation, kubeTestTemplate())
	workspaceID := id.WorkspaceID("workspace-kube-delegated")

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
		t.Fatalf("CreateClaim calls = %d; Status/Release must never provision", got)
	}
	if got := activation.count(); got != 0 {
		t.Fatalf("activation lookups = %d; Status/Release must not consult activation", got)
	}
	if got := activation.tenantCount(); got != 0 {
		t.Fatalf("tenant lookups = %d; Status/Release must not read the tenant", got)
	}
}

// TestKubernetesProvisioningRequiresDependencies proves an incomplete decorator
// or an invalid trusted template is refused at construction rather than failing
// open later.
func TestKubernetesProvisioningRequiresDependencies(t *testing.T) {
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	valid := kubeTestTemplate()
	cases := []struct {
		name        string
		inner       kernelengine.Workspace
		provisioner ClaimProvisioner
		activation  WorkspaceActivation
		template    WorkspacePVC
	}{
		{name: "missing workspace", inner: nil, provisioner: provisioner, activation: activation, template: valid},
		{name: "missing provisioner", inner: inner, provisioner: nil, activation: activation, template: valid},
		{name: "missing activation fact", inner: inner, provisioner: provisioner, activation: nil, template: valid},
		{name: "empty namespace", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.Namespace = "" })},
		{name: "empty storage class", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.StorageClass = "  " })},
		{name: "empty size", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.Size = "" })},
		{name: "empty access mode", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.AccessMode = "" })},
		{name: "invalid tenant", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.TenantID = "tenant bad" })},
		{name: "unsupported access mode", inner: inner, provisioner: provisioner, activation: activation, template: withKubeTemplate(func(template *WorkspacePVC) { template.AccessMode = "ReadWriteSometimes" })},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := newKubeProvisioningWorkspace(testCase.inner, testCase.provisioner, testCase.activation, testCase.template)
			if err == nil {
				t.Fatal("constructor accepted an incomplete decorator or an invalid template")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("constructor error = %v; want invalid_input", err)
			}
		})
	}
}

// withKubeTemplate copies the valid template and applies one mutation.
func withKubeTemplate(mutate func(*WorkspacePVC)) WorkspacePVC {
	template := kubeTestTemplate()
	mutate(&template)
	return template
}

// TestKubernetesClaimProvisioningWiringForTestProfile proves the composition
// installs the decorator around the resolved workspace for the Kubernetes test
// profile: a fresh Retain creates the claim before the adapter's Retain.
func TestKubernetesClaimProvisioningWiringForTestProfile(t *testing.T) {
	config := kubeWiringConfig(t)
	inner := &recordingWorkspace{}
	provisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	template := kubeTestTemplate()
	config.Adapters.Workspace = inner
	config.Adapters.ClaimProvisioner = provisioner
	config.Adapters.WorkspaceActivation = activation
	config.WorkspacePVC = &template

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	workspaceID := id.WorkspaceID("workspace-kube-wired")
	if err := environment.workspace.Retain(context.Background(), workspaceID); err != nil {
		t.Fatalf("Retain through the wired decorator: %v", err)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateClaim calls = %d; the Kubernetes profile must install the claim decorator", got)
	}
	want := expectedKubeClaimSpec(workspaceID)
	if got := provisioner.requested(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("CreateClaim specs = %#v; want exactly %#v", got, want)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want the decorator to delegate after provisioning", got)
	}
}

// TestKubernetesClaimProvisioningWiringRequiresTemplate proves an injected
// provisioner without trusted claim configuration refuses startup instead of
// sending a request with empty storage fields.
func TestKubernetesClaimProvisioningWiringRequiresTemplate(t *testing.T) {
	config := kubeWiringConfig(t)
	config.Adapters.Workspace = &recordingWorkspace{}
	config.Adapters.ClaimProvisioner = &recordingClaimProvisioner{}
	config.Adapters.WorkspaceActivation = &fakeActivation{tenant: kubeTestTenant}
	config.WorkspacePVC = nil

	_, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err == nil {
		t.Fatal("buildEnvironment accepted a claim provisioner without a claim template")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("buildEnvironment error = %v; want invalid_input", err)
	}
	if !strings.Contains(err.Error(), "workspace claim template") {
		t.Fatalf("refusal = %q; want it to name the missing workspace claim template", err.Error())
	}
}

// TestKubernetesClaimProvisioningNotInstalledWithoutProvisioner proves a
// configured template alone does not wrap the workspace: claims are then
// provisioned externally and Retain keeps failing visibly for a missing claim.
func TestKubernetesClaimProvisioningNotInstalledWithoutProvisioner(t *testing.T) {
	config := kubeWiringConfig(t)
	inner := &recordingWorkspace{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	template := kubeTestTemplate()
	config.Adapters.Workspace = inner
	config.Adapters.WorkspaceActivation = activation
	config.Adapters.ClaimProvisioner = nil
	config.WorkspacePVC = &template

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if err := environment.workspace.Retain(context.Background(), id.WorkspaceID("workspace-kube-external")); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want the workspace used directly", got)
	}
	if got := activation.count(); got != 0 {
		t.Fatalf("activation lookups = %d; a nil provisioner must leave the workspace unwrapped", got)
	}
}

// TestKubernetesClaimProvisioningNotInstalledForPodmanProfile proves the Podman
// profile keeps its volume decorator and never creates a Kubernetes claim even
// when both provisioners and a template are supplied.
func TestKubernetesClaimProvisioningNotInstalledForPodmanProfile(t *testing.T) {
	config := kubeWiringConfig(t)
	config.Installation.Runtime = installation.RuntimePodman
	inner := &recordingWorkspace{}
	volumeProvisioner := &recordingProvisioner{}
	claimProvisioner := &recordingClaimProvisioner{}
	activation := &fakeActivation{tenant: kubeTestTenant}
	template := kubeTestTemplate()
	config.Adapters.Workspace = inner
	config.Adapters.VolumeProvisioner = volumeProvisioner
	config.Adapters.WorkspaceActivation = activation
	config.Adapters.ClaimProvisioner = claimProvisioner
	config.WorkspacePVC = &template

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if err := environment.workspace.Retain(context.Background(), id.WorkspaceID("workspace-podman-both")); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := volumeProvisioner.count(); got != 1 {
		t.Fatalf("CreateVolume calls = %d; the Podman decorator must still provision", got)
	}
	if got := claimProvisioner.count(); got != 0 {
		t.Fatalf("CreateClaim calls = %d; the Podman profile must never create a Kubernetes claim", got)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want 1", got)
	}
}

// TestKubernetesClaimProvisioningNotInstalledForProcessProfile proves the
// process profile wraps nothing even when both provisioners and a template are
// supplied.
func TestKubernetesClaimProvisioningNotInstalledForProcessProfile(t *testing.T) {
	config := kubeWiringConfig(t)
	config.Installation.Runtime = installation.RuntimeProcess
	inner := &recordingWorkspace{}
	volumeProvisioner := &recordingProvisioner{}
	claimProvisioner := &recordingClaimProvisioner{}
	template := kubeTestTemplate()
	config.Adapters.Workspace = inner
	config.Adapters.VolumeProvisioner = volumeProvisioner
	config.Adapters.ClaimProvisioner = claimProvisioner
	config.WorkspacePVC = &template

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if err := environment.workspace.Retain(context.Background(), id.WorkspaceID("workspace-process-both")); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got := volumeProvisioner.count(); got != 0 {
		t.Fatalf("CreateVolume calls = %d; the process profile must not be wrapped", got)
	}
	if got := claimProvisioner.count(); got != 0 {
		t.Fatalf("CreateClaim calls = %d; the process profile must not be wrapped", got)
	}
	if got := inner.retainCount(); got != 1 {
		t.Fatalf("inner Retain calls = %d; want the process workspace used directly", got)
	}
}

// TestKubernetesClaimProvisioningUsesDurableTenantByDefault proves the wired
// decorator takes the tenant from the persisted execution row and refuses a
// workspace whose durable tenant differs from the trusted template.
func TestKubernetesClaimProvisioningUsesDurableTenantByDefault(t *testing.T) {
	freshWorkspace := id.WorkspaceID("ws-kube-wiring-fresh")
	freshExecution := id.ExecutionID("execution-kube-wiring-fresh")

	build := func(t *testing.T, tenant id.TenantID) (Config, *recordingWorkspace, *recordingClaimProvisioner) {
		t.Helper()
		config := kubeWiringConfig(t)
		inner := &recordingWorkspace{}
		provisioner := &recordingClaimProvisioner{}
		template := kubeTestTemplate()
		config.Adapters.Workspace = inner
		config.Adapters.ClaimProvisioner = provisioner
		config.Adapters.Controller = &activationFactsStore{rows: []store.Row{
			{Table: "executions", Values: map[string]any{
				"id":           string(freshExecution),
				"workspace_id": string(freshWorkspace),
				"tenant_id":    string(tenant),
			}},
			activationActionRow("act-kube-wiring-fresh-0", freshExecution, 0, kernel.ActionPending),
		}}
		config.Adapters.WorkspaceActivation = nil
		config.WorkspacePVC = &template
		return config, inner, provisioner
	}

	t.Run("durable tenant matches the template", func(t *testing.T) {
		config, inner, provisioner := build(t, kubeTestTenant)
		environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
		if err != nil {
			t.Fatalf("buildEnvironment: %v", err)
		}
		if err := environment.workspace.Retain(context.Background(), freshWorkspace); err != nil {
			t.Fatalf("fresh Retain: %v", err)
		}
		if got := provisioner.count(); got != 1 {
			t.Fatalf("CreateClaim calls = %d; want 1", got)
		}
		if got := provisioner.requested()[0]; got.TenantID != kubeTestTenant {
			t.Fatalf("CreateClaim tenant = %q; want the durable tenant %q", got.TenantID, kubeTestTenant)
		}
		if got := inner.retainCount(); got != 1 {
			t.Fatalf("inner Retain calls = %d; want 1", got)
		}
	})

	t.Run("durable tenant mismatch is refused", func(t *testing.T) {
		config, inner, provisioner := build(t, id.TenantID("tenant-other"))
		environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
		if err != nil {
			t.Fatalf("buildEnvironment: %v", err)
		}
		err = environment.workspace.Retain(context.Background(), freshWorkspace)
		if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
			t.Fatalf("Retain error = %v; want forbidden", err)
		}
		if got := provisioner.count(); got != 0 {
			t.Fatalf("CreateClaim calls = %d; a durable tenant mismatch must not provision", got)
		}
		if got := inner.retainCount(); got != 0 {
			t.Fatalf("Retain calls = %d; a durable tenant mismatch must not delegate", got)
		}
	})
}

// TestWorkspacePVCConfigValidation pins the Config.WorkspacePVC validation
// matrix: an unset template is fine, a complete template is fine, and every
// incomplete or unusable field refuses startup before any adapter exists.
func TestWorkspacePVCConfigValidation(t *testing.T) {
	cases := []struct {
		name     string
		template *WorkspacePVC
		want     string
	}{
		{name: "unset template", template: nil},
		{name: "valid template", template: func() *WorkspacePVC { template := kubeTestTemplate(); return &template }()},
		{name: "empty namespace", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.Namespace = "" }), want: "namespace"},
		{name: "blank namespace", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.Namespace = "   " }), want: "namespace"},
		{name: "empty storage class", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.StorageClass = "" }), want: "storage class"},
		{name: "empty size", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.Size = "" }), want: "storage size"},
		{name: "empty access mode", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.AccessMode = "" }), want: "access mode"},
		{name: "invalid tenant", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.TenantID = "tenant bad" }), want: "tenant"},
		{name: "unsupported access mode", template: kubeTemplatePointer(func(template *WorkspacePVC) { template.AccessMode = "ReadWriteSometimes" }), want: "access mode"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := kubeWiringConfig(t)
			config.WorkspacePVC = testCase.template
			err := config.resolved().validate()
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("validate refused a usable workspace claim template: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validate accepted an unusable workspace claim template")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("validate error = %v; want invalid_input", err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

// kubeTemplatePointer copies the valid template, applies one mutation and
// returns a pointer for Config.WorkspacePVC.
func kubeTemplatePointer(mutate func(*WorkspacePVC)) *WorkspacePVC {
	template := withKubeTemplate(mutate)
	return &template
}
