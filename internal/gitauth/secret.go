package gitauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"regexp"
)

// Digest is a SHA-256 value. Credentials are persisted only as digests, so a
// database read or a backup never yields a usable credential.
type Digest [sha256.Size]byte

// Hex returns the lowercase hexadecimal form.
func (d Digest) Hex() string { return hex.EncodeToString(d[:]) }

// Equal compares in constant time.
func (d Digest) Equal(o Digest) bool { return subtle.ConstantTimeCompare(d[:], o[:]) == 1 }

// IsZero reports an unset digest.
func (d Digest) IsZero() bool { return d == Digest{} }

// DigestFromBytes converts a stored 32-byte value.
func DigestFromBytes(b []byte) (Digest, error) {
	var d Digest
	if len(b) != len(d) {
		return d, invalidRequest("a digest is %d bytes", len(d))
	}
	copy(d[:], b)
	return d, nil
}

const (
	secretPrefix = "gitc_"
	// SecretBytes is the entropy of a credential: 256 bits.
	SecretBytes  = 32
	secretDomain = "kodama-git-credential-v1\x00"
)

// secretPattern is a subset of the contract's Credential syntax
// (gitv1.Patterns["Credential"]): the prefix and exactly the base64url encoding
// of SecretBytes bytes. A JWT (a forwarding assertion or a worker identity)
// contains dots and can never match.
var secretPattern = regexp.MustCompile(`^` + secretPrefix + `[A-Za-z0-9_-]{43}$`)

// Secret is an opaque Git credential. It prints as a redaction in every
// formatter, logger and encoder; the only way to read it is Reveal, which the
// delivery path calls once.
type Secret struct{ value string }

// GenerateSecret draws SecretBytes random bytes from r (crypto/rand when nil).
func GenerateSecret(r io.Reader) (Secret, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, SecretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return Secret{}, unavailable(err, "no randomness available")
	}
	return Secret{value: secretPrefix + base64.RawURLEncoding.EncodeToString(b)}, nil
}

// ParseSecret accepts a presented credential that has the credential syntax.
// Anything else, a signed token included, is invalid_credential.
func ParseSecret(s string) (Secret, error) {
	if !secretPattern.MatchString(s) {
		return Secret{}, invalidCredential()
	}
	return Secret{value: s}, nil
}

// Reveal returns the secret value. Never log it.
func (s Secret) Reveal() string { return s.value }

// Digest is the persisted form.
func (s Secret) Digest() Digest { return sha256.Sum256([]byte(secretDomain + s.value)) }

// IsZero reports an unset secret.
func (s Secret) IsZero() bool { return s.value == "" }

func (Secret) String() string               { return "[redacted git credential]" }
func (Secret) GoString() string             { return "gitauth.Secret{[redacted]}" }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
