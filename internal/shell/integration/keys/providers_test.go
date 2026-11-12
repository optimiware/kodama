package keys_test

// Ephemeral and keyfile providers. Both are deterministic and use only
// temporary files or in-memory material.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestEphemeralRoundTripAndReconstruction(t *testing.T) {
	first := newEphemeral(t, deterministicRandom(0x11))
	second := newEphemeral(t, deterministicRandom(0x22))

	if first.KeyRef() == "" {
		t.Fatal("ephemeral key reference is empty")
	}
	if first.KeyRef() == second.KeyRef() {
		t.Fatalf("two ephemeral providers share key reference %q; a reconstructed process would keep decrypting", first.KeyRef())
	}

	firstCipher := newCipher(t, first, deterministicRandom(0x42))
	plaintext := []byte("ephemeral-value")
	envelope, err := firstCipher.Seal(context.Background(), testAssociated(1), plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := firstCipher.Open(context.Background(), testAssociated(1), envelope)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q; want %q", opened, plaintext)
	}

	secondCipher := newCipher(t, second, deterministicRandom(0x42))
	lost, err := secondCipher.Open(context.Background(), testAssociated(1), envelope)
	requireFaultClass(t, err, fault.NotFound)
	if len(lost) != 0 {
		t.Fatalf("reconstructed ephemeral provider returned %d plaintext bytes", len(lost))
	}
	requireNoMaterial(t, err, plaintext, testKEK())
}

func TestEphemeralRefusesUnknownKeyVersion(t *testing.T) {
	provider := newEphemeral(t, deterministicRandom(0x11))
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	envelope, err := cipher.Seal(context.Background(), testAssociated(2), []byte("versioned"))
	requireFaultClass(t, err, fault.NotFound)
	if len(envelope.Ciphertext) != 0 {
		t.Fatalf("refused Seal returned a partial envelope: %+v", envelope)
	}
}

func TestKeyfileRoundTripAcrossReconstruction(t *testing.T) {
	path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
	ref := secret.KeyRef("keyfile:kodama")

	first := newKeyfile(t, ref, path, deterministicRandom(0x11))
	second := newKeyfile(t, ref, path, deterministicRandom(0x22))
	if first.KeyRef() != ref || second.KeyRef() != ref {
		t.Fatalf("keyfile key reference = %q/%q; want %q", first.KeyRef(), second.KeyRef(), ref)
	}

	firstCipher := newCipher(t, first, deterministicRandom(0x42))
	envelope, err := firstCipher.Seal(context.Background(), testAssociated(1), []byte("keyfile-value"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	secondCipher := newCipher(t, second, deterministicRandom(0x43))
	opened, err := secondCipher.Open(context.Background(), testAssociated(1), envelope)
	if err != nil {
		t.Fatalf("Open after reconstruction: %v", err)
	}
	if string(opened) != "keyfile-value" {
		t.Fatalf("opened plaintext = %q; want %q", opened, "keyfile-value")
	}
}

func TestKeyfileConfigurationRefusals(t *testing.T) {
	ref := secret.KeyRef("keyfile:kodama")

	t.Run("empty key reference", func(t *testing.T) {
		path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
		_, err := keys.NewKeyfile("", path, deterministicRandom(0x11))
		requireFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("empty path", func(t *testing.T) {
		_, err := keys.NewKeyfile(ref, "", deterministicRandom(0x11))
		requireFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := keys.NewKeyfile(ref, filepath.Join(t.TempDir(), "missing"), deterministicRandom(0x11))
		requireFaultClass(t, err, fault.NotFound)
	})

	for _, size := range []int{0, secret.DataKeyBytes - 1, secret.DataKeyBytes + 1} {
		t.Run("wrong length", func(t *testing.T) {
			content := bytes.Repeat([]byte{0x7e}, size)
			path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), content, 0o600)
			_, err := keys.NewKeyfile(ref, path, deterministicRandom(0x11))
			requireFaultClass(t, err, fault.InvalidInput)
			requireNoMaterial(t, err, content)
		})
	}

	for _, mode := range []os.FileMode{0o644, 0o666} {
		t.Run("insecure mode", func(t *testing.T) {
			path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), mode)
			_, err := keys.NewKeyfile(ref, path, deterministicRandom(0x11))
			requireFaultClassIn(t, err, fault.InvalidInput, fault.Forbidden)
			requireNoMaterial(t, err, testKEK())
		})
	}

	t.Run("directory", func(t *testing.T) {
		_, err := keys.NewKeyfile(ref, t.TempDir(), deterministicRandom(0x11))
		requireFaultClassIn(t, err, fault.InvalidInput, fault.Forbidden)
	})

	t.Run("symlink", func(t *testing.T) {
		directory := t.TempDir()
		target := writeKeyfile(t, filepath.Join(directory, "kek"), testKEK(), 0o600)
		link := filepath.Join(directory, "kek-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		_, err := keys.NewKeyfile(ref, link, deterministicRandom(0x11))
		requireFaultClassIn(t, err, fault.InvalidInput, fault.Forbidden)
	})
}

func TestKeyfileDoesNotCleanFilesItDidNotCreate(t *testing.T) {
	directory := t.TempDir()
	path := writeKeyfile(t, filepath.Join(directory, "kek"), testKEK(), 0o600)

	provider := newKeyfile(t, "keyfile:kodama", path, deterministicRandom(0x11))
	cipher := newCipher(t, provider, deterministicRandom(0x42))
	if _, err := cipher.Seal(context.Background(), testAssociated(1), []byte("value")); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	wrongLength := writeKeyfile(t, filepath.Join(directory, "short"), []byte{0x01, 0x02}, 0o600)
	if _, err := keys.NewKeyfile("keyfile:kodama", wrongLength, deterministicRandom(0x11)); err == nil {
		t.Fatal("short keyfile accepted")
	}

	for _, candidate := range []string{path, wrongLength} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("provider removed or changed a file it did not create: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode = %o; want 600", info.Mode().Perm())
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read keyfile after use: %v", err)
	}
	if !bytes.Equal(content, testKEK()) {
		t.Fatal("provider rewrote the keyfile it read")
	}
}

