// Pinned Kubernetes runtime adapter API.
//
// This file declares the minimal Kubernetes API seam the adapter consumes and
// the adapter configuration surface. It is intentionally much smaller than
// client-go: the seam is an interface implemented by the production client and
// by the deterministic in-memory fake in tests. Nothing here opens a network
// connection or starts a process.
package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

// Pinned manifest vocabulary shared by the generated manifests and the pod
// selectors, so a tenant network policy and the adapter agree.
const (
	// LabelTenant names the owning tenant.
	LabelTenant = "kodama.dev/tenant"
	// LabelExecution names the execution.
	LabelExecution = "kodama.dev/execution"
	// LabelAction names the action.
	LabelAction = "kodama.dev/action"
	// LabelWorkload names the stable runtime workload identity.
	LabelWorkload = "kodama.dev/workload"
	// LabelWorkspace names the retained execution workspace.
	LabelWorkspace = "kodama.dev/workspace"
	// LabelRetained marks a workspace claim whose retention must survive a
	// stale release.
	LabelRetained = "kodama.dev/retained"
	// LabelPreparation carries the deterministic preparation declaration
	// digest on the immutable activation ConfigMap.
	LabelPreparation = "kodama.dev/preparation"
	// AnnotationIdempotencyKey carries the persisted idempotency key so a
	// retry can be attributed to the original effect.
	AnnotationIdempotencyKey = "kodama.dev/idempotency-key"
	// AnnotationRetained carries the durable workspace-retention marker.
	AnnotationRetained = "kodama.dev/retained"
)

const (
	// MaxLogBytes bounds one log read. A request above this bound is refused
	// before any external call.
	MaxLogBytes = 1 << 20
	// Redacted replaces configured credential and secret values in log output.
	Redacted = "[REDACTED]"
)

const (
	// WorkloadCAKey is the literal Secret key carrying the installation CA
	// bundle. Secret keys are not path-encoded.
	WorkloadCAKey = "ca.crt"
	// WorkloadCAMountPath is the fixed read-only mount root of the workload CA.
	WorkloadCAMountPath = "/etc/kodama/ca"
	// WorkloadCAPath is the fixed absolute path of the mounted CA bundle.
	WorkloadCAPath = WorkloadCAMountPath + "/" + WorkloadCAKey
)

// WorkloadCAConfig configures the installation-provided CA bundle projected
// read-only into every workload. An empty SecretName disables the mount, and the
// adapter never reads or re-materializes the Secret: workloads opt in with an
// explicit --cacert.
type WorkloadCAConfig struct {
	// SecretName is the tenant-local, CA-only Secret in the adapter namespace.
	SecretName string
}

// Sentinel errors the Cluster seam wraps. The adapter interprets them; an
// absent object is never converted into proof that a command never ran or that
// a process stopped.
var (
	// ErrNotFound reports that an object does not exist.
	ErrNotFound = errors.New("kubernetes: object not found")
	// ErrAlreadyExists reports that an object with the same name exists.
	ErrAlreadyExists = errors.New("kubernetes: object already exists")
	// ErrConflict reports a stale resource version or an immutable change.
	ErrConflict = errors.New("kubernetes: object conflict")
)

// Clock is the injected time source. Production uses the real clock; tests use
// a deterministic clock that never sleeps in wall-clock time.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// Config binds one adapter instance to one tenant namespace.
type Config struct {
	// Cluster is the only external effect surface.
	Cluster Cluster
	// Namespace is the tenant namespace this adapter is bound to.
	Namespace string
	// TenantID is the tenant ownership label expected on that namespace.
	TenantID id.TenantID
	// KodamaURL is the reserved KODAMA_URL value injected into workloads.
	KodamaURL string
	// Policy carries the tenant resource limits and finished-Job TTL.
	Policy Policy
	// Cleanup bounds cleanup attempts per Remove call.
	Cleanup CleanupPolicy
	// WorkloadCA configures the installation CA bundle mounted into workloads.
	WorkloadCA WorkloadCAConfig
	// ImagePullSecrets names the tenant-local pull Secrets the Job references.
	// An empty list renders no imagePullSecrets field.
	ImagePullSecrets []string
	// Clock is optional; nil means a real clock.
	Clock Clock
}

// Policy is the admitted workload resource policy.
type Policy struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
	JobTTLSeconds int
}

