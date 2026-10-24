package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/workspace"
)

func memIdentity(t *testing.T, key string, body byte) httpapi.RequestIdentity {
	t.Helper()
	id := httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: "u_1"}, TenantID: "t_acme", ProjectID: "p_inv", Method: http.MethodPost,
		Route: "/api/v1/tenants/t_acme/projects/p_inv/runs", Key: httpapi.ClientKey(key),
	}
	id.Hash[0] = body
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	return id
}

func okOutcome(body string) httpapi.Outcome {
	return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 201, Body: []byte(body)}, Target: httpapi.Reference{Kind: "run", ID: "run_1"}}
}

// businessRow stands for the business state of a keyed mutation: written in the
// same transaction as the record, so the tests see exactly what survives.
type businessRow struct{ ID string }

var businessT = memdb.NewTable("app_test_business", func(r businessRow) string { return r.ID })

const testTenant domain.TenantID = "t_acme"

func countRows(t *testing.T, db *memdb.Store) int {
	t.Helper()
	n := 0
	if err := db.View(context.Background(), testTenant, func(tx memdb.Tx) error {
		businessT.Scan(tx, func(businessRow) bool { n++; return true })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func hasRecord(t *testing.T, db *memdb.Store, id httpapi.RequestIdentity) bool {
	t.Helper()
	found := false
	if err := db.View(context.Background(), id.TenantID, func(tx memdb.Tx) error {
		_, found = idempotencyT.Get(tx, lookupKey(id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

func allowReplay(context.Context, httpapi.Reference) error { return nil }

func TestMemoryIdempotencyRunsOncePerKey(t *testing.T) {
	ctx := context.Background()
	db := memdb.New()
	m := newMemIdempotency(nil)
	var calls, replays atomic.Int32
	do := func(_ context.Context, tx memdb.Tx) (httpapi.Outcome, error) {
		calls.Add(1)
		return okOutcome(`{"id":"run_1"}`), businessT.Put(tx, businessRow{ID: "row_" + string(rune('0'+calls.Load()))})
	}
	allow := func(_ context.Context, ref httpapi.Reference) error {
		replays.Add(1)
		if ref.ID != "run_1" {
			t.Errorf("the replay is authorized against %+v", ref)
		}
		return nil
	}
	first, replayed, err := m.executeTx(ctx, db, memIdentity(t, "k1", 1), allow, do)
	if err != nil || replayed || calls.Load() != 1 {
		t.Fatalf("first: %v replayed=%v calls=%d", err, replayed, calls.Load())
	}
	second, replayed, err := m.executeTx(ctx, db, memIdentity(t, "k1", 1), allow, do)
	if err != nil || !replayed || calls.Load() != 1 || string(second.Body) != string(first.Body) || replays.Load() != 1 {
		t.Fatalf("replay: %v replayed=%v calls=%d", err, replayed, calls.Load())
	}
	// The same key with another request is the stable conflict and runs nothing.
	_, _, err = m.executeTx(ctx, db, memIdentity(t, "k1", 2), allow, do)
	var api *httpapi.APIError
	if !errors.As(err, &api) || api.Status != http.StatusConflict || calls.Load() != 1 {
		t.Fatalf("conflict: %v calls=%d", err, calls.Load())
	}
	// The key is scoped by principal.
	other := memIdentity(t, "k1", 1)
	other.Principal.ID = "u_2"
	if _, replayed, err := m.executeTx(ctx, db, other, allow, do); err != nil || replayed || calls.Load() != 2 {
		t.Fatalf("another principal shared the key: %v replayed=%v calls=%d", err, replayed, calls.Load())
	}
	if countRows(t, db) != 2 {
		t.Fatalf("business rows: %d", countRows(t, db))
	}
}

// A response that cannot be stored aborts the mutation that produced it, and a
// commit that fails leaves neither the mutation nor a record: the retry with the
// same key executes exactly once and is then replayed.
func TestMemoryIdempotencyIsJoinedToTheBusinessTransaction(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	db := memdb.New(memdb.WithCommitFault(func() error {
		if fail.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	m := newMemIdempotency(nil)
	var calls atomic.Int32
	write := func(out httpapi.Outcome) func(context.Context, memdb.Tx) (httpapi.Outcome, error) {
		return func(_ context.Context, tx memdb.Tx) (httpapi.Outcome, error) {
			calls.Add(1)
			return out, businessT.Put(tx, businessRow{ID: "row"})
		}
	}
	good := write(okOutcome(`{"id":"run_1"}`))

	for name, out := range map[string]httpapi.Outcome{
		"not JSON":        {Response: httpapi.StoredResponse{Status: 201, Body: []byte("plain")}},
		"oversized":       {Response: httpapi.StoredResponse{Status: 201, Body: []byte(`{"x":"` + strings.Repeat("a", httpapi.DefaultMaxStoredBodyBytes) + `"}`)}},
		"not a success":   {Response: httpapi.StoredResponse{Status: 500, Body: []byte(`{}`)}},
		"unstorable head": {Response: httpapi.StoredResponse{Status: 201, Body: []byte(`{}`), Header: map[string]string{"Set-Cookie": "x"}}},
	} {
		id := memIdentity(t, "k-"+strings.ReplaceAll(name, " ", "-"), 1)
		if _, _, err := m.executeTx(ctx, db, id, allowReplay, write(out)); err == nil {
			t.Fatalf("%s: an unstorable response was accepted", name)
		}
		if countRows(t, db) != 0 || hasRecord(t, db, id) {
			t.Fatalf("%s: the mutation or a record survived (rows=%d record=%v)", name, countRows(t, db), hasRecord(t, db, id))
		}
	}

	id := memIdentity(t, "k-commit", 1)
	fail.Store(true)
	_, _, err := m.executeTx(ctx, db, id, allowReplay, good)
	var api *httpapi.APIError
	if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable {
		t.Fatalf("a failed commit must be an unavailable outcome: %v", err)
	}
	if countRows(t, db) != 0 || hasRecord(t, db, id) {
		t.Fatalf("a failed commit left rows=%d record=%v", countRows(t, db), hasRecord(t, db, id))
	}
	fail.Store(false)
	before := calls.Load()
	if _, replayed, err := m.executeTx(ctx, db, id, allowReplay, good); err != nil || replayed || calls.Load() != before+1 {
		t.Fatalf("retry: %v replayed=%v calls=%d", err, replayed, calls.Load()-before)
	}
	if _, replayed, err := m.executeTx(ctx, db, id, allowReplay, good); err != nil || !replayed || calls.Load() != before+1 {
		t.Fatalf("replay: %v replayed=%v", err, replayed)
	}
	if countRows(t, db) != 1 || !hasRecord(t, db, id) {
		t.Fatalf("rows=%d record=%v", countRows(t, db), hasRecord(t, db, id))
	}
}

func TestMemoryIdempotencyReplayIsReauthorizedAndErrorsStoreNothing(t *testing.T) {
	ctx := context.Background()
	db := memdb.New()
	m := newMemIdempotency(nil)
	boom := errors.New("boom")
	var calls int
	failing := func(_ context.Context, tx memdb.Tx) (httpapi.Outcome, error) {
		calls++
		_ = businessT.Put(tx, businessRow{ID: "partial"})
		return httpapi.Outcome{}, boom
	}
	good := func(context.Context, memdb.Tx) (httpapi.Outcome, error) { calls++; return okOutcome(`{}`), nil }
	if _, _, err := m.executeTx(ctx, db, memIdentity(t, "k2", 1), allowReplay, failing); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if countRows(t, db) != 0 {
		t.Fatal("a failed callback left a row")
	}
	// A failed callback leaves the key absent: the retry runs the callback again.
	if _, replayed, err := m.executeTx(ctx, db, memIdentity(t, "k2", 1), allowReplay, good); err != nil || replayed || calls != 2 {
		t.Fatalf("retry: %v replayed=%v calls=%d", err, replayed, calls)
	}
	// A revoked caller does not get the stored response back.
	denied := func(context.Context, httpapi.Reference) error { return httpapi.ForbiddenError() }
	if _, _, err := m.executeTx(ctx, db, memIdentity(t, "k2", 1), denied, good); err == nil || calls != 2 {
		t.Fatalf("a replay skipped the current permission check: %v calls=%d", err, calls)
	}
	// An expired record is replaced, not replayed.
	now := time.Now()
	m.now = func() time.Time { return now.Add(48 * time.Hour) }
	if _, replayed, err := m.executeTx(ctx, db, memIdentity(t, "k2", 1), allowReplay, good); err != nil || replayed || calls != 3 {
		t.Fatalf("expired: %v replayed=%v calls=%d", err, replayed, calls)
	}
	if _, _, err := m.executeTx(ctx, db, httpapi.RequestIdentity{}, allowReplay, good); err == nil {
		t.Fatal("an invalid identity was accepted")
	}
	if _, _, err := m.executeTx(ctx, db, memIdentity(t, "k4", 1), nil, good); err == nil {
		t.Fatal("a missing replay authorizer was accepted")
	}
}

// The unit form (a store that opens its own update) records inside the unit
// and withdraws the record when the unit does not commit.
func TestMemoryIdempotencyUnitFormAbortsAndWithdraws(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	db := memdb.New(memdb.WithCommitFault(func() error {
		if fail.Load() {
			return errors.New("commit refused")
		}
		return nil
	}))
	m := newMemIdempotency(nil)
	var calls atomic.Int32
	unit := func(out httpapi.Outcome) func(context.Context, func(httpapi.Outcome) error) error {
		return func(ctx context.Context, record func(httpapi.Outcome) error) error {
			calls.Add(1)
			return db.Update(ctx, testTenant, func(tx memdb.Tx) error {
				if err := businessT.Put(tx, businessRow{ID: "row"}); err != nil {
					return err
				}
				return record(out)
			})
		}
	}
	id := memIdentity(t, "u1", 1)
	// An unstorable response aborts the write: no row, no record.
	if _, _, err := m.executeUnit(ctx, id, allowReplay, unit(httpapi.Outcome{Response: httpapi.StoredResponse{Status: 201, Body: []byte("plain")}})); err == nil {
		t.Fatal("an unstorable response was accepted")
	}
	if countRows(t, db) != 0 || len(m.records) != 0 {
		t.Fatalf("rows=%d records=%d", countRows(t, db), len(m.records))
	}
	// A failed commit withdraws the record.
	fail.Store(true)
	if _, _, err := m.executeUnit(ctx, id, allowReplay, unit(okOutcome(`{}`))); err == nil {
		t.Fatal("a failed commit was acknowledged")
	}
	if countRows(t, db) != 0 || len(m.records) != 0 {
		t.Fatalf("a failed commit left rows=%d records=%d", countRows(t, db), len(m.records))
	}
	fail.Store(false)
	before := calls.Load()
	if _, replayed, err := m.executeUnit(ctx, id, allowReplay, unit(okOutcome(`{}`))); err != nil || replayed || calls.Load() != before+1 {
		t.Fatalf("retry: %v replayed=%v", err, replayed)
	}
	if _, replayed, err := m.executeUnit(ctx, id, allowReplay, unit(okOutcome(`{}`))); err != nil || !replayed || calls.Load() != before+1 {
		t.Fatalf("replay: %v replayed=%v", err, replayed)
	}
	conflict := memIdentity(t, "u1", 2)
	var api *httpapi.APIError
	if _, _, err := m.executeUnit(ctx, conflict, allowReplay, unit(okOutcome(`{}`))); !errors.As(err, &api) || api.Status != http.StatusConflict {
		t.Fatalf("conflict: %v", err)
	}
}

type recordingGit struct{ urls []string }

func (g *recordingGit) ResolveRef(_ context.Context, r workspace.RepoRef, _ string) (workspace.Commit, bool, error) {
	g.urls = append(g.urls, r.URL)
	return "", false, nil
}
func (g *recordingGit) BranchContains(_ context.Context, r workspace.RepoRef, _ string, _ workspace.Commit) (workspace.BranchInfo, error) {
	g.urls = append(g.urls, r.URL)
	return workspace.BranchInfo{}, nil
}
func (g *recordingGit) Prepare(_ context.Context, r workspace.PrepareRequest) (workspace.Prepared, error) {
	g.urls = append(g.urls, r.Repo.URL)
	return nil, nil
}
func (g *recordingGit) Publish(_ context.Context, r workspace.RepoRef, _ workspace.PublishRequest) error {
	g.urls = append(g.urls, r.URL)
	return nil
}
func (g *recordingGit) CreateRef(_ context.Context, r workspace.RepoRef, _ string, _ workspace.Commit, _ string) error {
	g.urls = append(g.urls, r.URL)
	return nil
}
func (g *recordingGit) DeleteRef(_ context.Context, r workspace.RepoRef, _ string, _ workspace.Commit) error {
	g.urls = append(g.urls, r.URL)
	return nil
}

func TestMemoryGitServesOnlyMappedRepositories(t *testing.T) {
	ctx := context.Background()
	inner := &recordingGit{}
	reader, preparer, publisher, err := NewMemoryGit(inner, inner, inner, map[string]string{"https://git.development.invalid/a.git": "/tmp/a"})
	if err != nil {
		t.Fatal(err)
	}
	known := workspace.RepoRef{URL: "https://git.development.invalid/a.git"}
	unknown := workspace.RepoRef{URL: "/etc"}
	if _, _, err := reader.ResolveRef(ctx, known, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.BranchContains(ctx, known, "main", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.Prepare(ctx, workspace.PrepareRequest{Repo: known}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { return publisher.Publish(ctx, known, workspace.PublishRequest{}) },
		func() error { return publisher.CreateRef(ctx, known, "refs/x", "", "main") },
		func() error { return publisher.DeleteRef(ctx, known, "refs/x", "") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range inner.urls {
		if u != "/tmp/a" {
			t.Fatalf("the adapter was called with %q", u)
		}
	}
	if len(inner.urls) != 6 {
		t.Fatalf("calls %v", inner.urls)
	}
	// A path, or any URL the composition did not map, never reaches Git.
	before := len(inner.urls)
	if _, _, err := reader.ResolveRef(ctx, unknown, "main"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("unmapped read: %v", err)
	}
	if _, err := preparer.Prepare(ctx, workspace.PrepareRequest{Repo: unknown}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("unmapped prepare: %v", err)
	}
	if err := publisher.Publish(ctx, unknown, workspace.PublishRequest{}); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("unmapped publish: %v", err)
	}
	if len(inner.urls) != before {
		t.Fatal("an unmapped repository reached Git")
	}
	if _, _, _, err := NewMemoryGit(inner, inner, inner, nil); err == nil {
		t.Fatal("an empty mapping was accepted")
	}
	for _, a := range []any{reader, preparer, publisher} {
		if _, err := Admit(Production, PortSource, a); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("production accepted %T", a)
		}
	}
}

type stubAuthority struct{ err error }

func (a stubAuthority) Authorize(context.Context, identity.Actor, identity.Target, identity.Permission) error {
	return a.err
}

func TestMemoryTransactionAuthorityMapsIdentityOutcomes(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	actor := execution.Actor{ID: "u_1", Type: execution.ActorUser}
	for name, tc := range map[string]struct {
		err  error
		perm execution.Permission
		want error
	}{
		"allowed":      {nil, execution.PermRunManage, nil},
		"hidden":       {identity.ErrHidden, execution.PermRunRead, execution.ErrHidden},
		"denied":       {identity.ErrDenied, execution.PermRunManage, execution.ErrDenied},
		"outage":       {errors.New("down"), execution.PermRunRead, execution.ErrUnavailable},
		"unknown perm": {nil, execution.Permission("run:erase"), execution.ErrDenied},
	} {
		err := memTxAuthority{authority: stubAuthority{tc.err}}.Authorize(context.Background(), actor, scope, tc.perm)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

// A same-key retry that arrives while the first request is paused before its
// commit must not be answered from the first request's uncommitted state. The
// first request's commit then fails: the retry runs afresh, and no caller
// received a successful replay of rolled-back state.
func TestMemoryIdempotencyUnitFormNeverReplaysUncommittedState(t *testing.T) {
	ctx := context.Background()
	var failFirstCommit atomic.Bool
	failFirstCommit.Store(true)
	db := memdb.New(memdb.WithCommitFault(func() error {
		if failFirstCommit.CompareAndSwap(true, false) {
			return errors.New("commit refused")
		}
		return nil
	}))
	m := newMemIdempotency(nil)
	id := memIdentity(t, "u-race", 1)
	paused, resume := make(chan struct{}), make(chan struct{})
	var secondRuns atomic.Int32

	type result struct {
		resp     httpapi.StoredResponse
		replayed bool
		err      error
	}
	first := make(chan result, 1)
	go func() {
		resp, replayed, err := m.executeUnit(ctx, id, allowReplay, func(ctx context.Context, record func(httpapi.Outcome) error) error {
			return db.Update(ctx, testTenant, func(tx memdb.Tx) error {
				if err := businessT.Put(tx, businessRow{ID: "row"}); err != nil {
					return err
				}
				if err := record(okOutcome(`{"id":"first"}`)); err != nil {
					return err
				}
				close(paused) // the record exists, the commit has not happened
				<-resume
				return nil
			})
		})
		first <- result{resp, replayed, err}
	}()
	<-paused

	second := make(chan result, 1)
	go func() {
		resp, replayed, err := m.executeUnit(ctx, id, allowReplay, func(ctx context.Context, record func(httpapi.Outcome) error) error {
			secondRuns.Add(1)
			return db.Update(ctx, testTenant, func(tx memdb.Tx) error {
				if err := businessT.Put(tx, businessRow{ID: "row"}); err != nil {
					return err
				}
				return record(okOutcome(`{"id":"second"}`))
			})
		})
		second <- result{resp, replayed, err}
	}()
	select {
	case r := <-second:
		t.Fatalf("the retry was answered while the first request was uncommitted: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	if secondRuns.Load() != 0 {
		t.Fatal("the retry ran its business effects concurrently with the first")
	}

	close(resume)
	r1 := <-first
	if r1.err == nil {
		t.Fatal("the first request was acknowledged although its commit failed")
	}
	r2 := <-second
	if r2.err != nil || r2.replayed || string(r2.resp.Body) != `{"id":"second"}` || secondRuns.Load() != 1 {
		t.Fatalf("the retry after the rollback: %+v runs=%d", r2, secondRuns.Load())
	}
	if countRows(t, db) != 1 {
		t.Fatalf("rows: %d", countRows(t, db))
	}
	// And now the committed response is the one replayed.
	if resp, replayed, err := m.executeUnit(ctx, id, allowReplay, func(context.Context, func(httpapi.Outcome) error) error {
		t.Error("a committed key ran again")
		return nil
	}); err != nil || !replayed || string(resp.Body) != `{"id":"second"}` {
		t.Fatalf("replay: %v %v %s", err, replayed, resp.Body)
	}

	// A waiting request honours its context.
	hold, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _, _ = m.executeUnit(ctx, memIdentity(t, "u-hold", 1), allowReplay, func(context.Context, func(httpapi.Outcome) error) error {
			close(hold)
			<-release
			return nil
		})
	}()
	<-hold
	waiting, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	var api *httpapi.APIError
	if _, _, err := m.executeUnit(waiting, memIdentity(t, "u-wait", 1), allowReplay, func(context.Context, func(httpapi.Outcome) error) error { return nil }); !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable {
		t.Fatalf("a cancelled waiter: %v", err)
	}
	close(release)
}
