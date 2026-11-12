package cli

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// idTokenClockSkew is the small allowance applied to exp and nbf.
const idTokenClockSkew = 30 * time.Second

// idTokenHeader is the protected JOSE header of one ID token.
type idTokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// idTokenClaims is the verified claim subset. Unknown claims are ignored; the
// signature is what makes a token trustworthy.
type idTokenClaims struct {
	Issuer    string          `json:"iss"`
	Nonce     string          `json:"nonce"`
	Audience  json.RawMessage `json:"aud"`
	Expires   *int64          `json:"exp"`
	NotBefore *int64          `json:"nbf"`
}

// jsonWebKey is one verification key from the issuer JWKS.
type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// jsonWebKeySet is the issuer JWKS document.
type jsonWebKeySet struct {
	Keys []jsonWebKey `json:"keys"`
}

// VerifyIDToken verifies one compact JWS ID token against the issuer JWKS and
// the expected issuer, audience, nonce and clock. Only the RS256 and ES256
// algorithms are accepted. Every refusal is a generic error that never renders
// the token.
func VerifyIDToken(token string, jwks json.RawMessage, expectations IDTokenExpectations) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("invalid ID token: not a compact JWS")
	}
	headerData, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("invalid ID token: malformed header")
	}
	var header idTokenHeader
	if err := json.Unmarshal(headerData, &header); err != nil {
		return errors.New("invalid ID token: malformed header")
	}
	switch header.Alg {
	case "RS256", "ES256":
	default:
		return fmt.Errorf("invalid ID token: algorithm %q is not allowed", header.Alg)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("invalid ID token: malformed signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := verifyJWSSignature(jwks, header, digest[:], signature); err != nil {
		return err
	}
	claimsData, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid ID token: malformed claims")
	}
	var claims idTokenClaims
	if err := json.Unmarshal(claimsData, &claims); err != nil {
		return errors.New("invalid ID token: malformed claims")
	}
	now := expectations.Now
	if now.IsZero() {
		now = time.Now()
	}
	if claims.Issuer != expectations.Issuer || expectations.Issuer == "" {
		return errors.New("invalid ID token: unexpected issuer")
	}
	if !audienceContains(claims.Audience, expectations.Audience) {
		return errors.New("invalid ID token: unexpected audience")
	}
	if claims.Expires == nil {
		return errors.New("invalid ID token: missing expiry")
	}
	if now.After(time.Unix(*claims.Expires, 0).Add(idTokenClockSkew)) {
		return errors.New("invalid ID token: expired")
	}
	if claims.NotBefore != nil && now.Add(idTokenClockSkew).Before(time.Unix(*claims.NotBefore, 0)) {
		return errors.New("invalid ID token: not yet valid")
	}
	if expectations.Nonce != "" {
		if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectations.Nonce)) != 1 {
			return errors.New("invalid ID token: unexpected nonce")
		}
	}
	return nil
}

// audienceContains reports whether the aud claim names want as a string or as
// a member of a string array.
func audienceContains(raw json.RawMessage, want string) bool {
	if want == "" || len(raw) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	return slices.Contains(many, want)
}

// verifyJWSSignature verifies digest/signature against the eligible keys of the
// issuer JWKS. A key is eligible when its alg is empty or matches and its kid
// matches the header kid when the header names one.
func verifyJWSSignature(raw json.RawMessage, header idTokenHeader, digest, signature []byte) error {
	if len(raw) == 0 {
		return errors.New("invalid ID token: no issuer keys")
	}
	var keys jsonWebKeySet
	if err := json.Unmarshal(raw, &keys); err != nil {
		return errors.New("invalid ID token: malformed issuer keys")
	}
	eligible := 0
	for _, key := range keys.Keys {
		if key.Alg != "" && key.Alg != header.Alg {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if header.Kid != "" && key.Kid != header.Kid {
			continue
		}
		eligible++
		if verifyJWK(key, header.Alg, digest, signature) {
			return nil
		}
	}
	if eligible == 0 {
		return errors.New("invalid ID token: no matching issuer key")
	}
	return errors.New("invalid ID token: bad signature")
}

// verifyJWK verifies one signature with one JWK.
func verifyJWK(key jsonWebKey, algorithm string, digest, signature []byte) bool {
	switch {
	case algorithm == "RS256" && key.Kty == "RSA":
		modulus, err := decodeBigInt(key.N)
		if err != nil || modulus.Sign() <= 0 {
			return false
		}
		exponent, err := decodeBigInt(key.E)
		if err != nil || !exponent.IsInt64() {
			return false
		}
		published := int(exponent.Int64())
		if published < 3 {
			return false
		}
		public := &rsa.PublicKey{N: modulus, E: published}
		return rsa.VerifyPKCS1v15(public, crypto.SHA256, digest, signature) == nil
	case algorithm == "ES256" && key.Kty == "EC":
		if key.Crv != "P-256" {
			return false
		}
		x, err := decodeBigInt(key.X)
		if err != nil {
			return false
		}
		y, err := decodeBigInt(key.Y)
		if err != nil {
			return false
		}
		public, ok := p256PublicKey(x, y)
		if !ok {
			return false
		}
		if len(signature) != 64 {
			return false
		}
		left := new(big.Int).SetBytes(signature[:32])
		right := new(big.Int).SetBytes(signature[32:])
		return ecdsa.Verify(public, digest, left, right)
	}
	return false
}

// p256PublicKey builds the P-256 verification key for the affine point (x, y).
// A coordinate wider than the 256-bit field is refused; the fixed-width
// uncompressed SEC 1 point is then parsed by crypto/ecdsa, which refuses an
// unreduced coordinate and a point that is not on the curve, including (0, 0).
func p256PublicKey(x, y *big.Int) (*ecdsa.PublicKey, bool) {
	const coordinateBytes = 32
	if x.BitLen() > 8*coordinateBytes || y.BitLen() > 8*coordinateBytes {
		return nil, false
	}
	point := make([]byte, 1+2*coordinateBytes)
	point[0] = 4 // uncompressed point
	x.FillBytes(point[1 : 1+coordinateBytes])
	y.FillBytes(point[1+coordinateBytes:])
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, false
	}
	return public, true
}

// decodeBigInt decodes one unpadded base64url big-endian integer.
func decodeBigInt(value string) (*big.Int, error) {
	if value == "" {
		return nil, errors.New("empty integer")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(decoded), nil
}
