package gitops

import (
	"context"
	"io"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- the tenant's Git worker ----

// Worker is gitops's view of one tenant's Git worker: the subset of
// repository.Hosts it uses plus the T-160 bundle replay (which the worker's
// Hosts adapter provides). The tenant selects the
// worker; every call is idempotent on the repository ID and OperationID. A call
// that fails after the request was sent has an unknown outcome and is repeated
// with the identical request on the next pass.
type Worker interface {
	// Initialize ensures the repository exists with the server-owned
	// configuration (created is false when it already did); a restore uses it
	// before replaying a bundle into a volume that lost the repository.
	Initialize(ctx context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error)
	ReadRefs(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error)
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error)
	SetRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error)
	DeleteRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error)
	DeleteBranch(ctx context.Context, tenant domain.TenantID, req gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error)
	Maintain(ctx context.Context, tenant domain.TenantID, req gitv1.MaintainRequest) (gitv1.MaintainResponse, error)
	// SetRestoreFence (T-160) enters (true) or leaves (false) the repository's
	// restore fence: while fenced the worker refuses receives, server ref
	// operations and maintenance and accepts ReplayBundle, which it refuses at any
	// other time (restore_fence_required). Entering waits for the running
	// operations to end. Both directions are idempotent.
	SetRestoreFence(ctx context.Context, tenant domain.TenantID, repositoryID string, fenced bool) error
	// ReplayBundle (T-160) verifies the bundle (and its digest, when the request
	// names one) and creates the refs the repository LACKS; it never advances,
	// rewinds or deletes a ref and reports every ref whose value differs with its
	// relation to the bundle's. It runs only while the repository is fenced for
	// restore and is idempotent on the operation ID (a repeat of an applied replay
	// reports its created refs as unchanged).
	ReplayBundle(ctx context.Context, tenant domain.TenantID, req ReplayRequest, r io.Reader) (ReplayResult, error)
}

// ReplayRequest identifies one bundle replay (the contract's ReplayBundleRequest).
type ReplayRequest struct {
	RepositoryID string
	// OperationID is deterministic per (recovery, repository): a repeat after an
	// unknown outcome is the identical request.
	OperationID string
	// SHA256 is the hex digest of the bundle bytes the worker verifies before it
	// changes anything.
	SHA256 string
}

// Bundle is what CreateBundle holds, read atomically under the worker's gate:
// exactly the refs the bundle contains and the journal head at that moment.
type Bundle struct {
	Refs        []gitv1.RefTip
	JournalHead int64
	// Consistent is true only when no receive or server operation ran while the
	// bundle was taken, so Refs and JournalHead are one exact boundary. A backup
	// never records an inconsistent bundle.
	Consistent bool
}

// Objects is the read-only object-level access the worker's RPC offers for
// pruning and backup: ancestry, commit existence and times, and bundles. The
// worker (T-160) provides it under its shared mutation gate; package localgit
// implements the same reads with the Git binary for tests and for a process that
// mounts the volume. Nothing here changes a repository.
type Objects interface {
	// IsAncestor reports whether ancestor is reachable from descendant (equal
	// commits are). A commit the repository lacks is an error, never false.
	IsAncestor(ctx context.Context, tenant domain.TenantID, repositoryID, ancestor, descendant string) (bool, error)
	// HasCommit reports whether the commit exists in the repository.
	HasCommit(ctx context.Context, tenant domain.TenantID, repositoryID, commit string) (bool, error)
	// CommitTime is the committer time of the commit.
	CommitTime(ctx context.Context, tenant domain.TenantID, repositoryID, commit string) (time.Time, error)
	// CreateBundle writes a bundle of every ref of the repository (heads, tags and
	// the hidden retained refs) to w and returns the ref set and journal head it
	// was taken at. A repository without refs is ErrEmptyRepository.
	CreateBundle(ctx context.Context, tenant domain.TenantID, repositoryID string, w io.Writer) (Bundle, error)
}

// ConflictRelation says how the repository's value of a ref relates to the
// bundle's (the contract's BundleRelation).
type ConflictRelation string

// Conflict relations. The ref is never changed whichever applies.
const (
	// RepositoryAhead: the bundle's commit is an ancestor of the repository's.
	RepositoryAhead ConflictRelation = "repository_ahead"
	// BundleAhead: the repository's commit is an ancestor of the bundle's.
	BundleAhead ConflictRelation = "bundle_ahead"
	// Diverged: neither is an ancestor of the other.
	Diverged ConflictRelation = "diverged"
)

// RefConflict is a ref the bundle replay did not change because the repository
// holds another value.
type RefConflict struct {
	Ref      string
	Have     string
	Want     string
	Relation ConflictRelation
}

