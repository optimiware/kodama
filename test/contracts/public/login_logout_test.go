// Logout vectors.
//
// Logout shares the session lock, revokes the refresh token at the RFC 7009
// revocation endpoint, and always deletes the local session file. A revocation
// failure is reported as a dependency failure without leaking the token.
package publiccontract

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
)

func logoutDeps(fixture *loginFixture) cli.LogoutDeps {
	return cli.LogoutDeps{
		APIURL:   fixture.discovery.APIURL,
		Issuer:   fixture.discovery.Issuer,
		ClientID: fixture.discovery.ClientID,
		Audience: fixture.discovery.Audience,
		OIDC:     fixture.oidc,
		Lock:     fixture.lock,
		Store:    fixture.store,
	}
}

// TestLogoutRevokesAndDeletes pins the successful logout path.
func TestLogoutRevokesAndDeletes(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true

	if err := cli.Logout(context.Background(), logoutDeps(fixture)); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	revocations := fixture.oidc.revokedTokens()
	if len(revocations) != 1 {
		t.Fatalf("revocation endpoint called %d times; want 1", len(revocations))
	}
	request := revocations[0]
	if request.Token != "RT-CANARY" || request.ClientID != fixture.discovery.ClientID ||
		request.TokenTypeHint != "refresh_token" {
		t.Errorf("revocation request = %+v", request)
	}
	if request.Endpoint != fixture.discovery.RevocationEndpoint {
		t.Errorf("revocation endpoint = %q; want the recorded %q", request.Endpoint, fixture.discovery.RevocationEndpoint)
	}
	if _, present := fixture.store.current(); present {
		t.Error("Logout left the session file in place")
	}
	acquisitions, releases := fixture.lock.counts()
	if acquisitions != 1 || releases != 1 {
		t.Errorf("lock acquisitions/releases = %d/%d; want 1/1", acquisitions, releases)
	}
}

// TestLogoutRevocationFailureStillDeletes pins the required partial-failure
// behavior: revocation is reported, the local file is still deleted and the
// token never appears in the error.
func TestLogoutRevocationFailureStillDeletes(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.oidc.revokeFn = func(ctx context.Context, request cli.RevocationRequest) error {
		return errors.New("revocation endpoint rejected " + request.Token)
	}

	err := cli.Logout(context.Background(), logoutDeps(fixture))
	if err == nil {
		t.Fatal("Logout hid a revocation failure")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Errorf("revocation failure class = %v; want dependency unavailable", class)
	}
	if _, present := fixture.store.current(); present {
		t.Error("Logout kept the local session after a revocation failure")
	}
	if fixture.store.deleteCount() != 1 {
		t.Errorf("session deleted %d times; want 1", fixture.store.deleteCount())
	}
	assertNoLoginCanaries(t, []string{"RT-CANARY"}, err.Error())
}

// TestLogoutDeleteFailure pins that a failed local delete is reported.
func TestLogoutDeleteFailure(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	fixture.store.deleteErr = errors.New("permission denied")

	if err := cli.Logout(context.Background(), logoutDeps(fixture)); err == nil {
		t.Fatal("Logout hid a failed local delete")
	}
}

// TestLogoutLegacySession pins that a legacy session has no refresh token to
// revoke but is still deleted.
func TestLogoutLegacySession(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	record := loginRecord(now)
	record.Version = 1
	record.RefreshToken = ""
	fixture.store.record = record
	fixture.store.present = true

	if err := cli.Logout(context.Background(), logoutDeps(fixture)); err != nil {
		t.Fatalf("Logout(legacy): %v", err)
	}
	if revocations := fixture.oidc.revokedTokens(); len(revocations) != 0 {
		t.Errorf("legacy logout revoked %d tokens; want 0", len(revocations))
	}
	if _, present := fixture.store.current(); present {
		t.Error("legacy logout left the session file in place")
	}
}

// TestLogoutMissingSession pins that logging out without a session is a no-op
// success.
func TestLogoutMissingSession(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	if err := cli.Logout(context.Background(), logoutDeps(fixture)); err != nil {
		t.Fatalf("Logout(missing): %v", err)
	}
	if revocations := fixture.oidc.revokedTokens(); len(revocations) != 0 {
		t.Errorf("missing logout revoked %d tokens", len(revocations))
	}
}

