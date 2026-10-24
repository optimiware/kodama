package integration_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
	"github.com/optimiware/kodama/internal/storage"
)

// dbVersions reads the committed work-item versions of a project.
func dbVersions(t testing.TB, f *fixture, tenant domain.TenantID, project domain.ProjectID) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := storage.WithTenant(context.Background(), f.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, version FROM work_items WHERE tenant_id = $1 AND project_id = $2`, string(tenant), string(project))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var v int64
			if err := rows.Scan(&id, &v); err != nil {
				return err
			}
			out[id] = v
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dbSequences lists committed observation sequences of a type above after.
func dbSequences(t testing.TB, f *fixture, tenant domain.TenantID, project domain.ProjectID, eventType string, after int64) []int64 {
	t.Helper()
	var out []int64
	err := storage.WithTenant(context.Background(), f.Owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT seq FROM event_observations WHERE tenant_id = $1 AND scope_key = $2 AND event_type = $3 AND seq > $4 ORDER BY seq`,
			string(tenant), string(project), eventType, after)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s int64
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// replay applies a session's snapshot and events to a version map and checks
// the continuity that proves nothing was missed or duplicated.
func replay(t testing.TB, name string, c *collector) map[string]int64 {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := map[string]int64{}
	if c.snapshot == nil {
		t.Errorf("%s: no snapshot", name)
		return state
	}
	for _, r := range c.snapshot.Resources {
		if r.Type == observation.FamilyWork {
			state[r.ID] = r.Version
		}
	}
	want := c.snapshot.Position
	for _, d := range c.events {
		want++
		if d.Position != want {
			t.Errorf("%s: event %s at position %d, want %d (a committed event was skipped or repeated)", name, d.Envelope.ID, d.Position, want)
			return state
		}
		id, v := versionOf(d)
		prev, known := state[id]
		switch d.Envelope.Type {
		case eventsv1.TypeWorkItemCreated:
			if known {
				t.Errorf("%s: %s created again after the snapshot already contained it (snapshot not atomic with its position)", name, id)
			}
		default:
			if !known {
				t.Errorf("%s: %s changed but neither the snapshot nor a created event introduced it", name, id)
			} else if v != prev+1 {
				t.Errorf("%s: %s jumped from version %d to %d (lost or duplicated event)", name, id, prev, v)
			}
		}
		state[id] = v
	}
	return state
}

