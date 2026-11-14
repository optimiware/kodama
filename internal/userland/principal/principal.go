// Package principal derives deterministic Kodama human principal identities
// and canonical OIDC subjects across local, social and federated sign-in
// methods.
package principal

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

const (
	maxGitHubUsernameBytes = 39
	maxEmailBytes          = 320
)

// ForOIDC derives the stable verified human principal id for one
// issuer/subject pair.
//
// The derivation matches internal/shell/integration/oidc.PrincipalIDFor
// byte-for-byte: "oidc-" plus the lowercase hex encoding of the first 16
// SHA-256 bytes of issuer + "\x00" + subject.
func ForOIDC(issuer, subject string) (id.PrincipalID, error) {
	if issuer == "" {
		return "", fault.New(fault.InvalidInput, "OIDC issuer is empty")
	}
	if subject == "" {
		return "", fault.New(fault.InvalidInput, "OIDC subject is empty")
	}
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return id.PrincipalID("oidc-" + hex.EncodeToString(sum[:16])), nil
}

// SubjectForGitHub validates and normalizes a GitHub username and returns its
// canonical subject "github:<lowercase-login>".
func SubjectForGitHub(username string) (string, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" || len(trimmed) > maxGitHubUsernameBytes {
		return "", fault.New(fault.InvalidInput, "GitHub username must be 1..39 characters")
	}
	if trimmed[0] == '-' || trimmed[len(trimmed)-1] == '-' {
		return "", fault.New(fault.InvalidInput, "GitHub username must not start or end with a hyphen")
	}
	for index := 0; index < len(trimmed); index++ {
		ch := trimmed[index]
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= 'A' && ch <= 'Z':
		case ch >= '0' && ch <= '9':
		case ch == '-':
		default:
			return "", fault.New(fault.InvalidInput, "GitHub username contains invalid characters")
		}
	}
	return "github:" + strings.ToLower(trimmed), nil
}

// SubjectForEmail validates and normalizes an email address and returns its
// canonical subject "email:<lowercase-email>".
func SubjectForEmail(email string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" || len(normalized) > maxEmailBytes {
		return "", fault.New(fault.InvalidInput, "email address is empty or too long")
	}
	for index := 0; index < len(normalized); index++ {
		ch := normalized[index]
		if ch <= 0x20 || ch == 0x7f {
			return "", fault.New(fault.InvalidInput, "email address contains whitespace or control characters")
		}
	}
	local, domain, ok := strings.Cut(normalized, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", fault.New(fault.InvalidInput, "email address must have the form user@domain")
	}
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", fault.New(fault.InvalidInput, "email address has a leading or trailing dot")
	}
	return "email:" + normalized, nil
}

// SubjectForLocal validates a local sign-in persona identifier and returns the
// trimmed persona subject. Local personas use the bounded [A-Za-z0-9_-]
// identity alphabet, so they never contain ":" and cannot collide with
// "github:..." or "email:..." subjects.
func SubjectForLocal(persona string) (string, error) {
	trimmed := strings.TrimSpace(persona)
	if _, err := id.ParsePrincipalID(trimmed); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "local persona must be 1..128 [A-Za-z0-9_-] characters", err)
	}
	return trimmed, nil
}
