// Package auth implements PKCE loopback login, credential persistence and the
// `auth login|logout` commands for the CLI.
//
// The CLI treats an access token as an opaque credential: it never parses
// claims, verifies signatures or derives roles. The server verifies issuer,
// audience, signature, expiry and subject, and checks membership on every
// request. Automatic refresh and remote revocation are not implemented:
// expired credentials require an explicit `auth login`, and `auth logout`
// deletes only the local credential.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/cli/root"
)

// Sentinels for credential store failures. They are distinct on purpose: an
// absent credential, an unreachable store, a refused write and an insecure
// destination call for different operator actions.
var (
	ErrCredentialNotFound = errors.New("credential not found")
	ErrStoreUnavailable   = errors.New("credential store unavailable")
	ErrPersistenceFailed  = errors.New("credential could not be persisted")
	ErrInsecureStore      = errors.New("credential store destination is insecure")
	// ErrInteractionRequired means the store could not be used without
	// prompting the user (for example a locked OS keyring) and the invocation
	// forbids prompting. The operation was not attempted.
	ErrInteractionRequired = errors.New("credential store would require user interaction")
)

// StoreError carries one sentinel kind plus operator-safe detail. Cause is kept
// for errors.Is/As only and never rendered, because lower-level messages may
// echo paths or backend internals.
type StoreError struct {
	Kind   error
	Detail string
	Cause  error
}

func (e *StoreError) Error() string {
	if e.Detail == "" {
		return e.Kind.Error()
	}
	return e.Kind.Error() + ": " + e.Detail
}

// Unwrap lets errors.Is match the kind and the cause.
func (e *StoreError) Unwrap() []error {
	if e.Cause == nil {
		return []error{e.Kind}
	}
	return []error{e.Kind, e.Cause}
}

// CLI implements root.Failure.
func (e *StoreError) CLI() (root.ExitCode, string) {
	switch e.Kind {
	case ErrCredentialNotFound:
		return root.ExitAuth, "not_authenticated"
	case ErrInsecureStore:
		return root.ExitFailure, "credential_store_insecure"
	case ErrInteractionRequired:
		return root.ExitUsage, "credential_store_interaction_required"
	case ErrPersistenceFailed:
		return root.ExitFailure, "credential_persistence_failed"
	default:
		return root.ExitFailure, "credential_store_unavailable"
	}
}

func storeErr(kind error, cause error, format string, args ...any) *StoreError {
	return &StoreError{Kind: kind, Detail: fmt.Sprintf(format, args...), Cause: cause}
}

// Credential is a stored login. The access token is opaque. Every textual and
// structured rendering is redacted.
type Credential struct {
	AccessToken string
	TokenType   string
	// Expiry is zero when the provider reported none.
	Expiry     time.Time
	ObtainedAt time.Time
}

func (c Credential) String() string   { return "auth.Credential{redacted}" }
func (c Credential) GoString() string { return "auth.Credential{redacted}" }

// LogValue keeps the token out of structured logs.
func (c Credential) LogValue() slog.Value { return slog.StringValue("redacted") }

// Expired reports whether the credential is past its expiry (with a small
// margin so a token is not used moments before it lapses).
func (c Credential) Expired(now time.Time) bool {
	return !c.Expiry.IsZero() && !now.Add(30*time.Second).Before(c.Expiry)
}

// CredentialStore is the consumer-owned persistence port. key is derived from
// the effective factory and authentication configuration.
//
// A context built with WithNonInteractive forbids prompting: a store whose
// backend might prompt (a locked OS keyring) must fail with
// ErrInteractionRequired before invoking it, unless it can disable prompting.
// Every operation is bounded by ctx.
//
// Load returns ErrCredentialNotFound for an absent credential. Delete of an
// absent credential succeeds. Implementations never fall back to, or
// enumerate, another store.
type CredentialStore interface {
	Load(ctx context.Context, key string) (Credential, error)
	Save(ctx context.Context, key string, c Credential) error
	Delete(ctx context.Context, key string) error
}

type nonInteractiveKey struct{}

// WithNonInteractive returns a context under which credential stores must not
// prompt the user. The credential lookup and logout paths set it from
// --non-interactive.
func WithNonInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonInteractiveKey{}, true)
}

// NonInteractive reports whether ctx forbids prompting.
func NonInteractive(ctx context.Context) bool {
	v, _ := ctx.Value(nonInteractiveKey{}).(bool)
	return v
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return storeErr(ErrInsecureStore, nil, "invalid credential key")
	}
	return nil
}

const storedVersion = 1

type storedCredential struct {
	Version     int       `json:"version"`
	AccessToken string    `json:"accessToken"`
	TokenType   string    `json:"tokenType"`
	Expiry      time.Time `json:"expiry"`
	ObtainedAt  time.Time `json:"obtainedAt"`
}

func encodeCredential(c Credential) ([]byte, error) {
	if c.AccessToken == "" {
		return nil, errors.New("empty access token")
	}
	return json.Marshal(storedCredential{storedVersion, c.AccessToken, c.TokenType, c.Expiry.UTC(), c.ObtainedAt.UTC()})
}

func decodeCredential(b []byte) (Credential, error) {
	var s storedCredential
	if err := json.Unmarshal(b, &s); err != nil {
		return Credential{}, err
	}
	if s.Version != storedVersion || s.AccessToken == "" {
		return Credential{}, errors.New("unsupported or empty credential record")
	}
	exp := s.Expiry
	if exp.IsZero() || exp.Unix() == 0 {
		exp = time.Time{}
	}
	return Credential{AccessToken: s.AccessToken, TokenType: s.TokenType, Expiry: exp, ObtainedAt: s.ObtainedAt}, nil
}
