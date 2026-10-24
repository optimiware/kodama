package workspace

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/domain"
)

// Sync refreshes the remote branch tip and records an authorized human
// fast-forward as the accepted commit, keeping the prior commits reachable
// through their pins. It never merges, rewinds or adopts unrelated history:
// a diverged branch is recorded as a safe reason and reported as ErrDiverged.
// ifMatch, when non-zero, is compared atomically with the write.
func (s *Service) Sync(ctx context.Context, actor Actor, scope domain.Scope, id ID, ifMatch domain.Version) (*Workspace, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionSync, scope, Resource{"workspace", string(id)}); err != nil {
		return nil, err
	}
	return s.observe(ctx, actor, scope, id, ifMatch)
}

// observe is the synchronization step shared by Sync and writer admission.
func (s *Service) observe(ctx context.Context, actor Actor, scope domain.Scope, id ID, ifMatch domain.Version) (*Workspace, error) {
	var out *Workspace
	err := retry(3, func() error {
		w, err := s.d.Store.Get(ctx, scope, id)
		if err != nil {
			return err
		}
		if err := w.CheckVersion(ifMatch); err != nil {
			return err
		}
		if w.State != StateAvailable {
			return w.transitionErr("synchronize")
		}
		if w.Pending != nil {
			return ErrPublicationPending
		}
		rctx, cancel := s.gitCtx(ctx)
		defer cancel()
		info, err := s.d.Refs.BranchContains(rctx, s.repoRef(w), w.WorkBranch, w.AcceptedCommit)
		if err != nil {
			return unavailable("remote branch", err)
		}
		from := w.AcceptedCommit
		var divergence error
		agg, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
			if err := a.Workspace.CheckVersion(ifMatch); err != nil {
				return Effects{}, err
			}
			// A missing remote branch is recorded, never recreated or adopted.
			if !info.Exists {
				changed, err := a.Workspace.RecordBranchMissing(from, s.now())
				if !errors.Is(err, ErrDiverged) {
					return Effects{}, err
				}
				divergence = err
				if !changed {
					return Effects{}, nil
				}
				return Effects{Audit: audit1(actor, "workspace.sync_diverged", ReasonBranchMissing)}, nil
			}
			tip := info.Tip
			changed, err := a.Workspace.ObserveHumanTip(from, tip, info.Contains, s.now())
			if errors.Is(err, ErrDiverged) {
				divergence = err
				return Effects{Audit: audit1(actor, "workspace.sync_diverged", ReasonBranchDiverged)}, nil
			}
			if err != nil {
				return Effects{}, err
			}
			if !changed {
				return Effects{}, nil
			}
			if tip == from {
				return Effects{Audit: audit1(actor, "workspace.sync_cleared", "")}, nil
			}
			return Effects{
				Audit:  []Audit{audit(actor, "workspace.synchronized", "", map[string]any{"previousCommit": string(from), "commit": string(tip)})},
				Events: []Event{{Kind: EventCheckpointCreated, Actor: actor, PreviousCommit: from, NewCommit: tip, HumanSynchronization: true}},
			}, nil
		})
		if err != nil {
			return err
		}
		out = agg.Workspace
		return divergence
	})
	return out, err
}

func audit1(actor Actor, action, reason string) []Audit {
	return []Audit{audit(actor, action, reason, nil)}
}
