// Package memory implements the task transaction port over memdb.
package memory

import (
	"context"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/tasks"
)

type project struct {
	ID       domain.ProjectID
	Workflow tasks.Workflow
}

var projects = memdb.NewClonedTable("task_workflows", func(p project) project {
	p.Workflow = p.Workflow.Clone()
	return p
}, func(p project) string { return string(p.ID) })
var taskRows = memdb.NewClonedTable("tasks", tasks.Task.Clone, func(t tasks.Task) string {
	return string(t.Scope.ProjectID) + "/" + string(t.ID)
})

type eventRow struct {
	Project domain.ProjectID
	Event   tasks.Event
}

var eventRows = memdb.NewClonedTable("audit_events", func(r eventRow) eventRow {
	r.Event.Payload = slices.Clone(r.Event.Payload)
	return r
}, func(r eventRow) string { return r.Event.ID })

type Store struct {
	DB *memdb.Store
	// CheckReferences joins project repository/user lookups to this same Tx.
	// Until Phase 3 wires those peers, nonempty references fail closed.
	CheckReferences func(memdb.Tx, tasks.Task) error
}

var _ tasks.Store = Store{}

// SeedProject supplies the project-owned workflow in a memory composition.
// Reconfiguration of existing projects belongs to the Phase 3 project service.
func (s Store) SeedProject(ctx context.Context, scope domain.Scope, workflow tasks.Workflow) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := workflow.Validate(); err != nil {
		return err
	}
	return s.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, found := projects.Get(tx, string(scope.ProjectID)); found {
			return domain.ErrConflict
		}
		return projects.Put(tx, project{scope.ProjectID, workflow})
	})
}

func (s Store) Update(ctx context.Context, scope domain.Scope, fn func(tasks.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return s.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		p, found := projects.Get(tx, string(scope.ProjectID))
		if !found {
			return tasks.ErrNotFound
		}
		return fn(transaction{tx, scope, p.Workflow, s.CheckReferences})
	})
}

type transaction struct {
	tx              memdb.Tx
	scope           domain.Scope
	workflow        tasks.Workflow
	checkReferences func(memdb.Tx, tasks.Task) error
}

func (t transaction) Workflow() tasks.Workflow { return t.workflow.Clone() }
func (t transaction) Get(id domain.TaskID) (tasks.Task, error) {
	row, found := taskRows.Get(t.tx, string(t.scope.ProjectID)+"/"+string(id))
	if !found {
		return tasks.Task{}, tasks.ErrNotFound
	}
	return row, nil
}
func (t transaction) GetByBlocker(request domain.TaskID) (tasks.Task, error) {
	var found tasks.Task
	var ok bool
	taskRows.Scan(t.tx, func(row tasks.Task) bool {
		if row.Scope == t.scope && row.BlockedByID == request {
			found, ok = row, true
			return false
		}
		return true
	})
	if !ok {
		return tasks.Task{}, tasks.ErrNotFound
	}
	return found, nil
}

func (t transaction) Save(next tasks.Task, expected domain.Version) error {
	var current *tasks.Task
	if row, err := t.Get(next.ID); err == nil {
		current = &row
	}
	if err := tasks.CheckSave(next, current, expected, t.scope, t.workflow); err != nil {
		return err
	}
	for _, ref := range []domain.TaskID{next.ParentID, next.BlockedByID} {
		if ref != "" {
			if _, err := t.Get(ref); err != nil {
				return err
			}
		}
	}
	if next.RepositoryID != "" || next.AssignedUserID != "" {
		if t.checkReferences == nil {
			return tasks.ErrNotFound
		}
		if err := t.checkReferences(t.tx, next); err != nil {
			return err
		}
	}
	return taskRows.Put(t.tx, next)
}
func (t transaction) Append(event tasks.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if _, found := eventRows.Get(t.tx, event.ID); found {
		return domain.ErrConflict
	}
	return eventRows.Put(t.tx, eventRow{t.scope.ProjectID, event})
}

// Events is used by the contract fixture to inspect committed audit evidence.
func (s Store) Events(ctx context.Context, scope domain.Scope) ([]tasks.Event, error) {
	var out []tasks.Event
	err := s.DB.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		eventRows.Scan(tx, func(row eventRow) bool {
			if row.Project == scope.ProjectID {
				out = append(out, row.Event)
			}
			return true
		})
		return nil
	})
	return out, err
}
