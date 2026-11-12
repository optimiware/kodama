//go:build component

package messaging_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, secret.DataKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("random key: %v", err)
	}
	return key
}

func writeKeyfile(t *testing.T, path string, key []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, key, mode); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod keyfile: %v", err)
	}
}

func keyfileProvider(t *testing.T, path string, key []byte) keys.Provider {
	t.Helper()
	provider, err := keys.NewKeyfile(secret.KeyRef("keyfile:test-installation"), path, rand.Reader)
	if err != nil {
		t.Fatalf("new keyfile provider: %v", err)
	}
	return provider
}

func cipherFor(t *testing.T, provider keys.Provider) *keys.Cipher {
	t.Helper()
	cipher, err := keys.NewCipher(provider, rand.Reader)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return cipher
}

func associatedData(tenant, project, secretID, name string) secret.AssociatedData {
	return secret.AssociatedData{
		InstallationID: "installation-1",
		TenantID:       id.TenantID(tenant),
		ProjectID:      id.ProjectID(project),
		SecretID:       secret.SecretID(secretID),
		Name:           id.SecretName(name),
		KeyVersion:     secret.MinKeyVersion,
	}
}

// TestKeyfileLifecycle proves the real keyfile provider accepts only a
// regular, non-symlinked, owner-only, 32-byte file; serves key version 1 only;
// and seals/opens envelopes with associated-data binding and tamper refusal.
func TestKeyfileLifecycle(t *testing.T) {
	support.TrackTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "installation.key")
	key := randomKey(t)
	writeKeyfile(t, path, key, 0o600)

	provider := keyfileProvider(t, path, key)
	if provider.KeyRef() != secret.KeyRef("keyfile:test-installation") {
		t.Fatalf("key ref = %q", provider.KeyRef())
	}
	cipher := cipherFor(t, provider)
	associated := associatedData("tenant-a", "project-1", "secret-1", "API_TOKEN")
	plaintext := []byte("top-secret-value")
	envelope, err := cipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if envelope.Suite != secret.SuiteAES256GCM || envelope.KeyRef != provider.KeyRef() || envelope.KeyVersion != secret.MinKeyVersion {
		t.Fatalf("envelope header = %+v", envelope)
	}
	if len(envelope.Nonce) != secret.ContentNonceBytes || len(envelope.WrappedKey) != secret.WrappedKeyBytes || len(envelope.WrapNonce) != secret.WrapNonceBytes {
		t.Fatalf("envelope lengths are not the declared sizes: %+v", envelope)
	}
	opened, err := cipher.Open(context.Background(), associated, envelope)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q; want %q", opened, plaintext)
	}

	// Permission bits: any group/other access is refused, owner-only works.
	for _, mode := range []os.FileMode{0o644, 0o660, 0o604, 0o601} {
		loose := filepath.Join(dir, "loose.key")
		writeKeyfile(t, loose, key, mode)
		if _, err := keys.NewKeyfile("keyfile:test-installation", loose, rand.Reader); err == nil {
			t.Fatalf("keyfile mode %#o accepted; want refusal", mode)
		} else {
			requireClass(t, err, fault.Forbidden)
		}
	}
	ownerOnly := filepath.Join(dir, "owner-only.key")
	writeKeyfile(t, ownerOnly, key, 0o400)
	if _, err := keys.NewKeyfile("keyfile:test-installation", ownerOnly, rand.Reader); err != nil {
		t.Fatalf("owner-only keyfile refused: %v", err)
	}

	// Symlink, non-regular and wrong-length files are refused.
	symlink := filepath.Join(dir, "symlink.key")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := keys.NewKeyfile("keyfile:test-installation", symlink, rand.Reader); err == nil {
		t.Fatal("symlinked keyfile accepted; want refusal")
	} else {
		requireClass(t, err, fault.Forbidden)
	}
	directoryPath := filepath.Join(dir, "directory.key")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatalf("mkdir keyfile: %v", err)
	}
	if _, err := keys.NewKeyfile("keyfile:test-installation", directoryPath, rand.Reader); err == nil {
		t.Fatal("directory keyfile accepted; want refusal")
	}
	if _, err := keys.NewKeyfile("keyfile:test-installation", "/dev/null", rand.Reader); err == nil {
		t.Fatal("character device keyfile accepted; want refusal")
	}
	for _, length := range []int{secret.DataKeyBytes - 1, secret.DataKeyBytes + 1, 0} {
		wrong := filepath.Join(dir, "wrong.key")
		writeKeyfile(t, wrong, bytes.Repeat([]byte{0x41}, length), 0o600)
		if _, err := keys.NewKeyfile("keyfile:test-installation", wrong, rand.Reader); err == nil {
			t.Fatalf("keyfile of %d bytes accepted; want refusal", length)
		} else {
			requireClass(t, err, fault.InvalidInput)
		}
	}
	if _, err := keys.NewKeyfile("keyfile:test-installation", filepath.Join(dir, "missing.key"), rand.Reader); err == nil {
		t.Fatal("missing keyfile accepted; want refusal")
	} else {
		requireClass(t, err, fault.NotFound)
	}

	// The keyfile provider serves exactly key version 1.
	versionTwo := associated
	versionTwo.KeyVersion = 2
	if _, err := cipher.Seal(context.Background(), versionTwo, plaintext); err == nil {
		t.Fatal("seal under key version 2 accepted; want refusal")
	} else {
		requireClass(t, err, fault.NotFound)
	}
	if _, err := provider.Wrap(context.Background(), secret.WrapRequest{
		KeyRef:         provider.KeyRef(),
		KeyVersion:     2,
		Plaintext:      randomKey(t),
		AssociatedData: []byte("associated"),
	}); err == nil {
		t.Fatal("wrap under key version 2 accepted; want refusal")
	} else {
		requireClass(t, err, fault.NotFound)
	}
	zeroVersion := associated
	zeroVersion.KeyVersion = 0
	if _, err := cipher.Seal(context.Background(), zeroVersion, plaintext); err == nil {
		t.Fatal("seal under key version 0 accepted; want refusal")
	} else {
		requireClass(t, err, fault.InvalidInput)
	}

	// Associated-data binding: tenant A ciphertext cannot open under tenant B,
	// another project, another secret name or another installation.
	openCases := map[string]secret.AssociatedData{
		"tenant":      associatedData("tenant-b", "project-1", "secret-1", "API_TOKEN"),
		"project":     associatedData("tenant-a", "project-2", "secret-1", "API_TOKEN"),
		"tenant-wide": associatedData("tenant-a", "", "secret-1", "API_TOKEN"),
		"secret id":   associatedData("tenant-a", "project-1", "secret-2", "API_TOKEN"),
		"secret name": associatedData("tenant-a", "project-1", "secret-1", "OTHER_TOKEN"),
		"installation": func() secret.AssociatedData {
			value := associatedData("tenant-a", "project-1", "secret-1", "API_TOKEN")
			value.InstallationID = "installation-2"
			return value
		}(),
	}
	for name, mismatched := range openCases {
		t.Run("associated/"+name, func(t *testing.T) {
			if opened, err := cipher.Open(context.Background(), mismatched, envelope); err == nil {
				t.Fatalf("opened under mismatched %s: %q", name, opened)
			} else {
				requireClass(t, err, fault.InvalidInput)
			}
		})
	}

	// Tamper detection: every authenticated byte is checked and no plaintext
	// is returned.
	tamperCases := map[string]func(secret.Envelope) secret.Envelope{
		"ciphertext": func(value secret.Envelope) secret.Envelope {
			value.Ciphertext = append([]byte(nil), value.Ciphertext...)
			value.Ciphertext[0] ^= 0xff
			return value
		},
		"wrapped key": func(value secret.Envelope) secret.Envelope {
			value.WrappedKey = append([]byte(nil), value.WrappedKey...)
			value.WrappedKey[0] ^= 0xff
			return value
		},
		"content nonce": func(value secret.Envelope) secret.Envelope {
			value.Nonce = append([]byte(nil), value.Nonce...)
			value.Nonce[0] ^= 0xff
			return value
		},
		"wrap nonce": func(value secret.Envelope) secret.Envelope {
			value.WrapNonce = append([]byte(nil), value.WrapNonce...)
			value.WrapNonce[0] ^= 0xff
			return value
		},
		"suite": func(value secret.Envelope) secret.Envelope {
			value.Suite = "aes-128-gcm"
			return value
		},
	}
	for name, mutate := range tamperCases {
		t.Run("tamper/"+name, func(t *testing.T) {
			tampered := mutate(envelope)
			opened, err := cipher.Open(context.Background(), associated, tampered)
			if err == nil {
				t.Fatalf("tampered %s envelope opened: %q", name, opened)
			}
			if len(opened) != 0 {
				t.Fatalf("tampered %s envelope returned %d plaintext bytes", name, len(opened))
			}
		})
	}

	// No key material or plaintext in any public error message.
	errorSources := []error{
		func() error {
			_, err := keys.NewKeyfile("keyfile:test-installation", filepath.Join(dir, "loose.key"), rand.Reader)
			return err
		}(),
		func() error {
			_, err := cipher.Open(context.Background(), associatedData("tenant-b", "project-1", "secret-1", "API_TOKEN"), envelope)
			return err
		}(),
		func() error {
			_, err := cipher.Open(context.Background(), associated, tamperCases["ciphertext"](envelope))
			return err
		}(),
	}
	for _, err := range errorSources {
		if err == nil {
			continue
		}
		message := err.Error()
		for _, forbidden := range []string{hex.EncodeToString(key), string(key), string(plaintext), "top-secret"} {
			if forbidden != "" && strings.Contains(message, forbidden) {
				t.Fatalf("error message leaks key material or plaintext: %q", message)
			}
		}
	}
	support.Note(t, "keyfile_lifecycle", "pass")
}

