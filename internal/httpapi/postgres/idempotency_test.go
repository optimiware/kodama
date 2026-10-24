package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

func Test_idempotency_changed_body(t *testing.T) {
	e := newEnv(t)

	first := e.create("alice", "tenant-a", "pa1", "key-1", `{"name":"alpha"}`)
	if first.Code != 201 || first.replayed() {
		t.Fatalf("first create: %d %s", first.Code, first.Body)
	}
	original := first.widget(t)

	t.Run("same body replays the original response", func(t *testing.T) {
		again := e.create("alice", "tenant-a", "pa1", "key-1", `{"name":"alpha"}`)
		if again.Code != 201 || !again.replayed() || again.Body != first.Body {
			t.Fatalf("replay: %d replayed=%v %s (original %s)", again.Code, again.replayed(), again.Body, first.Body)
		}
		if again.Header.Get("ETag") != `"1"` || again.Header.Get("Location") != first.Header.Get("Location") {
			t.Fatalf("replay headers = %v", again.Header)
		}
		if again.widget(t) != original {
			t.Fatal("replayed a different resource")
		}
	})

	t.Run("changed or byte-different body conflicts without a second effect", func(t *testing.T) {
		for name, body := range map[string]string{
			"different name":   `{"name":"beta"}`,
			"whitespace":       `{"name": "alpha"}`,
			"trailing newline": "{\"name\":\"alpha\"}\n",
		} {
			rec := e.create("alice", "tenant-a", "pa1", "key-1", body)
			if rec.Code != 409 || rec.errorCode(t) != "idempotency_conflict" {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body, original.ID) {
				t.Errorf("%s: conflict disclosed the stored response", name)
			}
		}
		if e.widgets(t) != 1 || e.history(t) != 1 || e.records(t) != 1 {
			t.Fatalf("effects: widgets=%d history=%d records=%d", e.widgets(t), e.history(t), e.records(t))
		}
	})

	t.Run("a changed query string conflicts too", func(t *testing.T) {
		rec := e.call("POST", createA1+"?dry=1", "alice", `{"name":"alpha"}`, "Idempotency-Key", "key-1")
		if rec.Code != 409 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	})

	t.Run("the hash is stored separately and is not a key column", func(t *testing.T) {
		rows, err := e.mig.Query(testCtx(t), `
SELECT a.attname
FROM pg_index i
JOIN pg_class c ON c.oid = i.indrelid
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey)
WHERE c.relname = 'idempotency' AND i.indisprimary
ORDER BY array_position(i.indkey::int2[], a.attnum)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, c)
		}
		if got := strings.Join(cols, ","); got != "tenant_id,principal,method,canonical_route,key" {
			t.Fatalf("primary key = %s", got)
		}
		if n := e.count(t, `SELECT count(*) FROM pg_indexes WHERE tablename = 'idempotency' AND (indexdef ILIKE '%request_hash%' OR indexdef ILIKE '%project_id%')`); n != 0 {
			t.Fatalf("%d indexes include the hash or project", n)
		}
		if n := e.count(t, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid WHERE t.relname = 'idempotency' AND c.contype IN ('u','p') AND c.conkey <> ARRAY[]::int2[] AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = t.oid AND a.attnum = ANY (c.conkey) AND a.attname IN ('request_hash','project_id'))`); n != 0 {
			t.Fatalf("%d unique constraints include the hash or project", n)
		}
		var stored []byte
		if err := e.mig.QueryRow(testCtx(t), `SELECT request_hash FROM idempotency`).Scan(&stored); err != nil || len(stored) != 32 {
			t.Fatalf("stored hash: %v len=%d", err, len(stored))
		}
	})

	t.Run("a different key is a new request", func(t *testing.T) {
		rec := e.create("alice", "tenant-a", "pa1", "key-2", `{"name":"beta"}`)
		if rec.Code != 201 || rec.replayed() || rec.widget(t).ID == original.ID {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if e.widgets(t) != 2 {
			t.Fatalf("widgets = %d", e.widgets(t))
		}
	})
}

func Test_idempotency_concurrent(t *testing.T) {
	t.Run("identical simultaneous requests execute once", func(t *testing.T) {
		e := newEnv(t)
		// Hold the winner inside its transaction so the others genuinely queue
		// behind the claim.
		release := make(chan struct{})
		var once sync.Once
		entered := make(chan struct{})
		e.setHook(func(ctx context.Context, tx pgx.Tx) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		})
		const n = 8
		results := make([]result, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = e.create("alice", "tenant-a", "pa1", "same", `{"name":"once"}`)
			}()
		}
		<-entered
		time.Sleep(300 * time.Millisecond) // let the other requests reach the lock
		close(release)
		wg.Wait()

		replays := 0
		for i, r := range results {
			if r.Code != 201 || r.Body != results[0].Body {
				t.Fatalf("result %d: %d %s (first %s)", i, r.Code, r.Body, results[0].Body)
			}
			if r.replayed() {
				replays++
			}
		}
		if replays != n-1 {
			t.Fatalf("replays = %d, want %d", replays, n-1)
		}
		if e.doCalls.Load() != 1 || e.widgets(t) != 1 || e.history(t) != 1 || e.records(t) != 1 {
			t.Fatalf("callback ran %d times: widgets=%d history=%d records=%d", e.doCalls.Load(), e.widgets(t), e.history(t), e.records(t))
		}
	})

	t.Run("different bodies race to one success and conflicts", func(t *testing.T) {
		e := newEnv(t)
		const n = 6
		results := make([]result, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = e.create("alice", "tenant-a", "pa1", "race", fmt.Sprintf(`{"name":"n%d"}`, i))
			}()
		}
		close(start)
		wg.Wait()
		ok, conflict := 0, 0
		for _, r := range results {
			switch {
			case r.Code == 201 && !r.replayed():
				ok++
			case r.Code == 409 && r.errorCode(t) == "idempotency_conflict":
				conflict++
			default:
				t.Fatalf("unexpected: %d %s", r.Code, r.Body)
			}
		}
		if ok != 1 || conflict != n-1 || e.widgets(t) != 1 || e.records(t) != 1 {
			t.Fatalf("ok=%d conflict=%d widgets=%d records=%d", ok, conflict, e.widgets(t), e.records(t))
		}
	})

	t.Run("principals, routes and tenants are independent keys", func(t *testing.T) {
		e := newEnv(t)
		reqs := []struct{ token, tenant, project string }{
			{"alice", "tenant-a", "pa1"},
			{"bob", "tenant-a", "pa1"},   // another principal
			{"alice", "tenant-a", "pa2"}, // another route (project)
			{"alice", "tenant-b", "pb1"}, // another tenant
		}
		results := make([]result, len(reqs))
		var wg sync.WaitGroup
		for i, rq := range reqs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = e.create(rq.token, rq.tenant, rq.project, "shared-key", `{"name":"same"}`)
			}()
		}
		wg.Wait()
		ids := map[string]bool{}
		for i, r := range results {
			if r.Code != 201 || r.replayed() {
				t.Fatalf("request %d: %d %s", i, r.Code, r.Body)
			}
			ids[r.widget(t).ID] = true
		}
		if len(ids) != len(reqs) || e.widgets(t) != len(reqs) || e.records(t) != len(reqs) {
			t.Fatalf("ids=%d widgets=%d records=%d", len(ids), e.widgets(t), e.records(t))
		}
		// And each is replayed only by its own owner.
		if r := e.create("bob", "tenant-a", "pa1", "shared-key", `{"name":"same"}`); !r.replayed() || r.widget(t).ID != results[1].widget(t).ID {
			t.Fatalf("bob replay: %d %s", r.Code, r.Body)
		}
	})

	t.Run("a waiter gives up with 503 and retry guidance", func(t *testing.T) {
		e := newEnv(t, func(c *httppg.Config) { c.LockTimeout = 300 * time.Millisecond })
		release := make(chan struct{})
		entered := make(chan struct{})
		var once sync.Once
		e.setHook(func(ctx context.Context, tx pgx.Tx) error {
			// The business callback must not inherit the claim's short timeout.
			var lt string
			if err := tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&lt); err != nil || lt != "0" {
				return fmt.Errorf("lock_timeout in callback = %q, %v", lt, err)
			}
			once.Do(func() { close(entered) })
			<-release
			return nil
		})
		var first result
		done := make(chan struct{})
		go func() {
			defer close(done)
			first = e.create("alice", "tenant-a", "pa1", "slow", `{"name":"slow"}`)
		}()
		<-entered
		waiter := e.create("alice", "tenant-a", "pa1", "slow", `{"name":"slow"}`)
		close(release)
		<-done
		if waiter.Code != 503 || waiter.errorCode(t) != "unavailable" || waiter.Header.Get("Retry-After") == "" {
			t.Fatalf("waiter: %d %v %s", waiter.Code, waiter.Header, waiter.Body)
		}
		if first.Code != 201 || e.widgets(t) != 1 {
			t.Fatalf("first: %d %s widgets=%d", first.Code, first.Body, e.widgets(t))
		}
		// The retry after the wait replays the committed response.
		if r := e.create("alice", "tenant-a", "pa1", "slow", `{"name":"slow"}`); r.Code != 201 || !r.replayed() {
			t.Fatalf("retry: %d %s", r.Code, r.Body)
		}
	})
}

