package nats

// Test-only NKey helpers. They keep the authorization-callout tests on the
// same encoding implementation as the product without importing a key library
// that the module does not approve as a direct dependency.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

const (
	TestAccountPrefix byte = nkeyPrefixAccount
	TestUserPrefix    byte = nkeyPrefixUser
	TestServerPrefix  byte = nkeyPrefixServer
)

// NewTestKey generates one seed/public pair for the given NKey public prefix.
func NewTestKey(t *testing.T, prefix byte) (string, string) {
	t.Helper()
	seed, err := randomNkeySeed()
	if err != nil {
		t.Fatalf("generate nkey seed: %v", err)
	}
	private := ed25519.NewKeyFromSeed(seed)
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("generated nkey has no public key")
	}
	return encodeNkeySeed(prefix, seed), encodeNkeyPublic(prefix, public)
}

// SignTest signs one message with the account seed.
func SignTest(t *testing.T, seed string, data []byte) []byte {
	t.Helper()
	_, raw, err := decodeNkeySeed(seed)
	if err != nil {
		t.Fatalf("decode signing seed: %v", err)
	}
	return ed25519.Sign(ed25519.NewKeyFromSeed(raw), data)
}

// VerifyTest verifies one signature against a public NKey.
func VerifyTest(t *testing.T, public string, data, signature []byte) bool {
	t.Helper()
	_, key, err := nkeyPublic(public)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	return ed25519.Verify(key, data, signature)
}

// nkeyPublic decodes one public key and returns its prefix and Ed25519 bytes.
func nkeyPublic(encoded string) (byte, ed25519.PublicKey, error) {
	raw := make([]byte, nkeyBase32.DecodedLen(len(encoded)))
	n, err := nkeyBase32.Decode(raw, []byte(encoded))
	if err != nil {
		return 0, nil, fault.Wrap(fault.InvalidInput, "nats key is not base32", err)
	}
	raw = raw[:n]
	if len(raw) != 1+ed25519.PublicKeySize+2 {
		return 0, nil, fault.New(fault.InvalidInput, "nats key is not a public key")
	}
	payload, checksum := raw[:len(raw)-2], binary.LittleEndian.Uint16(raw[len(raw)-2:])
	if crc16(payload) != checksum {
		return 0, nil, fault.New(fault.InvalidInput, "nats key checksum is invalid")
	}
	return payload[0], ed25519.PublicKey(payload[1:]), nil
}

// randomNkeySeed returns 32 random bytes for one key.
func randomNkeySeed() ([]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return seed, nil
}

// encodeNkeySeed encodes 32 seed bytes for the given public prefix.
func encodeNkeySeed(prefix byte, seed []byte) string {
	raw := make([]byte, 0, 2+len(seed)+2)
	raw = append(raw, nkeyPrefixSeed|(prefix>>5), (prefix&31)<<3)
	raw = append(raw, seed...)
	var checksum [2]byte
	binary.LittleEndian.PutUint16(checksum[:], crc16(raw))
	raw = append(raw, checksum[:]...)
	return nkeyBase32.EncodeToString(raw)
}