// TestKeyfileRotationByReplace documents and proves the contract's rotation
// behavior: the provider reads the file once at construction and serves key
// version 1 only. Replacing the file content rotates the installation key for
// a newly constructed provider; the old provider keeps the key it read and the
// two cannot open each other's envelopes.
func TestKeyfileRotationByReplace(t *testing.T) {
	support.TrackTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "installation.key")
	oldKey := randomKey(t)
	writeKeyfile(t, path, oldKey, 0o600)
	oldProvider := keyfileProvider(t, path, oldKey)
	oldCipher := cipherFor(t, oldProvider)
	associated := associatedData("tenant-a", "project-1", "secret-1", "API_TOKEN")
	plaintext := []byte("rotated-secret")

	oldEnvelope, err := oldCipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("seal with old key: %v", err)
	}

	newKey := randomKey(t)
	if bytes.Equal(newKey, oldKey) {
		t.Fatal("test keys are identical")
	}
	writeKeyfile(t, path, newKey, 0o600)
	newProvider := keyfileProvider(t, path, newKey)
	newCipher := cipherFor(t, newProvider)
	newEnvelope, err := newCipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("seal with new key: %v", err)
	}

	if _, err := oldCipher.Open(context.Background(), associated, oldEnvelope); err != nil {
		t.Fatalf("old provider lost the key it already read: %v", err)
	}
	if _, err := newCipher.Open(context.Background(), associated, newEnvelope); err != nil {
		t.Fatalf("new provider cannot open its own envelope: %v", err)
	}
	if opened, err := newCipher.Open(context.Background(), associated, oldEnvelope); err == nil {
		t.Fatalf("new key opened an envelope sealed under the replaced key: %q", opened)
	} else {
		requireClass(t, err, fault.InvalidInput)
	}
	if opened, err := oldCipher.Open(context.Background(), associated, newEnvelope); err == nil {
		t.Fatalf("old key opened an envelope sealed under the replacement key: %q", opened)
	} else {
		requireClass(t, err, fault.InvalidInput)
	}
	// The old provider is unaffected by the file replacement: it never
	// re-reads the path.
	if _, err := oldCipher.Seal(context.Background(), associated, plaintext); err != nil {
		t.Fatalf("old provider cannot seal after file replacement: %v", err)
	}
	support.Note(t, "keyfile_rotation", "new provider over replaced file; old provider keeps its read key")
}

