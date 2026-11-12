//go:build e2e

package harness

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestHarnessCredentialScan proves issued credential text and the configured
// token are detected without echoing either value.
func TestHarnessCredentialScan(t *testing.T) {
	if err := ScanCredentials("build succeeded\nasset main.js 1.2 kB\n", Secret("project-token-value")); err != nil {
		t.Fatalf("clean output was refused: %v", err)
	}
	for _, leaked := range []string{
		"Authorization kodex.abc123",
		"kodsvc.service-token",
		"prefix project-token-value suffix",
	} {
		err := ScanCredentials(leaked, Secret("project-token-value"))
		if err == nil {
			t.Fatalf("credential-shaped output %q was accepted", leaked)
		}
		if strings.Contains(err.Error(), "project-token-value") || strings.Contains(err.Error(), "abc123") {
			t.Fatalf("credential scan error echoed the rejected value: %v", err)
		}
	}
	if err := ScanCredentials("kodex.abc", Secret("short")); err == nil {
		t.Fatal("issued credential prefix was accepted")
	}
}

// TestHarnessCredentialScanHandlesLongAndUnpaddedMaterial proves the scanner
// covers every credential size the codec allows, padded and unpadded, Basic and
// bare, even when many long words precede the leak.
func TestHarnessCredentialScanHandlesLongAndUnpaddedMaterial(t *testing.T) {
	for _, size := range []int{300, 380, 500, 900, api.MaxCredentialBytes} {
		token := Secret(strings.Repeat("a", size))
		basic := base64.StdEncoding.EncodeToString([]byte("kodama:" + token.Reveal()))
		bare := base64.StdEncoding.EncodeToString([]byte(token.Reveal()))
		for _, value := range []string{
			basic,
			strings.TrimRight(basic, "="),
			bare,
			strings.TrimRight(bare, "="),
		} {
			if err := ScanCredentials("Authorization: Basic "+value, token); err == nil {
				t.Fatalf("a %d-byte credential in base64 form was not detected (encoded length %d)", size, len(value))
			}
			if err := ScanCredentials("prefix "+value+" suffix", token); err == nil {
				t.Fatalf("a %d-byte bare base64 credential was not detected", size)
			}
		}
	}

	token := Secret(strings.Repeat("b", 900))
	leak := base64.StdEncoding.EncodeToString([]byte("kodama:" + token.Reveal()))
	var leading strings.Builder
	for range 100 {
		leading.WriteString(strings.Repeat("z", 40))
		leading.WriteString(" ")
	}
	if err := ScanCredentials(leading.String()+"Authorization: Basic "+leak, token); err == nil {
		t.Fatal("a credential after more than 64 long words was not detected")
	}
}

// TestHarnessCredentialScanDecodesBasic proves a Basic-encoded configured token
// or issued credential is detected from credential-shaped output without
// extracting or echoing the material.
func TestHarnessCredentialScanDecodesBasic(t *testing.T) {
	secret := Secret("project-token-value")
	for _, basic := range []string{
		base64.StdEncoding.EncodeToString([]byte("kodama:project-token-value")),
		base64.StdEncoding.EncodeToString([]byte("token:project-token-value")),
		base64.StdEncoding.EncodeToString([]byte("kodex.issued-credential")),
	} {
		err := ScanCredentials("Authorization: Basic "+basic, secret)
		if err == nil {
			t.Fatalf("Basic-encoded credential %q was accepted", basic)
		}
		if strings.Contains(err.Error(), basic) || strings.Contains(err.Error(), secret.Reveal()) {
			t.Fatalf("credential scan error echoed the rejected material: %v", err)
		}
	}
	if err := ScanCredentials("Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("user:unrelated-password")), secret); err != nil {
		t.Fatalf("an unrelated Basic value was refused: %v", err)
	}
}
