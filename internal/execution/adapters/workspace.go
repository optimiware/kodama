package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/workspace"
)

// WorkspaceService is the part of the workspace module's use cases execution
// consumes: writer-tip synchronization and retained source references.
type WorkspaceService interface {
	Sync(ctx context.Context, actor workspace.Actor, scope domain.Scope, id workspace.ID, ifMatch domain.Version) (*workspace.Workspace, error)
	PinCommit(ctx context.Context, actor workspace.Actor, in workspace.PinInput) (*workspace.RetainedRef, error)
}

// Sources implements execution.Sources over the workspace module. It performs
// all Git observation outside SQL transactions; the dispatch transaction
// rechecks the observed tip under the workspace lock. The workspace module's
// authorizer must permit the execution service actor for workspace.sync and
// workspace.pin.
type Sources struct {
	store workspace.Store
	refs  workspace.RefReader
	svc   WorkspaceService
}

var _ execution.Sources = (*Sources)(nil)

var service = workspace.Actor{Type: workspace.ActorService, ID: "execution"}

// NewSources requires every collaborator.
func NewSources(store workspace.Store, refs workspace.RefReader, svc WorkspaceService) (*Sources, error) {
	if store == nil || refs == nil || svc == nil {
		return nil, errors.New("execution/adapters: a workspace store, ref reader and service are required")
	}
	return &Sources{store: store, refs: refs, svc: svc}, nil
}

func mapWorkspace(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrNotFound):
		return execution.ErrNotFound
	case errors.Is(err, workspace.ErrDiverged), errors.Is(err, workspace.ErrPublicationPending), errors.Is(err, workspace.ErrAdmissionClosed),
		errors.Is(err, workspace.ErrFenced):
		return fmt.Errorf("%w: %s is not in a state that admits work", execution.ErrStale, what)
	case errors.Is(err, workspace.ErrInvalidTransition), errors.Is(err, workspace.ErrRetentionBusy):
		return execution.Refusedf("the workspace cannot take this run right now")
	}
	return execution.Unavailablef(err, "the workspace is unavailable")
}

// Verify implements execution.Sources: the commit must be the workspace's
// accepted commit or in its branch history. A commit outside the history is
// rejected before any run exists.
func (s *Sources) Verify(ctx context.Context, scope domain.Scope, item domain.WorkItemID, commit string) (execution.SourceInfo, error) {
	w, err := s.store.GetByWorkItem(ctx, scope, item)
	if err != nil {
		return execution.SourceInfo{}, mapWorkspace(err, "the work item's workspace")
	}
	if w.State == workspace.StateProvisioning {
		return execution.SourceInfo{}, execution.Refusedf("the workspace is still provisioning")
	}
	info := execution.SourceInfo{WorkspaceID: string(w.ID), RepositoryURL: w.RepositoryURL, Branch: w.WorkBranch, AcceptedTip: string(w.AcceptedCommit)}
	if commit == string(w.AcceptedCommit) {
		return info, nil
	}
	repo := workspace.RepoRef{Scope: scope, URL: w.RepositoryURL, CheckoutSecretRef: w.CheckoutSecretRef, PublisherSecretRef: w.PublisherSecretRef}
	bi, err := s.refs.BranchContains(ctx, repo, w.WorkBranch, workspace.Commit(commit))
	if err != nil {
		return execution.SourceInfo{}, execution.Unavailablef(err, "the remote history is unavailable")
	}
	if !bi.Exists || !bi.Contains {
		return execution.SourceInfo{}, execution.Refusedf("the source commit is not in the workspace history")
	}
	return info, nil
}

// Describe implements execution.Sources.
func (s *Sources) Describe(ctx context.Context, scope domain.Scope, workspaceID string, refresh bool) (execution.SourceInfo, error) {
	id := workspace.ID(workspaceID)
	var w *workspace.Workspace
	var err error
	if refresh {
		w, err = s.svc.Sync(ctx, service, scope, id, 0)
	} else {
		w, err = s.store.Get(ctx, scope, id)
	}
	if err != nil {
		return execution.SourceInfo{}, mapWorkspace(err, "the workspace")
	}
	return execution.SourceInfo{WorkspaceID: string(w.ID), RepositoryURL: w.RepositoryURL, Branch: w.WorkBranch, AcceptedTip: string(w.AcceptedCommit)}, nil
}

// Retain implements execution.Sources: it creates and confirms the run-bound
// retained reference (idempotently for the run and commit) and returns its
// identifier.
func (s *Sources) Retain(ctx context.Context, scope domain.Scope, workspaceID string, run domain.RunID, commit string) (string, error) {
	pin, err := s.svc.PinCommit(ctx, service, workspace.PinInput{Scope: scope, WorkspaceID: workspace.ID(workspaceID), Commit: workspace.Commit(commit), RunID: run})
	if err != nil {
		return "", mapWorkspace(err, "the workspace")
	}
	if pin.Status != workspace.PinConfirmed {
		return "", execution.Unavailablef(nil, "the retained reference is not confirmed")
	}
	return string(pin.ID), nil
}
