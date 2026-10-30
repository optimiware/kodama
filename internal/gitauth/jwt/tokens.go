package jwt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
)

// MaxTokenBytes bounds a presented token before any parsing.
const MaxTokenBytes = 4096

// JWS `typ` headers: a token of one purpose is not a token of the other.
const (
	forwardingType     = "kodama-git-forwarding+jwt"
	workerIdentityType = "kodama-git-worker-identity+jwt"
)

// The signing and verifying halves are separate types on purpose (IT-17):
//
//   - the control plane holds a Signer, with the private key rings, and signs
//     forwarding assertions and issues (and renews) worker identities;
//   - a tenant's Git worker holds a Verifier built from the forwarding ring's
//     PUBLIC keys only, so it can check an assertion and can never mint one, and
//     it is given an already issued, tenant-bound worker identity token (renewed
//     through the control plane) instead of any identity signing key;
//   - the control plane's own verification of worker identities uses the
//     identity ring's public keys (Signer.Verifier, or a Verifier over them).

// SignerConfig configures a Signer. The two rings must be different rings
// holding different keys, so that a key that signs forwarding assertions never
// issues worker identities.
type SignerConfig struct {
	// Issuer is the fixed issuer claim of both token kinds.
	Issuer string
	// Forwarding signs the assertions the Kodama server sends to a tenant's worker.
	Forwarding *KeyRing
	// Identity signs the worker identities a worker presents to the control plane.
	Identity *KeyRing
	// Clock defaults to time.Now; it bounds what signing accepts.
	Clock func() time.Time
}

// Signer signs forwarding assertions and issues worker identities. It holds
// signing keys: it belongs to the control plane only.
type Signer struct {
	issuer     string
	forwarding *KeyRing
	identity   *KeyRing
	now        func() time.Time
}

var (
	_ gitauth.ForwardingSigner       = (*Signer)(nil)
	_ gitauth.WorkerIdentityIssuer   = (*Signer)(nil)
	_ gitauth.ForwardingVerifier     = (*Verifier)(nil)
	_ gitauth.WorkerIdentityVerifier = (*Verifier)(nil)
	_ gitauth.Assertions             = (*Tokens)(nil)
)

// NewSigner validates the configuration.
func NewSigner(cfg SignerConfig) (*Signer, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("gitauth/jwt: an issuer is required")
	}
	if cfg.Forwarding == nil || cfg.Identity == nil || cfg.Forwarding == cfg.Identity {
		return nil, errors.New("gitauth/jwt: separate forwarding and identity key rings are required")
	}
	if cfg.Forwarding.Current().KeyID == cfg.Identity.Current().KeyID {
		return nil, errors.New("gitauth/jwt: the forwarding and identity rings must not share a key")
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Signer{issuer: cfg.Issuer, forwarding: cfg.Forwarding, identity: cfg.Identity, now: now}, nil
}

// ForwardingPublicKeys returns the public keys workers verify assertions with.
func (s *Signer) ForwardingPublicKeys() PublicKeys { return s.forwarding.PublicKeys() }

// IdentityPublicKeys returns the public keys the control plane verifies worker
// identities with.
func (s *Signer) IdentityPublicKeys() PublicKeys { return s.identity.PublicKeys() }

// Verifier returns a verifier over this signer's own rings, for the process that
// holds the keys (the control plane verifies worker identities with it). It
// follows rotations of the rings.
func (s *Signer) Verifier() *Verifier {
	return &Verifier{issuer: s.issuer, forwarding: s.forwarding, identity: s.identity}
}

// Tokens returns the combined signer and verifier of the control-plane process.
func (s *Signer) Tokens() *Tokens { return &Tokens{Signer: s, Verifier: s.Verifier()} }

// Tokens is a Signer with a Verifier over its own keys: the gitauth.Assertions
// of a process that holds the signing keys. Never give it to a worker.
type Tokens struct {
	*Signer
	*Verifier
}

// VerifierConfig configures a Verifier with public keys only. Either ring may be
// nil: a worker verifies forwarding assertions and has no identity ring, and
// verifying the other kind is then unavailable.
type VerifierConfig struct {
	Issuer     string
	Forwarding *PublicKeyRing
	Identity   *PublicKeyRing
}

// Verifier verifies forwarding assertions and worker identities with public keys
// only; it has no signing method.
type Verifier struct {
	issuer     string
	forwarding keyLookup
	identity   keyLookup
}

// NewVerifier validates the configuration.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("gitauth/jwt: an issuer is required")
	}
	if cfg.Forwarding == nil && cfg.Identity == nil {
		return nil, errors.New("gitauth/jwt: a public key ring is required")
	}
	v := &Verifier{issuer: cfg.Issuer}
	if cfg.Forwarding != nil {
		v.forwarding = cfg.Forwarding
	}
	if cfg.Identity != nil {
		v.identity = cfg.Identity
	}
	return v, nil
}