// CleanupPolicy bounds one Remove call.
type CleanupPolicy struct {
	// Attempts is the total attempts per resource per Remove call; at least 1.
	Attempts int
	// Backoff is the delay between attempts, applied through Clock.Sleep.
	Backoff time.Duration
}

// Namespace is the tenant namespace and its ownership labels.
type Namespace struct {
	Name   string
	Labels map[string]string
}

// Resources are the container resource requests and limits.
type Resources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// EnvVar is one literal environment variable.
type EnvVar struct {
	Name  string
	Value string
}

// VolumeItem is one projected file in a volume.
type VolumeItem struct {
	Key  string
	Path string
	Mode os.FileMode
}

// Volume is one pod volume and its mount.
type Volume struct {
	Name          string
	MountPath     string
	ClaimName     string
	ConfigMapName string
	SecretName    string
	ReadOnly      bool
	Items         []VolumeItem
}

// JobSpec is the generated Job pod template.
type JobSpec struct {
	Image         string
	Command       []string
	Env           []EnvVar
	Resources     Resources
	Volumes       []Volume
	RestartPolicy string
	BackoffLimit  int
	// PodReplacementPolicy maps to the batch/v1 Job spec.podReplacementPolicy.
	// Pinning Failed means a running pod is never replaced while it may still
	// write to the retained workspace.
	PodReplacementPolicy string
	// TTLSecondsAfterFinished is the finished-Job TTL sent on create. A
	// successful suspension clears it, so nil means no TTL remains.
	TTLSecondsAfterFinished      *int
	AutomountServiceAccountToken bool
	HostNetwork                  bool
	PodLabels                    map[string]string
	// ImagePullSecrets names the configured private-registry pull Secrets. An
	// empty list renders no imagePullSecrets field.
	ImagePullSecrets []string
	// Suspend maps to the batch/v1 Job spec.suspend intent. Nil means the
	// server did not report suspension state. The manifest builder never sends
	// it on create; only SuspendJob changes it.
	Suspend *bool
}

// JobCondition is one observed Job condition. Status keeps the exact server
// value ("True", "False" or "Unknown") instead of collapsing it to a bool, so
// an unacknowledged or unknown condition can never look affirmative.
type JobCondition struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

// JobStatus is the observed Job status. It is informational only: it never
// proves a command stopped.
type JobStatus struct {
	Active         int
	Succeeded      int
	Failed         int
	StartTime      *time.Time
	CompletionTime *time.Time
	// Terminating is the number of Pending or Running pods that carry a
	// deletion timestamp. It is presence-preserving: nil means the field was
	// absent or null (unknown), never an affirmative zero.
	Terminating *int
	// Conditions are the Job conditions exactly as the server reported them.
	Conditions []JobCondition
}

// Job is one generated Kubernetes Job.
type Job struct {
	Name            string
	Namespace       string
	UID             string
	Labels          map[string]string
	Annotations     map[string]string
	Spec            JobSpec
	Status          JobStatus
	ResourceVersion string
	// DeletionTimestamp is the observed metadata.deletionTimestamp. A Job with
	// a deletion timestamp is being removed and can never confirm a suspension.
	DeletionTimestamp *time.Time
}

// ContainerStatus is one pod container's observed status.
type ContainerStatus struct {
	Name       string
	Terminated bool
	ExitCode   int
}

// Pod is one observed workload pod.
type Pod struct {
	Name              string
	Namespace         string
	Labels            map[string]string
	Phase             string
	NodeName          string
	Containers        []ContainerStatus
	DeletionTimestamp *time.Time
}

// ConfigMap is an immutable activation snapshot.
type ConfigMap struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	Data            map[string][]byte
	Immutable       bool
	ResourceVersion string
}

// Secret is the mutable credential and declared-secret mount.
type Secret struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	Data            map[string][]byte
	Immutable       bool
	ResourceVersion string
}

// Claim is the pre-existing retained workspace volume claim.
type Claim struct {
	Name              string
	Namespace         string
	Labels            map[string]string
	Annotations       map[string]string
	Finalizers        []string
	DeletionTimestamp *time.Time
	ResourceVersion   string
}

// LogStreamCombined names the one interleaved pod log (stdout and stderr
// together). Stock clusters cannot split the two, so this is the only stream a
// Cluster is asked for.
const LogStreamCombined = "combined"

// PodLogRequest selects one bounded read of the combined pod log.
type PodLogRequest struct {
	Stream string
	Offset int64
	Limit  int
}

