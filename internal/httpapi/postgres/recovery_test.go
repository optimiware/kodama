package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	httppg "github.com/optimiware/kodama/internal/httpapi/postgres"
)

func identity(principal, tenant, project, key, body string) httpapi.RequestIdentity {
	return httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: principal},
		TenantID:  domain.TenantID(tenant), ProjectID: domain.ProjectID(project),
		Method: "POST", Route: "/api/v1/tenants/" + tenant + "/projects/" + project + "/widgets",
		Key: httpapi.ClientKey(key), Hash: sha256.Sum256([]byte(body)),
	}
}

func allowReplay(context.Context, httpapi.Reference) error { return nil }

// insertWidget writes the fixture rows the way a consuming module would.
func insertWidget(ctx context.Context, tx pgx.Tx, tenant, project, name string) (string, error) {
	id := newID()
	if _, err := tx.Exec(ctx, `INSERT INTO t004_widgets (tenant_id, id, project_id, name) VALUES ($1, $2, $3, $4)`, tenant, id, project, name); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `INSERT INTO t004_widget_history (tenant_id, widget_id, version, name) VALUES ($1, $2, 1, $3)`, tenant, id, name)
	return id, err
}

func okOutcome(id string) httpapi.Outcome {
	return httpapi.Outcome{
		Response: httpapi.StoredResponse{Status: 201, Header: map[string]string{"ETag": `"1"`}, Body: []byte(`{"id":"` + id + `"}`)},
		Target:   httpapi.Reference{Kind: "widget", ID: id},
	}
}

func (e *env) assertNoEffects(t *testing.T, why string) {
	t.Helper()
	if e.widgets(t) != 0 || e.history(t) != 0 || e.records(t) != 0 {
		t.Fatalf("%s left effects: widgets=%d history=%d records=%d", why, e.widgets(t), e.history(t), e.records(t))
	}
}

