// Package secret seals one revision of a Kodama secret with envelope
// encryption.
//
// One row of the simplified secrets table stores the value ciphertext, the KEK
// key reference and a JSON envelope. Seal generates a fresh 32-byte data key for
// every revision, wraps it with the installation KEK provider and seals the
// plaintext with [envelope] under that data key. The sealed value ciphertext is
// returned separately because it is the encrypted_value column; the envelope
// carries only the wrapped data key and the nonce/format/algorithm metadata.
//
// The ownership context is authenticated twice: it is the KEK wrap binding and
// it is the associated data of the sealed value. Changing the installation,
// tenant, project, secret id, name or version therefore makes Open fail.
//
// The package is pure computation: no database, clock or process-wide provider.
package secret

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

const (
	// envelopeVersion is the only supported JSON envelope framing.
	envelopeVersion = 1
	// maxEnvelopeBytes bounds the decoded persisted envelope.
	maxEnvelopeBytes = 8 << 10
	// maxWrappedBytes bounds the KEK-wrapped data key carried in the envelope.
	maxWrappedBytes = 4 << 10
	// maxFieldBytes bounds every context and provider metadata field, matching
	// the limit enforced by the envelope codec.
	maxFieldBytes = 1024

	wrapBindingPrefix = "kodama/kernel-secret/wrap-binding/v1"
	kekBindingPrefix  = "kodama/kernel-secret/kek-binding/v1"
	projectPrefix     = "project:"
	tenantProject     = "tenant"
)

// ErrKeyUnavailable reports that the KEK provider could not wrap or unwrap the
// data key. It is retryable only insofar as the provider outage is.
var ErrKeyUnavailable = errors.New("secret: key provider unavailable")

// Context is the ownership and revision binding of one sealed secret.
type Context struct {
	InstallationID string
	TenantID       string
	ProjectID      string // empty for a tenant-scoped secret
	SecretID       string
	Name           string
	Version        int64 // >= 1
}

func (c Context) validate() error {
	for _, f := range []string{c.InstallationID, c.TenantID, c.SecretID, c.Name} {
		if f == "" || len(f) > maxFieldBytes {
			return fmt.Errorf("%w: context field empty or too long", envelope.ErrMalformed)
		}
	}
	if c.ProjectID != "" && len(c.ProjectID) > maxFieldBytes-len(projectPrefix) {
		return fmt.Errorf("%w: context field too long", envelope.ErrMalformed)
	}
	if c.Version < 1 {
		return fmt.Errorf("%w: version must be positive", envelope.ErrMalformed)
	}
	return nil
}

// projectBinding distinguishes a tenant-scoped secret from every project-scoped
// one without leaving the project field empty, which the value codec rejects.
func projectBinding(projectID string) string {
	if projectID == "" {
		return tenantProject
	}
	return projectPrefix + projectID
}

// envelopeContext adapts the ownership context to the value codec. The scope key
// identity is derived from the wrapped data key metadata, so a swapped envelope
// from another KEK also fails the associated-data check.
func envelopeContext(c Context, w secretkeys.Wrapped) envelope.Context {
	return envelope.Context{
		InstallationID: c.InstallationID,
		TenantID:       c.TenantID,
		ProjectID:      projectBinding(c.ProjectID),
		SecretID:       c.SecretID,
		Name:           c.Name,
		Version:        c.Version,
		ScopeKeyID:     kekBinding(w),
		KeyGeneration:  1,
	}
}

// framed hashes a domain prefix followed by length-prefixed fields. Hashing
// keeps the binding and the envelope scope key identity within every provider's
// associated-data limit while remaining collision resistant.
func framed(prefix string, fields []string) [sha256.Size]byte {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, uint32(len(prefix)))
	b = append(b, prefix...)
	for _, f := range fields {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return sha256.Sum256(b)
}

// wrapBinding is the KEK wrap binding: every ownership field and the revision.
func wrapBinding(c Context) []byte {
	sum := framed(wrapBindingPrefix, []string{
		c.InstallationID, c.TenantID, c.ProjectID, c.SecretID, c.Name, versionField(c.Version),
	})
	return sum[:]
}

// kekBinding is the value-codec scope key identity: the KEK provider, format
// and key reference that wrapped this revision's data key.
func kekBinding(w secretkeys.Wrapped) string {
	sum := framed(kekBindingPrefix, []string{w.Provider, w.Format, w.KeyRef})
	return "kek:" + hex.EncodeToString(sum[:])
}

func versionField(v int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return string(b[:])
}

func validateWrapped(w secretkeys.Wrapped) error {
	for _, f := range []string{w.Provider, w.KeyRef, w.Format} {
		if f == "" || len(f) > maxFieldBytes {
			return fmt.Errorf("%w: wrapped key metadata", envelope.ErrMalformed)
		}
	}
	if len(w.Ciphertext) == 0 || len(w.Ciphertext) > maxWrappedBytes {
		return fmt.Errorf("%w: wrapped key size", envelope.ErrMalformed)
	}
	return nil
}

func unavailable() error {
	return fmt.Errorf("%w: wrap or unwrap failed", ErrKeyUnavailable)
}

