package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Archive durably closes source admission first, then settles the current
// publication, and only then marks the workspace archived. If settlement is
// still uncertain at ArchiveWait the archive request is retained (it never
// reports completion early), the reconciler finishes it later, and the returned
// workspace accompanies ErrArchiveUncertain. ifMatch, when non-zero, is
// compared atomically with the admission-closing write; a retry after the
// request was recorded must use the new version.
func (s *Service) Archive(ctx context.Context, actor Actor, scope domain.Scope, id ID, ifMatch domain.Version) (*Workspace, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionArchive, scope, Resource{"workspace", string(id)}); err != nil {
		return nil, err
	}
	agg, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
		if err := a.Workspace.CheckVersion(ifMatch); err != nil {
			return Effects{}, err
		}
		changed, err := a.Workspace.RequestArchive(s.now())
		if err != nil || !changed {
			return Effects{}, err
		}
		return Effects{Audit: audit1(actor, "workspace.archive_requested", "")}, nil
	})
	if err != nil {
		return nil, err
	}
	if agg.Workspace.State == StateArchived {
		return agg.Workspace, nil
	}

	deadline := time.Now().Add(s.o.ArchiveWait)
	for {
		cur, err := s.d.Store.Get(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if cur.Pending == nil {
			return s.finishArchive(ctx, actor, scope, id)
		}
		if err := s.advance(ctx, scope, id, nil); err != nil && errors.Is(err, ErrFenced) {
			return cur, errors.Join(ErrArchiveUncertain, err)
		}
		cur, err = s.d.Store.Get(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if cur.Pending == nil {
			continue
		}
		if !time.Now().Before(deadline) {
			return cur, ErrArchiveUncertain
		}
		select {
		case <-ctx.Done():
			return cur, errors.Join(ErrArchiveUncertain, ctx.Err())
		case <-time.After(s.o.ArchivePoll):
		}
	}
}

// finishArchive completes an archive whose publication has settled. An
// internally hosted workspace additionally waits until no push of any created
// task branch is unsettled (authorized, committing or unknown): the writer's
// last push may still land, and archiving would report completion over work the
// workspace has not accepted. The check reads gitpush outside SQL, after the
// workspace version, and the completion requires the version unchanged.
func (s *Service) finishArchive(ctx context.Context, actor Actor, scope domain.Scope, id ID) (*Workspace, error) {
	var agg *Aggregate
	err := retry(admissionRetries, func() error {
		var observed domain.Version
		view, err := s.d.Store.View(ctx, scope, id, Load{Associations: true})
		if err != nil {
			return err
		}
		if w := view.Workspace; w.Internal() && w.State != StateArchived && w.ArchiveRequestedAt != nil {
			observed = w.Version
			for _, a := range view.Associations {
				if a.State != AssociationCreated {
					continue
				}
				if err := s.requireSettled(ctx, scope, a); err != nil {
					return err
				}
			}
		}
		agg, err = s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
			if observed != 0 && a.Workspace.Version != observed {
				return Effects{}, ErrStale
			}
			changed, err := a.Workspace.CompleteArchive(s.now())
			if err != nil || !changed {
				return Effects{}, err
			}
			return Effects{
				Audit:  audit1(actor, "workspace.archived", ""),
				Events: []Event{stateEvent(actor, string(StateAvailable), string(StateArchived), "archived")},
			}, nil
		})
		return err
	})
	if errors.Is(err, ErrPublicationPending) || errors.Is(err, ErrPushPending) {
		cur, gerr := s.d.Store.Get(ctx, scope, id)
		if gerr != nil {
			return nil, gerr
		}
		return cur, ErrArchiveUncertain
	}
	if err != nil {
		return nil, err
	}
	return agg.Workspace, nil
}

// Restore explicitly reopens the same workspace and branch after settlement; it
// never provisions a replacement. ifMatch is compared atomically.
func (s *Service) Restore(ctx context.Context, actor Actor, scope domain.Scope, id ID, ifMatch domain.Version) (*Workspace, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionRestore, scope, Resource{"workspace", string(id)}); err != nil {
		return nil, err
	}
	agg, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
		if err := a.Workspace.CheckVersion(ifMatch); err != nil {
			return Effects{}, err
		}
		was := a.Workspace.State
		if err := a.Workspace.Restore(s.now()); err != nil {
			return Effects{}, err
		}
		fx := Effects{Audit: audit1(actor, "workspace.restored", "")}
		if was == StateArchived {
			fx.Events = []Event{stateEvent(actor, string(StateArchived), string(StateAvailable), "restored")}
		}
		return fx, nil
	})
	if err != nil {
		return nil, err
	}
	return agg.Workspace, nil
}
