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

	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/integrations/chatops/chatopstest"
	"github.com/optimiware/kodama/internal/storage"
)

var policy = chatops.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 3}

// seed creates a destination and n pending intents for it.
func seed(t testing.TB, e *chatopstest.Env, a chatopstest.Callers, name string, n int) (chatops.Destination, []chatops.DeliveryID) {
	t.Helper()
	d := e.Destination(a.Admin, scopeA, name, "https://hooks.t041.example/"+name)
	var ids []chatops.DeliveryID
	for i := 0; i < n; i++ {
		ev := chatopstest.WorkItem(scopeA, fmt.Sprintf("evt_%s_%03d", name, i), fmt.Sprintf("wi_%s_%d", name, i), 1)
		e.Append(ev)
		process(t, e, ev)
		ids = append(ids, chatops.DeliveryIDFor(scopeA, d.ID, ev.ID))
	}
	return d, ids
}

func now() time.Time { return time.Now().UTC().Add(time.Second) }

func attemptRows(t testing.TB, e *chatopstest.Env, id chatops.DeliveryID) (rows []string) {
	t.Helper()
	r, err := e.Owner.Query(context.Background(), `SELECT attempt || ':' || claim_token || ':' || result || ':' || class || ':' || uncertain FROM chatops_delivery_attempts
WHERE delivery_id = $1 ORDER BY attempt`, string(id))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for r.Next() {
		var s string
		if err := r.Scan(&s); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, s)
	}
	return rows
}

