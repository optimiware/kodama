package gitauth

import (
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Failure classes. Business refusals a caller maps to the Git service contract
// are *gitv1.ContractError values (a stable code with its HTTP status in
// gitv1.Status). The sentinels below are for the ports and for refusals that
// are not contract codes.
var (
	// ErrUnavailable marks a dependency failure or an unknown commit outcome. It
	// is never access: callers retry or fail closed.
	ErrUnavailable = errors.New("gitauth: unavailable")
	// ErrNotFound is what a port returns for an unknown or foreign object: the
	// use cases turn it into the contract's nondisclosing not_found.
	ErrNotFound = errors.New("gitauth: not found")
	// ErrConflict is what a store returns for a unique-key violation.
	ErrConflict = errors.New("gitauth: unique conflict")
	// ErrSeedConsumed is returned by ConsumeSeed for a seed grant already
	// consumed by another receive. The push authorizer maps it to the push
	// refusal reason gitv1.ReasonSeedConsumed; it is not an HTTP error code.
	ErrSeedConsumed = errors.New("gitauth: seed grant already consumed by another receive")
	// ErrGrantInactive is returned by ConsumeSeed for a grant that is revoked or
	// expired.
	ErrGrantInactive = errors.New("gitauth: grant is revoked or expired")
)

func refuse(code domain.ErrorCode, message string) error {
	return &gitv1.ContractError{Code: code, Message: message}
}

func notFound() error { return refuse(domain.CodeNotFound, "not found") }

func forbidden(message string) error { return refuse(domain.CodeForbidden, message) }

func invalidRequest(format string, args ...any) error {
	return refuse(domain.CodeInvalidRequest, fmt.Sprintf(format, args...))
}

// invalidCredential is the one answer for every refused credential: expired,
// revoked, unknown, foreign, malformed or no longer authorized. No reason is
// disclosed.
func invalidCredential() error {
	return refuse(domain.CodeInvalidCredential, "the credential is not valid")
}

func unavailable(cause error, what string) error {
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, cause)
}

// IsCode reports whether err is a contract error with the given code.
func IsCode(err error, code domain.ErrorCode) bool {
	var ce *gitv1.ContractError
	return errors.As(err, &ce) && ce.Code == code
}
