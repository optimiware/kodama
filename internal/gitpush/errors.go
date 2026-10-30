package gitpush

import (
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Failure classes. Business refusals a caller maps to the Git service contract
// are *gitv1.ContractError values (a stable code with its HTTP status in
// gitv1.Status). The sentinels below are for the ports.
var (
	// ErrUnavailable marks a dependency failure or an unknown commit outcome. It
	// is never access and never an outcome: callers retry or fail closed.
	ErrUnavailable = errors.New("gitpush: unavailable")
	// ErrNotFound is what a port returns for an unknown or foreign object; the
	// use cases turn it into the contract's nondisclosing not_found.
	ErrNotFound = errors.New("gitpush: not found")
	// ErrDenied is an authorization refusal of a read.
	ErrDenied = errors.New("gitpush: access denied")
	// ErrJournalUnreadable is what a Worker adapter wraps when the worker
	// affirmatively answered that its ref journal is unreadable (damaged). Only
	// that answer, and only after the evidence timeout, settles an entry unknown
	// (journal_unreadable); any other failure of a worker call (a timeout, a
	// reset, no worker, a refusal) is a transport failure that settles nothing.
	ErrJournalUnreadable = errors.New("gitpush: the worker's ref journal is unreadable")
	// ErrConflict is what a store returns for a unique-key violation (a repeated
	// receive or push identifier).
	ErrConflict = errors.New("gitpush: unique conflict")
	// ErrNotConfigured: a required collaborator was not injected (fail closed).
	ErrNotConfigured = errors.New("gitpush: required capability is not configured")
)

func refuse(code domain.ErrorCode, format string, args ...any) error {
	return &gitv1.ContractError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func notFound() error { return refuse(domain.CodeNotFound, "not found") }

func unavailable(cause error, what string) error {
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, cause)
}

// IsCode reports whether err is a contract error with the given code.
func IsCode(err error, code domain.ErrorCode) bool {
	var ce *gitv1.ContractError
	return errors.As(err, &ce) && ce.Code == code
}
