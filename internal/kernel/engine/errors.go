package engine

// Runtime adapters report an identity they have no resource for either as this
// sentinel or as a fault with class fault.NotFound. Only the sentinel is
// affirmative evidence that the identity was never prepared: a generic NotFound
// can also describe a resource that was prepared (and possibly started) and
// then disappeared, so it is never proof of nonexecution or of stop on its own.

import (
	"errors"

	"github.com/optimiware/kodama/internal/fault"
)

// ErrNotPrepared reports that a runtime has no resource for a stable workload
// identity. It is not uncertainty about a started effect: it means the identity
// was never prepared, so there is nothing to stop or remove.
var ErrNotPrepared = errors.New("engine: workload identity is not prepared")

// isNeverPreparedError reports affirmative evidence that the runtime never
// prepared the identity. It is the only absence error terminal cleanup may
// accept as proof from Observe or from an unproven Remove.
func isNeverPreparedError(err error) bool {
	return err != nil && errors.Is(err, ErrNotPrepared)
}

// isNotFoundError reports a generic typed fault.NotFound that is not the
// affirmative ErrNotPrepared sentinel. It is absence, not proof that the
// command never ran or that a started workload stopped.
func isNotFoundError(err error) bool {
	if err == nil || errors.Is(err, ErrNotPrepared) {
		return false
	}
	class, ok := fault.ClassOf(err)
	return ok && class == fault.NotFound
}

// retryableWorkspaceError reports a workspace port failure that says nothing
// about the workspace itself: the backend was unreachable, a concurrent change
// won, or the outcome is unknown. Such a failure is retried on a later pass and
// is never recorded as a lost workspace.
func retryableWorkspaceError(err error) bool {
	class, ok := fault.ClassOf(err)
	if !ok {
		return false
	}
	switch class {
	case fault.DependencyUnavailable, fault.VersionConflict, fault.UncertainExternalEffect:
		return true
	default:
		return false
	}
}

// isRemovalAbsentError reports that Remove has nothing left to remove. Callers
// may use it only after establishing confirmed stop or never-started proof:
// ErrNotPrepared is always affirmative, while a generic NotFound is accepted
// only because the caller already holds that evidence. Running-action observe
// maps ErrNotPrepared and fault.NotFound to a non-settling AbsentObservation(),
// while terminal cleanup keeps isNeverPreparedError distinct from fault.NotFound.
func isRemovalAbsentError(err error) bool {
	return isNeverPreparedError(err) || isNotFoundError(err)
}
