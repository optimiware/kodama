package keys

import (
	"context"
	"crypto/rand"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// Cipher is the envelope primitive over one key provider. It encrypts secret
// plaintext under a fresh data key, wraps that data key with the provider and
// authenticates the exact associated-data encoding in both operations.
type Cipher struct {
	provider Provider
	random   io.Reader

	randomMu sync.Mutex
}

// NewCipher returns an envelope cipher over one key provider. A nil random
// means crypto/rand; a nil provider is refused.
func NewCipher(provider Provider, random io.Reader) (*Cipher, error) {
	if provider == nil {
		return nil, fault.New(fault.InvalidInput, "envelope cipher requires a key provider")
	}
	if random == nil {
		random = rand.Reader
	}
	return &Cipher{provider: provider, random: random}, nil
}

// Seal encrypts plaintext under a fresh 32-byte data key and wraps that data
// key with the provider. The returned envelope carries no plaintext. An invalid
// associated identity or an oversized value is refused before any randomness
// or key operation.
func (cipher *Cipher) Seal(ctx context.Context, associated secret.AssociatedData, plaintext []byte) (secret.Envelope, error) {
	if err := associated.Validate(); err != nil {
		return secret.Envelope{}, err
	}
	if err := secret.DefaultBounds.CheckValue(len(plaintext)); err != nil {
		return secret.Envelope{}, err
	}
	if err := ctx.Err(); err != nil {
		return secret.Envelope{}, err
	}
	associatedData, err := associated.Bytes()
	if err != nil {
		return secret.Envelope{}, err
	}

	dataKey, err := cipher.randomBytes(secret.DataKeyBytes)
	if err != nil {
		return secret.Envelope{}, err
	}
	defer clear(dataKey)
	nonce, err := cipher.randomBytes(secret.ContentNonceBytes)
	if err != nil {
		return secret.Envelope{}, err
	}

	wrapped, err := cipher.provider.Wrap(ctx, secret.WrapRequest{
		KeyRef:         cipher.provider.KeyRef(),
		KeyVersion:     associated.KeyVersion,
		Plaintext:      dataKey,
		AssociatedData: associatedData,
	})
	if err != nil {
		return secret.Envelope{}, err
	}
	if err := wrapped.Validate(); err != nil {
		return secret.Envelope{}, err
	}
	if wrapped.KeyRef != cipher.provider.KeyRef() || wrapped.KeyVersion != associated.KeyVersion {
		return secret.Envelope{}, fault.New(fault.InvalidInput, "key provider returned a wrapped key for another reference or version")
	}

	aead, err := newAEAD(dataKey)
	if err != nil {
		return secret.Envelope{}, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, associatedData)
	return secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     wrapped.KeyRef,
		KeyVersion: wrapped.KeyVersion,
		Nonce:      nonce,
		Ciphertext: ciphertext,
		WrappedKey: wrapped.Wrapped,
		WrapNonce:  wrapped.Nonce,
	}, nil
}

// Open authenticates the envelope against associated and returns the
// plaintext. Every refusal returns no plaintext: a structurally invalid
// envelope, a mismatched identity or an unknown key reference is refused
// before any key operation, and every authentication failure is InvalidInput.
func (cipher *Cipher) Open(ctx context.Context, associated secret.AssociatedData, envelope secret.Envelope) ([]byte, error) {
	if err := envelope.Validate(); err != nil {
		return nil, err
	}
	if err := associated.Validate(); err != nil {
		return nil, err
	}
	if err := envelope.ValidateFor(associated); err != nil {
		return nil, err
	}
	if envelope.KeyRef != cipher.provider.KeyRef() {
		return nil, unknownKeyReference()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	associatedData, err := associated.Bytes()
	if err != nil {
		return nil, err
	}

	dataKey, err := cipher.provider.Unwrap(ctx, secret.UnwrapRequest{
		KeyRef:         envelope.KeyRef,
		KeyVersion:     envelope.KeyVersion,
		Wrapped:        envelope.WrappedKey,
		Nonce:          envelope.WrapNonce,
		AssociatedData: associatedData,
	})
	if err != nil {
		return nil, err
	}
	defer clear(dataKey)

	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, associatedData)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "envelope authentication failed", err)
	}
	return plaintext, nil
}

// randomBytes draws one nonce or data key from the cipher's randomness source.
func (cipher *Cipher) randomBytes(size int) ([]byte, error) {
	buffer := make([]byte, size)
	cipher.randomMu.Lock()
	_, err := io.ReadFull(cipher.random, buffer)
	cipher.randomMu.Unlock()
	if err != nil {
		clear(buffer)
		return nil, fault.Wrap(fault.InternalFailure, "read envelope randomness", err)
	}
	return buffer, nil
}
