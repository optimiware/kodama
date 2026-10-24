package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// Test_summary_snapshot_concurrency: while work mutations commit concurrently,
// every summary is the exact projection of one committed prefix of the item's
// history (its observed version), never a mixture of two; rows of an
// uncommitted transaction are invisible. WM-12.
func Test_summary_snapshot_concurrency(t *testing.T) {
	e := newEnv(t)
	to := at(mins(2000))
	w := window(at(0), to)

	// History row i happens at times[i] and leaves the item in states[i].
	times := []time.Time{{}, at(0), at(mins(10)), at(mins(20))}
	states := []work.State{"", work.StateBacklog, work.StateReady, work.StateActive}
	it := e.create(scopeA, times[1], "concurrent")
	it = e.move(it, times[2], work.StateReady)
	it = e.move(it, times[3], work.StateActive)

	// model is the projection of the first n rows, open at the window end.
	model := func(n int) (active, review time.Duration) {
		for j := 1; j <= n; j++ {
			end := to
			if j < n {
				end = times[j+1]
			}
			switch states[j] {
			case work.StateActive:
				active += end.Sub(times[j])
			case work.StateReview:
				review += end.Sub(times[j])
			}
		}
		return
	}

	itemID := it.ID // readers must not touch the mutating copy
	type observation struct {
		version        domain.Version
		active, review time.Duration
		lead           time.Duration
	}
	var (
		done sync.WaitGroup
		stop atomic.Bool
		mu   sync.Mutex
		obs  []observation
	)
	for r := 0; r < 3; r++ {
		done.Add(1)
		go func() {
			defer done.Done()
			var last domain.Version
			for !stop.Load() {
				got, err := e.store.ReadSummaries(context.Background(), scopeA, []domain.WorkItemID{itemID}, w)
				if err != nil || len(got) != 1 {
					t.Errorf("read: %v %v", got, err)
					return
				}
				s := got[0]
				if s.ObservedVersion < last {
					t.Errorf("observed version went back from %d to %d", last, s.ObservedVersion)
					return
				}
				last = s.ObservedVersion
				mu.Lock()
				obs = append(obs, observation{s.ObservedVersion, s.Active, s.Review, s.Lead})
				mu.Unlock()
			}
		}()
	}

	// Each step is one atomic work/history/audit/outbox commit.
	const steps = 40
	for i := 0; i < steps; i++ {
		next := work.StateReview
		if it.State == work.StateReview {
			next = work.StateActive
		}
		when := at(mins(30 + 5*i))
		it = e.move(it, when, next)
		times, states = append(times, when), append(states, next)
	}
	stop.Store(true)
	done.Wait()

	if len(obs) == 0 {
		t.Fatal("no concurrent read ran")
	}
	prefixes := map[domain.Version]bool{}
	for _, o := range obs {
		prefixes[o.version] = true
		n := int(o.version)
		if n < 3 || n >= len(times) {
			t.Fatalf("observed version %d is outside the committed history (1..%d)", n, len(times)-1)
		}
		active, review := model(n)
		if o.active != active || o.review != review || o.lead != to.Sub(times[1]) {
			t.Fatalf("version %d: active %v review %v lead %v, want %v %v %v (a mixture of prefixes)", n, o.active, o.review, o.lead, active, review, to.Sub(times[1]))
		}
	}
	final := e.summarize(it, w)
	active, review := model(len(times) - 1)
	if int(final.ObservedVersion) != len(times)-1 || final.Active != active || final.Review != review {
		t.Fatalf("final summary %+v, want version %d active %v review %v", final, len(times)-1, active, review)
	}
	t.Logf("%d concurrent reads observed %d distinct committed prefixes", len(obs), len(prefixes))

	t.Run("uncommitted_history_is_invisible", func(t *testing.T) {
		before := e.summarize(it, w)
		tx, err := e.owner.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(context.Background(), `INSERT INTO work_item_history
			(tenant_id, project_id, id, work_item_id, version, content_revision, change_kind, actor_type, actor_id, occurred_at, from_state, to_state)
			VALUES ($1, $2, 'wih_t042_uncommitted', $3, $4, 1, 'transitioned', 'user', 'u', $5, $6, 'cancelled')`,
			tenantA, projectA, string(it.ID), int64(it.Version)+1, at(mins(1000)), string(it.State)); err != nil {
			t.Fatal(err)
		}
		if during := e.summarize(it, w); during != before {
			t.Fatalf("an uncommitted row changed the summary\n before %+v\n during %+v", before, during)
		}
		if err := tx.Rollback(context.Background()); err != nil {
			t.Fatal(err)
		}
		if after := e.summarize(it, w); after != before {
			t.Fatalf("a rolled-back row changed the summary: %+v", after)
		}
	})
}
