package production

import (
	"context"
	"crypto/subtle"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

const (
	scopeKeySize = 32
	// maxWrapBytes is the limit the consumer persists (ScopeKey validation).
	maxWrapBytes = 4096
)

// Provider is the production secrets.KeyWrapper. It wraps and unwraps one
// 32-byte scope key per tenant generation through one cloud KMS; it never sees
// a secret value and holds no key material. It is immutable and safe for
// concurrent use.
//
// Persisted form of a wrap: WrappedKey.Bytes is the provider ciphertext,
// KEK = {cloud, logical KEK ID, version label}, Algorithm = the provider wrap
// format. The cloud key resource is not stored in the row; it is resolved from
// the immutable version label through the installation configuration.
type Provider struct {
	cloud     string
	format    string
	kekID     string
	current   resource
	resources map[string]resource
	inner     secretkeys.Provider
}

var _ secrets.KeyWrapper = (*Provider)(nil)

func newBridge(p plan, inner secretkeys.Provider) *Provider {
	resources := make(map[string]resource, len(p.byLabel))
	for k, v := range p.byLabel {
		resources[k] = v
	}
	return &Provider{cloud: p.provider, format: p.format, kekID: p.kekID, current: p.current, resources: resources, inner: inner}
}

// CurrentRef is the KEK reference recorded by every new wrap.
func (p *Provider) CurrentRef() secrets.KEKRef {
	return secrets.KEKRef{Provider: p.cloud, ID: p.kekID, Version: p.current.label}
}

// Wrap encrypts key under the current KEK version only.
func (p *Provider) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	if err := ctx.Err(); err != nil {
		return secrets.WrappedKey{}, keyError(secrets.KeyUnavailable, "wrap", err)
	}
	if len(key) != scopeKeySize {
		return secrets.WrappedKey{}, keyError(secrets.KeyCorrupt, "wrap", errWrapContext)
	}
	ref := p.CurrentRef()
	bind, err := binding(wc, ref, p.format)
	if err != nil {
		return secrets.WrappedKey{}, err
	}
	defer clear(bind)
	w, err := p.inner.Wrap(ctx, key, bind)
	if err != nil {
		return secrets.WrappedKey{}, classify(ctx, "wrap", err)
	}
	// The provider must answer for exactly the key and format that were asked.
	if w.Provider != p.cloud || w.Format != p.format || w.KeyRef != p.current.keyRef || len(w.Ciphertext) == 0 || len(w.Ciphertext) > maxWrapBytes {
		return secrets.WrappedKey{}, keyError(secrets.KeyCorrupt, "wrap", errWrapRejected)
	}
	return secrets.WrappedKey{Bytes: append([]byte(nil), w.Ciphertext...), KEK: ref, Algorithm: p.format}, nil
}

// Unwrap decrypts w with the KEK version it names. A reference of another
// provider, KEK identity, algorithm or version is refused before any network
// call; there is no fallback to another version or cloud.
func (p *Provider) Unwrap(ctx context.Context, wc secrets.WrapContext, w secrets.WrappedKey) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, keyError(secrets.KeyUnavailable, "unwrap", err)
	}
	if w.KEK.Provider != p.cloud || w.KEK.ID != p.kekID || w.Algorithm != p.format {
		return nil, keyError(secrets.KeyNotFound, "unwrap", errKeyMissing)
	}
	res, ok := p.resources[w.KEK.Version]
	if !ok {
		return nil, keyError(secrets.KeyNotFound, "unwrap", errKeyMissing)
	}
	if len(w.Bytes) == 0 || len(w.Bytes) > maxWrapBytes {
		return nil, keyError(secrets.KeyCorrupt, "unwrap", errWrapRejected)
	}
	bind, err := binding(wc, w.KEK, p.format)
	if err != nil {
		return nil, err
	}
	defer clear(bind)
	key, err := p.inner.Unwrap(ctx, secretkeys.Wrapped{
		Provider: p.cloud, KeyRef: res.keyRef, Format: p.format, Ciphertext: append([]byte(nil), w.Bytes...),
	}, bind)
	if err != nil {
		return nil, classify(ctx, "unwrap", err)
	}
	if len(key) != scopeKeySize {
		clear(key)
		return nil, keyError(secrets.KeyCorrupt, "unwrap", errWrapRejected)
	}
	return key, nil
}

// equalKeys compares recovered key material without timing leakage.
func equalKeys(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
