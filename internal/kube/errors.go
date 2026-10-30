package kube

import (
	"errors"
	"fmt"
)

// Failure classes of the executor. Callers classify with errors.Is; messages are
// fixed text and never carry credentials, Secret data or foreign identifiers.
var (
	// ErrPolicy: the execution policy is missing, invalid or does not cover the
	// tenant. Dispatch is denied; nothing is created.
	ErrPolicy = errors.New("kube: execution policy denies the dispatch")
	// ErrPolicyDrift: the current policy would produce a different Job than the
	// one the attempt's persisted fingerprint binds. The attempt is never
	// silently replanned.
	ErrPolicyDrift = errors.New("kube: execution policy changed since the attempt was planned")
	// ErrForeignWorkload: an object holds the expected name but is not the
	// attempt's workload (ownership, spec, fingerprint or UID differ). It is
	// neither adopted nor modified.
	ErrForeignWorkload = errors.New("kube: object is not this attempt's workload")
	// ErrNotDispatchable: the committed attempt no longer authorizes a
	// workload (cancelled, revoked, terminal or unknown).
	ErrNotDispatchable = errors.New("kube: the attempt does not authorize a workload")
	// ErrCreateUncertain: a Job create was issued and its outcome is not known;
	// absence is not permission to create another.
	ErrCreateUncertain = errors.New("kube: the Job create outcome is uncertain")
	// ErrCredentialConflict: a registration exists with a credential this
	// executor cannot reproduce. A credential is never regenerated.
	ErrCredentialConflict = errors.New("kube: the admission credential cannot be recovered")
	// ErrPreflight: the tenant namespace does not satisfy the execution baseline.
	ErrPreflight = errors.New("kube: the tenant namespace does not satisfy the execution baseline")
	// ErrUnavailable: a dependency (cluster, store, runtime admission) could not
	// answer. Nothing is concluded from it.
	ErrUnavailable = errors.New("kube: dependency unavailable")
	// ErrNotFound: the attempt does not exist in the scope (authoritative).
	ErrNotFound = errors.New("kube: not found")
	// ErrCleanupUnverified: an object carrying the marks of a resource's companion
	// cannot be proven to be that resource's (its record is lost, so neither a
	// recorded UID nor the committed spec is left to verify it by). It is neither
	// deleted nor assumed gone: removal stays unconfirmed until it is verified or
	// resolved by an operator.
	ErrCleanupUnverified = errors.New("kube: an object of the resource cannot be verified and is left untouched")
	// ErrInvalidTransition: the workload record does not allow the change.
	ErrInvalidTransition = errors.New("kube: invalid workload transition")
)

// diagError is an executor failure: a class, fixed text and an optional cause. Its text is fixed by this
// package (see the contract above), so it may appear in operator logs; the cause is described only by what
// it vouches for itself.
type diagError struct {
	kind  error
	msg   string
	cause error
}

func (e *diagError) Error() string {
	if e.cause == nil {
		return e.kind.Error() + ": " + e.msg
	}
	return e.kind.Error() + ": " + e.msg + " (" + e.cause.Error() + ")"
}

func (e *diagError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// SafeDiagnostic implements diag.Safe.
func (e *diagError) SafeDiagnostic() string { return e.kind.Error() + ": " + e.msg }

func wrap(kind error, format string, args ...any) error {
	return &diagError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

func wrapCause(kind, cause error, format string, args ...any) error {
	return &diagError{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}
