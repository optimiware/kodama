package runtimeauth

import (
	"regexp"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Run-resource subjects (T-113, runtime v2). Only a run's one-shot preparation
// workload and its support services have a runtime identity. They are not
// attempts: their grants live in their own table, their tokens carry their own
// audience, and the authority of each kind is a closed list (see ResourceOp).
// The caller's claim of which workload it is, is evidence; execution's
// committed (name, bound UID) is the authority.

var (
	resourceKeyPattern = regexp.MustCompile(`^(preparation|service\.[a-z][a-z0-9-]{0,29})$`)
	// workloadUIDPattern admits a provider's opaque instance identity: a Kubernetes
	// UID, a container ID or a process start tag. It matches the pattern of the
	// execution module's resource UID and the v2 wire's WorkloadUID.
	workloadUIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	workloadNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
	fingerprintPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ResourceKindOf returns the kind of a resource key. Only the two keys that have
// a runtime identity are valid; anything else (workspace, network, a malformed
// name) is refused rather than mapped onto another subject.
func ResourceKindOf(key string) (runtimev2.ResourceKind, error) {
	if !resourceKeyPattern.MatchString(key) {
		return "", fail(ErrInvalidRequest, "the resource key has no runtime identity")
	}
	if key == string(runtimev2.ResourcePreparation) {
		return runtimev2.ResourcePreparation, nil
	}
	return runtimev2.ResourceService, nil
}

// ResourceRef addresses one preparation or service of a run.
type ResourceRef struct {
	Scope domain.Scope
	RunID domain.RunID
	Key   string
}

// Validate checks the ownership and the key.
func (r ResourceRef) Validate() error {
	if err := firstErr(r.Scope.Validate(), r.RunID.Validate()); err != nil {
		return fail(ErrInvalidRequest, "%v", err)
	}
	_, err := ResourceKindOf(r.Key)
	return err
}

// WorkloadIdentity is the instance a resource process observes about itself, as
// admitted. PodName and PodUID are both empty or both set (a controller-owned
// Pod only).
type WorkloadIdentity struct {
	Name    string
	UID     string
	PodName string
	PodUID  string
}

// IdentityOf converts the wire identity.
func IdentityOf(i runtimev2.ResourceIdentity) WorkloadIdentity {
	w := WorkloadIdentity{Name: i.WorkloadName, UID: i.WorkloadUID}
	if i.PodName != nil && i.PodUID != nil {
		w.PodName, w.PodUID = *i.PodName, *i.PodUID
	}
	return w
}

// HasPod reports whether a controller-owned Pod is claimed.
func (w WorkloadIdentity) HasPod() bool { return w.PodName != "" || w.PodUID != "" }

// Validate checks syntax.
func (w WorkloadIdentity) Validate() error {
	if !workloadUIDPattern.MatchString(w.UID) || !workloadNamePattern.MatchString(w.Name) {
		return fail(ErrForbidden, "the workload identity is malformed")
	}
	if (w.PodName == "") != (w.PodUID == "") {
		return fail(ErrForbidden, "the workload identity is malformed")
	}
	if w.HasPod() && (!workloadNamePattern.MatchString(w.PodName) || !workloadUIDPattern.MatchString(w.PodUID)) {
		return fail(ErrForbidden, "the workload identity is malformed")
	}
	return nil
}

// ResourceConsumption is the durable result of a resource's first admission.
type ResourceConsumption struct {
	NonceDigest  Digest
	Workload     WorkloadIdentity
	SessionID    SessionID
	ConsumedAt   time.Time
	MaxExpiresAt time.Time
}

// ResourceGrant is the entity behind one resource's runtime authority: a
// one-time credential registered by the trusted adapter, then at most one
// session. Everything except Consumption and RevokedAt is fixed at registration;
// Consumption is written once; RevokedAt is written once and is irreversible.
// Expiry never revives anything.
type ResourceGrant struct {
	Scope domain.Scope
	RunID domain.RunID
	Key   string
	Kind  runtimev2.ResourceKind

	CredentialDigest Digest
	// Generation is the signing generation current at registration.
	Generation int64
	// SpecFingerprint is the committed resource record's fingerprint the credential
	// was registered for. A changed spec is a different resource.
	SpecFingerprint string
	// DeadlineAt is the resource's deadline: no token and no session outlives it.
	DeadlineAt time.Time
	// ExpiresAt bounds the first admission only.
	ExpiresAt time.Time
	CreatedAt time.Time

	Consumption *ResourceConsumption
	RevokedAt   *time.Time
}

// ResourceProvision is the input of NewResourceGrant.
type ResourceProvision struct {
	Ref              ResourceRef
	CredentialDigest Digest
	Generation       int64
	SpecFingerprint  string
	DeadlineAt       time.Time
}

// NewResourceGrant validates ownership and bindings and creates an unconsumed
// grant whose admission window is the policy's admission lifetime, never beyond
// the resource's deadline.
func NewResourceGrant(p ResourceProvision, now time.Time, pol Policy) (ResourceGrant, error) {
	if err := pol.Validate(); err != nil {
		return ResourceGrant{}, failWith(ErrInvalidRequest, err, "invalid policy")
	}
	if err := p.Ref.Validate(); err != nil {
		return ResourceGrant{}, err
	}
	kind, _ := ResourceKindOf(p.Ref.Key)
	created := micros(now)
	deadline := micros(p.DeadlineAt)
	switch {
	case p.CredentialDigest.IsZero():
		return ResourceGrant{}, fail(ErrInvalidRequest, "a credential digest is required")
	case p.Generation < 1:
		return ResourceGrant{}, fail(ErrInvalidRequest, "a signing generation is required")
	case !fingerprintPattern.MatchString(p.SpecFingerprint):
		return ResourceGrant{}, fail(ErrInvalidRequest, "a resource spec fingerprint is required")
	case !deadline.After(created):
		return ResourceGrant{}, fail(ErrInvalidRequest, "the resource deadline has passed")
	}
	expires := created.Add(pol.AdmissionTTL)
	if expires.After(deadline) {
		expires = deadline
	}
	return ResourceGrant{
		Scope: p.Ref.Scope, RunID: p.Ref.RunID, Key: p.Ref.Key, Kind: kind,
		CredentialDigest: p.CredentialDigest, Generation: p.Generation, SpecFingerprint: p.SpecFingerprint,
		DeadlineAt: deadline, CreatedAt: created, ExpiresAt: expires,
	}, nil
}

// Ref returns the resource the grant belongs to.
func (g ResourceGrant) Ref() ResourceRef {
	return ResourceRef{Scope: g.Scope, RunID: g.RunID, Key: g.Key}
}

// SameRegistration reports whether o registers exactly the same immutable
// authority. Expiry and creation time are excluded: a retried registration does
// not extend the stored window.
func (g ResourceGrant) SameRegistration(o ResourceGrant) bool {
	return g.Ref() == o.Ref() && g.Kind == o.Kind && g.CredentialDigest.Equal(o.CredentialDigest) &&
		g.Generation == o.Generation && g.SpecFingerprint == o.SpecFingerprint && g.DeadlineAt.Equal(o.DeadlineAt)
}

// Consumed reports whether the credential has been exchanged.
func (g ResourceGrant) Consumed() bool { return g.Consumption != nil }

// Revoked reports irreversible denial.
func (g ResourceGrant) Revoked() bool { return g.RevokedAt != nil }

// ResourceAdmission is the result of Consume.
type ResourceAdmission struct {
	ResourceConsumption
	// Replayed is true when an earlier admission with the same nonce and workload
	// identity was repeated.
	Replayed bool
}

// Consume applies an admission exchange at now.
//
//   - revoked: ErrResourceRevoked, always, first;
//   - first exchange: ErrInvalidCredential when expired, else consumes, fixing the
//     session, the workload binding and the maximum expiry (never beyond the
//     resource's deadline);
//   - same nonce, same workload: bounded replay returning the stored consumption
//     until the maximum expiry (the maximum never moves);
//   - same nonce, another workload: ErrForbidden;
//   - another nonce: ErrAdmissionConsumed.
func (g *ResourceGrant) Consume(nonce Digest, w WorkloadIdentity, session SessionID, now time.Time, pol Policy) (ResourceAdmission, error) {
	now = micros(now)
	if g.Revoked() {
		return ResourceAdmission{}, fail(ErrResourceRevoked, "the resource is no longer authorized")
	}
	if g.Consumption != nil {
		c := *g.Consumption
		if !c.NonceDigest.Equal(nonce) {
			return ResourceAdmission{}, fail(ErrAdmissionConsumed, "admission was already consumed")
		}
		if c.Workload != w {
			return ResourceAdmission{}, fail(ErrForbidden, "the workload identity differs from the admitted one")
		}
		if !now.Before(c.MaxExpiresAt) {
			return ResourceAdmission{}, fail(ErrInvalidCredential, "the session reached its maximum lifetime")
		}
		return ResourceAdmission{ResourceConsumption: c, Replayed: true}, nil
	}
	if err := w.Validate(); err != nil {
		return ResourceAdmission{}, err
	}
	if err := session.Validate(); err != nil {
		return ResourceAdmission{}, fail(ErrInvalidRequest, "a session reference is required")
	}
	if !now.Before(g.ExpiresAt) {
		return ResourceAdmission{}, fail(ErrInvalidCredential, "the admission credential expired")
	}
	limit := now.Add(pol.MaxSession)
	if limit.After(g.DeadlineAt) {
		limit = g.DeadlineAt
	}
	c := ResourceConsumption{NonceDigest: nonce, Workload: w, SessionID: session, ConsumedAt: now, MaxExpiresAt: limit}
	g.Consumption = &c
	return ResourceAdmission{ResourceConsumption: c}, nil
}

// Revoke irreversibly denies the grant. It reports whether this call changed the
// state; repeating it keeps the first revocation time.
func (g *ResourceGrant) Revoke(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	t := micros(now)
	if t.Before(g.CreatedAt) {
		t = g.CreatedAt
	}
	g.RevokedAt = &t
	return true
}

// Authorize decides whether the session is usable at now. It checks only the
// grant's own state; the caller combines it with the committed resource state
// and the kind's operation list.
func (g ResourceGrant) Authorize(now time.Time) error {
	if g.Revoked() {
		return fail(ErrResourceRevoked, "the resource is no longer authorized")
	}
	if g.Consumption == nil {
		return fail(ErrInvalidCredential, "the grant has no session")
	}
	if !micros(now).Before(g.Consumption.MaxExpiresAt) {
		return fail(ErrResourceRevoked, "the resource deadline or the session maximum was reached")
	}
	return nil
}

// TokenExpiry is now plus ttl, clamped to the session maximum (itself never
// beyond the resource's deadline).
func (g ResourceGrant) TokenExpiry(now time.Time, ttl time.Duration) (time.Time, error) {
	if g.Consumption == nil {
		return time.Time{}, fail(ErrInvalidCredential, "the grant has no session")
	}
	exp := now.UTC().Add(ttl).Truncate(time.Second)
	if exp.After(g.Consumption.MaxExpiresAt) {
		exp = g.Consumption.MaxExpiresAt.Truncate(time.Second)
	}
	if !exp.After(now.UTC()) {
		return time.Time{}, fail(ErrResourceRevoked, "the resource deadline or the session maximum was reached")
	}
	return exp, nil
}

// ResourceOp is one operation a resource token may be used for. The set is
// closed: a preparation and a service each have a short fixed list, and
// everything else (attempt routes, source, result, artifacts, begin, model/tool
// proxies, other resources) is not an operation of any resource.
type ResourceOp string

const (
	OpResourceRenew    ResourceOp = "token.renew"
	OpResourceContext  ResourceOp = "context.read"
	OpResourceSecrets  ResourceOp = "secret.resolve"
	OpResourceComplete ResourceOp = "preparation.complete"
)

// OpAllowed reports whether a resource of the kind may perform the operation:
// the preparation renews, reads its own context and completes its checkout; a
// service renews, reads its own context and resolves its pinned bindings.
func OpAllowed(kind runtimev2.ResourceKind, op ResourceOp) bool {
	switch op {
	case OpResourceRenew, OpResourceContext:
		return kind == runtimev2.ResourcePreparation || kind == runtimev2.ResourceService
	case OpResourceSecrets:
		return kind == runtimev2.ResourceService
	case OpResourceComplete:
		return kind == runtimev2.ResourcePreparation
	}
	return false
}
