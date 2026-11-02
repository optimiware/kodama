package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/outbox"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/storage"
)

func readyRepo(t *testing.T, s *store.Store, tenant domain.TenantID, project domain.ProjectID, id, name string) store.Repository {
	t.Helper()
	ctx := t.Context()
	newRepo(t, s, ctx, tenant, project, id, name)
	r, err := s.ConfirmInitialized(ctx, tenant, id, "main", true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func plannerOf(t *testing.T, keys ...string) store.Planner {
	t.Helper()
	return func(ps []store.Pipeline) ([]store.RunAdmission, error) {
		var out []store.RunAdmission
		for _, p := range ps {
			for _, k := range keys {
				id, err := store.NewID("run")
				if err != nil {
					return nil, err
				}
				out = append(out, store.RunAdmission{PipelineID: p.ID, TriggerKey: k, Sources: json.RawMessage(`[{"repositoryId":"repo_1","commit":"` + strings.Repeat("a", 40) + `","path":".","access":"read","branch":null}]`),
					Run:   store.Run{ID: id, Definition: p.Definition, ExecutionOrder: []string{"test"}},
					Steps: []store.Step{{ID: "test", StepKey: "test", Ordinal: 0, Image: "golang:1.27"}}})
			}
		}
		return out, nil
	}
}

// TestGitPostgresOutbox: the outbox lists unpublished events of one tenant in
// (next attempt, ID) order with the keys of their subject, a failure counts an
// attempt and defers the next one, a publication is recorded once, and another
// tenant's events are never visible.
func TestGitPostgresOutbox(t *testing.T) {
	s, _, ctx := gitDB(t)
	readyRepo(t, s, "t_a", "p_a1", "repo_1", "backend")
	readyRepo(t, s, "t_a", "", "repo_2", "context")
	readyRepo(t, s, "t_b", "p_b1", "repo_3", "backend")

	now := time.Now().Add(time.Minute)
	pending, err := s.PendingEvents(ctx, "t_a", now, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	byResource := map[string]store.OutboxEvent{}
	for _, e := range pending {
		byResource[e.ResourceID] = e
		if e.TenantKey != "a" || e.Type != "repository.created" || e.Attempts != 0 || len(e.Payload) == 0 {
			t.Fatalf("outbox event = %+v", e)
		}
	}
	if byResource["repo_1"].ProjectKey != "erp" || byResource["repo_1"].ProjectID != "p_a1" || byResource["repo_2"].ProjectKey != "" || byResource["repo_2"].ProjectID != "" {
		t.Fatalf("subject keys = %+v", byResource)
	}
	other, err := s.PendingEvents(ctx, "t_b", now, 10)
	if err != nil || len(other) != 1 || other[0].TenantKey != "b" {
		t.Fatalf("another tenant's outbox = %+v, %v", other, err)
	}
	// A limit bounds the page, and the order is stable.
	page, _ := s.PendingEvents(ctx, "t_a", now, 1)
	if len(page) != 1 || page[0].ID != pending[0].ID {
		t.Fatalf("page = %+v", page)
	}

	first := pending[0]
	next := now.Add(time.Hour)
	if err := s.PublishFailed(ctx, "t_a", first.ID, next); err != nil {
		t.Fatal(err)
	}
	due, _ := s.PendingEvents(ctx, "t_a", now, 10)
	if len(due) != 1 || due[0].ID == first.ID {
		t.Fatalf("a backed-off event is still due: %+v", due)
	}
	later, _ := s.PendingEvents(ctx, "t_a", next, 10)
	if len(later) != 2 {
		t.Fatalf("the deferred event never returns: %+v", later)
	}
	for _, e := range later {
		if e.ID == first.ID && e.Attempts != 1 {
			t.Fatalf("attempts = %d", e.Attempts)
		}
	}
	// A publication is recorded and final; a late failure report cannot reopen it.
	at := time.Now()
	if err := s.MarkPublished(ctx, "t_a", first.ID, at); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPublished(ctx, "t_a", first.ID, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishFailed(ctx, "t_a", first.ID, now); err != nil {
		t.Fatal(err)
	}
	left, _ := s.PendingEvents(ctx, "t_a", next.Add(time.Hour), 10)
	if len(left) != 1 || left[0].ID == first.ID {
		t.Fatalf("a published event is pending again: %+v", left)
	}
	// Another tenant's event cannot be marked through this tenant.
	if err := s.MarkPublished(ctx, "t_a", other[0].ID, at); err != nil {
		t.Fatal(err)
	}
	if still, _ := s.PendingEvents(ctx, "t_b", now, 10); len(still) != 1 {
		t.Fatal("tenant A marked tenant B's event published")
	}
}

// TestGitPostgresDispatchPush: indexing and admission are one transaction under
// the repository lock: definitions are upserted with their exact commit, a removed
// file disables its pipeline, refusals are recorded, a run is unique per
// (pipeline, trigger key), a replayed event changes nothing, a stale one leaves
// the newer index alone, and a failed plan applies nothing.
func TestGitPostgresDispatchPush(t *testing.T) {
	s, _, ctx := gitDB(t)
	readyRepo(t, s, "t_a", "p_a1", "repo_1", "backend")
	tenantLevel := readyRepo(t, s, "t_a", "", "repo_2", "context")
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	def := func(name, marker string) store.IndexedDefinition {
		return store.IndexedDefinition{Name: name, Definition: json.RawMessage(`{"name":"` + name + `","steps":[{"name":"test","image":"x","commands":["` + marker + `"]}]}`)}
	}
	dispatch := func(id string, position int64, ix *store.PushIndex, plan store.Planner) store.PushDispatchResult {
		t.Helper()
		res, err := s.DispatchPush(ctx, store.PushDispatch{TenantID: "t_a", RepositoryID: "repo_1", EventID: id, JournalPosition: position, ReceiveID: "rcv_" + id, Index: ix, Plan: plan})
		if err != nil {
			t.Fatalf("dispatch %s: %v", id, err)
		}
		return res
	}
	pipelines := func() map[string]store.Pipeline {
		list, err := s.ListPipelines(ctx, "t_a", "p_a1")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]store.Pipeline{}
		for _, p := range list {
			out[p.Name] = p
		}
		return out
	}
	runs := func() int {
		list, err := s.ListRuns(ctx, "t_a", "p_a1")
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	res := dispatch("evt_1", 10, &store.PushIndex{Ref: "refs/heads/main", Commit: c1, Present: []string{"build", "deploy", "bad"},
		Definitions: []store.IndexedDefinition{def("build", "echo 1"), def("deploy", "echo 1")},
		Refusals:    []store.IndexRefusal{{Path: ".kodama/bad.yaml", Code: "unknown_field", DocumentPath: "x"}}}, plannerOf(t, "k1"))
	if len(res.Indexed) != 2 || res.Refused != 1 || len(res.Runs) != 2 || res.Replayed || res.Stale {
		t.Fatalf("first dispatch = %+v", res)
	}
	got := pipelines()
	if got["build"].Source != "git" || got["build"].SourceCommit != c1 || got["build"].RepositoryID != "repo_1" || got["build"].Disabled || got["deploy"].SourceCommit != c1 {
		t.Fatalf("indexed = %+v", got)
	}
	if runs() != 2 {
		t.Fatalf("%d runs", runs())
	}
	run, err := s.GetRun(ctx, "t_a", "p_a1", res.Runs[0])
	if err != nil || run.Status != "queued" || len(run.Steps) != 1 || len(run.ExecutionOrder) != 1 {
		t.Fatalf("run = %+v, %v", run, err)
	}
	info, err := s.RunGitInfo(ctx, "t_a", "p_a1", res.Runs[0])
	if err != nil || !strings.Contains(string(info.Sources), `"access": "read"`) && !strings.Contains(string(info.Sources), `"access":"read"`) {
		t.Fatalf("pinned sources = %s, %v", info.Sources, err)
	}
	queued, _ := s.QueuedRuns(ctx, "t_a")
	if len(queued) != 2 {
		t.Fatalf("queued = %+v", queued)
	}

	// The audit rows: one marker and one refusal, neither echoing content.
	var markers, refusals int
	events, _ := s.PendingEvents(ctx, "t_a", time.Now().Add(time.Hour), 100)
	for _, e := range events {
		switch e.Type {
		case store.EventPipelinesIndexed:
			markers++
			if e.ID != store.DispatchMarkerID("evt_1") || e.ResourceID != "repo_1" || e.ProjectKey != "erp" {
				t.Fatalf("marker = %+v", e)
			}
		case store.EventPipelineRefused:
			refusals++
			if !strings.Contains(string(e.Payload), "unknown_field") || strings.Contains(string(e.Payload), "echo") {
				t.Fatalf("refusal = %s", e.Payload)
			}
		}
	}
	if markers != 1 || refusals != 1 {
		t.Fatalf("markers %d refusals %d", markers, refusals)
	}

	// A replay changes nothing, even with a different plan.
	again := dispatch("evt_1", 10, &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"build"},
		Definitions: []store.IndexedDefinition{def("build", "echo REPLAYED")}}, plannerOf(t, "k2"))
	if !again.Replayed || runs() != 2 || pipelines()["build"].SourceCommit != c1 || pipelines()["deploy"].Disabled {
		t.Fatalf("a replay changed state: %+v", again)
	}

	// The next push replaces build, drops deploy (disabled, not deleted) and
	// starts one more run for the pipelines in force only.
	res = dispatch("evt_2", 20, &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"build"},
		Definitions: []store.IndexedDefinition{def("build", "echo 2")}}, plannerOf(t, "k3"))
	got = pipelines()
	if len(res.Disabled) != 1 || res.Disabled[0] != "deploy" || !got["deploy"].Disabled || got["deploy"].SourceCommit != c1 ||
		got["build"].SourceCommit != c2 || !strings.Contains(string(got["build"].Definition), "echo 2") || got["build"].Version != 2 || len(res.Runs) != 1 {
		t.Fatalf("after the second push: %+v %+v", res, got)
	}
	// An invalid file leaves the definition alone: it is simply not in Definitions.
	dispatch("evt_3", 30, &store.PushIndex{Ref: "refs/heads/main", Commit: c1, Present: []string{"build"},
		Refusals: []store.IndexRefusal{{Path: ".kodama/build.yaml", Code: "invalid_value"}}}, nil)
	if p := pipelines()["build"]; p.SourceCommit != c2 || p.Disabled || p.Version != 2 {
		t.Fatalf("a refused file displaced the definition: %+v", p)
	}
	// An unreadable commit disables nothing.
	dispatch("evt_4", 40, &store.PushIndex{Ref: "refs/heads/main", Commit: c1, Unreadable: true,
		Refusals: []store.IndexRefusal{{Path: ".kodama/", Code: "files_refused"}}}, nil)
	if p := pipelines()["build"]; p.Disabled {
		t.Fatal("an unreadable commit disabled a pipeline")
	}
	// A pipeline whose file comes back is enabled again with the new commit.
	dispatch("evt_5", 50, &store.PushIndex{Ref: "refs/heads/main", Commit: c1, Present: []string{"build", "deploy"},
		Definitions: []store.IndexedDefinition{def("build", "echo 5"), def("deploy", "echo 5")}}, nil)
	if p := pipelines()["deploy"]; p.Disabled || p.SourceCommit != c1 {
		t.Fatalf("a returned file did not re-enable its pipeline: %+v", p)
	}

	// A stale event (older than an indexed one) leaves the index alone.
	stale := dispatch("evt_old", 15, &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"build"},
		Definitions: []store.IndexedDefinition{def("build", "echo STALE")}}, nil)
	if !stale.Stale || strings.Contains(string(pipelines()["build"].Definition), "STALE") {
		t.Fatalf("stale dispatch = %+v, %+v", stale, pipelines()["build"])
	}

	// Another event starting the same (pipeline, trigger key) starts nothing.
	before := runs()
	res = dispatch("evt_6", 60, nil, plannerOf(t, "k1", "k1", "k9"))
	if len(res.Runs) != 2 || runs() != before+2 { // k1 existed for the two pipelines indexed first; k9 is new for both
		t.Fatalf("trigger key dedup: %+v (%d runs, was %d)", res, runs(), before)
	}

	// A failing plan rolls back everything, the marker included.
	boom := errors.New("plan failed")
	if _, err := s.DispatchPush(ctx, store.PushDispatch{TenantID: "t_a", RepositoryID: "repo_1", EventID: "evt_7", JournalPosition: 70,
		Index: &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"fresh"}, Definitions: []store.IndexedDefinition{def("fresh", "echo")}},
		Plan:  func([]store.Pipeline) ([]store.RunAdmission, error) { return nil, boom }}); !errors.Is(err, boom) {
		t.Fatalf("plan error = %v", err)
	}
	if _, ok := pipelines()["fresh"]; ok || pipelines()["build"].Disabled {
		t.Fatalf("a failed dispatch left %+v", pipelines())
	}
	if retried := dispatch("evt_7", 70, nil, nil); retried.Replayed {
		t.Fatal("a rolled-back event left its marker")
	}

	// Concurrent deliveries of one event admit one run per pipeline.
	before = runs()
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.DispatchPush(ctx, store.PushDispatch{TenantID: "t_a", RepositoryID: "repo_1", EventID: "evt_c", JournalPosition: 80, Plan: plannerOf(t, "kc")})
			if err != nil {
				t.Error(err)
				return
			}
			created.Add(int32(len(r.Runs)))
		}()
	}
	wg.Wait()
	if created.Load() != 2 || runs() != before+2 {
		t.Fatalf("concurrent deliveries created %d runs (%d -> %d)", created.Load(), before, runs())
	}

	// An archived repository is left alone, under the same row lock the dispatch
	// takes: nothing is indexed or admitted and no marker is written, so the event
	// dispatches after a restore.
	if done, err := s.Dispatched(ctx, "t_a", "evt_1"); err != nil || !done {
		t.Fatalf("Dispatched(evt_1) = %v, %v", done, err)
	}
	if _, err := s.SetRepositoryStatus(ctx, "t_a", "repo_1", store.RepoArchived, "", "user", "u1"); err != nil {
		t.Fatal(err)
	}
	before = runs()
	buildVersion := pipelines()["build"].Version
	res = dispatch("evt_arch", 90, &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"build"},
		Definitions: []store.IndexedDefinition{def("build", "echo ARCHIVED")}}, plannerOf(t, "karch"))
	if !res.Archived || len(res.Runs) != 0 || runs() != before || pipelines()["build"].Version != buildVersion {
		t.Fatalf("an archived repository was dispatched: %+v", res)
	}
	if done, _ := s.Dispatched(ctx, "t_a", "evt_arch"); done {
		t.Fatal("an archived repository's event got its marker")
	}
	if _, err := s.SetRepositoryStatus(ctx, "t_a", "repo_1", store.RepoReady, "", "user", "u1"); err != nil {
		t.Fatal(err)
	}
	if res = dispatch("evt_arch", 90, &store.PushIndex{Ref: "refs/heads/main", Commit: c2, Present: []string{"build"},
		Definitions: []store.IndexedDefinition{def("build", "echo RESTORED")}}, plannerOf(t, "karch")); res.Archived || len(res.Runs) == 0 || !strings.Contains(string(pipelines()["build"].Definition), "RESTORED") {
		t.Fatalf("after the restore: %+v", res)
	}

	// The tenant-level repository has no pipelines; an unknown repository is not found.
	if r, err := s.DispatchPush(ctx, store.PushDispatch{TenantID: "t_a", RepositoryID: tenantLevel.ID, EventID: "evt_8", Plan: plannerOf(t, "k")}); err != nil || !r.Skipped {
		t.Fatalf("tenant-level dispatch = %+v, %v", r, err)
	}
	if _, err := s.DispatchPush(ctx, store.PushDispatch{TenantID: "t_b", RepositoryID: "repo_1", EventID: "evt_9"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another tenant's repository: %v", err)
	}
}

