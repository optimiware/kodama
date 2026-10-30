package kube

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// The ports below are small and owned by this module. adapters/ implements the
// attempt reads over the PostgreSQL execution store; *kubernetes.Client
// implements Cluster; execution.Service and runtimeauth.Service implement the
// observation and registration sinks.

// AttemptRecord is the committed view of an attempt the executor verifies every
// dispatch field against. It carries only data execution persisted (never data
// read from a workload).
type AttemptRecord struct {
	Scope     domain.Scope
	RunID     domain.RunID
	StepID    string
	AttemptID domain.AttemptID
	ActionKey string

	Status execution.AttemptStatus
	// Live is true while the attempt is running and its authority is not
	// revoked: only then may a workload be created or adopted.
	Live bool

	// WorkspaceID is the workspace the attempt is bound to (empty without
	// one). It is committed execution data and identifies the source scope of the
	// checkout credential.
	WorkspaceID string

	JobName         string
	SpecFingerprint string
	ContextDigest   string
	// Context is the canonical pinned context document.
	Context []byte
	Access  execution.Access

	// ObservedJobUID is the Job UID execution bound at dispatch confirmation
	// or first start.
	ObservedJobUID    string
	StartedAt         *time.Time
	DispatchConfirmed bool
	// TerminationRequested and TerminationConfirmed mirror execution's control
	// bookkeeping; Settled is true once every effect is accounted for.
	TerminationRequested bool
	TerminationConfirmed bool
	Settled              bool
	SettledAt            *time.Time
	Launch               execution.LaunchEvidence

	// Shared is the committed run_shared launch of the attempt (nil for an
	// attempt_private attempt): the run's pinned step role, its bound workspace
	// claim and its network model, derived from the run's immutable pins and the
	// workspace's bound identity. Every expectation of the attempt's workload
	// (adoption, observation, termination) is rebuilt from it, so a run_shared
	// Job is verified against the shape it was created with and nothing else.
	Shared *execution.SharedLaunch
}

// Attempts reads committed attempts. Implementations scope every read by
// tenant and project; Attempt returns ErrNotFound only when the scoped lookup
// definitely found nothing, and any other error for an outage.
type Attempts interface {
	Attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (AttemptRecord, error)
	// Unsettled lists the tenant's attempts that are not settled, in pages.
	// after is the continuation key returned by the previous page ("" first).
	Unsettled(ctx context.Context, tenant domain.TenantID, after string, limit int) (page []AttemptRecord, next string, err error)
}

// Observations receives verified evidence. execution.Service implements it.
type Observations interface {
	ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, obs execution.Observation) error
}

// Registrar registers an attempt's admission credential. runtimeauth.Service
// implements it.
type Registrar interface {
	Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error)
}

// Cluster is the bounded Kubernetes surface: get, list, create, conditional
// update, UID-preconditioned delete, and read-only policy inspection.
// *kubernetes.Client implements it.
type Cluster interface {
	GetJob(ctx context.Context, ns, name string) (kubernetes.Job, error)
	CreateJob(ctx context.Context, ns string, j kubernetes.Job) (kubernetes.Job, error)
	ListJobs(ctx context.Context, ns, selector string) ([]kubernetes.Job, error)
	DeleteJob(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error

	ListPods(ctx context.Context, ns, selector string) ([]kubernetes.Pod, error)
	GetPod(ctx context.Context, ns, name string) (kubernetes.Pod, error)
	DeletePod(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error

	GetSecret(ctx context.Context, ns, name string) (kubernetes.Secret, error)
	CreateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)
	UpdateSecret(ctx context.Context, ns string, s kubernetes.Secret) (kubernetes.Secret, error)
	DeleteSecret(ctx context.Context, ns, name string, o kubernetes.DeleteOptions) error
	ListSecrets(ctx context.Context, ns, selector string) ([]kubernetes.Secret, error)

	GetNamespace(ctx context.Context, name string) (kubernetes.Namespace, error)
	GetServiceAccount(ctx context.Context, ns, name string) (kubernetes.ServiceAccount, error)
	ListResourceQuotas(ctx context.Context, ns string) ([]kubernetes.ResourceQuota, error)
	ListNetworkPolicies(ctx context.Context, ns string) ([]kubernetes.NetworkPolicy, error)
	Allowed(ctx context.Context, user string, groups []string, attrs kubernetes.ResourceAttributes) (bool, error)
}

var _ Cluster = (*kubernetes.Client)(nil)

// PodLogs reads the log of one container of a Pod, bounded by the reader.
// *kubernetes.Client implements it.
type PodLogs interface {
	PodLog(ctx context.Context, ns, pod, container string) ([]byte, error)
}

var _ PodLogs = (*kubernetes.Client)(nil)

// Tenants enumerates tenants for scans; execution's Tenants port satisfies it.
type Tenants interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}
