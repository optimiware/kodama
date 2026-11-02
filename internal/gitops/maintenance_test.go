package gitops_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
	"github.com/optimiware/kodama/internal/gitops/localgit"
)

const (
	tenantA domain.TenantID  = "t138_acme"
	tenantB domain.TenantID  = "t138_globex"
	projA   domain.ProjectID = "p138_inventory"
	projB   domain.ProjectID = "p138_inventory_b"
)

func scope(t domain.TenantID) domain.Scope {
	if t == tenantB {
		return domain.Scope{TenantID: t, ProjectID: projB}
	}
	return domain.Scope{TenantID: t, ProjectID: projA}
}

func repo(t domain.TenantID, id string) gitops.Repository {
	return gitops.Repository{ID: id, Scope: scope(t), DefaultBranch: "main"}
}

type rig struct {
	t       *testing.T
	vol     *gitopstest.Volume
	worker  *gitopstest.Worker
	catalog *gitopstest.Catalog
	holds   *gitopstest.Holds
	clock   *gitopstest.Clock
	objects localgit.Objects
	intents *gitopstest.Intents
	// fence and recoveries are the tenant state backups, pruning and maintenance
	// consult (and the restore tests drive).
	fence      *gitopstest.Fence
	recoveries *gitopstest.Recoveries
}

func newRig(t *testing.T) *rig {
	vol := gitopstest.NewVolume(t)
	clock := gitopstest.NewClock()
	worker := gitopstest.NewWorker(vol)
	return &rig{t: t, vol: vol, worker: worker, catalog: &gitopstest.Catalog{}, holds: &gitopstest.Holds{},
		clock: clock, objects: localgit.Objects{Locator: vol, JournalHead: worker.Head}, intents: &gitopstest.Intents{Now: clock.Now},
		fence: &gitopstest.Fence{}, recoveries: &gitopstest.Recoveries{}}
}