// checkUnpublishedEvents is the contract of OutboxBackend.UnpublishedEvents,
// shared by the memory and PostgreSQL backends. seed creates the repository
// resource id with its repository.created event created at the given time.
func checkUnpublishedEvents(t *testing.T, b store.OutboxBackend, seed func(resource string, at time.Time)) {
	t.Helper()
	ctx := t.Context()
	base := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }
	seed("r0", at(1))
	seed("r1", at(2))
	seed("r2", at(3))
	seed("tie1", at(4))
	seed("tie2", at(4)) // the same creation time: the id breaks the tie
	seed("r5", at(5))

	far := at(1000)
	all, err := b.PendingEvents(ctx, "t_a", far, 100)
	if err != nil || len(all) != 6 {
		t.Fatalf("setup: %+v, %v", all, err)
	}
	id := map[string]string{}
	for _, e := range all {
		id[e.ResourceID] = e.ID
	}
	lo, hi := id["tie1"], id["tie2"]
	if lo > hi {
		lo, hi = hi, lo
	}
	walk := func(cutoff time.Time, limit int) (ids []string) {
		t.Helper()
		var after store.OutboxCursor
		for range 20 {
			page, err := b.UnpublishedEvents(ctx, "t_a", cutoff, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) > limit {
				t.Fatalf("a page of %d rows for a limit of %d", len(page), limit)
			}
			if len(page) == 0 {
				return ids
			}
			for _, e := range page {
				ids = append(ids, e.ID)
			}
			after = page[len(page)-1].CursorOf()
		}
		t.Fatal("paging did not terminate")
		return nil
	}
	want := func(parts ...string) string { return strings.Join(parts, ",") }
	got := func(ids []string) string { return strings.Join(ids, ",") }

	// The cutoff is applied before the limit: r5 is younger than it, and the pages
	// of two cover the five older events in (created_at, id) order exactly once.
	if g := got(walk(at(4), 2)); g != want(id["r0"], id["r1"], id["r2"], lo, hi) {
		t.Fatalf("pages = %s", g)
	}
	if g := got(walk(at(10), 100)); g != want(id["r0"], id["r1"], id["r2"], lo, hi, id["r5"]) {
		t.Fatalf("one page = %s", g)
	}
	if g := got(walk(at(0), 3)); g != "" {
		t.Fatalf("nothing is older than the first event: %s", g)
	}
	// A deferred event stays listed whatever its next attempt, a failure never
	// moves it relative to the cursor, and a published one disappears.
	if err := b.PublishFailed(ctx, "t_a", id["r0"], at(2000)); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishFailed(ctx, "t_a", id["r2"], at(2000)); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkPublished(ctx, "t_a", id["r1"], at(6)); err != nil {
		t.Fatal(err)
	}
	if g := got(walk(at(4), 2)); g != want(id["r0"], id["r2"], lo, hi) {
		t.Fatalf("after a deferral and a publication: %s", g)
	}
	page, err := b.UnpublishedEvents(ctx, "t_a", at(10), store.OutboxCursor{}, 1)
	if err != nil || len(page) != 1 || page[0].ID != id["r0"] || page[0].Attempts != 1 || page[0].TenantKey != "a" || page[0].ProjectKey != "erp" {
		t.Fatalf("first deferred page = %+v, %v", page, err)
	}
	// Strictly after a cursor, including inside a tie.
	after, err := b.UnpublishedEvents(ctx, "t_a", at(10), store.OutboxCursor{CreatedAt: at(3), ID: id["r2"]}, 10)
	if err != nil || len(after) != 3 || after[0].ID != lo || after[1].ID != hi || after[2].ID != id["r5"] {
		t.Fatalf("after r2 = %+v, %v", after, err)
	}
	inTie, err := b.UnpublishedEvents(ctx, "t_a", at(10), store.OutboxCursor{CreatedAt: at(4), ID: lo}, 1)
	if err != nil || len(inTie) != 1 || inTie[0].ID != hi {
		t.Fatalf("after the first of a tie = %+v, %v", inTie, err)
	}
	if none, err := b.UnpublishedEvents(ctx, "t_a", at(10), store.OutboxCursor{}, 0); err != nil || len(none) != 0 {
		t.Fatalf("limit 0 = %+v, %v", none, err)
	}
	// Another tenant's rows are never listed.
	if other, err := b.UnpublishedEvents(ctx, "t_b", at(10), store.OutboxCursor{}, 10); err != nil || len(other) != 0 {
		t.Fatalf("another tenant's outbox = %+v, %v", other, err)
	}
}

