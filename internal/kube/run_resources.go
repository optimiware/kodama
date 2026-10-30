package kube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// RunResources implements execution.RunResources over a tenant namespace: the
// run's network policies, its scratch claim, its one preparation Job and its
// support-service Pods with their Services. It acts only on objects it can prove
// belong to the committed handle: scope, deterministic name, the pinned spec's
// fingerprint and the bound instance UID. A same-name object that is not
// ours is a collision, never adopted, stopped or removed.
//
// Every resource has a record Secret created before anything else (intent
// first, run_record.go). The record outlives the workload until Remove, fences
// the resource once Stop began and makes a lost create resolvable by
// quiescence. Kubernetes is never called inside an SQL transaction and every
// method may be repeated after a lost response or a controller restart.
//
// The adapter enforces the pinned policy exactly as rendered (the step,
// service and preparation roles, the storage class, the network model) and
// advertises only the capabilities it renders for every run (Capabilities).
// Real CSI and CNI behavior (release of a mount, DNAT with policies) is
// qualified separately (T-121, T-117); nothing here replaces that evidence.
type RunResources struct {
	cfg      ExecutionPolicy
	cl       RunCluster
	catalog  PolicyCatalog
	reg      ResourceRegistrar
	checkout CheckoutProvider
	logs     PodLogs
	resolver HostResolver
	pre      *Preflight
	opts     Options
	except   []string
	caps     execution.ResourceCapabilitySet
	log      *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	bodies map[execution.PolicyPin]execution.ExecutionPolicy
}

var _ execution.RunResources = (*RunResources)(nil)

// RunResourceDependencies are the collaborators of the adapter. Policy, Cluster
// and Catalog are required; Registrar is required for any run with a preparation
// or a service with secret bindings; Checkout for a source-backed run.
type RunResourceDependencies struct {
	Policy    ExecutionPolicy
	Cluster   RunCluster
	Catalog   PolicyCatalog
	Registrar ResourceRegistrar
	Checkout  CheckoutProvider
	// Logs reads Pod logs for Logs. Optional: without it Logs reports not configured.
	Logs PodLogs
	// Resolver pins the addresses of hostname egress destinations. Optional:
	// without it a profile naming a host (rather than an address or block) is refused.
	Resolver HostResolver
}

// RunResourceOptions are mechanical settings.
type RunResourceOptions struct {
	Options
	// ClusterCIDRs are further address blocks public egress never reaches (pod,
	// Service and node ranges that are not already private).
	ClusterCIDRs []string
	// Qualified lists the capabilities the installation vouches for after real
	// qualification (T-121): only builder_rootless is accepted; every other member
	// is either advertised by the adapter itself or never advertised.
	Qualified execution.ResourceCapabilitySet
}

// NewRunResources validates the dependencies and options.
func NewRunResources(d RunResourceDependencies, o RunResourceOptions) (*RunResources, error) {
	opts, err := o.Options.withDefaults()
	if err != nil {
		return nil, err
	}
	if !d.Policy.Valid() || d.Cluster == nil || d.Catalog == nil {
		return nil, wrap(ErrPolicy, "a validated execution policy, a cluster and a policy catalog are required")
	}
	var except []string
	for _, c := range o.ClusterCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return nil, wrap(ErrPolicy, "a cluster CIDR is not an IPv4 block")
		}
		except = append(except, p.Masked().String())
	}
	if o.Qualified&^execution.Set(execution.CapBuilderRootless) != 0 {
		return nil, wrap(ErrPolicy, "only builder_rootless may be vouched for by the installation")
	}
	caps := runCapabilities | o.Qualified
	r := &RunResources{
		cfg: d.Policy, cl: d.Cluster, catalog: d.Catalog, reg: d.Registrar, checkout: d.Checkout, logs: d.Logs, resolver: d.Resolver,
		pre: NewPreflight(d.Cluster, d.Policy, opts.PreflightTTL, opts.Clock), opts: opts, except: except, caps: caps,
		log: opts.Logger, now: opts.Clock, bodies: map[execution.PolicyPin]execution.ExecutionPolicy{},
	}
	r.pre.WithOwnedPolicies(r)
	return r, nil
}

