package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/httpapi"
)

func Test_replay_after_revocation(t *testing.T) {
	setup := func(t *testing.T) (*env, result) {
		e := newEnv(t)
		first := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`)
		if first.Code != 201 {
			t.Fatalf("create: %d %s", first.Code, first.Body)
		}
		return e, first
	}

	t.Run("route-level revocation denies the replay before any lookup", func(t *testing.T) {
		e, first := setup(t)
		e.auth.revoke("alice", httpapi.ErrDenied, nil)
		r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`)
		if r.Code != 403 || r.errorCode(t) != "forbidden" || r.Header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("%d %v %s", r.Code, r.Header, r.Body)
		}
		if e.widgets(t) != 1 || e.doCalls.Load() != 1 {
			t.Fatalf("revoked request mutated: widgets=%d calls=%d", e.widgets(t), e.doCalls.Load())
		}
		_ = first
	})

	t.Run("resource-level revocation denies without disclosing the cached body", func(t *testing.T) {
		e, first := setup(t)
		// Route access remains, but access to the created widget is gone: only the
		// stored target reference can reveal that.
		e.auth.revoke("alice", nil, httpapi.ErrDenied)
		r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`)
		if r.Code != 403 || r.errorCode(t) != "forbidden" {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		for _, leaked := range []string{"secret-name", first.widget(t).ID, "Location"} {
			if contains(r.Body, leaked) || contains(headerDump(r), leaked) {
				t.Fatalf("replay denial disclosed %q: %s %v", leaked, r.Body, r.Header)
			}
		}
		if e.widgets(t) != 1 || e.doCalls.Load() != 1 || e.records(t) != 1 {
			t.Fatalf("widgets=%d calls=%d records=%d", e.widgets(t), e.doCalls.Load(), e.records(t))
		}
		// A hidden reference is a plain 404.
		e.auth.revoke("alice", nil, httpapi.ErrHidden)
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`); r.Code != 404 || r.errorCode(t) != "not_found" {
			t.Fatalf("hidden: %d %s", r.Code, r.Body)
		}
		// An authorizer outage is a refusal too.
		e.auth.revoke("alice", nil, context.DeadlineExceeded)
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`); r.Code != 503 || contains(r.Body, "secret-name") {
			t.Fatalf("outage: %d %s", r.Code, r.Body)
		}
		// Another principal is unaffected and never sees alice's record.
		if r := e.create("bob", "tenant-a", "pa1", "k", `{"name":"secret-name"}`); r.Code != 201 || r.replayed() {
			t.Fatalf("bob: %d %s", r.Code, r.Body)
		}
	})

	t.Run("a changed body after revocation still discloses nothing", func(t *testing.T) {
		e, first := setup(t)
		e.auth.revoke("alice", httpapi.ErrDenied, nil)
		r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"other"}`)
		if r.Code != 403 || contains(r.Body, first.widget(t).ID) {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
	})

	t.Run("authorization is rechecked after waiting on a concurrent request", func(t *testing.T) {
		e := newEnv(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		e.setHook(func(ctx context.Context, tx pgx.Tx) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		})
		var winner, waiter result
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			winner = e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`)
		}()
		<-entered
		wg.Add(1)
		go func() {
			defer wg.Done()
			waiter = e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`)
		}()
		// The waiter passes route authorization immediately, then queues on the claim.
		deadline := time.Now().Add(5 * time.Second)
		for e.authCalls() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		// Revoke resource access while the waiter is blocked, then let the winner finish.
		e.auth.revoke("alice", nil, httpapi.ErrDenied)
		close(release)
		wg.Wait()

		if winner.Code != 201 {
			t.Fatalf("winner: %d %s", winner.Code, winner.Body)
		}
		if waiter.Code != 403 || contains(waiter.Body, "secret-name") || contains(waiter.Body, winner.widget(t).ID) {
			t.Fatalf("waiter: %d %s", waiter.Code, waiter.Body)
		}
		if e.widgets(t) != 1 || e.doCalls.Load() != 1 {
			t.Fatalf("widgets=%d calls=%d", e.widgets(t), e.doCalls.Load())
		}
	})

	// derivePath addresses the keyed, resource-scoped operations of the env.
	const seed = "w_seed"
	derivePath := func(op string) string { return createA1 + "/" + seed + "/" + op }
	derive := func(e *env, op, key string) result {
		return e.call("POST", derivePath(op), "alice", `{"x":1}`, "Idempotency-Key", key)
	}

	// waitedReplay runs a winner holding the claim and a waiter queued behind
	// it, calls between (while the waiter waits) and then lets the winner finish.
	waitedReplay := func(t *testing.T, e *env, op string, between func()) (winner, waiter result) {
		t.Helper()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		e.setHook(func(ctx context.Context, tx pgx.Tx) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); winner = derive(e, op, "k") }()
		<-entered
		wg.Add(1)
		go func() { defer wg.Done(); waiter = derive(e, op, "k") }()
		deadline := time.Now().Add(5 * time.Second)
		for e.authCalls() < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
		between()
		close(release)
		wg.Wait()
		return winner, waiter
	}
	assertNoDisclosure := func(t *testing.T, r result, winner result) {
		t.Helper()
		for _, leaked := range []string{"derived-secret", winner.widget(t).ID} {
			if contains(r.Body, leaked) || contains(headerDump(r), leaked) {
				t.Fatalf("denial disclosed %q: %s %v", leaked, r.Body, r.Header)
			}
		}
		if r.Header.Get("Etag") != "" || r.Header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("denial carried cached headers: %v", r.Header)
		}
	}

	t.Run("a resource-scoped replay without a stored target rechecks the route resource after waiting", func(t *testing.T) {
		e := newEnv(t)
		winner, waiter := waitedReplay(t, e, "clone", func() {
			// Access to the addressed widget disappears while the waiter is queued.
			e.auth.denyResource("widget", seed, httpapi.ErrDenied)
		})
		if winner.Code != 201 {
			t.Fatalf("winner: %d %s", winner.Code, winner.Body)
		}
		if waiter.Code != 403 || waiter.errorCode(t) != "forbidden" {
			t.Fatalf("waiter: %d %s", waiter.Code, waiter.Body)
		}
		assertNoDisclosure(t, waiter, winner)
		if e.widgets(t) != 1 || e.doCalls.Load() != 1 {
			t.Fatalf("widgets=%d calls=%d", e.widgets(t), e.doCalls.Load())
		}
		// The same holds for a later, unraced replay, and a hidden resource is a 404.
		if r := derive(e, "clone", "k"); r.Code != 403 {
			t.Fatalf("later replay: %d %s", r.Code, r.Body)
		}
		e.auth.denyResource("widget", seed, httpapi.ErrHidden)
		if r := derive(e, "clone", "k"); r.Code != 404 {
			t.Fatalf("hidden: %d %s", r.Code, r.Body)
		}
		// Once access returns the stored response is replayed.
		e.auth.denyResource("widget", seed, nil)
		if r := derive(e, "clone", "k"); r.Code != 201 || !r.replayed() {
			t.Fatalf("restored: %d %s", r.Code, r.Body)
		}
	})

	t.Run("a distinct stored target is checked in addition to the route resource", func(t *testing.T) {
		// Only the route resource is revoked: the stored target alone is not enough.
		e := newEnv(t)
		winner, waiter := waitedReplay(t, e, "derive", func() {
			e.auth.denyResource("widget", seed, httpapi.ErrDenied)
		})
		if winner.Code != 201 || waiter.Code != 403 {
			t.Fatalf("winner %d %s / waiter %d %s", winner.Code, winner.Body, waiter.Code, waiter.Body)
		}
		assertNoDisclosure(t, waiter, winner)

		// Only the stored target is revoked: the route resource is not enough.
		e = newEnv(t)
		first := derive(e, "derive", "k")
		if first.Code != 201 {
			t.Fatalf("first: %d %s", first.Code, first.Body)
		}
		e.auth.denyResource("widget", first.widget(t).ID, httpapi.ErrDenied)
		r := derive(e, "derive", "k")
		if r.Code != 403 {
			t.Fatalf("replay: %d %s", r.Code, r.Body)
		}
		assertNoDisclosure(t, r, first)
		if e.widgets(t) != 1 || e.doCalls.Load() != 1 {
			t.Fatalf("widgets=%d calls=%d", e.widgets(t), e.doCalls.Load())
		}
	})

	t.Run("authorization is called outside any SQL transaction", func(t *testing.T) {
		e, _ := setup(t)
		var open int
		e.auth.onTarget = func(ctx context.Context) {
			// Count sessions of the runtime role sitting inside a transaction.
			if err := e.mig.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND usename = $1 AND state LIKE 'idle in transaction%'`, e.db.RuntimeRole).Scan(&open); err != nil {
				t.Errorf("inspect sessions: %v", err)
			}
		}
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"secret-name"}`); r.Code != 201 || !r.replayed() {
			t.Fatalf("replay: %d %s", r.Code, r.Body)
		}
		if open != 0 {
			t.Fatalf("%d runtime transactions were open during replay authorization", open)
		}
	})
}
