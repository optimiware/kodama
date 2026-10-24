package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// reserve is the fixture stand-in for T-013's unique workspace reservation.
func (e *env) reserve(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.WorkItemID, owner string) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO t011_reservations (tenant_id, work_item_id, owner) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		string(scope.TenantID), string(id), owner)
	return tag.RowsAffected() == 1, err
}

// Test_workspace_admission: concurrent calls through the locked actionable-item
// seam cooperate with a unique fixture reservation; epics, cancelled and foreign
// items are refused; cancellation serializes against an open admission. The
// production workspace constraint belongs to T-013. Real PostgreSQL, no Git.
// WM-11, WM-A07.
func Test_workspace_admission(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	story := e.activate(scopeA, "provisioned once")

	t.Run("concurrent_provisioning_yields_one_reservation", func(t *testing.T) {
		const n = 8
		fns := make([]func() error, n)
		won := make(chan bool, n)
		for i := range fns {
			owner := "w" + string(rune('a'+i))
			fns[i] = func() error {
				return storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
					a, err := workpg.LockActionable(ctx, tx, scopeA, story.ID)
					if err != nil {
						return err
					}
					if a.Kind != work.KindStory || a.State != work.StateActive || a.Version != story.Version || a.ContentRevision != story.ContentRevision {
						t.Errorf("admission = %+v", a)
					}
					ok, err := e.reserve(ctx, tx, scopeA, story.ID, owner)
					won <- ok
					return err
				})
			}
		}
		for _, err := range parallel(fns...) {
			if err != nil {
				t.Fatal(err)
			}
		}
		close(won)
		winners := 0
		for ok := range won {
			if ok {
				winners++
			}
		}
		if winners != 1 || e.count(tenantA, `SELECT count(*) FROM t011_reservations WHERE work_item_id = $1`, string(story.ID)) != 1 {
			t.Fatalf("winners = %d", winners)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		epic := e.create(scopeA, work.KindEpic, "epic")
		foreign := e.create(scopeB, work.KindStory, "foreign")
		scopeA2 := domain.Scope{TenantID: tenantA, ProjectID: "p_other"}
		e.seedProject(tenantA, "p_other")
		cancelled := e.mustMove(e.create(scopeA, work.KindStory, "cancelled"), engineer, work.StateCancelled)
		for name, tc := range map[string]struct {
			scope domain.Scope
			id    domain.WorkItemID
			want  error
		}{
			"epic":            {scopeA, epic.ID, work.ErrNotActionable},
			"another tenant":  {scopeA, foreign.ID, work.ErrNotFound},
			"another project": {scopeA2, story.ID, work.ErrNotFound},
			"missing":         {scopeA, "wi_missing", work.ErrNotFound},
			"cancelled":       {scopeA, cancelled.ID, work.ErrCancelled},
		} {
			err := storage.WithTenant(ctx, e.rt, tc.scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := workpg.LockActionable(ctx, tx, tc.scope, tc.id)
				return err
			})
			if !is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A tenant context that differs from the scope cannot see the item at all.
		err := storage.WithTenant(ctx, e.rt, tenantB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := workpg.LockActionable(ctx, tx, scopeA, story.ID)
			return err
		})
		if !is(err, work.ErrNotFound) {
			t.Errorf("a transaction in another tenant: %v", err)
		}
	})

	t.Run("cancellation_waits_for_an_open_admission_and_later_claims_are_refused", func(t *testing.T) {
		item := e.activate(scopeA, "admitted then cancelled")
		h := e.hold(tenantA, lockActionable(scopeA, item.ID))
		cancel := e.moveAsync(item, engineer, work.StateCancelled)
		e.waitBlocked(1)
		select {
		case r := <-cancel:
			t.Fatalf("cancellation passed an open admission: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		// The reservation commits together with the admission; then the cancel proceeds.
		if err := h.finish(func(ctx context.Context, tx pgx.Tx) error {
			_, err := e.reserve(ctx, tx, scopeA, item.ID, "late")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := <-cancel; r.err != nil || r.v.State != work.StateCancelled {
			t.Fatalf("cancellation: %+v", r)
		}
		err := storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := workpg.LockActionable(ctx, tx, scopeA, item.ID)
			return err
		})
		if !is(err, work.ErrCancelled) {
			t.Fatalf("claim after cancellation: %v", err)
		}
	})
}
