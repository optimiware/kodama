package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/outbox"
	"github.com/optimiware/kodama/internal/storage"
)

// Postgres is the PostgreSQL Store. Every operation is one transaction under
// storage.WithTenant, so row-level security binds it to the tenant, and every
// statement also names the project. A state change and the outbox events that
// describe it are written by the same transaction.
//
// Where the data lives: the task row holds its content (version), its selected
// pipeline_id and the projection (current_run_id, status, current_step_key,
// waiting_for, result, progress_version, progress_run_version). A run belongs to
// its task through runs.task_id; the composite key of tasks.current_run_id keeps
// a current run to its own task and project. The legacy workflow columns of the
// table (state, parked_at, resume_state, blocked_by_id, resolution, resolved_at)
// remain until the schema's contract stage: this adapter never reads them, and
// writes the one NOT NULL among them (state) a constant.
type Postgres struct{ db storage.TxBeginner }

var _ Store = (*Postgres)(nil)

// NewPostgres returns the adapter over a pool of the runtime role.
func NewPostgres(db storage.TxBeginner) *Postgres { return &Postgres{db: db} }

// legacyState fills tasks.state, a NOT NULL column that the contract stage drops.
// Nothing reads it.
const legacyState = "open"

const taskColumns = `id, parent_id, title, body, assigned_role, repo_id, branch, context::jsonb, pipeline_id, version,
  status, current_run_id, current_step_key, waiting_for, result::jsonb, progress_version, progress_run_version,
  created_at, updated_at`

func scanTask(row pgx.Row, scope domain.Scope) (Task, error) {
	var (
		id, title, body                      string
		parent, role, repo, branch, pipeline *string
		ctxJSON                              []byte
		version                              int64
		status, run, step, waitingFor        *string
		result                               []byte
		progressVersion, progressRunVersion  *int64
		created, updated                     time.Time
	)
	if err := row.Scan(&id, &parent, &title, &body, &role, &repo, &branch, &ctxJSON, &pipeline, &version,
		&status, &run, &step, &waitingFor, &result, &progressVersion, &progressRunVersion, &created, &updated); err != nil {
		return Task{}, err
	}
	t := Task{Scope: scope, ID: domain.TaskID(id), Version: domain.Version(version), Created: created.UTC(), Updated: updated.UTC()}
	t.ParentID, t.AssignedRole = domain.TaskID(deref(parent)), deref(role)
	t.RepositoryID, t.Branch = domain.RepositoryID(deref(repo)), deref(branch)
	t.Title, t.Body, t.Context = title, body, ctxJSON
	t.PipelineID = domain.PipelineID(deref(pipeline))
	t.Progress = Progress{RunID: domain.RunID(deref(run)), Status: Status(deref(status)), Step: domain.StepID(deref(step)), WaitingFor: deref(waitingFor)}
	if t.Progress.Status == "" {
		t.Progress.Status = StatusPending
	}
	if len(result) > 0 && string(result) != "null" {
		var r Result
		if err := json.Unmarshal(result, &r); err != nil {
			return Task{}, fmt.Errorf("tasks/postgres: task %s result: %w", id, err)
		}
		t.Progress.Result = r
	}
	if progressVersion != nil {
		t.Progress.Version = domain.Version(*progressVersion)
	}
	if progressRunVersion != nil {
		t.Progress.RunVersion = domain.Version(*progressRunVersion)
	}
	return t, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// resultArg is the result column's value: NULL for none, otherwise a JSON object.
func resultArg(r Result) any {
	if r == (Result{}) {
		return nil
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func (p *Postgres) tx(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(ctx, p.db, tenant, fn)
}

// translate maps a database refusal to the store's vocabulary.
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return domain.ErrConflict
		case "23503":
			switch {
			case pg.ConstraintName == "tasks_tenant_id_project_id_fkey":
				return ErrNotFound // the project does not exist in this tenant
			case pg.ConstraintName == "tasks_current_run_fk":
				return fmt.Errorf("%w: the run was not admitted for this task in this project (runs.task_id)", domain.ErrInvalidValue)
			}
			return fmt.Errorf("%w: a referenced parent task, repository or pipeline is not in this project", domain.ErrInvalidValue)
		case "23514":
			return fmt.Errorf("%w: %s", domain.ErrInvalidValue, pg.ConstraintName)
		}
	}
	return err
}

