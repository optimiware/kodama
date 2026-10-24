// Package secretstest holds the memory adapters and the contract suites of the
// variables and secrets module: the in-memory store over memdb, the pin and
// resolution adapters, the ephemeral key wrapper, and the exported contracts
// that the memory and PostgreSQL adapters both run (SD-13).
package secretstest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/secrets"
)

const (
	// EphemeralProvider is recorded in every KEK reference the wrapper issues.
	EphemeralProvider = "memory-ephemeral"
	// EphemeralKeyID is the key identity of the wrapper's KEK.
	EphemeralKeyID = "ephemeral"
	// EphemeralAlgorithm names AES-256-GCM key wrapping with a random nonce.
	EphemeralAlgorithm = "aes-256-gcm/wrap-v1"

	kekSize   = 32
	nonceSize = 12
)

// EphemeralWrapper is the `development` secrets.KeyWrapper: key-encryption keys
// generated at construction and held only in memory, so every wrap dies with the
// process. It wraps scope keys exactly like the keyfile provider (AES-256-GCM
// with the installation, tenant, generation, key identity and KEK version bound
// as additional data) and never sees a secret value.
//
// Rotate adds a new KEK version and makes it current; older versions stay
// available to unwrap what they wrapped. SetDown simulates a provider outage.
// The wrapper is development-only: production refuses it (SD-12), so a
// development KEK can never become a fallback.
type EphemeralWrapper struct {
	mu      sync.Mutex
	keys    map[string][]byte
	current string
	next    int
	down    error
	wraps   int
	unwraps int
}

var _ secrets.KeyWrapper = (*EphemeralWrapper)(nil)

// NewEphemeralWrapper generates the first KEK, version "v1".
func NewEphemeralWrapper() (*EphemeralWrapper, error) {
	w := &EphemeralWrapper{keys: map[string][]byte{}}
	if _, err := w.Rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*EphemeralWrapper) DevelopmentOnly() bool { return true }

// Rotate generates a new KEK version, makes it current and returns its label.
func (w *EphemeralWrapper) Rotate() (string, error) {
	key := make([]byte, kekSize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("secretstest: generate KEK: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	v := fmt.Sprintf("v%d", w.next)
	w.keys[v], w.current = key, v
	return v, nil
}

// CurrentRef is the KEK reference of new wraps.
func (w *EphemeralWrapper) CurrentRef() secrets.KEKRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	return secrets.KEKRef{Provider: EphemeralProvider, ID: EphemeralKeyID, Version: w.current}
}

// SetDown makes every operation fail as an unavailable provider; nil restores it.
func (w *EphemeralWrapper) SetDown(err error) {
	w.mu.Lock()
	w.down = err
	w.mu.Unlock()
}

// Calls reports how many wraps and unwraps were attempted.
func (w *EphemeralWrapper) Calls() (wraps, unwraps int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wraps, w.unwraps
}

func frame(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func aad(wc secrets.WrapContext, ref secrets.KEKRef) []byte {
	b := frame(nil, "kodama/memory-wrap/v1")
	for _, f := range []string{wc.InstallationID, string(wc.TenantID), wc.KeyID, ref.Provider, ref.ID, ref.Version} {
		b = frame(b, f)
	}
	return binary.BigEndian.AppendUint64(b, uint64(wc.Generation))
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (w *EphemeralWrapper) unavailable(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return &secrets.KeyError{Class: secrets.KeyUnavailable, Op: op, Err: err}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if op == "wrap" {
		w.wraps++
	} else {
		w.unwraps++
	}
	if w.down != nil {
		return &secrets.KeyError{Class: secrets.KeyUnavailable, Op: op, Err: w.down}
	}
	return nil
}

func (w *EphemeralWrapper) kek(version string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k, ok := w.keys[version]
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), k...), nil
}

// Wrap implements secrets.KeyWrapper.
func (w *EphemeralWrapper) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	if err := w.unavailable(ctx, "wrap"); err != nil {
		return secrets.WrappedKey{}, err
	}
	if len(key) != kekSize {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "wrap", Err: errors.New("scope key must be 32 bytes")}
	}
	ref := w.CurrentRef()
	kek, _ := w.kek(ref.Version)
	defer clear(kek)
	aead, err := gcm(kek)
	if err != nil {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "wrap", Err: err}
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "wrap", Err: err}
	}
	return secrets.WrappedKey{Bytes: aead.Seal(nonce, nonce, key, aad(wc, ref)), KEK: ref, Algorithm: EphemeralAlgorithm}, nil
}

// Unwrap implements secrets.KeyWrapper. A KEK version that this wrapper never
// held is KeyNotFound: there is no fallback to another version or provider.
func (w *EphemeralWrapper) Unwrap(ctx context.Context, wc secrets.WrapContext, wk secrets.WrappedKey) ([]byte, error) {
	if err := w.unavailable(ctx, "unwrap"); err != nil {
		return nil, err
	}
	if wk.KEK.Provider != EphemeralProvider || wk.KEK.ID != EphemeralKeyID || wk.Algorithm != EphemeralAlgorithm {
		return nil, &secrets.KeyError{Class: secrets.KeyNotFound, Op: "unwrap", Err: errors.New("wrap was not produced by this provider and key")}
	}
	kek, _ := w.kek(wk.KEK.Version)
	if kek == nil {
		return nil, &secrets.KeyError{Class: secrets.KeyNotFound, Op: "unwrap", Err: fmt.Errorf("key version %q is not held", wk.KEK.Version)}
	}
	defer clear(kek)
	aead, err := gcm(kek)
	if err != nil {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: err}
	}
	if len(wk.Bytes) < nonceSize+aead.Overhead() {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: errors.New("wrapped key is malformed")}
	}
	key, err := aead.Open(nil, wk.Bytes[:nonceSize], wk.Bytes[nonceSize:], aad(wc, wk.KEK))
	if err != nil {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: errors.New("wrapped key failed authentication")}
	}
	return key, nil
}
