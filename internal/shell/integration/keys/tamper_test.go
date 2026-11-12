package keys_test

// Associated-data binding, ciphertext authentication, envelope swapping and
// error hygiene. Every refusal must return no plaintext and no key material.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

type sealedFixture struct {
	provider   *recordingProvider
	cipher     *keys.Cipher
	associated secret.AssociatedData
	envelope   secret.Envelope
	plaintext  []byte
}

func newSealedFixture(t *testing.T, associated secret.AssociatedData, plaintext []byte) sealedFixture {
	t.Helper()
	path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
	provider := &recordingProvider{inner: newKeyfile(t, "keyfile:tamper", path, deterministicRandom(0x11))}
	cipher := newCipher(t, provider, deterministicRandom(0x42))
	envelope, err := cipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("Seal fixture: %v", err)
	}
	return sealedFixture{provider: provider, cipher: cipher, associated: associated, envelope: envelope, plaintext: plaintext}
}

// TestOpenRefusesAssociatedDataMismatch mutates every authenticated scope
// field. An envelope sealed for one identity must not open for another.
func TestOpenRefusesAssociatedDataMismatch(t *testing.T) {
	fixture := newSealedFixture(t, testAssociated(1), []byte("scoped-value"))

	otherInstallation := testAssociated(1)
	otherInstallation.InstallationID = "install-2"
	otherTenant := testAssociated(1)
	otherTenant.TenantID = id.TenantID("tenant-2")
	otherProject := testAssociated(1)
	otherProject.ProjectID = id.ProjectID("project-2")
	otherSecret := testAssociated(1)
	otherSecret.SecretID = secret.SecretID("secret-2")
	otherName := testAssociated(1)
	otherName.Name = id.SecretName("OTHER_SECRET")
	tenantScoped := testAssociated(1)
	tenantScoped.ProjectID = ""
	otherVersion := testAssociated(2)

	cases := []struct {
		name       string
		associated secret.AssociatedData
	}{
		{name: "installation", associated: otherInstallation},
		{name: "tenant", associated: otherTenant},
		{name: "project", associated: otherProject},
		{name: "secret id", associated: otherSecret},
		{name: "secret name", associated: otherName},
		{name: "tenant scoped project", associated: tenantScoped},
		{name: "key version", associated: otherVersion},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plaintext, err := fixture.cipher.Open(context.Background(), testCase.associated, fixture.envelope)
			requireFaultClass(t, err, fault.InvalidInput)
			if len(plaintext) != 0 {
				t.Fatalf("mismatched associated data returned %d plaintext bytes", len(plaintext))
			}
			requireNoMaterial(t, err, fixture.plaintext, testKEK())
		})
	}
}

// TestOpenRefusesCiphertextTamper flips one authenticated byte at a time.
func TestOpenRefusesCiphertextTamper(t *testing.T) {
	fixture := newSealedFixture(t, testAssociated(1), []byte("authenticated-value"))

	mutations := []struct {
		name   string
		mutate func(*secret.Envelope)
	}{
		{name: "content nonce", mutate: func(envelope *secret.Envelope) { envelope.Nonce[0] ^= 0xff }},
		{name: "ciphertext head", mutate: func(envelope *secret.Envelope) { envelope.Ciphertext[0] ^= 0xff }},
		{name: "ciphertext tag", mutate: func(envelope *secret.Envelope) { envelope.Ciphertext[len(envelope.Ciphertext)-1] ^= 0xff }},
		{name: "wrapped key", mutate: func(envelope *secret.Envelope) { envelope.WrappedKey[0] ^= 0xff }},
		{name: "wrap nonce", mutate: func(envelope *secret.Envelope) { envelope.WrapNonce[0] ^= 0xff }},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := fixture.envelope
			candidate.Nonce = append([]byte(nil), fixture.envelope.Nonce...)
			candidate.Ciphertext = append([]byte(nil), fixture.envelope.Ciphertext...)
			candidate.WrappedKey = append([]byte(nil), fixture.envelope.WrappedKey...)
			candidate.WrapNonce = append([]byte(nil), fixture.envelope.WrapNonce...)
			mutation.mutate(&candidate)

			plaintext, err := fixture.cipher.Open(context.Background(), fixture.associated, candidate)
			requireFaultClass(t, err, fault.InvalidInput)
			if len(plaintext) != 0 {
				t.Fatalf("tampered envelope returned %d plaintext bytes", len(plaintext))
			}
			requireNoMaterial(t, err, fixture.plaintext, testKEK(), bytes.Repeat([]byte{0x42}, secret.DataKeyBytes))
		})
	}
}

// TestOpenRefusesEnvelopeSwap seals two different secrets and scopes and proves
// each envelope is bound to its own identity.
func TestOpenRefusesEnvelopeSwap(t *testing.T) {
	first := newSealedFixture(t, testAssociated(1), []byte("first-secret"))
	otherSecret := testAssociated(1)
	otherSecret.SecretID = secret.SecretID("secret-2")
	second := newSealedFixture(t, otherSecret, []byte("second-secret"))

	secondScope := testAssociated(1)
	secondScope.ProjectID = id.ProjectID("project-2")
	third := newSealedFixture(t, secondScope, []byte("other-project-secret"))

	cases := []struct {
		name       string
		cipher     *keys.Cipher
		associated secret.AssociatedData
		envelope   secret.Envelope
	}{
		{name: "other secret", cipher: first.cipher, associated: first.associated, envelope: second.envelope},
		{name: "other project", cipher: first.cipher, associated: first.associated, envelope: third.envelope},
		{name: "swapped scope", cipher: third.cipher, associated: secondScope, envelope: first.envelope},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plaintext, err := testCase.cipher.Open(context.Background(), testCase.associated, testCase.envelope)
			requireFaultClass(t, err, fault.InvalidInput)
			if len(plaintext) != 0 {
				t.Fatalf("swapped envelope returned %d plaintext bytes", len(plaintext))
			}
		})
	}
}

// TestOpenRefusesUnknownKeyReference treats an envelope naming another key as
// an unknown key, not as tampered material.
func TestOpenRefusesUnknownKeyReference(t *testing.T) {
	fixture := newSealedFixture(t, testAssociated(1), []byte("reference-bound-value"))

	candidate := fixture.envelope
	candidate.KeyRef = secret.KeyRef("keyfile:somewhere-else")
	plaintext, err := fixture.cipher.Open(context.Background(), fixture.associated, candidate)
	requireFaultClass(t, err, fault.NotFound)
	if len(plaintext) != 0 {
		t.Fatalf("unknown key reference returned %d plaintext bytes", len(plaintext))
	}
}

// TestOpenErrorDoesNotDiscloseMaterial checks every refusal path renders no key
// or plaintext material in its public message.
func TestOpenErrorDoesNotDiscloseMaterial(t *testing.T) {
	plaintext := []byte("SUPER_SECRET_PLAINTEXT_MARKER")
	fixture := newSealedFixture(t, testAssociated(1), plaintext)
	dataKey := bytes.Repeat([]byte{0x42}, secret.DataKeyBytes)

	mutated := fixture.envelope
	mutated.Ciphertext = append([]byte(nil), fixture.envelope.Ciphertext...)
	mutated.Ciphertext[0] ^= 0xff

	refused := []struct {
		name     string
		envelope secret.Envelope
	}{
		{name: "ciphertext", envelope: mutated},
		{name: "zero", envelope: secret.Envelope{}},
	}

	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fixture.cipher.Open(context.Background(), fixture.associated, testCase.envelope)
			requireNoMaterial(t, err, plaintext, testKEK(), dataKey)
		})
	}
}
