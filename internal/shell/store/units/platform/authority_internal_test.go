package platform

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestAuthorityLocksEmptyTargetFailsClosed is the authority regression for the
// not-applicable exemption: only a human or service actor may carry an
// execution_running or action_deadline read with no target. An execution actor
// must name the action whose deadline it commits under, and an empty or unknown
// actor kind names no applicable authority at all, so both are refused instead
// of silently adding no lock.
func TestAuthorityLocksEmptyTargetFailsClosed(t *testing.T) {
	store := memory.NewWithClock(func() time.Time { return time.Unix(0, 0).UTC() })
	scope := tenancy.Scope{TenantID: "tenant-a", ProjectID: "project-a"}
	checks := []authorize.AuthorityCheck{authorize.CheckExecutionRunning, authorize.CheckActionDeadline}
	refusedKinds := []struct {
		name string
		kind identity.PrincipalKind
	}{
		{name: "empty kind", kind: ""},
		{name: "unknown kind", kind: identity.PrincipalKind("unrecognized")},
		{name: "execution kind", kind: identity.KindExecution},
	}
	for _, check := range checks {
		for _, refused := range refusedKinds {
			t.Run(string(check)+"/"+refused.name, func(t *testing.T) {
				reads := []authorize.AuthorityRead{{
					Operation: "actor.verify",
					Check:     check,
					Scope:     scope,
				}}
				locks, err := authorityLocks(context.Background(), store, store, scope, reads,
					authorityFacts{ActorKind: refused.kind})
				if locks != nil {
					t.Errorf("refused read added %d locks", len(locks))
				}
				if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
					t.Fatalf("error = %v (class %v), want invalid_input", err, class)
				}
			})
		}
	}

	t.Run("human and service kinds are not applicable", func(t *testing.T) {
		for _, kind := range []identity.PrincipalKind{identity.KindHuman, identity.KindService} {
			for _, check := range checks {
				reads := []authorize.AuthorityRead{{
					Operation: "actor.verify",
					Check:     check,
					Scope:     scope,
				}}
				locks, err := authorityLocks(context.Background(), store, store, scope, reads,
					authorityFacts{ActorKind: kind})
				if err != nil {
					t.Fatalf("%s/%s: %v", kind, check, err)
				}
				if len(locks) != 0 {
					t.Errorf("%s/%s added %d locks, want none", kind, check, len(locks))
				}
			}
		}
	})
}
