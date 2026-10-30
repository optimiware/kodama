package gitworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Phase is the observed state of a tenant's worker after a reconciliation.
type Phase string

const (
	// PhaseReady: every object verified and the single worker Pod is ready.
	PhaseReady Phase = "ready"
	// PhaseConverging: the objects verified but the worker is not ready yet
	// (starting, being replaced). No error: the next pass observes it again.
	PhaseConverging Phase = "converging"
	// PhaseBlocked: a refusal (foreign object, drift, storage, isolation,
	// identity) or an unavailable dependency stopped the pass.
	PhaseBlocked Phase = "blocked"
)

// Status is the result of one reconciliation of one tenant.
type Status struct {
	Tenant    domain.TenantID
	Namespace string
	Phase     Phase
	Ready     bool
	// Reason is fixed text for a phase other than ready.
	Reason string
	// Fingerprint is the digest of the desired objects; Endpoint the Service URL
	// of the worker when the objects are the desired ones.
	Fingerprint string
	Endpoint    string
	ObservedAt  time.Time
	// Actions are the mutations the pass made (created or replaced objects), in
	// order. A pass that finds everything in place has none.
	Actions []string
	// Drift are the document paths (never values) at which a live object differed
	// from its rendering before it was corrected.
	Drift []string
}

// Dependencies are the ports the reconciler needs.
type Dependencies struct {
	Cluster  Cluster
	Identity IdentityProvider
	// Retention gates OffboardTenant only; without it offboarding is refused.
	Retention Retention
}

// Options tunes the reconciler.
type Options struct {
	Clock func() time.Time
	// RWOPProvisioners are the storage provisioners the installation declares
	// capable of ReadWriteOncePod. The API cannot say whether a class supports it,
	// so a class whose provisioner is not listed is refused. At least one is
	// required.
	RWOPProvisioners []string
	// StatusTTL bounds how long a ready status answers Endpoint without another
	// reconciliation (default 2 minutes). Reconcile at least that often.
	StatusTTL time.Duration
	// MinIdentityValidity is the remaining lifetime below which an identity token
	// is not delivered (default 1 minute).
	MinIdentityValidity time.Duration
}

// Reconciler converges tenants' Git hosting objects and answers discovery. It
// keeps only observations in memory (the last status of each tenant); everything
// else is read from the cluster on every pass, so a restart loses nothing.
type Reconciler struct {
	deps Dependencies
	opts Options

	mu     sync.Mutex
	locks  map[domain.TenantID]*sync.Mutex
	status map[domain.TenantID]Status
}

// New validates the dependencies.
func New(deps Dependencies, opts Options) (*Reconciler, error) {
	if deps.Cluster == nil || deps.Identity == nil {
		return nil, errors.New("gitworker: a cluster and an identity provider are required")
	}
	if len(opts.RWOPProvisioners) == 0 {
		return nil, errors.New("gitworker: declare at least one ReadWriteOncePod-capable storage provisioner")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = 2 * time.Minute
	}
	if opts.MinIdentityValidity <= 0 {
		opts.MinIdentityValidity = time.Minute
	}
	opts.RWOPProvisioners = slices.Clone(opts.RWOPProvisioners)
	return &Reconciler{deps: deps, opts: opts, locks: map[domain.TenantID]*sync.Mutex{}, status: map[domain.TenantID]Status{}}, nil
}

