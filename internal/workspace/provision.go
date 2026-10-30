package workspace

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
)

// Provision returns the one workspace of an actionable work item, creating it
// if needed. The unique reservation is committed before the branch is created;
// the branch is created with an expected-absent update and a lost creation
// response is reconciled without overwriting an independently existing branch.
// created reports whether this call made the reservation. A workspace that
// could not finish provisioning is returned in provisioning state with a safe
// LastError; calling again (or the reconciler) retries.
func (s *Service) Provision(ctx context.Context, actor Actor, scope domain.Scope, item domain.WorkItemID) (w *Workspace, created bool, err error) {
	if err := item.Validate(); err != nil {
		return nil, false, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionProvision, scope, Resource{"work_item", string(item)}); err != nil {
		return nil, false, err
	}
	switch err := s.d.WorkItems.Actionable(ctx, scope, item); {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotActionable):
		return nil, false, err
	default:
		return nil, false, unavailable("work item", err)
	}

	w, err = s.d.Store.GetByWorkItem(ctx, scope, item)
	if errors.Is(err, ErrNotFound) {
		cfg, cerr := s.d.Repositories.Repository(ctx, scope)
		if cerr != nil {
			return nil, false, unavailable("repository configuration", cerr)
		}
		nw, nerr := New(NewParams{
			ID: ID(s.d.NewID("ws")), Scope: scope, WorkItemID: item, RepositoryID: cfg.ID, RepositoryURL: cfg.URL,
			CheckoutSecretRef: cfg.CheckoutSecretRef, PublisherSecretRef: cfg.PublisherSecretRef,
		}, s.now())
		if nerr != nil {
			return nil, false, nerr
		}
		w, created, err = s.d.Store.Insert(ctx, nw, Effects{
			Audit:  []Audit{audit(actor, "workspace.provision_requested", "", map[string]any{"workItemId": string(item), "branch": nw.WorkBranch})},
			Events: []Event{stateEvent(actor, "none", string(StateProvisioning), "provision_requested")},
		})
	}
	if err != nil {
		return nil, false, err
	}
	if w.State == StateProvisioning {
		w, err = s.completeProvisioning(ctx, actor, w)
		if err != nil {
			return nil, created, err
		}
	}
	return w, created, nil
}

// completeProvisioning drives a provisioning workspace toward available. Remote
// failures are recorded as safe reasons and leave it provisioning.
func (s *Service) completeProvisioning(ctx context.Context, actor Actor, w *Workspace) (*Workspace, error) {
	if w.Internal() {
		// An internally hosted workspace is provisioned by its associations,
		// never through the external publisher.
		if err := s.needHosting(); err != nil {
			return nil, err
		}
		cur, _, err := s.driveInternal(ctx, actor, w.Scope, w.ID)
		return cur, err
	}
	cfg, err := s.d.Repositories.Repository(ctx, w.Scope)
	if err != nil {
		return s.provisionFailed(ctx, actor, w, ReasonRepositoryUnready)
	}
	repo := s.repoRef(w)
	rctx, cancel := s.gitCtx(ctx)
	defer cancel()

	// 1. Record the intended initial commit before touching the branch.
	if w.InitialCommit == "" {
		base, ok, err := s.d.Refs.ResolveRef(rctx, repo, cfg.BaseRef)
		if err != nil || !ok {
			return s.provisionFailed(ctx, actor, w, ReasonRepositoryUnready)
		}
		agg, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{}, func(a *Aggregate) (Effects, error) {
			if a.Workspace.State != StateProvisioning {
				return Effects{}, nil
			}
			if _, err := a.Workspace.RecordInitialIntent(base, s.now()); err != nil {
				return Effects{}, err
			}
			return Effects{}, nil
		})
		if err != nil {
			return nil, err
		}
		w = agg.Workspace
		if w.State != StateProvisioning {
			return w, nil
		}
	}

	// 2. Create (or adopt) the branch at the recorded commit.
	tip, exists, err := s.d.Refs.ResolveRef(rctx, repo, w.WorkBranch)
	if err != nil {
		return s.provisionFailed(ctx, actor, w, ReasonProvisionFailed)
	}
	if !exists {
		if cerr := s.d.Publisher.CreateRef(rctx, repo, w.WorkBranch, w.InitialCommit, cfg.BaseRef); cerr != nil {
			// A lost response may hide a successful creation: read it back.
			tip, exists, err = s.d.Refs.ResolveRef(rctx, repo, w.WorkBranch)
			if err != nil || !exists {
				return s.provisionFailed(ctx, actor, w, ReasonProvisionFailed)
			}
		} else {
			tip, exists = w.InitialCommit, true
		}
	}
	if exists && tip != w.InitialCommit {
		// Our branch can only be ahead of the recorded commit through
		// later human work; anything else is an independent branch.
		info, err := s.d.Refs.BranchContains(rctx, repo, w.WorkBranch, w.InitialCommit)
		if err != nil {
			return s.provisionFailed(ctx, actor, w, ReasonProvisionFailed)
		}
		if !info.Contains {
			return s.provisionFailed(ctx, actor, w, ReasonBranchExists)
		}
	}

	// 3. Make it available at the confirmed initial commit.
	agg, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{}, func(a *Aggregate) (Effects, error) {
		if a.Workspace.State != StateProvisioning {
			return Effects{}, nil // a concurrent provision completed it
		}
		if err := a.Workspace.CompleteProvisioning(a.Workspace.InitialCommit, s.now()); err != nil {
			return Effects{}, err
		}
		return Effects{
			Audit:  []Audit{audit(actor, "workspace.provisioned", "", map[string]any{"commit": string(a.Workspace.AcceptedCommit), "branch": a.Workspace.WorkBranch})},
			Events: []Event{stateEvent(actor, string(StateProvisioning), string(StateAvailable), "branch_ready")},
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return agg.Workspace, nil
}

func (s *Service) provisionFailed(ctx context.Context, actor Actor, w *Workspace, reason string) (*Workspace, error) {
	agg, err := s.d.Store.Update(ctx, w.Scope, w.ID, Load{}, func(a *Aggregate) (Effects, error) {
		if a.Workspace.State != StateProvisioning {
			return Effects{}, nil
		}
		changed, err := a.Workspace.RecordProvisioningError(reason, s.now())
		if err != nil || !changed {
			return Effects{}, err
		}
		return Effects{Audit: []Audit{audit(actor, "workspace.provision_failed", reason, nil)}}, nil
	})
	if err != nil {
		return nil, err
	}
	return agg.Workspace, nil
}
