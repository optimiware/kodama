package deliverycleanup_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

var (
	scopeA = executiontest.ScopeA
	scopeB = executiontest.ScopeB
	tenant = scopeA.TenantID
)

// records is the deployment record double (T-035's boundary): it implements the
// scheduler's Records port and execution's DeploymentCleanup authorization from
// one table, the way delivery's adapters do from one record, and it is driven by
// the controlled clock.
type records struct {
	mu    sync.Mutex
	clock *executiontest.FakeClock
	deps  map[string]*recorded

	authCalls int
	// outage fails every call with the error while set.
	outage error
	// foreign makes Scan also returns other scopes' records, which
	// the scheduler must not act on.
	foreign bool
	pageMax int
}

type recorded struct {
	deliverycleanup.Candidate
	Settled bool
}

func key(scope domain.Scope, id string) string {
	return string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + id
}

func newRecords(clock *executiontest.FakeClock) *records {
	return &records{clock: clock, deps: map[string]*recorded{}}
}

// add records a settled deployment expiring after ttl (0: no expiry).
func (r *records) add(scope domain.Scope, id string, ttl time.Duration) *recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := &recorded{Candidate: deliverycleanup.Candidate{Scope: scope, Deployment: id, ActionKey: "act:" + id}, Settled: true}
	if ttl > 0 {
		t := r.clock.Now().Add(ttl)
		d.ExpiresAt = &t
	}
	r.deps[key(scope, id)] = d
	return d
}

func (r *records) with(scope domain.Scope, id string, f func(*recorded)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r.deps[key(scope, id)])
}

func (r *records) snapshot(scope domain.Scope, id string) deliverycleanup.Candidate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deps[key(scope, id)].Candidate
}

// report is a cleanup run's report to the record (ReportCleanup).
func (r *records) report(scope domain.Scope, id string, state deliverycleanup.RecordState) {
	r.with(scope, id, func(d *recorded) {
		d.State = state
		d.Deleted = state == deliverycleanup.RecordDone
	})
}

