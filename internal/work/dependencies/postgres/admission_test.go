package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
	deppg "github.com/optimiware/kodama/internal/work/dependencies/postgres"
)

// prerequisiteIn returns a fresh item driven to the state.
func prerequisiteIn(t testing.TB, st *depstest.Stack, scope domain.Scope, state work.State) work.Item {
	t.Helper()
	it := st.Create(scope, work.KindStory, "prerequisite "+string(state))
	switch state {
	case work.StateBacklog:
	case work.StateReady:
		it = st.Ready(it)
	case work.StateActive:
		it = st.Activate(it)
	case work.StateReview:
		it = st.Review(st.Activate(it))
	case work.StateDone:
		it = st.Complete(st.Review(st.Activate(it)))
	case work.StateCancelled:
		it = st.MustMove(it, depstest.Engineer, work.StateCancelled)
	default:
		t.Fatalf("unknown state %s", state)
	}
	if it.State != state {
		t.Fatalf("prerequisite is %s, want %s", it.State, state)
	}
	return it
}

func activeCount(st *depstest.Stack, tenant string) int {
	return st.Count(tenant, `SELECT count(*) FROM work_items WHERE tenant_id = $1 AND state = 'active'`, tenant)
}

// Test_dependency_not_done: the real work service and store with the real
// dependency guard refuse ready -> active for every prerequisite state other
// than done, including cancellation; all-done and no-link cases are admitted;
// a refused admission changes nothing. Real PostgreSQL; no NATS, S3 or Git.
// WM-03, WM-05, WM-A04, IT-11.
func Test_dependency_not_done(t *testing.T) {
	st := depstest.NewStack(t)

	for _, state := range []work.State{work.StateBacklog, work.StateReady, work.StateActive, work.StateReview, work.StateCancelled} {
		t.Run("a_"+string(state)+"_prerequisite_blocks", func(t *testing.T) {
			dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
			pre := prerequisiteIn(t, st, scopeA, state)
			linked := st.MustLink(dep, pre)
			dep = st.Get(dep)
			if dep.Version != linked.Owner.Version {
				t.Fatalf("version %d vs %d", dep.Version, linked.Owner.Version)
			}
			before, active := snap(st, dep), activeCount(st, tenantA)

			_, err := st.Move(dep, depstest.Engineer, work.StateActive)
			if !is(err, work.ErrPrerequisite) {
				t.Fatalf("admission with a %s prerequisite: %v", state, err)
			}
			if is(err, work.ErrWIPLimit) || is(err, work.ErrUnavailable) {
				t.Fatalf("the refusal must be the dependency failure, got %v", err)
			}
			for _, leak := range []string{string(pre.ID), string(state)} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("the refusal discloses %q: %v", leak, err)
				}
			}
			if after := snap(st, dep); after != before || st.Get(dep).State != work.StateReady {
				t.Fatalf("a refused admission changed state: %+v -> %+v", before, after)
			}
			if got := activeCount(st, tenantA); got != active {
				t.Fatalf("a refused admission consumed WIP: %d -> %d", active, got)
			}
		})
	}

	t.Run("a_done_prerequisite_admits", func(t *testing.T) {
		dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
		st.MustLink(dep, prerequisiteIn(t, st, scopeA, work.StateDone))
		if got := st.MustMove(st.Get(dep), depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("state %s", got.State)
		}
	})

	t.Run("every_prerequisite_must_be_done", func(t *testing.T) {
		dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
		done1, done2 := prerequisiteIn(t, st, scopeA, work.StateDone), prerequisiteIn(t, st, scopeA, work.StateDone)
		pending := prerequisiteIn(t, st, scopeA, work.StateReview)
		st.MustLink(dep, done1)
		st.MustLink(dep, done2)
		st.MustLink(dep, pending)
		if _, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("two done and one review prerequisite: %v", err)
		}
		// Completing the last one satisfies the link; nothing else changes on the dependent.
		st.Complete(pending)
		if got := st.MustMove(st.Get(dep), depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("state %s", got.State)
		}
	})

	t.Run("no_links_admit", func(t *testing.T) {
		dep := st.Ready(st.Create(scopeA, work.KindStory, "free"))
		if got := st.MustMove(dep, depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("state %s", got.State)
		}
	})

	t.Run("reopening_a_done_prerequisite_withdraws_the_satisfaction", func(t *testing.T) {
		dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
		pre := prerequisiteIn(t, st, scopeA, work.StateDone)
		st.MustLink(dep, pre)
		st.MustMove(pre, depstest.Reviewer, work.StateActive) // reopen
		if _, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("a reopened prerequisite: %v", err)
		}
	})

	t.Run("a_dependency_failure_does_not_consume_wip", func(t *testing.T) {
		st := depstest.NewStack(t)
		st.OwnerExec(`UPDATE projects SET active_wip_limit = 1 WHERE tenant_id = $1 AND id = $2`, tenantA, projectA)
		blocked := st.Ready(st.Create(scopeA, work.KindStory, "blocked by a dependency"))
		free := st.Ready(st.Create(scopeA, work.KindStory, "free"))
		st.MustLink(blocked, st.Create(scopeA, work.KindStory, "unfinished"))
		// The dependency failure is reported even though capacity exists, and the
		// slot stays free for unrelated work.
		if _, err := st.Move(st.Get(blocked), depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("blocked: %v", err)
		}
		if got := st.MustMove(free, depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("the free item could not take the slot: %v", got.State)
		}
		// Once the slot is taken, an unblocked dependent sees the WIP limit, not a dependency failure.
		other := st.Ready(st.Create(scopeA, work.KindStory, "other"))
		if _, err := st.Move(other, depstest.Engineer, work.StateActive); !is(err, work.ErrWIPLimit) {
			t.Fatalf("with no capacity: %v", err)
		}
	})
}