func (r *Reconciler) lock(t domain.TenantID) func() {
	r.mu.Lock()
	l, ok := r.locks[t]
	if !ok {
		l = &sync.Mutex{}
		r.locks[t] = l
	}
	r.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Reconcile converges the tenant's hosting to the desired worker. It is
// idempotent and safe to repeat after any failure: each object is read before it
// is written, an object is created only when it is absent, an existing object is
// adopted only when it verifies, and nothing is ever deleted. The returned status
// is also what Endpoint answers from.
func (r *Reconciler) Reconcile(ctx context.Context, tenant domain.TenantID, spec WorkerSpec) (Status, error) {
	objs, err := Render(tenant, spec)
	if err != nil {
		return Status{Tenant: tenant, Phase: PhaseBlocked, Reason: "the specification is invalid"}, err
	}
	defer r.lock(tenant)()
	spec = spec.withDefaults()
	st := Status{
		Tenant: tenant, Namespace: NamespaceOf(tenant), Fingerprint: objs.SpecFingerprint(), Endpoint: EndpointURL(tenant, spec),
	}
	x := &pass{r: r, tenant: tenant, spec: spec, objs: objs, key: TenantKey(tenant), ns: NamespaceOf(tenant)}
	ready, reason, err := x.run(ctx)
	st.Actions, st.Drift, st.ObservedAt = x.actions, x.notes, r.opts.Clock()
	switch {
	case err != nil:
		st.Phase, st.Reason = PhaseBlocked, blockReason(err)
		if errors.Is(err, ErrUnavailable) && !x.dirty {
			// A pure observation failure: nothing contrary was found and nothing was
			// changed, so the last observation stands until it ages out. Once drift
			// was detected or a mutation began, the last observation no longer
			// describes the cluster and is replaced by this not-ready one.
			return st, err
		}
	case ready:
		st.Phase, st.Ready = PhaseReady, true
	default:
		st.Phase, st.Reason = PhaseConverging, reason
	}
	r.mu.Lock()
	r.status[tenant] = st
	r.mu.Unlock()
	return st, err
}

func blockReason(err error) string {
	var d *diagError
	if errors.As(err, &d) {
		return d.SafeDiagnostic()
	}
	return "blocked"
}

// Status returns the last observation of the tenant.
func (r *Reconciler) Status(tenant domain.TenantID) (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.status[tenant]
	return st, ok
}

// pass is one reconciliation of one tenant.
type pass struct {
	r       *Reconciler
	tenant  domain.TenantID
	spec    WorkerSpec
	objs    Objects
	key, ns string
	nsUID   string
	actions []string
	// dirty is set when this pass found drift or began a mutation: from then on a
	// failure invalidates the tenant's cached readiness.
	dirty bool
	// notes are the document paths (never values) that made an object stale.
	notes []string
}

func (x *pass) cl() Cluster { return x.r.deps.Cluster }

func (x *pass) did(format string, args ...any) {
	x.actions = append(x.actions, fmt.Sprintf(format, args...))
}

func (x *pass) chain() chain { return chain{tenant: x.tenant, key: x.key, ns: x.ns, nsUID: x.nsUID} }

func unavailable(err error, what string) error {
	return wrapCause(ErrUnavailable, err, "%s", what)
}

func notFound(err error) bool { return errors.Is(err, kubernetes.ErrNotFound) }

func (x *pass) run(ctx context.Context) (ready bool, reason string, err error) {
	// Refusals that need nothing created come first: a class that cannot hold the
	// volume must not leave a namespace or a claim behind, and an unusable identity
	// must not leave a worker that cannot authenticate.
	if err := x.preflightStorage(ctx); err != nil {
		return false, "", err
	}
	data, err := x.material(ctx)
	if err != nil {
		return false, "", err
	}
	if err := x.ensureNamespace(ctx); err != nil {
		return false, "", err
	}
	// Isolation before anything is delivered or started: a namespace left with an
	// allow-all policy or a stray Pod by a partial provisioning must not receive
	// the worker's identity or a running worker.
	if err := x.checkIsolation(ctx); err != nil {
		return false, "", err
	}
	for _, step := range []func(context.Context) error{
		x.ensureServiceAccount, x.ensureClaim, x.ensureService,
		// Policies before the identity and the Deployment: the worker's first Pod
		// never runs with an open network.
		x.ensurePolicies,
		func(ctx context.Context) error { return x.ensureSecret(ctx, data) },
	} {
		if err := step(ctx); err != nil {
			return false, "", err
		}
	}
	dep, err := x.ensureDeployment(ctx)
	if err != nil {
		return false, "", err
	}
	if err := x.checkVolume(ctx); err != nil {
		return false, "", err
	}
	return x.observe(ctx, dep)
}

// ---- generic convergence ----

// kind describes how to read, create, verify and (when allowed) replace one
// object. verify returns stale for an owned object that differs from the
// rendering; an error means it is foreign or must not be used.
type kind[T any] struct {
	what    string
	get     func(context.Context) (T, error)
	create  func(context.Context) (T, error)
	verify  func(T) (stale bool, err error)
	replace func(context.Context, T) (T, error)
}

func converge[T any](ctx context.Context, x *pass, k kind[T]) (T, error) {
	var zero T
	live, err := k.get(ctx)
	if notFound(err) {
		var created T
		x.dirty = true // the create may land whatever its answer is
		created, err = k.create(ctx)
		switch {
		case err == nil:
			x.did("created %s", k.what)
			return created, nil
		case errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
			// A concurrent writer or a lost answer: read what is there and verify it
			// like any other existing object.
			if live, err = k.get(ctx); notFound(err) {
				return zero, wrap(ErrUnavailable, "the outcome of creating the %s is unknown", k.what)
			}
		default:
			return zero, unavailable(err, "creating the "+k.what)
		}
	}
	if err != nil {
		return zero, unavailable(err, "reading the "+k.what)
	}
	stale, err := k.verify(live)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			x.dirty = true // the object is going away: contrary evidence
		}
		return zero, err
	}
	if !stale {
		return live, nil
	}
	x.dirty = true
	if k.replace == nil {
		return zero, wrap(ErrDrift, "the %s differs from its specification", k.what)
	}
	out, err := k.replace(ctx, live)
	switch {
	case err == nil:
		x.did("replaced %s", k.what)
		return out, nil
	case errors.Is(err, kubernetes.ErrConflict), errors.Is(err, kubernetes.ErrNotFound):
		return zero, wrap(ErrUnavailable, "the %s changed while it was being corrected", k.what)
	}
	return zero, unavailable(err, "correcting the "+k.what)
}

