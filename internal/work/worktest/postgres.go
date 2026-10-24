package worktest

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// PGEnv is the PostgreSQL Env: the existing Stack with a store whose event
// appender can be made to fail after the real append, like the memory side.
type PGEnv struct {
	*Stack
	Svc   *work.Service
	Store *workpg.Store

	mu    sync.Mutex
	fault error
}

var _ Env = (*PGEnv)(nil)

type pgFaultAppender struct {
	inner workpg.EventAppender
	env   *PGEnv
}

func (f pgFaultAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	res, err := f.inner.Append(ctx, tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// OpenPostgres is the Open of the PostgreSQL adapter: an isolated migrated
// database in the `test` environment. It skips without PostgreSQL, and always
// in the unit layer.
func OpenPostgres(t testing.TB) Env {
	t.Helper()
	s := NewStack(t)
	e := &PGEnv{Stack: s}
	var err error
	e.Store, err = workpg.NewStore(s.Runtime, pgFaultAppender{inner: s.Events, env: e}, func(tx pgx.Tx) work.TxGuards {
		g := txGuards{tx}
		return work.TxGuards{Permissions: g, Writers: g, Dependencies: work.NoDependencyGraph{}, Subjects: g}
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Svc, err = work.NewService(work.Config{Repository: e.Store, Authority: authority{s}}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *PGEnv) Service() *work.Service                 { return e.Svc }
func (e *PGEnv) Repository() work.Repository            { return e.Store }
func (e *PGEnv) Cancellations() work.CancellationSource { return e.Store }

func (e *PGEnv) SeedProject(t testing.TB, scope domain.Scope, wip int) {
	t.Helper()
	e.Stack.SeedProject(string(scope.TenantID), string(scope.ProjectID), wip)
}

func (e *PGEnv) Principal(t testing.TB, tenant domain.TenantID, id string, typ work.ActorType, perms ...work.Permission) {
	t.Helper()
	e.Stack.Principal(string(tenant), id, typ, perms...)
}

func (e *PGEnv) Revoke(t testing.TB, tenant domain.TenantID, id string, perm work.Permission) {
	t.Helper()
	e.Stack.Revoke(string(tenant), id, perm)
}

func (e *PGEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}

func (e *PGEnv) EventIDs(t testing.TB, tenant domain.TenantID) []string {
	t.Helper()
	var ids []string
	// The outbox policies are tenant-only: read in the tenant's context.
	err := storage.WithTenant(context.Background(), e.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT event_id FROM outbox WHERE tenant_id = $1 ORDER BY event_id`, string(tenant))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func (e *PGEnv) Admit(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (workpg.Admission, error) {
	var a workpg.Admission
	err := storage.WithTenant(ctx, e.Runtime, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		a, err = workpg.LockActionable(ctx, tx, scope, id)
		return err
	})
	return a, err
}
