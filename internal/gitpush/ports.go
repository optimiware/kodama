package gitpush

import (
	"context"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
)

// ---- persistence ----

// AuditActor is who an audit row is attributed to.
type AuditActor struct {
	// Type is "user", "workload" or "system" (the audit table's actor types).
	Type string
	ID   string
}

// Audit is one audit record written in the transaction of the change. Metadata
// must hold only values safe for every audit reader (never a credential).
type Audit struct {
	Action   string
	Actor    AuditActor
	Reason   string
	Metadata map[string]any
}

// Effects are the records a record write carries: audit rows and, when Event
// is set, the one reference-only git.refs_updated event of the record. The
// adapters write them in the same transaction as the record; the record must
// then be applied (eventsv1.NewGitRefsUpdated).
type Effects struct {
	Audit []Audit
	Event bool
	// Resolutions are the operator resolutions of unknown entries this write
	// carries. The adapters insert each row before the entry changes state in the
	// same transaction: the database refuses any other move out of unknown
	// (migration 015).
	Resolutions []Resolution
}

// Resolution is the durable trace of an operator's decision on one unknown
// entry: who, what, why and which value of the ref the operator observed.
type Resolution struct {
	Ref string
	// State is the entry's new state: applied or refused.
	State gitv1.EntryState
	// By is the administrator who decided, Reason the operator's explanation and
	// Observed the ref's value the control plane read when the decision was
	// taken ("" when it could not be read; the operator's inspection stands).
	By       string
	Reason   string
	Observed string
	At       time.Time
}

// ListQuery selects one page of a repository's push records, newest first,
// ordered by (created_at, id) descending.
type ListQuery struct {
	After *Position
	Limit int
}

// Position is a list continuation: the (created_at, id) of the last row.
type Position struct {
	CreatedAt time.Time
	ID        string
}

// Store persists push records. Do runs fn in one tenant transaction: everything
// it wrote commits or is discarded together, and an unknown commit outcome is
// ErrUnavailable. View is a read-only one. Neither may be used across a network
// call: fn must make none.
type Store interface {
	Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx Tx) error) error
	View(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx Tx) error) error
}

// Tx is what a use case sees inside one tenant transaction. Every method is
// confined to the transaction's tenant; a foreign or absent object is
// ErrNotFound.
type Tx interface {
	// ByID returns the record; lock takes the record's row lock until the
	// transaction ends (Do only), so settlement and commit intent are serialized.
	ByID(ctx context.Context, pushID string, lock bool) (*Record, error)
	// ByReceive finds the record of a receive of the repository.
	ByReceive(ctx context.Context, repositoryID, receiveID string) (*Record, error)
	// Insert stores a new record with its effects; a duplicate push ID or
	// (repository, receive) pair is ErrConflict.
	Insert(ctx context.Context, r *Record, fx Effects) error
	// Save writes the changed record (its version advanced by exactly one by the
	// entity) and its effects. A concurrent change is an error.
	Save(ctx context.Context, r *Record, fx Effects) error
	// List returns a page of the repository's records in the scope.
	List(ctx context.Context, scope domain.Scope, repositoryID string, q ListQuery) ([]*Record, error)
	// Unsettled returns records of the repository with an entry still authorized
	// or committing, ordered by ID after afterID.
	Unsettled(ctx context.Context, repositoryID, afterID string, limit int) ([]*Record, error)
	// HasUnsettled reports whether the repository has any record (other than the
	// receive exceptReceive) whose status is authorized or unknown.
	HasUnsettled(ctx context.Context, scope domain.Scope, repositoryID, exceptReceive string) (bool, error)
	// UnsettledOnRef reports whether any record of the repository in the scope has
	// an entry for ref that is authorized, committing or unknown.
	UnsettledOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error)
	// AppliedOnRef returns up to limit records of the repository in the scope with
	// an applied entry for ref, newest first by the order in which that entry
	// applied (persisted per entry: the database numbers an entry when it becomes
	// applied; memory does the same).
	AppliedOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]*Record, error)
	// HasUnknownEntry reports whether an entry of ref in the repository is unknown.
	HasUnknownEntry(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error)
	// EmptyReportPending returns records whose applied entries made an empty
	// repository non-empty and which the repository module has not been told.
	EmptyReportPending(ctx context.Context, repositoryID string, limit int) ([]*Record, error)
	// ClearEmptyReport records that the repository module was told.
	ClearEmptyReport(ctx context.Context, pushID string) error
	// KnownHead is the highest ref-journal head the control plane saw for the
	// repository (0 if never); NoteHead raises it, never lowers it.
	KnownHead(ctx context.Context, repositoryID string) (int64, error)
	NoteHead(ctx context.Context, repositoryID string, head int64, now time.Time) error
	// RepositoryState re-reads the repository's row in this transaction and holds
	// it against change (FOR SHARE) until the transaction ends: the status, the
	// default branch and the policy a push is authorized under cannot change
	// between the check and the commit of the record. A foreign or absent
	// repository is ErrNotFound.
	RepositoryState(ctx context.Context, repositoryID string) (RepositoryState, error)
	// EntryCounts counts the entries of the repository still in flight (authorized
	// or committing) and unknown ones.
	EntryCounts(ctx context.Context, repositoryID string) (EntryCounts, error)
	// ResetHead lowers the known journal head of the repository to head and
	// records who did it, why and the head it replaces (migration 015; the
	// database refuses any other decrease). The caller has established that the
	// volume was restored and that nothing is in flight.
	ResetHead(ctx context.Context, scope domain.Scope, repositoryID string, head int64, reset HeadReset) error
	// Grants is the grant transaction joined to this transaction, so seed
	// consumption and the push record commit together (gitauth.ConsumeSeedIn).
	Grants() gitauth.GrantTx
}

