package request

import "context"

// ActivationPort applies one complete request-activation commit unit.
type ActivationPort interface {
	Activate(context.Context, ActivationUnit) (ActivationResult, error)
}

// ResolvePort applies one complete request-resolution commit unit.
//
// The adapter looks up the receipt FIRST, under the unit's scope and receipt
// key. An equal-content hit of a committed receipt returns the original result
// and applies nothing, so a replay unit validated by
// ResolveUnit.ValidateReplay may carry a zero Commit. Changed content is
// refused with changed_idempotency_content. Only a receipt miss reaches first
// settlement: the adapter then applies ResolveUnit.Validate, which requires
// the Kernel answer reduction (ResolveUnit.Commit) and refuses a zero Commit.
// A miss with a zero Commit is therefore refused rather than applied.
//
// On first settlement the adapter rechecks the expected versions, the current
// assignment authority and the authoritative deadline, and persists the
// reduction atomically with the decision, receipt, audit, event publication
// intent and post-commit effects.
type ResolvePort interface {
	Resolve(context.Context, ResolveUnit) (ResolveResult, error)
}
