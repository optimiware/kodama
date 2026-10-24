package gitwebhook

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// The ports below are owned by this package. Adapters implement them and the
// composition root wires them; a missing implementation fails construction.

// Action names an authorization question about a trigger.
type Action string

const (
	// ActionInstall manages a trigger (project settings authority).
	ActionInstall Action = "git_trigger.manage"
	// ActionRun starts a run from a push (run management authority).
	ActionRun Action = "git_trigger.run"
)

// Authorizer decides with current state, on every call, whether the principal
// holds the action in the scope. It returns nil to allow and ErrDenied or
// ErrNotFound to refuse; any other error is an outage and refuses too.
type Authorizer interface {
	Authorize(ctx context.Context, principal string, scope domain.Scope, action Action) error
}

// Secrets resolves a secret reference just in time. It returns
// ErrSecretNotFound for a missing, revoked or foreign reference. The caller
// never retains or logs the value.
type Secrets interface {
	Resolve(ctx context.Context, scope domain.Scope, ref string) ([]byte, error)
}

// Definitions confirms a pipeline definition is registered in the scope: nil,
// ErrNotFound for a missing or foreign one, or an outage error.
type Definitions interface {
	Registered(ctx context.Context, scope domain.Scope, id domain.DefinitionID) error
}

// RunRequest asks run admission to start one exact-commit run. IdempotencyKey
// is stable per delivery: starting twice returns the same run.
type RunRequest struct {
	Scope          domain.Scope
	DefinitionID   domain.DefinitionID
	WorkItemID     domain.WorkItemID
	Commit         workspace.Commit
	Ref            string
	TriggerID      TriggerID
	DeliveryID     string
	IdempotencyKey string
	// RequestedBy is the trigger's installing principal, whose authority admission
	// re-checks. The hook credential is never an identity.
	RequestedBy string
	// RepositoryID is the repository identity verified against the project's
	// current binding immediately before admission. Admission must preserve it
	// as the run's source identity and refuse a conflicting source.
	RepositoryID workspace.RepositoryID
}

// RunRef identifies the admitted run.
type RunRef struct {
	ID      domain.RunID
	Created bool
}

// Runs admits a run from a verified push. A permanent refusal wraps
// ErrRunRefused (ErrAdmissionClosed after DecideRefusal); any other error is an outage that leaves the delivery
// retryable.
type Runs interface {
	StartRun(ctx context.Context, req RunRequest) (RunRef, error)
	// FindRun returns the run already admitted under the idempotency key in the
	// scope, without checking any authority or creating anything. It is how a
	// delivery whose admission succeeded but whose result was lost recovers the
	// actual run. found is false when no run exists.
	FindRun(ctx context.Context, scope domain.Scope, idempotencyKey string) (ref RunRef, found bool, err error)
	// DecideRefusal is the admission-owned half of the success/refusal ledger,
	// atomic with run creation for the same key. If a run exists it returns it
	// (Run, Found) and records nothing. Otherwise it durably records the
	// refusal, the first reason winning, and returns that reason; from then on
	// StartRun for the key creates nothing and returns ErrAdmissionClosed, even
	// for a caller that stalled before the decision and resumes later. Delivery
	// code records a terminal admission refusal only from this result, so a
	// rejected delivery can never acquire an unrecorded run.
	DecideRefusal(ctx context.Context, scope domain.Scope, idempotencyKey, reason string) (Decision, error)
}

// Decision is the result of Runs.DecideRefusal.
type Decision struct {
	// Found is true when a run already exists for the key; Run identifies it.
	Found bool
	Run   RunRef
	// Reason is the recorded (first) refusal reason when Found is false.
	Reason string
}

// Outcome is the final result of a delivery.
type Outcome string

const (
	OutcomeStarted  Outcome = "started"
	OutcomeIgnored  Outcome = "ignored"
	OutcomeRejected Outcome = "rejected"
)

// Stable reasons recorded with outcomes and audit.
const (
	ReasonBranchFiltered     = "branch_filtered"
	ReasonNotBranch          = "ref_not_branch"
	ReasonBranchDeleted      = "branch_deleted"
	ReasonTriggerDisabled    = "trigger_disabled"
	ReasonBadSignature       = "bad_signature"
	ReasonStaleTimestamp     = "stale_timestamp"
	ReasonSecretUnavailable  = "secret_unavailable"
	ReasonForeignRepository  = "foreign_repository"
	ReasonRepositoryRebound  = "repository_rebound"
	ReasonBranchNotFound     = "branch_not_found"
	ReasonCommitNotOnBranch  = "commit_not_on_branch"
	ReasonInstallerDenied    = "installer_not_authorized"
	ReasonAdmissionRefused   = "admission_refused"
	ReasonDeliveryConflict   = "delivery_conflict"
	ReasonDuplicateDelivery  = "duplicate_delivery"
	ReasonInvalidPayload     = "invalid_payload"
	ReasonMissingCredentials = "missing_credentials"
)

