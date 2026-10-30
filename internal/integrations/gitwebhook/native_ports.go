package gitwebhook

import (
	"context"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are owned by the native trigger and implemented by adapters
// (adapters/, postgres/); the composition root (T-132) wires them and the event
// subscription. A missing implementation is a construction error, never an
// allow-all default.

// PushRecords reloads the push record of the receive a git.refs_updated event
// names (its receiveId, which is also the envelope's resource ID). The record is
// authoritative about the entries and who pushed. Reload applies the scope
// comparison only: the consumer already holds the scope's authority
// (gitpush.Service.ReloadByReceive). ErrNotFound is a missing or foreign record;
// any other error is an outage.
type PushRecords interface {
	Reload(ctx context.Context, scope domain.Scope, repositoryID, receiveID string) (gitv1.PushRecord, error)
}

// RunOrigin is what started a run: a push (an applied record and ref) or a
// ready work item.
type RunOrigin struct {
	// Push is set for a push origin; WorkItemID for a work item origin.
	PushID       string
	RepositoryID string
	Ref          string
	WorkItemID   domain.WorkItemID
}

// SourceRunRequest asks run admission to start one run on the internal Git
// service (execution.AdmitSourceRun). A push run has no work item and pins the
// entry's new commit from the applied record; the request never carries a
// branch tip.
type SourceRunRequest struct {
	Scope domain.Scope
	// RunID is the stable identity derived from the inbox key. Admitting it twice
	// returns the same run, and the same ID with another declaration is a conflict.
	RunID        domain.RunID
	DefinitionID domain.DefinitionID
	Origin       RunOrigin
	// ContentRevision pins a work item origin's specification (zero otherwise).
	ContentRevision domain.ContentRevision
	// ExpectedCommit is, for a push origin, the entry's `new` value in the applied
	// record the trigger read. Admission pins it from the record itself; the
	// adapter verifies that the pinned commit equals it, so the run's source is the
	// pushed commit and never a later tip.
	ExpectedCommit string
	// Principal is the configuring principal whose current authority admission
	// re-checks; RequestID correlates with the event.
	Principal string
	RequestID string
}

// SourceRuns is run admission for the native path. A permanent refusal wraps
// ErrRunRefused; quota, busy, unconfigured and infrastructure failures wrap
// ErrUnavailable and decide nothing.
type SourceRuns interface {
	// Admit starts the run or returns the one already admitted under RunID.
	Admit(ctx context.Context, req SourceRunRequest) (RunRef, error)
	// Find reports whether the run exists, without authority checks or effects: it
	// is how a claim whose admission succeeded but whose result was lost recovers
	// the actual run, even after the configurer's authority was revoked.
	Find(ctx context.Context, scope domain.Scope, id domain.RunID) (found bool, err error)
}

// RepositoryFacts are what trigger configuration needs of a repository.
type RepositoryFacts struct {
	ID      string
	Hosting gitv1.Hosting
	Status  gitv1.RepositoryStatus
}

// PushRepositories looks up a repository in a scope: ErrNotFound for an unknown
// or foreign one (indistinguishable), any other error is an outage.
type PushRepositories interface {
	Repository(ctx context.Context, scope domain.Scope, repositoryID string) (RepositoryFacts, error)
}

// WorkFacts is the current state of a work item.
type WorkFacts struct {
	State           string
	ContentRevision domain.ContentRevision
	Actionable      bool
	Cancelled       bool
	BlockedReason   string
}

// WorkItems reads a work item's current state: ErrNotFound for an unknown or
// foreign item, any other error is an outage.
type WorkItems interface {
	Facts(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (WorkFacts, error)
}

// NativeStore is the persistence seam of the native path: trigger and route
// configuration (audit written in the transaction of the change) and the inbox
// that deduplicates admissions. The memory and PostgreSQL implementations run
// one shared contract (gitwebhooktest.NativeStoreContract).
type NativeStore interface {
	// InsertPushTrigger stores a new trigger (ErrConflict when the ID exists in the project).
	InsertPushTrigger(ctx context.Context, t PushTrigger, audit AuditRecord) error
	// PushTrigger loads a trigger in scope (ErrNotFound when absent or foreign).
	PushTrigger(ctx context.Context, scope domain.Scope, id string) (PushTrigger, error)
	// UpdatePushTrigger replaces the mutable members when the stored version equals
	// expected, advancing the version (ErrConflict otherwise). The repository
	// binding never changes.
	UpdatePushTrigger(ctx context.Context, t PushTrigger, expected int64, audit AuditRecord) (PushTrigger, error)
	// ListPushTriggers returns the project's triggers ordered by ID after the given
	// ID, at most limit.
	ListPushTriggers(ctx context.Context, scope domain.Scope, after string, limit int) ([]PushTrigger, error)
	// EnabledPushTriggers returns the project's enabled triggers of one repository,
	// ordered by ID.
	EnabledPushTriggers(ctx context.Context, scope domain.Scope, repositoryID string) ([]PushTrigger, error)

	// PutTaskRoute creates the project's route (expected 0) or replaces it when
	// the stored version equals expected (ErrConflict otherwise).
	PutTaskRoute(ctx context.Context, r TaskRoute, expected int64, audit AuditRecord) (TaskRoute, error)
	// TaskRoute loads the project's route (ErrNotFound when none).
	TaskRoute(ctx context.Context, scope domain.Scope) (TaskRoute, error)

	// LatestInbox returns the most recently decided inbox record of the source
	// (the trigger, or the project for a task route), restricted to outcome when
	// it is not empty; ok is false when none exists. It is the status read:
	// records carry only stable reason codes, never names or secrets.
	LatestInbox(ctx context.Context, scope domain.Scope, kind InboxKind, source string, outcome Outcome) (rec InboxRecord, ok bool, err error)

	// ClaimInbox inserts the key unless it exists and grants holder the attempt
	// lease until now+ttl, or reports acquired=false while another holder's
	// unexpired lease stands. A finished record is returned unchanged with
	// acquired=false. Re-acquiring one's own lease succeeds. The consumer uses a
	// distinct holder per attempt.
	ClaimInbox(ctx context.Context, key InboxKey, holder string, now time.Time, ttl time.Duration) (rec InboxRecord, acquired bool, err error)
	// FinishInbox records the write-once outcome with its audit, atomically. The
	// first finish wins: a finished record is returned unchanged and no audit is
	// written. A started finish needs no lease (a run exists); another outcome
	// is recorded only by the holder of the lease while it is still valid at now:
	// a stalled attempt whose lease expired, or was taken over, cannot record a
	// refusal or an ignore while another attempt may be creating the run
	// (ErrConflict, nothing changes). The finish shape is validated first, even
	// for a decided or unknown key (ErrInvalid).
	FinishInbox(ctx context.Context, key InboxKey, fin InboxFinish, now time.Time, audit AuditRecord) (InboxRecord, error)
	// ReleaseInbox drops holder's lease after a retryable failure; it never
	// changes another holder's lease.
	ReleaseInbox(ctx context.Context, key InboxKey, holder string) error
}
