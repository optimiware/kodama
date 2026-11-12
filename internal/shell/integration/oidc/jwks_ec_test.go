package oidc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// ecCoordinates returns the fixed-width big-endian affine coordinates of a
// P-256 public key.
func ecCoordinates(t *testing.T, key *ecdsa.PublicKey) (x, y []byte) {
	t.Helper()
	encoded, err := key.Bytes()
	if err != nil {
		t.Fatalf("encoding EC public key: %v", err)
	}
	if len(encoded) != 1+2*p256CoordinateBytes || encoded[0] != 4 {
		t.Fatalf("EC public key encoding is %d bytes with prefix %d; want an uncompressed P-256 point", len(encoded), encoded[0])
	}
	return encoded[1 : 1+p256CoordinateBytes], encoded[1+p256CoordinateBytes:]
}

// ecKeyWithLeadingZeroX returns a deterministic P-256 key whose X coordinate
// starts with a zero byte, so its minimal big-endian form is shorter than the
// field width.
func ecKeyWithLeadingZeroX(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	for counter := range 1 << 16 {
		scalar := sha256.Sum256([]byte{'j', 'w', 'k', 's', byte(counter >> 8), byte(counter)})
		key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar[:])
		if err != nil {
			continue
		}
		if x, _ := ecCoordinates(t, &key.PublicKey); x[0] == 0 {
			return key
		}
	}
	t.Fatal("no P-256 key with a leading zero X coordinate was found")
	return nil
}

// p256PointWithZeroX returns the Y coordinate of a P-256 point whose X
// coordinate is zero, together with the field prime.
func p256PointWithZeroX(t *testing.T) (y, prime []byte) {
	t.Helper()
	params := elliptic.P256().Params()
	root := new(big.Int).ModSqrt(params.B, params.P)
	if root == nil {
		t.Fatal("P-256 has no point with a zero X coordinate")
	}
	return root.FillBytes(make([]byte, p256CoordinateBytes)), params.P.FillBytes(make([]byte, p256CoordinateBytes))
}

func rawURL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func ecJWKFields(curve, x, y string) map[string]any {
	return map[string]any{"kty": "EC", "kid": "ec-key", "use": "sig", "alg": "ES256", "crv": curve, "x": x, "y": y}
}

// TestJWKSAcceptsEveryEncodingOfAValidP256Key pins the accepted set: a point
// on P-256 is usable whether its coordinates are fixed width, minimal, zero
// padded or padded base64, and the parsed key is exactly the published one.
func TestJWKSAcceptsEveryEncodingOfAValidP256Key(t *testing.T) {
	key := ecKeyWithLeadingZeroX(t)
	x, y := ecCoordinates(t, &key.PublicKey)
	zeroXRoot, _ := p256PointWithZeroX(t)
	zeroXKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(),
		append(append([]byte{4}, make([]byte, p256CoordinateBytes)...), zeroXRoot...))
	if err != nil {
		t.Fatalf("building the zero-X P-256 point: %v", err)
	}

	cases := []struct {
		name string
		x, y string
		want *ecdsa.PublicKey
	}{
		{"fixed_width", rawURL(x), rawURL(y), &key.PublicKey},
		{"minimal_big_endian", rawURL(bytes.TrimLeft(x, "\x00")), rawURL(bytes.TrimLeft(y, "\x00")), &key.PublicKey},
		{"one_leading_zero_byte", rawURL(append([]byte{0}, x...)), rawURL(append([]byte{0}, y...)), &key.PublicKey},
		{"many_leading_zero_bytes", rawURL(append(make([]byte, 64), x...)), rawURL(append(make([]byte, 64), y...)), &key.PublicKey},
		{"padded_base64", base64.URLEncoding.EncodeToString(x), base64.URLEncoding.EncodeToString(y), &key.PublicKey},
		{"zero_x_fixed_width", rawURL(make([]byte, p256CoordinateBytes)), rawURL(zeroXRoot), zeroXKey},
		{"zero_x_single_byte", rawURL([]byte{0}), rawURL(zeroXRoot), zeroXKey},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			keys, err := parseJWKS([]byte(jwksJSON(t, ecJWKFields("P-256", testCase.x, testCase.y))))
			if err != nil {
				t.Fatalf("parseJWKS refused a valid P-256 key: %v", err)
			}
			parsed, ok := keys["ec-key"]
			if !ok || parsed.ec == nil || parsed.rsa != nil {
				t.Fatalf("parsed keys = %+v; want one EC key", keys)
			}
			if !parsed.ec.Equal(testCase.want) {
				t.Fatal("parsed EC key is not the published key")
			}
		})
	}

	// The lenient encodings verify a real token end to end, not only parse.
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(),
		ecJWKFields("P-256", rawURL(bytes.TrimLeft(x, "\x00")), rawURL(append([]byte{0}, y...))))
	token := makeJWT(t, "ES256", "ec-key", testClaimsMap(now), ecSHA256Signer(key))
	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("Verify with a minimally encoded EC key: %v", err)
	}
}

