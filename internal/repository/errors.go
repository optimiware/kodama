package repository

import (
	"errors"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Sentinel errors. Use errors.Is; adapters translate them with ContractCode.
// None of the messages carries credentials, repository contents or foreign
// identifiers.
var (
	// ErrNotFound covers an absent repository and one owned by another tenant or
	// project; callers must not distinguish them (GR-05).
	ErrNotFound = errors.New("repository: not found")
	// ErrDenied is an authorization refusal.
	ErrDenied = errors.New("repository: access denied")
	// ErrVersionConflict: a stale expected version (If-Match).
	ErrVersionConflict = errors.New("repository: version conflict")
	// ErrNameTaken: the name is used by a different repository of the project.
	ErrNameTaken = errors.New("repository: name is already used in the project")
	// ErrNotReady: the repository has not finished provisioning.
	ErrNotReady = errors.New("repository: not ready")
	// ErrArchived: the repository is archived.
	ErrArchived = errors.New("repository: archived")
	// ErrHostingMismatch: the operation applies to an internally hosted
	// repository only (an external record is a read-only mapping).
	ErrHostingMismatch = errors.New("repository: hosting does not support this operation")
	// ErrUnavailable wraps a dependency failure; the operation may be retried.
	ErrUnavailable = errors.New("repository: dependency unavailable")
	// ErrNotConfigured: a required adapter was not injected (fail closed).
	ErrNotConfigured = errors.New("repository: required capability is not configured")
	// ErrInvalidConfiguration: a constructor option is missing or invalid.
	ErrInvalidConfiguration = errors.New("repository: invalid configuration")
	// ErrInvalidValue is returned for malformed names, branches and identifiers;
	// it is the shared domain sentinel so one errors.Is covers every value.
	ErrInvalidValue = domain.ErrInvalidValue
)

// ContractCode maps an error of this package to the stable code of the Git
// service contract (contracts/git/v1). ok is false for an unclassified error,
// which an adapter renders as an internal failure.
func ContractCode(err error) (code domain.ErrorCode, ok bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrNotFound):
		return domain.CodeNotFound, true
	case errors.Is(err, ErrDenied):
		return domain.CodeForbidden, true
	case errors.Is(err, ErrVersionConflict):
		return domain.CodeVersionConflict, true
	case errors.Is(err, ErrNameTaken):
		return gitv1.CodeRepositoryNameTaken, true
	case errors.Is(err, ErrNotReady):
		return gitv1.CodeRepositoryNotReady, true
	case errors.Is(err, ErrArchived):
		return gitv1.CodeRepositoryArchived, true
	case errors.Is(err, ErrHostingMismatch):
		return gitv1.CodeHostingMismatch, true
	case errors.Is(err, ErrUnavailable):
		return domain.CodeUnavailable, true
	case errors.Is(err, ErrInvalidValue):
		return domain.CodeInvalidRequest, true
	}
	return "", false
}
