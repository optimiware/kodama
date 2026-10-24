package secrets

import (
	"context"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

// ServiceConfig wires the management use cases.
type ServiceConfig struct {
	// InstallationID is bound into every wrapped key and sealed value. It is an
	// explicit installation setting, never derived from request data.
	InstallationID string
	Store          Store
	Wrapper        KeyWrapper
	IDs            IDGenerator
}

// Service implements the management use cases of variables and secrets. It
// performs every key-provider call and all encryption before an atomic unit of
// work starts, and carries the expected versions into the final write; the
// Writer then applies the entity transition atomically with audit and event.
type Service struct {
	installation string
	store        Store
	wrapper      KeyWrapper
	ids          IDGenerator
}

// NewService validates the configuration. There is no default key wrapper: a
// service without one cannot create secrets, so a missing provider fails
// startup instead of falling back.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.InstallationID == "" || len(cfg.InstallationID) > 256 {
		return nil, invalidf("an installation ID is required")
	}
	if cfg.Store == nil || cfg.Wrapper == nil || cfg.IDs == nil {
		return nil, invalidf("store, key wrapper and ID generator are required")
	}
	return &Service{installation: cfg.InstallationID, store: cfg.Store, wrapper: cfg.Wrapper, ids: cfg.IDs}, nil
}

// Installation returns the bound installation identity.
func (s *Service) Installation() string { return s.installation }

// Reader exposes the read side for metadata endpoints.
func (s *Service) Reader() Reader { return s.store }

// Atomically runs fn in one unit of work for non-keyed mutations.
func (s *Service) Atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, w Writer) error) error {
	return s.store.Atomically(ctx, tenant, fn)
}

// ---- variables ----

// CreateVariableInput is the request to create a variable.
type CreateVariableInput struct {
	Scope       domain.Scope
	Name        string
	Value       runtimev1.Value
	Description string
}

// CreateVariable creates a variable inside w.
func (s *Service) CreateVariable(ctx context.Context, w Writer, m Mutation, in CreateVariableInput) (Variable, error) {
	if err := m.Validate(); err != nil {
		return Variable{}, err
	}
	id, err := s.ids.NewID("var")
	if err != nil {
		return Variable{}, err
	}
	v, err := NewVariable(in.Scope, id, in.Name, in.Value, in.Description, m.Now)
	if err != nil {
		return Variable{}, err
	}
	if err := w.CreateVariable(ctx, m, v); err != nil {
		return Variable{}, err
	}
	return v, nil
}

// EditVariable edits a variable inside w under its expected resource version.
func (s *Service) EditVariable(ctx context.Context, w Writer, m Mutation, scope domain.Scope, id string, expected domain.Version, edit VariableEdit) (Variable, error) {
	if err := m.Validate(); err != nil {
		return Variable{}, err
	}
	return w.EditVariable(ctx, m, scope, id, expected, edit)
}

// DeleteVariable logically deletes a variable inside w.
func (s *Service) DeleteVariable(ctx context.Context, w Writer, m Mutation, scope domain.Scope, id string, expected domain.Version) (Variable, error) {
	if err := m.Validate(); err != nil {
		return Variable{}, err
	}
	return w.DeleteVariable(ctx, m, scope, id, expected)
}

// ---- secrets ----

// PreparedSecret is a new secret with its first sealed value, ready for the
// final write. It contains ciphertext only.
type PreparedSecret struct {
	Secret  Secret
	Version SecretVersion
}

// PreparedVersion is a sealed next value version with the resource version it
// was prepared against.
type PreparedVersion struct {
	Scope    domain.Scope
	SecretID string
	Expected domain.Version
	Version  SecretVersion
}

// PrepareSecret seals the first value of a new secret. It calls the key
// provider and must run outside any database transaction. The caller owns
// value and should clear it afterwards.
func (s *Service) PrepareSecret(ctx context.Context, m Mutation, scope domain.Scope, name string, kind SecretKind, value []byte) (PreparedSecret, error) {
	if err := m.Validate(); err != nil {
		return PreparedSecret{}, err
	}
	if err := checkValue(value); err != nil {
		return PreparedSecret{}, err
	}
	id, err := s.ids.NewID("sec")
	if err != nil {
		return PreparedSecret{}, err
	}
	sec, err := NewSecret(scope, id, name, kind, m.Now)
	if err != nil {
		return PreparedSecret{}, err
	}
	sv, err := s.seal(ctx, m, sec, 1, value)
	if err != nil {
		return PreparedSecret{}, err
	}
	if err := sec.AcceptVersion(sec.Version(), sv, m.Now); err != nil {
		return PreparedSecret{}, err
	}
	return PreparedSecret{Secret: sec, Version: sv}, nil
}