func Test_idempotency_rollback_and_retry(t *testing.T) {
	retry := func(t *testing.T, e *env, id httpapi.RequestIdentity) {
		t.Helper()
		var calls int
		res, err := e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
				calls++
				wid, err := insertWidget(ctx, tx, string(id.TenantID), string(id.ProjectID), "retry")
				return okOutcome(wid), err
			}})
		if err != nil || res.Replayed || res.Response.Status != 201 || calls != 1 {
			t.Fatalf("retry: %+v, %v, calls=%d", res, err, calls)
		}
		if e.widgets(t) != 1 || e.history(t) != 1 || e.records(t) != 1 {
			t.Fatalf("retry effects: widgets=%d history=%d records=%d", e.widgets(t), e.history(t), e.records(t))
		}
	}

	t.Run("callback error", func(t *testing.T) {
		e := newEnv(t)
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		want := &domain.Error{Code: domain.CodeNotFound, Message: "gone"}
		_, err := e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
				if _, err := insertWidget(ctx, tx, "tenant-a", "pa1", "doomed"); err != nil {
					return httpapi.Outcome{}, err
				}
				return httpapi.Outcome{}, want
			}})
		if !errors.Is(err, want) {
			t.Fatalf("err = %v", err)
		}
		if httpapi.ResolveError(err).Status != 404 {
			t.Fatalf("callback's own error was altered: %v", err)
		}
		e.assertNoEffects(t, "callback error")
		retry(t, e, id)
	})

	t.Run("SQL failure inside the callback", func(t *testing.T) {
		e := newEnv(t)
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		_, err := e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
				if _, err := insertWidget(ctx, tx, "tenant-a", "pa1", "doomed"); err != nil {
					return httpapi.Outcome{}, err
				}
				// A foreign-key violation: the project belongs to another tenant.
				_, err := insertWidget(ctx, tx, "tenant-a", "pb1", "cross-owner")
				return httpapi.Outcome{}, err
			}})
		if err == nil {
			t.Fatal("cross-owner write accepted")
		}
		e.assertNoEffects(t, "SQL failure")
		retry(t, e, id)
	})

	t.Run("response that cannot be stored", func(t *testing.T) {
		e := newEnv(t, func(c *httppg.Config) { c.MaxStoredBodyBytes = 64 })
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		bad := map[string]httpapi.StoredResponse{
			"oversized":  {Status: 201, Body: []byte(`{"v":"` + strings.Repeat("x", 200) + `"}`)},
			"not JSON":   {Status: 201, Body: []byte(`<html>`)},
			"truncated":  {Status: 201, Body: []byte(`{"id":"x"`)},
			"lone brace": {Status: 201, Body: []byte(`{`)},
			"trailing":   {Status: 201, Body: []byte(`{} trailing`)},
			"two values": {Status: 201, Body: []byte(`{"a":1}{"b":2}`)},
			"scalar":     {Status: 201, Body: []byte(`"x"`)},
			"error code": {Status: 500, Body: []byte(`{}`)},
			"set-cookie": {Status: 201, Header: map[string]string{"Set-Cookie": "a=b"}, Body: []byte(`{}`)},
		}
		for name, resp := range bad {
			_, err := e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
				Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
					if _, err := insertWidget(ctx, tx, "tenant-a", "pa1", "doomed"); err != nil {
						return httpapi.Outcome{}, err
					}
					return httpapi.Outcome{Response: resp}, nil
				}})
			if err == nil || httpapi.ResolveError(err).Status != 500 {
				t.Fatalf("%s: err = %v", name, err)
			}
			e.assertNoEffects(t, name)
		}
		retry(t, e, id)
	})

	t.Run("panic", func(t *testing.T) {
		e := newEnv(t)
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("panic was swallowed")
				}
			}()
			_, _ = e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
				Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
					_, _ = insertWidget(ctx, tx, "tenant-a", "pa1", "doomed")
					panic("callback exploded")
				}})
		}()
		e.assertNoEffects(t, "panic")
		retry(t, e, id)
	})

	t.Run("panic through the router becomes a safe 500 and a clean retry", func(t *testing.T) {
		e := newEnv(t)
		var explode atomic.Bool
		explode.Store(true)
		e.setHook(func(ctx context.Context, tx pgx.Tx) error {
			if explode.Load() {
				panic("secret panic text")
			}
			return nil
		})
		r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"x"}`)
		if r.Code != 500 || strings.Contains(r.Body, "secret panic text") {
			t.Fatalf("%d %s", r.Code, r.Body)
		}
		e.assertNoEffects(t, "router panic")
		explode.Store(false)
		if r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"x"}`); r.Code != 201 || r.replayed() {
			t.Fatalf("retry: %d %s", r.Code, r.Body)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		e := newEnv(t)
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		ctx, cancel := context.WithCancel(testCtx(t))
		_, err := e.idem.Execute(ctx, httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
				if _, err := insertWidget(ctx, tx, "tenant-a", "pa1", "doomed"); err != nil {
					return httpapi.Outcome{}, err
				}
				cancel() // the client disconnects mid-transaction
				_, err := tx.Exec(ctx, `SELECT 1`)
				if err == nil {
					err = ctx.Err()
				}
				return httpapi.Outcome{}, err
			}})
		if err == nil {
			t.Fatal("cancelled request reported success")
		}
		e.assertNoEffects(t, "cancellation")
		retry(t, e, id)
	})

	t.Run("cancellation while waiting for the claim", func(t *testing.T) {
		e := newEnv(t)
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		release, entered := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := e.idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
				Do: func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
					close(entered)
					<-release
					wid, err := insertWidget(ctx, tx, "tenant-a", "pa1", "winner")
					return okOutcome(wid), err
				}})
			done <- err
		}()
		<-entered
		ctx, cancel := context.WithCancel(testCtx(t))
		go cancel()
		_, err := e.idem.Execute(ctx, httppg.Request{Identity: id, AuthorizeReplay: allowReplay,
			Do: func(context.Context, pgx.Tx) (httpapi.Outcome, error) {
				t.Error("waiter executed the callback")
				return httpapi.Outcome{}, nil
			}})
		if err == nil || httpapi.ResolveError(err).Status != 503 {
			t.Fatalf("waiter err = %v", err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if e.widgets(t) != 1 || e.records(t) != 1 {
			t.Fatalf("widgets=%d records=%d", e.widgets(t), e.records(t))
		}
	})

	t.Run("invalid configuration and identity never execute", func(t *testing.T) {
		e := newEnv(t)
		ran := false
		do := func(context.Context, pgx.Tx) (httpapi.Outcome, error) { ran = true; return httpapi.Outcome{}, nil }
		id := identity("alice", "tenant-a", "pa1", "k", "b")
		bad := id
		bad.Key = "has space"
		for name, req := range map[string]httppg.Request{
			"no replay authorizer": {Identity: id, Do: do},
			"no callback":          {Identity: id, AuthorizeReplay: allowReplay},
			"bad identity":         {Identity: bad, AuthorizeReplay: allowReplay, Do: do},
		} {
			if _, err := e.idem.Execute(testCtx(t), req); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if ran {
			t.Fatal("callback ran")
		}
		if _, err := httppg.NewIdempotency(httppg.Config{}); err == nil {
			t.Error("missing database accepted")
		}
		var nilIdem *httppg.Idempotency
		if _, err := nilIdem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay, Do: do}); err == nil {
			t.Error("nil adapter executed")
		}
	})
}

// faultDB wraps the runtime pool so commit can fail in controlled ways.
type faultDB struct {
	inner *pgxpool.Pool
	mode  atomic.Value // string
}

const (
	commitOK          = ""
	commitLostAck     = "commit-then-error" // the commit is durable but the client sees an error
	commitFailsBefore = "error-without-commit"
)

func (f *faultDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := f.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	mode, _ := f.mode.Load().(string)
	return &faultTx{Tx: tx, mode: mode}, nil
}

type faultTx struct {
	pgx.Tx
	mode string
}

var errConnectionLost = errors.New("injected: connection lost during commit")

