//go:build e2e

package harness

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/api"
)

// credentialPrefixes are the issued credential kinds whose literal text must
// never appear in captured workload output.
var credentialPrefixes = []string{"kodex.", "kodsvc."}

// maxCredentialScanBytes bounds the total text one scan examines. The bound is
// on scanned input, not on the number of candidates, so a credential far into a
// large capture is still found.
const maxCredentialScanBytes = 8 << 20

// base64MaterialLimit is the largest encoded credential form worth decoding.
// A 4096-byte credential ("kodama:" + token) encodes to about 5.5 KB; the extra
// room covers a configured username prefix and optional padding.
const base64MaterialLimit = 4*((api.MaxCredentialBytes+128)/3) + 8

// basicMaterialPattern finds a credential-shaped Basic value. The quantifier is
// unbounded because Go's regexp engine caps counted repetition below the
// credential size; the loop below enforces the byte limit.
var (
	basicMaterialPattern = regexp.MustCompile(`(?i)\bbasic\s+([A-Za-z0-9+/=_-]{8,})`)
	base64TokenPattern   = regexp.MustCompile(`[A-Za-z0-9+/=_-]{16,}`)
)

// ScanCredentials reports credential material in captured output without ever
// echoing the rejected value. Configured secrets are compared exactly, as
// issued credential prefixes, and after decoding credential-shaped Basic and
// base64 material (padded or unpadded, standard or URL alphabet).
func ScanCredentials(text string, secrets ...Secret) error {
	if len(text) > maxCredentialScanBytes {
		text = text[:maxCredentialScanBytes]
	}
	for _, prefix := range credentialPrefixes {
		if strings.Contains(text, prefix) {
			return fmt.Errorf("captured output contains issued credential text (%s...)", prefix)
		}
	}
	for _, secret := range secrets {
		if secret.IsSet() && len(secret) >= 8 && strings.Contains(text, secret.Reveal()) {
			return fmt.Errorf("captured output contains the configured credential value")
		}
	}
	for _, match := range basicMaterialPattern.FindAllStringSubmatch(text, -1) {
		if len(match[1]) > base64MaterialLimit {
			continue
		}
		if decodedContainsCredential(match[1], secrets) {
			return fmt.Errorf("captured output contains Basic-encoded credential material")
		}
	}
	for _, candidate := range base64TokenPattern.FindAllString(text, -1) {
		if len(candidate) > base64MaterialLimit {
			continue
		}
		if decodedContainsCredential(candidate, secrets) {
			return fmt.Errorf("captured output contains base64-encoded credential material")
		}
	}
	return nil
}

// decodedContainsCredential decodes one base64-shaped value under the standard
// and URL alphabets, padded or unpadded, and reports whether the decoded text
// carries a credential.
func decodedContainsCredential(value string, secrets []Secret) bool {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err != nil {
			continue
		}
		text := string(decoded)
		for _, prefix := range credentialPrefixes {
			if strings.Contains(text, prefix) {
				return true
			}
		}
		for _, secret := range secrets {
			if secret.IsSet() && len(secret) >= 8 && strings.Contains(text, secret.Reveal()) {
				return true
			}
		}
	}
	return false
}
