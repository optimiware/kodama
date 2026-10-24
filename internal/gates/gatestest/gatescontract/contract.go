// Package gatescontract is the one exported contract suite of the gate port
// (SD-13): every adapter of the gate store, its current authority and its
// execution peer runs it. The memory adapter (gatestest/memory) runs it in the
// unit layer with no service; the PostgreSQL adapter runs it behind
// layer.SkipService in the `test` environment (gatestest.PostgresHarness).
//
// The suite asserts what callers rely on: tenant isolation and not-found
// versus forbidden, one decision per gate, replay of the same command, stale
// subjects refused and recorded once, revoked reviewers never consumed, a
// waiting gate holding no executor capacity, and progression performed once.
// It does not assert lock order, isolation levels, SQL or RLS: those belong to
// the PostgreSQL checkpoints (T-073).
package gatescontract

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
)

// Case is one run whose gate step is ready, with the facts its subject binds.
type Case struct {
	Scope      domain.Scope
	Run        domain.RunID
	Step       string
	Item       domain.WorkItemID
	Evidence   domain.ArtifactID
	Deployment string
}

// Events counts the gate events recorded for one gate.
type Events struct {
	Requested, Decided, Invalidated int
}

// Principals are the fixture identities of the primary scope.
type Principals struct {
	// Reviewer holds the reviewer role and nothing else.
	Reviewer string
	// Engineer holds a role that may not decide.
	Engineer string
	// Stranger has no binding in the scope.
	Stranger string
	// Foreign is a reviewer of another tenant only.
	Foreign string
}

// Harness is what an adapter provides: the service and store under test over
// its own persistence, current authority and execution peer, and primitives
// that change the facts a gate subject binds.
type Harness interface {
	Service() *gates.Service
	Store() gates.Store
	Principals() Principals
	// Scope is the primary project; Foreign is a project of another tenant.
	Scope() domain.Scope
	Foreign() domain.Scope
	// TTL is the gate deadline duration the service was built with.
	TTL() time.Duration

	// NewCase creates a run whose gate step is ready and has no gate yet. With
	// deployment the gate step also names a deployment of the candidate.
	NewCase(t testing.TB, deployment bool) Case
	// Grant makes principal a reviewer of the scope; Revoke removes every
	// binding of it (the member becomes a stranger).
	Grant(t testing.TB, principal string)
	Revoke(t testing.TB, principal string)
	// ChangeContent edits the work item's specification, ChangeEvidence makes
	// the bound artifact unusable, ChangeDeployment changes the deployment's
	// configuration digest.
	ChangeContent(t testing.TB, c Case)
	ChangeEvidence(t testing.TB, c Case)
	ChangeDeployment(t testing.TB, c Case)
	// DeploymentOutage turns an outage of the deployment peer on or off.
	DeploymentOutage(t testing.TB, on bool)
	Advance(t testing.TB, d time.Duration)

	// NewAgentCase creates a run whose gate step is ready and whose candidate
	// was published by a successful agent writer (AgentCommit) under the
	// specification: the producing attempt's pinned context carries Baseline,
	// the producer declares the report output when Declare is set, keeps
	// Supports supporting artifacts and delivers the report Report renders.
	NewAgentCase(t testing.TB, spec AgentSpec) Case
	// Pins counts the artifacts the gate pinned.
	Pins(t testing.TB, c Case, id gates.ID) int

	StepStatus(t testing.TB, c Case) execution.StepStatus
	// Attempts counts the attempts the step has ever had.
	Attempts(t testing.TB, c Case) int
	Events(t testing.TB, c Case, id gates.ID) Events
}

// Open builds a fresh, isolated Harness for one subtest.
type Open func(testing.TB) Harness

const reason = "Reviewed the identified source and test evidence."