// TestEphemeralProviderNondeterminism proves each ephemeral provider gets a
// fresh key and reference, so equal plaintext produces distinct ciphertext and
// providers cannot open each other's envelopes.
func TestEphemeralProviderNondeterminism(t *testing.T) {
	support.TrackTest(t)
	first, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("first ephemeral provider: %v", err)
	}
	second, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("second ephemeral provider: %v", err)
	}
	if first.KeyRef() == second.KeyRef() {
		t.Fatalf("ephemeral key references collide: %q", first.KeyRef())
	}
	if !strings.HasPrefix(string(first.KeyRef()), "ephemeral:") {
		t.Fatalf("ephemeral key ref = %q; want ephemeral: prefix", first.KeyRef())
	}
	associated := associatedData("tenant-a", "project-1", "secret-1", "API_TOKEN")
	plaintext := []byte("same-plaintext")
	firstEnvelope, err := cipherFor(t, first).Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("first seal: %v", err)
	}
	secondEnvelope, err := cipherFor(t, second).Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if bytes.Equal(firstEnvelope.Ciphertext, secondEnvelope.Ciphertext) {
		t.Fatal("equal plaintext produced identical ciphertext across ephemeral providers")
	}
	if bytes.Equal(firstEnvelope.Nonce, secondEnvelope.Nonce) || bytes.Equal(firstEnvelope.WrappedKey, secondEnvelope.WrappedKey) {
		t.Fatal("ephemeral providers reused a nonce or wrapped key")
	}
	if opened, err := cipherFor(t, second).Open(context.Background(), associated, firstEnvelope); err == nil {
		t.Fatalf("second provider opened the first provider's envelope: %q", opened)
	} else {
		requireClass(t, err, fault.NotFound)
	}
	support.Note(t, "ephemeral_nondeterministic", "true")
}
