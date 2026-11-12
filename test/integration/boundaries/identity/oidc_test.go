//go:build integration

package identity_test

import (
	"context"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	shellidentity "github.com/optimiware/kodama/internal/shell/identity"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

func newVerifier(t *testing.T, provider *idfix.Provider) shellidentity.OIDCVerifier {
	t.Helper()
	verifier, err := provider.NewVerifier()
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

func verifyToken(t *testing.T, verifier shellidentity.OIDCVerifier, provider *idfix.Provider, token string) (shellidentity.Principal, error) {
	t.Helper()
	return verifier.Verify(context.Background(), shellidentity.OIDCRequest{
		AccessToken: token,
		Now:         provider.Clock().Now(),
	})
}

func TestOIDCDiscoveryDocument(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	response, err := provider.Client().Get(provider.Issuer() + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET discovery: %v", err)
	}
	var document map[string]any
	decodeJSON(t, readBody(t, response), &document)
	if document["issuer"] != provider.Issuer() {
		t.Fatalf("issuer = %v; want %q", document["issuer"], provider.Issuer())
	}
	if document["jwks_uri"] != provider.JWKSURL() {
		t.Fatalf("jwks_uri = %v; want %q", document["jwks_uri"], provider.JWKSURL())
	}
	if document["token_endpoint"] != provider.Issuer()+"/oauth/token" {
		t.Fatalf("token_endpoint = %v", document["token_endpoint"])
	}
	algorithms, ok := document["id_token_signing_alg_values_supported"].([]any)
	if !ok || len(algorithms) == 0 {
		t.Fatalf("id_token_signing_alg_values_supported = %v", document["id_token_signing_alg_values_supported"])
	}
	if got := provider.PublishedKeys(); len(got) != 1 || got[0] != "key-1" {
		t.Fatalf("published keys = %v; want [key-1]", got)
	}
}

func TestOIDCVerifierAcceptsMintedHumanTokens(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)

	t.Run("rs256", func(t *testing.T) {
		token, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
		if err != nil {
			t.Fatalf("MintAccessToken: %v", err)
		}
		principal, err := verifyToken(t, verifier, provider, token.Value)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		wantID, err := shelloidc.PrincipalIDFor(provider.Issuer(), "auth0|human-1")
		if err != nil {
			t.Fatalf("PrincipalIDFor: %v", err)
		}
		if principal.ID != wantID || principal.Kind != shellidentity.KindHuman {
			t.Fatalf("principal = %+v; want human %q", principal, wantID)
		}
		if string(principal.Scope.TenantID) != fixtureTenant || string(principal.Scope.ProjectID) != fixtureProject {
			t.Fatalf("scope = %+v; want tenant/project", principal.Scope)
		}
	})

	t.Run("es256", func(t *testing.T) {
		ecKey, err := idfix.NewECKey("ec-key-1")
		if err != nil {
			t.Fatalf("NewECKey: %v", err)
		}
		if err := provider.AddKey(ecKey); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
		token, err := provider.Mint(idfix.TokenSpec{
			Subject:    "auth0|human-2",
			KeyID:      "ec-key-1",
			SigningKey: ecKey,
			Algorithm:  "ES256",
		})
		if err != nil {
			t.Fatalf("Mint ES256: %v", err)
		}
		principal, err := verifyToken(t, verifier, provider, token.Value)
		if err != nil {
			t.Fatalf("Verify ES256: %v", err)
		}
		wantID, _ := shelloidc.PrincipalIDFor(provider.Issuer(), "auth0|human-2")
		if principal.ID != wantID {
			t.Fatalf("principal id = %q; want %q", principal.ID, wantID)
		}
	})
}

func TestOIDCVerifierRefusesIssuerAudienceExpiryAndNotBefore(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	now := provider.Clock().Now()

	cases := []struct {
		name string
		spec idfix.TokenSpec
	}{
		{"wrong issuer", idfix.TokenSpec{Issuer: "https://evil.example"}},
		{"wrong audience", idfix.TokenSpec{Audience: "other-api"}},
		{"expired", idfix.TokenSpec{ExpiresAt: now.Add(-time.Second)}},
		{"not yet valid", idfix.TokenSpec{NotBefore: new(now.Add(time.Hour))}},
		{"missing expiry", idfix.TokenSpec{Mutate: func(claims map[string]any) { delete(claims, "exp") }}},
		{"malformed expiry", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims["exp"] = "soon" }}},
		{"fractional expiry", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims["exp"] = 1.5 }}},
		{"missing tenant", idfix.TokenSpec{Mutate: func(claims map[string]any) { delete(claims, provider.TenantClaim()) }}},
		{"invalid tenant", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims[provider.TenantClaim()] = "bad tenant" }}},
		{"empty tenant", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims[provider.TenantClaim()] = "" }}},
		{"invalid project", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims[provider.ProjectClaim()] = "bad project" }}},
		{"missing subject", idfix.TokenSpec{Mutate: func(claims map[string]any) { delete(claims, "sub") }}},
		{"empty subject", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims["sub"] = "" }}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := provider.Mint(testCase.spec)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, err = verifyToken(t, verifier, provider, token.Value)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	// Positive control: the same verifier accepts a valid token, so the
	// refusals above are claim checks rather than a verifier that refuses
	// everything.
	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint valid: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the refusal cases: %v", err)
	}
}

