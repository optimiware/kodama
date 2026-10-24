// Package keyfile is the development key-wrapping adapter: a local file of raw
// 32-byte key-encryption keys supplied outside the database. It implements
// secrets.KeyWrapper for the standalone development profile only.
//
// The scaffold's master-key file is such a KEK. It wraps tenant-generation scope
// keys and never encrypts a secret value directly. The provider must be
// selected explicitly (Config.Development); there is no plaintext, alternate
// provider or implicit keyfile fallback, a missing or malformed key is an
// error, and no replacement key is ever generated. A production installation
// selects a KMS adapter (T-028) instead; this package makes no claim about
// production qualification.
package keyfile

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/optimiware/kodama/internal/secrets"
)

const (
	// ProviderName is recorded in every KEK reference this adapter issues.
	ProviderName = "keyfile-dev"
	// DefaultKeyID is the identity of the scaffold's key file.
	DefaultKeyID = "master-key"
	// KeySize is the required size of every key file.
	KeySize = 32
	// WrapAlgorithm names AES-256-GCM key wrapping with a random nonce.
	WrapAlgorithm = "aes-256-gcm/wrap-v1"

	nonceSize = 12
)

// Config selects the development keyfile provider.
type Config struct {
	// Development must be true. It is an explicit acknowledgement that this
	// provider is development custody; production configuration cannot select
	// it by omission.
	Development bool
	// KeyID is the key identity recorded in KEK references; empty selects
	// DefaultKeyID.
	KeyID string
	// Current is the version label used for new wraps; it must be in Files.
	Current string
	// Files maps immutable version labels to key file paths: the current key and
	// every retained key still needed to restore older backups or unwrap older
	// wraps.
	Files map[string]string
}

// Provider wraps scope keys under file-based KEKs. It reads a key file for each
// operation and keeps no key material.
type Provider struct {
	keyID   string
	current string
	files   map[string]string
}

// New validates the configuration and that every configured key file is
// present and well-formed. It never creates a key.
func New(cfg Config) (*Provider, error) {
	if !cfg.Development {
		return nil, errors.New("keyfile: the development key provider must be selected explicitly (Config.Development)")
	}
	if cfg.KeyID == "" {
		cfg.KeyID = DefaultKeyID
	}
	if cfg.Current == "" || cfg.Files[cfg.Current] == "" {
		return nil, errors.New("keyfile: the current key version must name a configured key file")
	}
	p := &Provider{keyID: cfg.KeyID, current: cfg.Current, files: make(map[string]string, len(cfg.Files))}
	for v, path := range cfg.Files {
		if v == "" || len(v) > 256 || path == "" {
			return nil, errors.New("keyfile: key versions and paths must be non-empty")
		}
		p.files[v] = path
	}
	for _, v := range p.versions() {
		key, err := p.load(v)
		if err != nil {
			return nil, err
		}
		clear(key)
	}
	return p, nil
}

func (p *Provider) versions() []string {
	vs := make([]string, 0, len(p.files))
	for v := range p.files {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

// CurrentRef is the KEK reference of new wraps.
func (p *Provider) CurrentRef() secrets.KEKRef {
	return secrets.KEKRef{Provider: ProviderName, ID: p.keyID, Version: p.current}
}

func (p *Provider) load(version string) ([]byte, error) {
	path, ok := p.files[version]
	if !ok {
		return nil, &secrets.KeyError{Class: secrets.KeyNotFound, Op: "load", Err: fmt.Errorf("key version %q is not configured", version)}
	}
	key, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, &secrets.KeyError{Class: secrets.KeyNotFound, Op: "load", Err: fmt.Errorf("key file for version %q is missing", version)}
	case errors.Is(err, os.ErrPermission):
		return nil, &secrets.KeyError{Class: secrets.KeyDenied, Op: "load", Err: fmt.Errorf("key file for version %q is not readable", version)}
	case err != nil:
		return nil, &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "load", Err: fmt.Errorf("key file for version %q could not be read", version)}
	}
	if len(key) != KeySize {
		clear(key)
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "load", Err: fmt.Errorf("key file for version %q must be exactly %d bytes", version, KeySize)}
	}
	return key, nil
}

func frame(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// aad binds the wrap to its installation, tenant, generation, key identity and
// the KEK version that produced it.
func aad(wc secrets.WrapContext, ref secrets.KEKRef) []byte {
	b := frame(nil, "kodama/keyfile-wrap/v1")
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

func classifyContext(ctx context.Context, op string) error {
	if err := ctx.Err(); err != nil {
		return &secrets.KeyError{Class: secrets.KeyUnavailable, Op: op, Err: err}
	}
	return nil
}

// Wrap encrypts key under the current KEK.
func (p *Provider) Wrap(ctx context.Context, wc secrets.WrapContext, key []byte) (secrets.WrappedKey, error) {
	if err := classifyContext(ctx, "wrap"); err != nil {
		return secrets.WrappedKey{}, err
	}
	if len(key) != KeySize {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "wrap", Err: errors.New("scope key must be 32 bytes")}
	}
	kek, err := p.load(p.current)
	if err != nil {
		return secrets.WrappedKey{}, err
	}
	defer clear(kek)
	aead, err := gcm(kek)
	if err != nil {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "wrap", Err: err}
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return secrets.WrappedKey{}, &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "wrap", Err: err}
	}
	ref := p.CurrentRef()
	ct := aead.Seal(nonce, nonce, key, aad(wc, ref))
	return secrets.WrappedKey{Bytes: ct, KEK: ref, Algorithm: WrapAlgorithm}, nil
}

// Unwrap decrypts w with the KEK version it names. A version that is not
// configured is KeyNotFound: there is no fallback to another version.
func (p *Provider) Unwrap(ctx context.Context, wc secrets.WrapContext, w secrets.WrappedKey) ([]byte, error) {
	if err := classifyContext(ctx, "unwrap"); err != nil {
		return nil, err
	}
	if w.KEK.Provider != ProviderName || w.KEK.ID != p.keyID || w.Algorithm != WrapAlgorithm {
		return nil, &secrets.KeyError{Class: secrets.KeyNotFound, Op: "unwrap", Err: errors.New("wrap was not produced by this provider and key")}
	}
	kek, err := p.load(w.KEK.Version)
	if err != nil {
		return nil, err
	}
	defer clear(kek)
	aead, err := gcm(kek)
	if err != nil {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: err}
	}
	if len(w.Bytes) < nonceSize+aead.Overhead() {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: errors.New("wrapped key is malformed")}
	}
	key, err := aead.Open(nil, w.Bytes[:nonceSize], w.Bytes[nonceSize:], aad(wc, w.KEK))
	if err != nil {
		return nil, &secrets.KeyError{Class: secrets.KeyCorrupt, Op: "unwrap", Err: errors.New("wrapped key failed authentication")}
	}
	return key, nil
}

var _ secrets.KeyWrapper = (*Provider)(nil)
