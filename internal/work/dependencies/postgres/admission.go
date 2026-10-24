package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// DependencyGuard implements work.DependencyGuard over one transaction. The
// work store's guard binder constructs it for each mutating transaction, so the
// answer is read after the work service took the project and item locks, and
// reflects every link change committed before that point.
type DependencyGuard struct{ tx pgx.Tx }

var _ work.DependencyGuard = DependencyGuard{}

// NewDependencyGuard binds the guard to tx, the caller's open tenant
// transaction. Wire it in the work store's binder, for example:
//
//	func(tx pgx.Tx) work.TxGuards {
//		return work.TxGuards{..., Dependencies: dependencypg.NewDependencyGuard(tx), ...}
//	}
//
// It never opens a second transaction, never caches a graph and never takes a
// lock: the project lock (and then the item lock) belong to the work transition
// that calls it, and taking the project lock after the item lock here would
// invert the lock order.
func NewDependencyGuard(tx pgx.Tx) work.DependencyGuard { return DependencyGuard{tx: tx} }

// DependenciesSatisfied returns nil when every direct prerequisite of the work
// item is done (or there are none), and a work.ErrPrerequisite error
// otherwise. The prerequisite states are read with an outer join keyed on the
// links, so a link whose work item row cannot be read is reported as missing
// and fails admission instead of being omitted.
func (g DependencyGuard) DependenciesSatisfied(ctx context.Context, scope domain.Scope, id domain.WorkItemID) error {
	if g.tx == nil {
		return work.Unavailablef(nil, "the dependency guard is not bound to a transaction")
	}
	if err := scope.Validate(); err != nil {
		return work.Invalidf("%v", err)
	}
	if err := id.Validate(); err != nil {
		return work.Invalidf("%v", err)
	}
	rows, err := g.tx.Query(ctx, `SELECT l.prerequisite_id, w.state
		FROM work_item_dependencies l
		LEFT JOIN work_items w
		  ON w.tenant_id = l.tenant_id AND w.project_id = l.project_id AND w.id = l.prerequisite_id
		WHERE l.tenant_id = $1 AND l.project_id = $2 AND l.dependent_id = $3
		ORDER BY l.prerequisite_id`,
		string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return err
	}
	defer rows.Close()
	var statuses []work.PrerequisiteStatus
	for rows.Next() {
		var prereq string
		var state *string
		if err := rows.Scan(&prereq, &state); err != nil {
			return err
		}
		st := work.PrerequisiteStatus{ID: domain.WorkItemID(prereq)}
		if state != nil {
			st.Found, st.State = true, work.State(*state)
		}
		statuses = append(statuses, st)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return work.DependenciesComplete(statuses)
}
