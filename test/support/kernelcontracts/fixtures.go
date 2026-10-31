package kernelcontracts

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
)

var (
	t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	// scopeA is the scope under test; scopeB is another tenant with the same
	// project name and scopeC another project of scopeA's tenant.
	scopeA = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	scopeB = domain.Scope{TenantID: "t2", ProjectID: "p1"}
	scopeC = domain.Scope{TenantID: "t1", ProjectID: "p2"}
)

const (
	ttl      = time.Minute
	longTTL  = 48 * time.Hour
	run1     = domain.RunID("run1")
	run2     = domain.RunID("run2")
	pipeline = domain.PipelineID("pipe1")
)

// Env is what a case works with: its Subject, the clock and fixtures. Fatal
// helpers stop the case through its TB.
type Env struct {
	T     TB
	S     Subject
	Clock *Clock
	Ctx   context.Context

	seq int
}

// ---------------------------------------------------------------------------
// Plans and admissions
// ---------------------------------------------------------------------------

func reviewSpec() runtime.WaitSpec {
	return runtime.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "build.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected"}, Approving: "approved", Timeout: time.Hour}
}

// reviewPlan is build (container) -> review (wait) -> ship (container).
func reviewPlan() runtime.Plan {
	spec := reviewSpec()
	return runtime.Plan{Digest: "sha256:review", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
		{Key: "review", Kind: runtime.StepWait, DependsOn: []domain.StepID{"build"}, Wait: &spec},
		{Key: "ship", Kind: runtime.StepContainer, DependsOn: []domain.StepID{"review"},
			Workload: &runtime.WorkloadSpec{Image: "alpine:3", Commands: []string{"ship"}}},
	}}
}

// solePlan is one container step.
func solePlan() runtime.Plan {
	return runtime.Plan{Digest: "sha256:sole", Steps: []runtime.PlanStep{
		{Key: "build", Kind: runtime.StepContainer, Workload: &runtime.WorkloadSpec{Image: "golang:1", Commands: []string{"go build"}}},
	}}
}

func waitIDOf(run domain.RunID) runtime.WaitID { return runtime.WaitID(string(run) + "-review") }

func (e *Env) admission(scope domain.Scope, id domain.RunID, plan runtime.Plan, maxCleanup int) runtime.Admission {
	e.T.Helper()
	run, err := runtime.NewRun(scope, id, pipeline, plan, maxCleanup)
	e.ok(err, "build the run")
	a := runtime.Admission{Run: run, Plan: plan}
	for _, p := range plan.Steps {
		if p.Kind != runtime.StepWait {
			continue
		}
		w, err := runtime.NewWait(scope, waitIDOf(id), id, p.Key, *p.Wait)
		e.ok(err, "build the wait")
		a.Waits = append(a.Waits, w)
	}
	a.Events = []runtime.Event{{ID: "run.created:" + string(id), Scope: scope, RunID: id, Kind: runtime.EventRunCreated, Version: 1, At: e.Clock.Now()}}
	return a
}

// Admit admits a review-plan run into scopeA.
func (e *Env) admit(scope domain.Scope, id domain.RunID, plan runtime.Plan) runtime.RunRecord {
	e.T.Helper()
	rec, err := e.S.Admit(e.Ctx, e.admission(scope, id, plan, 2))
	e.ok(err, "admit run %s", id)
	return rec
}

func (e *Env) claimReq(scope domain.Scope, id domain.RunID, owner string, purpose runtime.ClaimPurpose, d time.Duration) runtime.ClaimRequest {
	return runtime.ClaimRequest{Scope: scope, RunID: id, Owner: owner, Purpose: purpose, TTL: d}
}

func (e *Env) claim(scope domain.Scope, id domain.RunID, owner string, purpose runtime.ClaimPurpose, d time.Duration) runtime.RunRecord {
	e.T.Helper()
	rec, err := e.S.Claim(e.Ctx, e.claimReq(scope, id, owner, purpose, d), e.Clock.Now())
	e.ok(err, "claim %s for %s", id, owner)
	return rec
}

