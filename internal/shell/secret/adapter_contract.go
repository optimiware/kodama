// Package secret declares the envelope-custody and key-provider seams.
//
// A stored secret is one encrypted value in one tenant/project scope. Writes
// replace the current value; reads return metadata only. Envelope encryption
// authenticates the installation identity and the scoped secret identity as
// associated data, so a ciphertext cannot be replayed into another
// installation, tenant, project or secret name.
//
// The one declared suite is AES-256-GCM with a 12-byte content nonce, a 12-byte
// key-wrap nonce, a 32-byte data key and a 48-byte wrapped key. Associated data
// is the length-prefixed installation id, tenant id, project id (zero length
// when tenant-scoped), secret id, secret name, then a big-endian uint32 key
// version. The key adapter wraps only the data key; it never sees plaintext
// secret values.
//
// These declarations are the shape the key adapters implement. No key,
// ciphertext or provider client exists here, and no plaintext is ever logged or
// published.
package secret

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Suite is one authenticated envelope-encryption suite.
type Suite string

// SuiteAES256GCM is the only declared envelope suite.
const SuiteAES256GCM Suite = "aes-256-gcm"

// Suites returns every declared suite in declaration order.
func Suites() []Suite {
	return []Suite{SuiteAES256GCM}
}

// Valid reports whether the suite is the declared suite.
func (suite Suite) Valid() bool {
	return suite == SuiteAES256GCM
}

// Envelope byte lengths and the minimum key version.
const (
	// ContentNonceBytes is the AES-GCM content nonce length.
	ContentNonceBytes = 12
	// WrapNonceBytes is the key-wrap nonce length.
	WrapNonceBytes = 12
	// DataKeyBytes is the wrapped data-key length.
	DataKeyBytes = 32
	// WrappedKeyBytes is the key-adapter wrapped-key length.
	WrappedKeyBytes = 48
	// MinKeyVersion is the first usable key version.
	MinKeyVersion KeyVersion = 1
)

// KeyRef names one installation-configured key. It is an opaque reference, not
// key material.
type KeyRef string

// KeyVersion is the positive version of the referenced key. Associated data
// authenticates it, so an envelope cannot be replayed under another version.
type KeyVersion uint32

// AssociatedData is the exact authenticated scoped identity of one envelope.
// ProjectID is empty for a tenant-scoped secret and encodes as a zero length.
type AssociatedData struct {
	InstallationID string
	TenantID       id.TenantID
	ProjectID      id.ProjectID
	SecretID       SecretID
	Name           id.SecretName
	KeyVersion     KeyVersion
}

// Validate checks that every associated-data field is usable. It does not
// perform cryptography.
func (associated AssociatedData) Validate() error {
	if associated.InstallationID == "" {
		return fault.New(fault.InvalidInput, "envelope installation id is empty")
	}
	if _, err := id.ParseTenantID(string(associated.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "envelope tenant id is invalid", err)
	}
	if associated.ProjectID != "" {
		if _, err := id.ParseProjectID(string(associated.ProjectID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "envelope project id is invalid", err)
		}
	}
	if _, err := ParseSecretID(string(associated.SecretID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "envelope secret id is invalid", err)
	}
	if _, err := id.ParseSecretName(string(associated.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "envelope secret name is invalid", err)
	}
	if associated.KeyVersion < MinKeyVersion {
		return fault.New(fault.InvalidInput, "envelope key version is not positive")
	}
	return nil
}

// Bytes returns the exact declared associated-data encoding: each field is a
// big-endian uint32 length followed by its bytes, and the trailing key version
// is a big-endian uint32. A tenant-scoped project id contributes a zero length.
func (associated AssociatedData) Bytes() ([]byte, error) {
	if err := associated.Validate(); err != nil {
		return nil, err
	}
	fields := [][]byte{
		[]byte(associated.InstallationID),
		[]byte(associated.TenantID),
		[]byte(associated.ProjectID),
		[]byte(associated.SecretID),
		[]byte(associated.Name),
	}
	var buffer bytes.Buffer
	buffer.Grow(4*len(fields) + 4)
	for _, field := range fields {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		buffer.Write(length[:])
		buffer.Write(field)
	}
	var version [4]byte
	binary.BigEndian.PutUint32(version[:], uint32(associated.KeyVersion))
	buffer.Write(version[:])
	return buffer.Bytes(), nil
}

