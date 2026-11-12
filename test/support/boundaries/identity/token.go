package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"maps"
	"time"
)

// Token is one minted access token and the claims it carries.
type Token struct {
	Value  string
	Claims map[string]any
}

// TokenSpec describes one controllable fixture token. Zero values take the
// provider defaults; Mutate runs last so a test can delete or replace any
// claim, including the defaults.
type TokenSpec struct {
	Issuer string
	// Audience overrides the default API audience. Access tokens keep the API
	// audience; ID tokens must carry the OAuth client_id (OIDC Core 3.1.3.7),
	// which mintIDToken supplies.
	Audience   any
	Subject    string
	Tenant     string
	Project    string
	ExpiresAt  time.Time
	NotBefore  *time.Time
	IssuedAt   *time.Time
	Nonce      string
	AZP        string
	Roles      []string
	Extra      map[string]any
	Mutate     func(map[string]any)
	KeyID      string
	OmitKeyID  bool
	Algorithm  string
	SigningKey *SigningKey
	HMACSecret []byte
}

// MintAccessToken mints one human access token with the provider defaults.
func (provider *Provider) MintAccessToken(subject, tenant, project string) (Token, error) {
	return provider.Mint(TokenSpec{Subject: subject, Tenant: tenant, Project: project})
}

// Mint mints one signed token. The issuer, audience, subject, tenant, project,
// expiry, issue time, not-before, nonce, azp, roles and arbitrary extra claims
// are all controllable.
func (provider *Provider) Mint(spec TokenSpec) (Token, error) {
	if provider == nil {
		return Token{}, fmt.Errorf("OIDC provider is nil")
	}
	now := provider.clock.Now()

	issuer := spec.Issuer
	if issuer == "" {
		issuer = provider.issuer
	}
	subject := spec.Subject
	if subject == "" {
		subject = "auth0|human-1"
	}
	tenant := spec.Tenant
	if tenant == "" {
		tenant = "tenant-a"
	}
	project := spec.Project
	if project == "" {
		project = "project-1"
	}
	expiresAt := spec.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = now.Add(time.Hour)
	}
	issuedAt := now
	if spec.IssuedAt != nil {
		issuedAt = *spec.IssuedAt
	}

	claims := map[string]any{
		"iss":                issuer,
		"aud":                provider.audience,
		"sub":                subject,
		"exp":                expiresAt.Unix(),
		"iat":                issuedAt.Unix(),
		provider.tenantClaim: tenant,
	}
	if project != "" {
		claims[provider.projectClaim] = project
	}
	if spec.Audience != nil {
		claims["aud"] = spec.Audience
	}
	if spec.NotBefore != nil {
		claims["nbf"] = spec.NotBefore.Unix()
	}
	if spec.Nonce != "" {
		claims["nonce"] = spec.Nonce
	}
	if spec.AZP != "" {
		claims["azp"] = spec.AZP
	}
	if len(spec.Roles) > 0 {
		claims[provider.roleClaim] = spec.Roles
	}
	maps.Copy(claims, spec.Extra)
	if spec.Mutate != nil {
		spec.Mutate(claims)
	}

	key, err := provider.tokenKey(spec)
	if err != nil {
		return Token{}, err
	}
	algorithm := spec.Algorithm
	if algorithm == "" {
		if key != nil {
			algorithm = key.Algorithm
		} else {
			algorithm = "RS256"
		}
	}

	header := map[string]any{"alg": algorithm, "typ": "JWT"}
	if !spec.OmitKeyID {
		kid := spec.KeyID
		if kid == "" && key != nil {
			kid = key.Kid
		}
		if kid != "" {
			header["kid"] = kid
		}
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return Token{}, fmt.Errorf("marshal token header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return Token{}, fmt.Errorf("marshal token claims: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	signature, err := signToken(algorithm, key, spec.HMACSecret, []byte(signingInput))
	if err != nil {
		return Token{}, err
	}
	value := signingInput + "."
	if signature != nil {
		value += base64.RawURLEncoding.EncodeToString(signature)
	}
	return Token{Value: value, Claims: claims}, nil
}

func (provider *Provider) tokenKey(spec TokenSpec) (*SigningKey, error) {
	if spec.SigningKey != nil {
		return spec.SigningKey, nil
	}
	if spec.KeyID != "" {
		return provider.signingKey(spec.KeyID)
	}
	return provider.firstSigningKey()
}

func signToken(algorithm string, key *SigningKey, secret, signingInput []byte) ([]byte, error) {
	switch algorithm {
	case "none":
		return nil, nil
	case "HS256", "HS384", "HS512":
		if len(secret) == 0 {
			return nil, fmt.Errorf("HMAC algorithm %q has no secret", algorithm)
		}
		var constructor func() hash.Hash
		switch algorithm {
		case "HS256":
			constructor = sha256.New
		case "HS384":
			constructor = sha512.New384
		default:
			constructor = sha512.New
		}
		mac := hmac.New(constructor, secret)
		_, _ = mac.Write(signingInput)
		return mac.Sum(nil), nil
	case "RS256", "RS512":
		if key == nil || key.RSA == nil {
			return nil, fmt.Errorf("algorithm %q requires an RSA key", algorithm)
		}
		var digest []byte
		var hash crypto.Hash
		if algorithm == "RS256" {
			sum := sha256.Sum256(signingInput)
			digest = sum[:]
			hash = crypto.SHA256
		} else {
			sum := sha512.Sum512(signingInput)
			digest = sum[:]
			hash = crypto.SHA512
		}
		return rsa.SignPKCS1v15(rand.Reader, key.RSA, hash, digest)
	case "ES256":
		if key == nil || key.EC == nil {
			return nil, fmt.Errorf("algorithm %q requires an EC key", algorithm)
		}
		sum := sha256.Sum256(signingInput)
		return ecdsa.SignASN1(rand.Reader, key.EC, sum[:])
	default:
		return nil, fmt.Errorf("unsupported token algorithm %q", algorithm)
	}
}
