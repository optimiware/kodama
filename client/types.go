// Package client declares the Userland REST client contract: credential
// sourcing, retry policy and the transport boundary. It also provides the
// concrete HTTPTransport and the mounted-token FileCredentialSource.
//
// The package depends on the public schema package and neutral values only and
// never imports Kernel or Shell internals. The contract declarations never
// perform a request; HTTPTransport does, over an injected http.RoundTripper.
package client

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/api"
)

// CredentialKind names the source of one bearer credential.
type CredentialKind string

const (
	// KindSession is a human session credential.
	KindSession CredentialKind = "session"
	// KindExecution is a mounted execution credential.
	KindExecution CredentialKind = "execution"
	// KindService is a service-account credential.
	KindService CredentialKind = "service"
)

// credentialKinds is the declaration order.
var credentialKinds = []CredentialKind{KindSession, KindExecution, KindService}

// Valid reports whether the kind is one of the declared kinds.
func (kind CredentialKind) Valid() bool {
	for _, candidate := range credentialKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// Credential is one current bearer credential value. Token is never rendered by
// String, GoString or the default struct verbs; it exists only to authenticate.
type Credential struct {
	Kind      CredentialKind
	Token     string
	ExpiresAt time.Time
}

// Validate checks the source kind, the bounded non-empty token and the expiry.
// An opaque mounted execution credential may omit its expiry because the token
// file never carries one; a human session or service credential must carry an
// explicit expiry. No expiry is ever fabricated for the unknown case.
func (credential Credential) Validate() error {
	if !credential.Kind.Valid() {
		return fmt.Errorf("unknown credential kind %q", credential.Kind)
	}
	if credential.Token == "" {
		return fmt.Errorf("credential token is empty")
	}
	if len(credential.Token) > api.MaxCredentialBytes {
		return fmt.Errorf("credential token is %d bytes; at most %d are allowed", len(credential.Token), api.MaxCredentialBytes)
	}
	if credential.ExpiresAt.IsZero() && credential.Kind != KindExecution {
		return fmt.Errorf("credential expiry is zero")
	}
	return nil
}

// Usable reports whether the credential is valid and current at now. An
// unknown-expiry mounted execution credential is current; any explicit expiry
// equal to now is expired.
func (credential Credential) Usable(now time.Time) bool {
	if credential.Validate() != nil {
		return false
	}
	if credential.ExpiresAt.IsZero() {
		return credential.Kind == KindExecution
	}
	return now.Before(credential.ExpiresAt)
}

// String renders a redacted credential summary that never contains the token.
func (credential Credential) String() string {
	return fmt.Sprintf("Credential{Kind:%s, ExpiresAt:%s}", credential.Kind, credential.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString renders a redacted %#v representation that never contains the token.
func (credential Credential) GoString() string {
	return fmt.Sprintf("client.Credential{Kind:%q, Token:<redacted>, ExpiresAt:%s}", credential.Kind, credential.ExpiresAt.UTC().Format(time.RFC3339))
}

// RetryPolicy is the bounded retry schedule of one client.
type RetryPolicy struct {
	MaxAttempts int           `json:"max_attempts"`
	BaseDelay   time.Duration `json:"base_delay"`
	MaxDelay    time.Duration `json:"max_delay"`
}

// MaxRetryAttempts bounds the retry policy attempt count.
const MaxRetryAttempts = 8

// Validate checks the attempt count and delay bounds.
func (policy RetryPolicy) Validate() error {
	if policy.MaxAttempts < 1 || policy.MaxAttempts > MaxRetryAttempts {
		return fmt.Errorf("retry attempts %d is outside 1..%d", policy.MaxAttempts, MaxRetryAttempts)
	}
	if policy.BaseDelay <= 0 {
		return fmt.Errorf("retry base delay must be positive")
	}
	if policy.MaxDelay < policy.BaseDelay {
		return fmt.Errorf("retry max delay must not be shorter than the base delay")
	}
	return nil
}
