package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// knownSeedRaw and its encoded pair come from the reference NATS nkeys
// implementation (nkeys.FromRawSeed(nkeys.PrefixByteAccount, ...)). Pinning a
// fixed vector proves the copied codec the tool uses is byte-compatible with
// the Shell callout and the broker, not just self-consistent.
const (
	knownSeedRaw = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	knownSeed    = "SAAAAAICAMCAKBQHBAEQUCYMBUHA6EARCIJRIFIWC4MBSGQ3DQOR4HYMHI"
	knownPublic  = "AAB2CB576PHBBPQ5ODORRZ2LYCMWPZGWGCN2KDK7DXOIMZASKUY3QPZV"
)

// TestKnownVector pins the account seed/public encoding against the reference
// implementation.
func TestKnownVector(t *testing.T) {
	raw, err := hex.DecodeString(knownSeedRaw)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	seed, public, err := accountKeyPair(raw)
	if err != nil {
		t.Fatalf("accountKeyPair: %v", err)
	}
	if seed != knownSeed {
		t.Errorf("seed = %q, want %q", seed, knownSeed)
	}
	if public != knownPublic {
		t.Errorf("public = %q, want %q", public, knownPublic)
	}
}

// TestGeneratedPairRoundTrips proves a random pair is a valid account seed and
// that the public file is the seed's public half, not an unrelated key.
func TestGeneratedPairRoundTrips(t *testing.T) {
	seedFile := filepath.Join(t.TempDir(), "issuer.seed")
	publicFile := filepath.Join(t.TempDir(), "issuer.pub")
	if err := run([]string{"-seed-file", seedFile, "-public-file", publicFile}); err != nil {
		t.Fatalf("run: %v", err)
	}
	seed, err := os.ReadFile(seedFile)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	public, err := os.ReadFile(publicFile)
	if err != nil {
		t.Fatalf("read public: %v", err)
	}
	derived, err := publicFromSeed(string(seed))
	if err != nil {
		t.Fatalf("decode generated seed: %v", err)
	}
	if string(public) != derived {
		t.Errorf("public file %q does not match the seed's public key %q", public, derived)
	}
	if bytes.Equal(seed, public) {
		t.Errorf("seed and public file contain the same value")
	}

	info, err := os.Stat(seedFile)
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("seed file mode = %o, want 600", mode)
	}
}

// TestRunRefusesMissingAndCollidingPaths keeps the CLI from overwriting the
// seed with the public half or writing an empty file on a typo.
func TestRunRefusesMissingAndCollidingPaths(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("run accepted no flags")
	}
	if err := run([]string{"-seed-file", "only-seed"}); err == nil {
		t.Fatal("run accepted a missing -public-file")
	}
	same := filepath.Join(t.TempDir(), "pair")
	if err := run([]string{"-seed-file", same, "-public-file", same}); err == nil {
		t.Fatal("run accepted the same path for seed and public key")
	}
}

// publicFromSeed decodes the account seed with an independent implementation
// of the NKey framing (base32 + CRC16 + Ed25519) and returns the encoded
// public key.
func publicFromSeed(seed string) (string, error) {
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(seed)
	if err != nil {
		return "", err
	}
	if len(decoded) != 2+ed25519.SeedSize+2 {
		return "", errUnexpectedLength
	}
	payload, checksum := decoded[:len(decoded)-2], binary.LittleEndian.Uint16(decoded[len(decoded)-2:])
	if crc16(payload) != checksum {
		return "", errChecksum
	}
	if payload[0]&248 != nkeyPrefixSeed {
		return "", errPrefix
	}
	publicPrefix := (payload[0]&7)<<5 | (payload[1]&248)>>3
	if publicPrefix != nkeyPrefixAccount {
		return "", errPrefix
	}
	private := ed25519.NewKeyFromSeed(payload[2:])
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return "", errPrefix
	}
	return encodeNkeyPublic(nkeyPrefixAccount, public), nil
}

var (
	errUnexpectedLength = &decodeError{"unexpected seed length"}
	errChecksum         = &decodeError{"invalid seed checksum"}
	errPrefix           = &decodeError{"not an account seed"}
)

type decodeError struct{ message string }

func (e *decodeError) Error() string { return e.message }
