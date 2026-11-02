// Package jwt is the token adapter of gitauth: compact ES256-signed forwarding
// assertions and worker identities, signed with go-jose and verified through
// go-oidc against a local key set (decision P-16: no hand-rolled verifier, the
// libraries runtimeauth/jwt uses). Keys and the issuer are injected; nothing
// here reads the environment or fetches keys over the network.
//
// The two token kinds are separated three ways: each has its own key ring, its
// own JWS `typ` header and its own audience (kodama-git-worker:<tenant> and
// kodama-git-control), and neither is an opaque Git credential
// (kodama-git). A token of one kind therefore never verifies as another, and no
// token is ever accepted as a Git credential.
//
// Signing and verification are separate types (IT-17): a Signer holds the
// private KeyRings and belongs to the control plane; a Verifier holds
// PublicKeyRings only and has no signing method, which is what a tenant's worker
// is given. Rotation keeps one previous key for overlap and reaches verifiers as
// new PublicKeys.
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
		return nil, errors.New("gitauth/jwt: no PEM block")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("gitauth/jwt: the key is not an ECDSA key")
		}
		key = ec
	} else if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("gitauth/jwt: the private key cannot be parsed")
	}
	return key, checkKey(key)
}

func checkKey(key *ecdsa.PrivateKey) error {
	if key == nil || key.Curve != elliptic.P256() {
		return errors.New("gitauth/jwt: a P-256 ECDSA key is required")
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

type keyEntry struct {
	kid string
	key *ecdsa.PrivateKey
}

// KeyRing holds the current signing key and, after a rotation, the previous key
// for verification only, so tokens signed just before the rotation (they live
// minutes) keep verifying while no new token is signed with it. RetirePrevious
// ends that overlap at once, for example after a key compromise or a restore:
// every token signed by an earlier key then stops verifying. A ring never holds
// more than one previous key.
//
// The deployment persists the key and the generation and must never reuse a
// generation number for a different key.
type KeyRing struct {
	mu       sync.RWMutex
	gen      int64
	current  keyEntry
	previous *keyEntry
}

// NewKeyRing creates a ring at the given generation (at least 1).
func NewKeyRing(generation int64, key *ecdsa.PrivateKey) (*KeyRing, error) {
	if generation < 1 {
		return nil, errors.New("gitauth/jwt: the signing generation must be at least 1")
	}
	if err := checkKey(key); err != nil {
		return nil, err
	}
	kid, err := keyID(&key.PublicKey)
	if err != nil {
		return nil, errors.New("gitauth/jwt: the key cannot be identified")
	}
	return &KeyRing{gen: generation, current: keyEntry{kid: kid, key: key}}, nil
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
	return Snapshot{Generation: k.gen, KeyID: k.current.kid, Key: k.current.key}
}

// Generation returns the current signing generation.
func (k *KeyRing) Generation() int64 {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.gen
}

// Rotate installs a different key as the next generation and returns it. The
// key it replaces stays valid for verification until RetirePrevious or the next
// rotation.
func (k *KeyRing) Rotate(key *ecdsa.PrivateKey) (int64, error) {
	if err := checkKey(key); err != nil {
		return 0, err
	}
	kid, err := keyID(&key.PublicKey)
	if err != nil {
		return 0, errors.New("gitauth/jwt: the key cannot be identified")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if kid == k.current.kid || (k.previous != nil && kid == k.previous.kid) {
		return 0, errors.New("gitauth/jwt: rotation requires a key that was not used before")
	}
	prev := k.current
	k.previous = &prev
	k.gen++
	k.current = keyEntry{kid: kid, key: key}
	return k.gen, nil
}

// RetirePrevious drops the previous key: tokens signed by it stop verifying.
func (k *KeyRing) RetirePrevious() {
	k.mu.Lock()
	k.previous = nil
	k.mu.Unlock()
}

// verificationKey returns the public key for kid when it is the current or the
// previous key.
func (k *KeyRing) verificationKey(kid string) (*ecdsa.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if kid == k.current.kid {
		return &k.current.key.PublicKey, true
	}
	if k.previous != nil && kid == k.previous.kid {
		return &k.previous.key.PublicKey, true
	}
	return nil, false
}

// PublicKeys is the verification half of a ring: the current public key and,
// during a rotation overlap, the previous one. It is what is distributed to a
// verifier (a worker); it carries no signing authority.
type PublicKeys struct {
	Current  *ecdsa.PublicKey
	Previous *ecdsa.PublicKey
}

// PublicKeys returns the public keys of the ring for distribution.
func (k *KeyRing) PublicKeys() PublicKeys {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := PublicKeys{Current: &k.current.key.PublicKey}
	if k.previous != nil {
		out.Previous = &k.previous.key.PublicKey
	}
	return out
}

func checkPublicKey(pub *ecdsa.PublicKey) error {
	if pub == nil || pub.Curve != elliptic.P256() {
		return errors.New("gitauth/jwt: a P-256 ECDSA public key is required")
	}
	if _, err := pub.ECDH(); err != nil {
		return errors.New("gitauth/jwt: the public key is not a valid P-256 point")
	}
	return nil
}

// MarshalPublicKeyPEM encodes a public key (PKIX) for distribution.
func MarshalPublicKeyPEM(pub *ecdsa.PublicKey) ([]byte, error) {
	if err := checkPublicKey(pub); err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ParsePublicKeyPEM reads a PKIX P-256 public key. A private key is refused:
// a verifier is never given signing material.
func ParsePublicKeyPEM(data []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("gitauth/jwt: no PUBLIC KEY PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("gitauth/jwt: the public key cannot be parsed")
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("gitauth/jwt: the key is not an ECDSA key")
	}
	return pub, checkPublicKey(pub)
}

type pubEntry struct {
	kid string
	key *ecdsa.PublicKey
}

func newPubEntry(pub *ecdsa.PublicKey) (*pubEntry, error) {
	if err := checkPublicKey(pub); err != nil {
		return nil, err
	}
	kid, err := keyID(pub)
	if err != nil {
		return nil, errors.New("gitauth/jwt: the key cannot be identified")
	}
	return &pubEntry{kid: kid, key: pub}, nil
}

// PublicKeyRing is a verifier's view of a signing ring: public keys only, so
// whoever holds it can verify tokens and can never sign one. A rotation reaches
// it as a Replace with the signer's new PublicKeys (the new current key and the
// previous one for the overlap); dropping Previous ends the overlap.
type PublicKeyRing struct {
	mu       sync.RWMutex
	current  *pubEntry
	previous *pubEntry
}

// NewPublicKeyRing validates the keys; Current is required.
func NewPublicKeyRing(keys PublicKeys) (*PublicKeyRing, error) {
	r := &PublicKeyRing{}
	if err := r.Replace(keys); err != nil {
		return nil, err
	}
	return r, nil
}

// Replace installs the distributed keys atomically. Replacing with the keys the
// ring already holds is a no-op, so it can be repeated by a poller.
func (r *PublicKeyRing) Replace(keys PublicKeys) error {
	cur, err := newPubEntry(keys.Current)
	if err != nil {
		return err
	}
	var prev *pubEntry
	if keys.Previous != nil {
		if prev, err = newPubEntry(keys.Previous); err != nil {
			return err
		}
		if prev.kid == cur.kid {
			return errors.New("gitauth/jwt: the previous key must differ from the current key")
		}
	}
	r.mu.Lock()
	r.current, r.previous = cur, prev
	r.mu.Unlock()
	return nil
}

func (r *PublicKeyRing) verificationKey(kid string) (*ecdsa.PublicKey, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if kid == r.current.kid {
		return r.current.key, true
	}
	if r.previous != nil && kid == r.previous.kid {
		return r.previous.key, true
	}
	return nil, false
}

// keyLookup finds the public key of a key ID among the keys a ring accepts.
type keyLookup interface {
	verificationKey(kid string) (*ecdsa.PublicKey, bool)
}

var (
	_ keyLookup = (*KeyRing)(nil)
	_ keyLookup = (*PublicKeyRing)(nil)
)

// keySet adapts a ring to go-oidc: exactly one signature, only ES256, only the
// ring's keys and only the token type of its purpose.
type keySet struct {
	keys keyLookup
	typ  string
}

var _ coreoidc.KeySet = keySet{}

func (s keySet) VerifySignature(_ context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{Algorithm})
	if err != nil || len(jws.Signatures) != 1 {
		return nil, errors.New("unparsable or unsupported signature")
	}
	hdr := jws.Signatures[0].Protected
	if typ, _ := hdr.ExtraHeaders[jose.HeaderType].(string); typ != s.typ {
		return nil, errors.New("wrong token type")
	}
	pub, ok := s.keys.verificationKey(hdr.KeyID)
	if !ok {
		return nil, errors.New("unknown signing key")
	}
	payload, err := jws.Verify(pub)
	if err != nil {
		return nil, errors.New("signature does not verify")
	}
	return payload, nil
}
