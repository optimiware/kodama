// Pure login primitives.
//
// PKCE, state/nonce generation and the device polling schedule are pure
// functions over injected randomness and a duration. They are pinned here by
// golden vectors so the flows never depend on wall-clock timing.
package publiccontract

import (
	"encoding/base64"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestPKCEChallengeGolden pins RFC 7636 appendix B: S256 over the ASCII
// verifier, base64url without padding.
func TestPKCEChallengeGolden(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := cli.PKCEChallenge(verifier); got != challenge {
		t.Fatalf("PKCEChallenge = %q; want %q", got, challenge)
	}
}

// TestPKCEVerifierBounds pins the RFC 7636 verifier length and unreserved
// charset rules.
func TestPKCEVerifierBounds(t *testing.T) {
	if err := cli.ValidatePKCEVerifier("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); err != nil {
		t.Fatalf("valid verifier refused: %v", err)
	}
	valid43 := strings.Repeat("a", 43)
	valid128 := strings.Repeat("A", 128)
	for _, verifier := range []string{valid43, valid128} {
		if err := cli.ValidatePKCEVerifier(verifier); err != nil {
			t.Errorf("verifier of %d characters refused: %v", len(verifier), err)
		}
	}
	refused := []string{
		"",
		strings.Repeat("a", 42),
		strings.Repeat("a", 129),
		strings.Repeat("a", 43) + " ",
		"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjX!",
		"dBjftJeZ4CVP+mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"dBjftJeZ4CVP/mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"dBjftJeZ4CVP=mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
	}
	for _, verifier := range refused {
		if err := cli.ValidatePKCEVerifier(verifier); err == nil {
			t.Errorf("ValidatePKCEVerifier accepted %q", verifier)
		}
	}
}

// TestGeneratedSecretsEntropy pins that the verifier, state and nonce draw at
// least 256 bits from the injected random source and encode without padding.
func TestGeneratedSecretsEntropy(t *testing.T) {
	random := &loginFakeRandom{}
	verifier, err := cli.NewPKCEVerifier(random)
	if err != nil {
		t.Fatalf("NewPKCEVerifier: %v", err)
	}
	if err := cli.ValidatePKCEVerifier(verifier); err != nil {
		t.Errorf("generated verifier is invalid: %v", err)
	}
	if len(verifier) != 43 {
		t.Errorf("generated verifier length = %d; want 43", len(verifier))
	}

	for name, generate := range map[string]func(cli.Random) (string, error){
		"state": cli.NewState,
		"nonce": cli.NewNonce,
	} {
		generated, err := generate(random)
		if err != nil {
			t.Fatalf("generate %s: %v", name, err)
		}
		if strings.ContainsAny(generated, "=+/") {
			t.Errorf("%s %q is not unpadded base64url", name, generated)
		}
		raw, err := base64.RawURLEncoding.DecodeString(generated)
		if err != nil {
			t.Errorf("%s %q does not decode as base64url: %v", name, generated, err)
			continue
		}
		if len(raw) < 32 {
			t.Errorf("%s carries %d bytes of entropy; want at least 32", name, len(raw))
		}
	}

	requests := random.requested()
	if len(requests) != 3 {
		t.Fatalf("random source was read %d times; want 3 (verifier, state, nonce)", len(requests))
	}
	for index, requested := range requests {
		if requested < 32 {
			t.Errorf("random read %d requested %d bytes; want at least 32", index, requested)
		}
	}

	failing := &loginFakeRandom{err: errors.New("random source unavailable")}
	if _, err := cli.NewPKCEVerifier(failing); err == nil {
		t.Error("NewPKCEVerifier accepted a failing random source")
	}
	if _, err := cli.NewState(failing); err == nil {
		t.Error("NewState accepted a failing random source")
	}
	if _, err := cli.NewNonce(failing); err == nil {
		t.Error("NewNonce accepted a failing random source")
	}
}

// TestNextPollInterval pins the RFC 8628 device polling schedule.
func TestNextPollInterval(t *testing.T) {
	const five = 5 * time.Second
	cases := []struct {
		name      string
		current   time.Duration
		errorCode string
		wantNext  time.Duration
		terminal  bool
		wantError bool
	}{
		{name: "default interval", current: 0, errorCode: "authorization_pending", wantNext: five},
		{name: "pending keeps interval", current: five, errorCode: "authorization_pending", wantNext: five},
		{name: "slow down adds five", current: five, errorCode: "slow_down", wantNext: 10 * time.Second},
		{name: "slow down accumulates", current: 10 * time.Second, errorCode: "slow_down", wantNext: 15 * time.Second},
		{name: "access denied terminal", current: five, errorCode: "access_denied", terminal: true, wantError: true},
		{name: "expired token terminal", current: five, errorCode: "expired_token", terminal: true, wantError: true},
		{name: "unknown code terminal", current: five, errorCode: "invalid_request", terminal: true, wantError: true},
		{name: "empty code terminal", current: five, errorCode: "", terminal: true, wantError: true},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			next, terminal, err := cli.NextPollInterval(testCase.current, testCase.errorCode)
			if terminal != testCase.terminal {
				t.Errorf("terminal = %v; want %v", terminal, testCase.terminal)
			}
			if (err != nil) != testCase.wantError {
				t.Errorf("error = %v; want error=%v", err, testCase.wantError)
			}
			if !testCase.terminal && next != testCase.wantNext {
				t.Errorf("next = %s; want %s", next, testCase.wantNext)
			}
			if !testCase.terminal && next <= 0 {
				t.Errorf("next = %s; want a positive interval", next)
			}
		})
	}

	// The increment saturates instead of overflowing a negative duration.
	nearMax := time.Duration(math.MaxInt64) - time.Second
	next, terminal, err := cli.NextPollInterval(nearMax, "slow_down")
	if err != nil || terminal {
		t.Fatalf("overflow case = (%s, %v, %v); want a non-terminal interval", next, terminal, err)
	}
	if next != time.Duration(math.MaxInt64) {
		t.Errorf("overflow cap = %s; want %s", next, time.Duration(math.MaxInt64))
	}
	if next < nearMax {
		t.Errorf("slow_down moved the interval backwards: %s -> %s", nearMax, next)
	}
}
