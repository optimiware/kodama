package logs

import (
	"context"
	"io"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and owned by this module. Adapters implement them
// (identity, execution, artifacts, Kubernetes, PostgreSQL); the composition root
// wires them. Nothing has a permissive default: a missing port is
// ErrNotConfigured and the operation fails closed.

// Authority answers the current read question for diagnostics on every call,
// including reconnects and retained reads. It returns nil to allow,
// ErrNotFound to hide the scope, ErrDenied to refuse, and any other error for
// an outage, which is never access and never an alternate authority.
type Authority interface {
	AuthorizeRead(ctx context.Context, principal string, scope domain.Scope) error
}

// AttemptInfo is the trusted description of one attempt.
type AttemptInfo struct {
	Scope   domain.Scope
	Run     domain.RunID
	Step    string
	ID      domain.AttemptID
	Ordinal int
	// JobName is the expected Job identity; JobUID is the observed Job UID
	// ("" until execution observed the Job).
	JobName string
	JobUID  string
	// Terminal reports that the attempt reached a final status.
	Terminal bool
	// WorkItem and Workspace are correlation identifiers, when the run has them.
	WorkItem  domain.WorkItemID
	Workspace string
}

// Attempts resolves attempts through the execution module's scoped lookups. An
// absent, foreign or mismatched reference is ErrNotFound.
type Attempts interface {
	// Attempt returns the attempt, verifying that the run in scope owns it.
	Attempt(ctx context.Context, scope domain.Scope, run domain.RunID, attempt domain.AttemptID) (AttemptInfo, error)
	// ByID returns the attempt of the scope (internal collection path).
	ByID(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (AttemptInfo, error)
	// Attempts lists every attempt of the run in scope, in creation order.
	Attempts(ctx context.Context, scope domain.Scope, run domain.RunID) ([]AttemptInfo, error)
}

// Target is the trusted Kubernetes workload of an attempt. Namespace and
// Container come from composition, the Job identity from persisted execution
// state; nothing is taken from a caller.
type Target struct {
	Namespace string
	JobName   string
	JobUID    string
	Container string
}

// Workloads resolves the Kubernetes target of an attempt. An attempt whose Job
// was not observed yields ErrWorkloadNotObserved.
type Workloads interface {
	Target(ctx context.Context, attempt AttemptInfo) (Target, error)
}

// LiveQuery are the bounds sent to the Kubernetes log subresource.
type LiveQuery struct {
	TailLines  int64
	LimitBytes int64
	Follow     bool
}

// LiveReader is the narrow Kubernetes log-reader port. Implementations verify
// the workload's ownership before opening the log and return ErrWorkloadMissing,
// ErrWorkloadMismatch or ErrUnavailable; they never return provider errors to
// callers. The caller closes the stream.
type LiveReader interface {
	Open(ctx context.Context, target Target, q LiveQuery) (io.ReadCloser, error)
}

// Retained is the module's view of retained log artifacts.
type Retained interface {
	// Open returns verified retained bytes of an artifact that is a scoped,
	// same-attempt, usable diagnostic file whose ID, digest and size equal ref.
	Open(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ref ArtifactRef) (io.ReadCloser, error)
	// WrapperLog finds the wrapper's command log of the attempt and, when a
	// valid, bound sidecar exists, its completeness statement. A log without a
	// sidecar has unknown completeness. ErrNoLog when the attempt has none.
	WrapperLog(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (Link, error)
	// Usable reports whether the linked artifact is still readable (not removed
	// by retention). A scoped, same-attempt mismatch is ErrNotFound.
	Usable(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ref ArtifactRef) (bool, error)
	// Store retains one frozen controller capture as a diagnostic log plus its
	// sidecar. It is idempotent: repeating it with the same snapshot returns the
	// same link, recovering artifacts that were reserved, uploaded or committed
	// before an interruption. A nil body means the process restarted and the
	// bytes must come from the already-uploaded object; when they are gone it
	// returns ErrFrozenBytesLost.
	Store(ctx context.Context, info AttemptInfo, snap Snapshot, body io.ReadSeeker) (Link, error)
}

// Audit is the safe, content-free description of a record change written in
// the same transaction as the change.
type Audit struct {
	Action    string
	RequestID string
}

// Mutation changes the locked record. It is pure: no network or process calls.
// Returning an error rolls the transaction back; the audit is written only when
// the mutation changed state (changed true).
type Mutation func(r *Record) (changed bool, audit Audit, err error)

// Store persists capture records. One call is one transaction; implementations
// are tenant scoped, serialize claims per (attempt, source) and linkage per
// record, and recheck artifact ownership when linkage commits.
type Store interface {
	// Ensure returns the latest record of (attempt, source), creating
	// generation 1 when none exists. Concurrent callers get the same record.
	Ensure(ctx context.Context, tmpl Record) (*Record, error)
	// Get returns a record of the scope.
	Get(ctx context.Context, scope domain.Scope, id RecordID) (*Record, error)
	// ForAttempt lists the attempt's records, ordered by source and generation.
	ForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]*Record, error)
	// Update runs fn on the row under lock and saves the result with its audit.
	Update(ctx context.Context, scope domain.Scope, id RecordID, fn Mutation) (*Record, error)
	// Create inserts the next-generation record of a capture; an identical
	// existing generation is returned unchanged.
	Create(ctx context.Context, r *Record) (*Record, error)
}

// Redactions supplies the known injected secret values to remove from the
// attempt's output. It reads values the attempt was already given; it does not
// grant any new secret-read authority. An error fails the read closed.
type Redactions interface {
	Values(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([][]byte, error)
}

// IDs generates record identifiers and claim tokens.
type IDs interface {
	NewRecordID() RecordID
	NewToken() string
}

// Clock returns the current time.
type Clock func() time.Time