// runCapabilities are the members the adapter renders and verifies for every
// run: the claim and its release fence, the preparation Job, recovery by record,
// services with readiness, per-service isolation (own Pod, no workspace, no
// shared identity), the network models of the policy (tenant and run), declared
// edges, named egress and external default deny (nothing beyond the named
// profiles is allowed). builder_service and builder_isolated_worker are never
// members, and builder_rootless only when the installation vouches for it
// (RunResourceOptions.Qualified): the adapter cannot test a build.
var runCapabilities = execution.Set(
	execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation, execution.CapResourceRecovery,
	execution.CapRunServices, execution.CapServiceIsolation, execution.CapRunNetworkIsolation, execution.CapDeclaredEdges,
	execution.CapNamedEgress, execution.CapEgressDefaultDeny,
)

// Capabilities implements execution.RunResources.
func (r *RunResources) Capabilities(context.Context) execution.ResourceCapabilities {
	return execution.ResourceCapabilities{Backend: Backend, Set: r.caps}
}

// ---- policy and target resolution ----

// policyBody resolves a pin to its verified body: the catalog's answer must
// be a valid policy whose id and digest are the pinned ones. A body is cached by
// pin (a pin names immutable content).
func (r *RunResources) policyBody(ctx context.Context, pin execution.PolicyPin) (execution.ExecutionPolicy, error) {
	r.mu.Lock()
	b, ok := r.bodies[pin]
	r.mu.Unlock()
	if ok {
		return b, nil
	}
	body, err := r.catalog.Policy(ctx, pin)
	switch {
	case err == nil:
	case errors.Is(err, ErrPolicyUnavailable):
		return execution.ExecutionPolicy{}, wrap(ErrPolicyUnavailable, "the pinned policy is not retained")
	default:
		return execution.ExecutionPolicy{}, opaque(ErrUnavailable, err, "reading the pinned execution policy")
	}
	d, derr := body.Digest()
	if derr != nil || body.ID != pin.ID || d != pin.Digest {
		return execution.ExecutionPolicy{}, wrap(ErrPolicy, "the retained policy is not the pinned one")
	}
	if err := body.Validate(); err != nil {
		return execution.ExecutionPolicy{}, wrap(ErrPolicy, "the pinned policy is invalid")
	}
	r.mu.Lock()
	r.bodies[pin] = body
	r.mu.Unlock()
	return body, nil
}

// roleOfKind is the policy role whose shape a kind's spec pins.
func roleOfKind(spec execution.ResourceSpec) string {
	switch spec.Kind {
	case execution.KindPreparation:
		return execution.RolePreparation
	case execution.KindService:
		if spec.Service != nil && spec.Service.Role == "builder" {
			return execution.RoleBuilderService
		}
		return execution.RoleService
	}
	return execution.RoleStep
}

// render builds the rendering context of a spec after verifying that the spec is
// the pinned policy's: backend, tenant target, role-shape digest and, for a
// service, that its role is one this adapter provides.
func (r *RunResources) render(ctx context.Context, spec execution.ResourceSpec) (runRender, error) {
	if err := spec.Validate(); err != nil {
		return runRender{}, err
	}
	if spec.Backend != Backend {
		return runRender{}, fmt.Errorf("%w: the spec is pinned to another backend", execution.ErrInvalid)
	}
	tgt, err := r.cfg.Tenant(spec.Scope.TenantID)
	if err != nil {
		return runRender{}, err
	}
	if role := roleOfKind(spec); role == execution.RoleBuilderService {
		return runRender{}, wrap(ErrPolicy, "builder services are not provided by this adapter")
	}
	body, err := r.policyBody(ctx, spec.Policy)
	if err != nil {
		return runRender{}, err
	}
	if d, err := body.RoleDigest(roleOfKind(spec)); err != nil || d != spec.RoleShape {
		return runRender{}, wrap(ErrPolicy, "the spec's role shape is not the pinned policy's")
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return runRender{}, err
	}
	return runRender{cfg: r.cfg.cfg, tgt: tgt, spec: spec, fp: fp, body: body, publicExcept: r.except}, nil
}