// EntryCounts are the entries of one repository that block or wait.
type EntryCounts struct {
	InFlight int
	Unknown  int
}

// HeadReset is the audited acknowledgement of a restored volume.
type HeadReset struct {
	By     string
	Reason string
	At     time.Time
}

// ---- peer modules (injected; missing ones fail closed) ----

// RepositoryFacts is the repository module's answer for a push.
type RepositoryFacts struct {
	ID            string
	Scope         domain.Scope
	DefaultBranch string
	Hosting       gitv1.Hosting
	Status        gitv1.RepositoryStatus
	// DirectPushToDefault is policy.directPushToDefault (default false).
	DirectPushToDefault bool
}

// RepositoryState is the part of a repository's row that decides a push: what
// Tx.RepositoryState returns and what the policy was evaluated under.
type RepositoryState struct {
	Hosting             gitv1.Hosting
	Status              gitv1.RepositoryStatus
	DefaultBranch       string
	DirectPushToDefault bool
}

// State is the snapshot a push is evaluated under.
func (f RepositoryFacts) State() RepositoryState {
	return RepositoryState{Hosting: f.Hosting, Status: f.Status, DefaultBranch: f.DefaultBranch, DirectPushToDefault: f.DirectPushToDefault}
}

// Repositories is gitpush's view of the repository lifecycle module (T-127).
type Repositories interface {
	// Repository returns the repository of the tenant (the worker-facing read: the
	// project is whatever owns it). A foreign or unknown one is ErrNotFound.
	Repository(ctx context.Context, tenant domain.TenantID, id string) (RepositoryFacts, error)
	// ReportEmpty tells the repository module that its emptiness changed
	// (repository.Service.ReportEmpty); an older observation never wins.
	ReportEmpty(ctx context.Context, tenant domain.TenantID, id string, empty bool, observedAt time.Time) error
}

// Grants is gitpush's view of gitauth (T-136): the effective claims of a grant
// with the authority that can change after issuance re-checked (human roles,
// attempt liveness, expiry, revocation). *gitauth.Service implements it. The
// seed consumption is not a method here: it runs inside the authorization
// transaction through Tx.Grants.
type Grants interface {
	Claims(ctx context.Context, tenant domain.TenantID, grantID string) (gitv1.GrantClaims, error)
}

// Attempts is execution's authoritative view of whether an attempt is live
// (neither cancelled, finished, revoked nor superseded).
type Attempts interface {
	// Live reports the attempt's liveness; an unknown attempt of the scope is
	// ErrNotFound (not live).
	Live(ctx context.Context, scope domain.Scope, attemptID string) (bool, error)
}

// Reservations is the workspace module's view of writer reservations (WS-03,
// provider T-147).
type Reservations interface {
	// HoldsWriter reports whether the attempt currently holds the work item's
	// writer reservation.
	HoldsWriter(ctx context.Context, scope domain.Scope, workItemID, attemptID string) (bool, error)
}

// Worker is the reconciler's read access to the tenant's Git worker. It is the
// evidence subset of repository.Hosts, which implements it. Every error leaves
// the entry unchanged for the next pass: an Unknown or Unavailable call is never
// an outcome.
type Worker interface {
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	ReadJournal(ctx context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error)
	ReceiveLiveness(ctx context.Context, tenant domain.TenantID, repositoryID, receiveID string) (gitv1.ReceiveLivenessResponse, error)
}

// ReadAuthorizer decides whether a person may read the push records of a
// repository (the project's read roles, composed by T-132). It returns nil to
// allow and ErrDenied or ErrNotFound to refuse; any other error refuses too.
type ReadAuthorizer interface {
	AuthorizeRead(ctx context.Context, principal string, scope domain.Scope, repositoryID string) error
}

// ResolveAuthorizer decides whether a person may resolve unknown push entries of
// a repository: the project's administrators (the composition maps it to the
// admin role; admin gains no push or review authority from it). It returns nil
// to allow and ErrDenied or ErrNotFound to refuse; any other error refuses too.
type ResolveAuthorizer interface {
	AuthorizeResolve(ctx context.Context, principal string, scope domain.Scope, repositoryID string) error
}

type requestIDKey struct{}

// WithRequestID attaches the request/correlation identifier recorded in audit
// and events.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the identifier set by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
