package rtest

import (
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// MintClaims describes a token to sign; zero fields take valid defaults.
type MintClaims struct {
	Issuer     string
	Audience   string
	Subject    string
	Tenant     string
	Generation int64
	IssuedAt   time.Time
	ExpiresAt  time.Time
	KeyID      string // empty: the snapshot's key ID
	Algorithm  jose.SignatureAlgorithm
}

type mintBody struct {
	josejwt.Claims
	Tenant     string `json:"tid"`
	Generation int64  `json:"gen"`
}

// MintWith signs c with the snapshot's key. It exists so tests can build
// correctly signed tokens that violate one rule (audience, issuer, lifetime...).
func MintWith(t testing.TB, snap jwt.Snapshot, c MintClaims) string {
	t.Helper()
	if c.Issuer == "" {
		c.Issuer = Issuer
	}
	if c.Audience == "" {
		c.Audience = "kodama-runtime"
	}
	if c.Generation == 0 {
		c.Generation = snap.Generation
	}
	if c.KeyID == "" {
		c.KeyID = snap.KeyID
	}
	if c.Algorithm == "" {
		c.Algorithm = jose.ES256
	}
	if c.IssuedAt.IsZero() {
		c.IssuedAt = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = c.IssuedAt.Add(5 * time.Minute)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: c.Algorithm, Key: snap.Key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader(jose.HeaderKey("kid"), c.KeyID))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := josejwt.Signed(signer).Claims(mintBody{
		Claims: josejwt.Claims{
			Issuer: c.Issuer, Subject: c.Subject, Audience: josejwt.Audience{c.Audience},
			Expiry: josejwt.NewNumericDate(c.ExpiresAt), NotBefore: josejwt.NewNumericDate(c.IssuedAt),
			IssuedAt: josejwt.NewNumericDate(c.IssuedAt), ID: "jti_t019",
		},
		Tenant: c.Tenant, Generation: c.Generation,
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
