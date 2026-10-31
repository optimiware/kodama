package tasks

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/pipelines"
)

// Test_unit_tasks_pipeline_selection holds task creation to WM-13: a task
// selects one same-project pipeline by its project-unique name, resolved to the
// immutable ID at creation; a missing, ambiguous or other-project name fails
// without creating a task; and creation, pipeline selection and content are
// authorized by the caller's current role.
func Test_unit_tasks_pipeline_selection(t *testing.T) {
	t.Run("the name resolves to the immutable pipeline ID at creation", func(t *testing.T) {
		r := newRig(t)
		id := r.pipeline(scopeA, "sdlc")
		task, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeA, "eng"), Pipeline: "sdlc", Content: Content{Title: "ship it"}})
		if err != nil {
			t.Fatal(err)
		}
		if task.PipelineID != id || task.Progress.Status != StatusPending || task.Progress.RunID != "" || task.Version != 1 {
			t.Fatalf("task = %+v, want pipeline %s, pending, no run, version 1", task, id)
		}
		// Reading it back, and editing its content, never changes the selection.
		edited, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedVersion: 1,
			Content: Content{Title: "ship it now", Body: "details"}})
		if err != nil || edited.PipelineID != id || edited.Version != 2 {
			t.Fatalf("edit = %+v, %v", edited, err)
		}
		// The planner is asked for exactly the selected pipeline when the task starts.
		r.start(task)
		if r.plan.last.PipelineID != id || r.plan.last.TaskID != task.ID || r.plan.last.Scope != scopeA {
			t.Fatalf("planned %+v, want pipeline %s of task %s", r.plan.last, id, task.ID)
		}
	})

	t.Run("an unknown, other-project and other-tenant name fails and creates nothing", func(t *testing.T) {
		r := newRig(t)
		r.pipeline(scopeA, "sdlc")
		r.pipeline(scopeB, "tenant-b-only")
		r.pipeline(scopeC, "project-c-only")
		for _, name := range []string{"missing", "project-c-only", "tenant-b-only", "SDLC", ""} {
			_, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeA, "eng"), Pipeline: name, Content: Content{Title: "x"}})
			if err == nil {
				t.Fatalf("pipeline %q: created a task", name)
			}
			if name != "" && !errors.Is(err, pipelines.ErrNotFound) {
				t.Errorf("pipeline %q: %v, want not found", name, err)
			}
		}
		if ts, _ := r.store.List(r.ctx, ListQuery{Scope: scopeA}); len(ts) != 0 {
			t.Fatalf("%d tasks were created by refused requests", len(ts))
		}
		if len(r.events()) != 0 {
			t.Fatalf("a refused request left events: %+v", r.events())
		}
	})

	t.Run("a project cannot have two pipelines of one name, so a name is never ambiguous", func(t *testing.T) {
		r := newRig(t)
		r.pipeline(scopeA, "sdlc")
		_, err := r.reg.Register(r.ctx, pipelines.RegisterRequest{Scope: scopeA, Document: pipelineDoc("sdlc")})
		if !errors.Is(err, pipelines.ErrDuplicateName) {
			t.Fatalf("a second pipeline of the same name: %v, want duplicate", err)
		}
		// The same name in another project is another pipeline with its own ID.
		a := r.pipeline(scopeC, "sdlc")
		first, err := r.reg.Resolve(r.ctx, scopeA, "sdlc")
		if err != nil || domain.PipelineID(first) == a {
			t.Fatalf("the project resolves its own pipeline, got %s (project c has %s): %v", first, a, err)
		}
	})

	t.Run("creation is authorized by the caller's current role", func(t *testing.T) {
		r := newRig(t)
		r.pipeline(scopeA, "sdlc")
		create := func(principal string) error {
			_, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeA, principal), Pipeline: "sdlc", Content: Content{Title: "t"}})
			return err
		}
		for _, p := range []string{"view", "alice", "ops"} { // viewer, reviewer and operator do not select pipelines
			if err := create(p); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s: %v, want forbidden", p, err)
			}
		}
		if err := create("stranger"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a non-member: %v, want not found", err)
		}
		if err := create("eng"); err != nil {
			t.Errorf("engineer: %v", err)
		}
		if err := create("admin"); err != nil {
			t.Errorf("admin: %v", err)
		}
		// A role revoked a moment ago grants nothing now.
		r.auth.grant(scopeA, "eng")
		if err := create("eng"); !errors.Is(err, ErrNotFound) {
			t.Errorf("revoked engineer: %v, want not found", err)
		}
		// A member of another project is a stranger here.
		if _, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeC, "carol"), Pipeline: "sdlc", Content: Content{Title: "t"}}); !errors.Is(err, ErrNotFound) {
			t.Errorf("carol in project c: %v, want not found", err)
		}
	})

	t.Run("reading and editing follow the same authority and the content bounds hold", func(t *testing.T) {
		r := newRig(t)
		task := r.task("t")
		if _, err := r.svc.Get(r.ctx, r.caller(scopeA, "view"), task.ID); err != nil {
			t.Errorf("a viewer reads: %v", err)
		}
		if _, err := r.svc.Get(r.ctx, r.caller(scopeA, "stranger"), task.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("a non-member reads: %v", err)
		}
		if _, err := r.svc.Get(r.ctx, r.caller(scopeC, "eng"), task.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("another project reads: %v", err)
		}
		if _, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, "view"), TaskID: task.ID, ExpectedVersion: 1, Content: Content{Title: "x"}}); !errors.Is(err, ErrForbidden) {
			t.Errorf("a viewer edits: %v", err)
		}
		if _, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedVersion: 7, Content: Content{Title: "x"}}); !errors.Is(err, ErrContentConflict) {
			t.Errorf("a stale content edit: %v, want a version conflict", err)
		}
		for name, c := range map[string]Content{
			"no title":      {},
			"unknown role":  {Title: "x", AssignedRole: "root"},
			"array context": {Title: "x", Context: []byte(`[1]`)},
			"huge title":    {Title: string(make([]byte, 600))},
		} {
			if _, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeA, "eng"), Pipeline: "sdlc", Content: c}); !errors.Is(err, domain.ErrInvalidValue) {
				t.Errorf("%s: %v, want invalid", name, err)
			}
		}
	})

	t.Run("a content edit records its actor and a bounded event with the version change", func(t *testing.T) {
		r := newRig(t)
		task := r.task("audited")
		changed := func() (n int, versions []domain.Version) {
			for _, e := range r.events() {
				if e.Type != EventTaskContentChanged {
					continue
				}
				var d struct {
					TaskID  domain.TaskID  `json:"taskId"`
					Version domain.Version `json:"contentVersion"`
					Title   string         `json:"title"`
				}
				if err := json.Unmarshal(e.Data, &d); err != nil || d.TaskID != task.ID || d.Title != "" {
					t.Fatalf("event data %s: %v", e.Data, err)
				}
				n, versions = n+1, append(versions, d.Version)
			}
			return n, versions
		}
		edit := func(principal string, expected domain.Version, title string) error {
			_, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, principal), TaskID: task.ID, ExpectedVersion: expected, Content: Content{Title: title}})
			return err
		}
		if err := edit("carol", 1, "second"); err != nil {
			t.Fatal(err)
		}
		if err := edit("eng", 2, "third"); err != nil {
			t.Fatal(err)
		}
		if n, v := changed(); n != 2 || v[0] != 2 || v[1] != 3 {
			t.Fatalf("%d content events %v, want versions 2 and 3", n, v)
		}
		// The actor is recorded with the event.
		if r.mem != nil {
			var actors []string
			for _, e := range r.mem.Events() {
				if e.Type == EventTaskContentChanged {
					actors = append(actors, e.Actor)
				}
			}
			if len(actors) != 2 || actors[0] != "carol" || actors[1] != "eng" {
				t.Fatalf("actors = %v", actors)
			}
		} else {
			var n int
			if err := r.owner.QueryRow(r.ctx, `SELECT count(*) FROM audit_events WHERE event_type = 'task.content_changed' AND resource_id = $1
  AND actor_type = 'user' AND actor_id IN ('carol', 'eng')`, string(task.ID)).Scan(&n); err != nil || n != 2 {
				t.Fatalf("%d audit rows with their actor: %v", n, err)
			}
		}
		// Refusals leave no event: a stale version, a role that may not edit, a bad content.
		if err := edit("eng", 1, "stale"); !errors.Is(err, ErrContentConflict) {
			t.Fatalf("stale edit: %v", err)
		}
		if err := edit("view", 3, "viewer"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("viewer edit: %v", err)
		}
		if err := edit("eng", 3, ""); !errors.Is(err, domain.ErrInvalidValue) {
			t.Fatalf("empty title: %v", err)
		}
		if n, _ := changed(); n != 2 || r.get(task.ID).Version != 3 {
			t.Fatalf("a refused edit left %d events, version %d", n, r.get(task.ID).Version)
		}
	})

	t.Run("an edit whose event cannot be recorded changes nothing", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() { // the PostgreSQL rollback is proved by Test_postgres_tasks_store
			return
		}
		task := r.task("rollback")
		boom := errors.New("event store down")
		r.failNext(boom)
		before := len(r.events())
		if _, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedVersion: 1, Content: Content{Title: "lost"}}); !errors.Is(err, boom) {
			t.Fatalf("edit = %v", err)
		}
		if got := r.get(task.ID); got.Version != 1 || got.Title != "rollback" || len(r.events()) != before {
			t.Fatalf("the failed edit persisted: version %d title %q, %d events", got.Version, got.Title, len(r.events())-before)
		}
		if _, err := r.svc.Edit(r.ctx, EditRequest{Caller: r.caller(scopeA, "eng"), TaskID: task.ID, ExpectedVersion: 1, Content: Content{Title: "retried"}}); err != nil {
			t.Fatalf("the retry: %v", err)
		}
	})

	t.Run("a repeated create with the same ID returns the recorded task and a changed one conflicts", func(t *testing.T) {
		r := newRig(t)
		r.pipeline(scopeA, "sdlc")
		req := CreateRequest{Caller: r.caller(scopeA, "eng"), ID: "tsk_fixed", Pipeline: "sdlc",
			Content: Content{Title: "once", Context: []byte(`{"b":1,"a":2}`)}}
		first, err := r.svc.Create(r.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		req.Context = []byte(`{"a":2,"b":1}`) // the same context, keys in another order
		again, err := r.svc.Create(r.ctx, req)
		if err != nil || again.ID != first.ID || again.PipelineID != first.PipelineID {
			t.Fatalf("the replay = %+v, %v", again, err)
		}
		req.Title = "different"
		if _, err := r.svc.Create(r.ctx, req); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("a changed create under the same ID: %v, want a conflict", err)
		}
		created := 0
		for _, e := range r.events() {
			if e.Type == EventTaskCreated {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%d task.created events, want exactly one", created)
		}
	})

	t.Run("a failed commit leaves no task and no event", func(t *testing.T) {
		r := newRig(t)
		if !r.memoryOnly() {
			return
		}
		r.pipeline(scopeA, "sdlc")
		boom := errors.New("boom")
		r.failNext(boom)
		req := CreateRequest{Caller: r.caller(scopeA, "eng"), ID: "tsk_retry", Pipeline: "sdlc", Content: Content{Title: "retry"}}
		if _, err := r.svc.Create(r.ctx, req); !errors.Is(err, boom) {
			t.Fatalf("create = %v, want the commit failure", err)
		}
		if _, err := r.store.Get(r.ctx, scopeA, "tsk_retry"); !errors.Is(err, ErrNotFound) || len(r.events()) != 0 {
			t.Fatalf("the failed create persisted something: %v %v", err, r.events())
		}
		if _, err := r.svc.Create(r.ctx, req); err != nil {
			t.Fatalf("the retry: %v", err)
		}
	})
}
