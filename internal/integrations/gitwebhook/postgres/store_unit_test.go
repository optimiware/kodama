package postgres_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
)

var memSeq atomic.Int64

var (
	memScopeA = domain.Scope{TenantID: "t100_acme", ProjectID: "p100_web"}
	memScopeB = domain.Scope{TenantID: "t100_globex", ProjectID: "p100_ops"}
)

func memTrigger(scope domain.Scope) gw.Trigger {
	return gw.Trigger{
		ID: gw.TriggerID(fmt.Sprintf("trg-mem-%d", memSeq.Add(1))), Scope: scope, RepositoryID: "repo-main", DefinitionID: "def-ci",
		Branches: []string{"main", "release/*"}, SecretRef: "secret://project/hook", InstalledBy: "u-installer", Enabled: true, Version: 1,
		CreatedAt: clock, UpdatedAt: clock,
	}
}

// openMemoryStore is the module's in-memory Store as a storeSubject: the unit
// layer proves the port contract the PostgreSQL store must also satisfy.
func openMemoryStore(testing.TB) storeSubject {
	st := gitwebhooktest.NewStore()
	return storeSubject{
		Store: st, Restart: func() gw.Store { return st }, ScopeA: memScopeA, ScopeB: memScopeB,
		NewTrigger: func(_ testing.TB, scope domain.Scope) gw.Trigger { return memTrigger(scope) },
		NewRun: func(testing.TB, domain.Scope) domain.RunID {
			return domain.RunID(fmt.Sprintf("run-mem-%d", memSeq.Add(1)))
		},
		Audits: func(_ testing.TB, scope domain.Scope, id gw.TriggerID) int {
			n := 0
			for _, a := range st.Audits() {
				if a.TriggerID == id && a.Scope == scope {
					n++
				}
			}
			return n
		},
	}
}

// Test_git_trigger_store_atomic_claim: two concurrent identical deliveries
// produce one claim and one outcome, a replay returns the stored outcome, and
// another tenant's trigger or delivery is never visible. This is the unit-layer
// proof of the port contract over the module's memory store; the same contract
// runs against PostgreSQL in TestPostgresStoreContract, and the SQL-level
// isolation (row level security, composite keys) in TestPostgresStoreIsolation.
func Test_git_trigger_store_atomic_claim(t *testing.T) {
	storeContract(t, openMemoryStore)
}