// Contract runs the shared gate contract.
func Contract(t *testing.T, open Open) {
	t.Helper()
	t.Run("request is idempotent and tenant scoped", func(t *testing.T) { requestScoped(t, open(t)) })
	t.Run("only the human reviewer decides", func(t *testing.T) { onlyReviewer(t, open(t)) })
	t.Run("stale expectations are refused", func(t *testing.T) { staleExpectations(t, open(t)) })
	t.Run("approval is explicit and progresses once", func(t *testing.T) { approveProgresses(t, open(t)) })
	t.Run("repeated decisions converge", func(t *testing.T) { repeatedDecisions(t, open(t)) })
	t.Run("rejection fails the step once", func(t *testing.T) { rejection(t, open(t)) })
	t.Run("a changed subject is stale and recorded once", func(t *testing.T) { changedSubject(t, open(t)) })
	t.Run("a change after approval is never consumed", func(t *testing.T) { changedAfterApproval(t, open(t)) })
	t.Run("a deployment outage denies without invalidating", func(t *testing.T) { deploymentOutage(t, open(t)) })
	t.Run("a revoked reviewer is never consumed", func(t *testing.T) { revokedReviewer(t, open(t)) })
	t.Run("expiry denies without invalidating", func(t *testing.T) { expiry(t, open(t)) })
	t.Run("returned and supplied values are detached from the store", func(t *testing.T) { detached(t, open(t)) })
	t.Run("an agent candidate needs applicable conformance evidence", func(t *testing.T) { conformance(t, open) })
}

// ---- helpers ---------------------------------------------------------------------

func caller(principal string) gates.Caller {
	return gates.Caller{PrincipalID: principal, RequestID: "req_contract"}
}

func requested(t *testing.T, h Harness, c Case) *gates.Gate {
	t.Helper()
	g, created, err := h.Service().RequestForStep(context.Background(), c.Scope, c.Run, c.Step)
	if err != nil {
		t.Fatalf("request the gate: %v", err)
	}
	if !created || g.Status != gates.StatusPending || g.Version != 1 {
		t.Fatalf("a new gate must be pending at version 1: %+v created=%v", g, created)
	}
	return g
}

func cmd(g *gates.Gate, d gates.Decision) gates.DecideCommand {
	return gates.DecideCommand{Decision: d, Reason: reason, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest()}
}

func decide(h Harness, principal string, g *gates.Gate, d gates.Decision) (*gates.Gate, error) {
	return h.Service().Decide(context.Background(), caller(principal), g.Scope, g.ID, cmd(g, d))
}

func reload(t *testing.T, h Harness, g *gates.Gate) *gates.Gate {
	t.Helper()
	cur, err := h.Store().Get(context.Background(), g.Scope, g.ID)
	if err != nil {
		t.Fatalf("reload %s: %v", g.ID, err)
	}
	return cur
}

func process(t *testing.T, h Harness, g *gates.Gate) gates.Report {
	t.Helper()
	rep, err := h.Service().ProcessGate(context.Background(), g.Scope, g.ID)
	if err != nil {
		t.Fatalf("process %s: %v", g.ID, err)
	}
	return rep
}

func mustStale(t *testing.T, err error, reason string, definitive bool) {
	t.Helper()
	var se *gates.StaleError
	if !errors.As(err, &se) || !errors.Is(err, gates.ErrStaleSubject) || se.Reason != reason || se.Definitive != definitive {
		t.Fatalf("want stale %q (definitive=%v), got %v", reason, definitive, err)
	}
}

func noAttempt(t *testing.T, h Harness, c Case) {
	t.Helper()
	if n := h.Attempts(t, c); n != 0 {
		t.Fatalf("a gate step must never have an attempt, has %d", n)
	}
}

// ---- subtests ----------------------------------------------------------------------