// The claim sets are the contract's: the standard claims mirror them (so a
// generic verifier sees a well-formed token) and the contract document is
// carried whole in one private claim, which is what the verifier trusts after
// the signature, audience and window checks and a consistency check against the
// standard claims.
type forwardingClaims struct {
	josejwt.Claims
	Assertion gitv1.ForwardingAssertion `json:"fwd"`
}

type identityClaims struct {
	josejwt.Claims
	Identity gitv1.WorkerIdentity `json:"wid"`
}

func invalid() error {
	return &gitv1.ContractError{Code: domain.CodeInvalidCredential, Message: "the credential is not valid"}
}

func unavailable(what string) error {
	return fmt.Errorf("%w: gitauth/jwt: %s", gitauth.ErrUnavailable, what)
}

func (s *Signer) sign(ring *KeyRing, typ string, claims any) (string, error) {
	cur := ring.Current()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: Algorithm, Key: cur.Key},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader(jose.HeaderKey("kid"), cur.KeyID),
	)
	if err != nil {
		return "", unavailable("signing is not available")
	}
	token, err := josejwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		return "", unavailable("signing failed")
	}
	if len(token) > MaxTokenBytes {
		return "", unavailable("the token exceeds the wire bound")
	}
	return token, nil
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", unavailable("no randomness available")
	}
	return hex.EncodeToString(b), nil
}

func window(issued, expires gitv1.Timestamp) (time.Time, time.Time) {
	i, _ := issued.Time()
	e, _ := expires.Time()
	return i, e
}

// standard builds the standard claims of a token: nbf equals iat, the contract's
// issuance time, which verifiers require.
func (s *Signer) standard(subject, audience string, issued, expires time.Time) (josejwt.Claims, error) {
	jti, err := newID()
	if err != nil {
		return josejwt.Claims{}, err
	}
	return josejwt.Claims{
		Issuer: s.issuer, Subject: subject, Audience: josejwt.Audience{audience},
		Expiry: josejwt.NewNumericDate(expires), NotBefore: josejwt.NewNumericDate(issued),
		IssuedAt: josejwt.NewNumericDate(issued), ID: jti,
	}, nil
}

// SignForwarding signs a forwarding assertion. The claim set must be valid under
// the contract (audience kodama-git-worker:<tenantId>, lifetime at most five
// minutes) and not already expired.
func (s *Signer) SignForwarding(_ context.Context, a gitv1.ForwardingAssertion) (string, error) {
	if err := a.Validate(); err != nil {
		return "", &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: err.Error()}
	}
	issued, expires := window(a.IssuedAt, a.ExpiresAt)
	if !s.now().Before(expires) {
		return "", &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: "the assertion is already expired"}
	}
	std, err := s.standard(a.GrantID, a.Audience, issued, expires)
	if err != nil {
		return "", err
	}
	return s.sign(s.forwarding, forwardingType, forwardingClaims{Claims: std, Assertion: a})
}

// SignWorkerIdentity issues a worker identity (audience kodama-git-control,
// lifetime at most one hour). Only the control plane issues them.
func (s *Signer) SignWorkerIdentity(_ context.Context, w gitv1.WorkerIdentity) (string, error) {
	if err := w.Validate(); err != nil {
		return "", &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: err.Error()}
	}
	issued, expires := window(w.IssuedAt, w.ExpiresAt)
	if !s.now().Before(expires) {
		return "", &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: "the identity is already expired"}
	}
	std, err := s.standard(w.WorkerInstance, w.Audience, issued, expires)
	if err != nil {
		return "", err
	}
	return s.sign(s.identity, workerIdentityType, identityClaims{Claims: std, Identity: w})
}

// RenewWorkerIdentity is the control plane's renewal of a worker identity: the
// presented token must still verify (signature, key, window, and the route's
// tenant) and a new token for the same tenant and worker instance is issued for
// lifetime (at most one hour). A worker never holds an identity signing key; it
// renews through the control plane before its token expires. An expired token
// cannot be renewed: the worker must be re-provisioned.
func (s *Signer) RenewWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, lifetime time.Duration) (string, error) {
	now := s.now()
	old, err := s.Verifier().VerifyWorkerIdentity(ctx, token, pathTenant, now)
	if err != nil {
		return "", err
	}
	if lifetime <= 0 {
		return "", &gitv1.ContractError{Code: domain.CodeInvalidRequest, Message: "the lifetime must be positive"}
	}
	return s.SignWorkerIdentity(ctx, gitv1.WorkerIdentity{
		Audience: gitv1.ControlAudience, TenantID: old.TenantID, WorkerInstance: old.WorkerInstance,
		IssuedAt: gitv1.Timestamp(now.UTC().Format(time.RFC3339Nano)), ExpiresAt: gitv1.Timestamp(now.Add(lifetime).UTC().Format(time.RFC3339Nano)),
	})
}

