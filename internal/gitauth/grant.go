package gitauth

import (
	"regexp"
	"slices"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var idRegexp = regexp.MustCompile(domain.IDPattern)

func validID(s string) bool { return idRegexp.MatchString(s) }

// timestamp formats t as the contract's UTC RFC 3339 value, at the microsecond
// precision PostgreSQL stores, so a claim and its row column are the same instant.
func timestamp(t time.Time) gitv1.Timestamp {
	return gitv1.Timestamp(t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano))
}

// Consumption records the one receive a seed grant authorized.
type Consumption struct {
	ReceiveID string
	At        time.Time
}

// Grant is the server's durable record of one Git credential: the contract's
// claims plus the credential digest, the idempotency key of workload grants,
// the consumption of a seed and the revocation. The claims are immutable after
// issuance; only the digest (a retried issuance rotates the secret), the
// consumption (write-once) and the revocation (irreversible) change.
type Grant struct {
	Claims gitv1.GrantClaims
	Digest Digest
	// IssueKey is the idempotency key of a workload grant (kind and attempt or
	// workload ID, or gate and attempt); empty for human grants and the seed
	// grants of a person.
	IssueKey    string
	Consumption *Consumption
	RevokedAt   *time.Time
}

// ID is the grant identifier carried by the claims.
func (g Grant) ID() string { return g.Claims.GrantID }

// Revoked reports whether the grant was revoked.
func (g Grant) Revoked() bool { return g.RevokedAt != nil }

// IssuedAt parses the claims' issue time.
func (g Grant) IssuedAt() time.Time { t, _ := g.Claims.IssuedAt.Time(); return t }

// ExpiresAt parses the claims' expiry.
func (g Grant) ExpiresAt() time.Time { t, _ := g.Claims.ExpiresAt.Time(); return t }

// AttemptID is the attempt a grant is bound to, if any.
func (g Grant) AttemptID() (domain.AttemptID, bool) {
	if g.Claims.Actor.AttemptID == nil {
		return "", false
	}
	return domain.AttemptID(*g.Claims.Actor.AttemptID), true
}

// RepositoryIDs lists the repositories the grant names.
func (g Grant) RepositoryIDs() []string {
	ids := make([]string, len(g.Claims.Repositories))
	for i, r := range g.Claims.Repositories {
		ids[i] = r.ID
	}
	return ids
}

// Names reports whether the grant names the repository.
func (g Grant) Names(repositoryID string) bool {
	return slices.Contains(g.RepositoryIDs(), repositoryID)
}

// Revoke marks the grant revoked and reports whether it changed. Revocation is
// idempotent and irreversible; the recorded time never precedes issuance.
func (g *Grant) Revoke(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	if issued := g.IssuedAt(); now.Before(issued) {
		now = issued
	}
	t := now.UTC()
	g.RevokedAt = &t
	return true
}

// Window reports whether the grant is usable at now: not revoked and inside its
// validity window (gitv1.GrantClaims.CheckTime).
func (g Grant) Window(now time.Time) error {
	if g.Revoked() || g.Claims.CheckTime(now) != nil {
		return ErrGrantInactive
	}
	return nil
}

// Consume records that receiveID's authorization used up this seed grant. It
// is idempotent for the same receive (so a replayed authorization answers the
// same), refused with ErrSeedConsumed for any other receive and with
// ErrGrantInactive for a grant that is revoked or expired and not yet
// consumed. It reports whether anything changed.
func (g *Grant) Consume(receiveID string, now time.Time) (bool, error) {
	if g.Claims.Kind != gitv1.GrantSeed {
		return false, invalidRequest("only a seed grant is consumed")
	}
	if !validID(receiveID) {
		return false, invalidRequest("the receive ID is malformed")
	}
	if c := g.Consumption; c != nil {
		if c.ReceiveID == receiveID {
			return false, nil
		}
		return false, ErrSeedConsumed
	}
	if err := g.Window(now); err != nil {
		return false, err
	}
	g.Consumption = &Consumption{ReceiveID: receiveID, At: now.UTC()}
	return true, nil
}

// sameIssue reports whether two claim sets are the same issuance for the
// purpose of idempotent replay: identical actor, kind, tenant, project and
// repositories. Times and the secret are not part of the identity.
func sameIssue(a, b gitv1.GrantClaims) bool {
	if a.Kind != b.Kind || a.TenantID != b.TenantID || a.ProjectID != b.ProjectID || !sameActor(a.Actor, b.Actor) {
		return false
	}
	if len(a.Repositories) != len(b.Repositories) {
		return false
	}
	for i := range a.Repositories {
		x, y := a.Repositories[i], b.Repositories[i]
		if x.ID != y.ID || x.Read != y.Read || x.Seed != y.Seed || !slices.Equal(x.Update, y.Update) || !sameIntegration(x.Integration, y.Integration) {
			return false
		}
	}
	return true
}

func sameActor(a, b gitv1.Actor) bool {
	return a.Type == b.Type && a.ID == b.ID && samePtr(a.RunID, b.RunID) && samePtr(a.AttemptID, b.AttemptID)
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameIntegration(a, b *gitv1.IntegrationConstraint) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Clone returns a deep copy, so a store that keeps grants as values never shares
// slices or pointers with its callers.
func (g Grant) Clone() Grant {
	out := g
	out.Claims = cloneClaims(g.Claims)
	if g.Consumption != nil {
		c := *g.Consumption
		out.Consumption = &c
	}
	if g.RevokedAt != nil {
		t := *g.RevokedAt
		out.RevokedAt = &t
	}
	return out
}

// Policy holds the lifetimes of the grants this package issues. Each is bounded
// by the contract's maximum for its kind.
type Policy struct {
	// HumanTTL is the lifetime of a human credential (at most gitv1.MaxHumanGrantLifetime).
	HumanTTL time.Duration
	// SeedTTL is the lifetime of a person's seed credential (at most gitv1.MaxSeedLifetime).
	SeedTTL time.Duration
	// IntegrationTTL is the lifetime of an integration grant (at most
	// gitv1.MaxWorkloadGrantLifetime); the attempt's own deadline bounds it further.
	IntegrationTTL time.Duration
}

// DefaultPolicy is a conservative policy within the contract's bounds.
func DefaultPolicy() Policy {
	return Policy{HumanTTL: 15 * time.Minute, SeedTTL: gitv1.MaxSeedLifetime, IntegrationTTL: 30 * time.Minute}
}

// Validate checks every lifetime against the contract's bounds.
func (p Policy) Validate() error {
	switch {
	case p.HumanTTL <= 0 || p.HumanTTL > gitv1.MaxHumanGrantLifetime:
		return invalidRequest("the human credential lifetime must be in (0, %s]", gitv1.MaxHumanGrantLifetime)
	case p.SeedTTL <= 0 || p.SeedTTL > gitv1.MaxSeedLifetime:
		return invalidRequest("the seed credential lifetime must be in (0, %s]", gitv1.MaxSeedLifetime)
	case p.IntegrationTTL <= 0 || p.IntegrationTTL > gitv1.MaxWorkloadGrantLifetime:
		return invalidRequest("the integration grant lifetime must be in (0, %s]", gitv1.MaxWorkloadGrantLifetime)
	}
	return nil
}
