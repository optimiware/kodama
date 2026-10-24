package deliverycleanup

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Failure classes of the record port. Adapters map their module's errors onto
// them; anything else is an outage and is retried by the next tick.
var (
	// ErrNotSettled: the deployment is still applying, so no cleanup can be
	// requested for it yet.
	ErrNotSettled = errors.New("deliverycleanup: the deployment is not settled")
	// ErrNotFound: the deployment is missing, foreign or hidden.
	ErrNotFound = errors.New("deliverycleanup: deployment not found")
	// ErrNoDefinition: the scope has no cleanup definition to run.
	ErrNoDefinition = errors.New("deliverycleanup: no cleanup definition")
)

// RecordState is the cleanup dimension of a deployment record.
type RecordState string

const (
	RecordNone      RecordState = ""
	RecordRequested RecordState = "requested"
	RecordFailed    RecordState = "failed"
	RecordUnknown   RecordState = "unknown"
	RecordDone      RecordState = "done"
)

// Candidate is what the scheduler needs to know of a recorded deployment. It
// carries no manifests, credentials or object contents: the cleanup run reads
// the owned objects and retained configuration from the record itself.
type Candidate struct {
	Scope      domain.Scope
	Deployment string
	ActionKey  string
	// ExpiresAt is the effective expiry computed once at registration.
	ExpiresAt *time.Time
	// RequestedAt is when cleanup was requested; nil before.
	RequestedAt *time.Time
	State       RecordState
	Deleted     bool
}

// Records is the deployment record boundary (T-035) the scheduler consumes.
// Every method is scoped: another scope's deployment is ErrNotFound.
type Records interface {
	// Scan pages the scope's undeleted deployments in a stable order (by
	// identity) after the cursor ("" starts), whatever their expiry or cleanup
	// state; next is "" at the end. One resumable scan is how both expiry and
	// requested cleanups are discovered, so records that stay blocked (still
	// applying, unknown outcome, retries exhausted) cannot starve later ones.
	Scan(ctx context.Context, scope domain.Scope, after string, limit int) (page []Candidate, next string, err error)
	// Get reads one deployment.
	Get(ctx context.Context, scope domain.Scope, deployment string) (Candidate, error)
	// RequestCleanup records the cleanup request. It is idempotent; a deployment
	// that is still applying is ErrNotSettled.
	RequestCleanup(ctx context.Context, scope domain.Scope, deployment string) error
}

// Runs is the execution boundary: bounded rounds of cleanup runs, each with a
// stable identity derived from the deployment. *execution.Service implements it.
type Runs interface {
	CleanupRounds(ctx context.Context, scope domain.Scope, deployment string, max int) ([]execution.CleanupRun, error)
	AdmitCleanupRound(ctx context.Context, cmd execution.CleanupCommand, round int) (*execution.Run, bool, error)
}

var _ Runs = (*execution.Service)(nil)

// Definitions names the project's cleanup pipeline definition. Which image and
// script it runs is the project's own (T-065); the scheduler only chooses
// which recorded deployment it is admitted for.
type Definitions interface {
	CleanupDefinition(ctx context.Context, scope domain.Scope) (domain.DefinitionID, error)
}

// Scopes enumerates the project scopes to sweep, ordered, after a cursor.
type Scopes interface {
	Scopes(ctx context.Context, after *domain.Scope, limit int) ([]domain.Scope, error)
}
