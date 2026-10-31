package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"

	"github.com/optimiware/kodama/internal/shell/pipelines"
	"github.com/optimiware/kodama/internal/shell/runstate"
	"github.com/optimiware/kodama/test/support/kernelcontracts"
)

var (
	scopeA = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	scopeB = domain.Scope{TenantID: "t2", ProjectID: "p1"} // another tenant, same project name
	scopeC = domain.Scope{TenantID: "t1", ProjectID: "p2"} // another project of the first tenant
)

// directory is the test Authority: principals and the roles they hold per
// project. A role can be revoked between two calls.
type directory struct {
	mu    sync.Mutex
	roles map[domain.Scope]map[string][]string
}

func newDirectory() *directory { return &directory{roles: map[domain.Scope]map[string][]string{}} }

func (d *directory) grant(scope domain.Scope, principal string, roles ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.roles[scope] == nil {
		d.roles[scope] = map[string][]string{}
	}
	d.roles[scope][principal] = roles
}

func (d *directory) Roles(_ context.Context, scope domain.Scope, principal string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.roles[scope][principal]...), nil
}

// planner returns a build -> review (wait, assigned to reviewer) -> ship plan.
type planner struct {
	mu    sync.Mutex
	calls int
	plan  runtime.Plan
	last  PlanRequest
}

func reviewPlan(assign string) runtime.Plan {
	spec := runtime.WaitSpec{Assign: assign, Correlate: map[string]string{"candidate": "build.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour}
	return runtime.Plan{Digest: "sha256:review", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &spec},
		{Key: "ship", Kind: runtime.StepContainer, DependsOn: []domain.StepID{"review"},
			Workload: &runtime.WorkloadSpec{Image: "alpine:3", Commands: []string{"ship"}}},
	}}
}

func (p *planner) Plan(_ context.Context, req PlanRequest) (runtime.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.last = req
	return p.plan, nil
}

// rig is a Service over one set of adapters: the real pipeline registry, the
// kernel's run-state adapter and the task store, all in memory (newMemoryRig) or
// all on PostgreSQL (newPostgresRig). The same scenarios run on both.
type rig struct {
	t     *testing.T
	svc   *Service
	store Store
	runs  runtime.StateStore
	clock *kernelcontracts.Clock
	auth  *directory
	reg   *pipelines.Service
	plan  *planner
	// mem is the memory task store, nil on PostgreSQL; owner is the owner pool of
	// the PostgreSQL database, nil in memory.
	mem   *Memory
	owner *pgxpool.Pool
	ctx   context.Context
	// engines holds the one claim per run the test's engine took.
	engines map[domain.RunID]*engine
}

// buildRig builds the rig a scenario runs on. The PostgreSQL test replaces it for
// the length of its run.
var buildRig = newMemoryRig

func newRig(t *testing.T) *rig { t.Helper(); return buildRig(t) }

func newMemoryRig(t *testing.T) *rig {
	t.Helper()
	clock := kernelcontracts.NewClock()
	mem := NewMemory()
	pmem := pipelines.NewMemory(clock.Now)
	for _, s := range []domain.Scope{scopeA, scopeB, scopeC} {
		pmem.AddProject(s)
	}
	return assemble(t, clock, mem, mem, runstate.NewMemory(clock), pipelines.New(pmem), nil)
}

