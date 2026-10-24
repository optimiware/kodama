package gatewaytest

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

// Stranger is an actor no fixture scope knows.
const Stranger = "u083_stranger"

// MemHarness builds a fresh in-memory store with its table authorities for the
// Contract suite, as a PostgreSQL wiring builds its own.
func MemHarness(tb testing.TB) Harness {
	tb.Helper()
	auth, mgmt := NewMemAuthority(), NewMemManagement()
	for _, s := range []domain.Scope{ScopeA, ScopeA2, ScopeB} {
		mgmt.Grant(s, Admin, gateway.PermRead, gateway.PermSettings)
		mgmt.Grant(s, Viewer, gateway.PermRead)
	}
	store, err := NewMemStore(auth.Check, mgmt)
	must(tb, err)
	return Harness{
		Store: store, Scopes: Scopes{A: ScopeA, A2: ScopeA2, B: ScopeB},
		Admin: gateway.Actor{ID: Admin}, Viewer: gateway.Actor{ID: Viewer}, Stranger: gateway.Actor{ID: Stranger},
		NewWorkload: func(s domain.Scope) gateway.Workload { w, _ := auth.Add(s); return w },
		Revoke:      func(w gateway.Workload) { auth.Revoke(w.AttemptID) },
		Events:      func(t domain.TenantID, typ string) int { return store.EventsOfType(t, typ) },
		Audits:      func(t domain.TenantID, action string) int { return store.AuditsOf(t, action) },
		FailEvents:  store.FailEvents,
	}
}

// second is the granularity of a Harness epoch.
const second = time.Second