// CreateSecret persists a prepared secret inside w.
func (s *Service) CreateSecret(ctx context.Context, w Writer, m Mutation, p PreparedSecret) (Secret, error) {
	if err := m.Validate(); err != nil {
		return Secret{}, err
	}
	if err := w.CreateSecret(ctx, m, p.Secret, p.Version); err != nil {
		return Secret{}, err
	}
	return p.Secret, nil
}

// PrepareVersion seals the next value version of a secret against the resource
// version the caller saw. A stale expected version fails before any provider
// call; the final write compares it again, atomically.
func (s *Service) PrepareVersion(ctx context.Context, m Mutation, scope domain.Scope, secretID string, expected domain.Version, value []byte) (PreparedVersion, error) {
	if err := m.Validate(); err != nil {
		return PreparedVersion{}, err
	}
	if err := checkValue(value); err != nil {
		return PreparedVersion{}, err
	}
	sec, err := s.store.GetSecret(ctx, scope, secretID)
	if err != nil {
		return PreparedVersion{}, err
	}
	if sec.Deleted() {
		return PreparedVersion{}, ErrDeleted
	}
	if sec.Version() != expected {
		return PreparedVersion{}, &ConflictError{Resource: "secret", Expected: int64(expected), Current: int64(sec.Version())}
	}
	next, err := sec.NextVersion()
	if err != nil {
		return PreparedVersion{}, err
	}
	sv, err := s.seal(ctx, m, sec, next, value)
	if err != nil {
		return PreparedVersion{}, err
	}
	return PreparedVersion{Scope: scope, SecretID: secretID, Expected: expected, Version: sv}, nil
}

// AddVersion persists a prepared version inside w. The write fails with a
// conflict if the secret changed since PrepareVersion; the sealed value is then
// discarded and the caller re-reads and re-prepares.
func (s *Service) AddVersion(ctx context.Context, w Writer, m Mutation, p PreparedVersion) (Secret, error) {
	if err := m.Validate(); err != nil {
		return Secret{}, err
	}
	return w.AddSecretVersion(ctx, m, p.Scope, p.SecretID, p.Expected, p.Version)
}

// RevokeVersion revokes one value version inside w.
func (s *Service) RevokeVersion(ctx context.Context, w Writer, m Mutation, scope domain.Scope, id string, version int64) (RevocationResult, error) {
	if err := m.Validate(); err != nil {
		return RevocationResult{}, err
	}
	return w.RevokeSecretVersion(ctx, m, scope, id, version)
}

// DeleteSecret logically deletes a secret inside w and returns the retention
// accounting.
func (s *Service) DeleteSecret(ctx context.Context, w Writer, m Mutation, scope domain.Scope, id string, expected domain.Version) (Secret, RetentionReport, error) {
	if err := m.Validate(); err != nil {
		return Secret{}, RetentionReport{}, err
	}
	return w.DeleteSecret(ctx, m, scope, id, expected)
}

func checkValue(value []byte) error {
	if len(value) == 0 || len(value) > envelope.MaxPlaintextBytes {
		return invalidf("a secret value must be 1..%d bytes", envelope.MaxPlaintextBytes)
	}
	return nil
}

func (s *Service) seal(ctx context.Context, m Mutation, sec Secret, version int64, value []byte) (SecretVersion, error) {
	sk, material, err := s.scopeKeyMaterial(ctx, m, sec.Scope().TenantID)
	if err != nil {
		return SecretVersion{}, err
	}
	defer clear(material)
	ec := EnvelopeContext(s.installation, sec.Scope(), sec.ID(), sec.Name(), version, sk.ID(), sk.Generation())
	env, err := envelope.Seal(material, ec, value)
	if err != nil {
		return SecretVersion{}, err
	}
	return NewSecretVersion(sec, version, sk.ID(), sk.Generation(), env, m.Actor.ID, m.Now)
}