// assemble wires the service over the adapters and grants the test principals.
func assemble(t *testing.T, clock *kernelcontracts.Clock, store Store, mem *Memory, runs runtime.StateStore, reg *pipelines.Service, owner *pgxpool.Pool) *rig {
	t.Helper()
	r := &rig{t: t, store: store, mem: mem, owner: owner, runs: runs, clock: clock, auth: newDirectory(), reg: reg,
		plan: &planner{plan: reviewPlan("reviewer")}, ctx: t.Context(), engines: map[domain.RunID]*engine{}}
	n := 0
	svc, err := New(Config{Store: store, Authority: r.auth, Pipelines: r.reg, Planner: r.plan, Runs: runs, Clock: clock,
		NewID: func() (domain.TaskID, error) { n++; return domain.TaskID(fmt.Sprintf("tsk_%03d", n)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	for _, s := range []domain.Scope{scopeA, scopeB, scopeC} {
		r.auth.grant(s, "eng", RoleEngineer)
	}
	r.auth.grant(scopeA, "admin", RoleAdmin)
	r.auth.grant(scopeA, "ops", RoleOperator)
	r.auth.grant(scopeA, "view", RoleViewer)
	r.auth.grant(scopeA, "alice", RoleReviewer)
	r.auth.grant(scopeA, "bob", RoleReviewer)
	r.auth.grant(scopeA, "carol", RoleEngineer)
	return r
}

func (r *rig) caller(scope domain.Scope, principal string) Caller {
	return Caller{Scope: scope, Principal: principal}
}

func pipelineDoc(name string) []byte {
	return []byte(fmt.Sprintf("name: %s\nsteps:\n  - name: build\n    image: golang:1.27\n    commands:\n      - echo hi\n", name))
}

// pipeline registers an API pipeline in the scope and returns its ID.
func (r *rig) pipeline(scope domain.Scope, name string) domain.PipelineID {
	r.t.Helper()
	res, err := r.reg.Register(r.ctx, pipelines.RegisterRequest{Scope: scope, Document: pipelineDoc(name)})
	if err != nil {
		r.t.Fatalf("register %s: %v", name, err)
	}
	return domain.PipelineID(res.Definition.ID)
}

// task creates a task on pipeline "sdlc" of scopeA, registering it on first use.
func (r *rig) task(title string) Task {
	r.t.Helper()
	if _, err := r.reg.Resolve(r.ctx, scopeA, "sdlc"); err != nil {
		r.pipeline(scopeA, "sdlc")
	}
	t, err := r.svc.Create(r.ctx, CreateRequest{Caller: r.caller(scopeA, "eng"), Pipeline: "sdlc", Content: Content{Title: title}})
	if err != nil {
		r.t.Fatalf("create: %v", err)
	}
	return t
}

func (r *rig) start(t Task) Task {
	r.t.Helper()
	got, err := r.svc.Start(r.ctx, StartRequest{Caller: r.caller(scopeA, "eng"), TaskID: t.ID})
	if err != nil {
		r.t.Fatalf("start: %v", err)
	}
	return got
}

func (r *rig) get(id domain.TaskID) Task {
	r.t.Helper()
	got, err := r.store.Get(r.ctx, scopeA, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// engine drives a run through the kernel the way an engine would: claim, commit.
type engine struct {
	r   *rig
	rec runtime.RunRecord
	seq int
}

func (r *rig) engine(run domain.RunID) *engine {
	r.t.Helper()
	if e, ok := r.engines[run]; ok { // the engine already holds the claim: resync with the records
		rec, err := r.runs.Load(r.ctx, scopeA, run)
		if err != nil {
			r.t.Fatal(err)
		}
		rec.Claim = e.rec.Claim
		e.rec = rec
		return e
	}
	rec, err := r.runs.Claim(r.ctx, runtime.ClaimRequest{Scope: scopeA, RunID: run, Owner: "engine", Purpose: runtime.ClaimExecute, TTL: 48 * time.Hour}, r.clock.Now())
	if err != nil {
		r.t.Fatalf("claim: %v", err)
	}
	e := &engine{r: r, rec: rec}
	r.engines[run] = e
	return e
}

// step applies one run mutation and the wait changes, commits it at the next
// run version and keeps the record it returns.
func (e *engine) step(mutate func(*runtime.Run) error, waits ...runtime.Wait) runtime.RunRecord {
	e.r.t.Helper()
	run := e.rec.Run.Clone()
	if err := mutate(&run); err != nil {
		e.r.t.Fatalf("step move: %v", err)
	}
	sealed, err := run.Sealed(e.rec.Run)
	if err != nil {
		e.r.t.Fatalf("seal: %v", err)
	}
	e.seq++
	ev := runtime.Event{ID: fmt.Sprintf("run.state_changed:%s:%d", run.ID, sealed.Version), Scope: run.Scope, RunID: run.ID,
		Kind: runtime.EventRunStateChanged, Version: sealed.Version, At: e.r.clock.Now()}
	rec, err := e.r.runs.Commit(e.r.ctx, runtime.Transition{Scope: run.Scope, RunID: run.ID, Claim: e.rec.Claim, Now: e.r.clock.Now(),
		ExpectedVersion: e.rec.Run.Version, Run: sealed, Waits: waits, Events: []runtime.Event{ev}})
	if err != nil {
		e.r.t.Fatalf("commit: %v", err)
	}
	e.rec = rec
	return rec
}

func (e *engine) build() {
	e.r.t.Helper()
	e.step(func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) })
	e.step(func(r *runtime.Run) error {
		return r.FinishStep("build", runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	})
}

// openReview parks the review step on its opened wait.
func (e *engine) openReview() runtime.Wait {
	e.r.t.Helper()
	i := indexWait(e.rec, "review")
	w := e.rec.Waits[i]
	if err := w.Open(e.r.clock.Now(), map[string]string{"candidate": "c1"}, "please review c1"); err != nil {
		e.r.t.Fatal(err)
	}
	e.step(func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) }, w)
	return w
}

func indexWait(rec runtime.RunRecord, step domain.StepID) int {
	for i, w := range rec.Waits {
		if w.StepKey == step {
			return i
		}
	}
	return -1
}

// waiting drives a started task to an open review wait and projects it.
func (r *rig) waiting(t Task) (Task, runtime.Wait) {
	r.t.Helper()
	t = r.start(t)
	e := r.engine(t.Progress.RunID)
	e.build()
	e.openReview()
	r.apply(t.Progress.RunID)
	return r.get(t.ID), e.rec.Waits[indexWait(e.rec, "review")]
}

func (r *rig) apply(run domain.RunID) Applied {
	r.t.Helper()
	a, err := r.svc.Apply(r.ctx, runtime.Event{ID: "e", Scope: scopeA, RunID: run, Kind: runtime.EventRunStateChanged, Version: 1, At: r.clock.Now()})
	if err != nil {
		r.t.Fatalf("apply: %v", err)
	}
	return a
}

func isCode(err error, code domain.ErrorCode) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

// solePlan is one container step.
func solePlan() runtime.Plan {
	return runtime.Plan{Digest: "sha256:sole", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
	}}
}

// succeed runs the sole build step to success and settles the run with its
// cleanup complete, so the run is terminal.
func (e *engine) succeed() {
	e.r.t.Helper()
	e.build()
	e.step(func(r *runtime.Run) error { return r.Settle(runtime.OutcomeSucceeded, runtime.ReasonNone) })
	e.step(func(r *runtime.Run) error { return r.CompleteCleanup() })
}

// memoryOnly reports whether the scenario may use the memory adapter's fault
// injection. On PostgreSQL the same guarantees are held by the transaction, and
// the contract tests of the database adapter prove them there.
func (r *rig) memoryOnly() bool {
	if r.mem == nil {
		r.t.Log("memory adapter only: fault injection is not available on PostgreSQL")
		return false
	}
	return true
}

// failNext makes the next task-store write fail (memory only).
func (r *rig) failNext(err error) { r.mem.FailNextWrite(err) }

// events returns the committed task events in commit order. On PostgreSQL the
// deduplication key is hashed into the event ID, so only Type and Data are set.
func (r *rig) events() []Event {
	if r.mem != nil {
		return r.mem.Events()
	}
	rows, err := r.owner.Query(r.ctx, `SELECT event_type, payload::text FROM audit_events WHERE resource_type = 'task' ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var typ, payload string
		if err := rows.Scan(&typ, &payload); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, Event{Type: typ, Data: []byte(payload)})
	}
	return out
}

// damage overwrites a task's stored progress without any guard.
func (r *rig) damage(task Task, p Progress) {
	r.t.Helper()
	if r.mem != nil {
		r.mem.Damage(scopeA, task.ID, p)
		return
	}
	if _, err := r.owner.Exec(r.ctx, `UPDATE tasks SET status = $3, current_step_key = NULLIF($4, ''), waiting_for = NULLIF($5, '')
WHERE tenant_id = $1 AND id = $2`, string(scopeA.TenantID), string(task.ID), string(p.Status), string(p.Step), p.WaitingFor); err != nil {
		r.t.Fatal(err)
	}
}