// Test_dependency_deleted_before_admission: a committed link deletion is
// reflected in admission rather than a cached graph; stale owner versions
// conflict and a fresh-version retry sees the deletion; controlled lock
// ordering and reverse ordering give only the allowed outcomes. The deletion is
// that of the link: work items are never deleted by request handling. Real
// PostgreSQL; no NATS, S3 or Git. WM-03, WM-05, IT-11.
func Test_dependency_deleted_before_admission(t *testing.T) {
	setup := func(t *testing.T) (*depstest.Stack, work.Item, work.Item) {
		st := depstest.NewStack(t)
		dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
		pre := st.Create(scopeA, work.KindStory, "unfinished prerequisite")
		st.MustLink(dep, pre)
		return st, st.Get(dep), pre
	}

	t.Run("a_committed_deletion_is_seen_by_the_next_admission", func(t *testing.T) {
		st, dep, pre := setup(t)
		if _, err := st.Move(dep, depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("with the link: %v", err)
		}
		out, err := st.Unlink(dep, pre)
		if err != nil || !out.Changed {
			t.Fatalf("unlink: %+v %v", out, err)
		}
		// The deletion bumped the owner's version, so the pre-deletion version is stale.
		if _, err := st.Move(dep, depstest.Engineer, work.StateActive); !is(err, work.ErrVersionConflict) {
			t.Fatalf("stale version after the deletion: %v", err)
		}
		if got := st.Get(dep); got.State != work.StateReady || got.Version != out.Owner.Version {
			t.Fatalf("a conflicting admission changed the item: %+v", got)
		}
		// A fresh-version retry sees the deletion.
		got := st.MustMove(st.Get(dep), depstest.Engineer, work.StateActive)
		if got.State != work.StateActive {
			t.Fatalf("state %s", got.State)
		}
		// The prerequisite is untouched: deleting a link never deletes a work item.
		if st.Get(pre).State != work.StateBacklog {
			t.Fatal("the prerequisite changed")
		}
	})

	t.Run("cancelling_a_prerequisite_never_satisfies_it_but_deleting_the_link_does", func(t *testing.T) {
		st, dep, pre := setup(t)
		st.MustMove(pre, depstest.Engineer, work.StateCancelled)
		if _, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("cancelled prerequisite: %v", err)
		}
		if _, err := st.Unlink(dep, pre); err != nil {
			t.Fatal(err)
		}
		if got := st.MustMove(st.Get(dep), depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("state %s", got.State)
		}
	})

	t.Run("a_deletion_in_flight_commits_before_a_waiting_admission", func(t *testing.T) {
		st, dep, pre := setup(t)
		// An in-flight deletion: the holder performs exactly what the store does
		// (project lock, item lock, delete, version bump) and has not committed.
		h := hold(t, st, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if err := lockProjectRow(scopeA)(ctx, tx); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantA, string(dep.ID)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND dependent_id = $2 AND prerequisite_id = $3`, tenantA, string(dep.ID), string(pre.ID)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE work_items SET version = version + 1 WHERE tenant_id = $1 AND id = $2`, tenantA, string(dep.ID))
			return err
		})
		blocked := async(func() (work.Item, error) { return st.Move(dep, depstest.Engineer, work.StateActive) })
		waitBlocked(t, st, 1)
		select {
		case r := <-blocked:
			t.Fatalf("the admission did not wait for the in-flight graph change: %+v", r.err)
		default:
		}
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		if r := <-blocked; !is(r.err, work.ErrVersionConflict) {
			t.Fatalf("the waiting admission after the deletion committed: %v", r.err)
		}
		if got := st.MustMove(st.Get(dep), depstest.Engineer, work.StateActive); got.State != work.StateActive {
			t.Fatalf("retry: %s", got.State)
		}
	})

	t.Run("an_admission_in_flight_serializes_a_deletion_behind_it", func(t *testing.T) {
		st, dep, pre := setup(t)
		// Reverse ordering: the holder is an admission that took the project lock
		// first; the deletion must wait for it, not slip between its lock and its read.
		h := hold(t, st, tenantA, lockProjectRow(scopeA))
		removed := async(func() (any, error) { return st.Unlink(dep, pre) })
		waitBlocked(t, st, 1)
		select {
		case <-removed:
			t.Fatal("the deletion did not wait for the in-flight admission")
		default:
		}
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		if r := <-removed; r.err != nil {
			t.Fatalf("deletion after the admission: %v", r.err)
		}
	})

	t.Run("a_racing_admission_and_deletion_never_admit_through_the_link", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			st, dep, pre := setup(t)
			h := hold(t, st, tenantA, lockProjectRow(scopeA))
			move := async(func() (work.Item, error) { return st.Move(dep, depstest.Engineer, work.StateActive) })
			waitBlocked(t, st, 1)
			unlink := async(func() (any, error) { _, err := remove(st, dep, pre, dep.Version); return nil, err })
			waitBlocked(t, st, 2)
			if err := h.finish(nil); err != nil {
				t.Fatal(err)
			}
			m, u := <-move, <-unlink
			// Both started from the same version: whichever commits first makes the
			// other stale or refused. The admission can never succeed while the link
			// to an unfinished prerequisite exists.
			switch {
			case is(m.err, work.ErrPrerequisite) && u.err == nil: // admission first, then the deletion
			case is(m.err, work.ErrVersionConflict) && u.err == nil: // deletion first
			default:
				t.Fatalf("round %d: admission = %v, deletion = %v", round, m.err, u.err)
			}
			if st.Get(dep).State != work.StateReady {
				t.Fatalf("round %d: the dependent left ready", round)
			}
		}
	})

	t.Run("a_deleted_prerequisite_row_fails_admission_instead_of_being_omitted", func(t *testing.T) {
		st, dep, pre := setup(t)
		// Corrupt the graph as the owner: drop the referenced work item row and its
		// foreign key. An inner join would omit the link and admit; the guard must not.
		st.OwnerExec(`ALTER TABLE work_item_dependencies DROP CONSTRAINT work_item_dependencies_prerequisite_fkey`)
		st.OwnerExec(`ALTER TABLE work_item_history DROP CONSTRAINT work_item_history_tenant_id_project_id_work_item_id_fkey`)
		st.OwnerExec(`DELETE FROM work_items WHERE tenant_id = $1 AND id = $2`, tenantA, string(pre.ID))
		if _, err := st.Move(dep, depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("a dangling link: %v", err)
		}
		if st.Get(dep).State != work.StateReady {
			t.Fatal("admitted through a dangling link")
		}
	})
}

