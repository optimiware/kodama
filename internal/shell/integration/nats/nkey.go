package nats

// Minimal NKey encoding for the broker authorization callout. The NATS server
// signs requests with server keys and accepts responses signed by the account
// key configured as auth_callout.issuer. The adapter therefore needs only the
// Ed25519 seed decode, public-key encode and signature primitives, not the
// broader NKey or JWT libraries.

import (
	"crypto/ed25519"
	"encoding/base32"
	"encoding/binary"

	"github.com/optimiware/kodama/internal/fault"
)

const (
	nkeyPrefixSeed    byte = 18 << 3
	nkeyPrefixAccount byte = 0
	nkeyPrefixServer  byte = 13 << 3
	nkeyPrefixUser    byte = 20 << 3
)

var nkeyBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// decodeAccountSeed parses one account seed and returns its Ed25519 private key
// and the matching public account key.
func decodeAccountSeed(seed string) (ed25519.PrivateKey, string, error) {
	prefix, raw, err := decodeNkeySeed(seed)
	if err != nil {
		return nil, "", err
	}
	if prefix != nkeyPrefixAccount {
		return nil, "", fault.New(fault.InvalidInput, "nats callout issuer seed is not an account seed")
	}
	private := ed25519.NewKeyFromSeed(raw)
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return nil, "", fault.New(fault.InternalFailure, "nats callout issuer public key is unavailable")
	}
	return private, encodeNkeyPublic(nkeyPrefixAccount, public), nil
}

// decodeNkeySeed decodes an encoded NKey seed and verifies its checksum.
func decodeNkeySeed(seed string) (byte, []byte, error) {
	raw := make([]byte, nkeyBase32.DecodedLen(len(seed)))
	n, err := nkeyBase32.Decode(raw, []byte(seed))
	if err != nil {
		return 0, nil, fault.Wrap(fault.InvalidInput, "nats callout issuer seed is not base32", err)
	}
	raw = raw[:n]
	if len(raw) < 4 {
		return 0, nil, fault.New(fault.InvalidInput, "nats callout issuer seed is truncated")
	}
	payload, checksum := raw[:len(raw)-2], binary.LittleEndian.Uint16(raw[len(raw)-2:])
	if crc16(payload) != checksum {
		return 0, nil, fault.New(fault.InvalidInput, "nats callout issuer seed checksum is invalid")
	}
	if payload[0]&248 != nkeyPrefixSeed || len(payload) != 34 {
		return 0, nil, fault.New(fault.InvalidInput, "nats callout issuer seed is malformed")
	}
	publicPrefix := (payload[0]&7)<<5 | (payload[1]&248)>>3
	return publicPrefix, payload[2:], nil
}

// encodeNkeyPublic encodes one public key with its NKey prefix and checksum.
func encodeNkeyPublic(prefix byte, public ed25519.PublicKey) string {
	raw := make([]byte, 0, 1+len(public)+2)
	raw = append(raw, prefix)
	raw = append(raw, public...)
	var checksum [2]byte
	binary.LittleEndian.PutUint16(checksum[:], crc16(raw))
	raw = append(raw, checksum[:]...)
	return nkeyBase32.EncodeToString(raw)
}

// crc16 is the XMODEM checksum NKeys append to every encoded key.
func crc16(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
