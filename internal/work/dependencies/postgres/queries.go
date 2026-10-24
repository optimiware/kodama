package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
)

func notFound() error { return fmt.Errorf("%w: the work item was not found", work.ErrNotFound) }

// List implements dependencies.Repository. One statement reads the dependent
// and its page of links, so the returned version and links are one snapshot and
// the version is a sound If-Match for a following mutation. Ordering is the
// prerequisite identifier, the last component of the primary key, so a
// position identifies exactly one row. A dependent outside the scope returns
// work.ErrNotFound, like a missing one.
func (s *Store) List(ctx context.Context, scope domain.Scope, q dependencies.ListQuery) (dependencies.Page, error) {
	var out dependencies.Page
	err := s.inTenant(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT w.version, l.prerequisite_id, l.created_at, l.created_by_type, l.created_by_id
			FROM work_items w
			LEFT JOIN work_item_dependencies l
			  ON l.tenant_id = w.tenant_id AND l.project_id = w.project_id AND l.dependent_id = w.id AND l.prerequisite_id > $4
			WHERE w.tenant_id = $1 AND w.project_id = $2 AND w.id = $3
			ORDER BY l.prerequisite_id LIMIT $5`,
			string(scope.TenantID), string(scope.ProjectID), string(q.Dependent), string(q.After), q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			found = true
			var (
				version               int64
				prereq, actorType, by *string
				created               *time.Time
			)
			if err := rows.Scan(&version, &prereq, &created, &actorType, &by); err != nil {
				return err
			}
			out.OwnerVersion = domain.Version(version)
			if prereq == nil {
				continue // the dependent exists and has no (further) links
			}
			out.Items = append(out.Items, dependencies.Prerequisite{
				Link:      dependencies.Link{Scope: scope, DependentID: q.Dependent, PrerequisiteID: domain.WorkItemID(*prereq)},
				CreatedAt: created.UTC(),
				CreatedBy: work.Actor{Type: work.ActorType(*actorType), ID: *by},
			})
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if !found {
			return notFound()
		}
		return nil
	})
	return out, err
}

// lockProject takes the project row lock. FOR NO KEY UPDATE serializes link
// changes with transitions and parent changes of one project without blocking
// the foreign-key checks of concurrent inserts.
func lockProject(ctx context.Context, tx pgx.Tx, scope domain.Scope) error {
	var limit int
	err := tx.QueryRow(ctx, `SELECT active_wip_limit FROM projects WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE`,
		string(scope.TenantID), string(scope.ProjectID)).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the project was not found", work.ErrNotFound)
	}
	return err
}

// lockOwner locks the dependent work item FOR UPDATE and reads what the graph
// needs of it.
func lockOwner(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.WorkItemID) (dependencies.Owner, error) {
	var state string
	var version, revision int64
	err := tx.QueryRow(ctx, `SELECT state, version, content_revision FROM work_items
		WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&state, &version, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return dependencies.Owner{}, notFound()
	}
	if err != nil {
		return dependencies.Owner{}, err
	}
	return dependencies.Owner{Scope: scope, ID: id, State: work.State(state),
		Version: domain.Version(version), ContentRevision: domain.ContentRevision(revision)}, nil
}

// txEnv is dependencies.Tx bound to one mutating transaction.
type txEnv struct {
	tx    pgx.Tx
	scope domain.Scope
}

var _ dependencies.Tx = (*txEnv)(nil)

func (e *txEnv) check(scope domain.Scope) error {
	if scope != e.scope {
		return fmt.Errorf("%w: the transaction belongs to another scope", work.ErrDenied)
	}
	return nil
}

func (e *txEnv) PrerequisiteExists(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	if err := e.check(scope); err != nil {
		return err
	}
	var one int
	err := e.tx.QueryRow(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the dependency was not found", work.ErrNotFound)
	}
	return err
}

func (e *txEnv) LinkExists(ctx context.Context, l dependencies.Link) (bool, error) {
	if err := e.check(l.Scope); err != nil {
		return false, err
	}
	var exists bool
	err := e.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_item_dependencies
		WHERE tenant_id = $1 AND project_id = $2 AND dependent_id = $3 AND prerequisite_id = $4)`,
		string(l.Scope.TenantID), string(l.Scope.ProjectID), string(l.DependentID), string(l.PrerequisiteID)).Scan(&exists)
	return exists, err
}

func (e *txEnv) PrerequisiteCount(ctx context.Context, scope domain.Scope, dependent domain.WorkItemID) (int, error) {
	if err := e.check(scope); err != nil {
		return 0, err
	}
	var n int
	err := e.tx.QueryRow(ctx, `SELECT count(*) FROM work_item_dependencies WHERE tenant_id = $1 AND project_id = $2 AND dependent_id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(dependent)).Scan(&n)
	return n, err
}

// DependsOn walks prerequisite links from `from`. UNION (not UNION ALL)
// removes duplicate rows at every step, so a diamond or any cyclic fragment
// terminates and the walk visits each work item once. The project lock held by
// every graph writer makes the walk see a consistent committed graph.
func (e *txEnv) DependsOn(ctx context.Context, scope domain.Scope, from, target domain.WorkItemID) (bool, error) {
	if err := e.check(scope); err != nil {
		return false, err
	}
	var reaches bool
	err := e.tx.QueryRow(ctx, `WITH RECURSIVE reach(id) AS (
			SELECT prerequisite_id FROM work_item_dependencies
			 WHERE tenant_id = $1 AND project_id = $2 AND dependent_id = $3
			UNION
			SELECT l.prerequisite_id FROM work_item_dependencies l JOIN reach r
			  ON l.tenant_id = $1 AND l.project_id = $2 AND l.dependent_id = r.id
		) SELECT EXISTS (SELECT 1 FROM reach WHERE id = $4)`,
		string(scope.TenantID), string(scope.ProjectID), string(from), string(target)).Scan(&reaches)
	return reaches, err
}
