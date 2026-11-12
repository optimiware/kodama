package keys_test

// Envelope primitives: the declared AES-256-GCM envelope shape, the
// stored-value bound, and the golden associated-data vector.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

func TestSealOpenRoundTrip(t *testing.T) {
	provider := newEphemeral(t, deterministicRandom(0x11))
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	tenantScoped := testAssociated(1)
	tenantScoped.ProjectID = ""

	cases := []struct {
		name        string
		associated  secret.AssociatedData
		plaintext   []byte
		cipherBytes int
	}{
		{name: "empty", associated: testAssociated(1), plaintext: nil, cipherBytes: 16},
		{name: "binary", associated: testAssociated(1), plaintext: []byte{0x00, 0xff, 0x01, 0xfe, 0x80, 0x7f}, cipherBytes: 22},
		{name: "tenant scoped", associated: tenantScoped, plaintext: []byte("tenant-wide-value"), cipherBytes: 16 + len("tenant-wide-value")},
		{name: "max bound", associated: testAssociated(1), plaintext: bytes.Repeat([]byte("k"), secret.DefaultBounds.MaxValueBytes), cipherBytes: 16 + secret.DefaultBounds.MaxValueBytes},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, err := cipher.Seal(context.Background(), testCase.associated, testCase.plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if err := envelope.Validate(); err != nil {
				t.Fatalf("sealed envelope refused by Validate: %v", err)
			}
			if err := envelope.ValidateFor(testCase.associated); err != nil {
				t.Fatalf("sealed envelope refused by ValidateFor: %v", err)
			}
			if envelope.Suite != secret.SuiteAES256GCM {
				t.Fatalf("suite = %q; want %q", envelope.Suite, secret.SuiteAES256GCM)
			}
			if envelope.KeyRef != provider.KeyRef() {
				t.Fatalf("key reference = %q; want %q", envelope.KeyRef, provider.KeyRef())
			}
			if envelope.KeyVersion != testCase.associated.KeyVersion {
				t.Fatalf("key version = %d; want %d", envelope.KeyVersion, testCase.associated.KeyVersion)
			}
			if len(envelope.Nonce) != secret.ContentNonceBytes {
				t.Fatalf("content nonce = %d bytes; want %d", len(envelope.Nonce), secret.ContentNonceBytes)
			}
			if len(envelope.WrapNonce) != secret.WrapNonceBytes {
				t.Fatalf("wrap nonce = %d bytes; want %d", len(envelope.WrapNonce), secret.WrapNonceBytes)
			}
			if len(envelope.WrappedKey) != secret.WrappedKeyBytes {
				t.Fatalf("wrapped key = %d bytes; want %d", len(envelope.WrappedKey), secret.WrappedKeyBytes)
			}
			if len(envelope.Ciphertext) != testCase.cipherBytes {
				t.Fatalf("ciphertext = %d bytes; want %d", len(envelope.Ciphertext), testCase.cipherBytes)
			}

			opened, err := cipher.Open(context.Background(), testCase.associated, envelope)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if !bytes.Equal(opened, testCase.plaintext) {
				t.Fatalf("opened plaintext = %d bytes; want %d", len(opened), len(testCase.plaintext))
			}
			if len(testCase.plaintext) == secret.DefaultBounds.MaxValueBytes {
				if bytes.Contains(envelope.Ciphertext, testCase.plaintext) {
					t.Fatal("ciphertext contains the plaintext")
				}
			}
		})
	}
}

func TestNewCipherRefusesNilProvider(t *testing.T) {
	cipher, err := keys.NewCipher(nil, deterministicRandom(0x42))
	requireFaultClass(t, err, fault.InvalidInput)
	if cipher != nil {
		t.Fatal("nil provider returned a cipher")
	}
}

func TestSealRefusesOversizePlaintext(t *testing.T) {
	provider := &recordingProvider{inner: newEphemeral(t, deterministicRandom(0x11))}
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	envelope, err := cipher.Seal(context.Background(), testAssociated(1), bytes.Repeat([]byte("x"), secret.DefaultBounds.MaxValueBytes+1))
	requireFaultClass(t, err, fault.InvalidInput)
	if !reflect.DeepEqual(envelope, secret.Envelope{}) {
		t.Fatalf("refused Seal returned a partial envelope: %+v", envelope)
	}
	if wraps, _ := provider.counts(); wraps != 0 {
		t.Fatalf("oversize plaintext reached the key provider %d times; want 0", wraps)
	}
}

