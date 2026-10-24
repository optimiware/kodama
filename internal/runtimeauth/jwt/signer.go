package jwt

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// MaxTokenBytes is the wire bound of a token (the contract's TokenValue).
const MaxTokenBytes = 1000

// Claims are the private claims of a runtime token. The standard claims are
// iss, sub (the session reference), aud (the fixed runtime audience), exp, nbf,
// iat and jti. Project, attempt, Job/Pod and actions are deliberately absent:
// they come from the durable grant, which keeps tokens small and revocable.
type claims struct {
	josejwt.Claims
	Tenant     string `json:"tid"`
	Generation int64  `json:"gen"`
}

// Signer issues tokens. It implements runtimeauth.TokenIssuer.
type Signer struct {
	ring   *KeyRing
	issuer string
}

var _ runtimeauth.TokenIssuer = (*Signer)(nil)

// NewSigner requires a ring and a non-empty issuer.
func NewSigner(ring *KeyRing, issuer string) (*Signer, error) {
	if ring == nil {
		return nil, errors.New("jwt: a key ring is required")
	}
	if issuer == "" {
		return nil, errors.New("jwt: an issuer is required")
	}
	return &Signer{ring: ring, issuer: issuer}, nil
}

// Generation implements runtimeauth.TokenIssuer.
func (s *Signer) Generation() int64 { return s.ring.Generation() }

// Issue signs a token for the session. A request for a generation that is no
// longer current fails closed.
func (s *Signer) Issue(req runtimeauth.IssueRequest) (string, error) {
	if err := req.Tenant.Validate(); err != nil {
		return "", errors.Join(runtimeauth.ErrInvalidRequest, errors.New("invalid tenant"))
	}
	if err := req.Session.Validate(); err != nil {
		return "", errors.Join(runtimeauth.ErrInvalidRequest, errors.New("invalid session"))
	}
	if !req.ExpiresAt.After(req.IssuedAt) {
		return "", errors.Join(runtimeauth.ErrInvalidRequest, errors.New("the token would already be expired"))
	}
	cur := s.ring.Current()
	if req.Generation != cur.Generation {
		return "", errors.Join(runtimeauth.ErrInvalidCredential, errors.New("the signing generation was rotated"))
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: Algorithm, Key: cur.Key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), cur.KeyID),
	)
	if err != nil {
		return "", errors.Join(runtimeauth.ErrUnavailable, errors.New("signing is not available"))
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return "", errors.Join(runtimeauth.ErrUnavailable, errors.New("no randomness available"))
	}
	c := claims{
		Claims: josejwt.Claims{
			Issuer: s.issuer, Subject: string(req.Session), Audience: josejwt.Audience{runtimev1.Audience},
			Expiry: josejwt.NewNumericDate(req.ExpiresAt), NotBefore: josejwt.NewNumericDate(req.IssuedAt),
			IssuedAt: josejwt.NewNumericDate(req.IssuedAt), ID: hex.EncodeToString(id),
		},
		Tenant: string(req.Tenant), Generation: cur.Generation,
	}
	token, err := josejwt.Signed(signer).Claims(c).Serialize()
	if err != nil {
		return "", errors.Join(runtimeauth.ErrUnavailable, errors.New("signing failed"))
	}
	if len(token) > MaxTokenBytes {
		return "", errors.Join(runtimeauth.ErrUnavailable, errors.New("the token exceeds the wire bound"))
	}
	return token, nil
}