func Test_idempotency_expiry(t *testing.T) {
	expire := func(e *env, key string) {
		t.Helper()
		e.ownerExec(t, `UPDATE idempotency SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE key = $1`, key)
	}

	t.Run("entries replay before expiry", func(t *testing.T) {
		e := newEnv(t)
		a := e.create("alice", "tenant-a", "pa1", "k", `{"name":"a"}`)
		var secs float64
		if err := e.mig.QueryRow(testCtx(t), `SELECT extract(epoch FROM expires_at - created_at) FROM idempotency`).Scan(&secs); err != nil || secs < 86399 || secs > 86401 {
			t.Fatalf("default retention = %vs, %v", secs, err)
		}
		b := e.create("alice", "tenant-a", "pa1", "k", `{"name":"a"}`)
		if !b.replayed() || b.Body != a.Body || e.widgets(t) != 1 {
			t.Fatalf("replay before expiry: %d %s", b.Code, b.Body)
		}
	})

	t.Run("retention is injected", func(t *testing.T) {
		e := newEnv(t, func(c *httppg.Config) { c.Retention = time.Second })
		a := e.create("alice", "tenant-a", "pa1", "k", `{"name":"a"}`)
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"a"}`); !r.replayed() {
			t.Fatalf("no replay inside retention: %d %s", r.Code, r.Body)
		}
		time.Sleep(1200 * time.Millisecond)
		b := e.create("alice", "tenant-a", "pa1", "k", `{"name":"a"}`)
		if b.Code != 201 || b.replayed() || b.widget(t).ID == a.widget(t).ID || e.widgets(t) != 2 || e.records(t) != 1 {
			t.Fatalf("after retention: %d %s widgets=%d records=%d", b.Code, b.Body, e.widgets(t), e.records(t))
		}
	})

	t.Run("an expired key is admitted once, even with a different body", func(t *testing.T) {
		e := newEnv(t)
		first := e.create("alice", "tenant-a", "pa1", "k", `{"name":"old"}`)
		expire(e, "k")

		const n = 8
		results := make([]result, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = e.create("alice", "tenant-a", "pa1", "k", `{"name":"new"}`)
			}()
		}
		close(start)
		wg.Wait()
		fresh := 0
		for i, r := range results {
			if r.Code != 201 || r.widget(t).Name != "new" || r.Body != results[0].Body {
				t.Fatalf("result %d: %d %s", i, r.Code, r.Body)
			}
			if !r.replayed() {
				fresh++
			}
		}
		if fresh != 1 || e.widgets(t) != 2 || e.records(t) != 1 {
			t.Fatalf("fresh=%d widgets=%d records=%d", fresh, e.widgets(t), e.records(t))
		}
		if results[0].widget(t).ID == first.widget(t).ID {
			t.Fatal("expired response was replayed")
		}
		var live bool
		if err := e.mig.QueryRow(testCtx(t), `SELECT expires_at > now() FROM idempotency`).Scan(&live); err != nil || !live {
			t.Fatalf("replacement record is not live: %v", err)
		}
	})

	t.Run("expiry and a failing replacement leave the key absent for a retry", func(t *testing.T) {
		e := newEnv(t)
		e.create("alice", "tenant-a", "pa1", "k", `{"name":"old"}`)
		expire(e, "k")
		e.setHook(func(context.Context, pgx.Tx) error { return fmt.Errorf("boom") })
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"new"}`); r.Code != 500 {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		if e.widgets(t) != 1 {
			t.Fatalf("widgets = %d", e.widgets(t))
		}
		e.setHook(nil)
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"new"}`); r.Code != 201 || r.replayed() {
			t.Fatalf("retry: %d %s", r.Code, r.Body)
		}
	})

	t.Run("purge removes only expired records of one tenant", func(t *testing.T) {
		e := newEnv(t)
		for i := 0; i < 3; i++ {
			e.create("alice", "tenant-a", "pa1", fmt.Sprintf("old-%d", i), `{"name":"x"}`)
		}
		e.create("alice", "tenant-a", "pa1", "live", `{"name":"x"}`)
		e.create("alice", "tenant-b", "pb1", "old-b", `{"name":"x"}`)
		e.ownerExec(t, `UPDATE idempotency SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE key LIKE 'old-%'`)

		if _, err := e.idem.PurgeExpired(testCtx(t), tenantA, 0); err == nil {
			t.Fatal("zero limit accepted")
		}
		n, err := e.idem.PurgeExpired(testCtx(t), tenantA, 2)
		if err != nil || n != 2 {
			t.Fatalf("first purge: %d, %v", n, err)
		}
		n, err = e.idem.PurgeExpired(testCtx(t), tenantA, 10)
		if err != nil || n != 1 {
			t.Fatalf("second purge: %d, %v", n, err)
		}
		if got := e.records(t); got != 2 { // tenant-a live + tenant-b old (other tenant untouched)
			t.Fatalf("records = %d", got)
		}
		if r := e.create("alice", "tenant-a", "pa1", "live", `{"name":"x"}`); !r.replayed() {
			t.Fatal("live record purged")
		}
	})
}