// ---- namespace ----

func (x *pass) ensureNamespace(ctx context.Context) error {
	want := x.objs.Namespace
	c := chain{tenant: x.tenant, key: x.key, ns: x.ns}
	live, err := converge(ctx, x, kind[kubernetes.Namespace]{
		what: "namespace",
		get:  func(ctx context.Context) (kubernetes.Namespace, error) { return x.cl().GetNamespace(ctx, x.ns) },
		create: func(ctx context.Context) (kubernetes.Namespace, error) {
			return x.cl().CreateNamespace(ctx, want)
		},
		verify: func(n kubernetes.Namespace) (bool, error) {
			if err := c.verifyMeta(n.Metadata, x.ns, RoleNamespace, false); err != nil {
				return false, err
			}
			if n.Status.Phase == "Terminating" {
				return false, wrap(ErrUnavailable, "the namespace is terminating")
			}
			stale := !current(n.Metadata, want.Metadata)
			for k, v := range want.Metadata.Labels {
				if n.Metadata.Labels[k] != v {
					stale = true
				}
			}
			return stale, nil
		},
		replace: func(ctx context.Context, n kubernetes.Namespace) (kubernetes.Namespace, error) {
			// Only the keys this package owns change; labels others added stay.
			n.Metadata.Labels = merge(n.Metadata.Labels, want.Metadata.Labels)
			n.Metadata.Annotations = merge(n.Metadata.Annotations, want.Metadata.Annotations)
			return x.cl().ReplaceNamespace(ctx, n)
		},
	})
	if err != nil {
		return err
	}
	if live.Metadata.UID == "" {
		return wrap(ErrUnavailable, "the namespace has no UID")
	}
	x.nsUID = live.Metadata.UID
	return nil
}

// ---- service account ----

func (x *pass) ensureServiceAccount(ctx context.Context) error {
	want := x.objs.ServiceAccount
	want.Metadata = stamp(want.Metadata, x.nsUID)
	_, err := converge(ctx, x, kind[kubernetes.ServiceAccount]{
		what: "service account",
		get: func(ctx context.Context) (kubernetes.ServiceAccount, error) {
			return x.cl().GetServiceAccount(ctx, x.ns, ServiceAccountName)
		},
		create: func(ctx context.Context) (kubernetes.ServiceAccount, error) {
			return x.cl().CreateServiceAccount(ctx, x.ns, want)
		},
		verify: func(s kubernetes.ServiceAccount) (bool, error) {
			if err := x.chain().verifyMeta(s.Metadata, ServiceAccountName, RoleServiceAccount, true); err != nil {
				return false, err
			}
			// A service account that mounts a token would give the worker a Kubernetes
			// identity (IT-17). It cannot be corrected in place: report it.
			return !current(s.Metadata, want.Metadata) || s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken, nil
		},
	})
	return err
}

// ---- volume claim ----

func (x *pass) ensureClaim(ctx context.Context) error {
	want := x.objs.Claim
	want.Metadata = stamp(want.Metadata, x.nsUID)
	_, err := converge(ctx, x, kind[kubernetes.PersistentVolumeClaim]{
		what: "volume claim",
		get: func(ctx context.Context) (kubernetes.PersistentVolumeClaim, error) {
			return x.cl().GetPVC(ctx, x.ns, ClaimName)
		},
		create: func(ctx context.Context) (kubernetes.PersistentVolumeClaim, error) {
			return x.cl().CreatePVC(ctx, x.ns, want)
		},
		// There is no replace: a claim is never recreated or edited by this package,
		// so the tenant's volume cannot be lost to a reconciliation.
		verify: func(p kubernetes.PersistentVolumeClaim) (bool, error) {
			if err := x.chain().verifyMeta(p.Metadata, ClaimName, RoleClaim, true); err != nil {
				return false, err
			}
			return !claimMatches(want, p) || !current(p.Metadata, want.Metadata), nil
		},
	})
	return err
}

