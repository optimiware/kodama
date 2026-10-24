package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/observation"
	"github.com/optimiware/kodama/internal/storage"
)

var repeatableReadOnly = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// verify checks that the project, and for a run stream the run, exist inside the
// scope's tenant and project, in tx.
func (s *Store) verify(ctx context.Context, tx pgx.Tx, scope observation.Scope) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2`,
		string(scope.Tenant), string(scope.Project)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the project was not found", observation.ErrNotFound)
	}
	if err != nil {
		return err
	}
	if scope.Run == "" {
		return nil
	}
	ok, err := s.bindings.Runs.RunExists(ctx, tx, domain.Scope{TenantID: scope.Tenant, ProjectID: scope.Project}, scope.Run)
	if err != nil {
		return observation.Unavailablef(err, "looking up the run")
	}
	if !ok {
		return fmt.Errorf("%w: the run was not found", observation.ErrNotFound)
	}
	return nil
}

// Verify implements observation.State.
func (s *Store) Verify(ctx context.Context, scope observation.Scope) error {
	return classify(storage.WithTenantOptions(ctx, s.db, scope.Tenant, readOnly, func(ctx context.Context, tx pgx.Tx) error {
		return s.verify(ctx, tx, scope)
	}))
}

// Snapshot implements observation.State. The project check, every binding read
// and the event watermark run in one read-only REPEATABLE READ transaction: the
// first statement fixes the snapshot, so the returned position reflects exactly
// the committed events whose state changes the resources already contain (state
// and event commit together in the owning module's transaction). The result is
// complete or an error: more than maxResources resources fail with
// ErrSnapshotTooLarge and a binding failure is ErrUnavailable. No cursor can be
// derived from a failed snapshot because none is returned.
func (s *Store) Snapshot(ctx context.Context, scope observation.Scope, maxResources int) (observation.Snapshot, error) {
	if maxResources < 1 {
		return observation.Snapshot{}, fmt.Errorf("%w: the snapshot bound must be positive", observation.ErrInvalidConfiguration)
	}
	var snap observation.Snapshot
	err := storage.WithTenantOptions(ctx, s.db, scope.Tenant, repeatableReadOnly, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.verify(ctx, tx, scope); err != nil {
			return err
		}
		wm, err := s.events.Watermark(ctx, tx, eventScope(scope))
		if err != nil {
			return err
		}
		families := scope.Families()
		var resources []observation.Resource
		read := func(b ResourceBinding, fams []observation.Family) error {
			if len(fams) == 0 {
				return nil
			}
			remaining := maxResources - len(resources)
			got, err := b.ReadResources(ctx, tx, ResourceQuery{Scope: scope, Families: fams, Limit: remaining + 1})
			if err != nil {
				return observation.Unavailablef(err, "reading resource state")
			}
			for _, r := range got {
				if !contains(fams, r.Type) {
					return observation.Unavailablef(nil, "a resource binding returned a %q resource that was not requested", r.Type)
				}
			}
			if len(got) > remaining {
				return fmt.Errorf("%w: more than %d resources", observation.ErrSnapshotTooLarge, maxResources)
			}
			resources = append(resources, got...)
			return nil
		}
		if err := read(s.bindings.Work, only(families, observation.FamilyWork)); err != nil {
			return err
		}
		if err := read(s.bindings.Runs, only(families, observation.FamilyRun, observation.FamilyStepAttempt)); err != nil {
			return err
		}
		snap = observation.Snapshot{Position: wm.HighWater, Resources: resources}
		return nil
	})
	if err != nil {
		return observation.Snapshot{}, classify(err)
	}
	return snap, nil
}

func contains(fams []observation.Family, f observation.Family) bool {
	for _, x := range fams {
		if x == f {
			return true
		}
	}
	return false
}

func only(fams []observation.Family, want ...observation.Family) []observation.Family {
	var out []observation.Family
	for _, f := range fams {
		if contains(want, f) {
			out = append(out, f)
		}
	}
	return out
}