// TestJWKSRejectsInvalidECKeys pins the rejected set: a point off the curve,
// the point at infinity, a coordinate that is not reduced or is wider than the
// field, a missing or undecodable coordinate and every curve other than P-256
// make the whole JWKS document malformed.
func TestJWKSRejectsInvalidECKeys(t *testing.T) {
	key := mustECKey(t)
	x, y := ecCoordinates(t, &key.PublicKey)
	zeroXRoot, prime := p256PointWithZeroX(t)
	zero := make([]byte, p256CoordinateBytes)

	flipped := bytes.Clone(y)
	flipped[len(flipped)-1] ^= 1

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generating P-384 key: %v", err)
	}
	p384Encoded, err := p384.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("encoding P-384 key: %v", err)
	}
	p384X, p384Y := p384Encoded[1:49], p384Encoded[49:]

	cases := []struct {
		name  string
		entry map[string]any
	}{
		{"y_off_by_one", ecJWKFields("P-256", rawURL(x), rawURL(flipped))},
		{"coordinates_swapped", ecJWKFields("P-256", rawURL(y), rawURL(x))},
		{"zero_x_with_unrelated_y", ecJWKFields("P-256", rawURL(zero), rawURL(y))},
		{"infinity_fixed_width", ecJWKFields("P-256", rawURL(zero), rawURL(zero))},
		{"infinity_single_byte", ecJWKFields("P-256", rawURL([]byte{0}), rawURL([]byte{0}))},
		{"x_not_reduced", ecJWKFields("P-256", rawURL(prime), rawURL(zeroXRoot))},
		{"x_wider_than_field", ecJWKFields("P-256", rawURL(append([]byte{1}, x...)), rawURL(y))},
		{"y_wider_than_field", ecJWKFields("P-256", rawURL(x), rawURL(append([]byte{1}, y...)))},
		{"x_missing", map[string]any{"kty": "EC", "kid": "ec-key", "crv": "P-256", "y": rawURL(y)}},
		{"y_missing", map[string]any{"kty": "EC", "kid": "ec-key", "crv": "P-256", "x": rawURL(x)}},
		{"x_empty", ecJWKFields("P-256", "", rawURL(y))},
		{"y_empty", ecJWKFields("P-256", rawURL(x), "")},
		{"x_not_base64", ecJWKFields("P-256", "!not-base64!", rawURL(y))},
		{"y_not_base64", ecJWKFields("P-256", rawURL(x), "!not-base64!")},
		{"curve_missing", map[string]any{"kty": "EC", "kid": "ec-key", "x": rawURL(x), "y": rawURL(y)}},
		{"curve_lower_case", ecJWKFields("p-256", rawURL(x), rawURL(y))},
		{"curve_secp256k1", ecJWKFields("secp256k1", rawURL(x), rawURL(y))},
		{"curve_p384_label", ecJWKFields("P-384", rawURL(x), rawURL(y))},
		{"curve_p521_label", ecJWKFields("P-521", rawURL(x), rawURL(y))},
		{"p384_key", ecJWKFields("P-384", rawURL(p384X), rawURL(p384Y))},
		{"p384_key_labelled_p256", ecJWKFields("P-256", rawURL(p384X), rawURL(p384Y))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			keys, err := parseJWKS([]byte(jwksJSON(t, testCase.entry)))
			if err == nil {
				t.Fatalf("parseJWKS accepted an invalid EC key: %+v", keys)
			}
			if keys != nil {
				t.Fatalf("parseJWKS returned keys with its refusal: %+v", keys)
			}
		})
	}

	// A provider that publishes an off-curve key is unavailable, never a
	// source of a usable key.
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), ecJWKFields("P-256", rawURL(x), rawURL(flipped)))
	token := makeJWT(t, "ES256", "ec-key", testClaimsMap(now), ecSHA256Signer(key))
	principal, err := verifyRequest(verifier, token, now)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if principal.ID != "" {
		t.Fatalf("off-curve key returned a principal: %+v", principal)
	}
}