func (e *Env) load(scope domain.Scope, id domain.RunID) runtime.RunRecord {
	e.T.Helper()
	rec, err := e.S.Load(e.Ctx, scope, id)
	e.ok(err, "load %s", id)
	return rec
}

func (e *Env) outbox(scope domain.Scope, id domain.RunID) []runtime.Event {
	e.T.Helper()
	ev, err := e.S.Outbox(e.Ctx, scope, id)
	e.ok(err, "read the outbox of %s", id)
	return ev
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

func (e *Env) event(scope domain.Scope, run domain.RunID, step domain.StepID, kind runtime.EventKind, version domain.Version) runtime.Event {
	e.seq++
	return runtime.Event{ID: fmt.Sprintf("%s:%s:%s:%d:%d", kind, run, step, version, e.seq), Scope: scope, RunID: run,
		StepKey: step, Kind: kind, Version: version, At: e.Clock.Now()}
}

// transition builds the Transition that applies mutate to rec's run, claimed by
// rec.Claim at the current time, at exactly rec's version plus one.
func (e *Env) transition(rec runtime.RunRecord, mutate func(r *runtime.Run) error, events ...runtime.Event) runtime.Transition {
	e.T.Helper()
	r := rec.Run.Clone()
	e.ok(mutate(&r), "apply the step move")
	sealed, err := r.Sealed(rec.Run)
	e.ok(err, "seal the run")
	return runtime.Transition{Scope: rec.Run.Scope, RunID: rec.Run.ID, Claim: rec.Claim, Now: e.Clock.Now(),
		ExpectedVersion: rec.Run.Version, Run: sealed, Events: events}
}

// bump builds a Transition whose run changes only its version.
func (e *Env) bump(rec runtime.RunRecord) runtime.Transition {
	r := rec.Run.Clone()
	r.Version++
	return runtime.Transition{Scope: rec.Run.Scope, RunID: rec.Run.ID, Claim: rec.Claim, Now: e.Clock.Now(),
		ExpectedVersion: rec.Run.Version, Run: r}
}

func (e *Env) commit(t runtime.Transition) runtime.RunRecord {
	e.T.Helper()
	rec, err := e.S.Commit(e.Ctx, t)
	e.ok(err, "commit version %d", t.ExpectedVersion)
	return rec
}

func (e *Env) step(rec runtime.RunRecord, mutate func(r *runtime.Run) error) runtime.RunRecord {
	e.T.Helper()
	return e.commit(e.transition(rec, mutate))
}

func (e *Env) startBuild(rec runtime.RunRecord) runtime.RunRecord {
	e.T.Helper()
	ev := e.event(rec.Run.Scope, rec.Run.ID, "build", runtime.EventStepStateChanged, rec.Run.Version+1)
	return e.commit(e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("build", runtime.StepRunning, runtime.ReasonNone) }, ev))
}

func (e *Env) finishStep(rec runtime.RunRecord, key domain.StepID) runtime.RunRecord {
	e.T.Helper()
	return e.step(rec, func(r *runtime.Run) error {
		return r.FinishStep(key, runtime.StepSucceeded, runtime.ReasonNone, runtime.Result{Code: runtime.ResultSucceeded})
	})
}

// openReview commits the review step waiting with its wait open for candidate c1.
func (e *Env) openReview(rec runtime.RunRecord) runtime.RunRecord {
	e.T.Helper()
	i := e.waitIndex(rec)
	w := rec.Waits[i]
	e.ok(w.Open(e.Clock.Now(), map[string]string{"candidate": "c1"}, "please review"), "open the wait")
	ev := e.event(rec.Run.Scope, rec.Run.ID, "review", runtime.EventWaitOpened, w.Version)
	t := e.transition(rec, func(r *runtime.Run) error { return r.AdvanceStep("review", runtime.StepWaiting, runtime.ReasonNone) }, ev)
	t.Waits = []runtime.Wait{w}
	return e.commit(t)
}

func (e *Env) waitIndex(rec runtime.RunRecord) int {
	for i, w := range rec.Waits {
		if w.StepKey == "review" {
			return i
		}
	}
	e.T.Fatalf("run %s has no review wait", rec.Run.ID)
	return -1
}