func TestMemoryGitUnpublishedEvents(t *testing.T) {
	m := store.NewMemoryGit()
	m.AddTenant("t_a", "a")
	m.AddTenant("t_b", "b")
	m.AddProject("t_a", "p_a1", "erp")
	checkUnpublishedEvents(t, m, func(resource string, at time.Time) {
		m.SetClock(func() time.Time { return at })
		if _, created, err := m.CreateRepository(t.Context(), store.Repository{TenantID: "t_a", ProjectID: "p_a1", ID: resource, Name: resource, DefaultBranch: "main"}); err != nil || !created {
			t.Fatal(created, err)
		}
		if _, err := m.ConfirmInitialized(t.Context(), "t_a", resource, "main", true); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGitPostgresUnpublishedEvents(t *testing.T) {
	s, db, ctx := gitDB(t)
	admin := db.MigrationPool(ctx, 1)
	checkUnpublishedEvents(t, s, func(resource string, at time.Time) {
		readyRepo(t, s, "t_a", "p_a1", resource, resource)
		if _, err := admin.Exec(ctx, `UPDATE audit_events SET created_at = $2 WHERE tenant_id = 't_a' AND resource_id = $1 AND event_type = 'repository.created'`, resource, at); err != nil {
			t.Fatal(err)
		}
	})
}

// pgSource adapts the store to outbox.Source the way the kernel does: the
// cursor is the event's (created_at, id) position. afterRead runs once, after
// the first reconciliation page has been read.
type pgSource struct {
	st        *store.Store
	afterRead func()
}

func (o *pgSource) Tenants(ctx context.Context) ([]domain.TenantID, error) {
	return o.st.TenantIDs(ctx)
}

func (o *pgSource) rows(events []store.OutboxEvent) []outbox.Row {
	out := make([]outbox.Row, 0, len(events))
	for _, e := range events {
		out = append(out, outbox.Row{
			Message:   bus.Message{ID: e.ID, TenantID: e.TenantID, ProjectID: e.ProjectID, Type: e.Type, ResourceType: e.ResourceType, ResourceID: e.ResourceID, Actor: bus.Actor{Type: e.ActorType, ID: e.ActorID}, OccurredAt: e.CreatedAt.UTC(), Data: json.RawMessage(e.Payload)},
			TenantKey: e.TenantKey, ProjectKey: e.ProjectKey, Attempts: e.Attempts, CreatedAt: e.CreatedAt,
			Cursor: fmt.Sprintf("%020d/%s", e.CreatedAt.UnixNano(), e.ID),
		})
	}
	return out
}

func (o *pgSource) Pending(ctx context.Context, tn domain.TenantID, now time.Time, limit int) ([]outbox.Row, error) {
	events, err := o.st.PendingEvents(ctx, tn, now, limit)
	return o.rows(events), err
}

func (o *pgSource) Unpublished(ctx context.Context, tn domain.TenantID, cutoff time.Time, after string, limit int) ([]outbox.Row, error) {
	var cur store.OutboxCursor
	if after != "" {
		n, id, _ := strings.Cut(after, "/")
		ns, err := strconv.ParseInt(n, 10, 64)
		if err != nil {
			return nil, err
		}
		cur = store.OutboxCursor{CreatedAt: time.Unix(0, ns).UTC(), ID: id}
	}
	events, err := o.st.UnpublishedEvents(ctx, tn, cutoff, cur, limit)
	if fn := o.afterRead; fn != nil && err == nil {
		o.afterRead = nil
		fn()
	}
	return o.rows(events), err
}

func (o *pgSource) MarkPublished(ctx context.Context, tn domain.TenantID, id string, at time.Time) error {
	return o.st.MarkPublished(ctx, tn, id, at)
}

func (o *pgSource) MarkFailed(ctx context.Context, tn domain.TenantID, id string, next time.Time) error {
	return o.st.PublishFailed(ctx, tn, id, next)
}

type recordingSink struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingSink) Publish(_ context.Context, _ string, m bus.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, m.ID)
	return nil
}

func (r *recordingSink) count(id string) (n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.ids {
		if got == id {
			n++
		}
	}
	return n
}

// TestGitPostgresOutboxCursorCoversConcurrentCommits is the two-connection case
// on a real database: one connection inserts an event (its created_at is the
// start of its transaction) and holds the transaction open, the publisher's
// reconciliation reads its first page and moves its cursor past that event's
// position, and only then the other connection commits. That pass cannot see the
// event; with one publisher and a working store and broker, the next
// reconciliation pass (or a drain) publishes it once.
func TestGitPostgresOutboxCursorCoversConcurrentCommits(t *testing.T) {
	for _, via := range []string{"reconcile", "drain"} {
		t.Run(via, func(t *testing.T) {
			s, db, ctx := gitDB(t)
			late := db.RuntimePool(ctx, 2) // the second connection
			const lateID = "evt_late_commit"

			// The late transaction runs under its own context: whatever ends the test
			// early cancels it, WithTenant rolls it back and the connection returns
			// to the pool before the pool is closed.
			txCtx, cancelTx := context.WithCancel(ctx)
			inserted, release, committed, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan struct{})
			t.Cleanup(func() {
				cancelTx()
				select {
				case <-finished:
				case <-time.After(10 * time.Second):
					t.Error("the late transaction did not end")
				}
			})
			go func() {
				defer close(finished)
				committed <- storage.WithTenant(txCtx, late, "t_a", func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `INSERT INTO audit_events (tenant_id,id,actor_type,actor_id,event_type,resource_type,resource_id)
						VALUES ('t_a',$1,'system','test','repository.created','repository','repo_late')`, lateID); err != nil {
						return err
					}
					close(inserted)
					select {
					case <-release:
						return nil // commits here
					case <-ctx.Done():
						return ctx.Err() // rolls back
					}
				})
			}()
			select {
			case <-inserted:
			case err := <-committed:
				t.Fatal(err)
			case <-time.After(10 * time.Second):
				t.Fatal("the late transaction never inserted")
			}
			// The late event is invisible until it commits.
			if got, err := s.PendingEvents(ctx, "t_a", time.Now().Add(time.Hour), 100); err != nil || len(got) != 0 {
				t.Fatalf("an uncommitted event is visible: %+v, %v", got, err)
			}
			// Events created after the late transaction began, committed before it.
			time.Sleep(5 * time.Millisecond)
			readyRepo(t, s, "t_a", "p_a1", "repo_1", "backend")
			readyRepo(t, s, "t_a", "p_a1", "repo_2", "frontend")

			sink, src := &recordingSink{}, &pgSource{st: s}
			src.afterRead = func() {
				close(release)
				select {
				case err := <-committed:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("the late transaction did not commit")
				}
			}
			future := time.Now().Add(time.Hour) // every event is older than the grace period
			p, err := outbox.NewPublisher(outbox.PublisherConfig{Store: src, Bus: sink, Batch: 1, Grace: time.Minute, Now: func() time.Time { return future }})
			if err != nil {
				t.Fatal(err)
			}
			first, err := p.Reconcile(ctx)
			if err != nil || first.Failed != 0 || first.Published < 2 {
				t.Fatalf("first pass = %+v, %v", first, err)
			}
			if sink.count(lateID) != 0 {
				t.Fatalf("the cursor already passed the late event's position, yet it was delivered: pass 1 must not see it")
			}
			if via == "reconcile" {
				if res, err := p.Reconcile(ctx); err != nil || res.Published != 1 || sink.count(lateID) != 1 {
					t.Fatalf("second pass = %+v, %v (late delivered %d times)", res, err, sink.count(lateID))
				}
			} else if res, err := p.Drain(ctx); err != nil || res.Published != 1 || sink.count(lateID) != 1 {
				t.Fatalf("drain = %+v, %v (late delivered %d times)", res, err, sink.count(lateID))
			}
			// Exactly once: nothing repeats on either path.
			if res, err := p.Reconcile(ctx); err != nil || res.Published != 0 {
				t.Fatalf("a later pass = %+v, %v", res, err)
			}
			if res, err := p.Drain(ctx); err != nil || res.Published != 0 {
				t.Fatalf("a later drain = %+v, %v", res, err)
			}
			sink.mu.Lock()
			defer sink.mu.Unlock()
			seen := map[string]int{}
			for _, id := range sink.ids {
				seen[id]++
			}
			if len(seen) < 3 {
				t.Fatalf("delivered %v", seen)
			}
			for id, n := range seen {
				if n != 1 {
					t.Fatalf("event %s delivered %d times", id, n)
				}
			}
		})
	}
}
