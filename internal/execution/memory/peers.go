package memory

import (
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// The peer seams below are the in-memory counterparts of the peer-module
// transaction calls of the PostgreSQL store. Every method receives the caller's
// transaction: a peer failure rolls the whole call back, and a peer write is
// visible to the rest of the call. A peer returns execution's classified errors
// (ErrNotFound, ErrBusy, ErrStale, ErrAdmissionRefused, ...) so the store and
// the use cases treat both backends alike.

// Workspaces is the workspace peer: the single writer slot of a workspace and
// the run-bound retained-source intent.
type Workspaces interface {
	// ReserveWriter reserves the workspace's writer slot for the attempt. A held
	// or fenced slot is ErrBusy and a moved tip is ErrStale; reserving again for
	// the same attempt succeeds.
	ReserveWriter(tx memdb.Tx, at *execution.Attempt) error
	// SettleWriter ends the writer reservation with the attempt's outcome. An
	// attempt that is not the writer succeeds; an unsettled publication is
	// ErrBusy.
	SettleWriter(tx memdb.Tx, at *execution.Attempt, outcome execution.WriterOutcome, source string) error
	// RetainSource records the run-bound retained-source intent (pin).
	RetainSource(tx memdb.Tx, run *execution.Run, pinID string) error
}

// InternalWorkspaces is the optional peer of a work item's internally hosted
// workspace (runtime context v3): the writer slot is reserved at the workspace
// version the writer gate was checked at, in the claim's transaction.
type InternalWorkspaces interface {
	ReserveInternalWriter(tx memdb.Tx, at *execution.Attempt, observed domain.Version) error
}

// SecretPins is the secrets peer: exact-version consumer pins of a run.
type SecretPins interface {
	// RegisterPins pins the bindings for the run. A missing or revoked version is
	// ErrAdmissionRefused.
	RegisterPins(tx memdb.Tx, scope domain.Scope, run domain.RunID, bindings []runtimev1.SecretBinding) error
	// ReleasePins ends the run's pins; repeating it releases nothing more.
	ReleasePins(tx memdb.Tx, scope domain.Scope, run domain.RunID) error
}

// EventAppender is the transaction-local append of the event outbox. It must
// deduplicate on the envelope ID: replaying a transaction never publishes twice.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) error
}

// WorkLock is the work module's admission lock: nil error means the item is
// actionable and not cancelled; ErrWorkCancelled, ErrNotActionable or
// ErrNotFound otherwise.
type WorkLock func(tx memdb.Tx, scope domain.Scope, id domain.WorkItemID) (execution.WorkAdmission, error)

// AuthorityBinder binds current-authorization checks to one open transaction.
type AuthorityBinder func(tx memdb.Tx) execution.TxAuthority

// GrantRevoker ends every runtime grant of an attempt inside the caller's
// transaction. It is idempotent. It is optional: a store without it revokes
// no grants, as the PostgreSQL component store.
type GrantRevoker func(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, now time.Time) error

// GrantFencer is the transaction-joined check of a runtime grant (the memory
// counterpart of the SQL fence): it reads the grant inside the caller's
// transaction and answers execution.ErrAttemptRevoked or ErrResourceRevoked when
// the grant is revoked, missing, or not the claim's session and generation. It is
// optional on a store, but a store without it refuses to record a begin or a
// preparation receipt (execution.ErrNotConfigured), never skips the check.
type GrantFencer interface {
	AttemptGrantLive(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim) error
	ResourceGrantLive(tx memdb.Tx, scope domain.Scope, run domain.RunID, key string, claim execution.GrantClaim) error
}

// Peers are the peer joins of a Store. Every one except Grants is required: a
// transition without its audit/outbox records, or a dispatch without authority,
// workspace and secret bindings, must not be constructible.
type Peers struct {
	Authority  AuthorityBinder
	Work       WorkLock
	Workspaces Workspaces
	Secrets    SecretPins
	Events     EventAppender
	Grants     GrantRevoker
	// GitGrants ends every Git grant of an attempt inside the caller's
	// transaction, for each attempt whose authority the transaction ended (the
	// gitauth memory adapter's RevokeAttempt). Optional: a store without it
	// revokes no Git grants.
	GitGrants GrantRevoker
	// Internal reserves a work item's internal writer; optional.
	Internal InternalWorkspaces
	// GrantFence is the runtime grant check joined to the transaction that records
	// a begin or a preparation receipt.
	GrantFence GrantFencer
}

// Options configure a Store.
type Options struct {
	Clock func() time.Time
	NewID func(prefix string) string
}
