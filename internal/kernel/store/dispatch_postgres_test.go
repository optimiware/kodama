package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/store"
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
