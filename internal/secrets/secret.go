package secrets

import (
	"fmt"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

// SecretKind is the immutable payload type of a secret. Providers' credential
// schemas are interpreted by their consumers, never by this module.
type SecretKind string

const (
	KindGeneric    SecretKind = "generic"
	KindGit        SecretKind = "git"
	KindModel      SecretKind = "model"
	KindMCP        SecretKind = "mcp"
	KindKubeconfig SecretKind = "kubeconfig"
)

// Kinds lists the supported kinds.
func Kinds() []SecretKind {
	return []SecretKind{KindGeneric, KindGit, KindModel, KindMCP, KindKubeconfig}
}

// Valid reports whether k is a supported kind.
func (k SecretKind) Valid() bool {
	for _, c := range Kinds() {
		if k == c {
			return true
		}
	}
	return false
}

// SecretNamePattern equals the runtime SecretRef name segment, so every secret
// name can be referenced as secret://project/<name>.
const SecretNamePattern = `^[a-z][a-z0-9-]{0,62}$`

var secretNameRegexp = regexp.MustCompile(SecretNamePattern)

// ValidateSecretName checks a secret name.
func ValidateSecretName(name string) error {
	if !secretNameRegexp.MatchString(name) {
		return invalidf("secret name must match %s", SecretNamePattern)
	}
	return nil
}

// SecretRecord is the plain persisted metadata of a Secret.
type SecretRecord struct {
	Scope          domain.Scope
	ID             string
	Name           string
	Kind           SecretKind
	CurrentVersion int64 // 0 only before the first value is accepted
	Version        domain.Version
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
}

// Secret is the metadata aggregate of an encrypted value history: immutable
// identity, name and kind, a monotonically increasing value version, and a
// separate resource version for management concurrency. It never holds a value.
type Secret struct{ rec SecretRecord }

// NewSecret creates a secret that has no value yet; accept its first version
// with AcceptVersion before persisting it.
func NewSecret(scope domain.Scope, id, name string, kind SecretKind, now time.Time) (Secret, error) {
	return RestoreSecret(SecretRecord{
		Scope: scope, ID: id, Name: name, Kind: kind, Version: 1,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	})
}

// RestoreSecret rebuilds a secret from persisted state and validates it.
func RestoreSecret(r SecretRecord) (Secret, error) {
	if err := r.Scope.Validate(); err != nil {
		return Secret{}, err
	}
	if _, err := validID(r.ID); err != nil {
		return Secret{}, err
	}
	if err := ValidateSecretName(r.Name); err != nil {
		return Secret{}, err
	}
	if !r.Kind.Valid() {
		return Secret{}, invalidf("unsupported secret kind (one of generic, git, model, mcp, kubeconfig)")
	}
	if err := r.Version.Validate(); err != nil {
		return Secret{}, err
	}
	if r.CurrentVersion < 0 || r.CurrentVersion > domain.MaxSafeInteger {
		return Secret{}, invalidf("current version out of range")
	}
	return Secret{rec: r}, nil
}

func (s Secret) Record() SecretRecord    { return s.rec }
func (s Secret) Scope() domain.Scope     { return s.rec.Scope }
func (s Secret) ID() string              { return s.rec.ID }
func (s Secret) Name() string            { return s.rec.Name }
func (s Secret) Kind() SecretKind        { return s.rec.Kind }
func (s Secret) CurrentVersion() int64   { return s.rec.CurrentVersion }
func (s Secret) Version() domain.Version { return s.rec.Version }
func (s Secret) CreatedAt() time.Time    { return s.rec.CreatedAt }
func (s Secret) UpdatedAt() time.Time    { return s.rec.UpdatedAt }
func (s Secret) Deleted() bool           { return s.rec.DeletedAt != nil }
func (s Secret) DeletedAt() *time.Time   { return cloneTime(s.rec.DeletedAt) }
func (s Secret) String() string          { return fmt.Sprintf("secret %s/%s", s.rec.Scope, s.rec.Name) }
func (s Secret) Ref() string             { return FormatSecretRef(s.rec.Name) }
func (s Secret) NextVersion() (int64, error) {
	if s.rec.CurrentVersion >= domain.MaxSafeInteger {
		return 0, invalidf("version space exhausted")
	}
	return s.rec.CurrentVersion + 1, nil
}

// SecretVersionRecord is the plain persisted state of a SecretVersion.
type SecretVersionRecord struct {
	Scope         domain.Scope
	SecretID      string
	Version       int64
	ScopeKeyID    string
	KeyGeneration int64
	Envelope      envelope.Envelope
	CreatedAt     time.Time
	CreatedBy     string
	RevokedAt     *time.Time
	RevokedBy     string
}

// SecretVersion is one immutable encrypted value. Only revocation, which is one
// way, ever changes it.
type SecretVersion struct{ rec SecretVersionRecord }

// NewSecretVersion wraps a sealed value. The envelope must have been sealed with
// the context returned by EnvelopeContext for the same fields.
func NewSecretVersion(s Secret, version int64, scopeKeyID string, generation int64, env envelope.Envelope, createdBy string, now time.Time) (SecretVersion, error) {
	return RestoreSecretVersion(SecretVersionRecord{
		Scope: s.rec.Scope, SecretID: s.rec.ID, Version: version, ScopeKeyID: scopeKeyID, KeyGeneration: generation,
		Envelope: env, CreatedAt: now.UTC(), CreatedBy: createdBy,
	})
}

// RestoreSecretVersion rebuilds a version from persisted state.
func RestoreSecretVersion(r SecretVersionRecord) (SecretVersion, error) {
	if err := r.Scope.Validate(); err != nil {
		return SecretVersion{}, err
	}
	if _, err := validID(r.SecretID); err != nil {
		return SecretVersion{}, err
	}
	if err := domain.Version(r.Version).Validate(); err != nil {
		return SecretVersion{}, err
	}
	if _, err := validID(r.ScopeKeyID); err != nil {
		return SecretVersion{}, err
	}
	if r.KeyGeneration < 1 {
		return SecretVersion{}, invalidf("key generation must be positive")
	}
	if (r.RevokedAt == nil) != (r.RevokedBy == "") {
		return SecretVersion{}, invalidf("revocation time and actor are set together")
	}
	return SecretVersion{rec: r}, nil
}

func (v SecretVersion) Record() SecretVersionRecord { return v.rec }
func (v SecretVersion) Version() int64              { return v.rec.Version }
func (v SecretVersion) SecretID() string            { return v.rec.SecretID }
func (v SecretVersion) Revoked() bool               { return v.rec.RevokedAt != nil }
func (v SecretVersion) RevokedAt() *time.Time       { return cloneTime(v.rec.RevokedAt) }
func (v SecretVersion) CreatedAt() time.Time        { return v.rec.CreatedAt }
func (v SecretVersion) Envelope() envelope.Envelope { return v.rec.Envelope }

// EnvelopeContext is the authenticated binding of this version under the
// installation. It is the single place ownership context is assembled.
func (v SecretVersion) EnvelopeContext(installation, secretName string) envelope.Context {
	return EnvelopeContext(installation, v.rec.Scope, v.rec.SecretID, secretName, v.rec.Version, v.rec.ScopeKeyID, v.rec.KeyGeneration)
}

// EnvelopeContext assembles the authenticated ownership context of a value.
func EnvelopeContext(installation string, scope domain.Scope, secretID, name string, version int64, scopeKeyID string, generation int64) envelope.Context {
	return envelope.Context{
		InstallationID: installation, TenantID: string(scope.TenantID), ProjectID: string(scope.ProjectID),
		SecretID: secretID, Name: name, Version: version, ScopeKeyID: scopeKeyID, KeyGeneration: generation,
	}
}

// AcceptVersion appends the next value version. The first version leaves the
// resource version at 1 (the secret is created with its value); later versions
// bump it. expected must equal the current resource version except for the
// first. The new version must be exactly current+1: versions never skip,
// repeat or move backwards.
func (s *Secret) AcceptVersion(expected domain.Version, v SecretVersion, now time.Time) error {
	if s.Deleted() {
		return ErrDeleted
	}
	first := s.rec.CurrentVersion == 0
	if !first && expected != s.rec.Version {
		return &ConflictError{Resource: "secret", Expected: int64(expected), Current: int64(s.rec.Version)}
	}
	next, err := s.NextVersion()
	if err != nil {
		return err
	}
	vr := v.rec
	if vr.Scope != s.rec.Scope || vr.SecretID != s.rec.ID {
		return invalidf("version belongs to another secret")
	}
	if vr.Version != next {
		return &ConflictError{Resource: "value version", Expected: vr.Version, Current: s.rec.CurrentVersion}
	}
	s.rec.CurrentVersion = next
	if !first {
		s.rec.Version++
	}
	s.rec.UpdatedAt = now.UTC()
	return nil
}

// RevokeVersion revokes v irreversibly. It reports whether this call changed
// anything: revoking a revoked version keeps the original time and actor. A
// change bumps the secret's resource version.
func (s *Secret) RevokeVersion(v *SecretVersion, by string, now time.Time) (changed bool, err error) {
	if v.rec.Scope != s.rec.Scope || v.rec.SecretID != s.rec.ID {
		return false, invalidf("version belongs to another secret")
	}
	if by == "" {
		return false, invalidf("a revoking actor is required")
	}
	if v.rec.RevokedAt != nil {
		return false, nil
	}
	// PostgreSQL timestamps have microsecond precision. Normalize here so the
	// first response and a later read-back of the same revocation agree exactly.
	t := now.UTC().Truncate(time.Microsecond)
	v.rec.RevokedAt, v.rec.RevokedBy = &t, by
	s.rec.Version++
	s.rec.UpdatedAt = t
	return true, nil
}

// Delete logically deletes the secret if expected is the current resource
// version. The name stays reserved and the history stays retained. Deleting a
// deleted secret reports changed=false.
func (s *Secret) Delete(expected domain.Version, now time.Time) (changed bool, err error) {
	if expected != s.rec.Version {
		return false, &ConflictError{Resource: "secret", Expected: int64(expected), Current: int64(s.rec.Version)}
	}
	if s.Deleted() {
		return false, nil
	}
	t := now.UTC()
	s.rec.DeletedAt = &t
	s.rec.Version++
	s.rec.UpdatedAt = t
	return true, nil
}