func TestProvidersRefuseInvalidRequests(t *testing.T) {
	path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
	keyfile := newKeyfile(t, "keyfile:kodama", path, deterministicRandom(0x11))
	ephemeral := newEphemeral(t, deterministicRandom(0x11))

	for name, provider := range map[string]keys.Provider{"keyfile": keyfile, "ephemeral": ephemeral} {
		t.Run(name, func(t *testing.T) {
			associatedData := testAssociatedBytes(t, testAssociated(1))
			valid := secret.WrapRequest{
				KeyRef:         provider.KeyRef(),
				KeyVersion:     1,
				Plaintext:      testDEK(),
				AssociatedData: associatedData,
			}

			invalidWraps := []struct {
				name    string
				request secret.WrapRequest
			}{
				{name: "empty key reference", request: func() secret.WrapRequest { candidate := valid; candidate.KeyRef = ""; return candidate }()},
				{name: "zero key version", request: func() secret.WrapRequest { candidate := valid; candidate.KeyVersion = 0; return candidate }()},
				{name: "short data key", request: func() secret.WrapRequest {
					candidate := valid
					candidate.Plaintext = candidate.Plaintext[:31]
					return candidate
				}()},
				{name: "empty associated data", request: func() secret.WrapRequest { candidate := valid; candidate.AssociatedData = nil; return candidate }()},
			}
			for _, testCase := range invalidWraps {
				t.Run(testCase.name, func(t *testing.T) {
					if _, err := provider.Wrap(context.Background(), testCase.request); err == nil {
						t.Fatal("invalid wrap request accepted")
					} else {
						requireFaultClass(t, err, fault.InvalidInput)
					}
				})
			}

			wrapped, err := provider.Wrap(context.Background(), valid)
			if err != nil {
				t.Fatalf("valid Wrap: %v", err)
			}
			validUnwrap := secret.UnwrapRequest{
				KeyRef:         provider.KeyRef(),
				KeyVersion:     1,
				Wrapped:        wrapped.Wrapped,
				Nonce:          wrapped.Nonce,
				AssociatedData: associatedData,
			}
			invalidUnwraps := []struct {
				name    string
				request secret.UnwrapRequest
			}{
				{name: "empty key reference", request: func() secret.UnwrapRequest { candidate := validUnwrap; candidate.KeyRef = ""; return candidate }()},
				{name: "zero key version", request: func() secret.UnwrapRequest { candidate := validUnwrap; candidate.KeyVersion = 0; return candidate }()},
				{name: "short wrapped key", request: func() secret.UnwrapRequest {
					candidate := validUnwrap
					candidate.Wrapped = candidate.Wrapped[:47]
					return candidate
				}()},
				{name: "short nonce", request: func() secret.UnwrapRequest {
					candidate := validUnwrap
					candidate.Nonce = candidate.Nonce[:11]
					return candidate
				}()},
				{name: "empty associated data", request: func() secret.UnwrapRequest {
					candidate := validUnwrap
					candidate.AssociatedData = nil
					return candidate
				}()},
			}
			for _, testCase := range invalidUnwraps {
				t.Run(testCase.name, func(t *testing.T) {
					if _, err := provider.Unwrap(context.Background(), testCase.request); err == nil {
						t.Fatal("invalid unwrap request accepted")
					} else {
						requireFaultClass(t, err, fault.InvalidInput)
					}
				})
			}
		})
	}
}
