package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/metrics"
	metricspg "github.com/optimiware/kodama/internal/metrics/postgres"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/work/worktest"
)

// Resource prefix of this task's fixtures (databases are provisioned by the
// shared helper with its own random names).
const prefix = "t042"

const (
	tenantA, projectA = "t_acme", "p_inventory"
	projectA2         = "p_billing_a"
	tenantB, projectB = "t_globex", "p_billing"
)

var (
	scopeA  = domain.Scope{TenantID: tenantA, ProjectID: projectA}
	scopeA2 = domain.Scope{TenantID: tenantA, ProjectID: projectA2}
	scopeB  = domain.Scope{TenantID: tenantB, ProjectID: projectB}
)

const commit = "1111111111111111111111111111111111111111"

const (
	engineer = "u_engineer"
	reviewer = "u_reviewer"
)

var (
	engineerPerms = []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermCancel, work.PermReopen}
	reviewerPerms = []work.Permission{work.PermRead, work.PermComplete, work.PermReopen, work.PermCancel}
)

// t0 is the base instant of every fixture timeline.
var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func mins(n int) time.Duration { return time.Duration(n) * time.Minute }

// clock is the controllable time source of the work service: history timestamps
// are exactly the instants a timeline names.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// authority resolves every principal to a person and allows everything: the
// real permission checks of the mutating transaction are bound to the stack's
// grant tables, so this only stands in for the identity module in the work
// service's pre-transaction check.
type authority struct{}

func (authority) ResolveActor(_ context.Context, _ domain.Scope, principal string) (work.Actor, error) {
	return work.Actor{ID: principal, Type: work.ActorUser}, nil
}
func (authority) Authorize(context.Context, work.Actor, domain.Scope, work.Permission) error {
	return nil
}

// env is a migrated database with the real T-011 store writing history under a
// controlled clock, and the metrics store reading it.
type env struct {
	t     testing.TB
	stack *worktest.Stack
	owner *pgxpool.Pool
	rt    *pgxpool.Pool
	clock *clock
	work  *work.Service
	store *metricspg.Store
}

