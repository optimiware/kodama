package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters/ledgertest"
	gwpg "github.com/optimiware/kodama/internal/integrations/gitwebhook/postgres"
)

// ledgerEnv is the PostgreSQL implementation of ledgertest.Env over the real
// execution store.
type ledgerEnv struct{ *env }

var _ ledgertest.Env = ledgerEnv{}

func (l ledgerEnv) Ledger() adapters.Ledger { return l.led }

// Restart is a ledger over new connections: only committed state survives.
func (l ledgerEnv) Restart() adapters.Ledger {
	led, err := gwpg.NewLedger(l.restartedPool(), l.x.Store)
	if err != nil {
		l.t.Fatal(err)
	}
	return led
}

func (l ledgerEnv) Plan(_ testing.TB, scope domain.Scope) *execution.AdmissionPlan {
	return l.plan(scope)
}

// RefusedPlan is a plan whose installer loses the run authority after planning:
// the admission transaction rechecks it and refuses.
func (l ledgerEnv) RefusedPlan(_ testing.TB, scope domain.Scope) *execution.AdmissionPlan {
	plan := l.plan(scope)
	l.x.Revoke(scope, executiontest.Engineer)
	return plan
}

func (l ledgerEnv) Runs(_ testing.TB, scope domain.Scope) int { return l.runs(scope) }
func (l ledgerEnv) ScopeA() domain.Scope                      { return executiontest.ScopeA }
func (l ledgerEnv) ScopeB() domain.Scope                      { return executiontest.ScopeB }

// TestPostgresLedgerContract runs the refusal-ledger contract that the memory
// ledger runs, against the real execution admission.
func TestPostgresLedgerContract(t *testing.T) {
	ledgertest.Contract(t, func(t testing.TB) ledgertest.Env { return ledgerEnv{newEnv(t)} })
}

// failingAdmitter creates the run through the real store and then fails, as a
// later statement of the admission transaction would.
type failingAdmitter struct {
	inner gwpg.Admitter
	err   error
}

func (f failingAdmitter) AdmitInTx(ctx context.Context, tx pgx.Tx, plan *execution.AdmissionPlan) (*execution.Run, bool, error) {
	if _, _, err := f.inner.AdmitInTx(ctx, tx, plan); err != nil {
		return nil, false, err
	}
	return nil, false, f.err
}

func TestPostgresLedgerAtomicity(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	scope := executiontest.ScopeA
	const key = "git-hook:trg1:dlv1"

	t.Run("a failure after the run was created rolls the run and the key back", func(t *testing.T) {
		led, err := gwpg.NewLedger(e.x.Runtime, failingAdmitter{inner: e.x.Store, err: execution.Unavailablef(nil, "later statement failed")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := led.Admit(ctx, scope, key, e.plan(scope)); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("admit = %v", err)
		}
		if n := e.runs(scope); n != 0 {
			t.Fatalf("%d run(s) survived a rolled-back admission", n)
		}
		if _, found, _ := e.led.Lookup(ctx, scope, key); found {
			t.Fatal("the key was decided by a rolled-back admission")
		}
		adm, err := e.led.Admit(ctx, scope, key, e.plan(scope))
		if err != nil || !adm.Created || e.runs(scope) != 1 {
			t.Fatalf("retry = %+v, %v (runs %d)", adm, err, e.runs(scope))
		}
	})

	t.Run("a lost commit acknowledgement is recovered without a second run", func(t *testing.T) {
		lost := &lostCommit{inner: e.x.Runtime}
		led, err := gwpg.NewLedger(lost, e.x.Store)
		if err != nil {
			t.Fatal(err)
		}
		const k = "git-hook:trg1:dlv-lost"
		before := e.runs(scope)
		lost.armed.Store(true)
		if _, err := led.Admit(ctx, scope, k, e.plan(scope)); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("admit with a lost acknowledgement = %v, want ErrUnavailable", err)
		}
		entry, found, err := e.led.Lookup(ctx, scope, k)
		if err != nil || !found || entry.RunID == "" {
			t.Fatalf("the commit landed, so the key names its run: %+v %v %v", entry, found, err)
		}
		again, err := e.led.Admit(ctx, scope, k, e.plan(scope))
		if err != nil || again.Created || again.RunID != entry.RunID {
			t.Fatalf("retry = %+v, %v; want the landed run", again, err)
		}
		if n := e.runs(scope) - before; n != 1 {
			t.Fatalf("%d runs were created for the key, want 1", n)
		}
		// A lost refusal acknowledgement likewise leaves the refusal durable.
		const kr = "git-hook:trg1:dlv-lost-refusal"
		lost.armed.Store(true)
		if _, err := mustDecide(led, scope, kr); !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("decide with a lost acknowledgement = %v", err)
		}
		if got, found, _ := e.led.Lookup(ctx, scope, kr); !found || got.Refusal != "admission_refused" {
			t.Fatalf("the refusal must be durable: %+v %v", got, found)
		}
	})

	t.Run("the schema rejects a decision that is both, or changed", func(t *testing.T) {
		run := e.newRun(scope)
		tenant, project := string(scope.TenantID), string(scope.ProjectID)
		err := e.x.OwnerExecErr(`INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, run_id, refusal_reason) VALUES ($1, $2, 'both', $3, 'x')`, tenant, project, string(run))
		if err == nil || !strings.Contains(err.Error(), "git_trigger_admissions_exclusive") {
			t.Fatalf("a decision naming a run and a refusal = %v", err)
		}
		if err := e.x.OwnerExecErr(`INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key) VALUES ($1, $2, 'neither')`, tenant, project); err == nil {
			t.Fatal("a decision naming neither a run nor a refusal was accepted")
		}
		e.owner(`INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, run_id) VALUES ($1, $2, 'one', $3)`, tenant, project, string(run))
		if err := e.x.OwnerExecErr(`INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, run_id) VALUES ($1, $2, 'two', $3)`, tenant, project, string(run)); err == nil {
			t.Fatal("a run was recorded under two keys")
		}
		// A run of another tenant cannot be recorded in this tenant's ledger.
		foreign := e.newRun(executiontest.ScopeB)
		if err := e.x.OwnerExecErr(`INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, run_id) VALUES ($1, $2, 'foreign', $3)`, tenant, project, string(foreign)); err == nil {
			t.Fatal("another tenant's run was recorded")
		}
		// The runtime role can neither change nor delete a decision.
		for _, stmt := range []string{
			`UPDATE git_trigger_admissions SET refusal_reason = 'x', run_id = NULL WHERE idempotency_key = 'one'`,
			`DELETE FROM git_trigger_admissions WHERE idempotency_key = 'one'`,
		} {
			err := e.inTenant(scope.TenantID, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("%s: %v, want permission denied", stmt, err)
			}
		}
	})
}

func mustDecide(l *gwpg.Ledger, scope domain.Scope, key string) (adapters.Entry, error) {
	return l.Decide(context.Background(), scope, key, "admission_refused")
}
