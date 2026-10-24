// Package jwt is the token adapter of runtimeauth: compact ES256-signed
// session-reference tokens signed with go-jose and verified through go-oidc
// against a local key set. Keys and the issuer are injected; nothing here reads
// the environment or fetches keys over the network.
package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// Algorithm is the only accepted signature algorithm.
const Algorithm = jose.ES256

// GenerateKey creates a P-256 signing key (for tests and first installs; key
// custody is the deployment's concern).
func GenerateKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// ParsePrivateKeyPEM reads a PKCS#8 or SEC1 P-256 private key.
func ParsePrivateKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("jwt: no PEM block")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("jwt: the key is not an ECDSA key")
		}
		key = ec
	} else if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("jwt: the private key cannot be parsed")
	}
	return key, checkKey(key)
}

func checkKey(key *ecdsa.PrivateKey) error {
	if key == nil || key.Curve != elliptic.P256() {
		return errors.New("jwt: a P-256 ECDSA key is required")
	}
	return nil
}

func keyID(pub *ecdsa.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sum[:16]), nil
}

// KeyRing holds the one current signing key and its generation. Rotation
// replaces both atomically and retires the old key immediately: tokens signed
// by an earlier generation stop verifying at once, and grants registered under
// an earlier generation can no longer be admitted (restoring an old database
// does not resurrect them). A first implementation has no overlap window.
//
// The deployment persists the key and the generation (T-027/T-029) and must
// never reuse a generation number for a different key.
type KeyRing struct {
	mu  sync.RWMutex
	gen int64
	kid string
	key *ecdsa.PrivateKey
}

// NewKeyRing creates a ring at the given generation (at least 1).
func NewKeyRing(generation int64, key *ecdsa.PrivateKey) (*KeyRing, error) {
	if generation < 1 {
		return nil, errors.New("jwt: the signing generation must be at least 1")
	}
	if err := checkKey(key); err != nil {
		return nil, err
	}
	kid, err := keyID(&key.PublicKey)
	if err != nil {
		return nil, errors.New("jwt: the key cannot be identified")
	}
	return &KeyRing{gen: generation, kid: kid, key: key}, nil
}

// Snapshot is a consistent view of the current signing state.
type Snapshot struct {
	Generation int64
	KeyID      string
	Key        *ecdsa.PrivateKey
}

// Current returns the current generation, key ID and key.
func (k *KeyRing) Current() Snapshot {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return Snapshot{Generation: k.gen, KeyID: k.kid, Key: k.key}
}

// Generation returns the current signing generation.
func (k *KeyRing) Generation() int64 {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.gen
}

// Rotate installs a different key as the next generation and returns it. The
// previous key stops being accepted immediately.
func (k *KeyRing) Rotate(key *ecdsa.PrivateKey) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	kid, err := keyID(&key.PublicKey)
	if err != nil {
		return 0, errors.New("jwt: the key cannot be identified")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if kid == k.kid {
		return 0, errors.New("jwt: rotation requires a different key")
	}
	k.gen++
	k.kid, k.key = kid, key
	return k.gen, nil
}

// keySet adapts the ring to go-oidc: exactly one signature, only ES256, and
// only the current key ID.
type keySet struct{ ring *KeyRing }

var _ coreoidc.KeySet = keySet{}

func (s keySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{Algorithm})
	if err != nil || len(jws.Signatures) != 1 {
		return nil, errors.New("unparsable or unsupported signature")
	}
	cur := s.ring.Current()
	if jws.Signatures[0].Header.KeyID != cur.KeyID {
		return nil, errors.New("unknown signing key")
	}
	payload, err := jws.Verify(&cur.Key.PublicKey)
	if err != nil {
		return nil, errors.New("signature does not verify")
	}
	return payload, nil
}
