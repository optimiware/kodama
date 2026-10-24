package secretkeys

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
)

const (
	awsFormat = "aws-kms-v1"
	gcpFormat = "gcp-kms-v1"
)

// Crypter is the encrypt/decrypt half of one cloud KMS key.
type Crypter interface {
	Encrypt(ctx context.Context, keyID string, plaintext, aad []byte) ([]byte, error)
	Decrypt(ctx context.Context, keyID string, ciphertext, aad []byte) ([]byte, error)
}

// Cloud wraps with one configured KMS key. retained names may unwrap only.
type Cloud struct {
	provider string
	format   string
	crypter  Crypter
	active   string
	allowed  map[string]bool
}

func newCloud(provider, format string, crypter Crypter, active string, retained []string) (*Cloud, error) {
	if crypter == nil || active == "" {
		return nil, ErrInvalid
	}
	p := &Cloud{provider: provider, format: format, crypter: crypter, active: active, allowed: map[string]bool{active: true}}
	for _, id := range retained {
		if id == "" {
			return nil, ErrInvalid
		}
		p.allowed[id] = true
	}
	return p, nil
}

func (p *Cloud) Wrap(ctx context.Context, key, binding []byte) (Wrapped, error) {
	if len(key) != 32 {
		return Wrapped{}, ErrInvalid
	}
	ct, err := p.crypter.Encrypt(ctx, p.active, key, binding)
	if err != nil {
		return Wrapped{}, err
	}
	if len(ct) == 0 || len(ct) > 8192 {
		return Wrapped{}, ErrCiphertext
	}
	return Wrapped{Provider: p.provider, KeyRef: p.active, Format: p.format, Ciphertext: ct}, nil
}

func (p *Cloud) Unwrap(ctx context.Context, w Wrapped, binding []byte) ([]byte, error) {
	if w.Provider != p.provider || w.Format != p.format || !p.allowed[w.KeyRef] || len(w.Ciphertext) == 0 || len(w.Ciphertext) > 8192 {
		return nil, ErrCiphertext
	}
	key, err := p.crypter.Decrypt(ctx, w.KeyRef, w.Ciphertext, binding)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		clear(key)
		return nil, ErrCiphertext
	}
	return key, nil
}

func bindingContext(binding []byte) string {
	sum := sha256.Sum256(binding)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
