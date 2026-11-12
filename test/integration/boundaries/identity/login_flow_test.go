//go:build integration

package identity_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// TestBrowserLoginWritesVersion2Session drives the real kodactl binary through
// the browser authorization-code + PKCE flow against the loopback fixtures. The
// fake browser is a separate OS process on PATH, so the real systemBrowser,
// loopback listener, discovery client, OIDC adapter and session store all run.
func TestBrowserLoginWritesVersion2Session(t *testing.T) {
	harness := newLoginHarness(t)
	record, stdout, stderr := harness.login()
	document := harness.discoveryDocument()

	if record.Version != cli.SessionFileVersion {
		t.Fatalf("session version = %d; want %d", record.Version, cli.SessionFileVersion)
	}
	if record.Kind != "session" || record.Token == "" || record.RefreshToken == "" {
		t.Fatalf("persisted record = %+v; want a bound version 2 session", record)
	}
	if record.Issuer != document.Issuer || record.ClientID != document.ClientID ||
		record.Audience != document.Audience || record.APIURL != document.APIURL {
		t.Fatalf("persisted binding = %+v; want %+v", record, document)
	}
	if record.TokenEndpoint != document.TokenEndpoint || record.RevocationEndpoint != document.RevocationEndpoint {
		t.Fatalf("persisted endpoints = %q/%q; want %q/%q",
			record.TokenEndpoint, record.RevocationEndpoint, document.TokenEndpoint, document.RevocationEndpoint)
	}
	if !strings.Contains(stdout, "logged in; issuer "+document.Issuer) {
		t.Fatalf("stdout = %q; want the login acknowledgement", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q; want empty", stderr)
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
	// The child's environment and command line carry no token either.
	requireNoTokenOutput(t, append([]string{"login"}, harness.environment(nil)...), record.Token, record.RefreshToken)

	directoryInfo, err := os.Stat(harness.sessionDir)
	if err != nil {
		t.Fatalf("Stat session directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("session directory mode = %o; want 700", directoryInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(harness.sessionPath)
	if err != nil {
		t.Fatalf("Stat session file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %o; want 600", fileInfo.Mode().Perm())
	}
	if calls := authorizationCodeCalls(harness.provider); calls != 1 {
		t.Fatalf("authorization-code grants = %d; want 1", calls)
	}
	// The access and refresh tokens may appear only in the session file. The
	// scan covers the session directory, the child TMPDIR and every log.
	requirePersistedSecretsOnly(t, harness.root,
		secretRule{label: "access token", value: record.Token, allowed: harness.sessionPath},
		secretRule{label: "refresh token", value: record.RefreshToken, allowed: harness.sessionPath},
	)

	// Negative control: the tree scan is not vacuous.
	planted := filepath.Join(harness.root, "planted.log")
	if err := os.WriteFile(planted, []byte(record.RefreshToken), 0o600); err != nil {
		t.Fatalf("WriteFile planted: %v", err)
	}
	violations, err := findPersistedSecrets(harness.root, []secretRule{{label: "refresh token", value: record.RefreshToken}})
	if err != nil {
		t.Fatalf("findPersistedSecrets: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("the secret scan did not detect a planted token")
	}
	if err := os.Remove(planted); err != nil {
		t.Fatalf("Remove planted: %v", err)
	}
}

// TestBrowserLoginEnforcesIDTokenAudience proves the OIDC Core 3.1.3.7
// audience rule: the ID token aud must contain the OAuth client_id, not the API
// audience the access token binds. The fixture's hostile modes are exercised
// through the real binary.
func TestBrowserLoginEnforcesIDTokenAudience(t *testing.T) {
	cases := []struct {
		name        string
		mode        idfix.IDTokenAudienceMode
		extra       string
		wantSuccess bool
	}{
		{name: "api audience only", mode: idfix.IDTokenAudienceAPI, wantSuccess: false},
		{name: "another client id", mode: idfix.IDTokenAudienceOther, wantSuccess: false},
		{name: "array contains client id", mode: idfix.IDTokenAudienceArray, extra: "other-aud", wantSuccess: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newLoginHarness(t)
			harness.provider.SetIDTokenAudience(testCase.mode, testCase.extra)
			code, stdout, stderr := harness.run(map[string]string{"DISPLAY": ":0"}, "login")
			if testCase.wantSuccess {
				if code != 0 {
					t.Fatalf("login with a client_id array aud exit = %d; stderr=%q", code, stderr)
				}
				record := harness.requireSession()
				requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
				return
			}
			if code != 1 {
				t.Fatalf("login with %s exit = %d; want a refusal (stdout=%q)", testCase.name, code, stdout)
			}
			if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
				t.Fatalf("a refused audience wrote a session file (stat err %v)", err)
			}
			if strings.Contains(stdout, "logged in") {
				t.Fatalf("a refused audience reported success: %q", stdout)
			}
			if !strings.Contains(stderr, "identity token was refused") {
				t.Fatalf("stderr = %q; want the identity-token refusal", stderr)
			}
			// The token endpoint still issued tokens before verification; no
			// token material may reach the output (JWTs start with eyJ).
			rendered := stdout + "\n" + stderr
			if strings.Contains(rendered, "eyJ") || strings.Contains(rendered, "refresh_token") || strings.Contains(rendered, "access_token") {
				t.Fatal("a refused audience rendered token material")
			}
		})
	}
}

// TestBrowserTamperingIsRefused drives hostile fake browsers. Each case asserts
// the product or the fixture refused the tampered value and no session was
// written, except where the hostile request is proven rejected while the normal
// callback still completes.
func TestBrowserTamperingIsRefused(t *testing.T) {
	cases := []struct {
		name           string
		mode           string
		wantExit       int
		wantSession    bool
		wantAuthStatus int
		wantCallback   int
		wantSecond     int
		wantHostile    int
		wantCodeGrants int
	}{
		{name: "state tamper", mode: "state", wantExit: 1, wantAuthStatus: 302, wantCallback: 200},
		{name: "nonce tamper", mode: "nonce", wantExit: 1, wantAuthStatus: 302, wantCallback: 200, wantCodeGrants: 1},
		{name: "redirect tamper", mode: "redirect", wantExit: 1, wantAuthStatus: 400, wantCallback: 200},
		{name: "second callback", mode: "second_callback", wantExit: 0, wantSession: true, wantAuthStatus: 302, wantCallback: 200, wantSecond: 410, wantCodeGrants: 1},
		{name: "dns rebinding host", mode: "host", wantExit: 0, wantSession: true, wantAuthStatus: 302, wantCallback: 200, wantHostile: 400, wantCodeGrants: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newLoginHarness(t)
			code, stdout, stderr := harness.run(map[string]string{
				"DISPLAY":          ":0",
				fakeBrowserModeEnv: testCase.mode,
			}, "login")
			if code != testCase.wantExit {
				t.Fatalf("exit = %d; want %d (stdout=%q stderr=%q)", code, testCase.wantExit, stdout, stderr)
			}
			observation := harness.readBrowserObservation()
			if observation.AuthorizationStatus != testCase.wantAuthStatus {
				t.Fatalf("authorization status = %d; want %d", observation.AuthorizationStatus, testCase.wantAuthStatus)
			}
			if observation.CallbackStatus != testCase.wantCallback {
				t.Fatalf("callback status = %d; want %d", observation.CallbackStatus, testCase.wantCallback)
			}
			if observation.SecondStatus != testCase.wantSecond {
				t.Fatalf("second callback status = %d; want %d", observation.SecondStatus, testCase.wantSecond)
			}
			if observation.HostileStatus != testCase.wantHostile {
				t.Fatalf("hostile Host status = %d; want %d", observation.HostileStatus, testCase.wantHostile)
			}
			if testCase.wantSession {
				record := harness.requireSession()
				requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
			} else {
				if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
					t.Fatalf("a tampered login wrote a session file (stat err %v)", err)
				}
				if strings.Contains(stdout, "logged in") {
					t.Fatalf("a tampered login reported success: %q", stdout)
				}
			}
			if got := authorizationCodeCalls(harness.provider); got != testCase.wantCodeGrants {
				t.Fatalf("authorization-code grants = %d; want %d", got, testCase.wantCodeGrants)
			}
			// No diagnostic may render the authorization code, nonce or state.
			for _, secret := range []string{observation.Location} {
				if secret != "" && strings.Contains(stderr, secret) {
					t.Fatal("a tampering diagnostic rendered the callback URL")
				}
			}
		})
	}
}

// TestDeviceLoginPendingSlowDownApproved drives the RFC 8628 flow through the
// real binary, an explicit poll plan, the approval fixture and automatic mode
// selection from SSH_CONNECTION with no DISPLAY.
func TestDeviceLoginPendingSlowDownApproved(t *testing.T) {
	harness := newLoginHarness(t)
	harness.provider.SetDeviceInterval(time.Second)
	harness.provider.SetDevicePollPlan([]string{"authorization_pending", "slow_down", "approved"})

	code, stdout, stderr, elapsed := harness.runTimed(map[string]string{
		"SSH_CONNECTION": "10.0.0.1 51515 10.0.0.2 22",
	}, "login")
	if code != 0 {
		t.Fatalf("device login exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "To sign in, open ") || !strings.Contains(stdout, "logged in; issuer ") {
		t.Fatalf("stdout = %q; want device instructions and a login acknowledgement", stdout)
	}
	if elapsed < 7*time.Second {
		t.Fatalf("device login finished in %s; want the pending + slow_down schedule", elapsed)
	}
	if !deviceAuthorizationCalled(harness.provider) {
		t.Fatal("the device authorization endpoint was not called")
	}
	if browserWasLaunched(harness) {
		t.Fatal("automatic selection launched a browser for an SSH session")
	}
	record := harness.requireSession()
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
	requirePersistedSecretsOnly(t, harness.root,
		secretRule{label: "access token", value: record.Token, allowed: harness.sessionPath},
		secretRule{label: "refresh token", value: record.RefreshToken, allowed: harness.sessionPath},
	)
}

// TestDeviceLoginDeniedAndExpired drives the two terminal device results.
func TestDeviceLoginDeniedAndExpired(t *testing.T) {
	cases := []struct {
		name string
		plan []string
		want string
	}{
		{name: "denied", plan: []string{"authorization_pending", "access_denied"}},
		{name: "expired", plan: []string{"expired_token"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newLoginHarness(t)
			harness.provider.SetDeviceInterval(time.Second)
			harness.provider.SetDevicePollPlan(testCase.plan)
			code, stdout, stderr := harness.run(nil, "login", "--device")
			if code != 1 {
				t.Fatalf("device %s exit = %d; want 1 (stdout=%q stderr=%q)", testCase.name, code, stdout, stderr)
			}
			if !strings.Contains(stdout, "To sign in") {
				t.Fatalf("stdout = %q; want the device instructions", stdout)
			}
			if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
				t.Fatalf("a refused device login wrote a session file (stat err %v)", err)
			}
			if stderr == "" {
				t.Fatal("a refused device login rendered no diagnostic")
			}
		})
	}
}

// TestDeviceLoginApprovalHook proves the manual approval gate: the flow stays
// pending until the fixture hook approves it.
func TestDeviceLoginApprovalHook(t *testing.T) {
	harness := newLoginHarness(t)
	harness.provider.SetDeviceInterval(time.Second)
	harness.provider.SetDevicePollPlan([]string{"authorization_pending", "authorization_pending", "approved"})

	waiter := harness.start(nil, "login", "--device")
	approved := waitFor(t, 10*time.Second, func() bool {
		codes := harness.provider.PendingUserCodes()
		if len(codes) == 0 {
			return false
		}
		return harness.provider.ApproveDevice(codes[0])
	})
	if !approved {
		t.Fatal("the device user code was never pending for approval")
	}
	if code, stdout, stderr := waiter(); code != 0 {
		t.Fatalf("approved device login exit = %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	record := harness.requireSession()
	if record.Token == "" || record.RefreshToken == "" {
		t.Fatalf("approved device session = %+v", record)
	}
}

// TestRefreshRotationUnderTheLock runs three real processes that all need a
// refresh. The provider holds the first refresh request until all three children
// have read the stale session (each performs one discovery before refreshing),
// so a lockless Refresh deterministically overlaps and is detected instead of
// relying on process scheduling luck. The cross-process lock makes exactly one
// of them rotate the token.
func TestRefreshRotationUnderTheLock(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	harness.provider.Clock().Advance(time.Minute)
	record.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(record)
	baselineDiscoveries := harness.discovery.RequestCount()

	gate := harness.provider.NewRefreshGate()
	defer gate.Close()
	waiters := make([]func() (int, string, string), 3)
	for index := range waiters {
		waiters[index] = harness.start(nil, "refresh")
	}
	select {
	case <-gate.Arrived():
	case <-time.After(20 * time.Second):
		t.Fatal("no child reached the refresh gate; the overlap could not be established")
	}
	if !waitFor(t, 20*time.Second, func() bool {
		return harness.discovery.RequestCount() >= baselineDiscoveries+len(waiters)
	}) {
		t.Fatalf("discovery requests = %d; want %d so every child read the stale session",
			harness.discovery.RequestCount(), baselineDiscoveries+len(waiters))
	}
	gate.Release()

	for index, wait := range waiters {
		code, stdout, stderr := wait()
		if code != 0 {
			t.Fatalf("refresh %d exit = %d; stderr=%q", index, code, stderr)
		}
		if !strings.Contains(stdout, "session refreshed") {
			t.Fatalf("refresh %d stdout = %q", index, stdout)
		}
	}
	if calls := refreshTokenCalls(harness.provider); calls != 1 {
		t.Fatalf("refresh-token grants = %d; want exactly 1 under the lock", calls)
	}
	rotated := harness.requireSession()
	if rotated.Token == record.Token || rotated.RefreshToken == record.RefreshToken {
		t.Fatalf("rotated session = %+v; want both tokens replaced", rotated)
	}
	requireNoTokenOutput(t, []string{rotated.Token, rotated.RefreshToken}, record.Token, record.RefreshToken)
}

// TestRefreshTokenReuseRevokesTheFamily forces an old refresh token back into
// the session: the provider revokes the family and the product deletes the
// session, so every later refresh needs a new login.
func TestRefreshTokenReuseRevokesTheFamily(t *testing.T) {
	harness := newLoginHarness(t)
	initial, _, _ := harness.login()
	harness.provider.Clock().Advance(time.Minute)

	first := initial
	first.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(first)
	if code, _, stderr := harness.run(nil, "refresh"); code != 0 {
		t.Fatalf("first refresh exit = %d; stderr=%q", code, stderr)
	}
	rotated := harness.requireSession()

	stale := rotated
	stale.RefreshToken = initial.RefreshToken
	stale.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(stale)
	code, stdout, stderr := harness.run(nil, "refresh")
	if code != 1 {
		t.Fatalf("reused refresh exit = %d; want 1 (stdout=%q)", code, stdout)
	}
	if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
		t.Fatalf("the session survived a replayed refresh token (stat err %v)", err)
	}
	if !strings.Contains(stderr, "login required") && !strings.Contains(stderr, "session must be established again") {
		t.Fatalf("stderr = %q; want the login-required refusal", stderr)
	}
	if !harness.provider.RefreshFamilyRevoked(initial.RefreshToken) {
		t.Fatal("the fixture did not revoke the reused refresh-token family")
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, initial.Token, initial.RefreshToken, rotated.Token, rotated.RefreshToken)

	// The rotated token belongs to the revoked family and must also fail.
	rotated.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(rotated)
	if code, _, _ := harness.run(nil, "refresh"); code != 1 {
		t.Fatalf("refresh with the revoked family exit = %d; want 1", code)
	}
	if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
		t.Fatalf("the revoked-family session survived (stat err %v)", err)
	}
}

// TestLogoutRevokesAndDeletes proves the real binary revokes the recorded
// refresh token at the RFC 7009 endpoint and deletes the session file.
func TestLogoutRevokesAndDeletes(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	code, stdout, stderr := harness.run(nil, "logout")
	if code != 0 {
		t.Fatalf("logout exit = %d; stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "logged out") {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
		t.Fatalf("the session file survived logout (stat err %v)", err)
	}
	revocations := harness.provider.Revocations()
	if len(revocations) != 1 {
		t.Fatalf("revocation requests = %d; want 1", len(revocations))
	}
	if revocations[0].Token != record.RefreshToken || revocations[0].ClientID != harness.clientID ||
		revocations[0].TokenTypeHint != "refresh_token" {
		t.Fatalf("revocation record does not name the recorded token/hint: %+v", revocations[0])
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestLogoutRevocationFailureStillDeletes proves a provider failure is visible
// but cannot keep the local session alive.
func TestLogoutRevocationFailureStillDeletes(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	harness.provider.SetRevocationStatus(503)
	code, stdout, stderr := harness.run(nil, "logout")
	if code != 1 {
		t.Fatalf("logout exit = %d; want a visible failure", code)
	}
	if _, err := os.Stat(harness.sessionPath); !os.IsNotExist(err) {
		t.Fatalf("the session survived a failed revocation (stat err %v)", err)
	}
	if !strings.Contains(stderr, "revoked") {
		t.Fatalf("stderr = %q; want the revocation failure", stderr)
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestStatusRendersNoToken proves the status view never carries a token.
func TestStatusRendersNoToken(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	code, stdout, stderr := harness.run(nil, "-output", "json", "status")
	if code != 0 || stderr != "" {
		t.Fatalf("status = %d/%q", code, stderr)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("decode status: %v (%q)", err, stdout)
	}
	if view["issuer"] != record.Issuer || view["refreshable"] != true {
		t.Fatalf("status view = %v", view)
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestHelperReturnsFreshTokenForRegisteredOriginOnly proves the Git credential
// helper performs a real rotation for an expiring session and only answers a
// registered trusted origin.
func TestHelperReturnsFreshTokenForRegisteredOriginOnly(t *testing.T) {
	harness := newLoginHarness(t)
	initial, _, _ := harness.login()
	harness.provider.Clock().Advance(time.Minute)
	near := initial
	near.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(near)

	code, stdout, stderr := harness.run(nil,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shop/repo.git")
	if code != 0 || stderr != "" {
		t.Fatalf("helper = %d/%q", code, stderr)
	}
	fresh := harness.requireSession()
	if fresh.Token == initial.Token {
		t.Fatalf("helper did not obtain a fresh access token: %+v", fresh)
	}
	if want := "username=kodama\npassword=" + fresh.Token + "\n"; stdout != want {
		t.Fatalf("helper stdout = %q; want %q", stdout, want)
	}
	if calls := refreshTokenCalls(harness.provider); calls != 1 {
		t.Fatalf("helper refresh grants = %d; want 1", calls)
	}

	code, stdout, stderr = harness.run(nil,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shopkeeper/repo.git")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("unregistered helper = %d/%q/%q; want an empty success", code, stdout, stderr)
	}
	if calls := refreshTokenCalls(harness.provider); calls != 1 {
		t.Fatalf("an unregistered helper caused %d refresh grants; want 1 total", calls)
	}
	if strings.Contains(stderr, fresh.Token) {
		t.Fatal("the helper rendered the token on stderr")
	}
}

// TestSessionBindingAnotherAPIURLRefused proves a session minted for another
// API URL never refreshes.
func TestSessionBindingAnotherAPIURLRefused(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	record.APIURL = "https://other.example"
	record.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(record)

	code, stdout, stderr := harness.run(nil, "refresh")
	if code != 1 {
		t.Fatalf("foreign-API refresh exit = %d; want 1 (stdout=%q)", code, stdout)
	}
	if calls := refreshTokenCalls(harness.provider); calls != 0 {
		t.Fatalf("a foreign-API session caused %d refresh grants; want 0", calls)
	}
	if stderr == "" {
		t.Fatal("the foreign-API binding refusal rendered no diagnostic")
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestRefreshUsesRecordedTokenEndpoint proves a hostile discovery change after
// login cannot redirect the refresh to a substituted endpoint.
func TestRefreshUsesRecordedTokenEndpoint(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	harness.provider.Clock().Advance(time.Minute)

	hostile := harness.discoveryDocument()
	hostile.TokenEndpoint = harness.provider.TokenEndpoint() + "-evil"
	harness.discovery.SetDocument(hostile)

	record.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(record)
	code, stdout, stderr := harness.run(nil, "refresh")
	if code != 0 {
		t.Fatalf("refresh with a hostile discovery exit = %d; stderr=%q", code, stderr)
	}
	if calls := refreshTokenCalls(harness.provider); calls != 1 {
		t.Fatalf("recorded-endpoint refresh grants = %d; want 1", calls)
	}
	if calls := harness.provider.HostileTokenCalls(); calls != 0 {
		t.Fatalf("the substituted token endpoint received %d requests; want 0", calls)
	}
	rotated := harness.requireSession()
	if rotated.TokenEndpoint != record.TokenEndpoint {
		t.Fatalf("rotated record endpoint = %q; want the recorded %q", rotated.TokenEndpoint, record.TokenEndpoint)
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestDiscoveryDownFailsClosed proves a dead discovery route fails a login and
// an expiring refresh without ever sending a token.
func TestDiscoveryDownFailsClosed(t *testing.T) {
	harness := newLoginHarness(t)
	record, _, _ := harness.login()
	record.ExpiresAt = time.Now().Add(30 * time.Second)
	harness.writeSession(record)
	before, err := os.ReadFile(harness.sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}

	harness.discovery.SetStatus(503)
	code, stdout, stderr := harness.run(nil, "refresh")
	if code != 1 {
		t.Fatalf("refresh with discovery down exit = %d; want 1", code)
	}
	if !strings.Contains(stderr, "login required") {
		t.Fatalf("stderr = %q; want the login-required refusal", stderr)
	}
	if calls := refreshTokenCalls(harness.provider); calls != 0 {
		t.Fatalf("a discovery-outage refresh sent %d tokens", calls)
	}
	after, err := os.ReadFile(harness.sessionPath)
	if err != nil {
		t.Fatalf("ReadFile session after outage: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("a discovery outage rewrote the session")
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)

	code, stdout, stderr = harness.run(map[string]string{"DISPLAY": ":0"}, "login")
	if code != 1 || strings.Contains(stdout, "logged in") {
		t.Fatalf("login with discovery down = %d/%q; want a refusal", code, stdout)
	}
	if !strings.Contains(stderr, "login configuration is unavailable") {
		t.Fatalf("stderr = %q; want the unavailable-login-configuration refusal", stderr)
	}
	requireNoTokenOutput(t, []string{stdout, stderr}, record.Token, record.RefreshToken)
}

// TestLegacySessionIsUnusableForLogin proves a version 1 seeded session neither
// authenticates the helper nor refreshes, and a real login replaces it.
func TestLegacySessionIsUnusableForLogin(t *testing.T) {
	harness := newLoginHarness(t)
	legacy := cli.SessionRecord{Kind: "session", Token: "legacy-seeded-token", ExpiresAt: time.Now().Add(time.Hour)}
	harness.writeSession(legacy)

	code, stdout, stderr := harness.run(nil,
		"-trusted-origin", "https://git.example.com/shop",
		"helper", "get", "https://git.example.com/shop/repo.git")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("legacy helper = %d/%q/%q; want an empty refusal", code, stdout, stderr)
	}
	code, stdout, _ = harness.run(nil, "refresh")
	if code != 1 {
		t.Fatalf("legacy refresh exit = %d; want 1 (stdout=%q)", code, stdout)
	}

	record, loginOut, loginErr := harness.login()
	if record.Version != cli.SessionFileVersion || record.Token == legacy.Token {
		t.Fatalf("login did not replace the legacy session: %+v", record)
	}
	requireNoTokenOutput(t, []string{loginOut, loginErr}, legacy.Token)
}

// deviceAuthorizationCalled reports whether the fixture served a device
// authorization request.
func deviceAuthorizationCalled(provider *idfix.Provider) bool {
	for _, request := range provider.Requests() {
		if request.Path == "/oauth/device" {
			return true
		}
	}
	return false
}

// browserWasLaunched reports whether the fake browser wrote an observation.
func browserWasLaunched(harness *loginHarness) bool {
	_, err := os.Stat(harness.browserLog)
	return err == nil
}