// TestConcurrentClaimsAndReplay: competing workers, expired claims, stale
// finalizers, duplicate replay requests and uncertain commits preserve one
// durable identity and history. Real PostgreSQL.
func TestConcurrentClaimsAndReplay(t *testing.T) {
	ctx := context.Background()

	t.Run("competing workers never claim the same delivery", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "claims")
		_, ids := seed(t, e, a, "race", 30)

		var mu sync.Mutex
		claimed := map[chatops.DeliveryID]int{}
		var wg sync.WaitGroup
		var errs atomic.Int64
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					batch, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 7, policy)
					if err != nil {
						errs.Add(1)
						return
					}
					if len(batch.Claims) == 0 {
						return
					}
					mu.Lock()
					for _, c := range batch.Claims {
						claimed[c.Delivery.ID]++
						if c.Token != 1 || c.Attempt != 1 || c.Delivery.State != chatops.StateClaimed {
							t.Errorf("claim = %+v", c)
						}
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if errs.Load() != 0 {
			t.Fatalf("%d claim errors", errs.Load())
		}
		if len(claimed) != len(ids) {
			t.Fatalf("claimed %d of %d deliveries", len(claimed), len(ids))
		}
		for id, n := range claimed {
			if n != 1 {
				t.Fatalf("delivery %s was claimed %d times", id, n)
			}
		}
		if n := e.Count(`SELECT count(*) FROM chatops_delivery_attempts`); n != len(ids) {
			t.Fatalf("attempt rows = %d, want %d (one per claim, written before any send)", n, len(ids))
		}
	})

	t.Run("a disabled destination's backlog does not starve the others", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "starve")
		backlog, _ := seed(t, e, a, "backlog", 6)
		disable := false
		if _, err := e.Svc.UpdateDestination(ctx, a.Admin, scopeA, backlog.ID, backlog.Version, chatops.DestinationEdit{Enabled: &disable}); err != nil {
			t.Fatal(err)
		}
		// A later destination's intents are due too, but sort after the backlog.
		time.Sleep(20 * time.Millisecond)
		seed(t, e, a, "live", 2)
		b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now().Add(time.Second), time.Minute, 2, policy)
		if err != nil || len(b.Claims) != 2 {
			t.Fatalf("claimed %d of the enabled destination's 2 deliveries behind a disabled backlog (%v)", len(b.Claims), err)
		}
		for _, c := range b.Claims {
			if c.Destination.Name != "live" {
				t.Fatalf("a disabled destination's delivery was claimed: %+v", c.Destination)
			}
		}
	})

	t.Run("an expired claim is reclaimed under a new token and the stale finalizer changes nothing", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "expiry")
		_, ids := seed(t, e, a, "expiry", 1)
		start := now()
		first, err := e.Store.ClaimDue(ctx, scopeA.TenantID, start, 10*time.Second, 5, policy)
		if err != nil || len(first.Claims) != 1 {
			t.Fatalf("first claim = %+v %v", first, err)
		}
		// Before the lease ends nobody else can take it.
		if again, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, start.Add(5*time.Second), 10*time.Second, 5, policy); len(again.Claims) != 0 {
			t.Fatal("a live claim was taken over")
		}
		second, err := e.Store.ClaimDue(ctx, scopeA.TenantID, start.Add(11*time.Second), 10*time.Second, 5, policy)
		if err != nil || len(second.Claims) != 1 {
			t.Fatalf("reclaim = %+v %v", second, err)
		}
		old, cur := first.Claims[0], second.Claims[0]
		if cur.Token != old.Token+1 || cur.Attempt != 2 {
			t.Fatalf("reclaim token/attempt = %d/%d", cur.Token, cur.Attempt)
		}
		// The abandoned attempt is closed as an unknown outcome.
		if got := attemptRows(t, e, ids[0]); len(got) != 2 || got[0] != "1:1:abandoned:lease_expired:true" || got[1][:4] != "2:2:" {
			t.Fatalf("attempt history = %v", got)
		}
		// The stale holder finishes late: refused, nothing changes.
		stale := chatops.Outcome{Delivered: true, HTTPStatus: 200}
		if _, err := e.Store.Finish(ctx, old, stale, start.Add(12*time.Second), policy); !errors.Is(err, chatops.ErrStaleClaim) {
			t.Fatalf("stale finish = %v", err)
		}
		if d := e.Delivery(scopeA, ids[0]); d.State != chatops.StateClaimed || d.ClaimToken != 2 || !d.DeliveredAt.IsZero() {
			t.Fatalf("a stale finalizer changed the delivery: %+v", d)
		}
		// The current holder completes; a second (retried) completion is idempotent;
		// the stale holder is still refused afterwards.
		done, err := e.Store.Finish(ctx, cur, chatops.Outcome{Delivered: true, HTTPStatus: 202, Receipt: "r-1"}, start.Add(13*time.Second), policy)
		if err != nil || done.State != chatops.StateDelivered || done.Receipt != "r-1" {
			t.Fatalf("finish = %+v %v", done, err)
		}
		again, err := e.Store.Finish(ctx, cur, chatops.Outcome{Delivered: true, HTTPStatus: 202, Receipt: "r-1"}, start.Add(14*time.Second), policy)
		if err != nil || again.Version != done.Version {
			t.Fatalf("repeated finish = %+v %v", again, err)
		}
		if _, err := e.Store.Finish(ctx, old, stale, start.Add(15*time.Second), policy); !errors.Is(err, chatops.ErrStaleClaim) {
			t.Fatalf("stale finish after delivery = %v", err)
		}
		if got := attemptRows(t, e, ids[0]); len(got) != 2 || got[1] != "2:2:delivered::false" {
			t.Fatalf("attempt history = %v", got)
		}
		// A delivered notification is never claimed or replayed again.
		if later, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, start.Add(time.Hour), time.Minute, 5, policy); len(later.Claims) != 0 {
			t.Fatal("a delivered notification was claimed again")
		}
	})

	t.Run("repeated lease expiry exhausts the cycle instead of sending forever", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "crashloop")
		_, ids := seed(t, e, a, "crashloop", 1)
		at := now()
		for i := 0; i < policy.MaxAttempts; i++ {
			b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, at, 5*time.Second, 5, policy)
			if err != nil || len(b.Claims) != 1 {
				t.Fatalf("claim %d = %+v %v", i, b, err)
			}
			at = at.Add(6 * time.Second)
		}
		b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, at, 5*time.Second, 5, policy)
		if err != nil || len(b.Claims) != 0 || b.Exhausted != 1 {
			t.Fatalf("exhaustion = %+v %v", b, err)
		}
		d := e.Delivery(scopeA, ids[0])
		if d.State != chatops.StateFailed || d.FailedReason != chatops.FailedExhausted || d.LastClass != chatops.ClassLeaseExpired || !d.LastUncertain || d.Attempts != policy.MaxAttempts {
			t.Fatalf("delivery = %+v", d)
		}
		if n := e.Count(`SELECT count(*) FROM chatops_delivery_attempts WHERE delivery_id = $1 AND result = 'abandoned'`, string(ids[0])); n != policy.MaxAttempts {
			t.Fatalf("abandoned attempts = %d", n)
		}
	})

	t.Run("duplicate replay requests create one replay and one history", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "replay")
		_, ids := seed(t, e, a, "replay", 1)
		id := ids[0]
		b, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 1, policy)
		if len(b.Claims) != 1 {
			t.Fatal("no claim")
		}
		failed, err := e.Store.Finish(ctx, b.Claims[0], chatops.Outcome{Class: chatops.ClassRejected, HTTPStatus: 400}, now(), policy)
		if err != nil || failed.State != chatops.StateFailed {
			t.Fatalf("finish = %+v %v", failed, err)
		}
		// Replaying a pending or a stale-version request is refused without a write.
		if _, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, id, failed.Version+9, "stale"); !errors.Is(err, chatops.ErrVersionConflict) {
			t.Fatalf("stale version = %v", err)
		}
		const n = 8
		var wg sync.WaitGroup
		var okCount, conflict, invalid atomic.Int64
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, id, failed.Version, "receiver fixed")
				switch {
				case err == nil:
					okCount.Add(1)
				case errors.Is(err, chatops.ErrVersionConflict):
					conflict.Add(1)
				case errors.Is(err, chatops.ErrInvalidTransition):
					invalid.Add(1)
				default:
					t.Errorf("replay: %v", err)
				}
			}()
		}
		wg.Wait()
		if okCount.Load() != 1 || conflict.Load()+invalid.Load() != n-1 {
			t.Fatalf("ok=%d conflict=%d invalid=%d", okCount.Load(), conflict.Load(), invalid.Load())
		}
		d := e.Delivery(scopeA, id)
		if d.State != chatops.StatePending || d.Replays != 1 || d.Attempts != 1 || d.CycleAttempts != 0 {
			t.Fatalf("delivery = %+v", d)
		}
		if n := e.Count(`SELECT count(*) FROM chatops_replays WHERE delivery_id = $1`, string(id)); n != 1 {
			t.Fatalf("replay rows = %d", n)
		}
		if n := e.Count(`SELECT count(*) FROM audit WHERE action = 'chatops.delivery.replay' AND resource_id = $1`, string(id)); n != 1 {
			t.Fatalf("audit rows = %d", n)
		}
		// The first attempt's record survives the replay: total history is preserved.
		if got := attemptRows(t, e, id); len(got) != 1 || got[0] != "1:1:failed:rejected:false" {
			t.Fatalf("history = %v", got)
		}
		// A second cycle appends to it; the failed delivery can fail and be replayed again.
		b2, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, now().Add(time.Second), time.Minute, 1, policy)
		if len(b2.Claims) != 1 || b2.Claims[0].Attempt != 2 || b2.Claims[0].Token != 2 {
			t.Fatalf("second cycle claim = %+v", b2)
		}
		if got := attemptRows(t, e, id); len(got) != 2 {
			t.Fatalf("history = %v", got)
		}
	})

	t.Run("a delivered notification can never be replayed", func(t *testing.T) {
		e := chatopstest.New(t, chatopstest.Options{})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "final")
		_, ids := seed(t, e, a, "final", 1)
		b, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 1, policy)
		done, err := e.Store.Finish(ctx, b.Claims[0], chatops.Outcome{Delivered: true, HTTPStatus: 200}, now(), policy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, ids[0], done.Version, "please resend"); !errors.Is(err, chatops.ErrInvalidTransition) {
			t.Fatalf("replay of a delivered notification: %v", err)
		}
		// The database guard backs the entity: even a direct write cannot move it.
		err = storage.WithTenant(ctx, e.Runtime, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE chatops_deliveries SET state = 'pending', delivered_at = NULL, version = version + 1 WHERE id = $1`, string(ids[0]))
			return err
		})
		if err == nil {
			t.Fatal("the database let a delivered notification reopen")
		}
	})

	t.Run("an uncertain commit preserves one durable identity and history", func(t *testing.T) {
		var armed atomic.Bool
		e := chatopstest.New(t, chatopstest.Options{WrapDB: func(db storage.TxBeginner) storage.TxBeginner { return &lostCommitDB{inner: db, armed: &armed} }})
		e.SeedProject(scopeA)
		a := e.Actors(scopeA, "uncertain")
		_, ids := seed(t, e, a, "uncertain", 1)
		b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 1, policy)
		if err != nil || len(b.Claims) != 1 {
			t.Fatalf("claim: %+v %v", b, err)
		}
		c := b.Claims[0]
		// The finalization commits but its acknowledgment is lost.
		armed.Store(true)
		_, err = e.Store.Finish(ctx, c, chatops.Outcome{Delivered: true, HTTPStatus: 200, Receipt: "r-9"}, now(), policy)
		if !errors.Is(err, chatops.ErrUnavailable) {
			t.Fatalf("a lost commit acknowledgment must be unavailable, got %v", err)
		}
		if d := e.Delivery(scopeA, ids[0]); d.State != chatops.StateDelivered {
			t.Fatalf("the finalization should have committed: %+v", d)
		}
		// The worker's retry observes what committed: success, no second history entry.
		got, err := e.Store.Finish(ctx, c, chatops.Outcome{Delivered: true, HTTPStatus: 200, Receipt: "r-9"}, now(), policy)
		if err != nil || got.State != chatops.StateDelivered {
			t.Fatalf("retry = %+v %v", got, err)
		}
		if rows := attemptRows(t, e, ids[0]); len(rows) != 1 || rows[0] != "1:1:delivered::false" {
			t.Fatalf("history = %v", rows)
		}

		// A replay whose acknowledgment is lost: retrying with the same version
		// observes one replay (the version moved), never a second one.
		_, ids2 := seed(t, e, a, "uncertain2", 1)
		b2, _ := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 5, policy)
		var c2 chatops.Claim
		for _, cl := range b2.Claims {
			if cl.Delivery.ID == ids2[0] {
				c2 = cl
			}
		}
		failed, err := e.Store.Finish(ctx, c2, chatops.Outcome{Class: chatops.ClassRejected, HTTPStatus: 410}, now(), policy)
		if err != nil {
			t.Fatal(err)
		}
		armed.Store(true)
		if _, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, ids2[0], failed.Version, "retry"); !errors.Is(err, chatops.ErrUnavailable) {
			t.Fatalf("lost replay acknowledgment = %v", err)
		}
		if _, err := e.Svc.ReplayDelivery(ctx, a.Admin, scopeA, ids2[0], failed.Version, "retry"); !errors.Is(err, chatops.ErrVersionConflict) {
			t.Fatalf("replay retry = %v, want the version conflict that proves it committed", err)
		}
		if n := e.Count(`SELECT count(*) FROM chatops_replays WHERE delivery_id = $1`, string(ids2[0])); n != 1 {
			t.Fatalf("replay rows = %d", n)
		}
	})
}

// lostCommitDB commits and then reports a failure once armed: the unknown commit
// outcome of a lost acknowledgment.
type lostCommitDB struct {
	inner storage.TxBeginner
	armed *atomic.Bool
}

func (d *lostCommitDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.inner.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &lostCommitTx{Tx: tx, armed: d.armed}, nil
}

type lostCommitTx struct {
	pgx.Tx
	armed *atomic.Bool
}

func (t *lostCommitTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.armed.CompareAndSwap(true, false) {
		return errors.New("connection lost after commit")
	}
	return nil
}

// TestRestoreFence: the per-tenant delivery fence stops claims in every process
// until an authorized operator clears it; T-005's consumer fence does not cover
// deliveries that already exist.
func TestRestoreFence(t *testing.T) {
	ctx := context.Background()
	e := chatopstest.New(t, chatopstest.Options{})
	e.SeedProject(scopeA)
	e.SeedProject(chatopstest.ScopeB)
	a := e.Actors(scopeA, "fence")
	_, _ = seed(t, e, a, "fence", 3)
	op := chatops.Operator{ID: "ops_1"}

	if err := e.Svc.FenceDeliveries(ctx, chatops.Operator{ID: "mallory"}, scopeA.TenantID, "restore"); !errors.Is(err, chatops.ErrDenied) {
		t.Fatalf("an unauthorized operator fenced: %v", err)
	}
	if err := e.Svc.FenceDeliveries(ctx, op, scopeA.TenantID, "  "); !errors.Is(err, chatops.ErrInvalid) {
		t.Fatalf("a fence without a reason: %v", err)
	}
	if err := e.Svc.FenceDeliveries(ctx, op, scopeA.TenantID, "database restored to backup 17"); err != nil {
		t.Fatal(err)
	}
	if b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 10, policy); err != nil || len(b.Claims) != 0 {
		t.Fatalf("a fenced tenant claimed %d deliveries (%v)", len(b.Claims), err)
	}
	// Another tenant is unaffected by the fence.
	if n := e.Count(`SELECT count(*) FROM chatops_delivery_fences WHERE fenced`); n != 1 {
		t.Fatalf("fences = %d", n)
	}
	// A worker pass sends nothing for the fenced tenant either.
	w := e.Worker(e.Client(nil))
	if res, err := w.RunOnce(ctx); err != nil || res.Claimed != 0 {
		t.Fatalf("worker = %+v %v", res, err)
	}
	if err := e.Svc.ClearDeliveryFence(ctx, op, scopeA.TenantID, "external effects reconciled"); err != nil {
		t.Fatal(err)
	}
	if b, err := e.Store.ClaimDue(ctx, scopeA.TenantID, now(), time.Minute, 10, policy); err != nil || len(b.Claims) != 3 {
		t.Fatalf("after clearing the fence claimed %d (%v)", len(b.Claims), err)
	}
	if n := e.Count(`SELECT count(*) FROM audit WHERE action IN ('chatops.fence.set', 'chatops.fence.clear') AND actor_type = 'operator'`); n != 2 {
		t.Fatalf("fence audit rows = %d", n)
	}
	// Process-local pause is the other half: a paused worker claims nothing.
	w.Pause()
	if res, err := w.RunOnce(ctx); err != nil || res.Claimed != 0 || !w.Paused() {
		t.Fatalf("paused worker = %+v %v", res, err)
	}
	w.Resume()
}
