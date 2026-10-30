package kernelrt

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Cluster is the Kubernetes surface this adapter needs: get, create,
// conditional update of its record Secrets, UID-preconditioned delete, and
// read-only inspection of the namespace, volumes and policies. It is owned
// here and implemented by *kubernetes.Client.
type Cluster interface {
	GetNamespace(ctx context.Context, name string) (kubernetes.Namespace, error)
	GetServiceAccount(ctx context.Context, ns, name string) (kubernetes.ServiceAccount, error)
	ListNetworkPolicies(ctx context.Context, ns string) ([]kubernetes.NetworkPolicy, error)

	GetJob(ctx context.Context, ns, name string) (kubernetes.Job, error)
	CreateJob(ctx context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error)
	DeleteJob(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error

	ListPods(ctx context.Context, ns, selector string) ([]kubernetes.Pod, error)
	GetPod(ctx context.Context, ns, name string) (kubernetes.Pod, error)
	CreatePod(ctx context.Context, ns string, p kubernetes.Pod) (kubernetes.Pod, error)
	DeletePod(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
	PodLog(ctx context.Context, ns, pod, container string) ([]byte, error)

	GetSecret(ctx context.Context, ns, name string) (kubernetes.Secret, error)
	CreateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)
	UpdateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)
	DeleteSecret(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
	ListSecrets(ctx context.Context, ns, selector string) ([]kubernetes.Secret, error)

	GetPVC(ctx context.Context, ns, name string) (kubernetes.PersistentVolumeClaim, error)
	CreatePVC(ctx context.Context, ns string, p kubernetes.PersistentVolumeClaim) (kubernetes.PersistentVolumeClaim, error)
	DeletePVC(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
	ListPVs(ctx context.Context) ([]kubernetes.PersistentVolume, error)
	GetStorageClass(ctx context.Context, name string) (kubernetes.StorageClass, error)
	ListVolumeAttachments(ctx context.Context) ([]kubernetes.VolumeAttachment, error)

	GetService(ctx context.Context, ns, name string) (kubernetes.Service, error)
	CreateService(ctx context.Context, ns string, s kubernetes.Service) (kubernetes.Service, error)
	DeleteService(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
}

var _ Cluster = (*kubernetes.Client)(nil)

// Target is where one scope's objects live. Tenant authority is decided by the
// installation here, never inferred from a request.
type Target struct {
	Namespace      string
	ServiceAccount string
}

// Targets resolves the namespace and service account of a scope. An error
// means the scope has no target; nothing is created for it.
type Targets interface {
	Target(ctx context.Context, scope domain.Scope) (Target, error)
}

type singleTarget Target

func (s singleTarget) Target(context.Context, domain.Scope) (Target, error) {
	return Target(s), nil
}

// SingleTarget serves every scope from one namespace. Object names and
// annotations still carry the scope, so scopes cannot adopt each other's
// objects.
func SingleTarget(t Target) Targets { return singleTarget(t) }

// SecretFiles supplies the files of one step's declared secrets. The kernel's
// Materialization names a private directory and the references it holds, never
// a value; in Kubernetes the adapter turns the returned files into an
// immutable Secret mounted read-only at Materialization.Directory. It must not
// log or return values in errors. Nil in Dependencies means a step that
// declares secrets is refused.
type SecretFiles interface {
	Files(ctx context.Context, m runtime.Materialization) (map[string][]byte, error)
}

// Dependencies are the collaborators of the adapter.
type Dependencies struct {
	Cluster Cluster
	Targets Targets
	Secrets SecretFiles
}

// Bounds are the CPU, memory and ephemeral-storage requests and limits of one
// container (Kubernetes quantity strings). All are required: an unbounded
// container is never created.
type Bounds struct {
	CPURequest, CPULimit             string
	MemoryRequest, MemoryLimit       string
	EphemeralRequest, EphemeralLimit string
}

var quantityRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|M|G|T|Ki|Mi|Gi|Ti)?$`)

func (b Bounds) validate(what string) error {
	for name, v := range map[string]string{
		"cpu request": b.CPURequest, "cpu limit": b.CPULimit,
		"memory request": b.MemoryRequest, "memory limit": b.MemoryLimit,
		"ephemeral request": b.EphemeralRequest, "ephemeral limit": b.EphemeralLimit,
	} {
		if !quantityRE.MatchString(v) {
			return fmt.Errorf("kernelrt: %s %s %q is not a quantity", what, name, v)
		}
	}
	return nil
}

func (b Bounds) resources() *kubernetes.Resources {
	return &kubernetes.Resources{
		Requests: map[string]string{"cpu": b.CPURequest, "memory": b.MemoryRequest, "ephemeral-storage": b.EphemeralRequest},
		Limits:   map[string]string{"cpu": b.CPULimit, "memory": b.MemoryLimit, "ephemeral-storage": b.EphemeralLimit},
	}
}

// Config is the immutable installation policy of the adapter.
type Config struct {
	// Backend is the name recorded in RuntimeBinding.Backend. Default "kubernetes".
	Backend string
	// RunAsUser and RunAsGroup are the non-root identity of every container.
	RunAsUser, RunAsGroup int64
	// Workload and Service bound step and service containers.
	Workload, Service Bounds
	// ScratchSize bounds the private /tmp volume of a container.
	ScratchSize string
	// WorkspaceSize is the storage request of a run workspace claim.
	WorkspaceSize string
	// StorageClass is the class of run workspace claims. It must exist; an
	// unnamed class is refused because its binding mode cannot be known.
	StorageClass string
	// ImagePullPolicy defaults to IfNotPresent (images are digest-pinned).
	ImagePullPolicy string
	// TerminationGrace is the Pod termination grace period. Default 30s.
	TerminationGrace time.Duration
	// Shell is the interpreter prefix that runs a step's commands as one
	// script. Default /bin/sh -ec; the step image must provide it.
	Shell []string
	// NetworkPolicyEnforced is the operator's attestation that the cluster's
	// network plugin enforces NetworkPolicy. Without it the adapter does not
	// advertise network_isolation. It is an attestation, not a proof.
	NetworkPolicyEnforced bool
}

func (c Config) withDefaults() (Config, error) {
	if c.Backend == "" {
		c.Backend = "kubernetes"
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = "IfNotPresent"
	}
	switch c.ImagePullPolicy {
	case "IfNotPresent", "Always", "Never":
	default:
		return c, fmt.Errorf("kernelrt: image pull policy %q", c.ImagePullPolicy)
	}
	if c.TerminationGrace == 0 {
		c.TerminationGrace = 30 * time.Second
	}
	if c.TerminationGrace < 0 {
		return c, errors.New("kernelrt: termination grace must not be negative")
	}
	if len(c.Shell) == 0 {
		c.Shell = []string{"/bin/sh", "-ec"}
	}
	if c.RunAsUser <= 0 || c.RunAsGroup <= 0 {
		return c, errors.New("kernelrt: containers run as a non-root user and group")
	}
	if err := c.Workload.validate("workload"); err != nil {
		return c, err
	}
	if err := c.Service.validate("service"); err != nil {
		return c, err
	}
	for name, v := range map[string]string{"scratch size": c.ScratchSize, "workspace size": c.WorkspaceSize} {
		if !quantityRE.MatchString(v) {
			return c, fmt.Errorf("kernelrt: %s %q is not a quantity", name, v)
		}
	}
	if !nameRE.MatchString(c.StorageClass) {
		return c, errors.New("kernelrt: a named storage class is required")
	}
	return c, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// Options are mechanical settings with safe defaults.
type Options struct {
	Clock func() time.Time
	// CreateQuiescence is how long an issued create with an unknown outcome is
	// assumed able to still land. Absence is reported only after it. It must
	// exceed the API request timeout.
	CreateQuiescence time.Duration
	// OrphanGrace spares young unreferenced objects from reconciliation.
	OrphanGrace time.Duration
}

func (o Options) withDefaults() (Options, error) {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.CreateQuiescence == 0 {
		o.CreateQuiescence = 2 * time.Minute
	}
	if o.OrphanGrace == 0 {
		o.OrphanGrace = 2 * time.Minute
	}
	if o.CreateQuiescence < time.Second || o.OrphanGrace < 0 {
		return o, errors.New("kernelrt: options are out of range")
	}
	return o, nil
}

// Adapter holds the Kubernetes implementation of both kernel ports.
type Adapter struct {
	*core
}

type core struct {
	cfg     Config
	opts    Options
	cluster Cluster
	targets Targets
	secrets SecretFiles
}

// New validates the policy and dependencies.
func New(cfg Config, deps Dependencies, opts Options) (*Adapter, error) {
	c, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	o, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	if deps.Cluster == nil || deps.Targets == nil {
		return nil, errors.New("kernelrt: a cluster and a target resolver are required")
	}
	return &Adapter{&core{cfg: c, opts: o, cluster: deps.Cluster, targets: deps.Targets, secrets: deps.Secrets}}, nil
}

// Runtime is the workload port.
func (a *Adapter) Runtime() *Runtime { return &Runtime{a.core} }

// Resources is the workspace and service port.
func (a *Adapter) Resources() *Resources { return &Resources{a.core} }

var (
	_ runtime.Runtime         = (*Runtime)(nil)
	_ runtime.ResourceManager = (*Resources)(nil)
)

// capabilities is what the adapter enforces from the objects it writes.
func (c *core) capabilities() runtime.CapabilitySet {
	set := runtime.CapabilitySet{
		runtime.CapNonRoot, runtime.CapResourceLimits, runtime.CapImagePinning,
		runtime.CapAdoptWorkload, runtime.CapFencedStop, runtime.CapRunServices,
	}
	if c.cfg.NetworkPolicyEnforced {
		set = append(set, runtime.CapNetworkIsolation)
	}
	return set
}
