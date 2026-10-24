package workspace

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// FencePublishers is the restore-recovery hook: it revokes every in-flight
// publisher of a project. A claim made under an earlier epoch can no longer
// finalize a publication, and no writer or proposal is admitted until
// publishing is resumed. It returns the number of workspaces fenced.
func (s *Service) FencePublishers(ctx context.Context, actor Actor, scope domain.Scope) (int, error) {
	if err := s.authorize(ctx, actor, ActionFence, scope, Resource{Kind: "project", ID: string(scope.ProjectID)}); err != nil {
		return 0, err
	}
	return s.forEach(ctx, scope, func(id ID) error {
		_, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
			a.Workspace.FencePublishing(s.now())
			return Effects{Audit: audit1(actor, "workspace.publisher_fenced", "")}, nil
		})
		return err
	})
}

// ResumePublishing lifts the fence once the publisher credentials were rotated.
// It refuses without verifiable rotation evidence. A publication that was in
// flight when the fence was raised stays pending and is settled from remote
// evidence by the reconciler, never abandoned.
func (s *Service) ResumePublishing(ctx context.Context, actor Actor, scope domain.Scope, e RotationEvidence) (int, error) {
	if err := s.authorize(ctx, actor, ActionFence, scope, Resource{Kind: "project", ID: string(scope.ProjectID)}); err != nil {
		return 0, err
	}
	if s.d.Rotation == nil {
		return 0, fmt.Errorf("%w: credential rotation verification", ErrNotConfigured)
	}
	if e.RotationID == "" {
		return 0, fmt.Errorf("%w: rotation evidence is required", ErrInvalidValue)
	}
	if err := s.d.Rotation.VerifyRotated(ctx, scope, e); err != nil {
		return 0, fmt.Errorf("%w: publisher credentials were not rotated: %w", ErrDenied, err)
	}
	return s.forEach(ctx, scope, func(id ID) error {
		_, err := s.d.Store.Update(ctx, scope, id, Load{}, func(a *Aggregate) (Effects, error) {
			if !a.Workspace.ResumePublishing(s.now()) {
				return Effects{}, nil
			}
			return Effects{Audit: []Audit{audit(actor, "workspace.publishing_resumed", "", map[string]any{"rotationId": e.RotationID})}}, nil
		})
		return err
	})
}

func (s *Service) forEach(ctx context.Context, scope domain.Scope, fn func(ID) error) (int, error) {
	var after ID
	n := 0
	for {
		ids, err := s.d.Store.ListProject(ctx, scope, after, s.o.ReconcileBatch)
		if err != nil {
			return n, err
		}
		for _, id := range ids {
			if err := fn(id); err != nil {
				return n, err
			}
			n++
		}
		if len(ids) < s.o.ReconcileBatch {
			return n, nil
		}
		after = ids[len(ids)-1]
	}
}