func newEnv(t testing.TB) *env {
	t.Helper()
	stack := worktest.NewStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manifest, err := storage.NewManifest(storage.BaseContribution(), eventspg.Migrations(), workpg.Migrations(), metricspg.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, stack.Owner, manifest, storage.MigrateOptions{RuntimeRole: stack.DB.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := &env{t: t, stack: stack, owner: stack.Owner, rt: stack.Runtime, clock: &clock{t: t0}}
	for _, s := range []domain.Scope{scopeA, scopeB} {
		stack.SeedProject(string(s.TenantID), string(s.ProjectID), 500)
	}
	stack.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 500)`, tenantA, projectA2)
	for _, tenant := range []string{tenantA, tenantB} {
		stack.Principal(tenant, engineer, work.ActorUser, engineerPerms...)
		stack.Principal(tenant, reviewer, work.ActorUser, reviewerPerms...)
	}
	e.work, err = work.NewService(work.Config{Repository: stack.Store, Authority: authority{}, Clock: e.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	e.store, err = metricspg.NewStore(e.rt)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) caller(principal string) work.Caller {
	return work.Caller{PrincipalID: principal, RequestID: "req_" + prefix}
}

// create makes a story at the instant.
func (e *env) create(scope domain.Scope, when time.Time, title string) work.Item {
	e.t.Helper()
	e.clock.set(when)
	it, err := e.work.Create(context.Background(), work.CreateCommand{
		Scope: scope, Caller: e.caller(engineer), Kind: work.KindStory, Title: title, Spec: "# Plan\nDo it.",
		Criteria: []work.Criterion{{ID: "AC-1", Text: "Observable outcome."}},
	})
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return it
}

// move transitions at the instant. Review needs a candidate, done a completion
// by a reviewer, and every other edge a reason.
func (e *env) move(it work.Item, when time.Time, to work.State) work.Item {
	e.t.Helper()
	e.clock.set(when)
	principal := engineer
	req := work.TransitionRequest{To: to, Reason: "because"}
	switch to {
	case work.StateReview:
		req.Review = &work.ReviewClaim{CandidateCommit: commit, Evidence: []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: "art_report"}}}
	case work.StateDone:
		principal = reviewer
		req.Reason = "accepted"
		req.Completion = &work.CompletionClaim{ContentRevision: it.ContentRevision, CandidateCommit: commit, AcceptedCriteria: []string{"AC-1"}}
	}
	out, err := e.work.Transition(context.Background(), work.TransitionCommand{
		Scope: it.Scope, Caller: e.caller(principal), ID: it.ID, Expected: it.Version, Request: req,
	})
	if err != nil {
		e.t.Fatalf("%s -> %s at %s: %v", it.State, to, when.Format(time.RFC3339Nano), err)
	}
	return out
}

func (e *env) edit(it work.Item, when time.Time, p work.Patch) work.Item {
	e.t.Helper()
	e.clock.set(when)
	out, err := e.work.Edit(context.Background(), work.EditCommand{Scope: it.Scope, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: p})
	if err != nil {
		e.t.Fatalf("edit at %s: %v", when.Format(time.RFC3339Nano), err)
	}
	return out
}

func (e *env) block(it work.Item, when time.Time, reason string) work.Item {
	return e.edit(it, when, work.Patch{BlockedReason: work.Some(reason)})
}

func (e *env) unblock(it work.Item, when time.Time) work.Item {
	return e.edit(it, when, work.Patch{BlockedReason: work.Clear[string]()})
}

// toActive moves a new item through ready to active at the instants.
func (e *env) toActive(it work.Item, ready, active time.Time) work.Item {
	e.t.Helper()
	it = e.move(it, ready, work.StateReady)
	return e.move(it, active, work.StateActive)
}

func window(from, to time.Time) metrics.Window {
	w, err := metrics.NewWindow(from, to, to)
	if err != nil {
		panic(err)
	}
	return w
}

// summarize reads one item's summary through the real store.
func (e *env) summarize(it work.Item, w metrics.Window) metrics.WorkSummary {
	e.t.Helper()
	return e.summarizeIn(it.Scope, it.ID, w)
}

func (e *env) summarizeIn(scope domain.Scope, id domain.WorkItemID, w metrics.Window) metrics.WorkSummary {
	e.t.Helper()
	got, err := e.store.ReadSummaries(context.Background(), scope, []domain.WorkItemID{id}, w)
	if err != nil {
		e.t.Fatalf("summaries: %v", err)
	}
	if len(got) != 1 {
		e.t.Fatalf("summaries = %+v", got)
	}
	return got[0]
}

// query runs fn in a tenant transaction of the runtime role.
func (e *env) query(tenant string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.rt, domain.TenantID(tenant), fn)
}

func (e *env) count(tenant, sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.query(tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		e.t.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// expect compares the durations and counts of a summary with the timeline's.
type expect struct {
	lead, active, review, blocked, reopened, completedLead time.Duration
	reopens, blockedIntervals, completed, cancelled        int
	inFlight, openEnd                                      bool
	state                                                  metrics.State
	observed                                               domain.Version
}

func (x expect) check(t testing.TB, got metrics.WorkSummary) {
	t.Helper()
	if got.Status != metrics.StatusOK {
		t.Fatalf("status = %s (%s), want ok", got.Status, got.Anomaly)
	}
	want := metrics.WorkSummary{
		Scope: got.Scope, ItemID: got.ItemID, Window: got.Window, Status: metrics.StatusOK, ObservedVersion: x.observed,
		Lead: x.lead, Active: x.active, Review: x.review, Blocked: x.blocked, Reopened: x.reopened, CompletedLead: x.completedLead,
		ReopenCount: x.reopens, BlockedIntervals: x.blockedIntervals, CompletedEpisodes: x.completed, CancelledEpisodes: x.cancelled,
		InFlightAtStart: x.inFlight, OpenAtEnd: x.openEnd, StateAtEnd: x.state,
	}
	if got != want {
		t.Fatalf("summary mismatch\n got  %+v\n want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("summary is not coherent: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func newStoreOn(db storage.TxBeginner) (*metricspg.Store, error) { return metricspg.NewStore(db) }
