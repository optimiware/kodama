package oidc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestPrincipalIDForIsDeterministicAndScoped pins the exact derivation:
// "oidc-" plus lowercase hex of the first 16 SHA-256 bytes of
// issuer + "\x00" + subject. The separator makes the encoding injective.
func TestPrincipalIDForIsDeterministicAndScoped(t *testing.T) {
	issuer := testIssuer
	subject := testSubject

	got, err := derivedPrincipalID(issuer, subject)
	if err != nil {
		t.Fatalf("PrincipalIDFor(%q, %q): %v", issuer, subject, err)
	}
	want := expectedPrincipalID(issuer, subject)
	if got != want {
		t.Fatalf("PrincipalIDFor = %q; want %q", got, want)
	}
	if _, err := id.ParsePrincipalID(string(got)); err != nil {
		t.Fatalf("derived principal id does not parse: %v", err)
	}
	if !strings.HasPrefix(string(got), "oidc-") {
		t.Fatalf("derived principal id %q lacks the oidc- prefix", got)
	}
	if len(got) != len("oidc-")+32 {
		t.Fatalf("derived principal id length = %d; want %d", len(got), len("oidc-")+32)
	}
	for _, character := range string(got)[len("oidc-"):] {
		if !strings.ContainsRune("0123456789abcdef", character) {
			t.Fatalf("derived principal id %q is not lowercase hex", got)
		}
	}

	again, err := derivedPrincipalID(issuer, subject)
	if err != nil || again != got {
		t.Fatalf("same input produced %q/%v; want %q", again, err, got)
	}

	otherIssuer, err := derivedPrincipalID("https://other.example", subject)
	if err != nil {
		t.Fatalf("PrincipalIDFor other issuer: %v", err)
	}
	if otherIssuer == got {
		t.Fatal("different issuers produced the same principal id")
	}
	otherSubject, err := derivedPrincipalID(issuer, "auth0|other")
	if err != nil {
		t.Fatalf("PrincipalIDFor other subject: %v", err)
	}
	if otherSubject == got {
		t.Fatal("different subjects produced the same principal id")
	}

	// The NUL separator prevents ("ab", "c") colliding with ("a", "bc").
	left, err := derivedPrincipalID("ab", "c")
	if err != nil {
		t.Fatalf("PrincipalIDFor ab/c: %v", err)
	}
	right, err := derivedPrincipalID("a", "bc")
	if err != nil {
		t.Fatalf("PrincipalIDFor a/bc: %v", err)
	}
	if left == right {
		t.Fatal("issuer/subject concatenation without an unambiguous separator collided")
	}

	if _, err := derivedPrincipalID("", subject); err == nil {
		t.Fatal("empty issuer accepted")
	} else {
		requireFaultClass(t, err, fault.InvalidInput)
	}
	if _, err := derivedPrincipalID(issuer, ""); err == nil {
		t.Fatal("empty subject accepted")
	} else {
		requireFaultClass(t, err, fault.InvalidInput)
	}
}

// TestPrincipalIDForProperty walks deterministic issuer/subject pairs and
// checks determinism, uniqueness, parseability and boundedness.
func TestPrincipalIDForProperty(t *testing.T) {
	seen := make(map[id.PrincipalID]string)
	for index := range 256 {
		issuer := fmt.Sprintf("https://issuer-%d.example", index%16)
		subject := fmt.Sprintf("auth0|user-%d", index)
		got, err := derivedPrincipalID(issuer, subject)
		if err != nil {
			t.Fatalf("PrincipalIDFor(%q, %q): %v", issuer, subject, err)
		}
		if _, err := id.ParsePrincipalID(string(got)); err != nil {
			t.Fatalf("derived id %q does not parse: %v", got, err)
		}
		key := issuer + "\x00" + subject
		if previous, ok := seen[got]; ok && previous != key {
			t.Fatalf("collision: %q and %q both produced %q", previous, key, got)
		}
		seen[got] = key

		again, err := derivedPrincipalID(issuer, subject)
		if err != nil || again != got {
			t.Fatalf("non-deterministic derivation for %q: %q/%v vs %q", key, again, err, got)
		}
	}
	if len(seen) != 256 {
		t.Fatalf("distinct derived ids = %d; want 256", len(seen))
	}
}

// TestVerifierDerivesPrincipalAndIgnoresPrincipalClaims proves the verifier
// never trusts a principal-id or role claim carried by the token.
func TestVerifierDerivesPrincipalAndIgnoresPrincipalClaims(t *testing.T) {
	key := primaryRSAKey(t)
	config := defaultTestConfig()
	verifier, _ := newVerifierWithJWKS(t, config, rsaJWK("key-1", key))

	now := testNow()
	claims := testClaimsMap(now)
	claims["principal_id"] = "human-admin"
	claims["roles"] = []string{"admin"}
	token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))

	principal, err := verifyRequest(verifier, token, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	wantID := expectedPrincipalID(testIssuer, testSubject)
	if principal.ID != wantID {
		t.Fatalf("principal id = %q; want derived %q", principal.ID, wantID)
	}
	if principal.ID == "human-admin" {
		t.Fatal("verifier trusted the token principal_id claim")
	}
	if principal.Kind != identity.KindHuman {
		t.Fatalf("principal kind = %q; want %q", principal.Kind, identity.KindHuman)
	}
	wantScope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-1"}
	if principal.Scope != wantScope {
		t.Fatalf("principal scope = %+v; want %+v", principal.Scope, wantScope)
	}
	if !principal.Enabled {
		t.Fatal("verified principal is disabled")
	}
	if len(principal.Roles) != 0 {
		t.Fatalf("verified principal carries token roles %v; current roles must be read separately", principal.Roles)
	}
	if principal.Version != 0 {
		t.Fatalf("verified principal version = %d; want zero", principal.Version)
	}
	if principal.ExecutionID != "" {
		t.Fatalf("verified principal execution id = %q; want empty", principal.ExecutionID)
	}
	if err := principal.Validate(); err != nil {
		t.Fatalf("verified principal is structurally invalid: %v", err)
	}
}
