package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Resource prefix of this task's test fixtures: databases are provisioned by the
// shared helper with random names, and fixture tables carry the prefix.
const prefix = depstest.Prefix

const (
	tenantA, projectA = depstest.TenantA, depstest.ProjectA
	tenantB           = depstest.TenantB
)

var (
	scopeA  = depstest.ScopeA
	scopeA2 = depstest.ScopeA2
	scopeB  = depstest.ScopeB
)

func is(err, target error) bool { return errors.Is(err, target) }

// add and remove call the dependency service as the engineer at an explicit
// version.
func add(st *depstest.Stack, dependent, prerequisite work.Item, expected domain.Version) (dependencies.Outcome, error) {
	return st.Deps.Add(context.Background(), dependencies.AddCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(depstest.Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: expected,
	})
}

func remove(st *depstest.Stack, dependent, prerequisite work.Item, expected domain.Version) (dependencies.Outcome, error) {
	return st.Deps.Remove(context.Background(), dependencies.RemoveCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(depstest.Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: expected,
	})
}

// prerequisites lists a dependent's direct prerequisite IDs through the service.
func prerequisites(t testing.TB, st *depstest.Stack, dependent work.Item) []domain.WorkItemID {
	t.Helper()
	page, err := st.Deps.List(context.Background(), dependencies.ListCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(depstest.Reader), Dependent: dependent.ID, Limit: dependencies.MaxPageRows,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []domain.WorkItemID
	for _, p := range page.Items {
		ids = append(ids, p.Link.PrerequisiteID)
	}
	return ids
}

// items creates n stories in the scope.
func items(st *depstest.Stack, scope domain.Scope, n int, name string) []work.Item {
	out := make([]work.Item, n)
	for i := range out {
		out[i] = st.Create(scope, work.KindStory, name)
	}
	return out
}

// snapshot is the observable footprint of one work item.
type snapshot struct {
	version                int64
	history, audit, outbox int
}

func snap(st *depstest.Stack, it work.Item) snapshot {
	h, a, o := st.Records(it)
	return snapshot{version: int64(st.Get(it).Version), history: h, audit: a, outbox: o}
}

// assertAcyclic fails when any work item of the tenant reaches itself through
// prerequisite links. It reads as the owner role, bypassing the application path.
func assertAcyclic(t testing.TB, st *depstest.Stack, tenant string) {
	t.Helper()
	var cyclic int
	err := st.Owner.QueryRow(context.Background(), `WITH RECURSIVE r(root, id) AS (
			SELECT dependent_id, prerequisite_id FROM work_item_dependencies WHERE tenant_id = $1
			UNION
			SELECT r.root, l.prerequisite_id FROM r JOIN work_item_dependencies l ON l.tenant_id = $1 AND l.dependent_id = r.id
		) SELECT count(*) FROM r WHERE root = id`, tenant).Scan(&cyclic)
	if err != nil {
		t.Fatal(err)
	}
	if cyclic != 0 {
		t.Fatalf("the committed graph contains %d work item(s) that depend on themselves", cyclic)
	}
}

// parallel runs fns concurrently, releasing them together, and returns their errors.
func parallel(fns ...func() error) []error {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, len(fns))
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// holder is an open tenant transaction that keeps its locks until release.
type holder struct {
	release chan func(ctx context.Context, tx pgx.Tx) error
	done    chan error
}

// hold starts a tenant transaction that runs lock, signals readiness and waits.
func hold(t testing.TB, st *depstest.Stack, tenant string, lock func(ctx context.Context, tx pgx.Tx) error) *holder {
	t.Helper()
	h := &holder{release: make(chan func(ctx context.Context, tx pgx.Tx) error, 1), done: make(chan error, 1)}
	ready := make(chan error, 1)
	go func() {
		h.done <- storage.WithTenant(context.Background(), st.Runtime, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
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
		t.Fatalf("hold: %v", err)
	}
	return h
}

// finish lets the holder run fn and commit, and returns its result.
func (h *holder) finish(fn func(ctx context.Context, tx pgx.Tx) error) error {
	h.release <- fn
	return <-h.done
}

// lockProjectRow takes the project row lock the way every graph writer and
// work transition does.
func lockProjectRow(scope domain.Scope) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		var limit int
		return tx.QueryRow(ctx, `SELECT active_wip_limit FROM projects WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE`,
			string(scope.TenantID), string(scope.ProjectID)).Scan(&limit)
	}
}

// waitBlocked waits until n backends of this test database are waiting for a lock.
func waitBlocked(t testing.TB, st *depstest.Stack, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, pgtest.AdminURL())
	if err != nil {
		t.Fatalf("admin connection: %v", err)
	}
	defer admin.Close(ctx)
	for {
		var got int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, st.DB.Name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d blocked backend(s), saw %d", n, got)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

type result[T any] struct {
	v   T
	err error
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

func withTx(st *depstest.Stack, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), st.Runtime, tenant, fn)
}

// scanTenant runs a single-row query in a tenant transaction as the runtime role.
func scanTenant(t testing.TB, st *depstest.Stack, tenant string, sql string, args []any, dest ...any) error {
	t.Helper()
	return withTx(st, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(dest...)
	})
}

func addCmd(scope domain.Scope, dependent, prerequisite work.Item, expected domain.Version) dependencies.AddCommand {
	return dependencies.AddCommand{Scope: scope, Caller: depstest.Caller(depstest.Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: expected}
}

func listCmd(scope domain.Scope, dependent work.Item) dependencies.ListCommand {
	return dependencies.ListCommand{Scope: scope, Caller: depstest.Caller(depstest.Reader), Dependent: dependent.ID, Limit: 10}
}