// Envelope is one encrypted secret value plus its wrapped data key. It carries
// no plaintext and no unwrapped key.
type Envelope struct {
	Suite      Suite
	KeyRef     KeyRef
	KeyVersion KeyVersion
	Nonce      []byte
	Ciphertext []byte
	WrappedKey []byte
	WrapNonce  []byte
}

// Validate checks the envelope suite, key reference/version and the exact byte
// lengths. Cryptographic authentication is key-adapter behavior; a structurally
// invalid envelope is refused before any decryption is attempted.
func (envelope Envelope) Validate() error {
	if !envelope.Suite.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown envelope suite %q", envelope.Suite))
	}
	if envelope.KeyRef == "" {
		return fault.New(fault.InvalidInput, "envelope key reference is empty")
	}
	if envelope.KeyVersion < MinKeyVersion {
		return fault.New(fault.InvalidInput, "envelope key version is not positive")
	}
	if len(envelope.Nonce) != ContentNonceBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("envelope content nonce is %d bytes; want %d", len(envelope.Nonce), ContentNonceBytes))
	}
	if len(envelope.Ciphertext) == 0 {
		return fault.New(fault.InvalidInput, "envelope ciphertext is empty")
	}
	if len(envelope.WrappedKey) != WrappedKeyBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("envelope wrapped key is %d bytes; want %d", len(envelope.WrappedKey), WrappedKeyBytes))
	}
	if len(envelope.WrapNonce) != WrapNonceBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("envelope wrap nonce is %d bytes; want %d", len(envelope.WrapNonce), WrapNonceBytes))
	}
	return nil
}

// ValidateFor checks the envelope shape and that its key version matches the
// associated scoped identity. The caller supplies the associated data it
// expects; a mismatched version is refused rather than decrypted.
func (envelope Envelope) ValidateFor(associated AssociatedData) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if err := associated.Validate(); err != nil {
		return err
	}
	if envelope.KeyVersion != associated.KeyVersion {
		return fault.New(fault.InvalidInput, "envelope key version does not match the associated data")
	}
	return nil
}

// ResolveRequest asks for the current declared secret values in one scope.
// Names are declared secret names, never paths.
type ResolveRequest struct {
	Scope tenancy.Scope
	Names []id.SecretName
}

// Validate checks the scope and the declared name set: at least one name, each
// name valid and no duplicates.
func (request ResolveRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if len(request.Names) == 0 {
		return fault.New(fault.InvalidInput, "resolve request declares no secret names")
	}
	seen := make(map[id.SecretName]bool, len(request.Names))
	for _, name := range request.Names {
		if _, err := id.ParseSecretName(string(name)); err != nil {
			return fault.Wrap(fault.InvalidInput, "resolve request secret name is invalid", err)
		}
		if seen[name] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("resolve request declares secret name %q twice", name))
		}
		seen[name] = true
	}
	return nil
}

// Resolved is one current declared secret value. Value is plaintext custody
// material and must never enter a log, event, receipt or task snapshot.
type Resolved struct {
	Name  id.SecretName
	Value string
	Scope tenancy.Scope
}

// Validate checks the name, scope and value bound.
func (resolved Resolved) Validate() error {
	if _, err := id.ParseSecretName(string(resolved.Name)); err != nil {
		return fault.Wrap(fault.InvalidInput, "resolved secret name is invalid", err)
	}
	if err := resolved.Scope.Validate(); err != nil {
		return err
	}
	return DefaultBounds.CheckValue(len(resolved.Value))
}