func TestSealRefusesInvalidAssociatedData(t *testing.T) {
	provider := &recordingProvider{inner: newEphemeral(t, deterministicRandom(0x11))}
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	zeroVersion := testAssociated(0)
	emptyInstallation := testAssociated(1)
	emptyInstallation.InstallationID = ""
	invalidTenant := testAssociated(1)
	invalidTenant.TenantID = "not a tenant"
	invalidName := testAssociated(1)
	invalidName.Name = "db_password"

	cases := []struct {
		name       string
		associated secret.AssociatedData
	}{
		{name: "zero version", associated: zeroVersion},
		{name: "empty installation", associated: emptyInstallation},
		{name: "invalid tenant", associated: invalidTenant},
		{name: "invalid secret name", associated: invalidName},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, err := cipher.Seal(context.Background(), testCase.associated, []byte("value"))
			requireFaultClass(t, err, fault.InvalidInput)
			if !reflect.DeepEqual(envelope, secret.Envelope{}) {
				t.Fatalf("refused Seal returned a partial envelope: %+v", envelope)
			}
		})
	}
	if wraps, _ := provider.counts(); wraps != 0 {
		t.Fatalf("invalid associated data reached the key provider %d times; want 0", wraps)
	}
}

// TestSealMatchesIndependentAESGCMFixture recomputes both GCM operations with
// crypto/aes instead of trusting the implementation. The keyfile key and the
// constant randomness streams make every input byte known: the cipher draws the
// data key and content nonce from its reader, and the provider draws the wrap
// nonce from its own reader.
func TestSealMatchesIndependentAESGCMFixture(t *testing.T) {
	path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
	provider := newKeyfile(t, "keyfile:fixture", path, deterministicRandom(0x11))
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	associated := testAssociated(1)
	associatedData := testAssociatedBytes(t, associated)
	plaintext := []byte("independently computed fixture plaintext")

	envelope, err := cipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	dataKey := bytes.Repeat([]byte{0x42}, secret.DataKeyBytes)
	contentNonce := bytes.Repeat([]byte{0x42}, secret.ContentNonceBytes)
	wrapNonce := bytes.Repeat([]byte{0x11}, secret.WrapNonceBytes)
	wantCiphertext := gcmSeal(t, dataKey, contentNonce, plaintext, associatedData)
	wantWrappedKey := gcmSeal(t, testKEK(), wrapNonce, dataKey, associatedData)

	if !bytes.Equal(envelope.Nonce, contentNonce) {
		t.Fatalf("content nonce = %x; want the deterministic stream %x", envelope.Nonce, contentNonce)
	}
	if !bytes.Equal(envelope.WrapNonce, wrapNonce) {
		t.Fatalf("wrap nonce = %x; want the deterministic stream %x", envelope.WrapNonce, wrapNonce)
	}
	if !bytes.Equal(envelope.Ciphertext, wantCiphertext) {
		t.Fatalf("ciphertext = %x; want independently computed %x", envelope.Ciphertext, wantCiphertext)
	}
	if !bytes.Equal(envelope.WrappedKey, wantWrappedKey) {
		t.Fatalf("wrapped key = %x; want independently computed %x", envelope.WrappedKey, wantWrappedKey)
	}

	opened, err := cipher.Open(context.Background(), associated, envelope)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q; want %q", opened, plaintext)
	}
}

type envelopeDocument struct {
	Suite      string `json:"suite"`
	KeyRef     string `json:"key_ref"`
	KeyVersion uint32 `json:"key_version"`
	Associated struct {
		InstallationID string `json:"installation_id"`
		TenantID       string `json:"tenant_id"`
		ProjectID      string `json:"project_id"`
		SecretID       string `json:"secret_id"`
		Name           string `json:"name"`
		KeyVersion     uint32 `json:"key_version"`
	} `json:"associated_data"`
	AssociatedDataHex string `json:"associated_data_hex"`
	Envelope          struct {
		Suite         string `json:"suite"`
		KeyRef        string `json:"key_ref"`
		KeyVersion    uint32 `json:"key_version"`
		NonceHex      string `json:"nonce_hex"`
		CiphertextHex string `json:"ciphertext_hex"`
		WrappedKeyHex string `json:"wrapped_key_hex"`
		WrapNonceHex  string `json:"wrap_nonce_hex"`
	} `json:"envelope"`
	Tamper []struct {
		Field string `json:"field"`
		Valid bool   `json:"valid"`
	} `json:"tamper"`
}