func requestScoped(t *testing.T, h Harness) {
	ctx := context.Background()
	c := h.NewCase(t, false)
	p := h.Principals()

	ready, err := h.Store().ReadyGateSteps(ctx, c.Scope.TenantID, gates.StepRef{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range ready {
		found = found || (r.RunID == c.Run && r.StepID == c.Step && r.Scope == c.Scope)
	}
	if !found {
		t.Fatalf("the ready gate step is not listed: %+v", ready)
	}

	g := requested(t, h, c)
	again, created, err := h.Service().RequestForStep(ctx, c.Scope, c.Run, c.Step)
	if err != nil || created || again.ID != g.ID || again.Version != g.Version || again.SubjectDigest() != g.SubjectDigest() {
		t.Fatalf("a repeated request must return the same gate: %v created=%v", err, created)
	}
	if g.ID != gates.GateIDFor(c.Scope, c.Run, c.Step) {
		t.Fatalf("the gate identity is not derived from the run step")
	}
	if got := h.Events(t, c, g.ID); got != (Events{Requested: 1}) {
		t.Fatalf("events %+v, want exactly one gate.requested", got)
	}
	if h.StepStatus(t, c) != execution.StepPending {
		t.Fatalf("a requested gate step must keep waiting")
	}
	if g.Required.Conformance != nil {
		t.Fatalf("a non-agent candidate needs no conformance report: %+v", g.Required.Conformance)
	}
	noAttempt(t, h, c)

	// The gate is bound to its step: it leaves the ready scan and is open.
	ready, err = h.Store().ReadyGateSteps(ctx, c.Scope.TenantID, gates.StepRef{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ready {
		if r.RunID == c.Run && r.StepID == c.Step {
			t.Fatalf("a bound gate step is still listed ready")
		}
	}
	open, err := h.Store().OpenGates(ctx, c.Scope.TenantID, gates.GateRef{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	openFound := false
	for _, r := range open {
		openFound = openFound || r.ID == g.ID
	}
	if !openFound {
		t.Fatalf("the pending gate is not open: %+v", open)
	}

	// Reads: the project's readers see it, a stranger and another tenant do not.
	if got, err := h.Service().Get(ctx, caller(p.Engineer), c.Scope, g.ID); err != nil || got.ID != g.ID {
		t.Fatalf("a project member reads the gate: %v", err)
	}
	if _, err := h.Service().Get(ctx, caller(p.Stranger), c.Scope, g.ID); !errors.Is(err, gates.ErrHidden) {
		t.Fatalf("a stranger reads the gate: %v", err)
	}
	if _, err := h.Service().Get(ctx, caller(p.Foreign), c.Scope, g.ID); !errors.Is(err, gates.ErrHidden) {
		t.Fatalf("a foreign reviewer reads the gate: %v", err)
	}
	if _, err := h.Store().Get(ctx, h.Foreign(), g.ID); !errors.Is(err, gates.ErrNotFound) {
		t.Fatalf("another tenant's store read: %v", err)
	}
	if _, err := h.Store().ByStep(ctx, h.Foreign(), c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
		t.Fatalf("another tenant's step read: %v", err)
	}
	foreign, err := h.Store().List(ctx, h.Foreign(), gates.ListQuery{Limit: 100})
	if err != nil || len(foreign) != 0 {
		t.Fatalf("another tenant lists %d gates: %v", len(foreign), err)
	}
	mine, err := h.Store().List(ctx, c.Scope, gates.ListQuery{Statuses: []gates.Status{gates.StatusPending}, RunID: c.Run, Limit: 100})
	if err != nil || len(mine) != 1 || mine[0].ID != g.ID {
		t.Fatalf("list %v: %v", mine, err)
	}
	if err := h.Store().Reevaluate(ctx, c.Scope, g.ID); err != nil {
		t.Fatalf("a current gate is not stale: %v", err)
	}
}

func onlyReviewer(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	p := h.Principals()
	for who, want := range map[string]error{p.Engineer: gates.ErrDenied, p.Stranger: gates.ErrHidden, p.Foreign: gates.ErrHidden} {
		if _, err := decide(h, who, g, gates.DecisionApprove); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", who, err, want)
		}
	}
	cur := reload(t, h, g)
	if cur.Status != gates.StatusPending || cur.Version != g.Version || cur.Decision != nil {
		t.Fatalf("a refused decision changed the gate: %+v", cur)
	}
	if got := h.Events(t, c, g.ID); got.Decided != 0 {
		t.Fatalf("events %+v after refused decisions", got)
	}
	// A service identity cannot decide even with the right version and digest.
	_, err := h.Store().Decide(context.Background(), &gates.DecisionPlan{Scope: c.Scope, GateID: g.ID, Command: gates.DecideCommand{
		Decision: gates.DecisionApprove, Reason: "automatic", Actor: gates.System, ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
	}})
	if !errors.Is(err, gates.ErrDenied) {
		t.Fatalf("a service identity decided: %v", err)
	}
	if got, err := decide(h, p.Reviewer, g, gates.DecisionApprove); err != nil || got.Decision.By.ID != p.Reviewer || got.Decision.By.Type != gates.ActorUser {
		t.Fatalf("the reviewer: %v", err)
	}
}

func staleExpectations(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	p := h.Principals()
	wrongVersion := cmd(g, gates.DecisionApprove)
	wrongVersion.ExpectedVersion = g.Version + 1
	if _, err := h.Service().Decide(context.Background(), caller(p.Reviewer), g.Scope, g.ID, wrongVersion); !errors.Is(err, gates.ErrVersionConflict) {
		t.Fatalf("a stale version: %v", err)
	}
	wrongDigest := cmd(g, gates.DecisionApprove)
	wrongDigest.SubjectDigest = "sha256:" + strings.Repeat("0", 64)
	_, err := h.Service().Decide(context.Background(), caller(p.Reviewer), g.Scope, g.ID, wrongDigest)
	mustStale(t, err, "subject_digest_mismatch", false)
	cur := reload(t, h, g)
	if cur.Status != gates.StatusPending || cur.Invalidation != nil || cur.Version != g.Version {
		t.Fatalf("a request-side mismatch is not a definitive change: %+v", cur)
	}
	if got := h.Events(t, c, g.ID); got != (Events{Requested: 1}) {
		t.Fatalf("events %+v", got)
	}
}

func approveProgresses(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	p := h.Principals()
	// Waiting is explicit: until a decision the step stays pending, and a
	// pass over the pending gate neither completes nor invalidates it.
	if rep := process(t, h, g); rep.Waiting != 1 || rep.Completed != 0 || rep.Invalidated != 0 || rep.Failed != 0 {
		t.Fatalf("pending gate: %+v", rep)
	}
	got, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
	if err != nil || got.Status != gates.StatusApproved || got.Version != g.Version+1 {
		t.Fatalf("approve: %v %+v", err, got)
	}
	// The decision is durable before progression: the step still waits.
	if h.StepStatus(t, c) != execution.StepPending {
		t.Fatalf("a decision must not move the step by itself")
	}
	if rep := process(t, h, g); rep.Completed != 1 {
		t.Fatalf("progression: %+v", rep)
	}
	if h.StepStatus(t, c) != execution.StepSucceeded {
		t.Fatalf("the gate step did not succeed")
	}
	noAttempt(t, h, c)
	// Redelivery: nothing completes again, nothing is rewritten.
	for i := 0; i < 2; i++ {
		if rep := process(t, h, g); rep.Completed != 0 || rep.Failed != 0 {
			t.Fatalf("redelivery %d: %+v", i, rep)
		}
	}
	cur := reload(t, h, g)
	if cur.Status != gates.StatusApproved || cur.Version != got.Version || cur.Invalidation != nil || *cur.Decision != *got.Decision {
		t.Fatalf("redelivery rewrote the gate: %+v", cur)
	}
	if ev := h.Events(t, c, g.ID); ev != (Events{Requested: 1, Decided: 1}) {
		t.Fatalf("events %+v", ev)
	}
}

func repeatedDecisions(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	p := h.Principals()
	first, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	// The same command again (a client retry) carries the old version.
	if _, err := decide(h, p.Reviewer, g, gates.DecisionApprove); !errors.Is(err, gates.ErrVersionConflict) {
		t.Fatalf("replayed command: %v", err)
	}
	// A fresh command on the current version cannot decide twice, in either
	// direction, and a rejection never overturns an approval.
	for _, d := range []gates.Decision{gates.DecisionApprove, gates.DecisionReject} {
		if _, err := decide(h, p.Reviewer, first, d); !errors.Is(err, gates.ErrInvalidTransition) {
			t.Fatalf("second %s on a decided gate: %v", d, err)
		}
	}
	cur := reload(t, h, g)
	if cur.Status != gates.StatusApproved || cur.Version != first.Version || *cur.Decision != *first.Decision {
		t.Fatalf("a repeated decision changed the gate: %+v", cur)
	}
	if ev := h.Events(t, c, g.ID); ev != (Events{Requested: 1, Decided: 1}) {
		t.Fatalf("events %+v: repeated decisions must record one decision", ev)
	}
	// The one approval progresses the step exactly once.
	if rep := process(t, h, g); rep.Completed != 1 {
		t.Fatalf("progression: %+v", rep)
	}
	if _, err := decide(h, p.Reviewer, cur, gates.DecisionReject); err == nil {
		t.Fatalf("a rejection after progression was accepted")
	}
	if h.StepStatus(t, c) != execution.StepSucceeded {
		t.Fatalf("progression was undone")
	}
}

func rejection(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	p := h.Principals()
	got, err := decide(h, p.Reviewer, g, gates.DecisionReject)
	if err != nil || got.Status != gates.StatusRejected {
		t.Fatalf("reject: %v", err)
	}
	if h.StepStatus(t, c) != execution.StepPending {
		t.Fatalf("a decision must not move the step by itself")
	}
	if rep := process(t, h, g); rep.Rejected != 1 {
		t.Fatalf("rejection progression: %+v", rep)
	}
	if h.StepStatus(t, c) != execution.StepFailed {
		t.Fatalf("a rejected gate must fail its step: %s", h.StepStatus(t, c))
	}
	if rep := process(t, h, g); rep.Rejected != 0 || rep.Failed != 0 {
		t.Fatalf("redelivery: %+v", rep)
	}
	// Rejection is final: rework is a new run.
	if _, err := decide(h, p.Reviewer, got, gates.DecisionApprove); !errors.Is(err, gates.ErrInvalidTransition) {
		t.Fatalf("approving a rejected gate: %v", err)
	}
	noAttempt(t, h, c)
}

type change struct {
	name       string
	deployment bool
	apply      func(Harness, testing.TB, Case)
	reason     string
}

var changes = []change{
	{"content", false, Harness.ChangeContent, gates.ReasonContentChanged},
	{"evidence", false, Harness.ChangeEvidence, gates.ReasonEvidenceChanged},
	{"deployment configuration", true, Harness.ChangeDeployment, gates.ReasonDeploymentChanged},
}

func changedSubject(t *testing.T, h Harness) {
	for _, ch := range changes {
		t.Run(ch.name, func(t *testing.T) {
			c := h.NewCase(t, ch.deployment)
			g := requested(t, h, c)
			p := h.Principals()
			ch.apply(h, t, c)
			_, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
			mustStale(t, err, ch.reason, true)
			cur := reload(t, h, g)
			if cur.Status != gates.StatusPending || cur.Decision != nil {
				t.Fatalf("a stale decision was recorded: %+v", cur)
			}
			if cur.Invalidation == nil || cur.Invalidation.Reason != ch.reason {
				t.Fatalf("the definitive staleness was not recorded: %+v", cur.Invalidation)
			}
			// Recorded once: a repeat and a pass over the gate add nothing.
			if _, err := decide(h, p.Reviewer, cur, gates.DecisionApprove); !errors.Is(err, gates.ErrStaleSubject) {
				t.Fatalf("approving an invalidated gate: %v", err)
			}
			process(t, h, g)
			if ev := h.Events(t, c, g.ID); ev != (Events{Requested: 1, Invalidated: 1}) {
				t.Fatalf("events %+v", ev)
			}
			if h.StepStatus(t, c) != execution.StepPending {
				t.Fatalf("a stale gate moved its step")
			}
			noAttempt(t, h, c)
		})
	}
}

func changedAfterApproval(t *testing.T, h Harness) {
	for _, ch := range changes {
		t.Run(ch.name, func(t *testing.T) {
			c := h.NewCase(t, ch.deployment)
			g := requested(t, h, c)
			p := h.Principals()
			approved, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
			if err != nil {
				t.Fatal(err)
			}
			ch.apply(h, t, c)
			rep := process(t, h, g)
			if rep.Completed != 0 || rep.Invalidated != 1 {
				t.Fatalf("pass over a stale approval: %+v", rep)
			}
			if h.StepStatus(t, c) != execution.StepPending {
				t.Fatalf("a stale approval completed the gate step")
			}
			cur := reload(t, h, g)
			if cur.Status != gates.StatusApproved || *cur.Decision != *approved.Decision || cur.Invalidation == nil || cur.Invalidation.Reason != ch.reason {
				t.Fatalf("the decision must stay with a separate invalidation: %+v", cur)
			}
			// A repeat records nothing more.
			if rep := process(t, h, g); rep.Invalidated != 0 || rep.Completed != 0 {
				t.Fatalf("repeat: %+v", rep)
			}
			if ev := h.Events(t, c, g.ID); ev != (Events{Requested: 1, Decided: 1, Invalidated: 1}) {
				t.Fatalf("events %+v", ev)
			}
		})
	}
}

func deploymentOutage(t *testing.T, h Harness) {
	c := h.NewCase(t, true)
	g := requested(t, h, c)
	p := h.Principals()
	if _, err := decide(h, p.Reviewer, g, gates.DecisionApprove); err != nil {
		t.Fatal(err)
	}
	h.DeploymentOutage(t, true)
	defer h.DeploymentOutage(t, false)
	rep, err := h.Service().ProcessGate(context.Background(), g.Scope, g.ID)
	if err == nil || rep.Completed != 0 || rep.Invalidated != 0 || rep.Failed == 0 {
		t.Fatalf("an outage must deny progression and record nothing: %+v %v", rep, err)
	}
	cur := reload(t, h, g)
	if cur.Invalidation != nil || h.StepStatus(t, c) != execution.StepPending {
		t.Fatalf("an outage fabricated an invalidation or moved the step: %+v", cur)
	}
	h.DeploymentOutage(t, false)
	if rep := process(t, h, g); rep.Completed != 1 {
		t.Fatalf("after recovery: %+v", rep)
	}
}

func revokedReviewer(t *testing.T, h Harness) {
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	const leaving = "u_contract_leaving"
	h.Grant(t, leaving)
	if _, err := decide(h, leaving, g, gates.DecisionApprove); err != nil {
		t.Fatal(err)
	}
	h.Revoke(t, leaving)
	// Current authority is the rule: the approval of a reviewer who no longer
	// holds the authority is neither consumed nor invalidated; it waits.
	rep := process(t, h, g)
	if rep.Completed != 0 || rep.Invalidated != 0 || rep.Waiting != 1 {
		t.Fatalf("a revoked reviewer's approval: %+v", rep)
	}
	cur := reload(t, h, g)
	if cur.Status != gates.StatusApproved || cur.Invalidation != nil || h.StepStatus(t, c) != execution.StepPending {
		t.Fatalf("an authority check must not invalidate or rewrite the decision: %+v", cur)
	}
	noAttempt(t, h, c)
	// Restoring the reviewer restores the right to consume the same decision.
	h.Grant(t, leaving)
	if rep := process(t, h, g); rep.Completed != 1 {
		t.Fatalf("after the reviewer was restored: %+v", rep)
	}
	if h.StepStatus(t, c) != execution.StepSucceeded {
		t.Fatalf("the restored approval did not progress")
	}

	// A revoked reviewer cannot decide at all.
	c2 := h.NewCase(t, false)
	g2 := requested(t, h, c2)
	h.Revoke(t, leaving)
	if _, err := decide(h, leaving, g2, gates.DecisionApprove); !errors.Is(err, gates.ErrHidden) {
		t.Fatalf("a revoked reviewer decided: %v", err)
	}
	if cur := reload(t, h, g2); cur.Status != gates.StatusPending {
		t.Fatalf("gate %+v", cur)
	}
}

func expiry(t *testing.T, h Harness) {
	p := h.Principals()
	// Before the decision: the deadline passes.
	c := h.NewCase(t, false)
	g := requested(t, h, c)
	h.Advance(t, h.TTL())
	_, err := decide(h, p.Reviewer, g, gates.DecisionApprove)
	mustStale(t, err, gates.ReasonExpired, false)
	if cur := reload(t, h, g); cur.Status != gates.StatusPending || cur.Invalidation != nil {
		t.Fatalf("an expiry is derived, never written: %+v", cur)
	}

	// After the decision: an approval that reached its deadline never
	// authorizes progression and cannot be renewed by a retry.
	c2 := h.NewCase(t, false)
	g2 := requested(t, h, c2)
	if _, err := decide(h, p.Reviewer, g2, gates.DecisionApprove); err != nil {
		t.Fatal(err)
	}
	h.Advance(t, h.TTL())
	for i := 0; i < 2; i++ {
		rep := process(t, h, g2)
		if rep.Completed != 0 || rep.Invalidated != 0 || rep.Waiting != 1 {
			t.Fatalf("pass %d over an expired approval: %+v", i, rep)
		}
	}
	cur := reload(t, h, g2)
	if cur.Status != gates.StatusApproved || cur.Invalidation != nil || h.StepStatus(t, c2) != execution.StepPending {
		t.Fatalf("expiry must not rewrite the approval or move the step: %+v", cur)
	}
}

// detached (DQ-03, SD-14): the store never shares mutable state with a caller.
// Changing a value a call returned, or one that was supplied to it, changes
// nothing persisted: not the decision (so not the reviewer authority consumption
// rechecks), not the evidence snapshot, the version or the events.
func detached(t *testing.T, h Harness) {
	ctx := context.Background()
	p := h.Principals()
	c := h.NewCase(t, false)
	g := requested(t, h, c)

	tamper := func(x *gates.Gate) {
		if x == nil {
			t.Fatalf("no gate to tamper with")
		}
		for i := range x.Required.Evidence {
			x.Required.Evidence[i].Digest = "sha256:" + strings.Repeat("e", 64)
			x.Required.Evidence[i].ID = "art_tampered"
		}
		x.Required.Evidence = append(x.Required.Evidence, gates.RequiredEvidence{Role: gates.RoleSupport, ID: "art_extra"})
		if x.Required.Conformance != nil {
			x.Required.Conformance.Status = gates.ConformanceBlocked
			for i := range x.Required.Conformance.RequiredChecks {
				x.Required.Conformance.RequiredChecks[i] = "tampered"
			}
		}
		if x.Decision != nil {
			x.Decision.By.ID, x.Decision.By.Type, x.Decision.Reason, x.Decision.Decision = "u_substitute", gates.ActorService, "tampered", gates.DecisionReject
		}
		if x.Invalidation != nil {
			x.Invalidation.Reason = "tampered"
		}
		x.Status, x.Version = gates.StatusRejected, 99
	}
	same := func(label string, want *gates.Gate) {
		t.Helper()
		cur := reload(t, h, want)
		if !reflect.DeepEqual(cur, want) {
			t.Fatalf("%s: the stored gate changed:\n got %+v\nwant %+v", label, cur, want)
		}
	}

	// A returned pending gate.
	pristine := reload(t, h, g)
	tamper(g)
	same("the gate returned by the request", pristine)
	for label, get := range map[string]func() *gates.Gate{
		"Get":    func() *gates.Gate { return reload(t, h, pristine) },
		"ByStep": func() *gates.Gate { x, err := h.Store().ByStep(ctx, c.Scope, c.Run, c.Step); must(t, err); return x },
		"List": func() *gates.Gate {
			xs, err := h.Store().List(ctx, c.Scope, gates.ListQuery{RunID: c.Run, Limit: 10})
			must(t, err)
			if len(xs) != 1 {
				t.Fatalf("list %d", len(xs))
			}
			return xs[0]
		},
	} {
		tamper(get())
		same("a gate returned by "+label, pristine)
	}

	// A returned decision: changing the recorded reviewer must neither change
	// the persisted reviewer nor make consumption recheck a substitute.
	approved, err := decide(h, p.Reviewer, pristine, gates.DecisionApprove)
	must(t, err)
	stored := reload(t, h, approved)
	events := h.Events(t, c, g.ID)
	tamper(approved)
	same("the gate returned by the decision", stored)
	tamper(reload(t, h, stored))
	same("a decided gate returned by Get", stored)
	if cur := reload(t, h, stored); cur.Decision.By.ID != p.Reviewer || cur.Decision.By.Type != gates.ActorUser || cur.Version != stored.Version {
		t.Fatalf("the persisted decision was substituted: %+v", cur.Decision)
	}
	if got := h.Events(t, c, g.ID); got != events {
		t.Fatalf("events changed without a transaction: %+v -> %+v", events, got)
	}
	if rep := process(t, h, stored); rep.Completed != 1 {
		t.Fatalf("consumption must recheck the original reviewer: %+v", rep)
	}
	same("the gate after consumption", stored)

	// The conformance snapshot of an agent candidate.
	base := mustBaseline(t, "SD-01")
	ca := h.NewAgentCase(t, AgentSpec{Baseline: &base, Declare: true, Supports: 2, Report: good})
	ga := requested(t, h, ca)
	if ga.Required.Conformance == nil || len(ga.Required.Conformance.RequiredChecks) == 0 {
		t.Fatalf("no conformance snapshot: %+v", ga.Required)
	}
	wantA := reload(t, h, ga)
	tamper(ga)
	tamper(reload(t, h, wantA))
	same("an agent candidate's gate", wantA)
	if _, err := decide(h, p.Reviewer, wantA, gates.DecisionApprove); err != nil {
		t.Fatalf("the verified snapshot was tampered with: %v", err)
	}

	// A returned invalidation.
	c2 := h.NewCase(t, false)
	g2 := requested(t, h, c2)
	h.ChangeContent(t, c2)
	_, err = decide(h, p.Reviewer, g2, gates.DecisionApprove)
	mustStale(t, err, gates.ReasonContentChanged, true)
	inv := reload(t, h, g2)
	if inv.Invalidation == nil {
		t.Fatalf("no invalidation recorded")
	}
	want := reload(t, h, g2)
	tamper(inv)
	same("a gate with an invalidation", want)
	if want.Invalidation.Reason != gates.ReasonContentChanged {
		t.Fatalf("invalidation %+v", want.Invalidation)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