// waitingRun admits a review-plan run in scope, claims it for "engine" with a
// long lease, runs build to success and opens the review wait.
func (e *Env) waitingRun(scope domain.Scope, id domain.RunID) runtime.RunRecord {
	e.T.Helper()
	e.admit(scope, id, reviewPlan())
	rec := e.claim(scope, id, "engine", runtime.ClaimExecute, longTTL)
	rec = e.startBuild(rec)
	rec = e.finishStep(rec, "build")
	return e.openReview(rec)
}

func (e *Env) review(rec runtime.RunRecord) runtime.Wait { return rec.Waits[e.waitIndex(rec)] }

func handle(scope domain.Scope, run domain.RunID, name string) runtime.ResourceHandle {
	return runtime.ResourceHandle{Scope: scope, Owner: runtime.OwnerFor(runtime.LifetimePipeline, scope),
		Lifetime: runtime.LifetimePipeline, RunID: run, Kind: runtime.ResourceService, Name: name,
		Identity: string(run) + "-" + name, Fence: 1}
}

// ---------------------------------------------------------------------------
// Signals
// ---------------------------------------------------------------------------

func identity(scope domain.Scope, caller, key string) runtime.SignalIdentity {
	return runtime.SignalIdentity{Scope: scope, Caller: caller, Key: key}
}

// byWait is an inbox resolution of the wait by identity.
func byWait(scope domain.Scope, caller, key string, wait runtime.WaitID, outcome string) runtime.SignalRequest {
	return runtime.SignalRequest{Identity: identity(scope, caller, key), WaitID: wait, Outcome: outcome,
		Answer: "because", Actor: "user:" + caller}
}

// byCorrelation is an external signal addressed by the complete correlation.
func byCorrelation(scope domain.Scope, caller, key, candidate, outcome string) runtime.SignalRequest {
	return runtime.SignalRequest{Identity: identity(scope, caller, key), Correlation: map[string]string{"candidate": candidate},
		Outcome: outcome, Actor: "svc:" + caller}
}

func (e *Env) decide(req runtime.SignalRequest) runtime.SignalResult {
	e.T.Helper()
	res, err := e.S.DecideSignal(e.Ctx, req, e.Clock.Now())
	e.ok(err, "decide signal %q", req.Identity.Key)
	return res
}

func countKind(events []runtime.Event, kind runtime.EventKind) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

func (e *Env) ok(err error, format string, args ...any) {
	e.T.Helper()
	if err != nil {
		e.T.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
}

func (e *Env) expect(cond bool, format string, args ...any) {
	e.T.Helper()
	if !cond {
		e.T.Fatalf(format, args...)
	}
}

func (e *Env) isErr(err, target error, what string) {
	e.T.Helper()
	if !errors.Is(err, target) {
		e.T.Fatalf("%s: got %v, want %v", what, err, target)
	}
}

func hasCode(err error, code domain.ErrorCode) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

func (e *Env) notFound(err error, what string) {
	e.T.Helper()
	if !hasCode(err, domain.CodeNotFound) {
		e.T.Fatalf("%s: got %v, want a not-found error", what, err)
	}
}

func (e *Env) versionConflict(err error, what string) {
	e.T.Helper()
	if !hasCode(err, domain.CodeVersionConflict) {
		e.T.Fatalf("%s: got %v, want a version conflict", what, err)
	}
}

// unchanged asserts the run record is exactly before and the outbox still has
// n events, which is what a refused write must leave.
func (e *Env) unchanged(before runtime.RunRecord, n int, what string) {
	e.T.Helper()
	after := e.load(before.Run.Scope, before.Run.ID)
	if !reflect.DeepEqual(before, after) {
		e.T.Fatalf("%s changed the run record:\nbefore %+v\nafter  %+v", what, before, after)
	}
	if got := len(e.outbox(before.Run.Scope, before.Run.ID)); got != n {
		e.T.Fatalf("%s changed the outbox: %d events, want %d", what, got, n)
	}
}