// ReplayResult is what a bundle replay did: it creates missing refs and
// nothing else.
type ReplayResult struct {
	Created, Unchanged int
	// Conflicts are refs left alone because the repository's value differs from
	// the bundle's, whichever way: an older value, a newer one or a divergent one.
	Conflicts []RefConflict
}

// ---- repositories and tenants ----

// Repository is one internal repository the operations apply to.
type Repository struct {
	ID            string
	Scope         domain.Scope
	DefaultBranch string
	// Archived repositories are maintained but never pruned (GR-14: archive
	// keeps history).
	Archived bool
}

// Catalog enumerates tenants and their internal, provisioned repositories (a
// provisioning or external repository is never listed). The composition adapts
// the repository module.
type Catalog interface {
	Tenants(ctx context.Context) ([]domain.TenantID, error)
	// Repositories returns up to limit repositories of the tenant ordered by ID
	// after the given one.
	Repositories(ctx context.Context, tenant domain.TenantID, after string, limit int) ([]Repository, error)
}

// ---- pins and evidence ----

// PinnedRef is a retained ref a run's record says must exist.
type PinnedRef struct {
	Ref    string
	Commit string
}

// Held is what runs, evidence and deployments still reference in a repository
// (GR-13): commits that must stay reachable, branches that must not be pruned
// (the branch of a run in flight, a branch an evidence record names) and the
// retained refs the run records expect.
type Held struct {
	Commits []string
	Refs    []string
	Pins    []PinnedRef
}

// Holds is implemented by the modules that own run, evidence and deployment
// records (execution T-148/T-149, delivery). An error means "unknown": pruning
// of the repository is skipped and restore reconciliation stays closed.
type Holds interface {
	Held(ctx context.Context, scope domain.Scope, repositoryID string) (Held, error)
}

// ReleaseGate decides whether a run's retained refs may be released
// (retention). A missing gate refuses every release.
type ReleaseGate interface {
	MayRelease(ctx context.Context, scope domain.Scope, runID domain.RunID) (bool, error)
}

// ---- backups ----

// BackupStore is the narrow object-storage port of backups. Keys are logical;
// the adapter owns the bucket and prefix and refuses keys outside it (the
// composition adapts the artifact store's S3 adapter and adds List).
type BackupStore interface {
	// Put stores the object, replacing a previous one at the key.
	Put(ctx context.Context, key string, r io.Reader) error
	// Open returns ErrBackupNotFound for an absent key.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns the keys with the prefix, ascending.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes the key; an absent key is success.
	Delete(ctx context.Context, key string) error
}

// Snapshot identifies one volume snapshot.
type Snapshot struct {
	ID      string
	TakenAt time.Time
}

// Snapshots is the volume snapshot provider (the storage class's snapshot
// capability, composed by T-132/T-156).
type Snapshots interface {
	// Snapshot takes a snapshot of the tenant's Git volume.
	Snapshot(ctx context.Context, tenant domain.TenantID) (Snapshot, error)
	// RestoreVolume makes the tenant's Git volume the snapshot's content and
	// returns once the worker serves it again (the tenant is fenced meanwhile).
	RestoreVolume(ctx context.Context, tenant domain.TenantID, snapshotID string) error
}

// ---- access fence and push records ----

// FenceReason names why Git access is closed.
type FenceReason string

// Fence reasons.
const (
	FenceRestore     FenceReason = "restore"
	FenceOffboarding FenceReason = "offboarding"
)

// AccessFence closes and reopens all Git access of a tenant (smart HTTP,
// credential issuance, push authorization): the repository-level "restoring"
// fence the Git endpoint (T-132) consumes. Closing is durable and idempotent;
// Open reopens only a fence this reason closed (a fence closed for another
// reason stays closed) and is idempotent.
type AccessFence interface {
	Close(ctx context.Context, tenant domain.TenantID, reason FenceReason) error
	Open(ctx context.Context, tenant domain.TenantID, reason FenceReason) error
	// ClosedFor reports whether the tenant's access is closed for the reason.
	ClosedFor(ctx context.Context, tenant domain.TenantID, reason FenceReason) (bool, error)
}

// PushSettlement is what the restore reconciliation needs from the push records.
type PushSettlement struct {
	// InFlight entries are authorized or committing: not settled yet.
	InFlight int
	// Unknown entries stay visible, block their writers and wait for an operator.
	Unknown int
	// KnownHead is the highest journal head the control plane has seen.
	KnownHead int64
}

// Pushes is gitops's view of gitpush (*gitpush.Service through PushesOf).
type Pushes interface {
	// Reconcile settles the repository's unsettled entries from the worker's
	// evidence; a journal older than the head seen settles unknown, never refused.
	Reconcile(ctx context.Context, tenant domain.TenantID, repositoryID string) error
	Settlement(ctx context.Context, tenant domain.TenantID, repositoryID string) (PushSettlement, error)
	// AcknowledgeRestore accepts the restored journal head as the baseline once
	// nothing is in flight; it reports whether the known head changed.
	AcknowledgeRestore(ctx context.Context, scope domain.Scope, repositoryID string, head int64, by, reason string) (bool, error)
}

