package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/workspace"
)

// Preparer is the core's admission use case. *execution.Service satisfies it.
type Preparer interface {
	Prepare(ctx context.Context, caller execution.Caller, cmd execution.AdmitCommand) (*execution.AdmissionPlan, error)
}

// WorkspaceReader reads the workspace a run will be admitted against. The
// workspace module's store satisfies it. It returns workspace.ErrNotFound for a
// missing or foreign workspace.
type WorkspaceReader interface {
	Get(ctx context.Context, scope domain.Scope, id workspace.ID) (*workspace.Workspace, error)
}

// PlannerConfig wires the ExecutionPlanner.
type PlannerConfig struct {
	// Preparer authorizes the installing principal and pins the definition,
	// work item, source commit and configuration.
	Preparer Preparer
	// Work reads the work item's current content revision, the revision the
	// admission transaction re-verifies under the item lock (the core's
	// execution.WorkContent).
	Work execution.WorkContent
	// Repositories is the project's current repository binding. A push for
	// another repository than the binding is refused, so the run's source
	// identity is always the one the delivery verified.
	Repositories workspace.Repositories
	// Workspaces verifies that the workspace admission selected for the run (the
	// work item's existing one, whatever the current binding says) belongs to
	// the verified repository, so a rebound project can never run against a
	// workspace of the old one.
	Workspaces WorkspaceReader
}

// ExecutionPlanner maps a verified push onto the core's run admission: an
// exact-commit work run of the trigger's definition on the trigger's work item,
// requested by the installing principal, whose current authority admission
// re-checks. The hook credential never appears here and no payload field other
// than the verified commit reaches admission.
type ExecutionPlanner struct{ cfg PlannerConfig }

var _ Planner = (*ExecutionPlanner)(nil)

// NewExecutionPlanner requires every collaborator: a missing one fails
// construction rather than admitting without its check.
func NewExecutionPlanner(cfg PlannerConfig) (*ExecutionPlanner, error) {
	if cfg.Preparer == nil || cfg.Work == nil || cfg.Repositories == nil || cfg.Workspaces == nil {
		return nil, errors.New("gitwebhook/adapters: a preparer, work content, repository binding and workspace reader are required")
	}
	return &ExecutionPlanner{cfg: cfg}, nil
}

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", gw.ErrRunRefused, fmt.Sprintf(format, args...))
}

// maxRequestID bounds the request identifier recorded with the run's audit.
const maxRequestID = 256

// Plan implements Planner.
func (p *ExecutionPlanner) Plan(ctx context.Context, req gw.RunRequest) (*execution.AdmissionPlan, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	if err := req.Commit.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	if req.RequestedBy == "" {
		return nil, fmt.Errorf("%w: the requesting principal is required", gw.ErrInvalid)
	}
	if req.WorkItemID == "" {
		return nil, refusedf("the trigger names no work item to attach the run to")
	}
	cfg, err := p.cfg.Repositories.Repository(ctx, req.Scope)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return nil, refusedf("the project has no repository binding")
	case err != nil:
		return nil, err
	case cfg.ID != req.RepositoryID:
		return nil, refusedf("the project's repository is not the one the delivery verified")
	}
	snap, err := p.cfg.Work.Snapshot(ctx, req.Scope, req.WorkItemID)
	if err != nil {
		return nil, ClassifyAdmission(err)
	}
	requestID := req.IdempotencyKey
	if len(requestID) > maxRequestID {
		requestID = requestID[:maxRequestID]
	}
	plan, err := p.cfg.Preparer.Prepare(ctx, execution.Caller{PrincipalID: req.RequestedBy, RequestID: requestID}, execution.AdmitCommand{
		Scope: req.Scope, WorkItemID: req.WorkItemID, DefinitionID: req.DefinitionID,
		ContentRevision: snap.ContentRevision, SourceCommit: string(req.Commit),
	})
	if err != nil {
		return nil, ClassifyAdmission(err)
	}
	if err := p.verifySource(ctx, req, cfg, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// verifySource refuses a plan whose workspace is not the verified repository's:
// the repository identity and URL recorded on the workspace must equal the
// delivery's verified RepositoryID and the project's current binding. It runs
// before the ledger admits, so a mismatch creates nothing.
func (p *ExecutionPlanner) verifySource(ctx context.Context, req gw.RunRequest, cfg workspace.RepositoryConfig, plan *execution.AdmissionPlan) error {
	if plan == nil || plan.Aggregate == nil || plan.Aggregate.Run == nil {
		return fmt.Errorf("%w: admission returned no run plan", gw.ErrUnavailable)
	}
	id := plan.Aggregate.Run.WorkspaceID
	if id == "" || (plan.Source.WorkspaceID != "" && plan.Source.WorkspaceID != id) {
		return refusedf("the run has no workspace of the verified repository")
	}
	ws, err := p.cfg.Workspaces.Get(ctx, req.Scope, workspace.ID(id))
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return refusedf("the run's workspace is not in the scope")
	case err != nil:
		return fmt.Errorf("%w: workspace lookup: %w", gw.ErrUnavailable, err)
	}
	if ws.Scope != req.Scope || ws.RepositoryID != req.RepositoryID || ws.RepositoryURL != cfg.URL ||
		(plan.Source.RepositoryURL != "" && plan.Source.RepositoryURL != cfg.URL) {
		return refusedf("the run's workspace belongs to another repository than the verified one")
	}
	return nil
}
