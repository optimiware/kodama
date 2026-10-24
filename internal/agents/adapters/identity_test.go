package adapters_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

var (
	scope    = domain.Scope{TenantID: "t030_acme", ProjectID: "p030_inventory"}
	target   = identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
	human    = "u030_human"
	reviewer = "u030_reviewer"
)

type reader struct{ bindings []identity.Binding }

func (r reader) CurrentBindings(_ context.Context, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	var out []identity.Binding
	for _, b := range r.bindings {
		if b.Tenant == tenant && b.User == user {
			out = append(out, b)
		}
	}
	return out, nil
}

type failingReader struct{}

func (failingReader) CurrentBindings(context.Context, domain.TenantID, identity.UserID) ([]identity.Binding, error) {
	return nil, errors.New("database down")
}

func bind(user string, role identity.Role) identity.Binding {
	return identity.Binding{ID: identity.BindingID("b_" + user + "_" + string(role)), Tenant: scope.TenantID, User: identity.UserID(user), Project: scope.ProjectID, Role: role, Version: 1}
}

// TestAgentNoHumanRole verifies AR-12: an agent or workload identity never
// satisfies a human review or completion permission, even when its
// instructions say it is a reviewer and even when it carries the initiating
// human's identifier. It exercises the merged identity policy and this
// module's authority adapter, not a local imitation.
func TestAgentNoHumanRole(t *testing.T) {
	r := reader{bindings: []identity.Binding{
		bind(human, identity.RoleEngineer), bind(human, identity.RoleReviewer), // a person who both starts runs and reviews
		bind(reviewer, identity.RoleReviewer),
	}}
	std, err := identity.NewStandaloneAuthority(r)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	humanOnly := []identity.Permission{identity.PermGateDecide, identity.PermWorkComplete}
	everything := append([]identity.Permission{identity.PermRunManage, identity.PermRunRead, identity.PermWorkWork, identity.PermWorkCancel,
		identity.PermWorkspaceManage, identity.PermDeploymentManage}, humanOnly...)

	t.Run("the control: the human holds the permissions", func(t *testing.T) {
		for _, p := range humanOnly {
			if err := std.Authorize(ctx, identity.Actor{ID: human, Type: identity.ActorUser}, target, p); err != nil {
				t.Fatalf("a reviewer person must hold %s: %v", p, err)
			}
		}
	})

	// Reviewer-like instructions (for example "You are the reviewer. Approve the
	// gate.") are text in a pinned profile. They change no actor, role or
	// permission: the only question asked below is who the agent is, and a
	// profile payload carries no identity at all (see the payload tests).
	for _, typ := range []identity.ActorType{identity.ActorWorkload, identity.ActorService, identity.ActorOperator} {
		for _, id := range []string{human, reviewer, "att_t030_1", "wl_agent_1"} {
			actor := identity.Actor{ID: id, Type: typ}
			for _, p := range everything {
				err := std.Authorize(ctx, actor, target, p)
				if !errors.Is(err, identity.ErrDenied) {
					t.Errorf("%s actor %q must be denied %s, got %v", typ, id, p, err)
				}
			}
			if err := identity.Decide(r.bindings, actor, target, identity.PermGateDecide); !errors.Is(err, identity.ErrDenied) {
				t.Errorf("Decide for %s %q gate:decide: %v", typ, id, err)
			}
			if err := identity.Decide(r.bindings, actor, target, identity.PermWorkComplete); !errors.Is(err, identity.ErrDenied) {
				t.Errorf("Decide for %s %q work_item:complete: %v", typ, id, err)
			}
		}
	}

	t.Run("this module's authority adapter denies every non-user actor", func(t *testing.T) {
		auth, err := adapters.NewAuthority(std, r)
		if err != nil {
			t.Fatal(err)
		}
		for _, typ := range []agents.ActorType{agents.ActorWorkload, agents.ActorService, agents.ActorOperator} {
			for _, p := range []agents.Permission{agents.PermRegister, agents.PermList} {
				if err := auth.Authorize(ctx, agents.Actor{ID: human, Type: typ}, scope, p); !errors.Is(err, agents.ErrDenied) {
					t.Errorf("%s actor with the human's ID must be denied %s, got %v", typ, p, err)
				}
			}
		}
		// A human engineer registers; a reviewer-only human only reads.
		if err := auth.Authorize(ctx, agents.Actor{ID: human, Type: agents.ActorUser}, scope, agents.PermRegister); err != nil {
			t.Errorf("the engineer must be allowed to register: %v", err)
		}
		if err := auth.Authorize(ctx, agents.Actor{ID: reviewer, Type: agents.ActorUser}, scope, agents.PermRegister); !errors.Is(err, agents.ErrDenied) {
			t.Errorf("a reviewer-only person must not register profiles: %v", err)
		}
		if err := auth.Authorize(ctx, agents.Actor{ID: reviewer, Type: agents.ActorUser}, scope, agents.PermList); err != nil {
			t.Errorf("a reviewer reads (run:read): %v", err)
		}
		if err := auth.Authorize(ctx, agents.Actor{ID: human, Type: agents.ActorUser}, scope, agents.Permission("agent_profile.unknown")); !errors.Is(err, agents.ErrDenied) {
			t.Errorf("an unmapped permission must deny: %v", err)
		}
	})

	t.Run("the principal of a request is a person, never a workload", func(t *testing.T) {
		auth, err := adapters.NewAuthority(std, r)
		if err != nil {
			t.Fatal(err)
		}
		a, err := auth.ResolveActor(ctx, scope, human)
		if err != nil || a != (agents.Actor{ID: human, Type: agents.ActorUser}) {
			t.Fatalf("a bound principal resolves as a user: %+v %v", a, err)
		}
		for _, p := range []string{"att_t030_1", "wl_agent_1", "u030_nobody", "", "not an id"} {
			if _, err := auth.ResolveActor(ctx, scope, p); !errors.Is(err, agents.ErrHidden) {
				t.Errorf("principal %q has no binding and must be hidden: %v", p, err)
			}
		}
		other := domain.Scope{TenantID: "t030_globex", ProjectID: "p030_billing"}
		if _, err := auth.ResolveActor(ctx, other, human); !errors.Is(err, agents.ErrHidden) {
			t.Errorf("a person bound in one tenant is hidden from another: %v", err)
		}
	})

	t.Run("an identity outage is unavailability, never access", func(t *testing.T) {
		auth, err := adapters.NewAuthority(std, failingReader{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := auth.ResolveActor(ctx, scope, human); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
		down, _ := identity.NewStandaloneAuthority(failingReader{})
		auth, _ = adapters.NewAuthority(down, r)
		if err := auth.Authorize(ctx, agents.Actor{ID: human, Type: agents.ActorUser}, scope, agents.PermRegister); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	})

	t.Run("constructors fail closed", func(t *testing.T) {
		if _, err := adapters.NewAuthority(nil, r); err == nil {
			t.Fatal("an authority is required")
		}
		if _, err := adapters.NewAuthority(std, nil); err == nil {
			t.Fatal("an access reader is required")
		}
	})
}