// Finish is the terminal result handed to Store.FinishDelivery.
type Finish struct {
	Outcome Outcome
	Reason  string
	RunID   domain.RunID
	// Admission is true for the results of the admission phase: started, or a
	// permanent admission refusal. Pre-admission results (ignored and every
	// other rejection) leave it false.
	Admission bool
	// Holder is the lease holder recording a rejected admission result.
	Holder string
}

// DeliveryKey identifies one delivery: the trigger and the sender's identifier.
type DeliveryKey struct {
	TriggerID  TriggerID
	DeliveryID string
}

// DeliveryRecord is the durable deduplication record. It is created only after
// authentication, so a forged request can never consume a delivery identifier.
type DeliveryRecord struct {
	Key        DeliveryKey
	Scope      domain.Scope
	BodyDigest string
	Ref        string
	Commit     workspace.Commit
	ReceivedAt time.Time
	// Admitting is set durably before the first run admission attempt. From then
	// on a run may exist for the delivery, so the only terminal outcomes are
	// the run recovered by idempotency key or a permanent admission refusal; a
	// rejection based on a later observation (branch movement, revoked
	// installer) is fenced out.
	Admitting bool
	// LeaseHolder/LeaseUntil are the exclusive right to attempt admission and
	// to record its refusal; see Store.AcquireAdmission.
	LeaseHolder string
	LeaseUntil  time.Time
	// Done is false while the delivery is claimed but unfinished (a crash or
	// outage); a redelivery then resumes it.
	Done        bool
	Outcome     Outcome
	Reason      string
	RunID       domain.RunID
	CompletedAt time.Time
}

// AuditRecord is an append-only audit entry. Metadata holds only values safe
// for every audit reader: never signatures, bodies or secrets.
type AuditRecord struct {
	Action     string
	Scope      domain.Scope
	TriggerID  TriggerID
	Principal  string
	DeliveryID string
	Reason     string
	Metadata   map[string]string
	At         time.Time
}

// Store is the persistence seam. Implementations write each audit record in the
// transaction of the change it describes.
type Store interface {
	// InsertTrigger stores a new trigger (ErrConflict when the ID exists).
	InsertTrigger(ctx context.Context, t Trigger, audit AuditRecord) error
	// Trigger loads a trigger by address (ErrNotFound when absent). The result
	// carries the owning scope; the caller never supplies one.
	Trigger(ctx context.Context, id TriggerID) (Trigger, error)
	// UpdateTrigger replaces a trigger when the stored version equals
	// expected, advancing the version (ErrConflict otherwise).
	UpdateTrigger(ctx context.Context, t Trigger, expected int64, audit AuditRecord) (Trigger, error)
	// ClaimDelivery inserts rec unless the key exists. It returns the stored
	// record and whether this call created it.
	ClaimDelivery(ctx context.Context, rec DeliveryRecord) (DeliveryRecord, bool, error)
	// BeginAdmission atomically marks a pending delivery Admitting and returns
	// it. A finished delivery is returned unchanged (the caller replays it).
	BeginAdmission(ctx context.Context, key DeliveryKey) (DeliveryRecord, error)
	// AcquireAdmission grants holder the exclusive admission lease of an
	// Admitting delivery until now+ttl, or reports acquired=false while another
	// holder's unexpired lease stands (re-acquiring one's own lease succeeds).
	// A finished delivery is returned unchanged with acquired=false. Only the
	// lease holder may call StartRun or record a refusal, which makes success
	// and refusal mutually exclusive: no processor can record a rejection while
	// another may still be creating the run. A holder that disappears blocks
	// others only until its lease expires; ttl must exceed the longest StartRun.
	AcquireAdmission(ctx context.Context, key DeliveryKey, holder string, now time.Time, ttl time.Duration) (rec DeliveryRecord, acquired bool, err error)
	// ReleaseAdmission drops holder's lease after a definite non-terminal
	// result (an outage); it never changes another holder's lease.
	ReleaseAdmission(ctx context.Context, key DeliveryKey, holder string) error
	// FinishDelivery records the outcome of a claimed delivery together with
	// its audit record, atomically. The first finish wins: a finished delivery
	// is returned unchanged and no audit is written. The fence: a pre-admission
	// finish (fin.Admission false) of an Admitting delivery, an admission
	// finish of one that never began admission, and a rejected admission finish
	// whose fin.Holder does not hold the current lease all fail with ErrConflict
	// and change nothing. A started finish needs no lease: a run exists.
	FinishDelivery(ctx context.Context, key DeliveryKey, fin Finish, audit AuditRecord) (DeliveryRecord, error)
	// RecordAudit appends an audit record that has no state change.
	RecordAudit(ctx context.Context, audit AuditRecord) error
}
