package gitopstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/gitopstest"
)

// memEnv is the Env of the in-memory stand-ins; Reopen shares the state.
type memEnv struct {
	clock      *gitopstest.Clock
	leases     *gitopstest.Leases
	recoveries *gitopstest.Recoveries
	intents    *gitopstest.Intents
}

func openMem(testing.TB) gitopstest.Env {
	clock := gitopstest.NewClock()
	return &memEnv{clock: clock, leases: &gitopstest.Leases{Now: clock.Now}, recoveries: &gitopstest.Recoveries{}, intents: &gitopstest.Intents{Now: clock.Now}}
}

func (e *memEnv) Leases() gitops.BackupLeases              { return e.leases }
func (e *memEnv) Recoveries() gitops.RecoveryStore         { return e.recoveries }
func (e *memEnv) Intents() gitops.PruneIntents             { return e.intents }
func (e *memEnv) Clock() *gitopstest.Clock                 { return e.clock }
func (e *memEnv) Seed(testing.TB, domain.Scope, ...string) {}
func (e *memEnv) Reopen(testing.TB) gitopstest.Env         { return e }

// Test_unit_gitops_state_contract runs the shared durable-state contract on the
// in-memory stand-ins; the PostgreSQL adapter runs the same suite in
// gitops/postgres.
func Test_unit_gitops_state_contract(t *testing.T) { gitopstest.Contract(t, openMem) }
