package oidc

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestVerifierAcceptsValidTokens(t *testing.T) {
	now := testNow()

	t.Run("rs256", func(t *testing.T) {
		key := primaryRSAKey(t)
		verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		principal, err := verifyRequest(verifier, token, now)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if principal.ID != expectedPrincipalID(testIssuer, testSubject) {
			t.Fatalf("principal id = %q; want derived id", principal.ID)
		}
	})

	t.Run("es256", func(t *testing.T) {
		key := mustECKey(t)
		verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), ecJWK("ec-key", key))
		token := makeJWT(t, "ES256", "ec-key", testClaimsMap(now), ecSHA256Signer(key))
		principal, err := verifyRequest(verifier, token, now)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if principal.ID != expectedPrincipalID(testIssuer, testSubject) {
			t.Fatalf("principal id = %q; want derived id", principal.ID)
		}
	})

	t.Run("audience_array", func(t *testing.T) {
		key := primaryRSAKey(t)
		verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		claims["aud"] = []string{"other-api", testAudience}
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		if _, err := verifyRequest(verifier, token, now); err != nil {
			t.Fatalf("Verify with audience array: %v", err)
		}
	})

	t.Run("tenant_scoped", func(t *testing.T) {
		key := primaryRSAKey(t)
		verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		delete(claims, testProjectClaim)
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		principal, err := verifyRequest(verifier, token, now)
		if err != nil {
			t.Fatalf("Verify tenant-scoped token: %v", err)
		}
		want := tenancy.Scope{TenantID: "tenant-a"}
		if principal.Scope != want {
			t.Fatalf("principal scope = %+v; want %+v", principal.Scope, want)
		}
	})
}

func TestVerifierRefusesAlgorithmConfusion(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
	claims := testClaimsMap(now)

	cases := []struct {
		name  string
		token string
	}{
		{"none", makeJWT(t, "none", "key-1", claims, noSigner())},
		{"hs256_with_public_key", makeJWT(t, "HS256", "key-1", claims, hmacSHA256Signer(publicKeyDER(t, key)))},
		{"rs512_not_allowed", makeJWT(t, "RS512", "key-1", claims, rsaSHA256Signer(key))},
		{"missing_kid", makeJWT(t, "RS256", "", claims, rsaSHA256Signer(key))},
		{"es256_token_against_rsa_key", makeJWT(t, "ES256", "key-1", claims, ecSHA256Signer(mustECKey(t)))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := verifyRequest(verifier, testCase.token, now)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}
}

func TestVerifierRefusesBadSignatures(t *testing.T) {
	key := primaryRSAKey(t)
	other := secondaryRSAKey(t)
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))

	t.Run("wrong_key", func(t *testing.T) {
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(other))
		_, err := verifyRequest(verifier, token, now)
		requireFaultClass(t, err, fault.Unauthenticated)
	})

	t.Run("tampered_signature", func(t *testing.T) {
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		parts := strings.Split(token, ".")
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("decoding signature: %v", err)
		}
		signature[0] ^= 0xff
		tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
		_, err = verifyRequest(verifier, tampered, now)
		requireFaultClass(t, err, fault.Unauthenticated)
	})

	t.Run("tampered_payload", func(t *testing.T) {
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		parts := strings.Split(token, ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decoding payload: %v", err)
		}
		payload = []byte(strings.Replace(string(payload), "tenant-a", "tenant-b", 1))
		tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
		_, err = verifyRequest(verifier, tampered, now)
		requireFaultClass(t, err, fault.Unauthenticated)
	})
}