// handleIdentity validates a handle and resolves its tenant namespace.
func (r *RunResources) handleTarget(h execution.ResourceHandle) (TenantTarget, error) {
	if err := h.Validate(); err != nil {
		return TenantTarget{}, err
	}
	return r.cfg.Tenant(h.Scope.TenantID)
}

// identOf is the identity annotation set a handle's objects must carry
// (the fingerprint is compared separately, once a spec is known).
func identOf(h execution.ResourceHandle) map[string]string {
	return map[string]string{
		AnnTenant: string(h.Scope.TenantID), AnnProject: string(h.Scope.ProjectID), AnnRun: string(h.RunID),
		AnnResourceKind: string(h.Kind), AnnResourceKey: h.Key, AnnResourceName: h.Name,
	}
}

func ownedOf(h execution.ResourceHandle, uid string) ownedWant {
	return ownedWant{scope: h.Scope, run: h.RunID, kind: h.Kind, key: h.Key, name: h.Name, uid: uid}
}

// ---- error classes ----

func gitCommit(s string) runtimev1.GitCommit { return runtimev1.GitCommit(s) }

func collision(why string) error {
	return fmt.Errorf("%w: %w", execution.ErrResourceCollision, wrap(ErrForeignWorkload, "%s", why))
}

func fenced(why string) error {
	return fmt.Errorf("%w: %w", execution.ErrResourceFenced, wrap(ErrNotDispatchable, "%s", why))
}

// asCollision converts a verification refusal into the port's collision error.
func asCollision(err error) error {
	if errors.Is(err, ErrForeignWorkload) && !errors.Is(err, execution.ErrResourceCollision) {
		return fmt.Errorf("%w: %w", execution.ErrResourceCollision, err)
	}
	return err
}

// unavailable classifies an API failure that concludes nothing.
func unavailable(err error, what string) error { return wrapCause(ErrUnavailable, err, "%s", what) }

func isNotFound(err error) bool { return errors.Is(err, kubernetes.ErrNotFound) }

// ---- record persistence ----

// saveRec persists the record annotation under the Secret's resourceVersion. A
// lost race is errRecMoved; a lost response is resolved by reading back.
func (r *RunResources) saveRec(ctx context.Context, ns string, a *resRec) error {
	want := a.rec.marshal()
	if a.secret.Metadata.Annotations[AnnRecord] == want {
		return nil
	}
	next := a.secret
	next.Metadata.Annotations = map[string]string{}
	for k, v := range a.secret.Metadata.Annotations {
		next.Metadata.Annotations[k] = v
	}
	next.Metadata.Annotations[AnnRecord] = want
	out, err := r.cl.UpdateSecret(ctx, ns, next)
	switch {
	case err == nil:
		a.secret = out
		a.secret.Data = next.Data
		return nil
	case errors.Is(err, kubernetes.ErrConflict), isNotFound(err):
		return errRecMoved
	case errors.Is(err, kubernetes.ErrUnknownOutcome):
		cur, rerr := r.cl.GetSecret(ctx, ns, a.secret.Metadata.Name)
		if rerr != nil {
			return wrapCause(ErrUnavailable, errors.Join(err, rerr), "saving the resource record")
		}
		if cur.Metadata.Annotations[AnnRecord] == want {
			a.secret = cur
			return nil
		}
		return errRecMoved
	}
	return unavailable(err, "saving the resource record")
}

const maxResourceRetries = maxRecordRetries

// retrying repeats an operation that lost a record race.
func retrying[T any](op func() (T, error)) (T, error) {
	var zero T
	for i := 0; i < maxResourceRetries; i++ {
		v, err := op()
		if !errors.Is(err, errRecMoved) {
			return v, err
		}
	}
	return zero, wrap(ErrUnavailable, "the resource record kept changing")
}

// ---- Ensure ----

// Ensure implements execution.RunResources: it creates the resource or adopts the
// one whose record, scope, name, fingerprint and objects verify, and reports it
// with the provider's instance UID. A fenced resource is never created again
// (ErrResourceFenced) and a same-name object or record of another spec or UID is
// ErrResourceCollision.
func (r *RunResources) Ensure(ctx context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	x, err := r.render(ctx, spec)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	if err := x.precheck(); err != nil {
		return execution.ResourceObservation{}, err
	}
	return retrying(func() (execution.ResourceObservation, error) { return r.ensureOnce(ctx, x) })
}

