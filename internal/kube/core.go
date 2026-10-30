package kube

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Dependencies are the collaborators of the runtime, observer and reconciler.
// Required ports must be set; nothing has an allow-all default.
type Dependencies struct {
	Policy    ExecutionPolicy
	Cluster   Cluster
	Attempts  Attempts
	Registrar Registrar
	// Observations and Tenants are needed only by the reconciler.
	Observations Observations
	Tenants      Tenants
	// Checkout issues the read-only checkout credential and Git CA of
	// source-consuming attempts (see checkout.go). It is consulted only by the
	// executor, and only to create an attempt's bootstrap Secret. Nil is valid: a
	// source-consuming attempt is then denied and an attempt without a
	// workspace is unaffected.
	Checkout CheckoutProvider
	// Logs reads Pod logs for Runtime.Logs. Optional.
	Logs PodLogs
	// OwnedPolicies verifies the run network policies of the namespace for a
	// run_shared attempt's preflight (the RunResources of the same composition).
	// Without it a namespace that holds run policies fails that preflight.
	OwnedPolicies OwnedPolicyVerifier
}

// Options are mechanical settings with safe defaults. None of them is an
// authoritative deadline: startup and execution deadlines belong to execution.
type Options struct {
	Clock  func() time.Time
	Logger *slog.Logger
	// CreateQuiescence is how long an issued Job create with an unknown outcome
	// is assumed able to still land. Termination reports the workload gone only
	// after it, and only with LaunchUnknown. It must exceed the API request
	// timeout.
	CreateQuiescence time.Duration
	// PreflightTTL caches a successful namespace policy check.
	PreflightTTL time.Duration
	// OrphanGrace spares young unreferenced objects from orphan cleanup.
	OrphanGrace time.Duration
	// CompletedRetention is how long the Job of a settled attempt is kept
	// before deletion (an operator inspection window). Jobs are never deleted
	// before their attempt is settled, and no TTL controller is enabled.
	CompletedRetention time.Duration
	// Batch bounds each scan page.
	Batch int
}

func (o Options) withDefaults() (Options, error) {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.CreateQuiescence == 0 {
		o.CreateQuiescence = 2 * time.Minute
	}
	if o.PreflightTTL == 0 {
		o.PreflightTTL = 30 * time.Second
	}
	if o.OrphanGrace == 0 {
		o.OrphanGrace = 2 * time.Minute
	}
	if o.Batch == 0 {
		o.Batch = 100
	}
	if o.CreateQuiescence < time.Second || o.PreflightTTL < 0 || o.OrphanGrace < 0 || o.CompletedRetention < 0 || o.Batch < 1 || o.Batch > 1000 {
		return o, wrap(ErrPolicy, "executor options are out of range")
	}
	return o, nil
}

// core is the shared machinery of the runtime, observer and reconciler.
type core struct {
	policy  ExecutionPolicy
	cluster Cluster
	// run is the cluster's run-resource surface, nil for a cluster that has none
	// (a run_shared attempt is then refused, never started without its claim).
	run       RunCluster
	attempts  Attempts
	registrar Registrar
	obs       Observations
	tenants   Tenants
	checkout  CheckoutProvider
	preflight *Preflight
	opts      Options
	log       *slog.Logger
	now       func() time.Time
}

func newCore(d Dependencies, o Options, needRegistrar bool) (*core, error) {
	opts, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	if !d.Policy.Valid() {
		return nil, wrap(ErrPolicy, "a validated execution policy is required")
	}
	if d.Cluster == nil || d.Attempts == nil || (needRegistrar && d.Registrar == nil) {
		return nil, wrap(ErrPolicy, "the cluster, attempt reader and registrar are required")
	}
	rc, _ := d.Cluster.(RunCluster)
	return &core{
		policy: d.Policy, cluster: d.Cluster, run: rc, attempts: d.Attempts, registrar: d.Registrar, obs: d.Observations, tenants: d.Tenants, checkout: d.Checkout,
		preflight: NewPreflight(d.Cluster, d.Policy, opts.PreflightTTL, opts.Clock).WithOwnedPolicies(d.OwnedPolicies), opts: opts, log: opts.Logger, now: opts.Clock,
	}, nil
}

// readJob returns the Job by name. Absence is (zero, false, nil) only for a
// definite not-found; every other failure is an outage that concludes nothing.
func (c *core) readJob(ctx context.Context, ns, name string) (kubernetes.Job, bool, error) {
	j, err := c.cluster.GetJob(ctx, ns, name)
	switch {
	case err == nil:
		return j, true, nil
	case errors.Is(err, kubernetes.ErrNotFound):
		return kubernetes.Job{}, false, nil
	}
	return kubernetes.Job{}, false, wrapCause(ErrUnavailable, err, "reading the Job")
}

// podsOf lists the Pods labelled for the Job. The label only selects candidates:
// callers keep a Pod only when its controlling owner is the verified Job.
func (c *core) podsOf(ctx context.Context, ns, jobName string) ([]kubernetes.Pod, error) {
	pods, err := c.cluster.ListPods(ctx, ns, LabelJob+"="+jobName)
	if err != nil {
		return nil, wrapCause(ErrUnavailable, err, "listing Pods")
	}
	return pods, nil
}

// controlledBy reports whether the Pod's controlling owner is the named Job with
// the given UID.
func controlledBy(p kubernetes.Pod, jobName, jobUID string) bool {
	if jobUID == "" {
		return false
	}
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controlled() && o.Kind == "Job" && o.APIVersion == "batch/v1" && o.Name == jobName && o.UID == jobUID {
			return true
		}
	}
	return false
}

// attemptOf reads the committed attempt and maps an authoritative absence.
func (c *core) attemptOf(ctx context.Context, scope domain.Scope, id domain.AttemptID) (AttemptRecord, error) {
	rec, err := c.attempts.Attempt(ctx, scope, id)
	switch {
	case err == nil:
		return rec, nil
	case errors.Is(err, ErrNotFound):
		return AttemptRecord{}, err
	}
	return AttemptRecord{}, wrapCause(ErrUnavailable, err, "reading the attempt")
}