// Test_snapshot_stream_race: concurrent atomic state/event commits around
// snapshot acquisition, transaction completion, backlog drain and entry into
// live polling produce neither a missed committed change nor an inconsistent
// snapshot cursor; includes rollback and filtered sequence gaps.
func Test_snapshot_stream_race(t *testing.T) {
	f := newFixture(t, ot.Options{})
	svc := f.Service()
	actor := observation.Actor{ID: string(f.viewer)}

	t.Run("a commit overlapping snapshot acquisition lands on exactly one side", func(t *testing.T) {
		if err := createWork(f.Env, ot.TenantA, ot.ProjectA, "wi_hold", "t024_hold_c"); err != nil {
			t.Fatal(err)
		}
		held, release := make(chan struct{}), make(chan struct{})
		commitErr := make(chan error, 1)
		go func() {
			commitErr <- storage.WithTenant(context.Background(), f.Runtime, ot.TenantA, func(ctx context.Context, tx pgx.Tx) error {
				var v int64
				if err := tx.QueryRow(ctx, `UPDATE work_items SET state = 'ready', version = version + 1 WHERE tenant_id = kodama_tenant_id() AND id = 'wi_hold' RETURNING version`).Scan(&v); err != nil {
					return err
				}
				if _, err := f.Events.Append(ctx, tx, ot.WorkEvent(ot.TenantA, ot.ProjectA, "t024_hold_e", "wi_hold", v)); err != nil {
					return err
				}
				close(held)
				<-release // the transaction stays open across the snapshot
				return nil
			})
		}()
		<-held
		scope := scopeOf(t, ot.TenantA, ot.ProjectA, "")
		start := time.Now()
		st, err := svc.Open(context.Background(), observation.Request{Actor: actor, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("snapshot acquisition must not wait for an open writer")
		}
		snap := st.Initial()
		var heldVersion int64
		for _, r := range snap.Resources {
			if r.ID == "wi_hold" {
				heldVersion = r.Version
			}
		}
		if heldVersion != 1 {
			t.Fatalf("the snapshot shows version %d of an item whose change was not committed yet", heldVersion)
		}
		// The writer commits after the snapshot read but before the backlog is drained.
		close(release)
		if err := <-commitErr; err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		cancel, done := run(t, st, c)
		waitFor(t, 5*time.Second, "the held commit to be delivered", func() bool { return c.eventCount() >= 1 })
		cancel()
		_ = waitEnd(t, done, 3*time.Second)
		ids := c.eventIDs()
		if len(ids) != 1 || ids[0] != "t024_hold_e" || c.events[0].Position != snap.Position+1 {
			t.Fatalf("delivered %v at %d, snapshot position %d: the commit must be delivered exactly once, right after the snapshot", ids, c.events[0].Position, snap.Position)
		}
		if state := replay(t, "held", c); state["wi_hold"] != 2 {
			t.Fatalf("replayed state %v", state)
		}
	})

	t.Run("rolled-back commits leave no sequence gap and no trace", func(t *testing.T) {
		if err := createWork(f.Env, ot.TenantA, ot.ProjectA, "wi_rb", "t024_rb_c"); err != nil {
			t.Fatal(err)
		}
		scope := scopeOf(t, ot.TenantA, ot.ProjectA, "")
		before, err := f.Obs.Watermark(context.Background(), scope)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_rb", fmt.Sprintf("t024_rb_x%d", i), true); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_rb", "t024_rb_ok", false); err != nil {
			t.Fatal(err)
		}
		after, _ := f.Obs.Watermark(context.Background(), scope)
		if after.HighWater != before.HighWater+1 {
			t.Fatalf("high-water %d -> %d: rolled-back appends must not consume sequences", before.HighWater, after.HighWater)
		}
		st, err := svc.Open(context.Background(), observation.Request{Actor: actor, Scope: scope, Resume: &before.HighWater})
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		cancel, done := run(t, st, c)
		waitFor(t, 5*time.Second, "the committed event", func() bool { return c.eventCount() == 1 })
		cancel()
		_ = waitEnd(t, done, 3*time.Second)
		if ids := c.eventIDs(); len(ids) != 1 || ids[0] != "t024_rb_ok" {
			t.Fatalf("delivered %v", ids)
		}
	})

	t.Run("concurrent writers and sessions", func(t *testing.T) {
		const (
			seedItems    = 4
			writers      = 4
			opsPerWriter = 30
			sessions     = 8
		)
		var mu sync.Mutex
		var items []string
		for i := 0; i < seedItems; i++ {
			id := fmt.Sprintf("wi_race%d", i)
			if err := createWork(f.Env, ot.TenantA, ot.ProjectA, id, "t024_race_c"+id); err != nil {
				t.Fatal(err)
			}
			items = append(items, id)
		}
		var seq atomic.Int64
		var writersDone sync.WaitGroup
		var rolledBack atomic.Int64
		writeErr := make(chan error, writers)
		for w := 0; w < writers; w++ {
			writersDone.Add(1)
			go func() {
				defer writersDone.Done()
				rng := rand.New(rand.NewSource(int64(w) + 1))
				for op := 0; op < opsPerWriter; op++ {
					time.Sleep(time.Duration(rng.Intn(6)) * time.Millisecond)
					n := seq.Add(1)
					switch {
					case w == 0 && op%6 == 5: // introduce a new work item while sessions run
						id := fmt.Sprintf("wi_new%d", n)
						if err := createWork(f.Env, ot.TenantA, ot.ProjectA, id, fmt.Sprintf("t024_race_new%d", n)); err != nil {
							writeErr <- err
							return
						}
						mu.Lock()
						items = append(items, id)
						mu.Unlock()
					default:
						mu.Lock()
						item := items[rng.Intn(len(items))]
						mu.Unlock()
						rollback := rng.Intn(5) == 0
						if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, item, fmt.Sprintf("t024_race_e%d", n), rollback); err != nil {
							writeErr <- err
							return
						}
						if rollback {
							rolledBack.Add(1)
						}
					}
				}
			}()
		}

		scope := scopeOf(t, ot.TenantA, ot.ProjectA, "")
		type session struct {
			c      *collector
			cancel func()
			done   <-chan error
		}
		var live []session
		var started sync.WaitGroup
		var liveMu sync.Mutex
		for s := 0; s < sessions; s++ {
			started.Add(1)
			go func() {
				defer started.Done()
				time.Sleep(time.Duration(s*25) * time.Millisecond) // staggered across the write burst
				st, err := svc.Open(context.Background(), observation.Request{Actor: actor, Scope: scope})
				if err != nil {
					t.Errorf("open session %d: %v", s, err)
					return
				}
				c := &collector{}
				cancel, done := run(t, st, c)
				liveMu.Lock()
				live = append(live, session{c, cancel, done})
				liveMu.Unlock()
			}()
		}
		started.Wait()
		writersDone.Wait()
		close(writeErr)
		for err := range writeErr {
			t.Fatal(err)
		}
		wm, err := f.Obs.Watermark(context.Background(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if rolledBack.Load() == 0 {
			t.Fatal("the scenario must include rolled-back commits")
		}
		final := dbVersions(t, f, ot.TenantA, ot.ProjectA)
		for i, s := range live {
			var ended error
			waitFor(t, 20*time.Second, fmt.Sprintf("session %d to reach %d", i, wm.HighWater), func() bool {
				select {
				case ended = <-s.done:
					t.Fatalf("session %d ended on its own at position %d of %d: %v", i, s.c.lastPosition(), wm.HighWater, ended)
				default:
				}
				return s.c.lastPosition() >= wm.HighWater
			})
			s.cancel()
			if err := waitEnd(t, s.done, 3*time.Second); !errors.Is(err, context.Canceled) {
				t.Errorf("session %d ended with %v", i, err)
			}
			got := replay(t, fmt.Sprintf("session %d", i), s.c)
			if len(got) != len(final) {
				t.Errorf("session %d: %d items, database has %d", i, len(got), len(final))
			}
			for id, v := range final {
				if got[id] != v {
					t.Errorf("session %d: %s at version %d, database has %d", i, id, got[id], v)
				}
			}
		}
		if len(live) != sessions {
			t.Fatalf("%d of %d sessions opened", len(live), sessions)
		}
	})

	t.Run("filtered streams cross sequence gaps without skipping a match", func(t *testing.T) {
		scope := scopeOf(t, ot.TenantA, ot.ProjectA, "", eventsv1.TypeWorkItemCreated)
		st, err := svc.Open(context.Background(), observation.Request{Actor: actor, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		start := st.Initial().Position
		c := &collector{}
		cancel, done := run(t, st, c)
		var wg sync.WaitGroup
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 12; i++ {
					id := fmt.Sprintf("wi_f%d_%d", w, i)
					if i%3 == 0 {
						_ = createWork(f.Env, ot.TenantA, ot.ProjectA, id, "t024_f_c"+id)
						continue
					}
					_, _ = bump(f.Env, ot.TenantA, ot.ProjectA, "wi_race0", fmt.Sprintf("t024_f_e%d_%d", w, i), i%4 == 0)
				}
			}()
		}
		wg.Wait()
		wm, _ := f.Obs.Watermark(context.Background(), scope)
		waitFor(t, 10*time.Second, "the filtered stream to reach the head", func() bool { return c.lastPosition() >= wm.HighWater })
		cancel()
		_ = waitEnd(t, done, 3*time.Second)
		want := dbSequences(t, f, ot.TenantA, ot.ProjectA, eventsv1.TypeWorkItemCreated, start)
		c.mu.Lock()
		defer c.mu.Unlock()
		var got []int64
		for _, d := range c.events {
			got = append(got, d.Position)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("matching events delivered %v, committed %v", got, want)
		}
		for i := 1; i < len(c.positions); i++ {
			if c.positions[i] <= c.positions[i-1] {
				t.Fatalf("continuation positions went backwards: %v", c.positions)
			}
		}
	})
}
