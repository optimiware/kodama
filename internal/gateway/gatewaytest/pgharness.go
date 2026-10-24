package gatewaytest

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/adapters"
)

// PGHarness is the Contract harness of the PostgreSQL store (SD-13, SD-A11):
// one isolated database with every module the store depends on, the model
// secret at versions one and two in each fixture project (the store registers
// the exact version a run's binding names), real runtime-authority attempts as
// workloads, and the real identity authority for actors. It needs a service:
// callers gate it with layer.SkipService (see the integration package).
//
// Revoke cancels the workload's run through the execution service, which is how
// an attempt really loses authority.
func PGHarness(tb testing.TB) Harness {
	tb.Helper()
	e := New(tb)
	scopes := Scopes{A: executiontest.ScopeA, A2: executiontest.ScopeA2, B: executiontest.ScopeB}
	for _, s := range []domain.Scope{scopes.A, scopes.A2, scopes.B} {
		sec := e.ModelSecret(s)
		e.Rotate(s, sec.ID(), e.Payload(Bearer+"-v2", e.Provider.Origin()))
	}

	// A replica whose event appender can be made to fail, to prove that a write
	// whose event cannot be appended leaves nothing behind.
	var mu sync.Mutex
	var injected error
	inner := adapters.EventsFrom(e.X.Events)
	replica := e.NewReplica(ReplicaOptions{Events: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		mu.Lock()
		err := injected
		mu.Unlock()
		if err != nil {
			return err
		}
		return inner(ctx, tx, env)
	}})

	var amu sync.Mutex
	attempts := map[domain.AttemptID]Attempt{}
	return Harness{
		Store: replica.Store, Scopes: scopes,
		Admin: gateway.Actor{ID: Admin}, Viewer: gateway.Actor{ID: Viewer}, Stranger: gateway.Actor{ID: Stranger},
		NewWorkload: func(s domain.Scope) gateway.Workload {
			a := e.NewAttempt(s)
			amu.Lock()
			attempts[a.ID] = a
			amu.Unlock()
			return a.Workload
		},
		Revoke: func(w gateway.Workload) {
			amu.Lock()
			a, ok := attempts[w.AttemptID]
			amu.Unlock()
			if !ok {
				tb.Fatalf("revoke: no attempt %s", w.AttemptID)
			}
			e.CancelRun(a)
		},
		Epoch:  e.X.Clock.Now().UTC().Truncate(second),
		Events: func(t domain.TenantID, typ string) int { return e.Events(t, typ) },
		Audits: func(t domain.TenantID, action string) int {
			return e.X.CountTenant(t, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2`, string(t), action)
		},
		FailEvents: func(err error) { mu.Lock(); injected = err; mu.Unlock() },
	}
}
