package engine

// Cleanup retry policy is installation policy, not persisted execution state.
// The Kernel enforces the attempt bound and keeps exhausted cleanup visible as
// pending work with its last actionable error; it never treats exhaustion as a
// successful cleanup and never releases a workspace whose stop is unconfirmed.
// Durable backoff scheduling belongs to the installation/scan cadence: the
// engine never sleeps, and Scan is the authoritative reschedule mechanism.

// DefaultCleanupMaxAttempts is the cleanup attempt bound used when an
// installation supplies no policy. A zero or negative MaxAttempts always
// selects this bound; there is no unlimited retry mode.
const DefaultCleanupMaxAttempts = 5

// CleanupPolicy bounds cleanup retries for one execution. MaxAttempts is the
// number of failed cleanup attempts after which the engine stops issuing
// cleanup effects and leaves the record pending with the last actionable error.
// Backoff scheduling remains installation/scan cadence, not engine behavior.
type CleanupPolicy struct {
	// MaxAttempts is the failed-attempt count at which retries stop. Zero or a
	// negative value selects DefaultCleanupMaxAttempts.
	MaxAttempts int
}

func (p CleanupPolicy) normalized() CleanupPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultCleanupMaxAttempts
	}
	return p
}

// SetCleanupPolicy replaces the installation cleanup retry policy. It is safe to
// call while Reconcile or Scan is running; the next cleanup decision observes
// the new bound. The policy is process configuration, not persisted execution
// state, so a restarted installation re-applies it.
func (e *Engine) SetCleanupPolicy(policy CleanupPolicy) {
	e.policyMu.Lock()
	e.policy = policy.normalized()
	e.policyMu.Unlock()
}

// cleanupPolicy returns the effective policy, normalizing a zero value so the
// engine always has a finite bound.
func (e *Engine) cleanupPolicy() CleanupPolicy {
	e.policyMu.RLock()
	policy := e.policy
	e.policyMu.RUnlock()
	return policy.normalized()
}