// Resolver resolves declared secret names at activation. Project values are
// resolved before tenant fallback; a missing, denied or unreadable value fails
// preparation. Requests resolve no secrets.
type Resolver interface {
	Resolve(context.Context, ResolveRequest) ([]Resolved, error)
}

// WrapRequest asks a key adapter to wrap one fresh data key. AssociatedData is
// the envelope associated-data encoding; the adapter authenticates it but never
// receives the secret plaintext.
type WrapRequest struct {
	KeyRef         KeyRef
	KeyVersion     KeyVersion
	Plaintext      []byte
	AssociatedData []byte
}

// Validate checks the key reference/version, the exact data-key length and a
// non-empty associated-data encoding.
func (request WrapRequest) Validate() error {
	if request.KeyRef == "" {
		return fault.New(fault.InvalidInput, "wrap request key reference is empty")
	}
	if request.KeyVersion < MinKeyVersion {
		return fault.New(fault.InvalidInput, "wrap request key version is not positive")
	}
	if len(request.Plaintext) != DataKeyBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("wrap request data key is %d bytes; want %d", len(request.Plaintext), DataKeyBytes))
	}
	if len(request.AssociatedData) == 0 {
		return fault.New(fault.InvalidInput, "wrap request associated data is empty")
	}
	return nil
}

// WrappedKey is one key adapter's wrapped data key.
type WrappedKey struct {
	KeyRef     KeyRef
	KeyVersion KeyVersion
	Wrapped    []byte
	Nonce      []byte
}

// Validate checks the key reference/version and the exact wrapped-key and
// wrap-nonce lengths.
func (wrapped WrappedKey) Validate() error {
	if wrapped.KeyRef == "" {
		return fault.New(fault.InvalidInput, "wrapped key reference is empty")
	}
	if wrapped.KeyVersion < MinKeyVersion {
		return fault.New(fault.InvalidInput, "wrapped key version is not positive")
	}
	if len(wrapped.Wrapped) != WrappedKeyBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("wrapped key is %d bytes; want %d", len(wrapped.Wrapped), WrappedKeyBytes))
	}
	if len(wrapped.Nonce) != WrapNonceBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("wrapped key nonce is %d bytes; want %d", len(wrapped.Nonce), WrapNonceBytes))
	}
	return nil
}

// UnwrapRequest asks a key adapter to unwrap one stored data key. It carries
// the same associated data used at wrap time.
type UnwrapRequest struct {
	KeyRef         KeyRef
	KeyVersion     KeyVersion
	Wrapped        []byte
	Nonce          []byte
	AssociatedData []byte
}

// Validate checks the unwrap request shape.
func (request UnwrapRequest) Validate() error {
	if request.KeyRef == "" {
		return fault.New(fault.InvalidInput, "unwrap request key reference is empty")
	}
	if request.KeyVersion < MinKeyVersion {
		return fault.New(fault.InvalidInput, "unwrap request key version is not positive")
	}
	if len(request.Wrapped) != WrappedKeyBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unwrap request wrapped key is %d bytes; want %d", len(request.Wrapped), WrappedKeyBytes))
	}
	if len(request.Nonce) != WrapNonceBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unwrap request nonce is %d bytes; want %d", len(request.Nonce), WrapNonceBytes))
	}
	if len(request.AssociatedData) == 0 {
		return fault.New(fault.InvalidInput, "unwrap request associated data is empty")
	}
	return nil
}

// KeyProvider wraps and unwraps data keys with one installation key. It never
// receives or returns secret plaintext.
type KeyProvider interface {
	Wrap(context.Context, WrapRequest) (WrappedKey, error)
	Unwrap(context.Context, UnwrapRequest) ([]byte, error)
}

// MountPath returns the confined mount path for one declared secret name. The
// reserved execution-credential name is refused.
func MountPath(name id.SecretName) (string, error) {
	if _, err := id.ParseSecretName(string(name)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "secret name is invalid", err)
	}
	return "/kodama/secrets/" + string(name), nil
}
