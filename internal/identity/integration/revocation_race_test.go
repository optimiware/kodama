package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// lockWaiters counts sessions of the test database currently waiting for a lock.
func (e *env) lockWaiters() int {
	e.t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.AdminURL())
	if err != nil {
		e.t.Fatalf("admin connection: %v", err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND wait_event_type = 'Lock'`, e.stack.DB.Name).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) waitForLockWaiters(n int) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for e.lockWaiters() < n {
		if time.Now().After(deadline) {
			e.t.Fatalf("no session is waiting for a lock (want %d)", n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// holdItem locks the work item row as an unrelated transaction would and returns
// the release function.
func (e *env) holdItem(itemPath string) (release func()) {
	e.t.Helper()
	ctx := context.Background()
	id := itemPath[strings.LastIndex(itemPath, "/")+1:]
	tx, err := e.stack.Owner.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, storage.TenantSetting, tenantA); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_items WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantA, id); err != nil {
		e.t.Fatal(err)
	}
	var released bool
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(ctx)
		}
	}
	e.t.Cleanup(release)
	return release
}

type result struct {
	r    response
	done time.Time
}

func (e *env) async(f func() response) chan result {
	ch := make(chan result, 1)
	go func() {
		r := f()
		ch <- result{r: r, done: time.Now()}
	}()
	return ch
}

func notDone(t *testing.T, ch chan result, d time.Duration, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s returned early: %d %s", what, r.r.code, r.r.body)
	case <-time.After(d):
	}
}

func get(t *testing.T, ch chan result, what string) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
	return result{}
}

// TestRevocationAfterLockWait: IT-09 / IT-10. A revocation committed while a
// completion waits on a work lock is seen by the transaction-bound permission
// guard; the opposite ordering settles before the revocation returns.
func TestRevocationAfterLockWait(t *testing.T) {
	e := newEnv(t)
	adminTok, engTok, revTok := e.signIn("admin"), e.signIn("engineer"), e.signIn("reviewer")
	e.bind("admin", tenantA, "", identity.RoleAdmin)
	e.bind("engineer", tenantA, "", identity.RoleEngineer)
	e.bind("reviewer", tenantA, "", identity.RoleViewer) // stays a member, so refusal is 403 rather than hidden

	t.Run("revoked while the completion waits on the work item lock", func(t *testing.T) {
		bid, etag := e.grantVia(adminTok, tenantA, e.users["reviewer"], "", "reviewer")
		item, itemETag := e.itemInReview(engTok, tenantA, projectA, "waits")
		release := e.holdItem(item)

		completion := e.async(func() response { return e.complete(revTok, item, itemETag) })
		e.waitForLockWaiters(1) // the completion passed its route and service checks and now waits
		notDone(t, completion, 200*time.Millisecond, "the blocked completion")

		if r := e.revokeVia(adminTok, tenantA, bid, etag); r.code != 204 {
			t.Fatalf("revocation: %d %s", r.code, r.body)
		}
		release()

		res := get(t, completion, "the completion")
		if res.r.code != 403 || res.r.errCode(t) != "forbidden" {
			t.Fatalf("the revoked reviewer completed after the lock wait: %d %s", res.r.code, res.r.body)
		}
		if e.itemState(item) != "review" {
			t.Fatal("the item changed")
		}
		if got := e.stack.CountIn(tenantA, `SELECT count(*) FROM work_item_history WHERE tenant_id = $1 AND work_item_id = $2 AND to_state = 'done'`, string(tenantA), item[strings.LastIndex(item, "/")+1:]); got != 0 {
			t.Fatalf("%d completion history rows", got)
		}
	})

	t.Run("opposite order: the in-flight completion settles before the revocation returns", func(t *testing.T) {
		bid, etag := e.grantVia(adminTok, tenantA, e.users["reviewer"], "", "reviewer")
		item, itemETag := e.itemInReview(engTok, tenantA, projectA, "settles first")

		reached, gate := make(chan struct{}), make(chan struct{})
		hook := func(context.Context) {
			select {
			case <-reached:
			default:
				close(reached)
			}
			<-gate
		}
		e.hook.Store(&hook)
		defer e.hook.Store(nil)

		completion := e.async(func() response { return e.complete(revTok, item, itemETag) })
		select {
		case <-reached: // the permission check passed and holds the user's authorization revision
		case <-time.After(15 * time.Second):
			t.Fatal("the completion never reached the review-subject guard")
		}
		revocation := e.async(func() response { return e.revokeVia(adminTok, tenantA, bid, etag) })
		e.waitForLockWaiters(1) // the revocation waits for the completion's authorization lock
		notDone(t, revocation, 400*time.Millisecond, "the revocation")

		close(gate)
		c, r := get(t, completion, "the completion"), get(t, revocation, "the revocation")
		if c.r.code != 200 || e.itemState(item) != "done" {
			t.Fatalf("the in-flight completion: %d %s", c.r.code, c.r.body)
		}
		if r.r.code != 204 {
			t.Fatalf("revocation: %d %s", r.r.code, r.r.body)
		}
		// After the revocation returned, the same token can complete nothing more.
		another, anotherETag := e.itemInReview(engTok, tenantA, projectA, "after")
		if resp := e.complete(revTok, another, anotherETag); resp.code != 403 {
			t.Fatalf("after the revocation returned: %d %s", resp.code, resp.body)
		}
	})

	t.Run("project-scoped revocation serializes behind the completion without deadlock", func(t *testing.T) {
		bid, etag := e.grantVia(adminTok, tenantA, e.users["reviewer"], projectA, "reviewer")
		item, itemETag := e.itemInReview(engTok, tenantA, projectA, "project scoped")
		release := e.holdItem(item)

		completion := e.async(func() response { return e.complete(revTok, item, itemETag) })
		e.waitForLockWaiters(1)
		revocation := e.async(func() response { return e.revokeVia(adminTok, tenantA, bid, etag) })
		e.waitForLockWaiters(2) // the revocation queues on the project row the completion holds
		notDone(t, revocation, 300*time.Millisecond, "the revocation")
		release()

		c, r := get(t, completion, "the completion"), get(t, revocation, "the revocation")
		if c.r.code != 200 || r.r.code != 204 {
			t.Fatalf("completion %d %s / revocation %d %s", c.r.code, c.r.body, r.r.code, r.r.body)
		}
		if !c.done.Before(r.done.Add(time.Second)) || e.itemState(item) != "done" {
			t.Fatalf("unexpected ordering")
		}
	})

	t.Run("the same two operations interleaved many times never leave a completion without authority", func(t *testing.T) {
		for round := 0; round < 4; round++ {
			bid, etag := e.grantVia(adminTok, tenantA, e.users["reviewer"], "", "reviewer")
			item, itemETag := e.itemInReview(engTok, tenantA, projectA, "race")
			completion := e.async(func() response { return e.complete(revTok, item, itemETag) })
			revocation := e.async(func() response { return e.revokeVia(adminTok, tenantA, bid, etag) })
			c, r := get(t, completion, "the completion"), get(t, revocation, "the revocation")
			if r.r.code != 204 {
				t.Fatalf("revocation: %d %s", r.r.code, r.r.body)
			}
			switch c.r.code {
			case 200: // the completion committed first; the revocation then returned
				if e.itemState(item) != "done" {
					t.Fatal("200 without a completed item")
				}
			case 403:
				if e.itemState(item) != "review" {
					t.Fatal("403 with a changed item")
				}
			default:
				t.Fatalf("round %d: %d %s", round, c.r.code, c.r.body)
			}
			// Once both returned the reviewer has no authority left.
			if err := e.gateAllowed(e.users["reviewer"]); err == nil {
				t.Fatal("authority survived the revocation")
			}
		}
	})
}

var (
	_ = http.StatusOK
	_ = domain.TenantID("")
)