func (r *RunResources) ensureOnce(ctx context.Context, x runRender) (execution.ResourceObservation, error) {
	h := x.spec.Handle("")
	a, err := readRec(ctx, r.cl, x.tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	if err != nil {
		return execution.ResourceObservation{}, asCollision(err)
	}
	created := false
	if a == nil {
		// A removed resource is never created again, whichever controller asks: its
		// removal marker outlives its record (Remove writes it first).
		tomb, err := readTomb(ctx, r.cl, x.tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
		if err != nil {
			return execution.ResourceObservation{}, asCollision(err)
		}
		if tomb != nil {
			return execution.ResourceObservation{}, fenced("the resource was removed and is never created again")
		}
		if err := r.refuseForeign(ctx, x); err != nil {
			return execution.ResourceObservation{}, err
		}
		if err := r.pre.CheckRun(ctx, h.Scope.TenantID, x.body.PodSecurity); err != nil {
			return execution.ResourceObservation{}, err
		}
		if a, err = r.createRec(ctx, x); err != nil {
			return execution.ResourceObservation{}, err
		}
		created = true
	}
	// The removal marker is read again once the record exists and before any effect:
	// a Stop and Remove that ran between the first read and the record create wrote
	// the marker first and deleted the record they saw, so a record created now is a
	// resurrection attempt. It is deleted (it is ours, never used) or, when it is
	// someone else's, fenced.
	if tomb, err := readTomb(ctx, r.cl, x.tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name); err != nil {
		return execution.ResourceObservation{}, asCollision(err)
	} else if tomb != nil {
		if created {
			err := r.cl.DeleteSecret(ctx, x.tgt.Namespace, recName(h.Name), kubernetes.DeleteOptions{UID: a.secret.Metadata.UID})
			if err == nil || isNotFound(err) || errors.Is(err, kubernetes.ErrConflict) {
				return execution.ResourceObservation{}, fenced("the resource was removed while it was being planned")
			}
		}
		if a.rec.fence(r.now()) {
			if err := r.saveRec(ctx, x.tgt.Namespace, a); err != nil {
				return execution.ResourceObservation{}, err
			}
		}
		return execution.ResourceObservation{}, fenced("the resource was removed and is never created again")
	}
	if a.rec.Fenced {
		return execution.ResourceObservation{}, fenced("the resource was stopped and is never started again")
	}
	if a.secret.Metadata.Annotations[AnnFingerprint] != x.fp || !specMatches(a.secret.Data[KeyResourceSpec], x.spec) {
		return execution.ResourceObservation{}, collision("a record of another spec holds the resource name")
	}
	if x.spec.Kind.HasIdentity() && a.rec.Phase == recPlanned {
		if err := r.register(ctx, x, a); err != nil {
			return execution.ResourceObservation{}, err
		}
	}
	if err := r.pre.CheckRun(ctx, h.Scope.TenantID, x.body.PodSecurity); err != nil {
		return execution.ResourceObservation{}, err
	}
	if err := r.provision(ctx, x, a); err != nil {
		return execution.ResourceObservation{}, err
	}
	return r.observeVerified(ctx, x, a, h)
}

// refuseForeign refuses to plan a resource when an object already holds the name
// of its primary object: with no record of ours it is someone else's.
func (r *RunResources) refuseForeign(ctx context.Context, x runRender) error {
	found, err := r.primaryExists(ctx, x)
	if err != nil {
		return err
	}
	if found {
		return collision("an object without a record of ours holds the resource name")
	}
	return nil
}

// createRec creates the resource's record Secret: spec, effect record and, for a
// workload with a runtime identity, the admission credential (and the
// preparation's read-only checkout material). It is created before any other
// object. A concurrent winner is read back and used instead (its stored
// credential, never ours); a lost response is resolved by reading.
func (r *RunResources) createRec(ctx context.Context, x runRender) (*resRec, error) {
	spec := x.spec
	canonical, err := spec.Canonical()
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{KeyResourceSpec: canonical}
	rec := &resRecord{Version: 1, Name: spec.Name, Phase: recRegistered}
	if spec.Kind.HasIdentity() {
		rec.Phase = recPlanned
		if spec.Kind == execution.KindService && !usesSupervisor(spec.Service) {
			// A service without secret bindings runs its declared image and argv
			// directly: it has no credential to register.
			rec.Phase = recRegistered
		} else {
			if r.reg == nil {
				return nil, wrap(ErrPolicy, "no resource registrar is configured")
			}
			cred, err := runtimeauth.GenerateCredential(nil)
			if err != nil {
				return nil, wrapCause(ErrUnavailable, err, "no randomness for the credential")
			}
			data[KeyAdmission], data[KeyCA] = []byte(cred.Reveal()), r.cfg.RuntimeCA()
			if spec.Kind == execution.KindPreparation {
				m, err := r.checkoutFor(ctx, spec)
				if err != nil {
					return nil, err
				}
				data[KeyCheckoutCredential], data[KeyGitCA] = m.Credential, m.GitCA
			}
		}
	}
	if spec.Kind == execution.KindNetwork {
		resolved, err := resolveHosts(ctx, r.resolver, profileHosts(spec.Network, x.body))
		if err != nil {
			return nil, err
		}
		if len(resolved) > 0 {
			b, _ := marshalResolved(resolved)
			data[KeyResolved] = b
		}
	}
	ann := identityAnnotations(spec, x.fp)
	ann[AnnRecord] = rec.marshal()
	s := kubernetes.Secret{
		Metadata:  kubernetes.ObjectMeta{Name: recName(spec.Name), Namespace: x.tgt.Namespace, Labels: ownerLabels(spec.Scope, spec.RunID, spec.Kind), Annotations: ann},
		Type:      "Opaque",
		Immutable: ptr(true),
		Data:      data,
	}
	_, err = r.cl.CreateSecret(ctx, x.tgt.Namespace, s)
	switch {
	case err == nil, errors.Is(err, kubernetes.ErrAlreadyExists), errors.Is(err, kubernetes.ErrUnknownOutcome):
		h := spec.Handle("")
		a, rerr := readRec(ctx, r.cl, x.tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
		if rerr != nil {
			return nil, asCollision(rerr)
		}
		if a == nil {
			return nil, wrapCause(ErrUnavailable, errors.New("record not readable after create"), "the resource record")
		}
		return a, nil
	case errors.Is(err, kubernetes.ErrForbidden), errors.Is(err, kubernetes.ErrInvalid):
		return nil, wrapCause(ErrPreflight, err, "the resource record was refused")
	}
	return nil, unavailable(err, "creating the resource record")
}

// checkoutFor asks the checkout provider once for the preparation's read-only
// credential and validates its answer like an attempt's.
func (r *RunResources) checkoutFor(ctx context.Context, spec execution.ResourceSpec) (*CheckoutMaterial, error) {
	if r.checkout == nil {
		return nil, wrap(ErrCheckoutDenied, "no checkout credential provider is configured")
	}
	p := spec.Preparation
	// The request type is shared with the attempt runtimes (field for field), so a
	// preparation is asked for as a step of the run named by its resource key with
	// no attempt and no workspace binding: the credential is read-only for the
	// run's pinned repository and commit.
	req := CheckoutRequest{
		Scope: spec.Scope, RunID: spec.RunID, StepID: spec.Key, Access: execution.AccessRead,
		RepositoryURL: p.RepositoryURL, Commit: gitCommit(p.Commit),
	}
	m, err := r.checkout.Checkout(ctx, req)
	switch {
	case err == nil:
	case errors.Is(err, ErrCheckoutDenied):
		return nil, wrap(ErrCheckoutDenied, "the provider refused the request")
	default:
		return nil, opaque(ErrUnavailable, err, "the checkout credential provider failed")
	}
	if m.Scope != req.Scope || m.WorkspaceID != "" || m.RepositoryURL != req.RepositoryURL {
		return nil, wrap(ErrCheckoutDenied, "the provider answered for another scope or repository")
	}
	if err := validCheckoutCredential(m.Credential); err != nil {
		return nil, wrap(ErrCheckoutDenied, "%v", err)
	}
	if err := validatePEMCerts(m.GitCA, "Git CA"); err != nil {
		return nil, wrap(ErrCheckoutDenied, "%v", err)
	}
	return &CheckoutMaterial{
		Scope: m.Scope, RepositoryURL: m.RepositoryURL,
		Credential: append([]byte(nil), m.Credential...), GitCA: append([]byte(nil), m.GitCA...),
	}, nil
}

// register registers the stored credential once the record holds it (seam S5):
// persisted before registered, repeated with the identical value after a lost
// response, never regenerated.
func (r *RunResources) register(ctx context.Context, x runRender, a *resRec) error {
	if r.reg == nil {
		return wrap(ErrPolicy, "no resource registrar is configured")
	}
	cred, err := runtimeauth.ParseCredential(string(a.secret.Data[KeyAdmission]))
	if err != nil {
		return wrap(ErrCredentialConflict, "the stored credential is malformed")
	}
	deadline, err := time.Parse(time.RFC3339, x.spec.DeadlineAt)
	if err != nil {
		return fmt.Errorf("%w: the resource has no deadline", execution.ErrInvalid)
	}
	err = r.reg.RegisterResource(ctx, ResourceRegisterCommand{
		Scope: x.spec.Scope, RunID: x.spec.RunID, Key: x.spec.Key, Credential: cred, SpecFingerprint: x.fp, ExpiresAt: deadline,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrRegistrationRevoked):
		return fenced("the resource is no longer authorized")
	case errors.Is(err, ErrRegistrationConflict):
		return wrap(ErrCredentialConflict, "a different registration exists for the resource")
	default:
		return opaque(ErrUnavailable, err, "registering the resource")
	}
	if err := a.rec.registered(); err != nil {
		return err
	}
	return r.saveRec(ctx, x.tgt.Namespace, a)
}

// ---- Observe ----

// Observe implements execution.RunResources. It never creates and never
// reports a resource absent unless the object (and its record) are definitely
// not there: an outage, an unreadable policy or an unverifiable object is
// PhaseUnknown.
func (r *RunResources) Observe(ctx context.Context, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	tgt, err := r.handleTarget(h)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	a, err := readRec(ctx, r.cl, tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil && errors.Is(err, ErrForeignWorkload):
		return execution.ResourceObservation{}, asCollision(err)
	case err != nil:
		return r.unknown(h, reasonUnobservable), nil
	case a == nil:
		return r.observeNoRecord(ctx, tgt, h)
	}
	if h.UID != "" && a.rec.UID != "" && h.UID != a.rec.UID {
		return execution.ResourceObservation{}, collision("the resource record binds another instance")
	}
	if len(a.secret.Data[KeyResourceSpec]) == 0 {
		// A tombstone: Stop fenced a resource that was never planned.
		return execution.ResourceObservation{Handle: h, Phase: execution.PhaseStopped, ObservedAt: r.now().UTC()}, nil
	}
	spec, err := a.spec()
	if err != nil {
		return execution.ResourceObservation{}, asCollision(err)
	}
	if spec.Handle("").Name != h.Name || spec.Kind != h.Kind || spec.Key != h.Key || spec.Scope != h.Scope || spec.RunID != h.RunID {
		return execution.ResourceObservation{}, collision("the stored spec is not this resource's")
	}
	x, err := r.render(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrPolicyUnavailable) {
			return r.unknown(h, execution.ReasonPolicyUnavailable), nil
		}
		if errors.Is(err, ErrUnavailable) {
			return r.unknown(h, reasonUnobservable), nil
		}
		return execution.ResourceObservation{}, err
	}
	if x.fp != a.secret.Metadata.Annotations[AnnFingerprint] {
		return execution.ResourceObservation{}, collision("the stored spec does not match its fingerprint")
	}
	return r.observeVerified(ctx, x, a, h)
}

// Reasons execution's frozen vocabulary spells but does not export.
const (
	reasonUnobservable execution.ResourceReason = "resource_unobservable"
	reasonUnverified   execution.ResourceReason = "resource_unverified"
)

func (r *RunResources) unknown(h execution.ResourceHandle, reason execution.ResourceReason) execution.ResourceObservation {
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseUnknown, Reason: reason, ObservedAt: r.now().UTC()}
}

// observeNoRecord: with no record, an absent primary object is a definite
// absence; one that exists is somebody else's (collision), and anything
// unreadable is unknown.
func (r *RunResources) observeNoRecord(ctx context.Context, tgt TenantTarget, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	tomb, err := readTomb(ctx, r.cl, tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil && errors.Is(err, ErrForeignWorkload):
		return execution.ResourceObservation{}, asCollision(err)
	case err != nil:
		return r.unknown(h, reasonUnobservable), nil
	case tomb != nil:
		if h.UID != "" && tomb.rec.UID != "" && h.UID != tomb.rec.UID {
			return execution.ResourceObservation{}, collision("the removal marker binds another instance")
		}
		return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: r.now().UTC()}, nil
	}
	found, err := r.primaryExistsOf(ctx, tgt.Namespace, h)
	switch {
	case err != nil:
		return r.unknown(h, reasonUnobservable), nil
	case found:
		return execution.ResourceObservation{}, collision("an object without a record of ours holds the resource name")
	}
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: r.now().UTC()}, nil
}