func insertEvent(ctx context.Context, tx pgx.Tx, scope domain.Scope, task domain.TaskID, ev Event) error {
	id, err := outbox.EventID(scope.TenantID, ev.Type, ev.DedupKey)
	if err != nil {
		return err
	}
	actorType, actor := "system", "task-tracker"
	if ev.Actor != "" {
		actorType, actor = "user", ev.Actor
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit_events (tenant_id, id, project_id, actor_type, actor_id, event_type, resource_type,
  resource_id, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'task', $7, $8::jsonb, clock_timestamp())
ON CONFLICT (tenant_id, id) DO NOTHING`, scope.TenantID, id, scope.ProjectID, actorType, actor, ev.Type, task, string(ev.Data))
	return err
}

func (p *Postgres) Create(ctx context.Context, t Task, actor string) error {
	if err := t.Scope.Validate(); err != nil {
		return err
	}
	return p.tx(ctx, t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tasks (tenant_id, project_id, id, parent_id, title, body, state, assigned_role, repo_id,
  branch, context, pipeline_id, status, version, created_at, updated_at)
VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), $11::jsonb, $12, 'pending', $13, $14, $15)`,
			t.Scope.TenantID, t.Scope.ProjectID, t.ID, string(t.ParentID), t.Title, t.Body, legacyState, t.AssignedRole,
			string(t.RepositoryID), t.Branch, string(t.Context), t.PipelineID, int64(t.Version), t.Created, t.Updated)
		if err != nil {
			return translate(err)
		}
		return insertEvent(ctx, tx, t.Scope, t.ID, createdEvent(t, actor))
	})
}

func getTask(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.TaskID, lock bool) (Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks WHERE tenant_id = $1 AND project_id = $2 AND id = $3`
	if lock {
		q += ` FOR UPDATE`
	}
	t, err := scanTask(tx.QueryRow(ctx, q, scope.TenantID, scope.ProjectID, id), scope)
	if err != nil {
		return Task{}, translate(err)
	}
	return t, nil
}

func (p *Postgres) Get(ctx context.Context, scope domain.Scope, id domain.TaskID) (Task, error) {
	var out Task
	err := p.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = getTask(ctx, tx, scope, id, false)
		return err
	})
	return out, err
}

func (p *Postgres) List(ctx context.Context, q ListQuery) ([]Task, error) {
	var out []Task
	err := p.tx(ctx, q.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks
WHERE tenant_id = $1 AND project_id = $2
  AND ($3 = '' OR COALESCE(status, 'pending') = $3)
  AND ($4 = '' OR parent_id = $4)
  AND ($5 = '' OR id COLLATE "C" > $5 COLLATE "C")
ORDER BY id COLLATE "C" LIMIT $6`, q.Scope.TenantID, q.Scope.ProjectID, string(q.Status), string(q.ParentID), string(q.After), q.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows, q.Scope)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

