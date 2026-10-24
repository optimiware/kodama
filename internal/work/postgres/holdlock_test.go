package postgres_test

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// holder is an open tenant transaction that has locked a work item (and, with
// the project, the project row) and keeps them until release.
type holder struct {
	release chan func(ctx context.Context, tx pgx.Tx) error
	done    chan error
}

// hold starts a tenant transaction that runs lock, signals readiness and waits.
func (e *env) hold(tenant string, lock func(ctx context.Context, tx pgx.Tx) error) *holder {
	e.t.Helper()
	h := &holder{release: make(chan func(ctx context.Context, tx pgx.Tx) error, 1), done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		h.done <- storage.WithTenant(context.Background(), e.rt, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
			if err := lock(ctx, tx); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			if fn := <-h.release; fn != nil {
				return fn(ctx, tx)
			}
			return nil
		})
	}()
	if err := <-ready; err != nil {
		e.t.Fatalf("hold: %v", err)
	}
	return h
}

// finish lets the holder run fn and commit, and returns its result.
func (h *holder) finish(fn func(ctx context.Context, tx pgx.Tx) error) error {
	h.release <- fn
	return <-h.done
}

func lockItemRow(scope domain.Scope, id domain.WorkItemID) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(id)).Scan(&one)
	}
}

func lockActionable(scope domain.Scope, id domain.WorkItemID) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := workpg.LockActionable(ctx, tx, scope, id)
		return err
	}
}

// waitBlocked waits until n backends of this test database are waiting for a lock.
func (e *env) waitBlocked(n int) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, pgtest.AdminURL())
	if err != nil {
		e.t.Fatalf("admin connection: %v", err)
	}
	defer admin.Close(ctx)
	for {
		var got int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, e.db.Name).Scan(&got); err != nil {
			e.t.Fatal(err)
		}
		if got >= n {
			return
		}
		select {
		case <-ctx.Done():
			e.t.Fatalf("timed out waiting for %d blocked backend(s), saw %d", n, got)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// async runs fn in a goroutine and returns its result channel.
func async[T any](fn func() (T, error)) <-chan result[T] {
	ch := make(chan result[T], 1)
	go func() {
		v, err := fn()
		ch <- result[T]{v, err}
	}()
	return ch
}

type result[T any] struct {
	v   T
	err error
}

func (e *env) moveAsync(it work.Item, principal string, to work.State, mutate ...func(*work.TransitionRequest)) <-chan result[work.Item] {
	return async(func() (work.Item, error) { return e.move(it, principal, to, mutate...) })
}
