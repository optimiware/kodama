package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
)

// Env is one ledger implementation under test, with the persistent store it
// runs over.
type Env interface {
	// Ledger returns a handle over the shared store.
	Ledger() adapters.Ledger
	// Restart returns a new handle over the same store, as after a process
	// restart: only what was made durable survives.
	Restart() adapters.Ledger
	// Plan returns a fresh admission plan creating a distinct run in scope.
	Plan(t testing.TB, scope domain.Scope) *execution.AdmissionPlan
	// RefusedPlan returns a plan whose admission fails with one of execution's
	// classified errors, or nil when the implementation cannot produce one.
	RefusedPlan(t testing.TB, scope domain.Scope) *execution.AdmissionPlan
	// Runs counts the runs that exist in scope.
	Runs(t testing.TB, scope domain.Scope) int
	// ScopeA and ScopeB are two scopes of different tenants.
	ScopeA() domain.Scope
	ScopeB() domain.Scope
}

// Open returns a fresh Env.
type Open func(t testing.TB) Env

// Contract is the behavior every adapters.Ledger has: keys are decided once and
// exclusively, decisions are durable and scoped, and a refusal closes admission
// for every later caller, however stale its plan.
func Contract(t *testing.T, open Open) {
	ctx := context.Background()
	const key = "git-hook:trg1:dlv1"

	t.Run("a run is created once per key and a replay returns it", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		first, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a))
		if err != nil || !first.Created || first.RunID == "" {
			t.Fatalf("first admit = %+v, %v", first, err)
		}
		again, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a))
		if err != nil || again.Created || again.RunID != first.RunID {
			t.Fatalf("replay = %+v, %v; want run %s not created", again, err, first.RunID)
		}
		if n := e.Runs(t, a); n != 1 {
			t.Fatalf("%d runs exist, want 1", n)
		}
		got, found, err := e.Ledger().Lookup(ctx, a, key)
		if err != nil || !found || got.RunID != first.RunID || got.Refusal != "" {
			t.Fatalf("lookup = %+v %v %v", got, found, err)
		}
	})

	t.Run("concurrent identical admissions create one run", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		const n = 8
		results := make([]adapters.Admission, n)
		errs := make([]error, n)
		plans := make([]*execution.AdmissionPlan, n)
		for i := range plans {
			plans[i] = e.Plan(t, a)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = e.Ledger().Admit(ctx, a, key, plans[i])
			}()
		}
		close(start)
		wg.Wait()
		created, run := 0, domain.RunID("")
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("admit %d: %v", i, errs[i])
			}
			if results[i].Created {
				created++
			}
			if run == "" {
				run = results[i].RunID
			}
			if results[i].RunID != run {
				t.Fatalf("callers got different runs: %s and %s", run, results[i].RunID)
			}
		}
		if created != 1 || e.Runs(t, a) != 1 {
			t.Fatalf("created=%d runs=%d, want exactly one", created, e.Runs(t, a))
		}
	})

	t.Run("a refusal closes the key and creates nothing", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		stale := e.Plan(t, a) // planned before the refusal, used after it
		got, err := e.Ledger().Decide(ctx, a, key, "admission_refused")
		if err != nil || got.Refusal != "admission_refused" || got.RunID != "" {
			t.Fatalf("decide = %+v, %v", got, err)
		}
		if _, err := e.Ledger().Admit(ctx, a, key, stale); !errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("a stale holder's admit = %v, want ErrAdmissionClosed", err)
		}
		if _, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a)); !errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("a fresh admit = %v, want ErrAdmissionClosed", err)
		}
		if n := e.Runs(t, a); n != 0 {
			t.Fatalf("%d runs exist after a terminal refusal", n)
		}
	})

	t.Run("the first refusal reason wins", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		if _, err := e.Ledger().Decide(ctx, a, key, "first"); err != nil {
			t.Fatal(err)
		}
		got, err := e.Ledger().Decide(ctx, a, key, "second")
		if err != nil || got.Refusal != "first" {
			t.Fatalf("second decide = %+v, %v; want the first reason", got, err)
		}
	})

	t.Run("an existing run wins over a refusal and nothing is recorded", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		adm, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a))
		if err != nil {
			t.Fatal(err)
		}
		got, err := e.Ledger().Decide(ctx, a, key, "admission_refused")
		if err != nil || got.RunID != adm.RunID || got.Refusal != "" {
			t.Fatalf("decide after a run = %+v, %v; want the run", got, err)
		}
		entry, _, _ := e.Ledger().Lookup(ctx, a, key)
		if entry.RunID != adm.RunID || entry.Refusal != "" {
			t.Fatalf("ledger = %+v: success and refusal must be exclusive", entry)
		}
	})

	t.Run("decisions are honoured after a restart", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		stale := e.Plan(t, a)
		if _, err := e.Ledger().Decide(ctx, a, key, "admission_refused"); err != nil {
			t.Fatal(err)
		}
		restarted := e.Restart()
		got, found, err := restarted.Lookup(ctx, a, key)
		if err != nil || !found || got.Refusal != "admission_refused" {
			t.Fatalf("refusal after restart = %+v %v %v", got, found, err)
		}
		if _, err := restarted.Admit(ctx, a, key, stale); !errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("admit after restart = %v, want ErrAdmissionClosed", err)
		}
		// A run decided before a restart is likewise returned, not recreated.
		const other = "git-hook:trg1:dlv2"
		adm, err := e.Ledger().Admit(ctx, a, other, e.Plan(t, a))
		if err != nil {
			t.Fatal(err)
		}
		again, err := e.Restart().Admit(ctx, a, other, e.Plan(t, a))
		if err != nil || again.Created || again.RunID != adm.RunID {
			t.Fatalf("replay after restart = %+v, %v", again, err)
		}
		if n := e.Runs(t, a); n != 1 {
			t.Fatalf("%d runs exist, want 1", n)
		}
	})

	t.Run("keys are scoped to their tenant", func(t *testing.T) {
		e := open(t)
		a, b := e.ScopeA(), e.ScopeB()
		if _, err := e.Ledger().Decide(ctx, a, key, "admission_refused"); err != nil {
			t.Fatal(err)
		}
		if _, found, err := e.Ledger().Lookup(ctx, b, key); err != nil || found {
			t.Fatalf("another tenant sees the decision: found=%v err=%v", found, err)
		}
		adm, err := e.Ledger().Admit(ctx, b, key, e.Plan(t, b))
		if err != nil || !adm.Created {
			t.Fatalf("the same key in another tenant is its own key: %+v, %v", adm, err)
		}
		if _, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a)); !errors.Is(err, gw.ErrAdmissionClosed) {
			t.Fatalf("tenant A's refusal must stand: %v", err)
		}
		if _, err := e.Ledger().Admit(ctx, a, "git-hook:trg1:dlv9", e.Plan(t, b)); err == nil {
			t.Fatal("a plan for another scope must be refused")
		}
	})

	t.Run("a refusal and an admission racing decide the key once", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		wantRuns := 0
		for i := range 12 {
			key := fmt.Sprintf("git-hook:trg1:race%d", i)
			plan := e.Plan(t, a)
			var wg sync.WaitGroup
			var adm adapters.Admission
			var admErr error
			var dec adapters.Entry
			var decErr error
			start := make(chan struct{})
			wg.Add(2)
			go func() { defer wg.Done(); <-start; adm, admErr = e.Ledger().Admit(ctx, a, key, plan) }()
			go func() { defer wg.Done(); <-start; dec, decErr = e.Ledger().Decide(ctx, a, key, "admission_refused") }()
			close(start)
			wg.Wait()
			if decErr != nil {
				t.Fatalf("round %d: decide: %v", i, decErr)
			}
			entry, found, err := e.Ledger().Lookup(ctx, a, key)
			if err != nil || !found {
				t.Fatalf("round %d: lookup %v %v", i, found, err)
			}
			switch {
			case admErr == nil:
				// The run won: the refusal must have returned it.
				wantRuns++
				if dec.RunID != adm.RunID || entry.RunID != adm.RunID || entry.Refusal != "" || e.Runs(t, a) != wantRuns {
					t.Fatalf("round %d: run won but dec=%+v entry=%+v runs=%d want %d", i, dec, entry, e.Runs(t, a), wantRuns)
				}
			case errors.Is(admErr, gw.ErrAdmissionClosed):
				if dec.Refusal != "admission_refused" || entry.Refusal != "admission_refused" || entry.RunID != "" || e.Runs(t, a) != wantRuns {
					t.Fatalf("round %d: refusal won but dec=%+v entry=%+v runs=%d want %d", i, dec, entry, e.Runs(t, a), wantRuns)
				}
			default:
				t.Fatalf("round %d: admit: %v", i, admErr)
			}
		}
	})

	t.Run("a failed run admission leaves the key undecided", func(t *testing.T) {
		e := open(t)
		a := e.ScopeA()
		bad := e.RefusedPlan(t, a)
		if bad == nil {
			t.Skip("this implementation cannot produce a refusing plan")
		}
		_, err := e.Ledger().Admit(ctx, a, key, bad)
		if err == nil || !execution.IsBusinessError(err) {
			t.Fatalf("admit of a refusing plan = %v, want an execution error", err)
		}
		if _, found, _ := e.Ledger().Lookup(ctx, a, key); found || e.Runs(t, a) != 0 {
			t.Fatalf("a failed admission must not decide the key (found=%v runs=%d)", found, e.Runs(t, a))
		}
		if adm, err := e.Ledger().Admit(ctx, a, key, e.Plan(t, a)); err != nil || !adm.Created {
			t.Fatalf("a later admission = %+v, %v", adm, err)
		}
	})
}