func TestOIDCVerifierRefusesWrongAndUnknownKeys(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	publishedRSA, _ := provider.Key("key-1")
	otherRSA, err := idfix.NewRSAKey("other-rsa")
	if err != nil {
		t.Fatalf("NewRSAKey: %v", err)
	}
	publishedEC, err := idfix.NewECKey("ec-key-1")
	if err != nil {
		t.Fatalf("NewECKey: %v", err)
	}
	if err := provider.AddKey(publishedEC); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	otherEC, err := idfix.NewECKey("other-ec")
	if err != nil {
		t.Fatalf("NewECKey: %v", err)
	}

	// RS256 triple: the published key verifies, a different RSA key using the
	// same published kid is refused, and a tampered signature is refused.
	t.Run("rs256 published key verifies", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{KeyID: "key-1", SigningKey: publishedRSA})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
	t.Run("rs256 wrong key for a published kid", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{KeyID: "key-1", SigningKey: otherRSA})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		_, err = verifyToken(t, verifier, provider, token.Value)
		requireFaultClass(t, err, fault.Unauthenticated)
	})
	t.Run("rs256 tampered signature", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{KeyID: "key-1", SigningKey: publishedRSA})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		_, err = verifyToken(t, verifier, provider, tamperSignature(t, token.Value, flipLastByte))
		requireFaultClass(t, err, fault.Unauthenticated)
	})

	// ES256 triple: the published EC key verifies, a different EC key using the
	// same published kid is refused, and a tampered signature is refused. The
	// last two are the cases that fail when ES256 signature verification is
	// skipped entirely.
	t.Run("es256 published key verifies", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|human-ec", KeyID: "ec-key-1", SigningKey: publishedEC, Algorithm: "ES256"})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
	t.Run("es256 wrong key for a published kid", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|human-forged", KeyID: "ec-key-1", SigningKey: otherEC, Algorithm: "ES256"})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		_, err = verifyToken(t, verifier, provider, token.Value)
		requireFaultClass(t, err, fault.Unauthenticated)
	})
	t.Run("es256 tampered signature", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|human-tampered", KeyID: "ec-key-1", SigningKey: publishedEC, Algorithm: "ES256"})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		_, err = verifyToken(t, verifier, provider, tamperSignature(t, token.Value, flipLastByte))
		requireFaultClass(t, err, fault.Unauthenticated)
	})

	t.Run("unknown kid refreshes once and refuses", func(t *testing.T) {
		before := provider.JWKSFetchCount()
		token, err := provider.Mint(idfix.TokenSpec{KeyID: "key-unknown", SigningKey: otherRSA})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		_, err = verifyToken(t, verifier, provider, token.Value)
		requireFaultClass(t, err, fault.Unauthenticated)
		if after := provider.JWKSFetchCount(); after != before+2 {
			t.Fatalf("JWKS fetches = %d; want %d (initial + one forced refresh)", after, before+2)
		}
	})

	t.Run("published key still verifies", func(t *testing.T) {
		token, err := provider.Mint(idfix.TokenSpec{KeyID: "key-1", SigningKey: publishedRSA})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

// TestOIDCVerifierRefusesForgedECDSASignatures closes the ES256 signature
// survivor class: a verifier that skips ECDSA verification accepts every token
// whose header names a published EC kid. The table covers both accepted
// encodings (ASN.1 DER and JOSE R||S) and the tampered, truncated and
// wrong-length forms that must never verify.
func TestOIDCVerifierRefusesForgedECDSASignatures(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	ecKey, err := idfix.NewECKey("ec-key-1")
	if err != nil {
		t.Fatalf("NewECKey: %v", err)
	}
	if err := provider.AddKey(ecKey); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	valid, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|human-ec", KeyID: "ec-key-1", SigningKey: ecKey, Algorithm: "ES256"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("baseline ES256 Verify: %v", err)
	}

	// Positive control: the same signature re-encoded as fixed-width JOSE R||S
	// still verifies, so the raw branch is exercised and not a permissive
	// fallback.
	raw := rawECDSASignature(t, valid.Value)
	if _, err := verifyToken(t, verifier, provider, raw); err != nil {
		t.Fatalf("valid raw R||S signature was refused: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"der flipped byte", tamperSignature(t, valid.Value, flipLastByte)},
		{"der truncated", tamperSignature(t, valid.Value, truncateOneByte)},
		{"der wrong length", tamperSignature(t, valid.Value, func(signature []byte) []byte { return signature[:len(signature)/2] })},
		{"raw all zero", replaceSignature(t, valid.Value, make([]byte, 64))},
		{"raw flipped byte", tamperSignature(t, raw, flipLastByte)},
		{"raw wrong length", replaceSignature(t, valid.Value, make([]byte, 63))},
		{"der prefix with short raw tail", replaceSignature(t, valid.Value, append([]byte{0x30, 0x06}, make([]byte, 6)...))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := verifyToken(t, verifier, provider, testCase.token)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	// The valid token still verifies after every tampered vector, so the
	// refusals above are not a verifier that refuses all ES256 tokens.
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid ES256 token after the tampered cases: %v", err)
	}
}

func TestOIDCKeyRotationWindowAndRevocation(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{CacheTTL: time.Hour})
	verifier := newVerifier(t, provider)
	first, _ := provider.Key("key-1")
	second, err := idfix.NewRSAKey("key-2")
	if err != nil {
		t.Fatalf("NewRSAKey: %v", err)
	}
	if err := provider.AddKey(second); err != nil {
		t.Fatalf("AddKey: %v", err)
	}

	oldToken, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|old", KeyID: "key-1", SigningKey: first, ExpiresAt: provider.Clock().Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("Mint old: %v", err)
	}
	newToken, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|new", KeyID: "key-2", SigningKey: second, ExpiresAt: provider.Clock().Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("Mint new: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, oldToken.Value); err != nil {
		t.Fatalf("old key before rotation: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, newToken.Value); err != nil {
		t.Fatalf("new key after publication: %v", err)
	}

	// Within the verifier's cache window the old key remains usable.
	provider.RetireKey("key-1")
	if _, err := verifyToken(t, verifier, provider, oldToken.Value); err != nil {
		t.Fatalf("old key inside the cache window: %v", err)
	}

	// Past the cache window the removed key is gone and the old token refuses.
	provider.Clock().Advance(2 * time.Hour)
	_, err = verifyToken(t, verifier, provider, oldToken.Value)
	requireFaultClass(t, err, fault.Unauthenticated)
	if _, err := verifyToken(t, verifier, provider, newToken.Value); err != nil {
		t.Fatalf("new key after cache refresh: %v", err)
	}
}

func TestOIDCAlgorithmConfusionRefusals(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	publishedRSA, _ := provider.Key("key-1")
	publicDER, err := publishedRSA.PublicKeyDER()
	if err != nil {
		t.Fatalf("PublicKeyDER: %v", err)
	}
	ecKey, err := idfix.NewECKey("ec-key-1")
	if err != nil {
		t.Fatalf("NewECKey: %v", err)
	}
	if err := provider.AddKey(ecKey); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	ecPublicDER, err := ecKey.PublicKeyDER()
	if err != nil {
		t.Fatalf("EC PublicKeyDER: %v", err)
	}

	cases := []struct {
		name string
		spec idfix.TokenSpec
	}{
		{"alg none", idfix.TokenSpec{Algorithm: "none", KeyID: "key-1"}},
		{"hs256 with rsa public key", idfix.TokenSpec{Algorithm: "HS256", KeyID: "key-1", HMACSecret: publicDER}},
		{"hs256 with ec public key", idfix.TokenSpec{Algorithm: "HS256", KeyID: "ec-key-1", HMACSecret: ecPublicDER}},
		{"rs512 not allowed", idfix.TokenSpec{Algorithm: "RS512", KeyID: "key-1", SigningKey: publishedRSA}},
		{"es256 against rsa key", idfix.TokenSpec{Algorithm: "ES256", KeyID: "key-1", SigningKey: ecKey}},
		{"rs256 against ec key", idfix.TokenSpec{Algorithm: "RS256", KeyID: "ec-key-1", SigningKey: publishedRSA}},
		{"missing kid", idfix.TokenSpec{OmitKeyID: true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token, err := provider.Mint(testCase.spec)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, err = verifyToken(t, verifier, provider, token.Value)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	// Algorithms the configured allowlist does not include are refused even
	// when the header names a published kid; a verifier that accepts an
	// unexpected JWS algorithm must never reach signature verification.
	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint valid: %v", err)
	}
	for _, algorithm := range []string{"EdDSA", "PS256", "ES384", "RS384", ""} {
		t.Run("forged header algorithm "+algorithm, func(t *testing.T) {
			_, err := verifyToken(t, verifier, provider, forgeHeaderAlgorithm(t, valid.Value, algorithm))
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	// Positive control: a valid RS256 token still verifies, so the algorithm
	// table is not passing because the verifier refuses every token.
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the algorithm-confusion cases: %v", err)
	}
}

func TestOIDCVerifierRefusesMalformedTokens(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := tokenParts(t, valid.Value)
	encode := func(value string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(value))
	}

	cases := []struct {
		name  string
		token string
	}{
		{"one segment", parts[0]},
		{"two segments", parts[0] + "." + parts[1]},
		{"four segments", valid.Value + ".extra"},
		{"empty signature", parts[0] + "." + parts[1] + "."},
		{"header not base64", "!!!" + "." + parts[1] + "." + parts[2]},
		{"header not json", encode("not json") + "." + parts[1] + "." + parts[2]},
		{"payload not base64", parts[0] + ".!!!" + "." + parts[2]},
		{"payload not json", parts[0] + "." + encode("not json") + "." + parts[2]},
		{"claims not an object", parts[0] + "." + encode("[]") + "." + parts[2]},
		{"claims null", parts[0] + "." + encode("null") + "." + parts[2]},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := verifyToken(t, verifier, provider, testCase.token)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	// An empty token is a caller error rather than an unverifiable token.
	_, err = verifier.Verify(context.Background(), shellidentity.OIDCRequest{AccessToken: "", Now: provider.Clock().Now()})
	requireFaultClass(t, err, fault.InvalidInput)
	// A zero authoritative time is refused too.
	_, err = verifier.Verify(context.Background(), shellidentity.OIDCRequest{AccessToken: valid.Value, Now: time.Time{}})
	requireFaultClass(t, err, fault.InvalidInput)

	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the malformed cases: %v", err)
	}
}

func TestOIDCVerifierClaimBoundaries(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	now := provider.Clock().Now()

	accepted := []struct {
		name string
		spec idfix.TokenSpec
	}{
		{"expiry one second in the future", idfix.TokenSpec{ExpiresAt: now.Add(time.Second)}},
		{"not-before equal to now", idfix.TokenSpec{NotBefore: new(now)}},
		{"issued-at equal to now", idfix.TokenSpec{IssuedAt: new(now)}},
		{"audience array contains the configured audience", idfix.TokenSpec{Audience: []string{"other-api", provider.Audience()}}},
	}
	for _, testCase := range accepted {
		t.Run("accept "+testCase.name, func(t *testing.T) {
			token, err := provider.Mint(testCase.spec)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}

	refused := []struct {
		name string
		spec idfix.TokenSpec
	}{
		{"expiry equal to now", idfix.TokenSpec{ExpiresAt: now}},
		{"not-before one second in the future", idfix.TokenSpec{NotBefore: new(now.Add(time.Second))}},
		{"issued-at one second in the future", idfix.TokenSpec{IssuedAt: new(now.Add(time.Second))}},
		{"malformed not-before", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims["nbf"] = "soon" }}},
		{"malformed issued-at", idfix.TokenSpec{Mutate: func(claims map[string]any) { claims["iat"] = "soon" }}},
		{"audience array does not contain the configured audience", idfix.TokenSpec{Audience: []string{"other-api"}}},
		{"empty audience array", idfix.TokenSpec{Audience: []string{}}},
		{"audience wrong type", idfix.TokenSpec{Audience: 42}},
	}
	for _, testCase := range refused {
		t.Run("refuse "+testCase.name, func(t *testing.T) {
			token, err := provider.Mint(testCase.spec)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, err = verifyToken(t, verifier, provider, token.Value)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}
}

func TestOIDCVerifierRefusesOversizedToken(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	// The padding makes the signed token exceed the configured 1 MiB token
	// bound while leaving every claim valid, so only the bound can refuse it.
	token, err := provider.Mint(idfix.TokenSpec{Extra: map[string]any{"padding": strings.Repeat("x", 1<<20)}})
	if err != nil {
		t.Fatalf("Mint oversized token: %v", err)
	}
	_, err = verifyToken(t, verifier, provider, token.Value)
	requireFaultClass(t, err, fault.Unauthenticated)

	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the oversized refusal: %v", err)
	}
}

func TestOIDCVerifierIgnoresNonSignatureKeys(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	encryptionKey, err := idfix.NewRSAKey("enc-key-1")
	if err != nil {
		t.Fatalf("NewRSAKey: %v", err)
	}
	encryptionKey.Use = "enc"
	if err := provider.AddKey(encryptionKey); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	verifier := newVerifier(t, provider)
	token, err := provider.Mint(idfix.TokenSpec{Subject: "auth0|human-enc", KeyID: "enc-key-1", SigningKey: encryptionKey})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// A key published for encryption is not a signature key. The verifier must
	// ignore it, so the kid is unknown and triggers the one forced refresh.
	before := provider.JWKSFetchCount()
	_, err = verifyToken(t, verifier, provider, token.Value)
	requireFaultClass(t, err, fault.Unauthenticated)
	if after := provider.JWKSFetchCount(); after != before+2 {
		t.Fatalf("JWKS fetches = %d; want %d (initial + one forced refresh)", after, before+2)
	}

	// The signature key in the same document still verifies.
	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the encryption-key refusal: %v", err)
	}
}

func TestOIDCVerifierRefusesAmbiguousJWKS(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	first, err := idfix.NewRSAKey("duplicate")
	if err != nil {
		t.Fatalf("NewRSAKey: %v", err)
	}
	second, err := idfix.NewRSAKey("duplicate")
	if err != nil {
		t.Fatalf("NewRSAKey: %v", err)
	}
	document, err := idfix.JWKS(first, second)
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	provider.SetRawJWKS(document)

	verifier := newVerifier(t, provider)
	token, err := provider.Mint(idfix.TokenSpec{KeyID: "duplicate", SigningKey: first})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// A duplicated kid is ambiguous, not a last-wins or first-wins key set.
	_, err = verifyToken(t, verifier, provider, token.Value)
	requireFaultClass(t, err, fault.DependencyUnavailable)

	provider.SetRawJWKS(nil)
	valid, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := verifyToken(t, verifier, provider, valid.Value); err != nil {
		t.Fatalf("valid token after the ambiguous-JWKS refusal: %v", err)
	}
}

func TestOIDCVerifierRefusesOversizedJWKS(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	// Valid JSON whose padding pushes the document past the 1 MiB JWKS bound.
	// Without the bound the document parses and only the unknown kid refuses,
	// so the dependency_unavailable class pins the bound itself.
	//
	// Mutation-sweep note: removing only the explicit size comparison is
	// equivalent, because the bounded reader still truncates the body to
	// MaxJWKSBytes+1 and truncated JSON is malformed. Both paths are
	// dependency_unavailable; the comparison exists to name the cause, not to
	// change the class.
	document := []byte(`{"keys":[],"padding":"` + strings.Repeat("x", 1<<20) + `"}`)
	provider.SetRawJWKS(document)

	verifier := newVerifier(t, provider)
	token, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = verifyToken(t, verifier, provider, token.Value)
	requireFaultClass(t, err, fault.DependencyUnavailable)

	provider.SetRawJWKS(nil)
	if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
		t.Fatalf("recovery after an oversized JWKS: %v", err)
	}
}

// TestOIDCVerifierRefusesMalformedJWKSKeys pins that a structurally invalid key
// entry makes the whole document dependency_unavailable instead of silently
// shrinking or weakening the key set.
func TestOIDCVerifierRefusesMalformedJWKSKeys(t *testing.T) {
	cases := []struct {
		name     string
		document string
	}{
		{"zero rsa modulus", `{"keys":[{"kty":"RSA","kid":"bad","n":"AA","e":"AQAB"}]}`},
		{"rsa exponent too small", `{"keys":[{"kty":"RSA","kid":"bad","n":"AQAB","e":"AA"}]}`},
		{"rsa exponent oversized", `{"keys":[{"kty":"RSA","kid":"bad","n":"AQAB","e":"AQABAAAAAA"}]}`},
		{"ec point off curve", `{"keys":[{"kty":"EC","kid":"bad","crv":"P-256","x":"AA","y":"AA"}]}`},
		{"ec wrong curve", `{"keys":[{"kty":"EC","kid":"bad","crv":"P-384","x":"AA","y":"AA"}]}`},
		{"unsupported key type", `{"keys":[{"kty":"oct","kid":"bad","k":"AA"}]}`},
		{"missing key material", `{"keys":[{"kty":"RSA","kid":"bad"}]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := newOIDC(t, idfix.OIDCOptions{})
			provider.SetRawJWKS([]byte(testCase.document))
			verifier := newVerifier(t, provider)
			token, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
			if err != nil {
				t.Fatalf("Mint: %v", err)
			}
			_, err = verifyToken(t, verifier, provider, token.Value)
			requireFaultClass(t, err, fault.DependencyUnavailable)
		})
	}
}

func TestOIDCJWKSFailureFailsClosedAndRecovers(t *testing.T) {
	// Mutation-sweep note: a mutation that installs an empty key set when a
	// JWKS fetch fails is equivalent here, because a failed fetch never
	// replaces the cache and every later Verify refetches; the recovery
	// assertions below therefore cannot distinguish it from the real code.
	provider := newOIDC(t, idfix.OIDCOptions{})
	verifier := newVerifier(t, provider)
	token, err := provider.MintAccessToken("auth0|human-1", fixtureTenant, fixtureProject)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Baseline: the same verifier accepts the token before any injected
	// failure, so a later refusal is attributable to the failure.
	if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
		t.Fatalf("baseline Verify: %v", err)
	}

	t.Run("provider error with an empty cache", func(t *testing.T) {
		provider.SetJWKSFailure(http.StatusInternalServerError)
		_, err := verifyToken(t, verifier, provider, token.Value)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		provider.SetJWKSFailure(0)
		// The SAME verifier must recover; a failed fetch must never poison the
		// cache into a permanent outage of human authentication.
		if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
			t.Fatalf("recovery after a 5xx JWKS failure: %v", err)
		}
	})

	t.Run("malformed document", func(t *testing.T) {
		provider.SetJWKSMalformed(true)
		_, err := verifyToken(t, verifier, provider, token.Value)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		provider.SetJWKSMalformed(false)
		if _, err := verifyToken(t, verifier, provider, token.Value); err != nil {
			t.Fatalf("recovery after a malformed JWKS: %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		provider.SetJWKSDelay(300 * time.Millisecond)
		config := provider.VerifierConfig()
		config.HTTPClient = provider.ClientWithTimeout(100 * time.Millisecond)
		timedVerifier, err := shelloidc.NewVerifier(config)
		if err != nil {
			t.Fatalf("NewVerifier: %v", err)
		}
		_, err = verifyToken(t, timedVerifier, provider, token.Value)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		provider.SetJWKSDelay(0)
		if _, err := verifyToken(t, timedVerifier, provider, token.Value); err != nil {
			t.Fatalf("recovery after a JWKS timeout: %v", err)
		}
	})

	t.Run("forced refresh failure recovers", func(t *testing.T) {
		// A known kid populates the cache; an unknown kid then forces exactly
		// one refetch, which fails. After the provider returns, the same
		// verifier must verify the rotated token instead of staying broken.
		cachedProvider := newOIDC(t, idfix.OIDCOptions{CacheTTL: time.Hour})
		cachedVerifier := newVerifier(t, cachedProvider)
		known, err := cachedProvider.Mint(idfix.TokenSpec{
			KeyID:     "key-1",
			ExpiresAt: cachedProvider.Clock().Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Mint known: %v", err)
		}
		if _, err := verifyToken(t, cachedVerifier, cachedProvider, known.Value); err != nil {
			t.Fatalf("initial Verify: %v", err)
		}
		rotated, err := idfix.NewRSAKey("key-2")
		if err != nil {
			t.Fatalf("NewRSAKey: %v", err)
		}
		unknown, err := cachedProvider.Mint(idfix.TokenSpec{
			KeyID:      "key-2",
			SigningKey: rotated,
			ExpiresAt:  cachedProvider.Clock().Now().Add(24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("Mint rotated: %v", err)
		}
		cachedProvider.SetJWKSFailure(http.StatusServiceUnavailable)
		_, err = verifyToken(t, cachedVerifier, cachedProvider, unknown.Value)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		cachedProvider.SetJWKSFailure(0)
		if err := cachedProvider.AddKey(rotated); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
		if _, err := verifyToken(t, cachedVerifier, cachedProvider, unknown.Value); err != nil {
			t.Fatalf("recovery after a forced-refresh failure: %v", err)
		}
	})

	t.Run("cached key survives a provider failure until the cache expires", func(t *testing.T) {
		cached := newOIDC(t, idfix.OIDCOptions{CacheTTL: time.Hour})
		cachedVerifier := newVerifier(t, cached)
		token, err := cached.Mint(idfix.TokenSpec{Subject: "auth0|human-3", ExpiresAt: cached.Clock().Now().Add(24 * time.Hour)})
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, err := verifyToken(t, cachedVerifier, cached, token.Value); err != nil {
			t.Fatalf("initial Verify: %v", err)
		}
		cached.SetJWKSFailure(http.StatusInternalServerError)
		// Before CacheTTL the cached key set still verifies.
		if _, err := verifyToken(t, cachedVerifier, cached, token.Value); err != nil {
			t.Fatalf("cached Verify before CacheTTL: %v", err)
		}
		cached.Clock().Advance(2 * time.Hour)
		// After CacheTTL the provider failure is surfaced.
		_, err = verifyToken(t, cachedVerifier, cached, token.Value)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		cached.SetJWKSFailure(0)
		// Recovery after cached-key expiry: the same verifier fetches the key
		// set again and verifies.
		if _, err := verifyToken(t, cachedVerifier, cached, token.Value); err != nil {
			t.Fatalf("recovery after cached-key expiry: %v", err)
		}
	})
}

func TestOIDCTokenEndpointAuthorizationCodeAndRefreshRotation(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	claims := map[string]any{
		"sub":                  "auth0|human-9",
		provider.TenantClaim(): fixtureTenant,
		provider.RoleClaim():   []string{"reviewer"},
	}
	if err := provider.RegisterClient("kodactl", "client-secret", claims); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	code, err := provider.IssueAuthorizationCode("kodactl", "https://kodama.example/callback", claims)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	first, err := provider.ExchangeCode(context.Background(), code, "kodactl", "client-secret", "https://kodama.example/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if first.AccessToken == "" || first.RefreshToken == "" || first.ExpiresIn <= 0 {
		t.Fatalf("token response = %+v", first)
	}
	verifier := newVerifier(t, provider)
	principal, err := verifyToken(t, verifier, provider, first.AccessToken)
	if err != nil {
		t.Fatalf("verify access token: %v", err)
	}
	wantID, _ := shelloidc.PrincipalIDFor(provider.Issuer(), "auth0|human-9")
	if principal.ID != wantID {
		t.Fatalf("principal id = %q; want %q", principal.ID, wantID)
	}

	// Advance the fixture clock so the refreshed access token has a different
	// issued-at/expiry (RSA PKCS#1 v1.5 signing is deterministic).
	provider.Clock().Advance(time.Minute)
	second, err := provider.ExchangeRefreshToken(context.Background(), first.RefreshToken, "kodactl", "client-secret")
	if err != nil {
		t.Fatalf("ExchangeRefreshToken: %v", err)
	}
	if second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh did not rotate both tokens")
	}
	if _, err := provider.ExchangeRefreshToken(context.Background(), first.RefreshToken, "kodactl", "client-secret"); err == nil {
		t.Fatal("the consumed refresh token was accepted again")
	}

	for _, request := range provider.Requests() {
		if strings.Contains(request.Path, "client-secret") {
			t.Fatalf("recorded request contains a client secret: %+v", request)
		}
	}
	logs := provider.LogText()
	for _, secret := range []string{"client-secret", first.AccessToken, first.RefreshToken, second.AccessToken} {
		if strings.Contains(logs, secret) {
			t.Fatalf("provider log contains secret material")
		}
	}
}

func TestOIDCTokenEndpointFailureInjection(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	if err := provider.RegisterClient("kodactl", "client-secret", nil); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	provider.SetTokenEndpointStatus(http.StatusServiceUnavailable)
	code, err := provider.IssueAuthorizationCode("kodactl", "", nil)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	_, err = provider.ExchangeCode(context.Background(), code, "kodactl", "client-secret", "")
	if err == nil {
		t.Fatal("the failing token endpoint returned a token")
	}
	requireNoLeakedSecrets(t, "token-endpoint failure", []string{err.Error()},
		secretRule{label: "client secret", value: "client-secret"},
		secretRule{label: "authorization code", value: code},
	)

	// Recovery: the same authorization code is still redeemable after the
	// endpoint returns, and the recovered token verifies.
	provider.SetTokenEndpointStatus(0)
	response, err := provider.ExchangeCode(context.Background(), code, "kodactl", "client-secret", "")
	if err != nil {
		t.Fatalf("recovery after a token-endpoint failure: %v", err)
	}
	if response.AccessToken == "" {
		t.Fatal("recovery exchange returned no access token")
	}
	verifier := newVerifier(t, provider)
	if _, err := verifyToken(t, verifier, provider, response.AccessToken); err != nil {
		t.Fatalf("verify recovered access token: %v", err)
	}
}

// tokenParts splits a compact JWS and fails the test on a malformed value.
func tokenParts(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments; want 3", len(parts))
	}
	return parts
}

// replaceSignature rebuilds a token with an exact signature segment.
func replaceSignature(t *testing.T, token string, signature []byte) string {
	t.Helper()
	parts := tokenParts(t, token)
	return parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// tamperSignature decodes the signature segment, applies mutate and rebuilds
// the token.
func tamperSignature(t *testing.T, token string, mutate func([]byte) []byte) string {
	t.Helper()
	parts := tokenParts(t, token)
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	return replaceSignature(t, token, mutate(signature))
}

func flipLastByte(signature []byte) []byte {
	tampered := append([]byte(nil), signature...)
	if len(tampered) > 0 {
		tampered[len(tampered)-1] ^= 0x01
	}
	return tampered
}

func truncateOneByte(signature []byte) []byte {
	if len(signature) == 0 {
		return signature
	}
	return append([]byte(nil), signature[:len(signature)-1]...)
}

// rawECDSASignature converts the fixture's ASN.1 DER ECDSA signature into the
// fixed-width JOSE R||S form. It fails if the DER encoding is not the expected
// two-integer structure.
func rawECDSASignature(t *testing.T, token string) string {
	t.Helper()
	parts := tokenParts(t, token)
	der, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	var parsed struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &parsed)
	if err != nil || len(rest) != 0 || parsed.R == nil || parsed.S == nil {
		t.Fatalf("signature is not a DER ECDSA pair: err=%v rest=%d", err, len(rest))
	}
	raw := make([]byte, 64)
	parsed.R.FillBytes(raw[:32])
	parsed.S.FillBytes(raw[32:])
	return replaceSignature(t, token, raw)
}

// forgeHeaderAlgorithm rebuilds a token with a different "alg" header while
// keeping the original signature. It is used for algorithm values the fixture
// signer cannot produce; the verifier must reject them before any signature
// check.
func forgeHeaderAlgorithm(t *testing.T, token, algorithm string) string {
	t.Helper()
	parts := tokenParts(t, token)
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var members map[string]any
	if err := json.Unmarshal(header, &members); err != nil {
		t.Fatalf("decode header JSON: %v", err)
	}
	members["alg"] = algorithm
	encoded, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded) + "." + parts[1] + "." + parts[2]
}

func TestOIDCDiscoveryIsJSON(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	response, err := provider.Client().Get(provider.Issuer() + "/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("GET JWKS: %v", err)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("JWKS content type = %q", response.Header.Get("Content-Type"))
	}
	var document struct {
		Keys []map[string]any `json:"keys"`
	}
	decodeJSON(t, readBody(t, response), &document)
	if len(document.Keys) != 1 {
		t.Fatalf("JWKS keys = %d; want 1", len(document.Keys))
	}
	encoded, _ := json.Marshal(document.Keys[0])
	if strings.Contains(string(encoded), "private") {
		t.Fatalf("JWKS exposed private key material: %s", encoded)
	}
}
