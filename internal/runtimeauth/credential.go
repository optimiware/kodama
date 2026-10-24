package runtimeauth

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

// Digest is a SHA-256 value. Credentials and nonces are persisted only as
// digests, so a database read or backup never yields a redeemable credential.
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
		return d, fail(ErrInvalidRequest, "a digest is %d bytes", len(d))
	}
	copy(d[:], b)
	return d, nil
}

// credentialPattern equals the AdmissionCredential syntax of the runtime
// contract (contracts/runtime/v1 values.go).
var credentialPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,512}$`)

const (
	credentialPrefix = "adm_"
	credentialBytes  = 32
	credentialDomain = "kodama-runtime-admission-v1\x00"
	nonceDomain      = "kodama-runtime-nonce-v1\x00"
)

// Credential is a one-time admission credential. It prints as a redaction in
// every formatter, logger and encoder; the only way to read it is Reveal,
// which the trusted executor calls once to place it in the Job.
type Credential struct{ value string }

// GenerateCredential draws 256 random bits from r (crypto/rand when nil).
// Generation is separate from registration so the executor can keep the
// credential and retry registration after a lost response.
func GenerateCredential(r io.Reader) (Credential, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, credentialBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return Credential{}, failWith(ErrUnavailable, err, "no randomness available")
	}
	return Credential{value: credentialPrefix + base64.RawURLEncoding.EncodeToString(b)}, nil
}

// ParseCredential accepts a presented credential that has the contract syntax.
func ParseCredential(s string) (Credential, error) {
	if !credentialPattern.MatchString(s) {
		return Credential{}, fail(ErrInvalidCredential, "the admission credential is malformed")
	}
	return Credential{value: s}, nil
}

// Reveal returns the secret value. Never log it.
func (c Credential) Reveal() string { return c.value }

// Digest is the persisted form.
func (c Credential) Digest() Digest { return sha256.Sum256([]byte(credentialDomain + c.value)) }

func (Credential) String() string               { return "[redacted admission credential]" }
func (Credential) GoString() string             { return "runtimeauth.Credential{[redacted]}" }
func (Credential) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Credential) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (c Credential) IsZero() bool               { return c.value == "" }

// NonceDigest hashes a client nonce for comparison and storage.
func NonceDigest(nonce string) Digest { return sha256.Sum256([]byte(nonceDomain + nonce)) }

// SessionID identifies a runtime session: the durable record a token refers
// to. It is not a secret on its own; the signed token is.
type SessionID string

var sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9_-]{22,43}$`)

// Validate checks the syntax.
func (s SessionID) Validate() error {
	if !sessionPattern.MatchString(string(s)) {
		return fail(ErrInvalidCredential, "the session reference is malformed")
	}
	return nil
}

// NewSessionID draws 128 random bits.
func NewSessionID(r io.Reader) (SessionID, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", failWith(ErrUnavailable, err, "no randomness available")
	}
	return SessionID("ses_" + base64.RawURLEncoding.EncodeToString(b)), nil
}