// scopeKeyMaterial returns the tenant's active scope key and its plaintext.
// The first key is created under a serialized unique (tenant, generation): the
// creator that loses discards its prepared material and uses the committed key.
func (s *Service) scopeKeyMaterial(ctx context.Context, m Mutation, tenant domain.TenantID) (ScopeKey, []byte, error) {
	sk, err := s.store.ActiveScopeKey(ctx, tenant)
	switch {
	case err == nil:
		material, err := s.UnwrapScopeKey(ctx, sk)
		return sk, material, err
	case !errors.Is(err, ErrNotFound):
		return ScopeKey{}, nil, err
	}

	material, err := envelope.NewScopeKey()
	if err != nil {
		return ScopeKey{}, nil, err
	}
	id, err := s.ids.NewID("sk")
	if err != nil {
		clear(material)
		return ScopeKey{}, nil, err
	}
	wc := WrapContext{InstallationID: s.installation, TenantID: tenant, Generation: 1, KeyID: id}
	w, err := s.wrapper.Wrap(ctx, wc, material)
	if err != nil {
		clear(material)
		return ScopeKey{}, nil, err
	}
	cand, err := NewScopeKey(tenant, id, 1, w, m.Now)
	if err != nil {
		clear(material)
		return ScopeKey{}, nil, err
	}
	committed, created, err := s.store.EnsureScopeKey(ctx, m, cand)
	if err != nil {
		clear(material)
		return ScopeKey{}, nil, err
	}
	if created {
		return committed, material, nil
	}
	clear(material) // lost the race: discard prepared material
	material, err = s.UnwrapScopeKey(ctx, committed)
	return committed, material, err
}

// UnwrapScopeKey returns the plaintext of sk through the key provider. The
// caller must clear it.
func (s *Service) UnwrapScopeKey(ctx context.Context, sk ScopeKey) ([]byte, error) {
	return UnwrapScopeKey(ctx, s.wrapper, s.installation, sk)
}

// UnwrapScopeKey unwraps sk and verifies the plaintext size.
func UnwrapScopeKey(ctx context.Context, w KeyWrapper, installation string, sk ScopeKey) ([]byte, error) {
	material, err := w.Unwrap(ctx, sk.WrapContext(installation), sk.Wrapped())
	if err != nil {
		return nil, err
	}
	if len(material) != envelope.KeySize {
		clear(material)
		return nil, &KeyError{Class: KeyCorrupt, Op: "unwrap", Err: errors.New("unexpected scope key size")}
	}
	return material, nil
}

// RewrapScopeKey rewraps one scope key (the active one when keyID is empty)
// under the provider's current KEK. Value ciphertext, value versions and pins
// are untouched. A concurrent rewrap makes this fail with a ConflictError and
// changes nothing.
func (s *Service) RewrapScopeKey(ctx context.Context, m Mutation, tenant domain.TenantID, keyID string) (ScopeKey, error) {
	if err := m.Validate(); err != nil {
		return ScopeKey{}, err
	}
	var (
		sk  ScopeKey
		err error
	)
	if keyID == "" {
		sk, err = s.store.ActiveScopeKey(ctx, tenant)
	} else {
		sk, err = s.store.GetScopeKey(ctx, tenant, keyID)
	}
	if err != nil {
		return ScopeKey{}, err
	}
	material, err := s.UnwrapScopeKey(ctx, sk)
	if err != nil {
		return ScopeKey{}, err
	}
	defer clear(material)
	w, err := s.wrapper.Wrap(ctx, sk.WrapContext(s.installation), material)
	if err != nil {
		return ScopeKey{}, err
	}
	var out ScopeKey
	err = s.store.Atomically(ctx, tenant, func(ctx context.Context, wr Writer) error {
		var err error
		out, err = wr.RewrapScopeKey(ctx, m, tenant, sk.ID(), sk.Revision(), w)
		return err
	})
	if err != nil {
		return ScopeKey{}, fmt.Errorf("secrets: rewrap: %w", err)
	}
	return out, nil
}