// PodLogPage is one bounded combined-log read result.
type PodLogPage struct {
	Data       []byte
	NextOffset int64
	Truncated  bool
}

// Cluster is the only external effect surface of the adapter.
type Cluster interface {
	GetNamespace(context.Context, string) (Namespace, error)
	CreateJob(context.Context, Job) (Job, error)
	GetJob(context.Context, string, string) (Job, error)
	DeleteJob(context.Context, string, string) error
	// SuspendJob asks the controller to suspend exactly the Job with the
	// expected UID and clears its finished-Job TTL in the same request, so a Job
	// that finishes while suspended survives until Remove deletes it instead of
	// being removed (with foreground propagation, taking its pods) by the TTL
	// controller. The UID guard makes a reused Job name a conflict instead of
	// suspending an unrelated Job. The answer carries the observed state; a
	// successful request is intent, not a confirmation.
	SuspendJob(context.Context, string, string, string) (Job, error)
	ListPods(context.Context, string, map[string]string) ([]Pod, error)
	ReadPodLogs(context.Context, string, string, PodLogRequest) (PodLogPage, error)
	PutConfigMap(context.Context, ConfigMap) (ConfigMap, error)
	GetConfigMap(context.Context, string, string) (ConfigMap, error)
	DeleteConfigMap(context.Context, string, string) error
	PutSecret(context.Context, Secret) (Secret, error)
	GetSecret(context.Context, string, string) (Secret, error)
	DeleteSecret(context.Context, string, string) error
	GetClaim(context.Context, string, string) (Claim, error)
	UpdateClaim(context.Context, Claim) (Claim, error)
	DeleteClaim(context.Context, string, string) error
	// DeleteClaimVersioned deletes only when the claim still carries the
	// expected resource version, so a stale Release never drops a retained
	// workspace.
	DeleteClaimVersioned(context.Context, string, string, string) error
	ListJobs(context.Context, string, map[string]string) ([]Job, error)
}

// preparedWorkload is the in-process binding between one persisted workload
// identity and the activation declaration. It is keyed by WorkloadID so a retry
// inside the same process cannot rebind the workload to a changed identity or
// declaration.
type preparedWorkload struct {
	identity        engine.EffectIdentity
	digest          string
	workspace       id.WorkspaceID
	actionName      id.ActionName
	taskID          id.TaskID
	container       containerDeclaration
	claimName       string
	configMap       string
	secret          string
	job             string
	activationPaths []string
	secretNames     []id.SecretName
}

// containerDeclaration is the subset of the admitted container declaration the
// adapter needs after Prepare. Values are copied so later mutation by a caller
// cannot change an already-prepared workload.
type containerDeclaration struct {
	image  string
	script string
	env    []EnvVar
}

// Adapter implements runtime.Client and engine.Workspace for the Kubernetes Job
// family.
type Adapter struct {
	cluster          Cluster
	namespace        string
	tenant           id.TenantID
	kodamaURL        string
	policy           Policy
	cleanup          CleanupPolicy
	workloadCA       WorkloadCAConfig
	imagePullSecrets []string
	clock            Clock

	mu       sync.Mutex
	prepared map[id.WorkloadID]preparedWorkload

	locksMu sync.Mutex
	locks   map[id.WorkspaceID]*workspaceLock
}

// workspaceLock serializes the lifecycle of one retained workspace inside this
// adapter. Start, Remove, Retain and Release share it so a claim is never
// deleted while a workload is being created or removed.
type workspaceLock struct {
	mu   sync.Mutex
	refs int
}