// ---- offboarding ----

// OffboardingState is the tenant lifecycle's answer.
type OffboardingState struct {
	// Requested is true once the tenant was offboarded.
	Requested bool
	// RetainUntil is the end of the retention period; the volume is deleted no
	// earlier. Hold keeps it regardless (legal or operator hold).
	RetainUntil time.Time
	Hold        bool
}

// Lifecycle is the tenant lifecycle's view for offboarding.
type Lifecycle interface {
	Offboarding(ctx context.Context, tenant domain.TenantID) (OffboardingState, error)
}

// Grants is gitauth's tenant-wide revocation (T-161), which exists as
// gitauth.Service.RevokeAll(ctx, tenant, reason, actor) (int, error) and
// Active(ctx, tenant) (int, error). The T-132 adapter calls RevokeAll with a
// reason ("tenant offboarding") and the system actor of the offboarding, discards
// the count and returns its error, and passes Active through. Offboarding repeats
// RevokeAll until Active is zero, and only after issuance was fenced: the access
// fence is closed first (StageFenced), so no grant is issued after the sweep.
type Grants interface {
	// RevokeAll revokes every Git grant of the tenant; idempotent.
	RevokeAll(ctx context.Context, tenant domain.TenantID) error
	// Active counts the tenant's unrevoked, unexpired grants.
	Active(ctx context.Context, tenant domain.TenantID) (int, error)
}

// StorageOffboarder deletes the tenant's Git storage (T-129's
// gitworker.Reconciler.OffboardTenant, which asks Retention.MayDeleteVolume
// again: Offboarding implements that method).
type StorageOffboarder interface {
	OffboardTenant(ctx context.Context, tenant domain.TenantID) error
}

// AuditEvent is one audit record of an operation (GR-14: deletion records audit).
type AuditEvent struct {
	// ID is deterministic per logical event, so an Auditor deduplicates a repeat.
	ID       string
	Tenant   domain.TenantID
	Action   string
	Metadata map[string]any
}

// Auditor records audit events (system actor).
type Auditor interface {
	Record(ctx context.Context, e AuditEvent) error
}

// ---- durable state (migration 016 in gitops/postgres) ----

// BackupState is the state of one backup run.
type BackupState string

// Backup states.
const (
	BackupActive   BackupState = "active"
	BackupComplete BackupState = "complete"
	BackupFailed   BackupState = "failed"
)

// BackupLeases serializes the backups and expiry of a tenant across control-plane
// instances and records, durably, which backups are in progress, complete or
// abandoned. At most one run per tenant is active.
type BackupLeases interface {
	// Acquire starts the run: it marks an active run whose lease expired failed
	// (abandoned) and creates this one, or returns ErrBackupInProgress while
	// another run's lease is live.
	Acquire(ctx context.Context, tenant domain.TenantID, backupID string, ttl time.Duration) error
	// Renew extends the lease; ErrLeaseLost when the run is no longer active
	// (another run abandoned it after the lease expired).
	Renew(ctx context.Context, tenant domain.TenantID, backupID string, ttl time.Duration) error
	// Complete publishes the run: ErrLeaseLost unless it is still active.
	Complete(ctx context.Context, tenant domain.TenantID, backupID string) error
	// Fail marks the run failed (idempotent; a no-op for a complete run).
	Fail(ctx context.Context, tenant domain.TenantID, backupID string) error
	// State reports a run's state; known is false for a backup with no row.
	State(ctx context.Context, tenant domain.TenantID, backupID string) (state BackupState, known bool, err error)
}

