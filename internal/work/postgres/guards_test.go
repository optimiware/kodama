package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/work"
)

// Test_prerequisite_races: a writer reservation, a change of the reviewed
// subject, a content change and an authorization revocation that commit while a
// transition waits for the item lock cannot be bypassed: every prerequisite is
// revalidated inside the transaction after the lock is acquired. Real
// PostgreSQL. WM-03, WM-07.
func Test_prerequisite_races(t *testing.T) {
	t.Run("a_writer_reserved_while_leaving_active_blocks_the_transition", func(t *testing.T) {
		e := newEnv(t)
		it := e.activate(scopeA, "writer arrives")
		// The execution module reserves the writer through the admission seam.
		h := e.hold(tenantA, lockActionable(scopeA, it.ID))
		leave := e.moveAsync(it, engineer, work.StateReady)
		e.waitBlocked(1)
		if err := h.finish(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO t011_writers (tenant_id, work_item_id, attempt_id, unsettled) VALUES ($1, $2, 'att_1', true)`, tenantA, string(it.ID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := <-leave; !is(r.err, work.ErrWorkspaceBusy) {
			t.Fatalf("leaving active after a reservation committed: %+v", r)
		}
		if got := e.get(it); got.State != work.StateActive || got.Version != it.Version {
			t.Fatalf("item = %+v", got)
		}
		// Once the writer settles the same request succeeds; cancellation never waited.
		e.ownerExec(`UPDATE t011_writers SET unsettled = false`)
		if _, err := e.move(it, engineer, work.StateReady); err != nil {
			t.Fatalf("after settlement: %v", err)
		}
	})

	t.Run("cancellation_does_not_wait_for_an_unsettled_writer", func(t *testing.T) {
		e := newEnv(t)
		it := e.activate(scopeA, "cancel while writing")
		e.ownerExec(`INSERT INTO t011_writers (tenant_id, work_item_id, attempt_id, unsettled) VALUES ($1, $2, 'att_1', true)`, tenantA, string(it.ID))
		if _, err := e.move(it, engineer, work.StateReady); !is(err, work.ErrWorkspaceBusy) {
			t.Fatalf("leave active: %v", err)
		}
		got, err := e.move(it, engineer, work.StateCancelled)
		if err != nil || got.State != work.StateCancelled {
			t.Fatalf("cancellation with an unsettled writer: %v", err)
		}
	})

	t.Run("the_reviewed_subject_changing_during_the_wait_is_detected", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "subject moves"))
		h := e.hold(tenantA, lockItemRow(scopeA, it.ID))
		complete := e.moveAsync(it, reviewer, work.StateDone, completion(it))
		e.waitBlocked(1)
		if err := h.finish(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE t011_workspaces SET accepted_commit = $2 WHERE work_item_id = $1`, string(it.ID), commitB)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := <-complete; !is(r.err, work.ErrStaleSubject) {
			t.Fatalf("completion against a moved subject: %+v", r)
		}
		if got := e.get(it); got.State != work.StateReview {
			t.Fatalf("state = %s", got.State)
		}
	})

	t.Run("a_content_change_during_the_wait_is_a_version_conflict", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "content moves"))
		h := e.hold(tenantA, lockItemRow(scopeA, it.ID))
		complete := e.moveAsync(it, reviewer, work.StateDone, completion(it))
		e.waitBlocked(1)
		if err := h.finish(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE work_items SET spec = 'changed', content_revision = content_revision + 1, version = version + 1 WHERE id = $1`, string(it.ID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := <-complete; !is(r.err, work.ErrVersionConflict) {
			t.Fatalf("completion against changed content: %+v", r)
		}
	})

	t.Run("revocation_during_the_wait_is_detected", func(t *testing.T) {
		e := newEnv(t)
		it := e.toReview(e.activate(scopeA, "revoked reviewer"))
		h := e.hold(tenantA, lockItemRow(scopeA, it.ID))
		complete := e.moveAsync(it, reviewer, work.StateDone, completion(it))
		e.waitBlocked(1)
		// The early authorization already passed; revoke before the lock is released.
		e.revoke(tenantA, reviewer, work.PermComplete)
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-complete:
			if !is(r.err, work.ErrDenied) {
				t.Fatalf("completion after revocation: %+v", r)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("completion did not finish")
		}
		if got := e.get(it); got.State != work.StateReview || got.Completion != nil {
			t.Fatalf("item = %+v", got)
		}
	})

	t.Run("a_guard_adapter_failure_is_unavailability_never_a_pass", func(t *testing.T) {
		e := newEnv(t)
		it := e.activate(scopeA, "guard breaks")
		e.ownerExec(`ALTER TABLE t011_writers RENAME TO t011_writers_gone`)
		if _, err := e.move(it, engineer, work.StateReady); !is(err, work.ErrUnavailable) {
			t.Fatalf("a failing writer guard: %v", err)
		}
		if got := e.get(it); got.State != work.StateActive {
			t.Fatalf("state = %s", got.State)
		}
	})
}