// verifier builds the go-oidc verifier of one purpose. The signature, key, type,
// issuer and audience are go-oidc's; every time bound is enforced by checkTimes
// (go-oidc's expiry check and tolerance are switched off), so there is one
// explicit window with the contract's skew bound. clientID is the audience
// go-oidc enforces; empty skips that check, for the forwarding audience that
// carries the tenant and is compared by the contract's CheckForwarding.
func (v *Verifier) verifier(keys keyLookup, typ, clientID string) *coreoidc.IDTokenVerifier {
	return coreoidc.NewVerifier(v.issuer, keySet{keys: keys, typ: typ}, &coreoidc.Config{
		ClientID:             clientID,
		SkipClientIDCheck:    clientID == "",
		SkipExpiryCheck:      true,
		SupportedSigningAlgs: []string{string(Algorithm)},
	})
}

func sameSecond(a time.Time, n *josejwt.NumericDate) bool {
	return n != nil && n.Time().Unix() == a.Unix()
}

// checkTimes requires nbf, iat and exp, nbf equal to iat (the contract's
// issuance time), both and exp equal to the contract document's times, and the
// token inside its window at now: not before nbf minus the contract's clock skew
// and strictly before exp.
func checkTimes(c josejwt.Claims, issued, expires, now time.Time) bool {
	if c.NotBefore == nil || c.IssuedAt == nil || c.Expiry == nil {
		return false
	}
	if c.NotBefore.Time().Unix() != c.IssuedAt.Time().Unix() || !sameSecond(issued, c.IssuedAt) || !sameSecond(expires, c.Expiry) {
		return false
	}
	return !now.Add(gitv1.ClockSkew).Before(c.NotBefore.Time()) && now.Before(c.Expiry.Time())
}

// VerifyForwarding is the worker-side verification of a forwarding assertion at
// now: signature and key (current or previous public key), token type, issuer, a
// consistent claim set with its times (nbf and iat required and equal, the
// contract's skew bound, expiry), then gitv1.CheckForwarding for workerTenant
// (forbidden for another tenant's worker, invalid_credential for anything
// malformed, expired or not yet valid). Every failure of the token itself is
// invalid_credential with one message; the token and the verifier's own detail
// are never propagated.
func (v *Verifier) VerifyForwarding(ctx context.Context, token string, workerTenant domain.TenantID, now time.Time) (gitv1.ForwardingAssertion, error) {
	if v.forwarding == nil {
		return gitv1.ForwardingAssertion{}, unavailable("no forwarding public keys are configured")
	}
	if token == "" || len(token) > MaxTokenBytes || workerTenant.Validate() != nil {
		return gitv1.ForwardingAssertion{}, invalid()
	}
	idt, err := v.verifier(v.forwarding, forwardingType, "").Verify(ctx, token)
	if err != nil {
		return gitv1.ForwardingAssertion{}, invalid()
	}
	var c forwardingClaims
	if err := idt.Claims(&c); err != nil {
		return gitv1.ForwardingAssertion{}, invalid()
	}
	a := c.Assertion
	issued, expires := window(a.IssuedAt, a.ExpiresAt)
	if len(c.Audience) != 1 || c.Audience[0] != a.Audience || c.Subject != a.GrantID || !checkTimes(c.Claims, issued, expires, now) {
		return gitv1.ForwardingAssertion{}, invalid()
	}
	if err := gitv1.CheckForwarding(a, string(workerTenant), now); err != nil {
		return gitv1.ForwardingAssertion{}, err
	}
	return a, nil
}

// VerifyWorkerIdentity is the control-plane verification of a worker identity on
// a route of pathTenant at now: signature and key, token type, audience
// kodama-git-control, issuer, a consistent claim set with the same time rules as
// VerifyForwarding, then gitv1.CheckWorkerCall (forbidden for a worker calling
// another tenant's route).
func (v *Verifier) VerifyWorkerIdentity(ctx context.Context, token string, pathTenant domain.TenantID, now time.Time) (gitv1.WorkerIdentity, error) {
	if v.identity == nil {
		return gitv1.WorkerIdentity{}, unavailable("no identity public keys are configured")
	}
	if token == "" || len(token) > MaxTokenBytes || pathTenant.Validate() != nil {
		return gitv1.WorkerIdentity{}, invalid()
	}
	idt, err := v.verifier(v.identity, workerIdentityType, gitv1.ControlAudience).Verify(ctx, token)
	if err != nil {
		return gitv1.WorkerIdentity{}, invalid()
	}
	var c identityClaims
	if err := idt.Claims(&c); err != nil {
		return gitv1.WorkerIdentity{}, invalid()
	}
	w := c.Identity
	issued, expires := window(w.IssuedAt, w.ExpiresAt)
	if len(c.Audience) != 1 || c.Audience[0] != w.Audience || c.Subject != w.WorkerInstance || !checkTimes(c.Claims, issued, expires, now) {
		return gitv1.WorkerIdentity{}, invalid()
	}
	if err := gitv1.CheckWorkerCall(w, string(pathTenant), now); err != nil {
		return gitv1.WorkerIdentity{}, err
	}
	return w, nil
}
