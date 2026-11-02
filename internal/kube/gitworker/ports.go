package gitworker

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Cluster is the Kubernetes API surface the reconciler uses. *kubernetes.Client
// implements it; tests use an in-memory cluster. Every mutation that fails with
// kubernetes.ErrUnknownOutcome is followed by a read, never by an assumption.
type Cluster interface {
	GetStorageClass(ctx context.Context, name string) (kubernetes.StorageClass, error)
	GetPV(ctx context.Context, name string) (kubernetes.PersistentVolume, error)

	GetNamespace(ctx context.Context, name string) (kubernetes.Namespace, error)
	CreateNamespace(ctx context.Context, n kubernetes.Namespace) (kubernetes.Namespace, error)
	ReplaceNamespace(ctx context.Context, n kubernetes.Namespace) (kubernetes.Namespace, error)
	DeleteNamespace(ctx context.Context, name string, o kubernetes.DeleteOptions) error

	GetServiceAccount(ctx context.Context, ns, name string) (kubernetes.ServiceAccount, error)
	CreateServiceAccount(ctx context.Context, ns string, s kubernetes.ServiceAccount) (kubernetes.ServiceAccount, error)

	GetPVC(ctx context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error)
	CreatePVC(ctx context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error)

	GetSecret(ctx context.Context, ns, name string) (kubernetes.Secret, error)
	CreateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)
	UpdateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)

	GetService(ctx context.Context, ns, name string) (kubernetes.Service, error)
	CreateService(ctx context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error)

	GetNetworkPolicy(ctx context.Context, ns, name string) (kubernetes.NetworkPolicy, error)
	CreateNetworkPolicy(ctx context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error)
	ReplaceNetworkPolicy(ctx context.Context, ns string, p kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error)
	ListNetworkPolicies(ctx context.Context, ns string) ([]kubernetes.NetworkPolicy, error)

	GetDeployment(ctx context.Context, ns, name string) (kubernetes.Deployment, error)
	CreateDeployment(ctx context.Context, ns string, d kubernetes.Deployment) (kubernetes.Deployment, error)
	ReplaceDeployment(ctx context.Context, ns string, d kubernetes.Deployment) (kubernetes.Deployment, error)

	ListReplicaSets(ctx context.Context, ns, selector string) ([]kubernetes.ReplicaSet, error)
	ListPods(ctx context.Context, ns, selector string) ([]kubernetes.Pod, error)
}

var _ Cluster = (*kubernetes.Client)(nil)

// Material is the identity a worker holds toward the control plane (S3, T-136):
// an issued token bound to one tenant (audience kodama-git-control), the public
// keys that verify forwarding assertions, and the CA that anchors the control
// plane. It never contains a signing key; a Material that does is refused.
type Material struct {
	// TenantID is the tenant the token is bound to; it must be the tenant the
	// worker is provisioned for.
	TenantID domain.TenantID
	// Token is the issued, short-lived worker identity token.
	Token []byte
	// ExpiresAt is the token's expiry. A token that has expired is not delivered.
	ExpiresAt time.Time
	// PublicKeys are the public verification keys of forwarding assertions.
	PublicKeys []byte
	// CA is the PEM trust anchor of the control plane.
	CA []byte
}

// IdentityProvider issues a tenant's worker identity material. It is implemented
// by the Git authority (T-136) and may return the same material until it is near
// expiry; the reconciler writes the Secret only when the material differs.
type IdentityProvider interface {
	WorkerIdentity(ctx context.Context, tenant domain.TenantID) (Material, error)
}

// Retention decides whether a tenant's volume may be deleted. It is consulted
// only by OffboardTenant, and any error or false answer refuses the deletion.
type Retention interface {
	// MayDeleteVolume reports true only when the tenant's retention allows the
	// deletion of its repositories' storage now (the tenant is offboarded, its
	// grants are revoked, its push records are settled and no hold applies).
	MayDeleteVolume(ctx context.Context, tenant domain.TenantID) (bool, error)
}