func (r *rig) scheduler(o gitops.Options) *gitops.Scheduler {
	r.t.Helper()
	o.Clock = r.clock.Now
	s, err := gitops.NewScheduler(gitops.SchedulerConfig{Worker: r.worker, Catalog: r.catalog, Holds: r.holds, Objects: r.objects, Intents: r.intents, Fence: r.fence, Recoveries: r.recoveries, Options: o})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// Test_unit_gitops_maintenance_scheduled: every repository is maintained
// through its tenant's worker once per slot, serialized per repository, under an
// operation ID derived from the slot (idempotent across retries and restarts);
// a failing repository neither stops the others nor is marked done; archived
// repositories are maintained but never pruned (GR-A09).
func Test_unit_gitops_maintenance_scheduled(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	for _, x := range []gitops.Repository{repo(tenantA, "r_a1"), repo(tenantA, "r_a2"), repo(tenantB, "r_b1")} {
		r.vol.Init(x.Scope.TenantID, x.ID)
		r.catalog.Add(x)
	}
	archived := repo(tenantA, "r_a3")
	archived.Archived = true
	r.vol.Init(tenantA, "r_a3")
	r.catalog.Add(archived)
	s := r.scheduler(gitops.Options{MaintenanceInterval: time.Hour})

	reports, err := s.RunOnce(ctx)
	if err != nil || len(reports) != 4 {
		t.Fatalf("first pass: %d reports, %v", len(reports), err)
	}
	for _, rep := range reports {
		if !rep.Maintained || rep.Skipped || rep.Err != nil {
			t.Fatalf("%s: %+v", rep.Repository.ID, rep)
		}
	}
	if n := r.worker.Count("maintain"); n != 4 {
		t.Fatalf("%d maintain calls, want 4 (archived repositories are maintained too)", n)
	}
	firstIDs := map[string]bool{}
	for _, c := range r.worker.Calls {
		if strings.HasPrefix(c, "maintain") {
			firstIDs[c] = true
		}
	}

	// The same slot: nothing more is asked of any worker.
	r.clock.Advance(10 * time.Minute)
	reports, err = s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reports {
		if !rep.Skipped || rep.Maintained {
			t.Fatalf("%s repeated inside the slot: %+v", rep.Repository.ID, rep)
		}
	}
	if n := r.worker.Count("maintain"); n != 4 {
		t.Fatalf("%d maintain calls after a repeat in the slot", n)
	}
	// A restarted scheduler (no memory) sends the identical request: the worker's
	// idempotency on the operation ID makes it a no-op there.
	restarted := r.scheduler(gitops.Options{MaintenanceInterval: time.Hour})
	if _, err := restarted.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := r.worker.Count("maintain"); n != 8 {
		t.Fatalf("restart sent %d maintain calls", n-4)
	}
	for _, c := range r.worker.Calls[4:] {
		if strings.HasPrefix(c, "maintain") && !firstIDs[c] {
			t.Fatalf("a restart used a new operation ID: %s", c)
		}
	}

	// The next slot maintains again, under new operation IDs; one failing
	// repository does not stop the rest and is retried by the next pass.
	r.clock.Advance(time.Hour)
	before := len(r.worker.Calls)
	r.worker.Fail["maintain"] = errors.New("worker unreachable")
	reports, err = s.RunOnce(ctx)
	if err == nil || len(reports) != 4 {
		t.Fatalf("failing pass: %d reports, %v", len(reports), err)
	}
	for _, rep := range reports {
		if rep.Err == nil || rep.Maintained {
			t.Fatalf("%s: %+v", rep.Repository.ID, rep)
		}
	}
	delete(r.worker.Fail, "maintain")
	reports, err = s.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reports {
		if !rep.Maintained {
			t.Fatalf("%s was not retried: %+v", rep.Repository.ID, rep)
		}
	}
	for _, c := range r.worker.Calls[before:] {
		if strings.HasPrefix(c, "maintain") && firstIDs[c] {
			t.Fatalf("the next slot reused an operation ID: %s", c)
		}
	}
}

// Test_unit_gitops_maintenance_serialized: concurrent requests for one
// repository never overlap; another repository is not held up.
func Test_unit_gitops_maintenance_serialized(t *testing.T) {
	r := newRig(t)
	r.vol.Init(tenantA, "r_s1")
	r.vol.Init(tenantA, "r_s2")
	r.catalog.Add(repo(tenantA, "r_s1"))
	r.catalog.Add(repo(tenantA, "r_s2"))
	s := r.scheduler(gitops.Options{MaintenanceInterval: time.Hour})

	var mu sync.Mutex
	active := map[string]int{}
	maxActive := map[string]int{}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	r.worker.Hook = func(op, repoID string) {
		if op != "maintain" {
			return
		}
		mu.Lock()
		active[repoID]++
		maxActive[repoID] = max(maxActive[repoID], active[repoID])
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		active[repoID]--
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); s.MaintainRepository(context.Background(), repo(tenantA, "r_s1")) }()
	}
	<-entered // one is inside r_s1's maintenance; the others wait for the repository
	// Another repository is not held by it.
	done := make(chan struct{})
	go func() { s.MaintainRepository(context.Background(), repo(tenantA, "r_s2")); close(done) }()
	<-entered
	close(release)
	wg.Wait()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if maxActive["r_s1"] != 1 {
		t.Fatalf("maintenance of one repository overlapped %d times", maxActive["r_s1"])
	}
	if n := r.worker.Count("maintain r_s1"); n != 1 {
		t.Fatalf("r_s1 was maintained %d times in one slot", n)
	}
}

func Test_unit_gitops_scheduler_validates(t *testing.T) {
	if _, err := gitops.NewScheduler(gitops.SchedulerConfig{}); !errors.Is(err, gitops.ErrNotConfigured) {
		t.Fatalf("no collaborators: %v", err)
	}
	r := newRig(t)
	if _, err := gitops.NewScheduler(gitops.SchedulerConfig{Worker: r.worker, Catalog: r.catalog, Fence: r.fence, Recoveries: r.recoveries, Options: gitops.Options{MaintenanceInterval: time.Second}}); err == nil {
		t.Fatal("an interval below a minute")
	}
	r.catalog.Fail = errors.New("down")
	s := r.scheduler(gitops.Options{})
	if _, err := s.RunOnce(context.Background()); !errors.Is(err, gitops.ErrUnavailable) {
		t.Fatalf("a catalog outage: %v", err)
	}
}
