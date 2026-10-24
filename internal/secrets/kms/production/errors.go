package production

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

// Startup failures. Constructors wrap them with a short, credential-free
// explanation; callers test with errors.Is and treat every one as fatal.
var (
	// ErrProviderRequired: no cloud KMS was selected. There is no default and
	// no fallback to the development keyfile or another cloud.
	ErrProviderRequired = errors.New("kms: a production key provider must be selected explicitly")
	// ErrUnsupportedProvider: the selection is not aws, gcp or scaleway
	// (including ephemeral and keyfile selections, which are not production).
	ErrUnsupportedProvider = errors.New("kms: unsupported production key provider")
	// ErrInvalidConfig: the key identity, versions or resources are invalid.
	ErrInvalidConfig = errors.New("kms: invalid production key configuration")
	// ErrCredentials: the selected cloud's client or credential is missing, or
	// a client for another cloud was supplied.
	ErrCredentials = errors.New("kms: workload credentials for the selected provider are missing or ambiguous")
	// ErrStartupProbe: the bounded wrap/unwrap round trip failed.
	ErrStartupProbe = errors.New("kms: startup wrap/unwrap probe failed")
)

// Static, sanitized causes carried by *secrets.KeyError. They never contain a
// provider message, request identifier, credential or key material.
var (
	errProviderDown   = errors.New("key provider unavailable")
	errProviderDenied = errors.New("key provider denied the operation")
	errKeyMissing     = errors.New("key or key version not found at the provider")
	errWrapRejected   = errors.New("wrapped key failed authentication or is malformed")
	errWrapContext    = errors.New("wrap context or key reference is invalid")
	errUnknown        = errors.New("unclassified key provider failure")
)

func keyError(class secrets.KeyErrorClass, op string, cause error) *secrets.KeyError {
	return &secrets.KeyError{Class: class, Op: op, Err: cause}
}

// classify translates a secretkeys failure into the consumer's classes. The
// provider error itself is dropped: only a static cause is kept.
func classify(ctx context.Context, op string, err error) *secrets.KeyError {
	switch {
	case ctx.Err() != nil:
		return keyError(secrets.KeyUnavailable, op, ctx.Err())
	case errors.Is(err, secretkeys.ErrUnavailable), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return keyError(secrets.KeyUnavailable, op, errProviderDown)
	case errors.Is(err, secretkeys.ErrDenied):
		return keyError(secrets.KeyDenied, op, errProviderDenied)
	case errors.Is(err, secretkeys.ErrNotFound):
		return keyError(secrets.KeyNotFound, op, errKeyMissing)
	case errors.Is(err, secretkeys.ErrCiphertext), errors.Is(err, secretkeys.ErrInvalid):
		return keyError(secrets.KeyCorrupt, op, errWrapRejected)
	default:
		return keyError(secrets.KeyUnavailable, op, errUnknown)
	}
}
