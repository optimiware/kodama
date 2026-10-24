package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/work"
)

type run struct {
	startedVersion, endedVersion int64
	ended                        bool
	started, endedAt             time.Time
}

func (e *env) blockedRuns(it work.Item) []run {
	e.t.Helper()
	var out []run
	err := e.query(string(it.Scope.TenantID), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT started_at, started_version, ended_at, ended_version FROM work_flow_blocked_runs
			WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3 ORDER BY run_no`,
			string(it.Scope.TenantID), string(it.Scope.ProjectID), string(it.ID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r run
			var endAt *time.Time
			var endV *int64
			if err := rows.Scan(&r.started, &r.startedVersion, &endAt, &endV); err != nil {
				return err
			}
			if endAt != nil {
				r.ended, r.endedAt, r.endedVersion = true, endAt.UTC(), *endV
			}
			r.started = r.started.UTC()
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// Test_blocked_intervals: real T-011 history of setting, replacing and clearing a
// blocked reason, unrelated edits, state changes, a terminal state and a reopen
// yields coalesced blocked runs with source versions, and blocked time that
// overlaps (does not add to) active and review time. WM-06, WM-12, WM-A08.
func Test_blocked_intervals(t *testing.T) {
	e := newEnv(t)
	it := e.create(scopeA, at(0), "blocked flow")                                  // v1
	it = e.toActive(it, at(mins(10)), at(mins(20)))                                // v2 ready, v3 active
	it = e.block(it, at(mins(30)), "waiting on ERP")                               // v4 run 1 starts
	it = e.edit(it, at(mins(40)), work.Patch{Title: ptr("renamed while blocked")}) // v5 unrelated edit
	it = e.block(it, at(mins(50)), "waiting on legal")                             // v6 replacement, same run
	it = e.unblock(it, at(mins(60)))                                               // v7 run 1 ends
	it = e.block(it, at(mins(70)), "waiting on vendor")                            // v8 run 2 starts
	it = e.move(it, at(mins(80)), work.StateReview)                                // v9 blocked continues into review
	it = e.move(it, at(mins(90)), work.StateActive)                                // v10 rework
	it = e.move(it, at(mins(100)), work.StateReview)                               // v11
	it = e.move(it, at(mins(110)), work.StateDone)                                 // v12 run 2 ends: terminal suspends accrual
	it = e.move(it, at(mins(130)), work.StateActive)                               // v13 reopen: reason still recorded, run 3 starts
	it = e.unblock(it, at(mins(150)))                                              // v14 run 3 ends
	it = e.move(it, at(mins(170)), work.StateCancelled)                            // v15

	runs := e.blockedRuns(it)
	want := []run{
		{started: at(mins(30)), startedVersion: 4, ended: true, endedAt: at(mins(60)), endedVersion: 7},
		{started: at(mins(70)), startedVersion: 8, ended: true, endedAt: at(mins(110)), endedVersion: 12},
		{started: at(mins(130)), startedVersion: 13, ended: true, endedAt: at(mins(150)), endedVersion: 14},
	}
	if len(runs) != len(want) {
		t.Fatalf("blocked runs = %+v, want %+v", runs, want)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i+1, runs[i], want[i])
		}
	}

	expect{
		lead: mins(150), active: mins(110), review: mins(20), blocked: mins(30 + 40 + 20), reopened: mins(40),
		completedLead: mins(110), reopens: 1, blockedIntervals: 3, completed: 1, cancelled: 1,
		state: "cancelled", observed: 15,
	}.check(t, e.summarize(it, window(at(0), at(mins(200)))))

	// Blocked time is not additive: it overlaps the active and review time it
	// happened in, so it never exceeds lead and the parts need not sum.
	got := e.summarize(it, window(at(0), at(mins(200))))
	if got.Active+got.Review+got.Blocked <= got.Lead {
		t.Errorf("fixture does not overlap blocked with active/review: %+v", got)
	}

	t.Run("a_run_open_at_the_end_of_history", func(t *testing.T) {
		open := e.create(scopeA, at(mins(300)), "still blocked")
		open = e.toActive(open, at(mins(310)), at(mins(320)))
		open = e.block(open, at(mins(330)), "waiting")
		open = e.block(open, at(mins(335)), "still waiting") // replacement: no new run
		runs := e.blockedRuns(open)
		if len(runs) != 1 || runs[0].ended || runs[0].startedVersion != 4 {
			t.Fatalf("runs = %+v", runs)
		}
		expect{
			lead: mins(50), active: mins(30), blocked: mins(20), blockedIntervals: 1, openEnd: true,
			state: "active", observed: 5,
		}.check(t, e.summarize(open, window(at(mins(290)), at(mins(350)))))
	})

	t.Run("blocking_before_the_item_is_started", func(t *testing.T) {
		// A reason may be recorded in any unfinished state; blocked time counts
		// wherever the item is unfinished, but not active/review time.
		it := e.create(scopeA, at(mins(400)), "blocked in backlog")
		it = e.block(it, at(mins(410)), "needs a decision")
		it = e.unblock(it, at(mins(430)))
		expect{
			lead: mins(60), blocked: mins(20), blockedIntervals: 1, openEnd: true, state: "backlog", observed: 3,
		}.check(t, e.summarize(it, window(at(mins(400)), at(mins(460)))))
	})
}

type episode struct {
	no             int64
	kind           string
	started        time.Time
	startedVersion int64
	ended          *time.Time
	endedVersion   *int64
	outcome        *string
}

func (e *env) episodes(it work.Item) []episode {
	e.t.Helper()
	var out []episode
	err := e.query(string(it.Scope.TenantID), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT episode_no, kind, started_at, started_version, ended_at, ended_version, outcome
			FROM work_flow_episodes WHERE tenant_id = $1 AND project_id = $2 AND work_item_id = $3 ORDER BY episode_no`,
			string(it.Scope.TenantID), string(it.Scope.ProjectID), string(it.ID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ep episode
			if err := rows.Scan(&ep.no, &ep.kind, &ep.started, &ep.startedVersion, &ep.ended, &ep.endedVersion, &ep.outcome); err != nil {
				return err
			}
			ep.started = ep.started.UTC()
			out = append(out, ep)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// Test_reopen_history: several completion/reopen cycles keep every earlier
// episode, exclude the time spent done, tell review rework from reopening, and
// close a reopened episode by completion or cancellation. WM-12, WM-A08.
func Test_reopen_history(t *testing.T) {
	e := newEnv(t)
	it := e.create(scopeA, at(0), "reopened twice")    // v1
	it = e.toActive(it, at(mins(5)), at(mins(10)))     // v2, v3
	it = e.move(it, at(mins(15)), work.StateReview)    // v4
	it = e.move(it, at(mins(18)), work.StateActive)    // v5 review rework: not a reopen
	it = e.move(it, at(mins(20)), work.StateReview)    // v6
	it = e.move(it, at(mins(30)), work.StateDone)      // v7 episode 1 completes
	it = e.move(it, at(mins(40)), work.StateActive)    // v8 reopen 1
	it = e.move(it, at(mins(50)), work.StateReview)    // v9
	it = e.move(it, at(mins(60)), work.StateDone)      // v10 episode 2 completes
	it = e.move(it, at(mins(70)), work.StateActive)    // v11 reopen 2
	it = e.move(it, at(mins(80)), work.StateCancelled) // v12 episode 3 is cancelled, not completed

	eps := e.episodes(it)
	str := func(s string) *string { return &s }
	i64 := func(n int64) *int64 { return &n }
	tm := func(m int) *time.Time { v := at(mins(m)); return &v }
	want := []episode{
		{no: 1, kind: "initial", started: at(0), startedVersion: 1, ended: tm(30), endedVersion: i64(7), outcome: str("done")},
		{no: 2, kind: "reopened", started: at(mins(40)), startedVersion: 8, ended: tm(60), endedVersion: i64(10), outcome: str("done")},
		{no: 3, kind: "reopened", started: at(mins(70)), startedVersion: 11, ended: tm(80), endedVersion: i64(12), outcome: str("cancelled")},
	}
	if len(eps) != len(want) {
		t.Fatalf("episodes = %+v", eps)
	}
	for i := range want {
		g, w := eps[i], want[i]
		if g.no != w.no || g.kind != w.kind || !g.started.Equal(w.started) || g.startedVersion != w.startedVersion ||
			g.ended == nil || !g.ended.Equal(*w.ended) || *g.endedVersion != *w.endedVersion || *g.outcome != *w.outcome {
			t.Errorf("episode %d = %+v, want %+v", i+1, g, w)
		}
	}

	t.Run("whole_history", func(t *testing.T) {
		// Lead excludes the 10 minutes done between episodes; the first rework
		// at v5 is not a reopen.
		expect{
			lead: mins(30 + 20 + 10), active: mins(5 + 2 + 10 + 10), review: mins(3 + 10 + 10),
			reopened: mins(20 + 10), completedLead: mins(30 + 20), reopens: 2, completed: 2, cancelled: 1,
			state: "cancelled", observed: 12,
		}.check(t, e.summarize(it, window(at(0), at(mins(100)))))
	})

	t.Run("window_inside_the_second_cycle", func(t *testing.T) {
		// The reopen at 40 and 70 are inside; the cancellation at 80 is not, so
		// the third episode is open at the window end rather than cancelled.
		expect{
			lead: mins(20 + 5), active: mins(10 + 5), review: mins(10), reopened: mins(20 + 5),
			completedLead: mins(20), reopens: 2, completed: 1, inFlight: false, openEnd: true,
			state: "active", observed: 11,
		}.check(t, e.summarize(it, window(at(mins(35)), at(mins(75)))))
	})

	t.Run("window_opening_while_reopened", func(t *testing.T) {
		// Opens at 45, after the reopen at 40: the reopen event is not counted,
		// the reopened interval still is, from the window start.
		expect{
			lead: mins(15), active: mins(5), review: mins(10), reopened: mins(15), completedLead: mins(20),
			completed: 1, inFlight: true, state: "done", observed: 10,
		}.check(t, e.summarize(it, window(at(mins(45)), at(mins(65)))))
	})

	t.Run("completion_resets_the_episode", func(t *testing.T) {
		// A completed item that is never reopened has exactly one episode.
		plain := e.create(scopeA, at(mins(200)), "plain")
		plain = e.toActive(plain, at(mins(201)), at(mins(202)))
		plain = e.move(plain, at(mins(203)), work.StateReview)
		plain = e.move(plain, at(mins(204)), work.StateDone)
		if eps := e.episodes(plain); len(eps) != 1 || eps[0].kind != "initial" || eps[0].outcome == nil || *eps[0].outcome != "done" {
			t.Fatalf("episodes = %+v", eps)
		}
		expect{
			lead: mins(4), active: mins(1), review: mins(1), completedLead: mins(4), completed: 1, state: "done", observed: 5,
		}.check(t, e.summarize(plain, window(at(mins(190)), at(mins(300)))))
	})
}
