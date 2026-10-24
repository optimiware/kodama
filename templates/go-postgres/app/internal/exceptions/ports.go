package exceptions

import (
	"context"
	"time"

	"example.invalid/inventory/internal/tenancy"
)

// Store is the persistence port consumed by Service. Every method is bound to
// one tenant through Access; an implementation must make cross-tenant rows
// unreachable, not merely filtered by the caller.
type Store interface {
	Create(ctx context.Context, a tenancy.Access, e Exception) error
	Get(ctx context.Context, a tenancy.Access, id ID) (Exception, error)
	List(ctx context.Context, a tenancy.Access, q ListQuery) (Page, error)
	History(ctx context.Context, a tenancy.Access, id ID) ([]AuditEntry, error)

	// Submit atomically locks the exception, resolves an idempotency replay or
	// conflict, applies decide, and commits the new state, the durable intent,
	// the idempotency record and the audit entry together or not at all.
	Submit(ctx context.Context, a tenancy.Access, id ID, req ResolveRequest, decide func(*Exception) (Intent, error)) (Receipt, error)

	// Claim leases one intent that is queued, whose dispatch lease expired, or
	// whose unknown outcome is due for reconciliation. It returns nil when
	// there is nothing to do.
	Claim(ctx context.Context, a tenancy.Access, owner string, lease time.Duration, now time.Time, maxAttempts int) (*Claim, error)

	// Observe atomically records an observation of the ERP outcome: intent
	// status, exception transition and audit entry.
	Observe(ctx context.Context, a tenancy.Access, obs Observation) (Exception, error)
}

// ERP is the legacy-system port consumed by this module. The ERP must apply an
// operation key at most once and return the original outcome on replay; a
// different payload under an existing key is rejected as a conflict.
type ERP interface {
	// Apply sends the adjustment. A returned error means the outcome is
	// unknown (the request may or may not have taken effect).
	Apply(ctx context.Context, tenant tenancy.TenantID, op Operation) (Outcome, error)
	// Lookup reads the outcome of an earlier operation by key.
	Lookup(ctx context.Context, tenant tenancy.TenantID, key string) (Outcome, error)
}

// Operation is the ERP-bound form of an Intent.
type Operation struct {
	Key              string
	ERPRef           string
	AdjustedQuantity int64
	Reason           string
	PayloadHash      string
}

// OutcomeStatus is the ERP's answer.
type OutcomeStatus string

// Outcome statuses.
const (
	OutcomeAcknowledged OutcomeStatus = "acknowledged"
	OutcomeRejected     OutcomeStatus = "rejected"
	OutcomeNotFound     OutcomeStatus = "not_found"
)

// Outcome is an established ERP answer.
type Outcome struct {
	Status    OutcomeStatus
	Reference string
	Reason    string
}

// ListQuery pages exceptions by (created_at, id).
type ListQuery struct {
	State State
	Limit int
	After *Position
}

// Position is the continuation key of a list.
type Position struct {
	CreatedAt time.Time
	ID        ID
}

// Page is one bounded page. Next is nil on the last page.
type Page struct {
	Items []Exception
	Next  *Position
}

// Receipt is the result of a resolution command.
type Receipt struct {
	Exception Exception
	Replayed  bool
}

// Claim is a leased intent.
type Claim struct {
	Intent Intent
	Owner  string
	// Reconcile is true when an earlier attempt may have reached the ERP, so
	// the outcome must be looked up before any further write.
	Reconcile bool
}

// ObservationKind classifies what the worker learned.
type ObservationKind string

// Observation kinds.
const (
	ObservedAcknowledged ObservationKind = "acknowledged"
	ObservedRejected     ObservationKind = "rejected"
	ObservedUnknown      ObservationKind = "unknown"
)

// Observation is recorded atomically with its exception transition.
type Observation struct {
	IntentID     string
	OperationKey string
	Owner        string
	Kind         ObservationKind
	Reference    string
	Detail       string
	// RetryAt schedules the next reconciliation of an unknown outcome. Zero
	// means no further automatic attempt: the outcome stays visibly unknown.
	RetryAt time.Time
	At      time.Time
}

// ApplyObservation performs the domain transition of an observation. Stores
// call it inside their transaction so every adapter shares one rule.
func ApplyObservation(e *Exception, o Observation) error {
	switch o.Kind {
	case ObservedAcknowledged:
		return e.Acknowledge(o.OperationKey, o.Reference, o.At)
	case ObservedRejected:
		return e.Reject(o.OperationKey, o.Detail, o.At)
	case ObservedUnknown:
		return e.MarkUnknown(o.OperationKey, o.Detail, o.At)
	}
	return ErrInvalid
}

// AuditEntry is one append-only history record.
type AuditEntry struct {
	Seq         int64
	ExceptionID ID
	Actor       string
	Action      string
	Detail      string
	At          time.Time
}