// ---- identity secret ----

func (x *pass) material(ctx context.Context) (map[string][]byte, error) {
	m, err := x.r.deps.Identity.WorkerIdentity(ctx, x.tenant)
	if err != nil {
		return nil, wrap(ErrUnavailable, "the identity provider could not issue the worker identity")
	}
	bad := func(why string) error { return wrap(ErrIdentity, "%s", why) }
	if m.TenantID != x.tenant {
		return nil, bad("it is bound to another tenant")
	}
	if len(m.Token) == 0 || len(m.Token) > 16<<10 || !tokenRe.Match(m.Token) {
		return nil, bad("the token is missing or malformed")
	}
	if !m.ExpiresAt.After(x.r.opts.Clock().Add(x.r.opts.MinIdentityValidity)) {
		return nil, bad("the token is expired or about to expire")
	}
	// Only the validated blocks, re-encoded, are delivered: never the input bytes.
	publicKeys, err := checkPublicKeys(m.PublicKeys)
	if err != nil {
		return nil, bad("the public keys are not public PEM keys: workers hold public verification material only")
	}
	ca, err := checkCA(m.CA)
	if err != nil {
		return nil, bad("the CA is not a PEM certificate bundle")
	}
	return map[string][]byte{
		KeyToken: bytes.Clone(m.Token), KeyPublicKeys: publicKeys, KeyCA: ca,
	}, nil
}

func (x *pass) ensureSecret(ctx context.Context, data map[string][]byte) error {
	want := x.objs.Secret
	want.Metadata = stamp(want.Metadata, x.nsUID)
	want.Data = data
	_, err := converge(ctx, x, kind[kubernetes.Secret]{
		what: "identity secret",
		get:  func(ctx context.Context) (kubernetes.Secret, error) { return x.cl().GetSecret(ctx, x.ns, SecretName) },
		create: func(ctx context.Context) (kubernetes.Secret, error) {
			return x.cl().CreateSecret(ctx, x.ns, want)
		},
		verify: func(s kubernetes.Secret) (bool, error) {
			if err := x.chain().verifyMeta(s.Metadata, SecretName, RoleSecret, true); err != nil {
				return false, err
			}
			if s.Immutable != nil && *s.Immutable {
				return false, wrap(ErrDrift, "the identity secret is immutable and cannot be refreshed")
			}
			return !current(s.Metadata, want.Metadata) || !secretDataMatches(s.Data, data), nil
		},
		replace: func(ctx context.Context, s kubernetes.Secret) (kubernetes.Secret, error) {
			s.Metadata.Labels, s.Metadata.Annotations = merge(s.Metadata.Labels, want.Metadata.Labels), merge(s.Metadata.Annotations, want.Metadata.Annotations)
			s.Data, s.Type = data, "Opaque"
			return x.cl().UpdateSecret(ctx, x.ns, s)
		},
	})
	return err
}

// ---- service ----

func (x *pass) ensureService(ctx context.Context) error {
	want := x.objs.Service
	want.Metadata = stamp(want.Metadata, x.nsUID)
	_, err := converge(ctx, x, kind[kubernetes.Service]{
		what: "service",
		get: func(ctx context.Context) (kubernetes.Service, error) {
			return x.cl().GetService(ctx, x.ns, ServiceName)
		},
		create: func(ctx context.Context) (kubernetes.Service, error) {
			return x.cl().CreateService(ctx, x.ns, want)
		},
		verify: func(s kubernetes.Service) (bool, error) {
			if err := x.chain().verifyMeta(s.Metadata, ServiceName, RoleService, true); err != nil {
				return false, err
			}
			diff := serviceDiff(want, s)
			x.notes = append(x.notes, diff...)
			return !current(s.Metadata, want.Metadata) || len(diff) > 0, nil
		},
	})
	return err
}

// ---- network policies ----

