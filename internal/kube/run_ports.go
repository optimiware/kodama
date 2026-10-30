package kube

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// RunCluster is the Kubernetes surface of the run-resource adapter and of
// run_shared attempts: the executor's Cluster plus claims, volumes (read only),
// volume attachments (read only), Services, Pod creation and NetworkPolicies.
// *kubernetes.Client implements it.
type RunCluster interface {
	Cluster

	GetPVC(ctx context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error)
	CreatePVC(ctx context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error)
	DeletePVC(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error

	GetPV(ctx context.Context, name string) (kubernetes.PersistentVolume, error)
	ListPVs(ctx context.Context) ([]kubernetes.PersistentVolume, error)
	GetStorageClass(ctx context.Context, name string) (kubernetes.StorageClass, error)
	ListVolumeAttachments(ctx context.Context) ([]kubernetes.VolumeAttachment, error)

	GetService(ctx context.Context, ns, name string) (kubernetes.Service, error)
	CreateService(ctx context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error)
	DeleteService(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error

	CreatePod(ctx context.Context, ns string, p kubernetes.Pod) (kubernetes.Pod, error)

	GetNetworkPolicy(ctx context.Context, ns, name string) (kubernetes.NetworkPolicy, error)
	SelectNetworkPolicies(ctx context.Context, ns, selector string) ([]kubernetes.NetworkPolicy, error)
	CreateNetworkPolicy(ctx context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error)
	DeleteNetworkPolicy(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
}

var _ RunCluster = (*kubernetes.Client)(nil)

// PolicyCatalog is the installation's retained execution policies (seam S9,
// append-only): the adapter resolves a run's pinned {id, digest} to the body it
// verifies every object against. The pin, not the installation's current policy,
// decides. A missing pin is ErrPolicyUnavailable; any other error is an outage.
// Implementations return the body the digest names and the adapter re-verifies
// that.
type PolicyCatalog interface {
	Policy(ctx context.Context, pin execution.PolicyPin) (execution.ExecutionPolicy, error)
}

// ErrPolicyUnavailable: the pinned execution policy is not in the catalog.
var ErrPolicyUnavailable = errors.New("kube: the pinned execution policy is unavailable")

// ResourceRegisterCommand registers a resource's admission credential with
// runtime admission (seam S5). It is the adapter-local shape of
// runtimeauth.ResourceRegisterCommand; T-116 bridges the two.
type ResourceRegisterCommand struct {
	Scope           domain.Scope
	RunID           domain.RunID
	Key             string
	Credential      runtimeauth.Credential
	SpecFingerprint string
	// ExpiresAt is the resource's deadline: the credential is never valid beyond it.
	ExpiresAt time.Time
}

// ResourceRegistrar registers resource admission credentials. RegisterResource is
// idempotent for the same credential. A different credential for the same
// resource is ErrRegistrationConflict and a resource that is stopping, revoked
// or past its deadline ErrRegistrationRevoked; every other error is an outage and
// the registration is repeated with the same credential.
type ResourceRegistrar interface {
	RegisterResource(ctx context.Context, cmd ResourceRegisterCommand) error
}

// Registrar failures a ResourceRegistrar returns.
var (
	ErrRegistrationConflict = errors.New("kube: a different registration exists for the resource")
	ErrRegistrationRevoked  = errors.New("kube: the resource is no longer authorized")
)

// HostResolver resolves a named egress destination host to addresses when a
// profile names a host rather than an address block. It is optional: without it
// a hostname destination is refused, never silently allowed. Implementations
// resolve through the installation's resolver and return addresses only.
type HostResolver interface {
	ResolveHost(ctx context.Context, host string) ([]netip.Addr, error)
}