// TestIDTokenVerification pins the browser-flow ID-token checks: signature,
// issuer, audience, expiry and nonce.
func TestIDTokenVerification(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	key := loginTestKey(t)
	keys := loginJWKS(t, key)
	expectations := cli.IDTokenExpectations{
		Issuer:   "https://auth.example.com",
		Audience: "kodama-api",
		Nonce:    "nonce-value",
		Now:      now,
	}
	claims := func(mutate func(map[string]any)) map[string]any {
		values := map[string]any{
			"iss":   expectations.Issuer,
			"aud":   expectations.Audience,
			"exp":   now.Add(time.Hour).Unix(),
			"nonce": expectations.Nonce,
			"sub":   "user-1",
		}
		if mutate != nil {
			mutate(values)
		}
		return values
	}

	valid := loginSignIDToken(t, key, claims(nil))
	if err := cli.VerifyIDToken(valid, keys, expectations); err != nil {
		t.Fatalf("valid ID token refused: %v", err)
	}

	otherKey := loginGenerateKey(t)
	tamperedParts := strings.Split(valid, ".")
	tamperedPayload := []byte(tamperedParts[1])
	tamperedPayload[0] ^= 0x01
	tamperedParts[1] = string(tamperedPayload)
	cases := map[string]string{
		"wrong issuer":   loginSignIDToken(t, key, claims(func(values map[string]any) { values["iss"] = "https://other.example.com" })),
		"wrong audience": loginSignIDToken(t, key, claims(func(values map[string]any) { values["aud"] = "other-api" })),
		"expired":        loginSignIDToken(t, key, claims(func(values map[string]any) { values["exp"] = now.Add(-time.Minute).Unix() })),
		"wrong nonce":    loginSignIDToken(t, key, claims(func(values map[string]any) { values["nonce"] = "other-nonce" })),
		"foreign key":    loginSignIDToken(t, otherKey, claims(nil)),
		"malformed":      "not.a.jwt",
		"tampered":       strings.Join(tamperedParts, "."),
	}
	for name, token := range cases {
		if err := cli.VerifyIDToken(token, keys, expectations); err == nil {
			t.Errorf("VerifyIDToken accepted a %s token", name)
		}
	}
}

// TestLogoutChangedBindingRefused pins that a foreign-bound refresh token is
// never sent to the recorded revocation endpoint, while the local file is still
// discarded.
func TestLogoutChangedBindingRefused(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	deps := logoutDeps(fixture)
	deps.Issuer = "https://other.example.com"
	if err := cli.Logout(context.Background(), deps); err == nil {
		t.Fatal("Logout hid a changed session binding")
	}
	if revocations := fixture.oidc.revokedTokens(); len(revocations) != 0 {
		t.Errorf("foreign-bound logout revoked %d tokens", len(revocations))
	}
	if _, present := fixture.store.current(); present {
		t.Error("foreign-bound logout left the local session file in place")
	}
}

// TestLogoutCanceledContextStillDeletes pins that a revocation failure caused
// by the caller's dead context does not skip the local delete.
func TestLogoutCanceledContextStillDeletes(t *testing.T) {
	now := time.Date(2026, 2, 3, 5, 5, 6, 0, time.UTC)
	fixture := newLoginFixture(now)
	fixture.store.record = loginRecord(now)
	fixture.store.present = true
	ctx, cancel := context.WithCancel(context.Background())
	fixture.oidc.revokeFn = func(callCtx context.Context, request cli.RevocationRequest) error {
		cancel()
		return callCtx.Err()
	}
	err := cli.Logout(ctx, logoutDeps(fixture))
	if err == nil {
		t.Fatal("Logout hid the canceled revocation")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Errorf("canceled revocation class = %v; want dependency unavailable", class)
	}
	if _, present := fixture.store.current(); present {
		t.Error("canceled revocation left the session file in place")
	}
	if fixture.store.deleteCount() != 1 {
		t.Errorf("session deleted %d times; want 1", fixture.store.deleteCount())
	}
	assertNoLoginCanaries(t, []string{"RT-CANARY"}, err.Error())
}
