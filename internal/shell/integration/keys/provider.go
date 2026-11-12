// Package keys implements the secret-custody adapters: ephemeral and keyfile
// key providers plus the AWS/GCP/Scaleway KMS-backed providers, and the
// AES-256-GCM envelope cipher over them.
//
// Every provider wraps a fresh 32-byte data key locally with AES-256-GCM under
// a 32-byte key-encryption key. A cloud provider obtains its key-encryption key
// by asking the injected KMS client to decrypt one installation-configured
// opaque blob, once per key version, and never forwards secret plaintext. No
// key material, plaintext or KMS response body enters a public error message.
package keys

import (
	"context"
	"crypto/aes"
	stdcipher "crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// Provider is one key provider plus the key reference it serves.
type Provider interface {
	secret.KeyProvider
	KeyRef() secret.KeyRef
}

// keyProvider is the shared local key-wrap implementation. The kekFor function
// resolves the key-encryption key for one configured key version; it is the
// only behavior that differs between the ephemeral, keyfile and cloud
// providers.
type keyProvider struct {
	keyRef secret.KeyRef
	random io.Reader

	randomMu sync.Mutex
	kekFor   func(context.Context, secret.KeyVersion) ([]byte, error)
}

// KeyRef returns the installation key reference this provider serves.
func (provider *keyProvider) KeyRef() secret.KeyRef {
	return provider.keyRef
}

// Wrap authenticates the request and seals one data key under the provider's
// key-encryption key. The caller's data key is never retained.
func (provider *keyProvider) Wrap(ctx context.Context, request secret.WrapRequest) (secret.WrappedKey, error) {
	if err := request.Validate(); err != nil {
		return secret.WrappedKey{}, err
	}
	if request.KeyRef != provider.keyRef {
		return secret.WrappedKey{}, unknownKeyReference()
	}
	if err := ctx.Err(); err != nil {
		return secret.WrappedKey{}, err
	}
	kek, err := provider.kekFor(ctx, request.KeyVersion)
	if err != nil {
		return secret.WrappedKey{}, err
	}
	aead, err := newAEAD(kek)
	if err != nil {
		return secret.WrappedKey{}, err
	}
	nonce, err := provider.readRandom(secret.WrapNonceBytes)
	if err != nil {
		return secret.WrappedKey{}, err
	}
	wrapped := aead.Seal(nil, nonce, request.Plaintext, request.AssociatedData)
	return secret.WrappedKey{
		KeyRef:     provider.keyRef,
		KeyVersion: request.KeyVersion,
		Wrapped:    wrapped,
		Nonce:      nonce,
	}, nil
}

// Unwrap authenticates the request and opens one stored data key. An
// authentication failure, wrong key material or wrong key length is
// InvalidInput and never returns plaintext.
func (provider *keyProvider) Unwrap(ctx context.Context, request secret.UnwrapRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if request.KeyRef != provider.keyRef {
		return nil, unknownKeyReference()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kek, err := provider.kekFor(ctx, request.KeyVersion)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, request.Nonce, request.Wrapped, request.AssociatedData)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "wrapped data key authentication failed", err)
	}
	if len(plaintext) != secret.DataKeyBytes {
		clear(plaintext)
		return nil, fault.New(fault.InvalidInput, "unwrapped data key has the wrong length")
	}
	return plaintext, nil
}

// readRandom draws one nonce from the provider's randomness source. A nil
// source was already replaced with crypto/rand by the constructor.
func (provider *keyProvider) readRandom(size int) ([]byte, error) {
	buffer := make([]byte, size)
	provider.randomMu.Lock()
	_, err := io.ReadFull(provider.random, buffer)
	provider.randomMu.Unlock()
	if err != nil {
		clear(buffer)
		return nil, fault.Wrap(fault.InternalFailure, "read key-wrap randomness", err)
	}
	return buffer, nil
}

