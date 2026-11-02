package gitworker

import (
	"errors"
	"fmt"
)

// Failure classes. Callers classify with errors.Is; messages are fixed text and
// never carry credentials, identity material or foreign object contents.
var (
	// ErrForeign: an object holds one of the tenant's names but is not provably
	// ours (labels, tenant, namespace UID, owner chain or finalizers differ). It is
	// neither adopted nor changed; an operator resolves the name collision.
	ErrForeign = errors.New("gitworker: an object with a reserved name is not this tenant's")
	// ErrDrift: an object is ours but differs in a field this package cannot change
	// in place (a claim, a Service, the namespace, the ServiceAccount). Nothing is
	// changed.
	ErrDrift = errors.New("gitworker: an owned object differs from its specification and cannot be corrected in place")
	// ErrStorage: the storage class cannot give the tenant a retained,
	// single-Pod volume, or the volume is not retained. Nothing is created.
	ErrStorage = errors.New("gitworker: the storage cannot provide a retained single-Pod volume")
	// ErrIsolation: the namespace holds something that would widen the worker's
	// boundary (a foreign NetworkPolicy or Pod). The worker is reported not ready.
	ErrIsolation = errors.New("gitworker: the namespace holds an object outside the worker's isolation")
	// ErrIdentity: the worker identity material is missing, expired, bound to
	// another tenant or contains a private key.
	ErrIdentity = errors.New("gitworker: the worker identity material is not acceptable")
	// ErrUnavailable: a dependency (the cluster, the identity provider) could not
	// answer, or a mutation's outcome is unknown. Nothing is concluded; the next
	// pass reads the objects back.
	ErrUnavailable = errors.New("gitworker: dependency unavailable")
	// ErrRetention: offboarding is refused because retention does not allow the
	// tenant's volume to be deleted yet, or could not be established.
	ErrRetention = errors.New("gitworker: the retention policy does not allow deleting the tenant's volume")
	// ErrWorkerUnavailable: the tenant has no reconciled, ready worker (not yet
	// provisioned, being replaced, not ready or its state is stale). Retryable.
	ErrWorkerUnavailable = errors.New("gitworker: the tenant's worker is not available")
)

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

// SafeDiagnostic implements diag.Safe: the class and the fixed text only.
func (e *diagError) SafeDiagnostic() string { return e.kind.Error() + ": " + e.msg }

func wrap(kind error, format string, args ...any) error {
	return &diagError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

func wrapCause(kind, cause error, format string, args ...any) error {
	return &diagError{kind: kind, msg: fmt.Sprintf(format, args...), cause: cause}
}
