package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// TestClaimFencing: concurrent publishers get disjoint claims, an expired lease
// is reclaimed under a new token, and a stale worker can neither complete nor
// fail a row another claim owns.
func TestClaimFencing(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	const total = 24
	for i := 0; i < total; i++ {
		f.append(eventA("cf"+string(rune('a'+i%26))+string(rune('a'+i/26)), int64(i+1)))
	}
	now := f.clock.Now()
	const lease = time.Minute

	t.Run("concurrent publishers receive disjoint claims", func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					claims, err := f.store.ClaimDue(ctx, tenantA, now.Add(time.Second), lease, 3)
					if err != nil {
						t.Errorf("ClaimDue: %v", err)
						return
					}
					if len(claims) == 0 {
						return
					}
					mu.Lock()
					for _, c := range claims {
						seen[c.EventID]++
						if c.Token != 1 || c.Attempt != 1 || c.SubjectToken == "" || len(c.Payload) == 0 {
							t.Errorf("claim: %+v", c)
						}
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(seen) != total {
			t.Fatalf("claimed %d distinct events, want %d", len(seen), total)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("event %s was handed to %d publishers", id, n)
			}
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE state = 'claimed' AND claim_token = 1`); got != total {
			t.Fatalf("claimed rows = %d", got)
		}
	})

	t.Run("expired lease is reclaimed and stale holders are fenced", func(t *testing.T) {
		// The first claim expires; a second publisher reclaims one row.
		old := now.Add(time.Second)
		later := old.Add(2 * lease)
		fresh, err := f.store.ClaimDue(ctx, tenantA, later, lease, 1)
		if err != nil || len(fresh) != 1 || fresh[0].Token != 2 || fresh[0].Attempt != 2 {
			t.Fatalf("reclaim: %+v %v", fresh, err)
		}
		c := fresh[0]
		stale := events.Claim{TenantID: c.TenantID, EventID: c.EventID, Token: 1}
		if err := f.store.Complete(ctx, stale, later); !errors.Is(err, events.ErrStaleClaim) {
			t.Fatalf("stale completion: %v", err)
		}
		if err := f.store.Fail(ctx, stale, events.ClassTransient, "late", later); !errors.Is(err, events.ErrStaleClaim) {
			t.Fatalf("stale failure: %v", err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = $1 AND state = 'claimed' AND claim_token = 2`, c.EventID); got != 1 {
			t.Fatalf("a stale worker changed the row")
		}
		// Unclassified failures are rejected and change nothing.
		if err := f.store.Fail(ctx, c, "", "boom", later); !errors.Is(err, events.ErrUnclassified) {
			t.Fatalf("unclassified failure: %v", err)
		}
		// The current holder fails with a class: retry is scheduled, the claim released.
		if err := f.store.Fail(ctx, c, events.ClassTransient, "broker unreachable", later); err != nil {
			t.Fatal(err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = $1 AND state = 'pending' AND last_failure_class = 'transient' AND next_attempt_at > $2`, c.EventID, later); got != 1 {
			t.Fatalf("retry not scheduled")
		}
		// The released token can no longer complete it.
		if err := f.store.Complete(ctx, c, later); !errors.Is(err, events.ErrInvalidTransition) {
			t.Fatalf("completion of a released claim: %v", err)
		}
		// After its backoff the row is claimed again under token 3.
		all, err := f.store.ClaimDue(ctx, tenantA, later.Add(time.Hour), lease, 100) // the other expired leases are reclaimable too
		if err != nil {
			t.Fatal(err)
		}
		var again []events.Claim
		for _, cl := range all {
			if cl.EventID == c.EventID {
				again = append(again, cl)
			}
		}
		if len(again) != 1 || again[0].Token != 3 {
			t.Fatalf("re-claim after backoff: %+v", again)
		}
		if err := f.store.Complete(ctx, again[0], later.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got := f.count(tenantA, `SELECT count(*) FROM outbox WHERE event_id = $1 AND state = 'published'`, c.EventID); got != 1 {
			t.Fatal("not published")
		}
		// Published is terminal: it is never claimed again and double completion fails.
		if err := f.store.Complete(ctx, again[0], later); !errors.Is(err, events.ErrInvalidTransition) {
			t.Fatalf("double completion: %v", err)
		}
		// Attempt history: unknown (expired), failed, published.
		var outcomes []string
		if err := f.tx(tenantA, func(ctx context.Context, tx pgxTx) error {
			rows, err := tx.Query(ctx, `SELECT outcome FROM outbox_attempts WHERE event_id = $1 ORDER BY attempt`, c.EventID)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var o string
				if err := rows.Scan(&o); err != nil {
					return err
				}
				outcomes = append(outcomes, o)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		if len(outcomes) != 3 || outcomes[0] != "transient" || outcomes[1] != "transient" || outcomes[2] != "published" {
			t.Fatalf("attempt history: %v", outcomes)
		}
	})

	t.Run("claims never cross tenants", func(t *testing.T) {
		f.append(eventB("cf_other", 1))
		claims, err := f.store.ClaimDue(ctx, domain.TenantID(tenantB), now.Add(24*time.Hour), lease, 100)
		if err != nil || len(claims) != 1 || claims[0].TenantID != tenantB || claims[0].EventID != "cf_other" {
			t.Fatalf("tenant B claims: %+v %v", claims, err)
		}
		// A claim for tenant B cannot be completed through tenant A's context.
		forged := claims[0]
		forged.TenantID = tenantA
		if err := f.store.Complete(ctx, forged, now); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("cross-tenant completion: %v", err)
		}
	})
}