// Test_dependency_admission_races: link insertion versus admission and
// prerequisite completion or reopening versus admission respect the shared
// project lock; dependency failures do not consume WIP; the merged rework and
// reopen policy is preserved. Real PostgreSQL. WM-03, WM-04, WM-05.
func Test_dependency_admission_races(t *testing.T) {
	t.Run("insertion_versus_admission_has_one_winner", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			st := depstest.NewStack(t)
			dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
			pre := st.Create(scopeA, work.KindStory, "unfinished")
			h := hold(t, st, tenantA, lockProjectRow(scopeA))
			move := async(func() (work.Item, error) { return st.Move(dep, depstest.Engineer, work.StateActive) })
			waitBlocked(t, st, 1)
			insert := async(func() (any, error) { _, err := add(st, dep, pre, dep.Version); return nil, err })
			waitBlocked(t, st, 2)
			if err := h.finish(nil); err != nil {
				t.Fatal(err)
			}
			m, i := <-move, <-insert
			got := st.Get(dep)
			switch {
			case m.err == nil && is(i.err, work.ErrVersionConflict):
				// Admission committed first: the late link cannot retroactively undo it.
				if got.State != work.StateActive || len(prerequisites(t, st, dep)) != 0 {
					t.Fatalf("round %d: %+v", round, got)
				}
			case i.err == nil && is(m.err, work.ErrVersionConflict):
				if got.State != work.StateReady || len(prerequisites(t, st, dep)) != 1 {
					t.Fatalf("round %d: %+v", round, got)
				}
				// A fresh-version admission now sees the link.
				if _, err := st.Move(got, depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
					t.Fatalf("round %d: %v", round, err)
				}
			default:
				t.Fatalf("round %d: admission = %v, insertion = %v", round, m.err, i.err)
			}
		}
	})

	t.Run("prerequisite_completion_versus_admission", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			st := depstest.NewStack(t)
			dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
			pre := st.Review(st.Activate(st.Create(scopeA, work.KindStory, "in review")))
			st.MustLink(dep, pre)
			dep = st.Get(dep)
			h := hold(t, st, tenantA, lockProjectRow(scopeA))
			complete := async(func() (work.Item, error) {
				return st.Move(pre, depstest.Reviewer, work.StateDone, func(r *work.TransitionRequest) {
					r.Reason = "accepted"
					r.Completion = &work.CompletionClaim{ContentRevision: pre.ContentRevision, CandidateCommit: depstest.Commit, AcceptedCriteria: []string{"AC-1"}}
				})
			})
			waitBlocked(t, st, 1)
			move := async(func() (work.Item, error) { return st.Move(dep, depstest.Engineer, work.StateActive) })
			waitBlocked(t, st, 2)
			if err := h.finish(nil); err != nil {
				t.Fatal(err)
			}
			c, m := <-complete, <-move
			if c.err != nil {
				t.Fatalf("round %d: completion: %v", round, c.err)
			}
			// The admission either ran before the completion committed (refused) or
			// after (admitted): it never observes a half state, and an admission
			// that succeeded implies the prerequisite is done now.
			switch {
			case m.err == nil:
				if st.Get(pre).State != work.StateDone || st.Get(dep).State != work.StateActive {
					t.Fatalf("round %d: admitted without a done prerequisite", round)
				}
			case is(m.err, work.ErrPrerequisite):
				if got, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); err != nil || got.State != work.StateActive {
					t.Fatalf("round %d: retry after the completion: %v", round, err)
				}
			default:
				t.Fatalf("round %d: admission: %v", round, m.err)
			}
		}
	})

	t.Run("prerequisite_reopen_versus_admission", func(t *testing.T) {
		for round := 0; round < 5; round++ {
			st := depstest.NewStack(t)
			dep := st.Ready(st.Create(scopeA, work.KindStory, "dependent"))
			pre := st.Complete(st.Review(st.Activate(st.Create(scopeA, work.KindStory, "done"))))
			st.MustLink(dep, pre)
			dep = st.Get(dep)
			h := hold(t, st, tenantA, lockProjectRow(scopeA))
			reopen := async(func() (work.Item, error) { return st.Move(pre, depstest.Reviewer, work.StateActive) })
			waitBlocked(t, st, 1)
			move := async(func() (work.Item, error) { return st.Move(dep, depstest.Engineer, work.StateActive) })
			waitBlocked(t, st, 2)
			if err := h.finish(nil); err != nil {
				t.Fatal(err)
			}
			r, m := <-reopen, <-move
			if r.err != nil {
				t.Fatalf("round %d: reopen: %v", round, r.err)
			}
			switch {
			case m.err == nil:
				// Admission ran while the prerequisite was still done; it stands.
				if st.Get(dep).State != work.StateActive {
					t.Fatalf("round %d", round)
				}
			case is(m.err, work.ErrPrerequisite):
				if st.Get(dep).State != work.StateReady {
					t.Fatalf("round %d: a refused admission changed state", round)
				}
			default:
				t.Fatalf("round %d: %v", round, m.err)
			}
		}
	})

	t.Run("rework_and_reopen_keep_the_merged_policy", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep := st.Review(st.Activate(st.Create(scopeA, work.KindStory, "dependent")))
		// A link added after admission does not retroactively block it, and the
		// merged transition table invokes the dependency guard for ready -> active
		// only: rework (review -> active) and reopen (done -> active) use WIP
		// admission alone.
		pending := st.Create(scopeA, work.KindStory, "unfinished")
		if _, err := st.Link(dep, pending); err != nil {
			t.Fatal(err)
		}
		if got, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); err != nil || got.State != work.StateActive {
			t.Fatalf("rework with an unfinished prerequisite: %v", err)
		}
		if _, err := st.Move(st.Get(dep), depstest.Engineer, work.StateReady); err != nil {
			t.Fatalf("active -> ready: %v", err)
		}
		// Back to ready, the guard applies again.
		if _, err := st.Move(st.Get(dep), depstest.Engineer, work.StateActive); !is(err, work.ErrPrerequisite) {
			t.Fatalf("ready -> active with an unfinished prerequisite: %v", err)
		}
	})

	t.Run("the_guard_is_bound_to_the_callers_transaction", func(t *testing.T) {
		st := depstest.NewStack(t)
		dep := st.Create(scopeA, work.KindStory, "dependent")
		pre := st.Create(scopeA, work.KindStory, "prerequisite")
		st.MustLink(dep, pre)
		// An uncommitted link in a caller's transaction is visible to that
		// transaction's guard and to no other connection: nothing is cached.
		var inside, outside error
		h := hold(t, st, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DELETE FROM work_item_dependencies WHERE tenant_id = $1 AND dependent_id = $2`, tenantA, string(dep.ID)); err != nil {
				return err
			}
			inside = deppg.NewDependencyGuard(tx).DependenciesSatisfied(ctx, scopeA, dep.ID)
			return nil
		})
		outside = guardOnFreshTx(t, st, dep)
		if err := h.finish(nil); err != nil {
			t.Fatal(err)
		}
		if inside != nil {
			t.Fatalf("inside the deleting transaction: %v", inside)
		}
		if !is(outside, work.ErrPrerequisite) {
			t.Fatalf("from another connection: %v", outside)
		}
	})

	t.Run("a_guard_without_a_transaction_fails_closed", func(t *testing.T) {
		g := deppg.NewDependencyGuard(nil)
		err := g.DependenciesSatisfied(context.Background(), scopeA, "wi_x")
		if !is(err, work.ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
}

// guardOnFreshTx evaluates the guard in a separate tenant transaction.
func guardOnFreshTx(t testing.TB, st *depstest.Stack, dep work.Item) error {
	t.Helper()
	var out error
	err := withTx(st, dep.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		out = deppg.NewDependencyGuard(tx).DependenciesSatisfied(ctx, dep.Scope, dep.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
