// Tests for the ES256 verification keys cli.VerifyIDToken accepts. The
// login vectors under test/contracts/public cover the RS256 path and the claim
// checks; these pin which P-256 JWK encodings verify a signed ID token and
// which are refused.
package cli_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// es256Fixture is one deterministic P-256 signing key with its public point and
// one ID token signed by it.
type es256Fixture struct {
	key          *ecdsa.PrivateKey
	x, y         []byte // fixed-width 32-byte big-endian coordinates
	token        string
	expectations cli.IDTokenExpectations
}

// newES256Fixture derives the key from a small fixed scalar. Scalar 43 yields a
// y coordinate and scalar 379 an x coordinate with a leading zero byte, so the
// minimal encoding of each differs from the fixed-width one.
func newES256Fixture(t *testing.T, scalar uint64) es256Fixture {
	t.Helper()
	raw := make([]byte, 32)
	binary.BigEndian.PutUint64(raw[24:], scalar)
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		t.Fatalf("ParseRawPrivateKey(%d): %v", scalar, err)
	}
	point, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("PublicKey.Bytes: %v", err)
	}
	if len(point) != 65 || point[0] != 4 {
		t.Fatalf("public point is not a 65-byte uncompressed encoding")
	}
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	expectations := cli.IDTokenExpectations{
		Issuer:   "https://auth.example.com",
		Audience: "kodactl",
		Nonce:    "nonce-value",
		Now:      now,
	}
	return es256Fixture{
		key:          key,
		x:            point[1:33],
		y:            point[33:],
		token:        signES256IDToken(t, key, expectations, es256RawSignature),
		expectations: expectations,
	}
}

// es256RawSignature is the JWS ES256 encoding: fixed-width r followed by s.
func es256RawSignature(r, s *big.Int) []byte {
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signature
}