func readGoldenEnvelope(t *testing.T) envelopeDocument {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "test", "contracts", "adapters", "testdata", "envelope.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden envelope vector: %v", err)
	}
	var document envelopeDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("decode golden envelope vector: %v", err)
	}
	return document
}

func goldenAssociated(document envelopeDocument) secret.AssociatedData {
	return secret.AssociatedData{
		InstallationID: document.Associated.InstallationID,
		TenantID:       id.TenantID(document.Associated.TenantID),
		ProjectID:      id.ProjectID(document.Associated.ProjectID),
		SecretID:       secret.SecretID(document.Associated.SecretID),
		Name:           id.SecretName(document.Associated.Name),
		KeyVersion:     secret.KeyVersion(document.Associated.KeyVersion),
	}
}

func goldenEnvelope(t *testing.T, document envelopeDocument) secret.Envelope {
	t.Helper()
	return secret.Envelope{
		Suite:      secret.Suite(document.Envelope.Suite),
		KeyRef:     secret.KeyRef(document.Envelope.KeyRef),
		KeyVersion: secret.KeyVersion(document.Envelope.KeyVersion),
		Nonce:      mustHex(t, document.Envelope.NonceHex),
		Ciphertext: mustHex(t, document.Envelope.CiphertextHex),
		WrappedKey: mustHex(t, document.Envelope.WrappedKeyHex),
		WrapNonce:  mustHex(t, document.Envelope.WrapNonceHex),
	}
}

func mustHex(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode hex %q: %v", encoded, err)
	}
	return decoded
}

// TestGoldenAssociatedDataRoundTrip uses the golden associated-data bytes
// unchanged and proves they authenticate a real envelope. The golden key
// version is 7, so a versioned cloud provider serves it.
func TestGoldenAssociatedDataRoundTrip(t *testing.T) {
	document := readGoldenEnvelope(t)
	associated := goldenAssociated(document)
	associatedData := testAssociatedBytes(t, associated)
	if encoded := hex.EncodeToString(associatedData); encoded != document.AssociatedDataHex {
		t.Fatalf("associated data = %s; want golden %s", encoded, document.AssociatedDataHex)
	}

	blob := []byte("golden-encrypted-kek")
	client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
	provider, err := keys.NewAWS(client, keys.CloudConfig{
		KeyRef:   secret.KeyRef(keys.AWSKeyRefPrefix + "alias/kodama"),
		Current:  associated.KeyVersion,
		Versions: map[secret.KeyVersion][]byte{associated.KeyVersion: blob},
	}, deterministicRandom(0x11))
	if err != nil {
		t.Fatalf("new AWS provider: %v", err)
	}
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	plaintext := []byte("golden associated-data value")
	envelope, err := cipher.Seal(context.Background(), associated, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := envelope.ValidateFor(associated); err != nil {
		t.Fatalf("golden associated data refused: %v", err)
	}
	opened, err := cipher.Open(context.Background(), associated, envelope)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q; want %q", opened, plaintext)
	}
}

// TestGoldenTamperVectorsAreRefusedByOpen drives Open with the golden tamper
// list. Every listed mutation is structural, so it must be refused before any
// key operation.
func TestGoldenTamperVectorsAreRefusedByOpen(t *testing.T) {
	document := readGoldenEnvelope(t)
	envelope := goldenEnvelope(t, document)

	path := writeKeyfile(t, filepath.Join(t.TempDir(), "kek"), testKEK(), 0o600)
	provider := &recordingProvider{inner: newKeyfile(t, secret.KeyRef(document.KeyRef), path, deterministicRandom(0x11))}
	cipher := newCipher(t, provider, deterministicRandom(0x42))

	for _, tamper := range document.Tamper {
		t.Run(tamper.Field, func(t *testing.T) {
			if tamper.Valid {
				t.Fatalf("tamper vector %q is marked valid", tamper.Field)
			}
			candidate := envelope
			candidate.Nonce = append([]byte(nil), envelope.Nonce...)
			candidate.Ciphertext = append([]byte(nil), envelope.Ciphertext...)
			candidate.WrappedKey = append([]byte(nil), envelope.WrappedKey...)
			candidate.WrapNonce = append([]byte(nil), envelope.WrapNonce...)
			switch tamper.Field {
			case "ciphertext":
				candidate.Ciphertext = nil
			case "nonce":
				candidate.Nonce = []byte{1, 2, 3}
			case "wrap_nonce":
				candidate.WrapNonce = nil
			case "wrapped_key":
				candidate.WrappedKey = make([]byte, 32)
			case "key_version":
				candidate.KeyVersion = 0
			case "suite":
				candidate.Suite = secret.Suite("aes-128-gcm")
			case "key_ref":
				candidate.KeyRef = ""
			default:
				t.Fatalf("unknown tamper field %q", tamper.Field)
			}

			plaintext, err := cipher.Open(context.Background(), goldenAssociated(document), candidate)
			requireFaultClass(t, err, fault.InvalidInput)
			if len(plaintext) != 0 {
				t.Fatalf("refused Open returned %d plaintext bytes", len(plaintext))
			}
		})
	}
	if wraps, unwraps := provider.counts(); wraps != 0 || unwraps != 0 {
		t.Fatalf("structural refusals reached the key provider (wraps=%d unwraps=%d); want 0", wraps, unwraps)
	}
}

