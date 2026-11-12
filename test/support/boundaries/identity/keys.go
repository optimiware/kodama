package identity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
)

// SigningKey is one asymmetric signing key published in the fixture JWKS.
//
// Algorithm is one of RS256 or ES256, matching the algorithms the
// internal/shell/integration/oidc verifier accepts. Exactly one of RSA or EC is
// set. Use is the JWK "use" member; an empty value publishes "sig" and a test
// can publish "enc" to prove the verifier ignores non-signature keys.
type SigningKey struct {
	Kid       string
	Algorithm string
	Use       string
	RSA       *rsa.PrivateKey
	EC        *ecdsa.PrivateKey
}

// NewRSAKey generates one RSA-2048 RS256 signing key.
func NewRSAKey(kid string) (*SigningKey, error) {
	if kid == "" {
		return nil, fmt.Errorf("RSA key id is empty")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}
	return &SigningKey{Kid: kid, Algorithm: "RS256", RSA: key}, nil
}

// NewECKey generates one ECDSA P-256 ES256 signing key.
func NewECKey(kid string) (*SigningKey, error) {
	if kid == "" {
		return nil, fmt.Errorf("EC key id is empty")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate EC key: %w", err)
	}
	return &SigningKey{Kid: kid, Algorithm: "ES256", EC: key}, nil
}

// JWK renders the public JWK for one signing key.
func (key *SigningKey) JWK() (map[string]any, error) {
	if key == nil {
		return nil, fmt.Errorf("signing key is nil")
	}
	if key.Kid == "" {
		return nil, fmt.Errorf("signing key id is empty")
	}
	switch {
	case key.RSA != nil:
		return map[string]any{
			"kty": "RSA",
			"kid": key.Kid,
			"use": key.use(),
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.RSA.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.RSA.E)).Bytes()),
		}, nil
	case key.EC != nil:
		return map[string]any{
			"kty": "EC",
			"kid": key.Kid,
			"use": key.use(),
			"alg": "ES256",
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString(key.EC.X.FillBytes(make([]byte, 32))),
			"y":   base64.RawURLEncoding.EncodeToString(key.EC.Y.FillBytes(make([]byte, 32))),
		}, nil
	default:
		return nil, fmt.Errorf("signing key has no private key")
	}
}

func (key *SigningKey) use() string {
	if key.Use == "" {
		return "sig"
	}
	return key.Use
}

// PublicKeyDER returns the PKIX DER encoding of the public key. It is used to
// build the HS256 algorithm-confusion vector.
func (key *SigningKey) PublicKeyDER() ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("signing key is nil")
	}
	switch {
	case key.RSA != nil:
		return x509.MarshalPKIXPublicKey(&key.RSA.PublicKey)
	case key.EC != nil:
		return x509.MarshalPKIXPublicKey(&key.EC.PublicKey)
	default:
		return nil, fmt.Errorf("signing key has no private key")
	}
}

// Sign signs one JWS signing input with the key's algorithm.
func (key *SigningKey) Sign(signingInput []byte) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("signing key is nil")
	}
	digest := sha256.Sum256(signingInput)
	switch key.Algorithm {
	case "RS256":
		if key.RSA == nil {
			return nil, fmt.Errorf("RS256 key has no RSA private key")
		}
		return rsa.SignPKCS1v15(rand.Reader, key.RSA, crypto.SHA256, digest[:])
	case "ES256":
		if key.EC == nil {
			return nil, fmt.Errorf("ES256 key has no EC private key")
		}
		return ecdsa.SignASN1(rand.Reader, key.EC, digest[:])
	default:
		return nil, fmt.Errorf("unsupported signing algorithm %q", key.Algorithm)
	}
}

// JWKS renders a JWKS document for the supplied keys.
func JWKS(keys ...*SigningKey) ([]byte, error) {
	entries := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		jwk, err := key.JWK()
		if err != nil {
			return nil, err
		}
		entries = append(entries, jwk)
	}
	return json.Marshal(map[string]any{"keys": entries})
}
