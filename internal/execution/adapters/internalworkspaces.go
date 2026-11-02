package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/workspace"
)

// InternalWorkspaceService is the part of the workspace module's use cases the
// internal-hosting path of execution consumes (T-147).
type InternalWorkspaceService interface {
	ProvisionInternal(ctx context.Context, actor workspace.Actor, scope domain.Scope, item domain.WorkItemID, repos []workspace.RepositoryID) (*workspace.Workspace, []*workspace.RepositoryAssociation, bool, error)
	Sync(ctx context.Context, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)
	CheckWriterGateAt(ctx context.Context, scope domain.Scope, id workspace.ID) (domain.Version, error)
	AcceptCheckpoint(ctx context.Context, actor workspace.Actor, in workspace.CheckpointInput) (workspace.CheckpointResult, error)
}

// InternalWorkspaces implements execution.InternalWorkspaces over the workspace
// module. It makes every Git observation outside SQL (the workspace service does)
// and answers the writer claim with the evidence the claim transaction rechecks:
// the workspace version the gate was checked at, which
// workspace.ReserveInternalWriterMutation refuses to reserve against once the
// workspace changed. The workspace authorizer must permit the execution service
// actor for workspace.provision, workspace.sync, workspace.admit and
// workspace.accept_checkpoint.
type InternalWorkspaces struct {
	store workspace.Store
	svc   InternalWorkspaceService
}

var (
	_ execution.InternalWorkspaces = (*InternalWorkspaces)(nil)
	_ InternalWorkspaceService     = (*workspace.Service)(nil)
)

// NewInternalWorkspaces requires every collaborator.
func NewInternalWorkspaces(store workspace.Store, svc InternalWorkspaceService) (*InternalWorkspaces, error) {
	if store == nil || svc == nil {
		return nil, errors.New("execution/adapters: a workspace store and service are required")
	}
	return &InternalWorkspaces{store: store, svc: svc}, nil
}

// mapInternal classifies a workspace failure of the internal-hosting path: an
// unknown outcome, an unsettled push, a held slot or a closed admission make the
// run wait (ErrBusy); a changed workspace repeats the observation (ErrStale); a
// refused output is an admission refusal; everything else is an outage.
func mapInternal(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrNotFound):
		return execution.ErrNotFound
	case errors.Is(err, workspace.ErrStale), errors.Is(err, workspace.ErrVersionConflict):
		return fmt.Errorf("%w: the workspace changed while it was observed", execution.ErrStale)
	case errors.Is(err, workspace.ErrWriterBlocked), errors.Is(err, workspace.ErrPushPending), errors.Is(err, workspace.ErrWriterBusy),
		errors.Is(err, workspace.ErrAdmissionClosed), errors.Is(err, workspace.ErrFenced), errors.Is(err, workspace.ErrCheckpointRequired),
		errors.Is(err, workspace.ErrDiverged), errors.Is(err, workspace.ErrRepositoryNotReady):
		return fmt.Errorf("%w: the workspace does not admit a writer right now", execution.ErrBusy)
	case errors.Is(err, workspace.ErrNotActionable), errors.Is(err, workspace.ErrNotConfigured):
		return execution.Refusedf("the workspace cannot take this run")
	case errors.Is(err, workspace.ErrSecretInSource), errors.Is(err, workspace.ErrPushNotApplied), errors.Is(err, workspace.ErrNotWriter),
		errors.Is(err, workspace.ErrInvalidValue), errors.Is(err, workspace.ErrAttemptRevoked):
		return execution.Refusedf("the %s was refused", what)
	}
	return execution.Unavailablef(err, "the workspace is unavailable")
}

// Provision implements execution.InternalWorkspaces.
func (w *InternalWorkspaces) Provision(ctx context.Context, scope domain.Scope, item domain.WorkItemID, repositories []string) (execution.InternalWorkspace, error) {
	repos := make([]workspace.RepositoryID, len(repositories))
	for i, r := range repositories {
		repos[i] = workspace.RepositoryID(r)
	}
	ws, _, _, err := w.svc.ProvisionInternal(ctx, service, scope, item, repos)
	if err != nil {
		return execution.InternalWorkspace{}, mapInternal(err, "workspace")
	}
	return execution.InternalWorkspace{ID: string(ws.ID)}, nil
}

// AdmitWriter implements execution.InternalWorkspaces: it synchronizes the task
// branches (adopting a human's fast-forward, refusing a rewrite or an unaccepted
// workload push), checks the writer gate and returns the version it was read at
// with the accepted commit of each created task branch.
func (w *InternalWorkspaces) AdmitWriter(ctx context.Context, scope domain.Scope, workspaceID string) (execution.WriterAdmission, error) {
	id := workspace.ID(workspaceID)
	if _, err := w.svc.Sync(ctx, service, scope, id, 0); err != nil {
		return execution.WriterAdmission{}, mapInternal(err, "workspace")
	}
	version, err := w.svc.CheckWriterGateAt(ctx, scope, id)
	if err != nil {
		return execution.WriterAdmission{}, mapInternal(err, "workspace")
	}
	agg, err := w.store.View(ctx, scope, id, workspace.Load{Associations: true})
	if err != nil {
		return execution.WriterAdmission{}, mapInternal(err, "workspace")
	}
	out := execution.WriterAdmission{Version: version, Accepted: map[string]string{}}
	for _, a := range agg.Associations {
		if a.State == workspace.AssociationCreated {
			out.Accepted[string(a.RepositoryID)] = string(a.AcceptedCommit)
		}
	}
	return out, nil
}

// AcceptCheckpoint implements execution.InternalWorkspaces: the writer's
// task-branch commit is accepted from its push record, with the prior-disclosure
// scan, before the writer slot is settled.
func (w *InternalWorkspaces) AcceptCheckpoint(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, out execution.CommitOutput) error {
	_, err := w.svc.AcceptCheckpoint(ctx, service, workspace.CheckpointInput{
		Scope: scope, AttemptID: attempt, RepositoryID: workspace.RepositoryID(out.RepositoryID), Ref: out.Ref,
		Commit: workspace.Commit(out.Commit), PushID: out.PushID,
	})
	return mapInternal(err, "checkpoint")
}