// PruneIntents is the reservation pruning and hold acquisition share, so a hold
// cannot be taken on a branch while it is being pruned (GR-13).
//
// Protocol. Pruning declares the intent (durable, with a lifetime) BEFORE it
// re-reads Held, deletes only if the fresh Held does not name the branch or its tip,
// and releases the intent after a definite outcome (an unknown outcome keeps it
// until it expires). A module that takes a hold on a branch or commit of an
// internally hosted repository (run admission T-148, writer reservation T-147,
// evidence) commits its hold FIRST and then asks Conflicts for the branch and its
// tip: when it answers true the hold is withdrawn and the operation is refused
// retryably (the branch is going away). Either the prune sees the committed hold
// or the acquirer sees the declared intent; both cannot miss each other.
//
// The intents are also the cross-instance guard between pruners: the lock a
// Scheduler holds per repository is in-process only. A reservation belongs to the
// owner that declared it: another owner's Declare of a ref with a live
// reservation is ErrPruneBusy, and only the owner's Release drops it, so two
// instances pruning one repository never release each other's reservation.
type PruneIntents interface {
	// Declare reserves the ref at its tip for the owner until the lifetime ends.
	// It is idempotent for the same owner (a repeat extends it and may name a new
	// tip), replaces an expired or released reservation of anyone, and is
	// ErrPruneBusy while another owner's reservation of the ref is live.
	Declare(ctx context.Context, scope domain.Scope, repositoryID, ref, tip, owner string, ttl time.Duration) error
	// Release drops the owner's reservation (idempotent; another owner's, or none,
	// is left untouched).
	Release(ctx context.Context, scope domain.Scope, repositoryID, ref, owner string) error
	// Conflicts reports whether an unexpired intent names the ref or the commit.
	Conflicts(ctx context.Context, scope domain.Scope, repositoryID, ref, commit string) (bool, error)
}

// HoldGuard is the half of PruneIntents a hold acquirer consumes: after it has
// committed its hold on a branch or commit of an internally hosted repository
// (run admission T-148, writer reservation T-147, evidence) it asks whether
// the branch is being pruned. A module declares its own one-method port with
// this signature; the composition (T-132) passes the PruneIntents store.
type HoldGuard interface {
	// Conflicts reports whether an unexpired prune intent names the ref or the commit.
	// On true the acquirer withdraws its hold and refuses the operation retryably.
	Conflicts(ctx context.Context, scope domain.Scope, repositoryID, ref, commit string) (bool, error)
}

var _ HoldGuard = PruneIntents(nil)

// RecoveryStatus is the state of a recovery.
type RecoveryStatus string

// Recovery statuses.
const (
	RecoveryActive   RecoveryStatus = "active"
	RecoveryComplete RecoveryStatus = "complete"
)

// DiscrepancyKind classifies how the restored volume differs from the recovery
// point.
type DiscrepancyKind string

// Discrepancy kinds.
const (
	// DiscrepancyMissing: the recovery point has the ref, the volume does not.
	DiscrepancyMissing DiscrepancyKind = "missing"
	// DiscrepancyExtra: the volume has a ref the recovery point does not (for
	// example a branch deleted before the bundle that an older snapshot brings back).
	DiscrepancyExtra DiscrepancyKind = "extra"
	// DiscrepancyDiffers: both have the ref at different commits (older, newer or
	// rewritten).
	DiscrepancyDiffers DiscrepancyKind = "differs"
)

// Discrepancy is one difference between the recovery point and the volume.
type Discrepancy struct {
	Repository string
	Ref        string
	Kind       DiscrepancyKind
	// Expected is the recovery point's commit ("" for extra), Actual the volume's
	// ("" for missing).
	Expected string
	Actual   string
}

// DiscrepancyResolution is an operator's audited acceptance of one discrepancy:
// the volume's state for that ref stands. It names the observed value, so a
// later change of the ref needs a new resolution.
type DiscrepancyResolution struct {
	Discrepancy
	By     string
	Reason string
	At     time.Time
}

// RecoveryRecord is the durable state of a tenant's recovery.
type RecoveryRecord struct {
	ID         string
	Tenant     domain.TenantID
	SnapshotID string
	BackupID   string
	Status     RecoveryStatus
	// VolumeRestored is true when no snapshot was requested or its restore completed.
	VolumeRestored bool
	// Replays holds the repositories whose bundle replay completed (or was not
	// required) and its result.
	Replays     map[string]ReplayResult
	Resolutions []DiscrepancyResolution
}

// RecoveryStore keeps the recovery selected for a tenant and what completed, so
// a failed step, a restart or another instance never reopens access on a
// half-restored volume.
type RecoveryStore interface {
	// Begin records the recovery (id) for the tenant, or returns the active one
	// for the identical source; another source while one is active is
	// ErrRecoveryActive.
	Begin(ctx context.Context, tenant domain.TenantID, id string, src RestoreSource, now time.Time) (RecoveryRecord, error)
	// Current returns the tenant's latest recovery.
	Current(ctx context.Context, tenant domain.TenantID) (RecoveryRecord, bool, error)
	MarkVolumeRestored(ctx context.Context, tenant domain.TenantID, id string, now time.Time) error
	MarkReplayed(ctx context.Context, tenant domain.TenantID, id, repositoryID string, r ReplayResult, now time.Time) error
	// Resolve records the operator's resolution with an audit row, atomically.
	Resolve(ctx context.Context, tenant domain.TenantID, id string, res DiscrepancyResolution) error
	// Complete ends the recovery (idempotent).
	Complete(ctx context.Context, tenant domain.TenantID, id string, now time.Time) error
}
