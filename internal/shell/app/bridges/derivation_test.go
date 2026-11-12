// Behavior tests for the tenant derivation-key resolver bridge.
//
// The bridge reconstructs the AES-256-GCM envelope from the structured
// token.TenantKey and opens it through the keys seam. It never logs or renders
// key material and maps every failure to a dependency fault.
package bridges_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/token"
)

const derivationPlaintext = "0123456789abcdef0123456789abcdef"

func derivationTemplate() secret.AssociatedData {
	return secret.AssociatedData{
		InstallationID: "install-1",
		SecretID:       secret.SecretID("tenant-key"),
		Name:           id.SecretName("TENANT_DERIVATION_KEY"),
		KeyVersion:     secret.MinKeyVersion,
	}
}

func TestDerivationKeyResolverOpensSealedTenantKey(t *testing.T) {
	provider, err := keys.NewEphemeral(bytes.NewReader(bytes.Repeat([]byte{0x11}, 256)))
	if err != nil {
		t.Fatalf("NewEphemeral: %v", err)
	}
	cipher, err := keys.NewCipher(provider, bytes.NewReader(bytes.Repeat([]byte{0x22}, 256)))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	associated := derivationTemplate()
	associated.TenantID = id.TenantID("tenant-a")
	envelope, err := cipher.Seal(context.Background(), associated, []byte(derivationPlaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	tenantKey := token.TenantKey{
		TenantID:   associated.TenantID,
		Version:    1,
		Ciphertext: append(append([]byte(nil), envelope.Nonce...), envelope.Ciphertext...),
		WrappedKey: append([]byte(nil), envelope.WrappedKey...),
		WrapNonce:  append([]byte(nil), envelope.WrapNonce...),
		KeyRef:     string(envelope.KeyRef),
		KeyVersion: uint64(envelope.KeyVersion),
	}
	if err := tenantKey.Validate(); err != nil {
		t.Fatalf("TenantKey.Validate: %v", err)
	}

	resolver, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: cipher, Associated: derivationTemplate()})
	if err != nil {
		t.Fatalf("NewDerivationKeyResolver: %v", err)
	}
	plaintext, err := resolver.ResolveDerivationKey(context.Background(), tenantKey)
	if err != nil {
		t.Fatalf("ResolveDerivationKey: %v", err)
	}
	if string(plaintext) != derivationPlaintext {
		t.Fatalf("plaintext = %q; want the sealed derivation key", plaintext)
	}
}

func TestDerivationKeyResolverMapsTamperWithoutRenderingKeyMaterial(t *testing.T) {
	provider, err := keys.NewEphemeral(bytes.NewReader(bytes.Repeat([]byte{0x33}, 256)))
	if err != nil {
		t.Fatalf("NewEphemeral: %v", err)
	}
	cipher, err := keys.NewCipher(provider, bytes.NewReader(bytes.Repeat([]byte{0x44}, 256)))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	associated := derivationTemplate()
	associated.TenantID = id.TenantID("tenant-a")
	envelope, err := cipher.Seal(context.Background(), associated, []byte(derivationPlaintext))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ciphertext := append(append([]byte(nil), envelope.Nonce...), envelope.Ciphertext...)
	ciphertext[len(ciphertext)-1] ^= 0xff
	tenantKey := token.TenantKey{
		TenantID:   associated.TenantID,
		Version:    1,
		Ciphertext: ciphertext,
		WrappedKey: append([]byte(nil), envelope.WrappedKey...),
		WrapNonce:  append([]byte(nil), envelope.WrapNonce...),
		KeyRef:     string(envelope.KeyRef),
		KeyVersion: uint64(envelope.KeyVersion),
	}
	resolver, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: cipher, Associated: derivationTemplate()})
	if err != nil {
		t.Fatalf("NewDerivationKeyResolver: %v", err)
	}
	plaintext, err := resolver.ResolveDerivationKey(context.Background(), tenantKey)
	if plaintext != nil {
		t.Fatalf("tampered key returned plaintext %q", plaintext)
	}
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if strings.Contains(err.Error(), derivationPlaintext) {
		t.Fatalf("error renders derivation key material: %v", err)
	}
}

