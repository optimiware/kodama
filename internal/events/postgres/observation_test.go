package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/storage"
)

func readPage(t testing.TB, f *fixture, tenant string, scope events.Scope, after int64, limit int, opts pgx.TxOptions) (seqs []int64, w struct{ High, Floor int64 }, err error) {
	t.Helper()
	err = storage.WithTenantOptions(context.Background(), f.env.Runtime, domain.TenantID(tenant), opts, func(ctx context.Context, tx pgx.Tx) error {
		obs, wm, err := f.store.ReadObservations(ctx, tx, scope, after, limit)
		for _, o := range obs {
			seqs = append(seqs, o.Sequence)
		}
		w.High, w.Floor = wm.HighWater, wm.Floor
		return err
	})
	return seqs, w, err
}

// TestObservationCommitOrder: delayed commits, rollbacks, concurrent appends,
// scoped reads, snapshot watermarks and retention expiry never let a committed
// cursor skip an observation.
func TestObservationCommitOrder(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	scopeA := events.Scope{TenantID: tenantA, ProjectID: projectA}

	t.Run("a delayed commit cannot be overtaken", func(t *testing.T) {
		tx1, err := f.env.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx1.Rollback(ctx)
		if _, err := tx1.Exec(ctx, `SELECT set_config('kodama.tenant_id', $1, true)`, tenantA); err != nil {
			t.Fatal(err)
		}
		first, err := f.store.Append(ctx, tx1, eventA("ord1", 1))
		if err != nil || first.Sequence != 1 {
			t.Fatalf("first append: %+v %v", first, err)
		}
		// A second writer must wait for the first to commit instead of taking a
		// later sequence that a reader could observe first.
		var secondSeq atomic.Int64
		done := make(chan error, 1)
		go func() {
			done <- f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				res, err := f.store.Append(ctx, tx, eventA("ord2", 1))
				secondSeq.Store(res.Sequence)
				return err
			})
		}()
		select {
		case err := <-done:
			t.Fatalf("the second append finished before the first committed: %v", err)
		case <-time.After(400 * time.Millisecond):
		}
		if seqs, w, err := readPage(t, f, tenantA, scopeA, 0, 10, pgx.TxOptions{}); err != nil || len(seqs) != 0 || w.High != 0 {
			t.Fatalf("an uncommitted event is visible: %v %+v %v", seqs, w, err)
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if secondSeq.Load() != 2 {
			t.Fatalf("second sequence = %d", secondSeq.Load())
		}
		if seqs, w, err := readPage(t, f, tenantA, scopeA, 0, 10, pgx.TxOptions{}); err != nil || fmt.Sprint(seqs) != "[1 2]" || w.High != 2 {
			t.Fatalf("after both commits: %v %+v %v", seqs, w, err)
		}
	})

	t.Run("concurrent appenders get gapless unique sequences and readers never skip", func(t *testing.T) {
		const n = 30
		stop := make(chan struct{})
		var readerErr atomic.Value
		var readerDone sync.WaitGroup
		readerDone.Add(1)
		go func() {
			defer readerDone.Done()
			cursor := int64(2)
			for {
				select {
				case <-stop:
					return
				default:
				}
				seqs, _, err := readPage(t, f, tenantA, scopeA, cursor, 5, pgx.TxOptions{})
				if err != nil {
					readerErr.Store(err)
					return
				}
				for _, s := range seqs {
					if s != cursor+1 {
						readerErr.Store(fmt.Errorf("cursor %d skipped to %d", cursor, s))
						return
					}
					cursor = s
				}
			}
		}()
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
					_, err := f.store.Append(ctx, tx, eventA(fmt.Sprintf("conc%02d", i), int64(i+1)))
					return err
				}); err != nil {
					t.Errorf("append: %v", err)
				}
			}()
		}
		wg.Wait()
		close(stop)
		readerDone.Wait()
		if err, _ := readerErr.Load().(error); err != nil {
			t.Fatal(err)
		}
		var all []int64
		for cursor := int64(0); ; {
			seqs, w, err := readPage(t, f, tenantA, scopeA, cursor, 7, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(seqs) == 0 {
				if w.High != int64(len(all)) {
					t.Fatalf("watermark %d vs %d events read", w.High, len(all))
				}
				break
			}
			all = append(all, seqs...)
			cursor = seqs[len(seqs)-1]
		}
		if len(all) != n+2 {
			t.Fatalf("read %d observations, want %d", len(all), n+2)
		}
		for i, s := range all {
			if s != int64(i+1) {
				t.Fatalf("sequence %d at position %d: not gapless", s, i)
			}
		}
	})

	t.Run("reads are scoped by tenant and project", func(t *testing.T) {
		f.append(eventB("ord_b", 1))
		f.append(eventstestMembership(tenantA, "ord_tw", 1))
		// Tenant B's transaction cannot read tenant A's scope.
		if seqs, w, err := readPage(t, f, tenantB, scopeA, 0, 100, pgx.TxOptions{}); err != nil || len(seqs) != 0 || w.High != 0 {
			t.Fatalf("cross-tenant read: %v %+v %v", seqs, w, err)
		}
		// The tenant-wide scope is separate from the project scope.
		if seqs, w, err := readPage(t, f, tenantA, events.Scope{TenantID: tenantA}, 0, 100, pgx.TxOptions{}); err != nil || fmt.Sprint(seqs) != "[1]" || w.High != 1 {
			t.Fatalf("tenant-wide scope: %v %+v %v", seqs, w, err)
		}
		if seqs, _, err := readPage(t, f, tenantB, events.Scope{TenantID: tenantB, ProjectID: projectB}, 0, 100, pgx.TxOptions{}); err != nil || fmt.Sprint(seqs) != "[1]" {
			t.Fatalf("tenant B scope: %v %v", seqs, err)
		}
		if _, _, err := readPage(t, f, tenantA, scopeA, -1, 10, pgx.TxOptions{}); err == nil {
			t.Fatal("negative cursor accepted")
		}
	})

	t.Run("a snapshot and its watermark agree", func(t *testing.T) {
		var before, after struct{ High, Floor int64 }
		var seqsBefore, seqsAfter []int64
		err := storage.WithTenantOptions(ctx, f.env.Runtime, tenantA, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(ctx context.Context, tx pgx.Tx) error {
			wm, err := f.store.Watermark(ctx, tx, scopeA)
			if err != nil {
				return err
			}
			before.High = wm.HighWater
			obs, _, err := f.store.ReadObservations(ctx, tx, scopeA, 0, 1000)
			if err != nil {
				return err
			}
			for _, o := range obs {
				seqsBefore = append(seqsBefore, o.Sequence)
			}
			// A concurrent writer commits while this snapshot is open.
			f.append(eventA("ord_snapshot", 99))
			wm2, err := f.store.Watermark(ctx, tx, scopeA)
			if err != nil {
				return err
			}
			after.High = wm2.HighWater
			obs, _, err = f.store.ReadObservations(ctx, tx, scopeA, 0, 1000)
			for _, o := range obs {
				seqsAfter = append(seqsAfter, o.Sequence)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if before.High != after.High || len(seqsBefore) != len(seqsAfter) || int64(len(seqsBefore)) != before.High {
			t.Fatalf("snapshot moved: watermark %d -> %d, rows %d -> %d", before.High, after.High, len(seqsBefore), len(seqsAfter))
		}
		if _, w, _ := readPage(t, f, tenantA, scopeA, 0, 1, pgx.TxOptions{}); w.High != before.High+1 {
			t.Fatalf("the new event is not visible to a later snapshot: %d", w.High)
		}
	})

	t.Run("retention expiry is explicit and never silent", func(t *testing.T) {
		_, w, _ := readPage(t, f, tenantA, scopeA, 0, 1, pgx.TxOptions{})
		high := w.High
		// Everything recorded so far is now older than the observation retention.
		// Time passes for the whole fixture (the append below checks producer
		// timestamps against the same clock).
		f.clock.Advance(60 * time.Hour)
		res, err := f.store.Prune(ctx, tenantA, f.clock.Now())
		if err != nil || res.Observations == 0 {
			t.Fatalf("prune: %+v %v", res, err)
		}
		_, w, err = readPage(t, f, tenantA, scopeA, high, 10, pgx.TxOptions{})
		if err != nil || w.Floor != high || w.High != high {
			t.Fatalf("after pruning everything: %+v %v", w, err)
		}
		if _, _, err := readPage(t, f, tenantA, scopeA, 0, 10, pgx.TxOptions{}); !errors.Is(err, events.ErrCursorExpired) {
			t.Fatalf("expired cursor: %v", err)
		}
		if _, _, err := readPage(t, f, tenantA, scopeA, high-1, 10, pgx.TxOptions{}); !errors.Is(err, events.ErrCursorExpired) {
			t.Fatalf("cursor just below the floor: %v", err)
		}
		// New events continue the sequence; a cursor at the floor resumes cleanly.
		// A producer's event after the prune occurs now, not at the fixture's
		// fixed past instant (which the boundary would rightly refuse).
		res2 := f.append(f.fresh(eventA("ord_after_prune", 1)))
		if res2.Sequence != high+1 {
			t.Fatalf("sequence after pruning = %d, want %d", res2.Sequence, high+1)
		}
		if seqs, _, err := readPage(t, f, tenantA, scopeA, high, 10, pgx.TxOptions{}); err != nil || fmt.Sprint(seqs) != fmt.Sprint([]int64{high + 1}) {
			t.Fatalf("resume at the floor: %v %v", seqs, err)
		}
		// The tenant-wide scope was pruned independently and keeps its own floor.
		if _, w, _ := readPage(t, f, tenantA, events.Scope{TenantID: tenantA}, 1, 10, pgx.TxOptions{}); w.Floor != 1 {
			t.Fatalf("tenant-wide floor: %+v", w)
		}
	})
}
