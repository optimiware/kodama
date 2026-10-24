package runtimeauth

import (
	"bytes"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

var (
	kubeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	uidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ExpectedJob is the Job the executor created for the attempt, known before
// the Job exists: its namespace and deterministic name. The Job's UID is
// assigned by Kubernetes and is learned only from the observed object.
type ExpectedJob struct {
	Namespace string
	Name      string
}

// Validate checks the Kubernetes name syntax.
func (j ExpectedJob) Validate() error {
	if !kubeNamePattern.MatchString(j.Namespace) || !kubeNamePattern.MatchString(j.Name) {
		return fail(ErrInvalidRequest, "the expected Job namespace and name must be Kubernetes names")
	}
	return nil
}

// PodBinding is the workload identity fixed by the first successful
// admission. Later exchanges must present exactly the same identity.
type PodBinding struct {
	JobUID  string
	PodName string
	PodUID  string
}

// Validate checks syntax.
func (b PodBinding) Validate() error {
	if !uidPattern.MatchString(b.JobUID) || !uidPattern.MatchString(b.PodUID) || !kubeNamePattern.MatchString(b.PodName) {
		return fail(ErrForbidden, "the workload identity is malformed")
	}
	return nil
}

// Consumption is the durable result of the first successful admission.
type Consumption struct {
	NonceDigest  Digest
	Pod          PodBinding
	SessionID    SessionID
	ConsumedAt   time.Time
	MaxExpiresAt time.Time
}

// AdmissionGrant is the entity behind one attempt's runtime authority: a
// one-time credential registered by the trusted executor, then at most one
// session. Every field except Consumption and RevokedAt is fixed at
// registration; Consumption is written once; RevokedAt is written once and is
// irreversible. Expiry never revives anything.
type AdmissionGrant struct {
	Scope     domain.Scope
	RunID     domain.RunID
	AttemptID domain.AttemptID
	Job       ExpectedJob

	CredentialDigest Digest
	// Generation is the signing generation current at registration. A grant
	// of an older generation is dead after a key rotation, including one
	// resurrected by restoring an old database.
	Generation int64
	// Actions is the immutable ceiling. Authority never exceeds it; it may be
	// narrowed further by the current attempt state at each use.
	Actions ActionSet
	// AuthorityRevision is the attempt's authority revision the grant was
	// registered against (see AttemptState).
	AuthorityRevision int64
	// ContextDigest identifies the immutable pinned context document, which
	// is opaque to the entity.
	ContextDigest Digest
	Context       []byte

	// ExpiresAt bounds the first admission only.
	ExpiresAt time.Time
	CreatedAt time.Time

	Consumption *Consumption
	RevokedAt   *time.Time
}

// MaxContextBytes bounds the stored context document.
const MaxContextBytes = 4 << 20

// Provision is the input of NewAdmissionGrant.
type Provision struct {
	Scope             domain.Scope
	RunID             domain.RunID
	AttemptID         domain.AttemptID
	Job               ExpectedJob
	CredentialDigest  Digest
	Generation        int64
	Actions           ActionSet
	AuthorityRevision int64
	ContextDigest     Digest
	Context           []byte
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// NewAdmissionGrant validates ownership and bindings and creates an
// unconsumed grant that expires after the policy's admission lifetime.
func NewAdmissionGrant(p Provision, now time.Time, pol Policy) (AdmissionGrant, error) {
	if err := pol.Validate(); err != nil {
		return AdmissionGrant{}, failWith(ErrInvalidRequest, err, "invalid policy")
	}
	if err := firstErr(
		p.Scope.Validate(), p.RunID.Validate(), p.AttemptID.Validate(), p.Job.Validate(),
	); err != nil {
		return AdmissionGrant{}, fail(ErrInvalidRequest, "%v", err)
	}
	switch {
	case p.CredentialDigest.IsZero():
		return AdmissionGrant{}, fail(ErrInvalidRequest, "a credential digest is required")
	case p.Generation < 1:
		return AdmissionGrant{}, fail(ErrInvalidRequest, "a signing generation is required")
	case p.Actions.IsZero():
		return AdmissionGrant{}, fail(ErrInvalidRequest, "a grant needs at least one action")
	case p.AuthorityRevision < 1 || p.AuthorityRevision > domain.MaxSafeInteger:
		return AdmissionGrant{}, fail(ErrInvalidRequest, "the authority revision is out of range")
	case p.ContextDigest.IsZero() || len(p.Context) == 0 || len(p.Context) > MaxContextBytes:
		return AdmissionGrant{}, fail(ErrInvalidRequest, "a bounded pinned context is required")
	}
	created := micros(now)
	return AdmissionGrant{
		Scope: p.Scope, RunID: p.RunID, AttemptID: p.AttemptID, Job: p.Job,
		CredentialDigest: p.CredentialDigest, Generation: p.Generation, Actions: p.Actions,
		AuthorityRevision: p.AuthorityRevision, ContextDigest: p.ContextDigest, Context: bytes.Clone(p.Context),
		CreatedAt: created, ExpiresAt: created.Add(pol.AdmissionTTL),
	}, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// SameRegistration reports whether o registers exactly the same immutable
// authority. Expiry and creation time are excluded: a retried registration
// does not extend the stored expiry.
func (g AdmissionGrant) SameRegistration(o AdmissionGrant) bool {
	return g.Scope == o.Scope && g.RunID == o.RunID && g.AttemptID == o.AttemptID && g.Job == o.Job &&
		g.CredentialDigest.Equal(o.CredentialDigest) && g.Generation == o.Generation && g.Actions.Equal(o.Actions) &&
		g.AuthorityRevision == o.AuthorityRevision && g.ContextDigest.Equal(o.ContextDigest) && bytes.Equal(g.Context, o.Context)
}

// Consumed reports whether the credential has been exchanged.
func (g AdmissionGrant) Consumed() bool { return g.Consumption != nil }

// Revoked reports irreversible denial.
func (g AdmissionGrant) Revoked() bool { return g.RevokedAt != nil }

// Admission is the result of Consume.
type Admission struct {
	Consumption
	// Replayed is true when an earlier admission with the same nonce and
	// workload identity was repeated.
	Replayed bool
}

// Consume applies an admission exchange at now.
//
//   - revoked: ErrAttemptRevoked, always, first;
//   - first exchange: ErrInvalidCredential when expired, else consumes, fixing
//     the session, the Pod binding and the maximum expiry;
//   - same nonce, same Pod: bounded replay returning the stored consumption,
//     until the maximum expiry (the maximum never moves);
//   - same nonce, other Pod: ErrForbidden;
//   - another nonce: ErrAdmissionConsumed.
func (g *AdmissionGrant) Consume(nonce Digest, pod PodBinding, session SessionID, now time.Time, pol Policy) (Admission, error) {
	now = micros(now)
	if g.Revoked() {
		return Admission{}, fail(ErrAttemptRevoked, "the attempt is no longer authorized")
	}
	if g.Consumption != nil {
		c := *g.Consumption
		if !c.NonceDigest.Equal(nonce) {
			return Admission{}, fail(ErrAdmissionConsumed, "admission was already consumed")
		}
		if c.Pod != pod {
			return Admission{}, fail(ErrForbidden, "the workload identity differs from the admitted one")
		}
		if !now.Before(c.MaxExpiresAt) {
			return Admission{}, fail(ErrInvalidCredential, "the session reached its maximum lifetime")
		}
		return Admission{Consumption: c, Replayed: true}, nil
	}
	if err := pod.Validate(); err != nil {
		return Admission{}, err
	}
	if err := session.Validate(); err != nil {
		return Admission{}, fail(ErrInvalidRequest, "a session reference is required")
	}
	if !now.Before(g.ExpiresAt) {
		return Admission{}, fail(ErrInvalidCredential, "the admission credential expired")
	}
	c := Consumption{NonceDigest: nonce, Pod: pod, SessionID: session, ConsumedAt: now, MaxExpiresAt: now.Add(pol.MaxSession)}
	g.Consumption = &c
	return Admission{Consumption: c}, nil
}

// Revoke irreversibly denies the grant. It reports whether this call changed
// the state; repeating it is a no-op that keeps the first revocation time.
func (g *AdmissionGrant) Revoke(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	t := micros(now)
	g.RevokedAt = &t
	return true
}

// Authorize decides whether the session may use action at now. It checks only
// the grant's own state and ceiling; the caller combines it with the current
// attempt state.
func (g AdmissionGrant) Authorize(action Action, now time.Time) error {
	if g.Revoked() {
		return fail(ErrAttemptRevoked, "the attempt is no longer authorized")
	}
	if g.Consumption == nil {
		return fail(ErrInvalidCredential, "the grant has no session")
	}
	if !micros(now).Before(g.Consumption.MaxExpiresAt) {
		return fail(ErrInvalidCredential, "the session reached its maximum lifetime")
	}
	if !g.Actions.Contains(action) {
		return fail(ErrForbidden, "the action is not granted")
	}
	return nil
}

// TokenExpiry is now plus ttl, clamped to the session maximum. It never
// exceeds the maximum fixed at first admission.
func (g AdmissionGrant) TokenExpiry(now time.Time, ttl time.Duration) (time.Time, error) {
	if g.Consumption == nil {
		return time.Time{}, fail(ErrInvalidCredential, "the grant has no session")
	}
	exp := now.UTC().Add(ttl).Truncate(time.Second)
	if exp.After(g.Consumption.MaxExpiresAt) {
		exp = g.Consumption.MaxExpiresAt.Truncate(time.Second)
	}
	if !exp.After(now.UTC()) {
		return time.Time{}, fail(ErrInvalidCredential, "the session reached its maximum lifetime")
	}
	return exp, nil
}
