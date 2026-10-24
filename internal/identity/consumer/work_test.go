package consumer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/consumer"
	"github.com/optimiware/kodama/internal/work"
)

type reader struct {
	bindings []identity.Binding
	err      error
}

func (r reader) CurrentBindings(context.Context, domain.TenantID, identity.UserID) ([]identity.Binding, error) {
	return r.bindings, r.err
}

var scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_a"}

func grant(t *testing.T, project domain.ProjectID, role identity.Role) identity.Binding {
	t.Helper()
	b, err := identity.Grant("mb_1", "t_acme", "u_1", project, role, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func authority(t *testing.T, r reader) *consumer.WorkAuthority {
	t.Helper()
	sa, err := identity.NewStandaloneAuthority(r)
	if err != nil {
		t.Fatal(err)
	}
	a, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: sa})
	if err != nil {
		t.Fatal(err)
	}
	w, err := consumer.NewWorkAuthority(a, r)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestWorkAuthorityTranslation(t *testing.T) {
	ctx := context.Background()
	allWork := []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec, work.PermEditTechnical, work.PermRefine, work.PermWork, work.PermComplete, work.PermReopen, work.PermCancel}

	t.Run("every work permission is mapped and follows the fixed role matrix", func(t *testing.T) {
		for _, role := range identity.Roles() {
			a := authority(t, reader{bindings: []identity.Binding{grant(t, "", role)}})
			actor := work.Actor{ID: "u_1", Type: work.ActorUser}
			for _, p := range allWork {
				want := role.Grants(identity.Permission(p))
				err := a.Authorize(ctx, actor, scope, p)
				if want != (err == nil) {
					t.Errorf("%s %s: matrix says %v, got %v", role, p, want, err)
				}
				if err != nil && !errors.Is(err, work.ErrDenied) {
					t.Errorf("%s %s: %v", role, p, err)
				}
			}
		}
	})

	t.Run("unknown permissions, non-human actors and outsiders are refused", func(t *testing.T) {
		a := authority(t, reader{bindings: []identity.Binding{grant(t, "", identity.RoleReviewer)}})
		user := work.Actor{ID: "u_1", Type: work.ActorUser}
		if err := a.Authorize(ctx, user, scope, "work_item:delete"); !errors.Is(err, work.ErrDenied) {
			t.Errorf("unknown permission: %v", err)
		}
		if err := a.Authorize(ctx, work.Actor{ID: "u_1", Type: work.ActorService}, scope, work.PermComplete); !errors.Is(err, work.ErrDenied) {
			t.Errorf("service actor completed: %v", err)
		}
		if err := a.Authorize(ctx, work.Actor{ID: "u_2", Type: work.ActorUser}, scope, work.PermRead); !errors.Is(err, work.ErrHidden) {
			t.Errorf("another user: %v", err)
		}
	})

	t.Run("an outage is unavailable, never access", func(t *testing.T) {
		a := authority(t, reader{err: errors.New("db down")})
		if err := a.Authorize(ctx, work.Actor{ID: "u_1", Type: work.ActorUser}, scope, work.PermRead); !errors.Is(err, work.ErrUnavailable) {
			t.Errorf("%v", err)
		}
		if _, err := a.ResolveActor(ctx, scope, "u_1"); !errors.Is(err, work.ErrUnavailable) {
			t.Errorf("%v", err)
		}
	})

	t.Run("actors resolve only for bound users and are always human", func(t *testing.T) {
		a := authority(t, reader{bindings: []identity.Binding{grant(t, "p_a", identity.RoleViewer)}})
		act, err := a.ResolveActor(ctx, scope, "u_1")
		if err != nil || act != (work.Actor{ID: "u_1", Type: work.ActorUser}) {
			t.Fatalf("%+v %v", act, err)
		}
		if _, err := a.ResolveActor(ctx, domain.Scope{TenantID: "t_acme", ProjectID: "p_other"}, "u_1"); !errors.Is(err, work.ErrHidden) {
			t.Errorf("another project: %v", err)
		}
		for _, bad := range []string{"", "svc pipeline", "svc/x"} {
			if _, err := a.ResolveActor(ctx, scope, bad); !errors.Is(err, work.ErrHidden) {
				t.Errorf("%q: %v", bad, err)
			}
		}
	})

	t.Run("collaborators are required", func(t *testing.T) {
		if _, err := consumer.NewWorkAuthority(nil, reader{}); err == nil {
			t.Error("nil authority accepted")
		}
		if _, err := consumer.NewWorkAuthority(failing{}, nil); err == nil {
			t.Error("nil reader accepted")
		}
	})
}

type failing struct{}

func (failing) Authorize(context.Context, identity.Actor, identity.Target, identity.Permission) error {
	return identity.ErrDenied
}
