package productmcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// flakyResults fails on demand, as a store outage does.
type flakyResults struct {
	Results
	failLookup, failRecord bool
}

func (r *flakyResults) Lookup(ctx context.Context, k ReplayKey) (Recorded, bool, error) {
	if r.failLookup {
		return Recorded{}, false, errors.New("store down")
	}
	return r.Results.Lookup(ctx, k)
}

func (r *flakyResults) Record(ctx context.Context, k ReplayKey, v Recorded) (Recorded, error) {
	if r.failRecord {
		return Recorded{}, errors.New("store down")
	}
	return r.Results.Record(ctx, k, v)
}

func Test_unit_productmcp_idempotent_create(t *testing.T) {
	ctx := context.Background()
	eng := personCaller(tenantA, "erin")
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.w.grant(eng, projP, "engineer")
		f.w.grant(eng, projQ, "engineer")
		f.w.grant(personCaller(tenantA, "ada"), "*", "admin")
		return f
	}
	repoIn := RepositoryCreateIn{ProjectID: string(projP), IdempotencyKey: "retry-1", Name: "backend"}

	t.Run("a retry returns the original resource for every creating tool", func(t *testing.T) {
		f := setup(t)
		r1, err := f.svc.CreateRepository(ctx, eng, repoIn)
		if err != nil || r1.Replayed {
			t.Fatalf("%v %+v", err, r1)
		}
		r2, err := f.svc.CreateRepository(ctx, eng, repoIn)
		if err != nil || r2.ID != r1.ID || !r2.Replayed {
			t.Fatalf("repository replay: %v %+v", err, r2)
		}
		i1, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "retry-1", Title: "Add search"})
		if err != nil {
			t.Fatal(err)
		}
		// The default kind is part of the normalized request: spelling it out is
		// the same request.
		i2, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "retry-1", Title: "Add search", Kind: "task"})
		if err != nil || i2.ID != i1.ID || !i2.Replayed {
			t.Fatalf("work item replay: %v %+v", err, i2)
		}
		_, err = f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "retry-1", Title: "Add search", Kind: "defect"})
		mustCode(t, err, "idempotency_conflict")
		ada := personCaller(tenantA, "ada")
		p1, err := f.svc.CreateProject(ctx, ada, ProjectCreateIn{IdempotencyKey: "retry-1", Key: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := f.svc.CreateProject(ctx, ada, ProjectCreateIn{IdempotencyKey: "retry-1", Key: "shop"})
		if err != nil || p2.ID != p1.ID || !p2.Replayed {
			t.Fatalf("project replay: %v %+v", err, p2)
		}
		if len(f.w.repos) != 1 || len(f.w.items) != 1 || len(f.w.projects) != 4 { // 3 fixtures + shop
			t.Fatalf("a replay created again: %d repos, %d items, %d projects", len(f.w.repos), len(f.w.items), len(f.w.projects))
		}
	})

	t.Run("the same key with another request conflicts and changes nothing", func(t *testing.T) {
		f := setup(t)
		if _, err := f.svc.CreateRepository(ctx, eng, repoIn); err != nil {
			t.Fatal(err)
		}
		changed := repoIn
		changed.Name = "frontend"
		_, err := f.svc.CreateRepository(ctx, eng, changed)
		mustCode(t, err, "idempotency_conflict")
		changed = repoIn
		changed.DefaultBranch = ptr("trunk")
		_, err = f.svc.CreateRepository(ctx, eng, changed)
		mustCode(t, err, "idempotency_conflict")
		changed = repoIn
		changed.DirectPushToDefault = ptr(true)
		_, err = f.svc.CreateRepository(ctx, eng, changed)
		mustCode(t, err, "idempotency_conflict")
		// The same key in another project, or for another tool, is another request.
		other := repoIn
		other.ProjectID = string(projQ)
		if r, err := f.svc.CreateRepository(ctx, eng, other); err != nil || r.Replayed {
			t.Fatalf("another project: %v %+v", err, r)
		}
		if _, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: repoIn.IdempotencyKey, Title: "t"}); err != nil {
			t.Fatalf("another tool: %v", err)
		}
		if len(f.w.repos) != 2 {
			t.Fatalf("%d repositories", len(f.w.repos))
		}
	})

	t.Run("keys are per principal and per tenant", func(t *testing.T) {
		f := setup(t)
		erin2 := personCaller(tenantA, "eli")
		f.w.grant(erin2, projP, "engineer")
		a, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "same"})
		if err != nil {
			t.Fatal(err)
		}
		b, err := f.svc.CreateWorkItem(ctx, erin2, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "same"})
		if err != nil || b.ID == a.ID || b.Replayed {
			t.Fatalf("another principal must not replay: %v %+v", err, b)
		}
		// A key someone else recorded cannot be used to read their resource.
		f.w.grant(erin2, projP, "")
		_, err = f.svc.CreateWorkItem(ctx, erin2, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "same"})
		mustCode(t, err, "not_found")
	})

	t.Run("concurrent retries converge on one resource", func(t *testing.T) {
		f := setup(t)
		const n = 16
		f.w.gate = barrier(t, n) // every caller has missed the lookup before any create
		var wg sync.WaitGroup
		ids := make([]string, n)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				it, err := f.svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "same", Title: "once"})
				if err == nil {
					ids[i] = it.ID
				}
			}()
		}
		wg.Wait()
		if len(f.w.items) != 1 {
			t.Fatalf("%d items created for one key", len(f.w.items))
		}
		for _, id := range ids {
			if id == "" || id != ids[0] {
				t.Fatalf("callers disagree: %v", ids)
			}
		}
	})

	t.Run("losing the record race returns the winner's resource, or a conflict", func(t *testing.T) {
		// A use case that ignores the key would orphan items; the record is the
		// second line: concurrent creators still converge on the first record.
		f := setup(t)
		const n = 8
		f.w.gate = barrier(t, n)
		svc, _ := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: &unkeyedItems{fakeItems: fakeItems{f.w}}, Provisioning: fakeProv{f.w}, Results: f.res})
		var wg sync.WaitGroup
		outs := make([]WorkItemOut, n)
		for i := range outs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outs[i], _ = svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "race", Title: "once"})
			}()
		}
		wg.Wait()
		replayed := 0
		for _, o := range outs {
			if o.ID == "" || o.ID != outs[0].ID {
				t.Fatalf("callers disagree: %+v", outs)
			}
			if o.Replayed {
				replayed++
			}
		}
		if replayed != n-1 {
			t.Fatalf("one creator wins the record; %d of %d replayed", replayed, n)
		}

		// Two different requests racing for one key: one wins, the other conflicts.
		g := setup(t)
		g.w.gate = barrier(t, 2)
		svc, _ = NewService(Config{Repositories: fakeRepos{g.w}, Projects: fakeProjects{g.w}, WorkItems: &unkeyedItems{fakeItems: fakeItems{g.w}}, Provisioning: fakeProv{g.w}, Results: g.res})
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = svc.CreateWorkItem(ctx, eng, WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "race", Title: fmt.Sprintf("title %d", i)})
			}()
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("exactly one request wins the key: %v %v", errs[0], errs[1])
		}
		if errs[0] != nil {
			mustCode(t, errs[0], "idempotency_conflict")
		} else {
			mustCode(t, errs[1], "idempotency_conflict")
		}
	})

	t.Run("a lost record still converges through the use case's own key", func(t *testing.T) {
		f := setup(t)
		flaky := &flakyResults{Results: f.res, failRecord: true}
		svc, _ := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: flaky})
		in := WorkItemCreateIn{ProjectID: string(projP), IdempotencyKey: "k", Title: "t"}
		a, err := svc.CreateWorkItem(ctx, eng, in)
		if err != nil {
			t.Fatalf("the create happened; a lost record must not fail it: %v", err)
		}
		flaky.failRecord = false
		b, err := svc.CreateWorkItem(ctx, eng, in) // nothing recorded: the use case's key converges
		if err != nil || b.ID != a.ID || len(f.w.items) != 1 {
			t.Fatalf("%v %+v items=%d", err, b, len(f.w.items))
		}
		c, err := svc.CreateWorkItem(ctx, eng, in)
		if err != nil || !c.Replayed || c.ID != a.ID {
			t.Fatalf("%v %+v", err, c)
		}
		// The key given to the use case is namespaced by tool.
		for _, k := range f.w.sawKeys {
			if k != "mcp."+ToolWorkItemCreate+".k" {
				t.Fatalf("use case key %q", k)
			}
		}
	})

	t.Run("an unavailable store fails closed before creating", func(t *testing.T) {
		f := setup(t)
		flaky := &flakyResults{Results: f.res, failLookup: true}
		svc, _ := NewService(Config{Repositories: fakeRepos{f.w}, Projects: fakeProjects{f.w}, WorkItems: fakeItems{f.w}, Provisioning: fakeProv{f.w}, Results: flaky})
		_, err := svc.CreateRepository(ctx, eng, repoIn)
		mustCode(t, err, "unavailable")
		if len(f.w.repos) != 0 {
			t.Fatal("created without knowing whether the key was used")
		}
	})

	t.Run("a different create failing does not consume the key", func(t *testing.T) {
		f := setup(t)
		viewer := personCaller(tenantA, "vic")
		f.w.grant(viewer, projP, "viewer")
		_, err := f.svc.CreateRepository(ctx, viewer, repoIn)
		mustCode(t, err, "forbidden")
		f.w.grant(viewer, projP, "engineer")
		if r, err := f.svc.CreateRepository(ctx, viewer, repoIn); err != nil || r.Replayed {
			t.Fatalf("%v %+v", err, r)
		}
	})

	t.Run("keys are validated and name the retry, nothing else", func(t *testing.T) {
		f := setup(t)
		for _, k := range []string{"", strings.Repeat("a", 65), "has space", "semi;colon", "új"} {
			in := repoIn
			in.IdempotencyKey = k
			_, err := f.svc.CreateRepository(ctx, eng, in)
			mustCode(t, err, "invalid_request")
		}
		if len(f.w.sawCalls) != 0 {
			t.Fatal("an invalid key reached a port")
		}
	})

	t.Run("a seed is never an idempotent create", func(t *testing.T) {
		f := setup(t)
		r, err := f.svc.CreateRepository(ctx, eng, repoIn)
		if err != nil {
			t.Fatal(err)
		}
		n := len(f.res.m)
		for i := 0; i < 2; i++ {
			if _, err := f.svc.RequestSeed(ctx, eng, SeedIn{ProjectID: string(projP), RepositoryID: r.ID}); err != nil {
				t.Fatal(err)
			}
		}
		if len(f.res.m) != n || f.w.seeds != 2 {
			t.Fatalf("a seed request must reach the grant issuer each time and store nothing (%d records, %d grants)", len(f.res.m), f.w.seeds)
		}
	})

	t.Run("the repository contract still decides a name clash", func(t *testing.T) {
		f := setup(t)
		if _, err := f.svc.CreateRepository(ctx, eng, repoIn); err != nil {
			t.Fatal(err)
		}
		clash := repoIn
		clash.IdempotencyKey = "other-key"
		_, err := f.svc.CreateRepository(ctx, eng, clash)
		mustCode(t, err, "repository_name_taken")
		var e *Error
		if !errors.As(err, &e) || !errors.Is(err, ErrConflict) {
			t.Fatalf("%v", err)
		}
	})
}

// barrier blocks each caller until n have arrived (or the test times out).
func barrier(t *testing.T, n int) func() {
	var mu sync.Mutex
	arrived := 0
	open := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(open)
		}
		mu.Unlock()
		select {
		case <-open:
		case <-time.After(10 * time.Second):
			t.Error("callers never became concurrent")
		}
	}
}

// unkeyedItems is a work use case that ignores the idempotency key, as a
// broken adapter would: every call creates a new item.
type unkeyedItems struct {
	fakeItems
	mu sync.Mutex
	n  int
}

func (u *unkeyedItems) Create(ctx context.Context, c Caller, scope domain.Scope, idem string, req CreateWorkItemRequest) (WorkItem, error) {
	u.mu.Lock()
	u.n++
	k := fmt.Sprintf("%s-%d", idem, u.n)
	u.mu.Unlock()
	return u.fakeItems.Create(ctx, c, scope, k, req)
}