// Seal returns the value ciphertext, the JSON envelope, and the KEK key
// reference. ciphertext is the sealed value column value and is not repeated in
// the envelope.
func Seal(ctx context.Context, p secretkeys.Provider, c Context, plaintext []byte) (ciphertext, envelopeJSON []byte, keyID string, err error) {
	if p == nil {
		return nil, nil, "", unavailable()
	}
	if err := c.validate(); err != nil {
		return nil, nil, "", err
	}
	if len(plaintext) > envelope.MaxPlaintextBytes {
		return nil, nil, "", fmt.Errorf("%w: value exceeds %d bytes", envelope.ErrMalformed, envelope.MaxPlaintextBytes)
	}

	dataKey, err := envelope.NewScopeKey()
	if err != nil {
		return nil, nil, "", fmt.Errorf("secret: data key: %w", err)
	}
	defer clear(dataKey)

	wrapped, err := p.Wrap(ctx, dataKey, wrapBinding(c))
	if err != nil {
		return nil, nil, "", unavailable()
	}
	if err := validateWrapped(wrapped); err != nil {
		return nil, nil, "", err
	}

	sealed, err := envelope.Seal(dataKey, envelopeContext(c, wrapped), plaintext)
	if err != nil {
		return nil, nil, "", err
	}

	doc := envelopeDoc{
		Version: envelopeVersion,
		Wrapped: wrappedKeyJSON{
			Provider:   wrapped.Provider,
			KeyRef:     wrapped.KeyRef,
			Format:     wrapped.Format,
			Ciphertext: base64.StdEncoding.EncodeToString(wrapped.Ciphertext),
		},
		Sealed: sealedJSON{
			FormatVersion: sealed.FormatVersion,
			Algorithm:     sealed.Algorithm,
			Nonce:         base64.StdEncoding.EncodeToString(sealed.Nonce),
		},
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, "", fmt.Errorf("secret: encode envelope: %w", err)
	}
	if len(encoded) > maxEnvelopeBytes {
		return nil, nil, "", fmt.Errorf("%w: envelope exceeds %d bytes", envelope.ErrMalformed, maxEnvelopeBytes)
	}
	return sealed.Ciphertext, encoded, wrapped.KeyRef, nil
}

// Open authenticates and decrypts one revision. The returned plaintext belongs
// to the caller, which should clear it when done.
func Open(ctx context.Context, p secretkeys.Provider, c Context, ciphertext, envelopeJSON []byte) ([]byte, error) {
	if p == nil {
		return nil, unavailable()
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	doc, err := decodeEnvelope(envelopeJSON)
	if err != nil {
		return nil, err
	}

	wrappedCiphertext, err := decodeBase64("wrapped key", doc.Wrapped.Ciphertext)
	if err != nil {
		return nil, err
	}
	nonce, err := decodeBase64("nonce", doc.Sealed.Nonce)
	if err != nil {
		return nil, err
	}

	wrapped := secretkeys.Wrapped{
		Provider:   doc.Wrapped.Provider,
		KeyRef:     doc.Wrapped.KeyRef,
		Format:     doc.Wrapped.Format,
		Ciphertext: wrappedCiphertext,
	}
	if err := validateWrapped(wrapped); err != nil {
		return nil, err
	}

	dataKey, err := p.Unwrap(ctx, wrapped, wrapBinding(c))
	if err != nil {
		if errors.Is(err, secretkeys.ErrCiphertext) || errors.Is(err, secretkeys.ErrInvalid) {
			return nil, fmt.Errorf("%w: wrapped key rejected", envelope.ErrAuthentication)
		}
		return nil, unavailable()
	}
	defer clear(dataKey)

	sealed := envelope.Envelope{
		FormatVersion: doc.Sealed.FormatVersion,
		Algorithm:     doc.Sealed.Algorithm,
		Nonce:         nonce,
		Ciphertext:    ciphertext,
	}
	return envelope.Open(dataKey, envelopeContext(c, wrapped), sealed)
}

// Persisted envelope shape. It is versioned and decoded strictly. The sealed
// value ciphertext is the encrypted_value column and is deliberately absent.
type envelopeDoc struct {
	Version int            `json:"v"`
	Wrapped wrappedKeyJSON `json:"wrapped"`
	Sealed  sealedJSON     `json:"sealed"`
}

type wrappedKeyJSON struct {
	Provider   string `json:"provider"`
	KeyRef     string `json:"keyRef"`
	Format     string `json:"format"`
	Ciphertext string `json:"ciphertext"`
}

type sealedJSON struct {
	FormatVersion int    `json:"formatVersion"`
	Algorithm     string `json:"algorithm"`
	Nonce         string `json:"nonce"`
}

func decodeEnvelope(data []byte) (envelopeDoc, error) {
	var doc envelopeDoc
	if len(data) == 0 || len(data) > maxEnvelopeBytes {
		return doc, fmt.Errorf("%w: envelope size", envelope.ErrMalformed)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("%w: envelope json", envelope.ErrMalformed)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return doc, fmt.Errorf("%w: trailing envelope data", envelope.ErrMalformed)
	}
	if doc.Version == 0 {
		return doc, fmt.Errorf("%w: missing envelope version", envelope.ErrMalformed)
	}
	if doc.Version != envelopeVersion {
		return doc, fmt.Errorf("%w: envelope version %d", envelope.ErrUnsupported, doc.Version)
	}
	return doc, nil
}

func decodeBase64(label, value string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%w: envelope %s encoding", envelope.ErrMalformed, label)
	}
	return b, nil
}
