package identity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

type fakeReader struct {
	calls    atomic.Int32
	bindings []Binding
	err      error
}

func (f *fakeReader) CurrentBindings(context.Context, domain.TenantID, UserID) ([]Binding, error) {
	f.calls.Add(1)
	return f.bindings, f.err
}

type fakeAuthority func() error

func (f fakeAuthority) Authorize(context.Context, Actor, Target, Permission) error { return f() }

func TestAuthorityUnavailableNoFallback(t *testing.T) {
	ctx := context.Background()
	actor, target := UserActor("u_1"), Target{TenantID: "t_acme", ProjectID: "p_a"}

	local := &fakeReader{bindings: []Binding{{ID: "mb_1", Tenant: "t_acme", User: "u_1", Role: RoleAdmin, Version: 1}}}
	standalone, err := NewStandaloneAuthority(local)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("profile must be selected explicitly", func(t *testing.T) {
		for _, cfg := range []AuthorityConfig{
			{},                           // nothing selected
			{Standalone: standalone},     // grants present but no profile
			{Profile: "federated"},       // unknown
			{Profile: ProfileStandalone}, // selected without an authority
			{Profile: ProfileHosted, Standalone: standalone}, // hosted without an adapter must not borrow local grants
		} {
			if a, err := SelectAuthority(cfg); err == nil {
				t.Errorf("%+v selected %T", cfg, a)
			}
		}
		if _, err := NewStandaloneAuthority(nil); err == nil {
			t.Error("standalone authority without a reader")
		}
	})

	t.Run("hosted outcomes never reach the local grants", func(t *testing.T) {
		for name, hosted := range map[string]error{
			"denied":      ErrDenied,
			"hidden":      ErrHidden,
			"outage":      errors.New("core down"),
			"unavailable": Unavailablef(nil, "core"),
		} {
			a, err := SelectAuthority(AuthorityConfig{Profile: ProfileHosted, Hosted: fakeAuthority(func() error { return hosted }), Standalone: standalone})
			if err != nil {
				t.Fatal(err)
			}
			got := a.Authorize(ctx, actor, target, PermProjectRead)
			switch name {
			case "denied":
				if !errors.Is(got, ErrDenied) {
					t.Errorf("%s: %v", name, got)
				}
			case "hidden":
				if !errors.Is(got, ErrHidden) {
					t.Errorf("%s: %v", name, got)
				}
			default:
				if !errors.Is(got, ErrUnavailable) || errors.Is(got, ErrDenied) {
					t.Errorf("%s: %v", name, got)
				}
			}
		}
		if n := local.calls.Load(); n != 0 {
			t.Fatalf("local grants consulted %d times under the hosted profile", n)
		}
	})

	t.Run("standalone never reaches a hosted adapter and fails closed on outage", func(t *testing.T) {
		var hostedCalls atomic.Int32
		hosted := fakeAuthority(func() error { hostedCalls.Add(1); return nil })
		a, err := SelectAuthority(AuthorityConfig{Profile: ProfileStandalone, Standalone: standalone, Hosted: hosted})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Authorize(ctx, actor, target, PermProjectRead); err != nil {
			t.Fatal(err)
		}
		if hostedCalls.Load() != 0 {
			t.Fatal("hosted adapter consulted by the standalone profile")
		}
		down := &fakeReader{err: errors.New("database down")}
		s2, _ := NewStandaloneAuthority(down)
		a2, _ := SelectAuthority(AuthorityConfig{Profile: ProfileStandalone, Standalone: s2})
		if err := a2.Authorize(ctx, actor, target, PermProjectRead); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("outage: %v", err)
		}
	})

	t.Run("a nil inner authority is unavailable, never allowed", func(t *testing.T) {
		if err := (failClosed{}).Authorize(ctx, actor, target, PermProjectRead); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	})

	t.Run("a malformed request is refused without reading grants", func(t *testing.T) {
		r := &fakeReader{bindings: local.bindings}
		s, _ := NewStandaloneAuthority(r)
		if err := s.Authorize(ctx, Actor{ID: "u_1", Type: ActorWorkload}, target, PermProjectRead); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
		if err := s.Authorize(ctx, actor, Target{TenantID: "bad id"}, PermProjectRead); !errors.Is(err, ErrHidden) {
			t.Fatal(err)
		}
		if r.calls.Load() != 0 {
			t.Fatal("grants read for a malformed request")
		}
	})
}
