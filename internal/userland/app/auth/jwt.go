package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
)

const jwtClockSkew = 30 * time.Second

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

type verifiedAccessClaims struct {
	Subject string
	Email   string
	Name    string
	Tenant  string
	Project string
	JTI     string
	Client  string
	Expiry  time.Time
}

type upstreamIDClaims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

func (server *Server) signJWT(claims map[string]any) (string, error) {
	headerBytes, err := json.Marshal(jwtHeader{
		Alg: "ES256",
		Kid: server.material.kid,
		Typ: "JWT",
	})
	if err != nil {
		return "", err
	}
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." +
		base64.RawURLEncoding.EncodeToString(payloadBytes)
	digest := sha256.Sum256([]byte(signingInput))

	r, s, err := ecdsa.Sign(server.config.Random, server.material.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (server *Server) mintAccessToken(user UserIdentity, clientID, scope, nonce string, now time.Time) (string, string, error) {
	jti, err := randomToken(server.config.Random, 16)
	if err != nil {
		return "", "", err
	}
	claims := map[string]any{
		"iss":                     server.config.Issuer,
		"sub":                     user.Subject,
		"aud":                     server.config.Audience,
		"iat":                     now.Unix(),
		"exp":                     now.Add(server.config.AccessTokenTTL).Unix(),
		"jti":                     jti,
		server.config.TenantClaim: string(server.config.TenantID),
	}
	if server.config.ProjectID != "" {
		claims[server.config.ProjectClaim] = string(server.config.ProjectID)
	}
	if user.Email != "" {
		claims["email"] = user.Email
	}
	if user.Name != "" {
		claims["name"] = user.Name
	}
	if clientID != "" {
		claims["client_id"] = clientID
	}
	if scope != "" {
		claims["scope"] = scope
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	signed, err := server.signJWT(claims)
	if err != nil {
		return "", "", err
	}
	return signed, jti, nil
}

func (server *Server) mintIDToken(user UserIdentity, clientID, nonce string, now time.Time) (string, error) {
	jti, err := randomToken(server.config.Random, 16)
	if err != nil {
		return "", err
	}
	claims := map[string]any{
		"iss":                     server.config.Issuer,
		"sub":                     user.Subject,
		"aud":                     clientID,
		"iat":                     now.Unix(),
		"exp":                     now.Add(server.config.AccessTokenTTL).Unix(),
		"jti":                     jti,
		server.config.TenantClaim: string(server.config.TenantID),
	}
	if server.config.ProjectID != "" {
		claims[server.config.ProjectClaim] = string(server.config.ProjectID)
	}
	if user.Email != "" {
		claims["email"] = user.Email
	}
	if user.Name != "" {
		claims["name"] = user.Name
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return server.signJWT(claims)
}

func (server *Server) verifyOwnAccessToken(raw string, now time.Time) (verifiedAccessClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return verifiedAccessClaims{}, errors.New("malformed JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return verifiedAccessClaims{}, errors.New("malformed JWT header")
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return verifiedAccessClaims{}, errors.New("malformed JWT header")
	}
	if header.Alg != "ES256" || header.Kid != server.material.kid {
		return verifiedAccessClaims{}, errors.New("unexpected JWT header")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return verifiedAccessClaims{}, errors.New("malformed JWT signature")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&server.material.privateKey.PublicKey, digest[:], r, s) {
		return verifiedAccessClaims{}, errors.New("invalid JWT signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return verifiedAccessClaims{}, errors.New("malformed JWT claims")
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return verifiedAccessClaims{}, errors.New("malformed JWT claims")
	}

	iss, ok := jsonStringClaim(claims, "iss")
	if !ok || iss != server.config.Issuer {
		return verifiedAccessClaims{}, errors.New("issuer mismatch")
	}
	sub, ok := jsonStringClaim(claims, "sub")
	if !ok {
		return verifiedAccessClaims{}, errors.New("missing subject")
	}
	if !jsonAudienceContains(claims["aud"], server.config.Audience) {
		return verifiedAccessClaims{}, errors.New("audience mismatch")
	}
	exp, present, valid := jsonInt64Claim(claims, "exp")
	if !present || !valid || !now.Before(time.Unix(exp, 0).Add(jwtClockSkew)) {
		return verifiedAccessClaims{}, errors.New("token expired")
	}
	if nbf, present, valid := jsonInt64Claim(claims, "nbf"); present {
		if !valid || now.Add(jwtClockSkew).Before(time.Unix(nbf, 0)) {
			return verifiedAccessClaims{}, errors.New("token not yet valid")
		}
	}
	jti, _ := jsonStringClaim(claims, "jti")
	if jti != "" && server.store.isJTIRevoked(jti, now) {
		return verifiedAccessClaims{}, errors.New("token revoked")
	}
	email, _ := jsonStringClaim(claims, "email")
	name, _ := jsonStringClaim(claims, "name")
	tenant, _ := jsonStringClaim(claims, server.config.TenantClaim)
	project := ""
	if server.config.ProjectClaim != "" {
		project, _ = jsonStringClaim(claims, server.config.ProjectClaim)
	}
	clientID, _ := jsonStringClaim(claims, "client_id")
	return verifiedAccessClaims{
		Subject: sub,
		Email:   email,
		Name:    name,
		Tenant:  tenant,
		Project: project,
		JTI:     jti,
		Client:  clientID,
		Expiry:  time.Unix(exp, 0),
	}, nil
}

// verifyUpstreamIDToken verifies an RS256 or ES256 ID token issued by the
// configured upstream OIDC provider.
func verifyUpstreamIDToken(raw string, jwksRaw []byte, expectedIssuer, expectedClientID, expectedNonce string, now time.Time) (upstreamIDClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return upstreamIDClaims{}, errors.New("upstream ID token is not a compact JWS")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return upstreamIDClaims{}, errors.New("upstream ID token header is malformed")
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return upstreamIDClaims{}, errors.New("upstream ID token header is malformed")
	}
	if header.Alg != "RS256" && header.Alg != "ES256" {
		return upstreamIDClaims{}, errors.New("upstream ID token uses an unsupported algorithm")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return upstreamIDClaims{}, errors.New("upstream ID token signature is malformed")
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := verifyUpstreamJWSSignature(jwksRaw, header, digest[:], sig); err != nil {
		return upstreamIDClaims{}, err
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return upstreamIDClaims{}, errors.New("upstream ID token claims are malformed")
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return upstreamIDClaims{}, errors.New("upstream ID token claims are malformed")
	}

	iss, ok := jsonStringClaim(claims, "iss")
	if !ok || iss != expectedIssuer {
		return upstreamIDClaims{}, errors.New("upstream ID token issuer mismatch")
	}
	sub, ok := jsonStringClaim(claims, "sub")
	if !ok {
		return upstreamIDClaims{}, errors.New("upstream ID token subject missing")
	}
	if !jsonAudienceContains(claims["aud"], expectedClientID) {
		return upstreamIDClaims{}, errors.New("upstream ID token audience mismatch")
	}
	// OIDC Core §3.1.3.7: azp must name this client, and is required when the
	// token has several audiences.
	azp, _ := jsonStringClaim(claims, "azp")
	var audiences []string
	_ = json.Unmarshal(claims["aud"], &audiences)
	if (azp != "" || len(audiences) > 1) && azp != expectedClientID {
		return upstreamIDClaims{}, errors.New("upstream ID token authorized party mismatch")
	}
	exp, present, valid := jsonInt64Claim(claims, "exp")
	if !present || !valid || !now.Before(time.Unix(exp, 0).Add(jwtClockSkew)) {
		return upstreamIDClaims{}, errors.New("upstream ID token expired")
	}
	if nbf, present, valid := jsonInt64Claim(claims, "nbf"); present {
		if !valid || now.Add(jwtClockSkew).Before(time.Unix(nbf, 0)) {
			return upstreamIDClaims{}, errors.New("upstream ID token not yet valid")
		}
	}
	if expectedNonce != "" {
		nonce, _ := jsonStringClaim(claims, "nonce")
		if !constantTimeEqualStrings(nonce, expectedNonce) {
			return upstreamIDClaims{}, errors.New("upstream ID token nonce mismatch")
		}
	}
	email, _ := jsonStringClaim(claims, "email")
	name, _ := jsonStringClaim(claims, "name")
	emailVerified := jsonBoolClaim(claims, "email_verified")
	return upstreamIDClaims{
		Issuer:        iss,
		Subject:       sub,
		Email:         email,
		EmailVerified: emailVerified,
		Name:          name,
	}, nil
}

type upstreamJWK struct {
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

func verifyUpstreamJWSSignature(jwksRaw []byte, header jwtHeader, digest, sig []byte) error {
	var keySet struct {
		Keys []upstreamJWK `json:"keys"`
	}
	if err := json.Unmarshal(jwksRaw, &keySet); err != nil || len(keySet.Keys) == 0 {
		return errors.New("upstream JWKS is empty or malformed")
	}
	for _, key := range keySet.Keys {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if key.Alg != "" && key.Alg != header.Alg {
			continue
		}
		if header.Kid != "" && key.Kid != header.Kid {
			continue
		}
		if verifyOneUpstreamJWK(key, header.Alg, digest, sig) {
			return nil
		}
	}
	return errors.New("upstream ID token signature verification failed")
}

func verifyOneUpstreamJWK(key upstreamJWK, alg string, digest, sig []byte) bool {
	switch {
	case alg == "RS256" && key.Kty == "RSA":
		nBytes, err := decodeBase64URL(key.N)
		if err != nil || len(nBytes) == 0 {
			return false
		}
		eBytes, err := decodeBase64URL(key.E)
		if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
			return false
		}
		modulus := new(big.Int).SetBytes(nBytes)
		if modulus.Sign() <= 0 {
			return false
		}
		exp := 0
		for _, b := range eBytes {
			exp = exp<<8 | int(b)
		}
		if exp < 3 {
			return false
		}
		pub := &rsa.PublicKey{N: modulus, E: exp}
		return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest, sig) == nil
	case alg == "ES256" && key.Kty == "EC" && key.Crv == "P-256":
		xBytes, err := decodeBase64URL(key.X)
		if err != nil || len(xBytes) == 0 {
			return false
		}
		yBytes, err := decodeBase64URL(key.Y)
		if err != nil || len(yBytes) == 0 {
			return false
		}
		xBytes = bytes.TrimLeft(xBytes, "\x00")
		yBytes = bytes.TrimLeft(yBytes, "\x00")
		if len(xBytes) > 32 || len(yBytes) > 32 {
			return false
		}
		point := make([]byte, 65)
		point[0] = 4
		copy(point[1+32-len(xBytes):], xBytes)
		copy(point[65-len(yBytes):], yBytes)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			return false
		}
		if ecdsa.VerifyASN1(pub, digest, sig) {
			return true
		}
		if len(sig) != 64 {
			return false
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(pub, digest, r, s)
	default:
		return false
	}
}

func decodeBase64URL(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("empty value")
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(raw)
}

func jsonStringClaim(claims map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := claims[name]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

func jsonBoolClaim(claims map[string]json.RawMessage, name string) bool {
	raw, ok := claims[name]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(s, "true")
	}
	return false
}

func jsonAudienceContains(raw json.RawMessage, want string) bool {
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

func jsonInt64Claim(claims map[string]json.RawMessage, name string) (int64, bool, bool) {
	raw, ok := claims[name]
	if !ok {
		return 0, false, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return 0, true, false
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, true, false
	}
	parsed, err := strconv.ParseInt(num.String(), 10, 64)
	if err != nil {
		return 0, true, false
	}
	return parsed, true, true
}

// constantTimeEqualStrings compares two strings in constant time without
// leaking their lengths by comparing their SHA-256 digests and checking
// equality of lengths cleanly.
func constantTimeEqualStrings(left, right string) bool {
	leftSum := sha256.Sum256([]byte(left))
	rightSum := sha256.Sum256([]byte(right))
	digestMatch := subtle.ConstantTimeCompare(leftSum[:], rightSum[:])
	lenMatch := subtle.ConstantTimeEq(int32(len(left)), int32(len(right)))
	return (digestMatch & lenMatch) == 1
}
