package keys_test

// Shared deterministic fixtures for the key-provider and envelope tests. Every
// test uses a constant randomness stream, a temporary keyfile directory or a
// fake KMS client. No test reads a wall clock, opens a socket, or contacts a
// real KMS.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// deterministicRandom fills every read with one byte. A constant stream keeps
// an independently computed AES-GCM fixture valid regardless of the order in
// which the implementation requests its random bytes.
type deterministicRandom byte

func (random deterministicRandom) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte(random)
	}
	return len(buffer), nil
}

// testKEK is the 32-byte key-encryption key every fixture uses.
func testKEK() []byte {
	return bytes.Repeat([]byte{0x2a}, secret.DataKeyBytes)
}

// testDEK is the 32-byte data key a provider-level fixture wraps directly.
func testDEK() []byte {
	return bytes.Repeat([]byte{0x42}, secret.DataKeyBytes)
}

// testAssociated is the golden scoped identity from
// test/contracts/adapters/testdata/envelope.json.
func testAssociated(version secret.KeyVersion) secret.AssociatedData {
	return secret.AssociatedData{
		InstallationID: "install-1",
		TenantID:       id.TenantID("tenant-1"),
		ProjectID:      id.ProjectID("project-1"),
		SecretID:       secret.SecretID("secret-1"),
		Name:           id.SecretName("DATABASE_PASSWORD"),
		KeyVersion:     version,
	}
}

func testAssociatedBytes(t *testing.T, associated secret.AssociatedData) []byte {
	t.Helper()
	encoded, err := associated.Bytes()
	if err != nil {
		t.Fatalf("AssociatedData.Bytes(%+v): %v", associated, err)
	}
	return encoded
}

// gcmSeal independently computes the declared AES-256-GCM output.
func gcmSeal(t *testing.T, key, nonce, plaintext, associatedData []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return aead.Seal(nil, nonce, plaintext, associatedData)
}

func mustProvider(t *testing.T, provider keys.Provider, err error) keys.Provider {
	t.Helper()
	if err != nil {
		t.Fatalf("new key provider: %v", err)
	}
	if provider == nil {
		t.Fatal("new key provider returned nil")
	}
	return provider
}

func newEphemeral(t *testing.T, random io.Reader) keys.Provider {
	t.Helper()
	provider, err := keys.NewEphemeral(random)
	return mustProvider(t, provider, err)
}

func newKeyfile(t *testing.T, keyRef secret.KeyRef, path string, random io.Reader) keys.Provider {
	t.Helper()
	provider, err := keys.NewKeyfile(keyRef, path, random)
	return mustProvider(t, provider, err)
}

func newCipher(t *testing.T, provider keys.Provider, random io.Reader) *keys.Cipher {
	t.Helper()
	cipher, err := keys.NewCipher(provider, random)
	return mustCipher(t, cipher, err)
}

func mustCipher(t *testing.T, cipher *keys.Cipher, err error) *keys.Cipher {
	t.Helper()
	if err != nil {
		t.Fatalf("new envelope cipher: %v", err)
	}
	if cipher == nil {
		t.Fatal("new envelope cipher returned nil")
	}
	return cipher
}

func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %q", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %q is not a fault; want class %q", err.Error(), want)
	}
	if class != want {
		t.Fatalf("fault class = %q; want %q (error: %q)", class, want, err.Error())
	}
}

func requireFaultClassIn(t *testing.T, err error, allowed ...fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want a refusal")
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %q is not a fault; want one of %v", err.Error(), allowed)
	}
	if slices.Contains(allowed, class) {
		return
	}
	t.Fatalf("fault class = %q; want one of %v (error: %q)", class, allowed, err.Error())
}

// requireNoMaterial proves an error never discloses raw or hex-encoded key
// material. The failure message never renders the material itself.
func requireNoMaterial(t *testing.T, err error, material ...[]byte) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want a refusal")
	}
	text := err.Error()
	for _, value := range material {
		if len(value) == 0 {
			continue
		}
		if strings.Contains(text, string(value)) {
			t.Fatal("error disclosed raw key or plaintext material")
		}
		if strings.Contains(text, hex.EncodeToString(value)) {
			t.Fatal("error disclosed hex-encoded key material")
		}
	}
}

// writeKeyfile creates one keyfile with an explicit mode. The explicit chmod
// keeps the mode independent of the process umask.
func writeKeyfile(t *testing.T, path string, content []byte, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod keyfile: %v", err)
	}
	return path
}

// recordingProvider counts provider calls so a test can prove a refusal
// happened before any key operation.
type recordingProvider struct {
	inner keys.Provider

	mu      sync.Mutex
	wraps   int
	unwraps int
}

func (provider *recordingProvider) KeyRef() secret.KeyRef {
	return provider.inner.KeyRef()
}

func (provider *recordingProvider) Wrap(ctx context.Context, request secret.WrapRequest) (secret.WrappedKey, error) {
	provider.mu.Lock()
	provider.wraps++
	provider.mu.Unlock()
	return provider.inner.Wrap(ctx, request)
}

func (provider *recordingProvider) Unwrap(ctx context.Context, request secret.UnwrapRequest) ([]byte, error) {
	provider.mu.Lock()
	provider.unwraps++
	provider.mu.Unlock()
	return provider.inner.Unwrap(ctx, request)
}

func (provider *recordingProvider) counts() (wraps, unwraps int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.wraps, provider.unwraps
}

// fakeKMS is a deterministic in-process KMS client. It records one call per
// encrypted blob, so tests can prove a version was unwrapped exactly once.
type fakeKMS struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	failures map[string]error
	counts   map[string]int
	hook     func(context.Context, secret.KeyRef, []byte) error
}

func newFakeKMS(blobs map[string][]byte) *fakeKMS {
	copied := make(map[string][]byte, len(blobs))
	maps.Copy(copied, blobs)
	return &fakeKMS{blobs: copied, failures: make(map[string]error), counts: make(map[string]int)}
}

// fail makes every decrypt of one blob return err.
func (fake *fakeKMS) fail(blob []byte, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.failures[string(blob)] = err
}

// before installs a hook that runs before the stored result is returned.
func (fake *fakeKMS) before(hook func(context.Context, secret.KeyRef, []byte) error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.hook = hook
}

func (fake *fakeKMS) calls(blob []byte) int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.counts[string(blob)]
}

func (fake *fakeKMS) Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error) {
	key := string(blob)
	fake.mu.Lock()
	fake.counts[key]++
	failure := fake.failures[key]
	hook := fake.hook
	material, configured := fake.blobs[key]
	fake.mu.Unlock()

	if hook != nil {
		if err := hook(ctx, keyRef, blob); err != nil {
			return nil, err
		}
	}
	if failure != nil {
		return nil, failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !configured {
		return nil, fault.New(fault.NotFound, "fake kms has no key for this blob")
	}
	return append([]byte(nil), material...), nil
}

// functionKMS adapts one function to the KMS client seam.
type functionKMS struct {
	decrypt func(context.Context, secret.KeyRef, []byte) ([]byte, error)
}

func (fake *functionKMS) Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error) {
	return fake.decrypt(ctx, keyRef, blob)
}
