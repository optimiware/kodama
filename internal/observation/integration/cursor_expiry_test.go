package integration_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/observation"
	ot "github.com/optimiware/kodama/internal/observation/observationtest"
	"github.com/optimiware/kodama/internal/storage"
)

// shortRetention makes pruning reachable in a test: every retention is one
// millisecond, which still satisfies the store's relationships (inbox and
// observation retention cover the broker horizon and replay window).
func shortRetention(o *eventspg.Options) {
	o.Retention = events.RetentionPolicy{
		PublishedOutbox: time.Millisecond, Observation: time.Millisecond, Inbox: time.Millisecond,
		ResolvedQuarantine: time.Millisecond, CompletedReplay: time.Millisecond,
		BrokerRedelivery: time.Millisecond, ReplayWindow: time.Millisecond, MaxClockSkew: time.Minute, BatchSize: 100,
	}
}

// recordedAt reads when the observation was recorded (the retention clock).
func recordedAt(t testing.TB, f *fixture, seq int64) time.Time {
	t.Helper()
	var at time.Time
	err := storage.WithTenant(context.Background(), f.Owner, ot.TenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT recorded_at FROM event_observations WHERE tenant_id = $1 AND scope_key = $2 AND seq = $3`,
			string(ot.TenantA), string(ot.ProjectA), seq).Scan(&at)
	})
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// pruneCutoff is the pruning time that makes the observation with this sequence
// (and every earlier one) leave retention while anything recorded 30ms or more
// later stays. It is derived from the database's own recorded_at stamps: the
// retention clock is the database clock, which need not agree with this
// process's clock (a containerized or virtualized database drifts by hundreds
// of milliseconds), so a cutoff taken from time.Now() may prune nothing.
func pruneCutoff(t testing.TB, f *fixture, seq int64) time.Time {
	t.Helper()
	return recordedAt(t, f, seq).Add(30 * time.Millisecond)
}

// awaitHostClock waits until this process's clock has passed the instant. A
// prune records the durable deduplication boundary at its cutoff, and Append
// refuses events that occurred (host time) before it, so a test that appends
// after pruning at a database-clock cutoff must first wait that cutoff out.
func awaitHostClock(until time.Time) {
	for time.Now().Before(until) {
		time.Sleep(5 * time.Millisecond)
	}
}

// hookLog lets a test act at an exact point of a stream's durable reads.
type hookLog struct {
	observation.Log
	before func(n int)
	n      int
}

func (h *hookLog) Read(ctx context.Context, s observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error) {
	h.n++
	if h.before != nil {
		h.before(h.n)
	}
	return h.Log.Read(ctx, s, after, limit)
}

// Test_cursor_expiry: actual event-store pruning makes an old cursor return 410
// cursor_expired; the floor boundary remains valid; snapshot resync supplies a
// usable cursor; midstream retention loss closes without silent skipping.
func Test_cursor_expiry(t *testing.T) {
	f := newFixture(t, ot.Options{Events: shortRetention})
	token := string(f.viewer)
	svc := f.Service()
	srv := f.NewServer(svc, f.Authority(), ot.HandlerOptions())
	url := ot.ProjectURL(srv.URL, ot.TenantA, ot.ProjectA)
	ctx := context.Background()

	if err := createWork(f.Env, ot.TenantA, ot.ProjectA, "wi_exp", "t024_exp_c"); err != nil {
		t.Fatal(err)
	}
	// Six events, collecting each one's cursor from a live stream.
	live := ot.Connect(t, url, token, nil)
	first := live.NextN(t, 1, 3*time.Second)
	if first[0].Event != "snapshot" {
		t.Fatalf("first frame = %+v", first[0])
	}
	// The snapshot was taken after the created event (sequence 1).
	cursors := map[int]string{1: first[0].ID} // sequence -> cursor
	commit := func(from, to int) {
		for i := from; i <= to; i++ {
			if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_exp", fmt.Sprintf("t024_exp_%d", i), false); err != nil {
				t.Fatal(err)
			}
		}
	}
	commit(2, 3)
	// Wait out the retention clock between the two batches so pruning can
	// separate them exactly.
	time.Sleep(60 * time.Millisecond)
	commit(4, 6)
	frames := live.NextN(t, 5, 5*time.Second) // sequences 2..6, in order
	for i, fr := range frames {
		cursors[i+2] = fr.ID
	}
	live.Close()

	prune := func(upTo int64) {
		t.Helper()
		at := pruneCutoff(t, f, upTo)
		res, err := f.Events.Prune(ctx, ot.TenantA, at)
		if err != nil {
			t.Fatal(err)
		}
		if res.Observations == 0 {
			t.Fatal("pruning removed nothing")
		}
		awaitHostClock(at)
	}
	prune(3) // the first three observations leave retention: floor = 3
	wm, err := f.Obs.Watermark(ctx, scopeOf(t, ot.TenantA, ot.ProjectA, ""))
	if err != nil || wm.Floor != 3 || wm.HighWater != 6 {
		t.Fatalf("watermark = %+v, %v", wm, err)
	}

	t.Run("an old cursor returns 410 cursor_expired", func(t *testing.T) {
		for _, seq := range []int{1, 2} {
			s := ot.Connect(t, url, token, map[string]string{"Last-Event-ID": cursors[seq]})
			if s.Resp.StatusCode != http.StatusGone || ot.ErrorCode(s.Body) != "cursor_expired" {
				t.Fatalf("cursor %d: %d %s", seq, s.Resp.StatusCode, s.Body)
			}
			if strings.Contains(string(s.Body), "floor") {
				t.Errorf("the response discloses retention internals: %s", s.Body)
			}
		}
	})

	t.Run("the floor boundary stays valid and the head is reachable", func(t *testing.T) {
		s := ot.Connect(t, url, token, map[string]string{"Last-Event-ID": cursors[3]})
		got := s.NextN(t, 3, 3*time.Second)
		for i, fr := range got {
			if fr.ID != cursors[4+i] {
				t.Fatalf("frame %d id %q, want the cursor of sequence %d", i, fr.ID, 4+i)
			}
		}
		s2 := ot.Connect(t, url, token, map[string]string{"Last-Event-ID": cursors[6]})
		if fr, ok := s2.Next(200 * time.Millisecond); ok {
			t.Fatalf("a cursor at the head must wait for new events, got %+v", fr)
		}
		if s2.Resp.StatusCode != 200 {
			t.Fatalf("status %d", s2.Resp.StatusCode)
		}
	})

	t.Run("snapshot resync supplies a usable cursor", func(t *testing.T) {
		code, _, body := ot.GetJSON(t, url, token, nil)
		if code != 200 {
			t.Fatalf("snapshot: %d %s", code, body)
		}
		cur := jsonField(t, body, "cursor")
		if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_exp", "t024_exp_7", false); err != nil {
			t.Fatal(err)
		}
		s := ot.Connect(t, url, token, map[string]string{"Last-Event-ID": cur})
		got := s.NextN(t, 1, 3*time.Second)
		if got[0].Event != "work_item.transitioned" || !strings.Contains(got[0].Data, "t024_exp_7") {
			t.Fatalf("resumed from the snapshot cursor with %+v", got[0])
		}
	})

	t.Run("retention loss midstream closes the stream without skipping", func(t *testing.T) {
		// Fresh events, a stream whose pages are small, and pruning between pages.
		if err := createWork(f.Env, ot.TenantA, ot.ProjectA, "wi_exp2", "t024_mid_c"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 11; i++ {
			if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_exp2", fmt.Sprintf("t024_mid_%d", i), false); err != nil {
				t.Fatal(err)
			}
		}
		head, _ := f.Obs.Watermark(ctx, scopeOf(t, ot.TenantA, ot.ProjectA, ""))
		resume := wm.HighWater + 1 // a position inside the retained range
		pruneAt := pruneCutoff(t, f, head.HighWater)
		hl := &hookLog{Log: f.Obs}
		hl.before = func(n int) {
			if n == 2 { // between the first and the second page
				if _, err := f.Events.Prune(ctx, ot.TenantA, pruneAt); err != nil {
					t.Errorf("prune: %v", err)
				}
			}
		}
		svc2 := f.Service(func(c *observation.Config) { c.Log = hl })
		st, err := svc2.Open(ctx, observation.Request{Actor: observation.Actor{ID: token}, Scope: scopeOf(t, ot.TenantA, ot.ProjectA, ""), Resume: &resume})
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		_, done := run(t, st, c)
		err = waitEnd(t, done, 5*time.Second)
		if !errors.Is(err, observation.ErrCursorExpired) {
			t.Fatalf("stream ended with %v, want ErrCursorExpired", err)
		}
		if !errors.Is(c.resync, observation.ErrCursorExpired) {
			t.Fatalf("no resync frame was offered: %v", c.resync)
		}
		// Only the first page (the PageSize of the test options) was delivered,
		// contiguously: nothing after the pruned gap leaked through.
		want := int(resume) + 1
		for _, d := range c.events {
			if d.Position != int64(want) {
				t.Fatalf("delivered position %d, want %d", d.Position, want)
			}
			want++
		}
		if len(c.events) != observation.Options(ot.TestOptions()).PageSize || int64(want-1) >= head.HighWater {
			t.Fatalf("delivered %d events up to %d (head %d)", len(c.events), want-1, head.HighWater)
		}
		// The prune moved the durable deduplication boundary to its (database
		// clock) cutoff; wait it out before producing more events.
		awaitHostClock(pruneAt)
		// The same loss over HTTP ends with the resync_required control frame.
		hl2 := &hookLog{Log: f.Obs}
		svc3 := f.Service(func(c *observation.Config) { c.Log = hl2 })
		srv3 := f.NewServer(svc3, f.Authority(), ot.HandlerOptions())
		if err := createWork(f.Env, ot.TenantA, ot.ProjectA, "wi_exp3", "t024_http_c"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 9; i++ {
			if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_exp3", fmt.Sprintf("t024_http_%d", i), false); err != nil {
				t.Fatal(err)
			}
		}
		code, _, body := ot.GetJSON(t, ot.ProjectURL(srv3.URL, ot.TenantA, ot.ProjectA), token, nil)
		if code != 200 {
			t.Fatalf("snapshot: %d %s", code, body)
		}
		// Resume from the snapshot cursor of an earlier, now-short head: write
		// more events so the stream has several pages to cross.
		cur := jsonField(t, body, "cursor")
		for i := 0; i < 9; i++ {
			if _, err := bump(f.Env, ot.TenantA, ot.ProjectA, "wi_exp3", fmt.Sprintf("t024_http2_%d", i), false); err != nil {
				t.Fatal(err)
			}
		}
		head3, err := f.Obs.Watermark(ctx, scopeOf(t, ot.TenantA, ot.ProjectA, ""))
		if err != nil {
			t.Fatal(err)
		}
		pruneAt3 := pruneCutoff(t, f, head3.HighWater)
		hl2.before = func(n int) {
			if n == 2 { // between the first and the second page
				if _, err := f.Events.Prune(ctx, ot.TenantA, pruneAt3); err != nil {
					t.Errorf("prune: %v", err)
				}
			}
		}
		s := ot.Connect(t, ot.ProjectURL(srv3.URL, ot.TenantA, ot.ProjectA), token, map[string]string{"Last-Event-ID": cur})
		var last ot.Frame
		for {
			fr, ok := s.Next(3 * time.Second)
			if !ok {
				break
			}
			last = fr
		}
		if last.Event != "resync_required" || last.ID != "" {
			t.Fatalf("last frame = %+v, want resync_required without an id", last)
		}
	})
}
