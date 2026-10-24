package adapters_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters/ledgertest"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
)

// fakePlanner stands in for the core's admission use case: it returns a plan
// creating a distinct run, or the configured error.
type fakePlanner struct {
	mu    sync.Mutex
	calls int
	err   error
	// afterPlan, when set, runs after the plan is built and before Plan returns:
	// the point where a stalled holder has planned but not yet admitted.
	afterPlan func(call int)
}

func (p *fakePlanner) Plan(_ context.Context, req gw.RunRequest) (*execution.AdmissionPlan, error) {
	p.mu.Lock()
	p.calls++
	n, err, hook := p.calls, p.err, p.afterPlan
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	plan := &execution.AdmissionPlan{Aggregate: &execution.Aggregate{Run: &execution.Run{Scope: req.Scope, ID: domain.RunID(fmt.Sprintf("run_plan_%d", n))}}}
	if hook != nil {
		hook(n)
	}
	return plan, nil
}

func (p *fakePlanner) set(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }
func (p *fakePlanner) count() int    { p.mu.Lock(); defer p.mu.Unlock(); return p.calls }

type ledgerFixture struct {
	*gitwebhooktest.Fixture
	state   *ledgertest.State
	planner *fakePlanner
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	return &ledgerFixture{Fixture: gitwebhooktest.NewFixture(t), state: ledgertest.NewState(), planner: &fakePlanner{}}
}

