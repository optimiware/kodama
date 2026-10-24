// Package envelope is the single codec of Kodama's local secret envelope: it
// derives a per-secret-version key from a tenant-generation scope key with
// HKDF-SHA-256 and seals the value with AES-256-GCM, binding the ownership
// context as authenticated data.
//
// Everything that fixes the byte framing lives here: the format version, the
// algorithm identifier, the HKDF salt and info layout and the associated-data
// layout. The specification fixes the algorithms and the bound fields (variables
// and secrets, "Encryption") but not their serialization, so this framing is an
// implementation choice recorded in the module README and frozen by the
// deterministic vectors in codec_test.go. Compatibility with the platform's
// secret rows is neither claimed nor needed: no row is copied between systems.
//
// The package is pure computation: no database, key provider or clock.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Format and algorithm identifiers persisted beside every ciphertext.
const (
	// FormatV1 is the only supported framing.
	FormatV1 = 1
	// AlgorithmAES256GCM names AES-256-GCM sealing under an HKDF-SHA-256
	// derived key.
	AlgorithmAES256GCM = "aes-256-gcm/hkdf-sha256"

	// KeySize is the size of a scope key and of a derived value key.
	KeySize = 32
	// NonceSize is the GCM nonce size.
	NonceSize = 12
	// MaxPlaintextBytes bounds one secret value.
	MaxPlaintextBytes = 128 << 10
)

// Errors. They carry no value, key or context data.
var (
	// ErrAuthentication: the ciphertext, nonce or any bound context field was
	// altered, or the wrong key was used. Never retryable.
	ErrAuthentication = errors.New("envelope: authentication failed")
	// ErrUnsupported: unknown format version or algorithm.
	ErrUnsupported = errors.New("envelope: unsupported format or algorithm")
	// ErrMalformed: structurally invalid input (sizes, empty fields).
	ErrMalformed = errors.New("envelope: malformed input")
)

// Context is the ownership and version binding of one sealed value. Every field
// is authenticated; changing any of them makes Open fail.
type Context struct {
	InstallationID string
	TenantID       string
	ProjectID      string
	SecretID       string
	// Name is the secret's immutable name.
	Name    string
	Version int64
	// ScopeKeyID and KeyGeneration identify the scope key the value was sealed
	// under. They are authenticated but not part of key derivation: a rewrap
	// never changes them.
	ScopeKeyID    string
	KeyGeneration int64
}

func (c Context) validate() error {
	for _, f := range []string{c.InstallationID, c.TenantID, c.ProjectID, c.SecretID, c.Name, c.ScopeKeyID} {
		if f == "" || len(f) > 1024 {
			return fmt.Errorf("%w: context field empty or too long", ErrMalformed)
		}
	}
	if c.Version < 1 || c.KeyGeneration < 1 {
		return fmt.Errorf("%w: version and key generation must be positive", ErrMalformed)
	}
	return nil
}

// Envelope is the persisted sealed value. Its fields map one-to-one to columns.
type Envelope struct {
	FormatVersion int
	Algorithm     string
	Nonce         []byte
	Ciphertext    []byte
}

const (
	hkdfSalt  = "kodama/secret-value/hkdf-salt/v1"
	aadPrefix = "kodama/secret-value/aad/v1"
)

func frame(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// derivationInfo is the HKDF info: ownership and version only, so a scope-key
// rewrap (which changes neither) keeps every derived key stable.
func derivationInfo(c Context) string {
	var b []byte
	b = frame(b, "kodama/secret-value/info/v1")
	for _, f := range []string{c.InstallationID, c.TenantID, c.ProjectID, c.SecretID, c.Name} {
		b = frame(b, f)
	}
	b = binary.BigEndian.AppendUint64(b, uint64(c.Version))
	return string(b)
}

// associatedData is the GCM additional data: the full context plus the format
// and algorithm, so a downgrade of either fails authentication.
func associatedData(c Context, format int, algorithm string) []byte {
	var b []byte
	b = frame(b, aadPrefix)
	b = binary.BigEndian.AppendUint32(b, uint32(format))
	b = frame(b, algorithm)
	for _, f := range []string{c.InstallationID, c.TenantID, c.ProjectID, c.SecretID, c.Name, c.ScopeKeyID} {
		b = frame(b, f)
	}
	b = binary.BigEndian.AppendUint64(b, uint64(c.Version))
	b = binary.BigEndian.AppendUint64(b, uint64(c.KeyGeneration))
	return b
}

func valueKey(scopeKey []byte, c Context) ([]byte, error) {
	if len(scopeKey) != KeySize {
		return nil, fmt.Errorf("%w: scope key must be %d bytes", ErrMalformed, KeySize)
	}
	return hkdf.Key(sha256.New, scopeKey, []byte(hkdfSalt), derivationInfo(c), KeySize)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext under a key derived from scopeKey and c, with a fresh
// random nonce.
func Seal(scopeKey []byte, c Context, plaintext []byte) (Envelope, error) {
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, fmt.Errorf("envelope: nonce: %w", err)
	}
	return sealWithNonce(scopeKey, c, plaintext, nonce)
}

func sealWithNonce(scopeKey []byte, c Context, plaintext, nonce []byte) (Envelope, error) {
	if err := c.validate(); err != nil {
		return Envelope{}, err
	}
	if len(plaintext) > MaxPlaintextBytes {
		return Envelope{}, fmt.Errorf("%w: value exceeds %d bytes", ErrMalformed, MaxPlaintextBytes)
	}
	if len(nonce) != NonceSize {
		return Envelope{}, fmt.Errorf("%w: nonce size", ErrMalformed)
	}
	key, err := valueKey(scopeKey, c)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(key)
	aead, err := newGCM(key)
	if err != nil {
		return Envelope{}, fmt.Errorf("envelope: cipher: %w", err)
	}
	ct := aead.Seal(nil, nonce, plaintext, associatedData(c, FormatV1, AlgorithmAES256GCM))
	return Envelope{FormatVersion: FormatV1, Algorithm: AlgorithmAES256GCM, Nonce: append([]byte(nil), nonce...), Ciphertext: ct}, nil
}

// Open authenticates and decrypts e. The caller owns the returned plaintext and
// should clear it when done.
func Open(scopeKey []byte, c Context, e Envelope) ([]byte, error) {
	if e.FormatVersion != FormatV1 || e.Algorithm != AlgorithmAES256GCM {
		return nil, ErrUnsupported
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if len(e.Nonce) != NonceSize || len(e.Ciphertext) < 16 || len(e.Ciphertext) > MaxPlaintextBytes+16 {
		return nil, fmt.Errorf("%w: nonce or ciphertext size", ErrMalformed)
	}
	key, err := valueKey(scopeKey, c)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	aead, err := newGCM(key)
	if err != nil {
		return nil, fmt.Errorf("envelope: cipher: %w", err)
	}
	pt, err := aead.Open(nil, e.Nonce, e.Ciphertext, associatedData(c, e.FormatVersion, e.Algorithm))
	if err != nil {
		return nil, ErrAuthentication
	}
	return pt, nil
}

// NewScopeKey returns a fresh random scope key.
func NewScopeKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, fmt.Errorf("envelope: scope key: %w", err)
	}
	return k, nil
}