func (f *faultTx) Commit(ctx context.Context) error {
	switch f.mode {
	case commitLostAck:
		if err := f.Tx.Commit(ctx); err != nil {
			return err
		}
		return errConnectionLost
	case commitFailsBefore:
		_ = f.Tx.Rollback(ctx)
		return errConnectionLost
	}
	return f.Tx.Commit(ctx)
}

// droppingWriter is an http.ResponseWriter whose peer has gone away: nothing is
// delivered to any client.
type droppingWriter struct{ h http.Header }

func (d *droppingWriter) Header() http.Header       { return d.h }
func (d *droppingWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func (d *droppingWriter) WriteHeader(int)           {}

func Test_idempotency_lost_response(t *testing.T) {
	t.Run("a dropped HTTP response is replayed without duplicate execution", func(t *testing.T) {
		e := newEnv(t)
		req := httptest.NewRequest("POST", createA1, strings.NewReader(`{"name":"lost"}`))
		req.Header.Set("Authorization", "Bearer alice")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "k")
		e.route.ServeHTTP(&droppingWriter{h: http.Header{}}, req) // commit happened; client never saw it

		if e.widgets(t) != 1 || e.records(t) != 1 {
			t.Fatalf("widgets=%d records=%d", e.widgets(t), e.records(t))
		}
		r := e.create("alice", "tenant-a", "pa1", "k", `{"name":"lost"}`)
		if r.Code != 201 || !r.replayed() || e.doCalls.Load() != 1 || e.widgets(t) != 1 {
			t.Fatalf("retry: %d replayed=%v calls=%d widgets=%d %s", r.Code, r.replayed(), e.doCalls.Load(), e.widgets(t), r.Body)
		}
	})

	t.Run("commit acknowledgement lost: unknown outcome, never success, never rerun", func(t *testing.T) {
		e := newEnv(t)
		db := &faultDB{inner: e.rt}
		db.mode.Store(commitLostAck)
		idem, err := httppg.NewIdempotency(httppg.Config{DB: db})
		if err != nil {
			t.Fatal(err)
		}
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		var calls int
		do := func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			calls++
			wid, err := insertWidget(ctx, tx, "tenant-a", "pa1", "durable")
			return okOutcome(wid), err
		}
		res, err := idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay, Do: do})
		api := httpapi.ResolveError(err)
		if err == nil || res.Response.Status != 0 || api.Status != 503 || api.Header.Get("Retry-After") == "" || !errors.Is(err, httppg.ErrOutcomeUnknown) {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if strings.Contains(api.Message, "injected") {
			t.Fatal("client message leaks the cause")
		}
		if calls != 1 {
			t.Fatalf("callback reran automatically: %d", calls)
		}
		// The commit was in fact durable; the retry replays it.
		db.mode.Store(commitOK)
		res, err = idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay, Do: do})
		if err != nil || !res.Replayed || calls != 1 || e.widgets(t) != 1 {
			t.Fatalf("retry: %+v %v calls=%d widgets=%d", res, err, calls, e.widgets(t))
		}
	})

	t.Run("commit failed before taking effect: retry executes once", func(t *testing.T) {
		e := newEnv(t)
		db := &faultDB{inner: e.rt}
		db.mode.Store(commitFailsBefore)
		idem, _ := httppg.NewIdempotency(httppg.Config{DB: db})
		id := identity("alice", "tenant-a", "pa1", "k", "body")
		var calls int
		do := func(ctx context.Context, tx pgx.Tx) (httpapi.Outcome, error) {
			calls++
			wid, err := insertWidget(ctx, tx, "tenant-a", "pa1", "once")
			return okOutcome(wid), err
		}
		if _, err := idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay, Do: do}); err == nil || !errors.Is(err, httppg.ErrOutcomeUnknown) {
			t.Fatalf("err = %v", err)
		}
		e.assertNoEffects(t, "failed commit")
		db.mode.Store(commitOK)
		res, err := idem.Execute(testCtx(t), httppg.Request{Identity: id, AuthorizeReplay: allowReplay, Do: do})
		if err != nil || res.Replayed || calls != 2 || e.widgets(t) != 1 || e.records(t) != 1 {
			t.Fatalf("retry: %+v %v calls=%d widgets=%d", res, err, calls, e.widgets(t))
		}
	})

	t.Run("an unavailable database is a 503 that executes nothing", func(t *testing.T) {
		e := newEnv(t)
		closed := e.db.RuntimePool(testCtx(t), 1)
		closed.Close()
		idem, _ := httppg.NewIdempotency(httppg.Config{DB: closed})
		_, err := idem.Execute(testCtx(t), httppg.Request{Identity: identity("alice", "tenant-a", "pa1", "k", "b"), AuthorizeReplay: allowReplay,
			Do: func(context.Context, pgx.Tx) (httpapi.Outcome, error) {
				t.Error("callback ran without a database")
				return httpapi.Outcome{}, nil
			}})
		if api := httpapi.ResolveError(err); api.Status != 503 || strings.Contains(api.Message, "pool") {
			t.Fatalf("err = %v", err)
		}
	})
}