func (p *Postgres) UpdateContent(ctx context.Context, scope domain.Scope, id domain.TaskID, expected domain.Version, c Content, now time.Time, actor string) (Task, error) {
	var out Task
	err := p.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		t, err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET title = $4, body = $5, assigned_role = NULLIF($6, ''), repo_id = NULLIF($7, ''),
  branch = NULLIF($8, ''), context = $9::jsonb, version = version + 1, updated_at = $10
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $11
RETURNING `+taskColumns, scope.TenantID, scope.ProjectID, id, c.Title, c.Body, c.AssignedRole, string(c.RepositoryID), c.Branch,
			string(c.Context), now, int64(expected)), scope)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, gerr := getTask(ctx, tx, scope, id, false); gerr != nil {
				return gerr
			}
			return ErrContentConflict
		}
		if err != nil {
			return translate(err)
		}
		out = t
		return insertEvent(ctx, tx, scope, id, contentEvent(t, actor))
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

func (p *Postgres) Select(ctx context.Context, sel RunSelection) (Task, error) {
	var out Task
	err := p.tx(ctx, sel.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		pr := sel.Progress
		t, err := scanTask(tx.QueryRow(ctx, `UPDATE tasks SET current_run_id = $4, status = $5, current_step_key = NULLIF($6, ''),
  waiting_for = NULLIF($7, ''), result = $8::jsonb, progress_version = COALESCE(progress_version, 0) + 1, progress_run_version = $9
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND current_run_id IS NOT DISTINCT FROM NULLIF($10, '')
RETURNING `+taskColumns, sel.Scope.TenantID, sel.Scope.ProjectID, sel.TaskID, sel.Run, string(pr.Status), string(pr.Step),
			pr.WaitingFor, resultArg(pr.Result), int64(pr.RunVersion), string(sel.Expected)), sel.Scope)
		if errors.Is(err, pgx.ErrNoRows) {
			cur, gerr := getTask(ctx, tx, sel.Scope, sel.TaskID, false)
			if gerr != nil {
				return gerr
			}
			if cur.Progress.RunID == sel.Run {
				out = cur // already current: a replay
				return nil
			}
			return ErrStaleRun
		}
		if err != nil {
			return translate(err)
		}
		out = t
		return insertEvent(ctx, tx, sel.Scope, sel.TaskID, progressEvent(t))
	})
	return out, err
}

func (p *Postgres) Advance(ctx context.Context, scope domain.Scope, id domain.TaskID, obs Progress, force bool) (Task, Verdict, error) {
	var (
		out Task
		v   Verdict
	)
	err := p.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The row lock serializes projections of one task: the loser re-reads the
		// version the winner committed and is judged against it.
		cur, err := getTask(ctx, tx, scope, id, true)
		if err != nil {
			return err
		}
		next, verdict := advance(cur.Progress, obs, force)
		v, out = verdict, cur
		if verdict != VerdictApplied {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE tasks SET status = $4, current_step_key = NULLIF($5, ''), waiting_for = NULLIF($6, ''),
  result = $7::jsonb, progress_version = $8, progress_run_version = $9
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND current_run_id = $10`, scope.TenantID, scope.ProjectID, id,
			string(next.Status), string(next.Step), next.WaitingFor, resultArg(next.Result), int64(next.Version),
			int64(next.RunVersion), string(cur.Progress.RunID))
		if err != nil {
			return translate(err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("tasks/postgres: task %s changed under its lock", id)
		}
		out.Progress = next
		if next.Version != cur.Progress.Version {
			return insertEvent(ctx, tx, scope, id, progressEvent(out))
		}
		return nil
	})
	if err != nil {
		return Task{}, v, err
	}
	return out, v, nil
}

func (p *Postgres) TaskOfRun(ctx context.Context, scope domain.Scope, run domain.RunID) (domain.TaskID, error) {
	var id string
	err := p.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT task_id FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND task_id IS NOT NULL`,
			scope.TenantID, scope.ProjectID, run).Scan(&id)
	})
	if err != nil {
		return "", translate(err)
	}
	return domain.TaskID(id), nil
}

func (p *Postgres) Waiting(ctx context.Context, scope domain.Scope, roles []string, after domain.TaskID, limit int) ([]Task, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	var out []Task
	err := p.tx(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks
WHERE tenant_id = $1 AND project_id = $2 AND status = 'waiting' AND waiting_for = ANY($3::text[])
  AND ($4 = '' OR id COLLATE "C" > $4 COLLATE "C")
ORDER BY id COLLATE "C" LIMIT $5`, scope.TenantID, scope.ProjectID, roles, string(after), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTask(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}