func TestVerifierRefusesClaimMismatches(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong_issuer", func(claims map[string]any) { claims["iss"] = "https://evil.example" }},
		{"wrong_audience", func(claims map[string]any) { claims["aud"] = "other-api" }},
		{"audience_array_without_expected", func(claims map[string]any) { claims["aud"] = []string{"other-api", "another-api"} }},
		{"missing_audience", func(claims map[string]any) { delete(claims, "aud") }},
		{"audience_not_string", func(claims map[string]any) { claims["aud"] = 123 }},
		{"missing_subject", func(claims map[string]any) { delete(claims, "sub") }},
		{"empty_subject", func(claims map[string]any) { claims["sub"] = "" }},
		{"missing_tenant", func(claims map[string]any) { delete(claims, testTenantClaim) }},
		{"invalid_tenant", func(claims map[string]any) { claims[testTenantClaim] = "bad tenant" }},
		{"invalid_project", func(claims map[string]any) { claims[testProjectClaim] = "bad project" }},
		{"missing_exp", func(claims map[string]any) { delete(claims, "exp") }},
		{"malformed_exp", func(claims map[string]any) { claims["exp"] = "soon" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			claims := testClaimsMap(now)
			testCase.mutate(claims)
			token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
			_, err := verifyRequest(verifier, token, now)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}
}

func TestVerifierEnforcesExpiryAtBoundary(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()

	verifyExpiry := func(t *testing.T, skew time.Duration, offset time.Duration) error {
		t.Helper()
		config := defaultTestConfig()
		config.skew = skew
		verifier, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		claims["exp"] = now.Add(offset).Unix()
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		_, err := verifyRequest(verifier, token, now)
		return err
	}

	if err := verifyExpiry(t, 0, 0); err == nil {
		t.Fatal("token at the exact exp boundary was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if err := verifyExpiry(t, 0, time.Second); err != nil {
		t.Fatalf("token expiring in one second was refused: %v", err)
	}
	if err := verifyExpiry(t, 2*time.Second, -time.Second); err != nil {
		t.Fatalf("token inside the configured skew was refused: %v", err)
	}
	if err := verifyExpiry(t, 2*time.Second, -2*time.Second); err == nil {
		t.Fatal("token at exp+skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if err := verifyExpiry(t, 2*time.Second, -3*time.Second); err == nil {
		t.Fatal("token past exp+skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
}

func TestVerifierEnforcesNotBeforeAndIssuedAtSkew(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()

	verifyClaim := func(t *testing.T, claim string, skew time.Duration, offset time.Duration) error {
		t.Helper()
		config := defaultTestConfig()
		config.skew = skew
		verifier, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		claims[claim] = now.Add(offset).Unix()
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		_, err := verifyRequest(verifier, token, now)
		return err
	}

	if err := verifyClaim(t, "nbf", 0, 0); err != nil {
		t.Fatalf("nbf at now was refused: %v", err)
	}
	if err := verifyClaim(t, "nbf", 0, time.Second); err == nil {
		t.Fatal("future nbf outside skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if err := verifyClaim(t, "nbf", 2*time.Second, time.Second); err != nil {
		t.Fatalf("nbf inside skew was refused: %v", err)
	}
	if err := verifyClaim(t, "nbf", 2*time.Second, 3*time.Second); err == nil {
		t.Fatal("nbf past skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}

	if err := verifyClaim(t, "iat", 0, time.Second); err == nil {
		t.Fatal("future iat outside skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if err := verifyClaim(t, "iat", 2*time.Second, time.Second); err != nil {
		t.Fatalf("iat inside skew was refused: %v", err)
	}
	if err := verifyClaim(t, "iat", 2*time.Second, 3*time.Second); err == nil {
		t.Fatal("iat past skew was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}

	t.Run("missing_optional_times", func(t *testing.T) {
		verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		delete(claims, "nbf")
		delete(claims, "iat")
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		if _, err := verifyRequest(verifier, token, now); err != nil {
			t.Fatalf("token without optional nbf/iat was refused: %v", err)
		}
	})
}

func TestVerifierRefusesMalformedAndOversizedTokens(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	verifier, _ := newVerifierWithJWKS(t, defaultTestConfig(), rsaJWK("key-1", key))
	valid := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))

	t.Run("empty_token", func(t *testing.T) {
		_, err := verifyRequest(verifier, "", now)
		requireFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("zero_time", func(t *testing.T) {
		_, err := verifyRequest(verifier, valid, time.Time{})
		requireFaultClass(t, err, fault.InvalidInput)
	})

	malformed := []string{
		"a.b",
		"a.b.c.d",
		"!!!.!!!.!!!",
		base64.RawURLEncoding.EncodeToString([]byte("not-json")) + "." +
			base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+testIssuer+`"}`)) + ".sig",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"key-1"}`)) + "." +
			base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig",
		valid[:len(valid)-1] + "!",
	}
	for index, token := range malformed {
		t.Run(fmt.Sprintf("malformed_%d", index), func(t *testing.T) {
			_, err := verifyRequest(verifier, token, now)
			requireFaultClass(t, err, fault.Unauthenticated)
		})
	}

	t.Run("oversized_token", func(t *testing.T) {
		config := defaultTestConfig()
		config.maxTokenBytes = 64
		bounded, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))
		_, err := verifyRequest(bounded, valid, now)
		requireFaultClass(t, err, fault.Unauthenticated)
	})
}

func TestVerifierConfigValidation(t *testing.T) {
	valid := defaultTestConfig()
	if err := validateConfig(valid); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	verifier, err := buildVerifier(valid)
	if err != nil || verifier == nil {
		t.Fatalf("valid config did not build a verifier: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*testConfig)
	}{
		{"http_issuer", func(config *testConfig) { config.issuer = "http://issuer.example" }},
		{"empty_issuer", func(config *testConfig) { config.issuer = "" }},
		{"empty_audience", func(config *testConfig) { config.audience = "" }},
		{"negative_skew", func(config *testConfig) { config.skew = -time.Second }},
		{"http_jwks", func(config *testConfig) { config.jwksURL = "http://issuer.example/jwks" }},
		{"empty_jwks", func(config *testConfig) { config.jwksURL = "" }},
		{"nil_algorithms", func(config *testConfig) { config.algorithms = nil }},
		{"empty_algorithms", func(config *testConfig) { config.algorithms = []string{} }},
		{"none_algorithm", func(config *testConfig) { config.algorithms = []string{"RS256", "none"} }},
		{"hs_algorithm", func(config *testConfig) { config.algorithms = []string{"HS256"} }},
		{"zero_jwks_bound", func(config *testConfig) { config.maxJWKSBytes = 0 }},
		{"zero_token_bound", func(config *testConfig) { config.maxTokenBytes = 0 }},
		{"negative_cache_ttl", func(config *testConfig) { config.cacheTTL = -time.Second }},
		{"negative_min_refresh_interval", func(config *testConfig) { config.minRefreshInterval = -time.Second }},
		{"nil_http_client", func(config *testConfig) { config.httpClient = nil }},
		{"empty_tenant_claim", func(config *testConfig) { config.tenantClaim = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := defaultTestConfig()
			testCase.mutate(&config)
			if err := validateConfig(config); err == nil {
				t.Fatal("invalid config passed Validate")
			} else {
				requireFaultClass(t, err, fault.InvalidInput)
			}
			built, err := buildVerifier(config)
			if err == nil || built != nil {
				t.Fatalf("invalid config built a verifier: %v", err)
			}
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestVerifierNeverLeaksTokenOrProviderBody(t *testing.T) {
	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	joinedLogs := func() string { return strings.Join(logs, "\n") }

	key := primaryRSAKey(t)
	now := testNow()
	const subjectMarker = "subject-secret-marker"
	const tokenMarker = "raw-token-secret-marker"
	const bodyMarker = "provider-body-secret-marker"

	t.Run("valid_token_is_not_logged", func(t *testing.T) {
		config := defaultTestConfig()
		config.logf = logf
		verifier, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))
		claims := testClaimsMap(now)
		claims["sub"] = "auth0|" + subjectMarker
		token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))
		if _, err := verifyRequest(verifier, token, now); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		requireNoMarker(t, joinedLogs(), tokenMarker, subjectMarker, bodyMarker)
	})

	t.Run("invalid_token_is_not_rendered", func(t *testing.T) {
		config := defaultTestConfig()
		config.logf = logf
		verifier, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))
		_, err := verifyRequest(verifier, tokenMarker, now)
		requireFaultClass(t, err, fault.Unauthenticated)
		requireNoMarker(t, err.Error(), tokenMarker)
		requireNoMarker(t, joinedLogs(), tokenMarker)
	})

	t.Run("provider_body_is_not_rendered", func(t *testing.T) {
		config := defaultTestConfig()
		config.logf = logf
		body := `{"keys":["` + bodyMarker + `"]}`
		transport := &fakeJWKS{handler: staticJSON(body)}
		config.httpClient = &http.Client{Transport: transport}
		verifier := newVerifier(t, config)
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		_, err := verifyRequest(verifier, token, now)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		requireNoMarker(t, err.Error(), bodyMarker)
		requireNoMarker(t, joinedLogs(), bodyMarker)
	})

	t.Run("transport_cause_is_not_rendered", func(t *testing.T) {
		config := defaultTestConfig()
		config.logf = logf
		transport := &fakeJWKS{handler: func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed: %s", bodyMarker)
		}}
		config.httpClient = &http.Client{Transport: transport}
		verifier := newVerifier(t, config)
		token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		_, err := verifyRequest(verifier, token, now)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		requireNoMarker(t, err.Error(), bodyMarker)
		requireNoMarker(t, joinedLogs(), bodyMarker)
	})
}
