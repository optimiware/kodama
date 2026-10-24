package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
	deppg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
)

func addAs(st *depstest.Stack, principal string, dependent, prerequisite work.Item) error {
	_, err := st.Deps.Add(context.Background(), dependencies.AddCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(principal), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: st.Get(dependent).Version,
	})
	return err
}

func listAs(st *depstest.Stack, principal string, dependent work.Item) error {
	_, err := st.Deps.List(context.Background(), dependencies.ListCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(principal), Dependent: dependent.ID, Limit: 10,
	})
	return err
}

// failingChecker is a transaction-bound permission checker whose identity
// backend is unreachable.
type failingChecker struct{}

func (failingChecker) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return errors.New("identity backend unreachable")
}

// Test_dependency_authorization: denial before persistence, revocation during a
// lock wait, a missing permission binder and authority outages all fail closed.
// Real PostgreSQL; no NATS, S3 or Git. WM-01, IT-02, IT-03, IT-05, SD-A04.
func Test_dependency_authorization(t *testing.T) {
	t.Run("denial_happens_before_persistence", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		st.MustLink(dep, st.Create(scopeA, work.KindStory, "existing"))
		before := snap(st, dep)
		links := st.Links(tenantA)

		// A reader may read but not mutate; an outsider may do neither.
		if err := addAs(st, depstest.Reader, dep, pre); !is(err, work.ErrDenied) {
			t.Fatalf("reader add: %v", err)
		}
		if _, err := st.Deps.Remove(context.Background(), dependencies.RemoveCommand{
			Scope: scopeA, Caller: depstest.Caller(depstest.Reader), Dependent: dep.ID, Prerequisite: pre.ID, Expected: st.Get(dep).Version}); !is(err, work.ErrDenied) {
			t.Fatalf("reader remove: %v", err)
		}
		if err := addAs(st, depstest.Outsider, dep, pre); !is(err, work.ErrDenied) {
			t.Fatalf("outsider add: %v", err)
		}
		if err := listAs(st, depstest.Outsider, dep); !is(err, work.ErrDenied) {
			t.Fatalf("outsider list: %v", err)
		}
		if err := listAs(st, depstest.Reader, dep); err != nil {
			t.Fatalf("reader list: %v", err)
		}
		// An unknown principal is hidden; so is a principal of another tenant.
		if err := addAs(st, "u_nobody", dep, pre); !is(err, work.ErrHidden) {
			t.Fatalf("unknown principal: %v", err)
		}
		if _, err := st.Deps.Add(context.Background(), dependencies.AddCommand{Scope: scopeA, Caller: work.Caller{}, Dependent: dep.ID, Prerequisite: pre.ID, Expected: 1}); !is(err, work.ErrDenied) {
			t.Fatalf("anonymous: %v", err)
		}
		if after := snap(st, dep); after != before || st.Links(tenantA) != links {
			t.Fatalf("a denied request changed state: %+v -> %+v", before, after)
		}
	})

	t.Run("a_denied_caller_learns_nothing_about_versions_or_references", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep := st.Create(scopeA, work.KindStory, "dep")
		// Wrong version and a missing prerequisite still answer "denied" for a
		// caller without permission, never "stale" or "not found".
		_, err := st.Deps.Add(context.Background(), dependencies.AddCommand{Scope: scopeA, Caller: depstest.Caller(depstest.Reader), Dependent: dep.ID, Prerequisite: "wi_missing", Expected: 99})
		if !is(err, work.ErrDenied) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a_revocation_during_a_lock_wait_is_seen_before_anything_is_written", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		before := snap(st, dep)
		h := hold(t, st, tenantA, lockProjectRow(scopeA))
		// The service-level authorization passes, then the store waits for the lock.
		pending := async(func() (any, error) { return nil, addAs(st, depstest.Engineer, dep, pre) })
		waitBlocked(t, st, 1)
		st.Revoke(tenantA, depstest.Engineer, work.PermEditSpec)
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		if r := <-pending; !is(r.err, work.ErrDenied) {
			t.Fatalf("the revoked caller's mutation: %v", r.err)
		}
		if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
			t.Fatalf("a revoked mutation wrote: %+v -> %+v", before, after)
		}
	})

	t.Run("a_revocation_while_waiting_on_the_item_lock_is_seen_too", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		// Hold the dependent row (an in-flight edit) so the store passes the project
		// lock and waits on the item lock.
		h := hold(t, st, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantA, string(dep.ID))
			return err
		})
		pending := async(func() (any, error) { return nil, addAs(st, depstest.Engineer, dep, pre) })
		waitBlocked(t, st, 1)
		st.Revoke(tenantA, depstest.Engineer, work.PermEditSpec)
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		if r := <-pending; !is(r.err, work.ErrDenied) {
			t.Fatalf("err = %v", r.err)
		}
		if st.Links(tenantA) != 0 {
			t.Fatal("a link was written")
		}
	})

	t.Run("a_missing_permission_binder_fails_closed", func(t *testing.T) {
		if _, err := deppg.NewStore(nil, nil, nil); err == nil {
			t.Fatal("a store without collaborators was constructed")
		}
		st := depstest.NewStack(t, depstest.Options{PermissionBinder: func(pgx.Tx) work.PermissionChecker { return nil }})
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		before := snap(st, dep)
		if err := addAs(st, depstest.Engineer, dep, pre); !is(err, work.ErrUnavailable) {
			t.Fatalf("a nil bound checker must fail closed: %v", err)
		}
		if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
			t.Fatal("a mutation without a permission binder wrote")
		}
		// Reading does not depend on the transaction-bound checker.
		if err := listAs(st, depstest.Reader, dep); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a_failing_transaction_bound_authority_is_unavailable_not_access", func(t *testing.T) {
		st := depstest.NewStack(t, depstest.Options{PermissionBinder: func(pgx.Tx) work.PermissionChecker { return failingChecker{} }})
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		if err := addAs(st, depstest.Engineer, dep, pre); !is(err, work.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
		if st.Links(tenantA) != 0 {
			t.Fatal("a link was written")
		}
	})

	t.Run("an_authority_outage_fails_closed_for_reads_and_writes", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep, pre := st.Create(scopeA, work.KindStory, "dep"), st.Create(scopeA, work.KindStory, "pre")
		before := snap(st, dep)
		st.Authority.Outage.Store(true)
		if err := addAs2(st, dep, pre, before.version); !is(err, work.ErrUnavailable) {
			t.Fatalf("add during the outage: %v", err)
		}
		if err := listAs(st, depstest.Reader, dep); !is(err, work.ErrUnavailable) {
			t.Fatalf("list during the outage: %v", err)
		}
		st.Authority.Outage.Store(false)
		if after := snap(st, dep); after != before || st.Links(tenantA) != 0 {
			t.Fatal("the outage let a write through")
		}
		if err := addAs(st, depstest.Engineer, dep, pre); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})

	t.Run("collaborators_are_required", func(t *testing.T) {
		if _, err := dependencies.NewService(dependencies.Config{}); err == nil {
			t.Fatal("a service without a repository and authority was constructed")
		}
		st := depstest.NewStack(t)
		if _, err := dependencies.NewService(dependencies.Config{Repository: st.DepStore}); err == nil {
			t.Fatal("a service without an authority was constructed")
		}
		if _, err := dependencies.NewService(dependencies.Config{Authority: st.Authority}); err == nil {
			t.Fatal("a service without a repository was constructed")
		}
	})

	t.Run("the_database_runtime_role_is_not_privileged", func(t *testing.T) {
		st := depstest.NewStack(t)
		var super, bypass bool
		if err := st.Runtime.QueryRow(context.Background(), `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
			t.Fatal(err)
		}
		if super || bypass {
			t.Fatalf("the runtime role is superuser=%v bypassrls=%v", super, bypass)
		}
	})
}

func addAs2(st *depstest.Stack, dependent, prerequisite work.Item, version int64) error {
	_, err := st.Deps.Add(context.Background(), dependencies.AddCommand{
		Scope: dependent.Scope, Caller: depstest.Caller(depstest.Engineer), Dependent: dependent.ID, Prerequisite: prerequisite.ID, Expected: domain.Version(version),
	})
	return err
}