// New validates one adapter configuration. An incomplete configuration is
// refused: the adapter never guesses a namespace, tenant, limit or cleanup
// bound.
func New(config Config) (*Adapter, error) {
	if config.Cluster == nil {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires a cluster client")
	}
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires a tenant namespace")
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "kubernetes adapter tenant id is invalid", err)
	}
	if strings.TrimSpace(config.KodamaURL) == "" {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires the Kodama URL")
	}
	for name, value := range map[string]string{
		"cpu request":    config.Policy.CPURequest,
		"cpu limit":      config.Policy.CPULimit,
		"memory request": config.Policy.MemoryRequest,
		"memory limit":   config.Policy.MemoryLimit,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires a "+name)
		}
	}
	if config.Policy.JobTTLSeconds <= 0 {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires a positive Job TTL")
	}
	if config.Cleanup.Attempts < 1 {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter requires at least one cleanup attempt")
	}
	if config.Cleanup.Backoff < 0 {
		return nil, fault.New(fault.InvalidInput, "kubernetes adapter cleanup backoff is negative")
	}
	if config.WorkloadCA.SecretName != "" {
		if err := validateWorkloadCASecretName(config.WorkloadCA.SecretName); err != nil {
			return nil, err
		}
	}
	if err := validateImagePullSecretNames(config.ImagePullSecrets); err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = realClock{}
	}
	return &Adapter{
		cluster:          config.Cluster,
		namespace:        config.Namespace,
		tenant:           config.TenantID,
		kodamaURL:        config.KodamaURL,
		policy:           config.Policy,
		cleanup:          config.Cleanup,
		workloadCA:       config.WorkloadCA,
		imagePullSecrets: append([]string(nil), config.ImagePullSecrets...),
		clock:            clock,
		prepared:         map[id.WorkloadID]preparedWorkload{},
		locks:            map[id.WorkspaceID]*workspaceLock{},
	}, nil
}

// validateWorkloadCASecretName refuses a reference that is not a DNS-1123
// subdomain before any cluster call, so an unusable CA Secret name never reaches
// the API server. The name is a configuration reference, not secret material.
func validateWorkloadCASecretName(name string) error {
	if !validDNS1123Subdomain(name) {
		return fault.New(fault.InvalidInput, "kubernetes adapter workload CA Secret name is not a DNS-1123 subdomain")
	}
	return nil
}

// validateImagePullSecretNames refuses an unusable or duplicated pull Secret
// reference before any cluster call. The names are configuration references,
// never credential material.
func validateImagePullSecretNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !validDNS1123Subdomain(name) {
			return fault.New(fault.InvalidInput, "kubernetes adapter image pull secret name is not a DNS-1123 subdomain")
		}
		if _, duplicate := seen[name]; duplicate {
			return fault.New(fault.InvalidInput, "kubernetes adapter image pull secret name is duplicated")
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validDNS1123Subdomain reports whether name is a Kubernetes DNS-1123
// subdomain (lowercase alphanumerics, '-' and '.', at most 253 characters).
func validDNS1123Subdomain(name string) bool {
	const maxDNSSubdomainLength = 253
	const maxDNSLabelLength = 63
	if len(name) == 0 || len(name) > maxDNSSubdomainLength {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > maxDNSLabelLength {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			alphanumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
			if !alphanumeric && character != '-' {
				return false
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

// Version reports the pinned Kubernetes API surface without contacting the
// cluster.
func (a *Adapter) Version(context.Context) (runtime.APIVersion, error) {
	return runtime.APIVersion{Adapter: runtime.AdapterKubernetes, Version: "batch/v1"}, nil
}

// Capabilities reports the explicit Kubernetes capability set. It is
// declaration-only and makes no cluster call.
func (a *Adapter) Capabilities(context.Context) (engine.Capabilities, error) {
	family := runtime.CapabilitiesFor(runtime.AdapterKubernetes)
	names := make([]string, len(family))
	for index, capability := range family {
		names[index] = string(capability)
	}
	return engine.Capabilities{Names: names}, nil
}

// CheckRequirements refuses admitted work the Kubernetes family cannot run.
func CheckRequirements(required []string) error {
	supported := make(map[string]bool)
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterKubernetes) {
		supported[string(capability)] = true
	}
	for _, name := range required {
		if !supported[name] {
			return fault.New(fault.UnsupportedCapability, "kubernetes adapter does not support required capability "+name)
		}
	}
	return nil
}

// WorkspaceClaimName returns the deterministic claim name for one retained
// workspace. The Shell provisions this claim before Prepare and the adapter
// attaches exactly this claim after a restart.
func WorkspaceClaimName(workspace id.WorkspaceID) (string, error) {
	if _, err := id.ParseWorkspaceID(string(workspace)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "workspace id is invalid", err)
	}
	return "kodama-workspace-" + stableDigest(string(workspace)), nil
}

// stableDigest returns a short deterministic, DNS-safe digest of a stable
// identity. Object names are derived only from persisted identities, never from
// GenerateName, so a retry finds the same object.
func stableDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:24]
}

// realClock is the production clock. It never appears in unit tests.
type realClock struct{}

// Now returns the current wall-clock time.
func (realClock) Now() time.Time { return time.Now() }

// Sleep waits for the duration or until the context ends.
func (realClock) Sleep(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
