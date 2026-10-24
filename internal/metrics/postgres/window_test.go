package postgres_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/metrics"
	"github.com/optimiware/kodama/internal/work"
)

// Test_incomplete_window: intervals are reconstructed from the whole history
// before being clipped to the half-open window, so a window that cuts an interval
// reports the clipped time with the correct state, flags the censoring, never
// presents the cap as a completion and never reveals what happened after it.
// WM-12, WM-A08.
func Test_incomplete_window(t *testing.T) {
	e := newEnv(t)
	it := e.create(scopeA, at(0), "in flight")       // v1
	it = e.toActive(it, at(mins(10)), at(mins(20)))  // v2 ready, v3 active
	it = e.block(it, at(mins(30)), "waiting")        // v4
	it = e.move(it, at(mins(50)), work.StateReview)  // v5
	done := e.move(it, at(mins(90)), work.StateDone) // v6, after every window below
	_ = done

	t.Run("window_opens_and_closes_inside_an_interval", func(t *testing.T) {
		// The predecessor row (active since 20, blocked since 30) is reconstructed
		// even though no history row lies inside [25,40).
		expect{
			lead: mins(15), active: mins(15), blocked: mins(10), blockedIntervals: 1,
			inFlight: true, openEnd: true, state: "active", observed: 4,
		}.check(t, e.summarize(it, window(at(mins(25)), at(mins(40)))))
	})

	t.Run("open_item_is_capped_at_the_window_end", func(t *testing.T) {
		// The review at 50 and completion at 90 are later than the window end 60 for
		// the completion only: the item is open, not done.
		expect{
			lead: mins(35), active: mins(25), review: mins(10), blocked: mins(30), blockedIntervals: 1,
			inFlight: true, openEnd: true, state: "review", observed: 5,
		}.check(t, e.summarize(it, window(at(mins(25)), at(mins(60)))))
	})

	t.Run("a_later_completion_is_not_revealed", func(t *testing.T) {
		got := e.summarize(it, window(at(mins(0)), at(mins(89))))
		if got.CompletedEpisodes != 0 || got.CompletedLead != 0 || got.StateAtEnd != "review" || !got.OpenAtEnd || got.ObservedVersion != 5 {
			t.Fatalf("a completion after the window leaked into it: %+v", got)
		}
		whole := e.summarize(it, window(at(mins(0)), at(mins(91))))
		if whole.CompletedEpisodes != 1 || whole.OpenAtEnd || whole.StateAtEnd != "done" || whole.CompletedLead != mins(90) {
			t.Fatalf("whole = %+v", whole)
		}
	})

	t.Run("half_open_bounds", func(t *testing.T) {
		// Active at exactly 20: excluded from [0,20), included from [20,40).
		expect{
			lead: mins(20), openEnd: true, state: "ready", observed: 2,
		}.check(t, e.summarize(it, window(at(0), at(mins(20)))))
		// A window opening exactly on the transition does not own the earlier
		// ready time, but the episode began before it.
		expect{
			lead: mins(20), active: mins(20), blocked: mins(10), blockedIntervals: 1, inFlight: true, openEnd: true, state: "active", observed: 4,
		}.check(t, e.summarize(it, window(at(mins(20)), at(mins(40)))))
		// A completion exactly at the window end happens outside it.
		got := e.summarize(it, window(at(mins(60)), at(mins(90))))
		if got.CompletedEpisodes != 0 || !got.OpenAtEnd || got.StateAtEnd != "review" {
			t.Fatalf("completion at the end instant: %+v", got)
		}
		// ... and one exactly at the window start belongs to it.
		got = e.summarize(it, window(at(mins(90)), at(mins(100))))
		if got.CompletedEpisodes != 1 || got.OpenAtEnd || got.StateAtEnd != "done" || !got.InFlightAtStart || got.Lead != 0 {
			t.Fatalf("completion at the start instant: %+v", got)
		}
	})

	t.Run("not_started_items_have_no_flow", func(t *testing.T) {
		late := e.create(scopeA, at(mins(500)), "created later")
		for name, w := range map[string]metrics.Window{
			"window before creation":    window(at(mins(400)), at(mins(450))),
			"window ending at creation": window(at(mins(400)), at(mins(500))),
		} {
			got := e.summarize(late, w)
			want := metrics.WorkSummary{Scope: late.Scope, ItemID: late.ID, Window: w, Status: metrics.StatusNotStarted}
			if got != want {
				t.Errorf("%s: %+v", name, got)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
		// One microsecond later the item exists.
		got := e.summarize(late, window(at(mins(400)), at(mins(500)+time.Microsecond)))
		if got.Status != metrics.StatusOK || got.Lead != time.Microsecond || got.StateAtEnd != "backlog" || !got.OpenAtEnd {
			t.Errorf("created at the last microsecond: %+v", got)
		}
	})

	t.Run("equal_timestamps_are_zero_length", func(t *testing.T) {
		z := e.create(scopeA, at(mins(600)), "all at once")
		z = e.toActive(z, at(mins(600)), at(mins(600)))
		z = e.move(z, at(mins(600)), work.StateReview)
		z = e.move(z, at(mins(600)), work.StateDone)
		expect{
			completed: 1, state: "done", observed: 5,
		}.check(t, e.summarize(z, window(at(mins(590)), at(mins(610)))))
		z = e.move(z, at(mins(600)), work.StateActive) // reopen in the same microsecond
		expect{
			lead: mins(10), active: mins(10), reopened: mins(10), reopens: 1, completed: 1, openEnd: true, state: "active", observed: 6,
		}.check(t, e.summarize(z, window(at(mins(590)), at(mins(610)))))
	})

	t.Run("microsecond_precision", func(t *testing.T) {
		u := e.create(scopeA, at(mins(700)), "tiny")
		u = e.move(u, at(mins(700)+time.Microsecond), work.StateReady)
		u = e.move(u, at(mins(700)+3*time.Microsecond), work.StateActive)
		got := e.summarize(u, window(at(mins(700)), at(mins(700)+10*time.Microsecond)))
		if got.Lead != 10*time.Microsecond || got.Active != 7*time.Microsecond {
			t.Fatalf("lead %v active %v", got.Lead, got.Active)
		}
	})

	t.Run("a_window_in_the_middle_of_nothing", func(t *testing.T) {
		// Finished item, window entirely inside the done gap: no flow, no censoring.
		gap := e.create(scopeA, at(mins(800)), "finished")
		gap = e.toActive(gap, at(mins(801)), at(mins(802)))
		gap = e.move(gap, at(mins(803)), work.StateReview)
		gap = e.move(gap, at(mins(804)), work.StateDone)
		expect{state: "done", observed: 5}.check(t, e.summarize(gap, window(at(mins(810)), at(mins(820)))))
	})

}