func TestOpenRefusesStructurallyInvalidEnvelopes(t *testing.T) {
	provider := &recordingProvider{inner: newEphemeral(t, deterministicRandom(0x11))}
	cipher := newCipher(t, provider, deterministicRandom(0x42))
	associated := testAssociated(1)

	valid := secret.Envelope{
		Suite:      secret.SuiteAES256GCM,
		KeyRef:     provider.KeyRef(),
		KeyVersion: 1,
		Nonce:      make([]byte, secret.ContentNonceBytes),
		Ciphertext: make([]byte, 32),
		WrappedKey: make([]byte, secret.WrappedKeyBytes),
		WrapNonce:  make([]byte, secret.WrapNonceBytes),
	}

	cases := []struct {
		name   string
		mutate func(*secret.Envelope)
	}{
		{name: "zero envelope", mutate: func(envelope *secret.Envelope) { *envelope = secret.Envelope{} }},
		{name: "unknown suite", mutate: func(envelope *secret.Envelope) { envelope.Suite = secret.Suite("aes-128-gcm") }},
		{name: "empty key reference", mutate: func(envelope *secret.Envelope) { envelope.KeyRef = "" }},
		{name: "zero key version", mutate: func(envelope *secret.Envelope) { envelope.KeyVersion = 0 }},
		{name: "short content nonce", mutate: func(envelope *secret.Envelope) { envelope.Nonce = envelope.Nonce[:11] }},
		{name: "empty ciphertext", mutate: func(envelope *secret.Envelope) { envelope.Ciphertext = nil }},
		{name: "short wrapped key", mutate: func(envelope *secret.Envelope) { envelope.WrappedKey = envelope.WrappedKey[:32] }},
		{name: "short wrap nonce", mutate: func(envelope *secret.Envelope) { envelope.WrapNonce = envelope.WrapNonce[:11] }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := valid
			candidate.Nonce = append([]byte(nil), valid.Nonce...)
			candidate.Ciphertext = append([]byte(nil), valid.Ciphertext...)
			candidate.WrappedKey = append([]byte(nil), valid.WrappedKey...)
			candidate.WrapNonce = append([]byte(nil), valid.WrapNonce...)
			testCase.mutate(&candidate)

			plaintext, err := cipher.Open(context.Background(), associated, candidate)
			requireFaultClass(t, err, fault.InvalidInput)
			if len(plaintext) != 0 {
				t.Fatalf("refused Open returned %d plaintext bytes", len(plaintext))
			}
		})
	}
	if _, unwraps := provider.counts(); unwraps != 0 {
		t.Fatalf("structural refusals reached the key provider %d times; want 0", unwraps)
	}
}

// TestCipherUsesProductionRandomnessWhenReaderIsNil proves the nil reader is
// the secure production source, not a zero or fixed nonce.
func TestCipherUsesProductionRandomnessWhenReaderIsNil(t *testing.T) {
	provider := newEphemeral(t, nil)
	cipher := newCipher(t, provider, nil)

	first, err := cipher.Seal(context.Background(), testAssociated(1), []byte("first"))
	if err != nil {
		t.Fatalf("first Seal: %v", err)
	}
	second, err := cipher.Seal(context.Background(), testAssociated(1), []byte("first"))
	if err != nil {
		t.Fatalf("second Seal: %v", err)
	}
	if bytes.Equal(first.Nonce, second.Nonce) {
		t.Fatal("two seals reused the content nonce")
	}
	if bytes.Equal(first.WrapNonce, second.WrapNonce) {
		t.Fatal("two seals reused the wrap nonce")
	}
	if bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("two seals produced the same ciphertext")
	}
	if bytes.Equal(first.WrappedKey, second.WrappedKey) {
		t.Fatal("two seals wrapped the same data key")
	}
}