// ---- Logs ----

// maxLogBytes bounds a log request.
const maxLogBytes = 1 << 20

// Logs implements execution.RunResources: a bounded tail of the preparation's or
// service's primary container, with the resource's own credential values removed.
// It is denied for the workspace and the network.
func (r *RunResources) Logs(ctx context.Context, h execution.ResourceHandle, bound execution.LogBound) (execution.ResourceLogs, error) {
	tgt, err := r.handleTarget(h)
	if err != nil {
		return execution.ResourceLogs{}, err
	}
	if h.Kind != execution.KindPreparation && h.Kind != execution.KindService {
		return execution.ResourceLogs{}, execution.ErrDenied
	}
	if r.logs == nil {
		return execution.ResourceLogs{}, fmt.Errorf("%w: no Pod log reader is configured", execution.ErrNotConfigured)
	}
	a, err := readRec(ctx, r.cl, tgt.Namespace, identOf(h), ownerLabels(h.Scope, h.RunID, h.Kind), h.Name)
	switch {
	case err != nil && errors.Is(err, ErrForeignWorkload):
		return execution.ResourceLogs{}, asCollision(err)
	case err != nil:
		return execution.ResourceLogs{}, err
	case a == nil:
		return execution.ResourceLogs{}, fmt.Errorf("%w: the resource has no record", execution.ErrNotFound)
	}
	pods, err := r.ownedPods(ctx, tgt.Namespace, h, a.rec.UID, h.UID)
	if err != nil {
		return execution.ResourceLogs{}, err
	}
	if len(pods) == 0 {
		return execution.ResourceLogs{}, fmt.Errorf("%w: the resource has no Pod yet", execution.ErrNotFound)
	}
	raw, err := r.logs.PodLog(ctx, tgt.Namespace, pods[len(pods)-1].Metadata.Name, PrimaryContainer)
	if err != nil {
		return execution.ResourceLogs{}, unavailable(err, "reading the Pod log")
	}
	max := bound.MaxBytes
	if max <= 0 || max > maxLogBytes {
		max = maxLogBytes
	}
	content := redact(string(raw), a.secret.Data)
	out := execution.ResourceLogs{Content: content}
	if int64(len(content)) > max {
		out.Content, out.Truncated = content[int64(len(content))-max:], true
	}
	return out, nil
}

// redact removes the resource's credential values from log text.
func redact(s string, data map[string][]byte) string {
	for _, k := range []string{KeyAdmission, KeyCheckoutCredential} {
		if v := string(data[k]); v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	if v := string(data[KeyCheckoutCredential]); v != "" {
		if _, pass, ok := strings.Cut(v, ":"); ok && pass != "" {
			s = strings.ReplaceAll(s, pass, "[redacted]")
		}
	}
	return s
}