// singleVersionKey serves exactly one stored key version. Every other version
// is an unknown key version.
func singleVersionKey(key []byte, version secret.KeyVersion) func(context.Context, secret.KeyVersion) ([]byte, error) {
	stored := append([]byte(nil), key...)
	return func(ctx context.Context, requested secret.KeyVersion) ([]byte, error) {
		if requested != version {
			return nil, unknownKeyVersion()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return stored, nil
	}
}

// newAEAD builds the declared AES-256-GCM primitive. A key of any other length
// is refused before any cryptographic operation.
func newAEAD(key []byte) (stdcipher.AEAD, error) {
	if len(key) != secret.DataKeyBytes {
		return nil, fault.New(fault.InvalidInput, "encryption key has the wrong length")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "create AES cipher", err)
	}
	aead, err := stdcipher.NewGCM(block)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "create AES-GCM cipher", err)
	}
	return aead, nil
}

func unknownKeyReference() error {
	return fault.New(fault.NotFound, "unknown key reference")
}

func unknownKeyVersion() error {
	return fault.New(fault.NotFound, "unknown key version")
}

// NewEphemeral keeps one random 32-byte key in memory for this process. The key
// reference is random per instance, so a reconstructed process serving the same
// identity refuses to open earlier envelopes instead of silently decrypting
// them with a fresh key.
func NewEphemeral(random io.Reader) (Provider, error) {
	if random == nil {
		random = rand.Reader
	}
	key := make([]byte, secret.DataKeyBytes)
	if _, err := io.ReadFull(random, key); err != nil {
		clear(key)
		return nil, fault.Wrap(fault.InternalFailure, "generate ephemeral key", err)
	}
	reference := make([]byte, 16)
	if _, err := io.ReadFull(random, reference); err != nil {
		clear(key)
		return nil, fault.Wrap(fault.InternalFailure, "generate ephemeral key reference", err)
	}
	provider := &keyProvider{
		keyRef: secret.KeyRef("ephemeral:" + hex.EncodeToString(reference)),
		random: random,
		kekFor: singleVersionKey(key, secret.MinKeyVersion),
	}
	return provider, nil
}

// NewKeyfile reads one 32-byte key from a regular, non-symlink file that is not
// group or world accessible. The file is read only; the provider never rewrites
// or removes it.
func NewKeyfile(keyRef secret.KeyRef, path string, random io.Reader) (Provider, error) {
	if keyRef == "" {
		return nil, fault.New(fault.InvalidInput, "keyfile key reference is empty")
	}
	if path == "" {
		return nil, fault.New(fault.InvalidInput, "keyfile path is empty")
	}
	if random == nil {
		random = rand.Reader
	}
	key, err := readKeyfile(path)
	if err != nil {
		return nil, err
	}
	provider := &keyProvider{
		keyRef: keyRef,
		random: random,
		kekFor: singleVersionKey(key, secret.MinKeyVersion),
	}
	return provider, nil
}

// readKeyfile refuses a missing, symlinked, non-regular, group/world-readable
// or wrongly sized file. The returned bytes are the only key material; error
// messages carry no part of them.
func readKeyfile(path string) ([]byte, error) {
	inspected, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fault.New(fault.NotFound, "keyfile does not exist")
		}
		return nil, fault.Wrap(fault.InvalidInput, "inspect keyfile", err)
	}
	if inspected.Mode()&os.ModeSymlink != 0 {
		return nil, fault.New(fault.Forbidden, "keyfile must not be a symbolic link")
	}
	if !inspected.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput, "keyfile is not a regular file")
	}
	if inspected.Mode().Perm()&0o077 != 0 {
		return nil, fault.New(fault.Forbidden, "keyfile is accessible by group or others")
	}

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fault.New(fault.NotFound, "keyfile does not exist")
		}
		return nil, fault.Wrap(fault.InvalidInput, "open keyfile", err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "inspect open keyfile", err)
	}
	if !os.SameFile(inspected, opened) {
		return nil, fault.New(fault.Forbidden, "keyfile changed while it was opened")
	}

	content, err := io.ReadAll(io.LimitReader(file, int64(secret.DataKeyBytes)+1))
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "read keyfile", err)
	}
	if len(content) != secret.DataKeyBytes {
		clear(content)
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("keyfile is %d bytes; want %d", len(content), secret.DataKeyBytes))
	}
	return content, nil
}

// keyRefPrefix reports whether one cloud key reference belongs to the expected
// provider family.
func keyRefPrefix(reference secret.KeyRef, prefix string) bool {
	text := string(reference)
	return strings.HasPrefix(text, prefix) && len(text) > len(prefix)
}
