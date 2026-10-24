package secrets

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// KEKRef is the immutable reference of one key-encryption-key version. It
// identifies which KEK wrapped a scope key, so retained backups can be restored
// with the right retained key. It never contains key material.
type KEKRef struct {
	Provider string // for example "keyfile-dev"
	ID       string // key identity within the provider
	Version  string // immutable version label
}

// Validate checks that every component is set and bounded.
func (r KEKRef) Validate() error {
	for _, f := range []string{r.Provider, r.ID, r.Version} {
		if f == "" || len(f) > 256 {
			return invalidf("KEK reference components must be 1..256 bytes")
		}
	}
	return nil
}

// WrappedKey is a scope key encrypted by a KEK.
type WrappedKey struct {
	Bytes     []byte
	KEK       KEKRef
	Algorithm string
}

// WrapContext is bound into every wrap so a wrapped scope key cannot be moved
// to another installation, tenant, generation or key identity.
type WrapContext struct {
	InstallationID string
	TenantID       domain.TenantID
	Generation     int64
	KeyID          string
}

// KeyWrapper is the consumer-owned port to the key provider that holds the
// KEK. Implementations wrap and unwrap one random 32-byte scope key per tenant
// generation; they never see secret values. Failures are *KeyError. A
// production KMS adapter (T-028) implements the same port; nothing falls back
// to another provider or to plaintext.
type KeyWrapper interface {
	// Wrap encrypts key under the provider's current KEK.
	Wrap(ctx context.Context, wc WrapContext, key []byte) (WrappedKey, error)
	// Unwrap decrypts w using the KEK version it names.
	Unwrap(ctx context.Context, wc WrapContext, w WrappedKey) ([]byte, error)
}

// ScopeKeyRecord is the plain persisted state of a ScopeKey.
type ScopeKeyRecord struct {
	TenantID        domain.TenantID
	ID              string
	Generation      int64
	CurrentRevision int64
	Wrapped         WrappedKey // the current wrap
	CreatedAt       time.Time
	WrappedAt       time.Time
}

// ScopeKey is the wrapped per-tenant-generation key from which value keys are
// derived. Rewrapping changes its wrapping revision and wrapped bytes, never
// its identity or generation, so no value ciphertext is touched.
type ScopeKey struct{ rec ScopeKeyRecord }

// NewScopeKey creates revision 1 of a scope key from its first wrap.
func NewScopeKey(tenant domain.TenantID, id string, generation int64, w WrappedKey, now time.Time) (ScopeKey, error) {
	return RestoreScopeKey(ScopeKeyRecord{
		TenantID: tenant, ID: id, Generation: generation, CurrentRevision: 1, Wrapped: w,
		CreatedAt: now.UTC(), WrappedAt: now.UTC(),
	})
}

// RestoreScopeKey rebuilds a scope key from persisted state and validates it.
func RestoreScopeKey(r ScopeKeyRecord) (ScopeKey, error) {
	if err := r.TenantID.Validate(); err != nil {
		return ScopeKey{}, err
	}
	if _, err := validID(r.ID); err != nil {
		return ScopeKey{}, err
	}
	if r.Generation < 1 || r.CurrentRevision < 1 || r.CurrentRevision > domain.MaxSafeInteger {
		return ScopeKey{}, invalidf("scope key generation and revision must be positive")
	}
	if len(r.Wrapped.Bytes) == 0 || len(r.Wrapped.Bytes) > 4096 || r.Wrapped.Algorithm == "" {
		return ScopeKey{}, invalidf("scope key wrap is missing")
	}
	if err := r.Wrapped.KEK.Validate(); err != nil {
		return ScopeKey{}, err
	}
	return ScopeKey{rec: r}, nil
}

func (k ScopeKey) Record() ScopeKeyRecord    { return k.rec }
func (k ScopeKey) TenantID() domain.TenantID { return k.rec.TenantID }
func (k ScopeKey) ID() string                { return k.rec.ID }
func (k ScopeKey) Generation() int64         { return k.rec.Generation }
func (k ScopeKey) Revision() int64           { return k.rec.CurrentRevision }
func (k ScopeKey) Wrapped() WrappedKey       { return k.rec.Wrapped }

// WrapContext returns the binding used to wrap and unwrap this key.
func (k ScopeKey) WrapContext(installation string) WrapContext {
	return WrapContext{InstallationID: installation, TenantID: k.rec.TenantID, Generation: k.rec.Generation, KeyID: k.rec.ID}
}

// Rewrap replaces the current wrap with w if expected is the current wrapping
// revision, and returns the new revision. The previous wrap stays retained by
// the adapter's history.
func (k *ScopeKey) Rewrap(expected int64, w WrappedKey, now time.Time) error {
	if expected != k.rec.CurrentRevision {
		return &ConflictError{Resource: "wrap revision", Expected: expected, Current: k.rec.CurrentRevision}
	}
	if len(w.Bytes) == 0 || w.Algorithm == "" {
		return invalidf("a wrapped key is required")
	}
	if err := w.KEK.Validate(); err != nil {
		return err
	}
	if k.rec.CurrentRevision >= domain.MaxSafeInteger {
		return invalidf("wrap revision space exhausted")
	}
	k.rec.CurrentRevision++
	k.rec.Wrapped = w
	k.rec.WrappedAt = now.UTC()
	return nil
}