func (r *records) sorted(keep func(*recorded) bool) []*recorded {
	var out []*recorded
	for _, d := range r.deps {
		if keep(d) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	return out
}

func (r *records) Scan(_ context.Context, scope domain.Scope, after string, limit int) ([]deliverycleanup.Candidate, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outage != nil {
		return nil, "", r.outage
	}
	if r.pageMax > 0 && limit > r.pageMax {
		limit = r.pageMax
	}
	all := r.sorted(func(d *recorded) bool { return (d.Scope == scope || r.foreign) && d.Deployment > after })
	var out []deliverycleanup.Candidate
	next := ""
	for i, d := range all {
		if i == limit {
			break
		}
		next = d.Deployment
		if !d.Deleted {
			out = append(out, d.Candidate)
		}
	}
	if len(all) <= limit {
		next = ""
	}
	return out, next, nil
}

func (r *records) Get(_ context.Context, scope domain.Scope, id string) (deliverycleanup.Candidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outage != nil {
		return deliverycleanup.Candidate{}, r.outage
	}
	d, ok := r.deps[key(scope, id)]
	if !ok {
		return deliverycleanup.Candidate{}, deliverycleanup.ErrNotFound
	}
	return d.Candidate, nil
}

func (r *records) RequestCleanup(_ context.Context, scope domain.Scope, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outage != nil {
		return r.outage
	}
	d, ok := r.deps[key(scope, id)]
	switch {
	case !ok:
		return deliverycleanup.ErrNotFound
	case d.Deleted || d.RequestedAt != nil:
		return nil
	case !d.Settled:
		return deliverycleanup.ErrNotSettled
	}
	t := r.clock.Now()
	d.RequestedAt, d.State = &t, deliverycleanup.RecordRequested
	return nil
}

// AuthorizeCleanup is execution.DeploymentCleanup: recorded in the scope,
// requested and not deleted.
func (r *records) AuthorizeCleanup(_ context.Context, scope domain.Scope, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authCalls++
	if r.outage != nil {
		return r.outage
	}
	d, ok := r.deps[key(scope, id)]
	if !ok || d.RequestedAt == nil || d.Deleted {
		return execution.ErrDenied
	}
	return nil
}

type scopes []domain.Scope

func (s scopes) Scopes(_ context.Context, after *domain.Scope, limit int) ([]domain.Scope, error) {
	var out []domain.Scope
	for _, sc := range s {
		if after != nil && fmt.Sprint(sc) <= fmt.Sprint(*after) {
			continue
		}
		out = append(out, sc)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type definitions map[domain.Scope]domain.DefinitionID

func (d definitions) CleanupDefinition(_ context.Context, scope domain.Scope) (domain.DefinitionID, error) {
	id, ok := d[scope]
	if !ok {
		return "", deliverycleanup.ErrNoDefinition
	}
	return id, nil
}

// world is one wired scenario: the execution memory fixture with the records
// double as its cleanup authorization, and a scheduler over both.
type world struct {
	t     *testing.T
	m     *executiontest.Mem
	rec   *records
	proj  executiontest.Project
	def   domain.DefinitionID
	sched *deliverycleanup.Scheduler
}

func newWorld(t *testing.T, policy deliverycleanup.Policy, mutate ...func(*deliverycleanup.Config)) *world {
	t.Helper()
	rec := newRecords(nil)
	m := executiontest.NewMem(t, executiontest.MemOptions{Cleanup: rec})
	rec.clock = m.Clock
	m.GrantAll()
	w := &world{t: t, m: m, rec: rec, proj: m.NewProject(scopeA)}
	w.def = m.Define(scopeA, pipelinetest.Doc(pipelinetest.Container("cleanup", "workspace: {access: none}"))).ID
	w.sched = w.scheduler(policy, mutate...)
	return w
}

func (w *world) scheduler(policy deliverycleanup.Policy, mutate ...func(*deliverycleanup.Config)) *deliverycleanup.Scheduler {
	w.t.Helper()
	cfg := deliverycleanup.Config{
		Scopes: scopes{scopeA}, Records: w.rec, Runs: w.m.Svc, Definitions: definitions{scopeA: w.def},
		Policy: policy, Clock: w.m.Clock.Now,
	}
	for _, f := range mutate {
		f(&cfg)
	}
	s, err := deliverycleanup.New(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

// runs lists the project's cleanup runs.
func (w *world) cleanupRuns() []*execution.Run {
	w.t.Helper()
	all, err := w.m.Mem.List(context.Background(), scopeA, execution.ListQuery{Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []*execution.Run
	for _, r := range all {
		if r.Purpose == execution.PurposeCleanup {
			out = append(out, r)
		}
	}
	return out
}

func (w *world) sweep(s *deliverycleanup.Scheduler) deliverycleanup.Report {
	w.t.Helper()
	rep, err := s.Sweep(context.Background(), scopeA)
	if err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
	return rep
}

// one returns the only item of a report.
func one(t *testing.T, rep deliverycleanup.Report) deliverycleanup.Item {
	t.Helper()
	if len(rep.Items) != 1 {
		t.Fatalf("report has %d items: %+v", len(rep.Items), rep.Items)
	}
	return rep.Items[0]
}

func wantOutcome(t *testing.T, it deliverycleanup.Item, want deliverycleanup.Outcome) {
	t.Helper()
	if it.Outcome != want {
		t.Fatalf("outcome %q (err %v), want %q", it.Outcome, it.Err, want)
	}
}

// ---- driving the admitted run to its ends ----

// claim dispatches the next cleanup attempt and starts its command.
func (w *world) claim() *execution.AttemptSpec {
	w.t.Helper()
	in := w.m.MustClaim(tenant)
	w.m.Start(in)
	return in
}

// succeed ends the run successfully.
func (w *world) succeed() {
	w.t.Helper()
	in := w.claim()
	if _, err := w.m.Succeed(in, nil, ""); err != nil {
		w.t.Fatal(err)
	}
	if err := w.m.Terminate(in, execution.LaunchStarted); err != nil {
		w.t.Fatal(err)
	}
}

// failKnown ends the run with a command failure whose effects are accounted for.
func (w *world) failKnown() {
	w.t.Helper()
	in := w.claim()
	if _, err := w.m.Fail(in); err != nil {
		w.t.Fatal(err)
	}
	if err := w.m.Terminate(in, execution.LaunchStarted); err != nil {
		w.t.Fatal(err)
	}
}

// failUnknown ends the run with an ambiguous infrastructure failure: the
// workload may have deleted objects, and nothing proves whether it did.
func (w *world) failUnknown() {
	w.t.Helper()
	in := w.m.MustClaim(tenant)
	if err := w.m.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
		w.t.Fatal(err)
	}
	if err := w.m.Terminate(in, execution.LaunchUnknown); err != nil {
		w.t.Fatal(err)
	}
}

func isErr(err, target error) bool { return errors.Is(err, target) }