func (x *pass) ensurePolicies(ctx context.Context) error {
	for _, p := range x.objs.Policies {
		want := p
		want.Metadata = stamp(want.Metadata, x.nsUID)
		name := want.Metadata.Name
		_, err := converge(ctx, x, kind[kubernetes.NetworkPolicy]{
			what: "network policy " + name,
			get: func(ctx context.Context) (kubernetes.NetworkPolicy, error) {
				return x.cl().GetNetworkPolicy(ctx, x.ns, name)
			},
			create: func(ctx context.Context) (kubernetes.NetworkPolicy, error) {
				return x.cl().CreateNetworkPolicy(ctx, x.ns, want)
			},
			verify: func(live kubernetes.NetworkPolicy) (bool, error) {
				if err := x.chain().verifyMeta(live.Metadata, name, RolePolicy, true); err != nil {
					return false, err
				}
				diff := policyDiff(want, live)
				x.notes = append(x.notes, diff...)
				return !current(live.Metadata, want.Metadata) || len(diff) > 0, nil
			},
			replace: func(ctx context.Context, live kubernetes.NetworkPolicy) (kubernetes.NetworkPolicy, error) {
				n := want
				n.Metadata.UID, n.Metadata.ResourceVersion = live.Metadata.UID, live.Metadata.ResourceVersion
				return x.cl().ReplaceNetworkPolicy(ctx, x.ns, n)
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- deployment ----

func (x *pass) ensureDeployment(ctx context.Context) (kubernetes.Deployment, error) {
	want := x.objs.Deployment
	want.Metadata = stamp(want.Metadata, x.nsUID)
	return converge(ctx, x, kind[kubernetes.Deployment]{
		what: "worker deployment",
		get: func(ctx context.Context) (kubernetes.Deployment, error) {
			return x.cl().GetDeployment(ctx, x.ns, DeploymentName)
		},
		create: func(ctx context.Context) (kubernetes.Deployment, error) {
			return x.cl().CreateDeployment(ctx, x.ns, want)
		},
		verify: func(live kubernetes.Deployment) (bool, error) {
			if err := x.chain().verifyMeta(live.Metadata, DeploymentName, RoleDeployment, true); err != nil {
				return false, err
			}
			diff := deploymentDiff(want, live)
			x.notes = append(x.notes, diff...)
			return !current(live.Metadata, want.Metadata) || len(diff) > 0, nil
		},
		// A replacement keeps the Deployment's UID, never touches the claim and, by
		// the Recreate strategy, stops the old Pod before the new one starts.
		replace: func(ctx context.Context, live kubernetes.Deployment) (kubernetes.Deployment, error) {
			n := want
			n.Metadata.UID, n.Metadata.ResourceVersion = live.Metadata.UID, live.Metadata.ResourceVersion
			return x.cl().ReplaceDeployment(ctx, x.ns, n)
		},
	})
}

// ---- storage ----

// preflightStorage refuses a class that cannot give the tenant a volume that
// survives its worker: the class must exist, retain its volumes and be served by
// a provisioner the installation declared ReadWriteOncePod-capable. Anything it
// cannot establish is a refusal, not a guess.
func (x *pass) preflightStorage(ctx context.Context) error {
	sc, err := x.cl().GetStorageClass(ctx, x.spec.Storage.Class)
	switch {
	case notFound(err):
		return wrap(ErrStorage, "the storage class does not exist")
	case err != nil:
		return unavailable(err, "reading the storage class")
	}
	if sc.ReclaimPolicy != "Retain" {
		return wrap(ErrStorage, "the storage class does not retain its volumes")
	}
	if !slices.Contains(x.r.opts.RWOPProvisioners, sc.Provisioner) || sc.Provisioner == "" {
		return wrap(ErrStorage, "the storage class provisioner is not declared ReadWriteOncePod-capable")
	}
	return nil
}

// checkVolume verifies, once the claim is bound, that the volume behind it is
// the claim's own and retained: a class that changed its policy, or a volume
// edited afterwards, would delete the repositories with the claim.
func (x *pass) checkVolume(ctx context.Context) error {
	p, err := x.cl().GetPVC(ctx, x.ns, ClaimName)
	if err != nil {
		return unavailable(err, "reading the volume claim")
	}
	if p.Status.Phase != "Bound" || p.Spec.VolumeName == "" {
		return nil // not bound yet (WaitForFirstConsumer): the worker Pod binds it
	}
	pv, err := x.cl().GetPV(ctx, p.Spec.VolumeName)
	if err != nil {
		return unavailable(err, "reading the volume")
	}
	if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != p.Metadata.UID {
		return wrap(ErrStorage, "the volume is not the claim's")
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != "Retain" {
		return wrap(ErrStorage, "the volume would be deleted with its claim")
	}
	return nil
}

// ---- isolation and readiness ----

// checkIsolation refuses a namespace that holds anything beside the worker's own
// policies and Pods: a further NetworkPolicy is additive and could admit another
// peer, and a Pod that is not the Deployment's would share the namespace (and
// claim the volume). It reads only; it runs before the identity is delivered and
// before the worker is created or replaced, and again through observe after.
func (x *pass) checkIsolation(ctx context.Context) error {
	if err := x.checkPolicies(ctx); err != nil {
		return err
	}
	// The Deployment, when it exists and verifies, is the root of the Pod chain.
	uid := ""
	d, err := x.cl().GetDeployment(ctx, x.ns, DeploymentName)
	switch {
	case notFound(err):
	case err != nil:
		return unavailable(err, "reading the worker deployment")
	default:
		// A Deployment that does not verify is reported by its own step; here it only
		// cannot be the root of a Pod chain.
		if err := x.chain().verifyMeta(d.Metadata, DeploymentName, RoleDeployment, true); err != nil {
			if errors.Is(err, ErrUnavailable) {
				x.dirty = true // going away: contrary evidence
			}
			return err
		}
		uid = d.Metadata.UID
	}
	_, err = x.workerPods(ctx, uid)
	return err
}

// workerPods lists the namespace's Pods and refuses any that is not controlled by
// a ReplicaSet controlled by the Deployment with this UID ("" matches none).
func (x *pass) workerPods(ctx context.Context, deploymentUID string) ([]kubernetes.Pod, error) {
	rs, err := x.cl().ListReplicaSets(ctx, x.ns, "")
	if err != nil {
		return nil, unavailable(err, "listing the replica sets")
	}
	owned := map[string]bool{}
	for _, s := range rs {
		o := s.Metadata.OwnerReferences
		if deploymentUID != "" && len(o) == 1 && o[0].Controlled() && o[0].Kind == "Deployment" && o[0].Name == DeploymentName && o[0].UID == deploymentUID {
			owned[s.Metadata.UID] = true
		}
	}
	pods, err := x.cl().ListPods(ctx, x.ns, "")
	if err != nil {
		return nil, unavailable(err, "listing the pods")
	}
	for _, p := range pods {
		o := p.Metadata.OwnerReferences
		if len(o) != 1 || !o[0].Controlled() || o[0].Kind != "ReplicaSet" || !owned[o[0].UID] {
			return nil, wrap(ErrIsolation, "the namespace holds a Pod that is not the worker's")
		}
	}
	return pods, nil
}

// observe reports readiness from the Deployment's status and the Pods behind it.
func (x *pass) observe(ctx context.Context, dep kubernetes.Deployment) (bool, string, error) {
	if err := x.checkPolicies(ctx); err != nil {
		return false, "", err
	}
	pods, err := x.workerPods(ctx, dep.Metadata.UID)
	if err != nil {
		return false, "", err
	}
	ready := 0
	for _, p := range pods {
		if p.Metadata.DeletionTimestamp == "" && podReady(p) {
			ready++
		}
	}
	if len(pods) > 1 {
		// One Pod only: during a Recreate rollout the old Pod is gone before the new
		// one exists, so two Pods are a replacement still in progress.
		return false, "more than one worker Pod exists", nil
	}
	s := dep.Status
	switch {
	case s.ObservedGeneration < dep.Generation:
		return false, "the worker rollout is not observed yet", nil
	case s.UpdatedReplicas != 1 || s.Replicas != 1:
		return false, "the worker is being replaced", nil
	case s.ReadyReplicas != 1 || s.AvailableReplicas != 1 || ready != 1:
		return false, "the worker is not ready", nil
	}
	return true, "", nil
}

// checkPolicies is the policy half of checkIsolation, repeated after the worker
// was created: nothing foreign may have appeared meanwhile.
func (x *pass) checkPolicies(ctx context.Context) error {
	pols, err := x.cl().ListNetworkPolicies(ctx, x.ns)
	if err != nil {
		return unavailable(err, "listing the network policies")
	}
	own := []string{PolicyDenyAll, PolicyIngress, PolicyEgress}
	for _, p := range pols {
		if !slices.Contains(own, p.Metadata.Name) {
			return wrap(ErrIsolation, "the namespace holds a network policy this package did not create")
		}
	}
	return nil
}

func podReady(p kubernetes.Pod) bool {
	if p.Status.Phase != "Running" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
