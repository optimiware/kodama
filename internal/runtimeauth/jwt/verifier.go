package jwt

import (
	"context"
	"errors"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Verifier verifies runtime tokens with go-oidc over the ring's local key set.
// It implements runtimeauth.TokenVerifier.
type Verifier struct {
	ring        *KeyRing
	issuer      string
	maxLifetime time.Duration
	now         func() time.Time
	oidc        *coreoidc.IDTokenVerifier
	oidcRes     *coreoidc.IDTokenVerifier
}

var (
	_ runtimeauth.TokenVerifier         = (*Verifier)(nil)
	_ runtimeauth.ResourceTokenVerifier = (*Verifier)(nil)
)

// VerifierConfig configures NewVerifier.
type VerifierConfig struct {
	Ring   *KeyRing
	Issuer string
	// MaxLifetime bounds exp - iat of any accepted token; it should equal the
	// policy's maximum session lifetime so no token outlives a session.
	MaxLifetime time.Duration
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// NewVerifier validates the configuration.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Ring == nil || cfg.Issuer == "" || cfg.MaxLifetime <= 0 {
		return nil, errors.New("jwt: a key ring, an issuer and a maximum lifetime are required")
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		ring: cfg.Ring, issuer: cfg.Issuer, maxLifetime: cfg.MaxLifetime, now: now,
		oidc: coreoidc.NewVerifier(cfg.Issuer, keySet{ring: cfg.Ring}, &coreoidc.Config{
			ClientID:             runtimev1.Audience,
			SupportedSigningAlgs: []string{string(Algorithm)},
			Now:                  now,
		}),
		oidcRes: coreoidc.NewVerifier(cfg.Issuer, keySet{ring: cfg.Ring}, &coreoidc.Config{
			ClientID:             runtimev2.ResourceAudience,
			SupportedSigningAlgs: []string{string(Algorithm)},
			Now:                  now,
		}),
	}, nil
}

func invalid(why string) error {
	return errors.Join(runtimeauth.ErrInvalidCredential, errors.New(why))
}

// Verify checks signature, algorithm, key, issuer, audience, expiry and claim
// structure. Every failure is ErrInvalidCredential with a fixed reason; the
// token and go-oidc's detailed message (which can quote token content) are
// never propagated.
func (v *Verifier) Verify(ctx context.Context, raw string) (runtimeauth.TokenClaims, error) {
	return v.verify(ctx, raw, v.oidc)
}

// VerifyResource is Verify for the resource audience (kodama-runtime-resource).
// A genuine attempt token fails here as invalid; the service layer decides
// whether that means "wrong route family" (forbidden).
func (v *Verifier) VerifyResource(ctx context.Context, raw string) (runtimeauth.TokenClaims, error) {
	return v.verify(ctx, raw, v.oidcRes)
}

func (v *Verifier) verify(ctx context.Context, raw string, oidc *coreoidc.IDTokenVerifier) (runtimeauth.TokenClaims, error) {
	if raw == "" || len(raw) > MaxTokenBytes {
		return runtimeauth.TokenClaims{}, invalid("the token is missing or too large")
	}
	tok, err := oidc.Verify(ctx, raw)
	if err != nil {
		var expired *coreoidc.TokenExpiredError
		if errors.As(err, &expired) {
			return runtimeauth.TokenClaims{}, invalid("the token is expired")
		}
		return runtimeauth.TokenClaims{}, invalid("the token is not valid for the runtime interface")
	}
	var c claims
	if err := tok.Claims(&c); err != nil {
		return runtimeauth.TokenClaims{}, invalid("the claims are malformed")
	}
	tenant := domain.TenantID(c.Tenant)
	session := runtimeauth.SessionID(c.Subject)
	switch {
	case tenant.Validate() != nil, session.Validate() != nil:
		return runtimeauth.TokenClaims{}, invalid("the token identities are malformed")
	case c.ID == "" || len(c.ID) > 64:
		return runtimeauth.TokenClaims{}, invalid("the token has no usable identifier")
	case c.Generation < 1 || c.Generation > domain.MaxSafeInteger:
		return runtimeauth.TokenClaims{}, invalid("the token has no signing generation")
	case c.IssuedAt == nil || c.Expiry == nil || c.NotBefore == nil:
		return runtimeauth.TokenClaims{}, invalid("the token lacks required times")
	}
	iat, exp := c.IssuedAt.Time(), c.Expiry.Time()
	if !exp.After(iat) || exp.Sub(iat) > v.maxLifetime {
		return runtimeauth.TokenClaims{}, invalid("the token lifetime is out of bounds")
	}
	// go-oidc applies its own clock tolerance; the runtime interface does not.
	now := v.now()
	if !now.Before(exp) {
		return runtimeauth.TokenClaims{}, invalid("the token is expired")
	}
	if now.Before(c.NotBefore.Time()) {
		return runtimeauth.TokenClaims{}, invalid("the token is not yet valid")
	}
	// A token of a retired generation cannot verify (its key is gone), but the
	// generation claim must also match the ring explicitly, so a replaced key
	// reusing a kid could never revive older tokens.
	if c.Generation != v.ring.Generation() {
		return runtimeauth.TokenClaims{}, invalid("the signing generation is retired")
	}
	return runtimeauth.TokenClaims{
		Tenant: tenant, Session: session, TokenID: c.ID, Generation: c.Generation, IssuedAt: iat, ExpiresAt: exp,
	}, nil
}