// service returns a Service over the fixture's persistence and doubles whose run
// port is the adapter over the shared ledger state: each call models a process
// start.
func (f *ledgerFixture) service(t *testing.T) (*gw.Service, *adapters.Runs) {
	t.Helper()
	runs, err := adapters.NewRuns(f.planner, ledgertest.NewMemory(f.state))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := gw.New(gw.Dependencies{
		Store: f.Store, Authorizer: f.Auth, Secrets: f.Secrets, Definitions: f.Defs, Repositories: f.Repos,
		Git: f.Git, Runs: runs, Now: f.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, runs
}

func (f *ledgerFixture) push(t *testing.T, delivery string) gw.Notification {
	t.Helper()
	return f.Sign(delivery, gitwebhooktest.Marshal(t, f.Payload(gitwebhooktest.MainRef, gitwebhooktest.Zero, string(gitwebhooktest.CommitA))))
}

func (f *ledgerFixture) key(delivery string) gw.DeliveryKey {
	return gw.DeliveryKey{TriggerID: f.Trigger.ID, DeliveryID: delivery}
}

// Test_git_trigger_refusal_ledger_durable: a refusal recorded by one holder is
// honoured after a restart and by a stale holder (no run is created after a
// terminal refusal), and success and refusal are mutually exclusive outcomes.
func Test_git_trigger_refusal_ledger_durable(t *testing.T) {
	ctx := context.Background()

	t.Run("the memory ledger satisfies the contract", func(t *testing.T) {
		ledgertest.Contract(t, ledgertest.NewMemEnv)
	})

	t.Run("a stale holder cannot create a run after a terminal refusal", func(t *testing.T) {
		f := newLedgerFixture(t)
		svc, _ := f.service(t)
		n := f.push(t, "d1")

		// A reaches StartRun, plans, and stalls before admitting.
		planned, resumeA := make(chan struct{}), make(chan struct{})
		f.planner.afterPlan = func(call int) {
			if call == 1 {
				close(planned)
				<-resumeA
			}
		}
		type result struct {
			r   gw.Receipt
			err error
		}
		aDone := make(chan result, 1)
		go func() {
			r, err := svc.Receive(ctx, n)
			aDone <- result{r, err}
		}()
		<-planned

		// A's lease expires. B replaces it; admission permanently refuses.
		f.Clock.Advance(gw.AdmissionLease + time.Second)
		f.planner.set(fmt.Errorf("%w: the definition no longer admits", gw.ErrRunRefused))
		r, err := svc.Receive(ctx, f.push(t, "d1"))
		if !errors.Is(err, gw.ErrRejected) || r.Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("B: %+v, %v", r, err)
		}

		// The refusal is durable in the ledger; A resumes with its old plan.
		f.planner.set(nil)
		close(resumeA)
		a := <-aDone
		if !errors.Is(a.err, gw.ErrRejected) || a.r.RunID != "" {
			t.Fatalf("A must see the recorded refusal: %+v, %v", a.r, a.err)
		}
		if n := f.state.Runs(f.Scope); n != 0 {
			t.Fatalf("%d run(s) exist after a terminal refusal", n)
		}
		rec, _ := f.Store.Delivery(f.key("d1"))
		if !rec.Done || rec.Outcome != gw.OutcomeRejected || rec.RunID != "" {
			t.Fatalf("delivery: %+v", rec)
		}
	})

	t.Run("a refusal is honoured by a restarted process", func(t *testing.T) {
		f := newLedgerFixture(t)
		first, _ := f.service(t)
		f.planner.set(fmt.Errorf("%w: refused", gw.ErrRunRefused))
		if _, err := first.Receive(ctx, f.push(t, "d1")); !errors.Is(err, gw.ErrRejected) {
			t.Fatalf("first: %v", err)
		}
		planned := f.planner.count()

		// Restart, and lose the delivery's finish: only the ledger remembers.
		second, runs := f.service(t)
		f.planner.set(nil)
		if _, err := runs.StartRun(ctx, gw.RunRequest{
			Scope: f.Scope, DefinitionID: gitwebhooktest.DefinitionName, WorkItemID: "wi1", Commit: gitwebhooktest.CommitA, Ref: gitwebhooktest.MainRef,
			TriggerID: f.Trigger.ID, DeliveryID: "d1", IdempotencyKey: "git-hook:" + string(f.Trigger.ID) + ":d1", RequestedBy: gitwebhooktest.Installer, RepositoryID: gitwebhooktest.RepositoryName,
		}); !errors.Is(err, gw.ErrAdmissionClosed) || !errors.Is(err, gw.ErrRunRefused) {
			t.Fatalf("StartRun after restart = %v, want ErrAdmissionClosed", err)
		}
		if f.planner.count() != planned {
			t.Fatal("a closed key must be answered without planning again")
		}
		r, err := second.Receive(ctx, f.push(t, "d1"))
		if !errors.Is(err, gw.ErrRejected) || !r.Duplicate {
			t.Fatalf("replay after restart: %+v, %v", r, err)
		}
		if f.state.Runs(f.Scope) != 0 {
			t.Fatal("a run exists after a terminal refusal")
		}
	})

	t.Run("a started delivery is never later rejected and replays its run", func(t *testing.T) {
		f := newLedgerFixture(t)
		svc, runs := f.service(t)
		r, err := svc.Receive(ctx, f.push(t, "d1"))
		if err != nil || r.Outcome != gw.OutcomeStarted || r.RunID == "" {
			t.Fatalf("first: %+v, %v", r, err)
		}
		// The ledger now refuses to record a refusal for the key.
		dec, err := runs.DecideRefusal(ctx, f.Scope, "git-hook:"+string(f.Trigger.ID)+":d1", gw.ReasonAdmissionRefused)
		if err != nil || !dec.Found || dec.Run.ID != r.RunID {
			t.Fatalf("DecideRefusal after a run = %+v, %v; want the run", dec, err)
		}
		found, ok, err := runs.FindRun(ctx, f.Scope, "git-hook:"+string(f.Trigger.ID)+":d1")
		if err != nil || !ok || found.ID != r.RunID || found.Created {
			t.Fatalf("FindRun = %+v %v %v", found, ok, err)
		}
		// A restart and a revoked installer cannot turn it into a rejection.
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		again, _ := f.service(t)
		r2, err := again.Receive(ctx, f.push(t, "d1"))
		if err != nil || !r2.Duplicate || r2.RunID != r.RunID {
			t.Fatalf("replay: %+v, %v", r2, err)
		}
		if f.planner.count() != 1 || f.state.Runs(f.Scope) != 1 {
			t.Fatalf("planned %d times, %d runs: want one of each", f.planner.count(), f.state.Runs(f.Scope))
		}
	})

	t.Run("a lost admission response recovers the run through the ledger", func(t *testing.T) {
		f := newLedgerFixture(t)
		svc, _ := f.service(t)
		f.Store.FailFinishes = 1 // the run is admitted, but the delivery's finish is lost
		if _, err := svc.Receive(ctx, f.push(t, "d1")); err == nil {
			t.Fatal("the first receive must report the lost finish")
		}
		f.Auth.Revoke(gitwebhooktest.Installer, f.Scope, gw.ActionRun)
		f.Clock.Advance(time.Second)
		r, err := svc.Receive(ctx, f.push(t, "d1"))
		if err != nil || r.Outcome != gw.OutcomeStarted || r.RunID == "" {
			t.Fatalf("recovery: %+v, %v", r, err)
		}
		if f.state.Runs(f.Scope) != 1 || f.planner.count() != 1 {
			t.Fatalf("runs=%d planned=%d, want 1 and 1", f.state.Runs(f.Scope), f.planner.count())
		}
	})

	t.Run("StartRun is idempotent by key and distinguishes refusal from outage", func(t *testing.T) {
		f := newLedgerFixture(t)
		_, runs := f.service(t)
		req := gw.RunRequest{
			Scope: f.Scope, DefinitionID: gitwebhooktest.DefinitionName, WorkItemID: "wi1", Commit: gitwebhooktest.CommitA, Ref: gitwebhooktest.MainRef,
			TriggerID: f.Trigger.ID, DeliveryID: "d2", IdempotencyKey: "git-hook:k:d2", RequestedBy: gitwebhooktest.Installer, RepositoryID: gitwebhooktest.RepositoryName,
		}
		// An outage decides nothing.
		f.planner.set(errors.New("database is down"))
		if _, err := runs.StartRun(ctx, req); !errors.Is(err, gw.ErrUnavailable) || errors.Is(err, gw.ErrRunRefused) {
			t.Fatalf("outage = %v, want ErrUnavailable", err)
		}
		if _, ok, _ := runs.FindRun(ctx, f.Scope, req.IdempotencyKey); ok {
			t.Fatal("an outage must not decide the key")
		}
		f.planner.set(nil)
		first, err := runs.StartRun(ctx, req)
		if err != nil || !first.Created {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := runs.StartRun(ctx, req)
		if err != nil || second.Created || second.ID != first.ID {
			t.Fatalf("second = %+v, %v", second, err)
		}
		// Even when planning would now fail, the decided key answers.
		f.planner.set(fmt.Errorf("%w: now refused", gw.ErrRunRefused))
		third, err := runs.StartRun(ctx, req)
		if err != nil || third.ID != first.ID {
			t.Fatalf("a decided key must not re-plan: %+v, %v", third, err)
		}
		// A refusal of planning is permanent and is not a decision until recorded.
		req.IdempotencyKey = "git-hook:k:d3"
		if _, err := runs.StartRun(ctx, req); !errors.Is(err, gw.ErrRunRefused) || errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("planning refusal = %v, want ErrRunRefused", err)
		}
		if _, ok, _ := runs.FindRun(ctx, f.Scope, req.IdempotencyKey); ok {
			t.Fatal("a refusal is not a run")
		}
		dec, err := runs.DecideRefusal(ctx, f.Scope, req.IdempotencyKey, gw.ReasonAdmissionRefused)
		if err != nil || dec.Found || dec.Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("decide = %+v, %v", dec, err)
		}
		again, err := runs.DecideRefusal(ctx, f.Scope, req.IdempotencyKey, "other")
		if err != nil || again.Reason != gw.ReasonAdmissionRefused {
			t.Fatalf("the first reason wins: %+v, %v", again, err)
		}
		f.planner.set(nil)
		if _, err := runs.StartRun(ctx, req); !errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("after the decision = %v, want ErrAdmissionClosed", err)
		}
	})

	t.Run("the ledger is scoped to the tenant", func(t *testing.T) {
		f := newLedgerFixture(t)
		_, runs := f.service(t)
		key := "git-hook:k:d1"
		if _, err := runs.DecideRefusal(ctx, f.Scope, key, gw.ReasonAdmissionRefused); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := runs.FindRun(ctx, f.Other, key); ok || err != nil {
			t.Fatalf("another tenant sees a decision: %v %v", ok, err)
		}
		dec, err := runs.DecideRefusal(ctx, f.Other, key, "other_reason")
		if err != nil || dec.Reason != "other_reason" {
			t.Fatalf("another tenant's decision is its own: %+v, %v", dec, err)
		}
	})
}