func TestDerivationKeyResolverUsesConfiguredAssociatedIdentity(t *testing.T) {
	cipher := &recordingEnvelopeCipher{plaintext: []byte(derivationPlaintext)}
	resolver, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: cipher, Associated: derivationTemplate()})
	if err != nil {
		t.Fatalf("NewDerivationKeyResolver: %v", err)
	}
	tenantKey := token.TenantKey{
		TenantID:   id.TenantID("tenant-b"),
		Version:    4,
		Ciphertext: append(bytes.Repeat([]byte{0x01}, secret.ContentNonceBytes), []byte("ciphertext")...),
		WrappedKey: bytes.Repeat([]byte{0x02}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{0x03}, secret.WrapNonceBytes),
		KeyRef:     "key-ref",
		KeyVersion: 7,
	}
	if _, err := resolver.ResolveDerivationKey(context.Background(), tenantKey); err != nil {
		t.Fatalf("ResolveDerivationKey: %v", err)
	}
	if cipher.associated.TenantID != tenantKey.TenantID || cipher.associated.ProjectID != "" {
		t.Fatalf("associated identity = %+v; want the tenant key tenant", cipher.associated)
	}
	if uint64(cipher.associated.KeyVersion) != tenantKey.KeyVersion || cipher.associated.InstallationID != "install-1" {
		t.Fatalf("associated version/installation = %+v", cipher.associated)
	}
	if cipher.associated.SecretID != secret.SecretID("tenant-key") || cipher.associated.Name != id.SecretName("TENANT_DERIVATION_KEY") {
		t.Fatalf("associated configured identity = %+v", cipher.associated)
	}
	if !bytes.Equal(cipher.envelope.Nonce, tenantKey.Ciphertext[:secret.ContentNonceBytes]) {
		t.Fatal("envelope nonce was not reconstructed from the tenant-key ciphertext prefix")
	}
	if !bytes.Equal(cipher.envelope.Ciphertext, tenantKey.Ciphertext[secret.ContentNonceBytes:]) {
		t.Fatal("envelope ciphertext was not reconstructed from the tenant-key ciphertext suffix")
	}
	if cipher.envelope.Suite != secret.SuiteAES256GCM || cipher.envelope.KeyRef != secret.KeyRef(tenantKey.KeyRef) {
		t.Fatalf("envelope suite/key reference = %+v", cipher.envelope)
	}
}

func TestDerivationKeyResolverRefusesInvalidInputAndDependencies(t *testing.T) {
	validCipher := &recordingEnvelopeCipher{plaintext: []byte(derivationPlaintext)}
	if _, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: nil, Associated: derivationTemplate()}); err == nil {
		t.Fatal("NewDerivationKeyResolver accepted a nil cipher")
	}
	badTemplate := derivationTemplate()
	badTemplate.SecretID = ""
	if _, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: validCipher, Associated: badTemplate}); err == nil {
		t.Fatal("NewDerivationKeyResolver accepted an invalid associated template")
	}

	resolver, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: validCipher, Associated: derivationTemplate()})
	if err != nil {
		t.Fatalf("NewDerivationKeyResolver: %v", err)
	}
	if _, err := resolver.ResolveDerivationKey(context.Background(), token.TenantKey{}); err == nil {
		t.Fatal("ResolveDerivationKey accepted an empty tenant key")
	}
	short := token.TenantKey{
		TenantID:   "tenant-a",
		Version:    1,
		Ciphertext: []byte("short"),
		WrappedKey: bytes.Repeat([]byte{0x01}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{0x02}, secret.WrapNonceBytes),
		KeyRef:     "key-ref",
		KeyVersion: 1,
	}
	if _, err := resolver.ResolveDerivationKey(context.Background(), short); err == nil {
		t.Fatal("ResolveDerivationKey accepted a ciphertext shorter than the content nonce")
	}
}

func TestDerivationKeyResolverPropagatesCancellationAndDependencyFailure(t *testing.T) {
	cipher := &recordingEnvelopeCipher{plaintext: []byte(derivationPlaintext)}
	resolver, err := bridges.NewDerivationKeyResolver(bridges.DerivationKeyConfig{Cipher: cipher, Associated: derivationTemplate()})
	if err != nil {
		t.Fatalf("NewDerivationKeyResolver: %v", err)
	}
	tenantKey := token.TenantKey{
		TenantID:   "tenant-a",
		Version:    1,
		Ciphertext: append(bytes.Repeat([]byte{0x01}, secret.ContentNonceBytes), []byte("ciphertext")...),
		WrappedKey: bytes.Repeat([]byte{0x02}, secret.WrappedKeyBytes),
		WrapNonce:  bytes.Repeat([]byte{0x03}, secret.WrapNonceBytes),
		KeyRef:     "key-ref",
		KeyVersion: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plaintext, err := resolver.ResolveDerivationKey(ctx, tenantKey)
	if plaintext != nil {
		t.Fatalf("canceled resolution returned plaintext %q", plaintext)
	}
	requireFaultClass(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}

	sentinel := errors.New("kms unavailable")
	cipher.err = sentinel
	_, err = resolver.ResolveDerivationKey(context.Background(), tenantKey)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the dependency failure: %v", err)
	}
}

type recordingEnvelopeCipher struct {
	associated secret.AssociatedData
	envelope   secret.Envelope
	plaintext  []byte
	err        error
}

func (cipher *recordingEnvelopeCipher) Seal(context.Context, secret.AssociatedData, []byte) (secret.Envelope, error) {
	return secret.Envelope{}, errors.New("not used")
}

func (cipher *recordingEnvelopeCipher) Open(_ context.Context, associated secret.AssociatedData, envelope secret.Envelope) ([]byte, error) {
	cipher.associated = associated
	cipher.envelope = envelope
	if cipher.err != nil {
		return nil, cipher.err
	}
	return append([]byte(nil), cipher.plaintext...), nil
}