// signES256IDToken signs one valid claim set and encodes the signature through
// encode, so a case can produce a malformed signature over a valid digest.
func signES256IDToken(t *testing.T, key *ecdsa.PrivateKey, expectations cli.IDTokenExpectations, encode func(r, s *big.Int) []byte) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": "ec-key", "typ": "JWT"})
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss":   expectations.Issuer,
		"aud":   expectations.Audience,
		"exp":   expectations.Now.Add(time.Hour).Unix(),
		"nonce": expectations.Nonce,
		"sub":   "user-1",
	})
	if err != nil {
		t.Fatalf("encode claims: %v", err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign ID token: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(encode(r, s))
}

// es256JWKS renders one single-key issuer JWKS from already encoded members.
func es256JWKS(t *testing.T, members map[string]string) json.RawMessage {
	t.Helper()
	key := map[string]string{"kty": "EC", "kid": "ec-key", "use": "sig", "alg": "ES256", "crv": "P-256"}
	for name, value := range members {
		if value == omitMember {
			delete(key, name)
			continue
		}
		key[name] = value
	}
	data, err := json.Marshal(map[string]any{"keys": []any{key}})
	if err != nil {
		t.Fatalf("encode JWKS: %v", err)
	}
	return data
}

// omitMember removes a JWK member instead of overriding it.
const omitMember = "\x00omit"

func encodeCoordinate(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

// p256Prime is the P-256 field prime.
func p256Prime() *big.Int {
	return new(big.Int).Set(elliptic.P256().Params().P)
}

// plusPrime returns coordinate+p, the unreduced encoding of the same field
// element.
func plusPrime(coordinate []byte) []byte {
	return new(big.Int).Add(new(big.Int).SetBytes(coordinate), p256Prime()).Bytes()
}

// flipLowBit returns a copy of coordinate with its lowest bit inverted.
func flipLowBit(coordinate []byte) []byte {
	flipped := bytes.Clone(coordinate)
	flipped[len(flipped)-1] ^= 0x01
	return flipped
}

var es256Scalars = map[string]uint64{
	"short y coordinate": 43,
	"short x coordinate": 379,
}

// TestVerifyIDTokenES256AcceptsEquivalentKeyEncodings pins that the published
// key verifies its token in the fixed-width encoding, in the minimal encoding
// with leading zero bytes stripped and with extra leading zero bytes.
func TestVerifyIDTokenES256AcceptsEquivalentKeyEncodings(t *testing.T) {
	for keyName, scalar := range es256Scalars {
		fixture := newES256Fixture(t, scalar)
		minimalX := bytes.TrimLeft(fixture.x, "\x00")
		minimalY := bytes.TrimLeft(fixture.y, "\x00")
		if len(minimalX) == 32 && len(minimalY) == 32 {
			t.Fatalf("%s: fixture has no coordinate with a leading zero byte", keyName)
		}
		pad := func(coordinate []byte, zeros int) []byte {
			return append(make([]byte, zeros), coordinate...)
		}
		cases := map[string][2][]byte{
			"fixed width":       {fixture.x, fixture.y},
			"minimal":           {minimalX, minimalY},
			"one leading zero":  {pad(fixture.x, 1), pad(fixture.y, 1)},
			"many leading zero": {pad(fixture.x, 9), pad(fixture.y, 17)},
		}
		for name, point := range cases {
			t.Run(keyName+"/"+name, func(t *testing.T) {
				keys := es256JWKS(t, map[string]string{"x": encodeCoordinate(point[0]), "y": encodeCoordinate(point[1])})
				if err := cli.VerifyIDToken(fixture.token, keys, fixture.expectations); err != nil {
					t.Fatalf("VerifyIDToken refused a valid ES256 key: %v", err)
				}
			})
		}
	}
}

// TestVerifyIDTokenES256RefusesInvalidKeys pins that a token the published key
// verifies is refused under every off-curve, unreduced, truncated, malformed or
// foreign-curve rendering of that key. A lenient decoder that reduced a
// coordinate modulo p, ignored the curve name or repaired the encoding would
// accept these, because the signature itself is valid.
func TestVerifyIDTokenES256RefusesInvalidKeys(t *testing.T) {
	for keyName, scalar := range es256Scalars {
		fixture := newES256Fixture(t, scalar)
		x, y := encodeCoordinate(fixture.x), encodeCoordinate(fixture.y)
		if err := cli.VerifyIDToken(fixture.token, es256JWKS(t, map[string]string{"x": x, "y": y}), fixture.expectations); err != nil {
			t.Fatalf("%s: the unmodified key refused its token: %v", keyName, err)
		}
		negatedY := new(big.Int).Sub(p256Prime(), new(big.Int).SetBytes(fixture.y)).FillBytes(make([]byte, 32))
		cases := map[string]map[string]string{
			"x off curve":            {"x": encodeCoordinate(flipLowBit(fixture.x)), "y": y},
			"y off curve":            {"x": x, "y": encodeCoordinate(flipLowBit(fixture.y))},
			"coordinates swapped":    {"x": y, "y": x},
			"negated y":              {"x": x, "y": encodeCoordinate(negatedY)},
			"x not reduced":          {"x": encodeCoordinate(plusPrime(fixture.x)), "y": y},
			"y not reduced":          {"x": x, "y": encodeCoordinate(plusPrime(fixture.y))},
			"x truncated":            {"x": encodeCoordinate(fixture.x[:31]), "y": y},
			"y truncated":            {"x": x, "y": encodeCoordinate(fixture.y[:31])},
			"x with a trailing byte": {"x": encodeCoordinate(append(bytes.Clone(fixture.x), 0)), "y": y},
			"y with a trailing byte": {"x": x, "y": encodeCoordinate(append(bytes.Clone(fixture.y), 0))},
			"x is the field prime":   {"x": encodeCoordinate(p256Prime().Bytes()), "y": y},
			"y is the field prime":   {"x": x, "y": encodeCoordinate(p256Prime().Bytes())},
			"x zero":                 {"x": "AA", "y": y},
			"y zero":                 {"x": x, "y": "AA"},
			"point at infinity":      {"x": "AA", "y": "AA"},
			"x empty":                {"x": "", "y": y},
			"y empty":                {"x": x, "y": ""},
			"x missing":              {"x": omitMember, "y": y},
			"y missing":              {"x": x, "y": omitMember},
			"x padded base64":        {"x": base64.URLEncoding.EncodeToString(fixture.x), "y": y},
			"y padded base64":        {"x": x, "y": base64.URLEncoding.EncodeToString(fixture.y)},
			"x not base64url":        {"x": x + "!", "y": y},
			"y not base64url":        {"x": x, "y": y + "!"},
			"curve P-384":            {"x": x, "y": y, "crv": "P-384"},
			"curve P-521":            {"x": x, "y": y, "crv": "P-521"},
			"curve secp256k1":        {"x": x, "y": y, "crv": "secp256k1"},
			"curve lower case":       {"x": x, "y": y, "crv": "p-256"},
			"curve missing":          {"x": x, "y": y, "crv": omitMember},
			"key type RSA":           {"x": x, "y": y, "kty": "RSA"},
			"key type OKP":           {"x": x, "y": y, "kty": "OKP"},
			"key type missing":       {"x": x, "y": y, "kty": omitMember},
		}
		for name, members := range cases {
			t.Run(keyName+"/"+name, func(t *testing.T) {
				err := cli.VerifyIDToken(fixture.token, es256JWKS(t, members), fixture.expectations)
				if err == nil {
					t.Fatal("VerifyIDToken accepted a token under an invalid ES256 key")
				}
				if got, want := err.Error(), "invalid ID token: bad signature"; got != want {
					t.Fatalf("error = %q; want %q", got, want)
				}
			})
		}
	}
}

// TestVerifyIDTokenES256RefusesMalformedSignatures pins the JWS signature
// shape: exactly the 64-byte r||s form with non-zero halves.
func TestVerifyIDTokenES256RefusesMalformedSignatures(t *testing.T) {
	fixture := newES256Fixture(t, es256Scalars["short y coordinate"])
	keys := es256JWKS(t, map[string]string{"x": encodeCoordinate(fixture.x), "y": encodeCoordinate(fixture.y)})

	cases := map[string]func(r, s *big.Int) []byte{
		"ASN.1 DER": func(r, s *big.Int) []byte {
			der, err := asn1.Marshal(struct{ R, S *big.Int }{r, s})
			if err != nil {
				t.Fatalf("encode DER signature: %v", err)
			}
			return der
		},
		"one byte short": func(r, s *big.Int) []byte { return es256RawSignature(r, s)[:63] },
		"one byte long":  func(r, s *big.Int) []byte { return append(es256RawSignature(r, s), 0) },
		"zero r":         func(_, s *big.Int) []byte { return es256RawSignature(new(big.Int), s) },
		"zero s":         func(r, _ *big.Int) []byte { return es256RawSignature(r, new(big.Int)) },
		"swapped halves": func(r, s *big.Int) []byte { return es256RawSignature(s, r) },
		"all zero":       func(_, _ *big.Int) []byte { return make([]byte, 64) },
	}
	for name, encode := range cases {
		t.Run(name, func(t *testing.T) {
			token := signES256IDToken(t, fixture.key, fixture.expectations, encode)
			err := cli.VerifyIDToken(token, keys, fixture.expectations)
			if err == nil {
				t.Fatal("VerifyIDToken accepted a malformed ES256 signature")
			}
			if got, want := err.Error(), "invalid ID token: bad signature"; got != want {
				t.Fatalf("error = %q; want %q", got, want)
			}
		})
	}
}
