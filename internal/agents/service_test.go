package agents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	scopeA = domain.Scope{TenantID: "t030_acme", ProjectID: "p030_inventory"}
	scopeB = domain.Scope{TenantID: "t030_globex", ProjectID: "p030_billing"}
)

const engineer = "u030_engineer"

type fixture struct {
	repo      *agentstest.MemRepo
	authority *agentstest.Authority
	approvals *agentstest.Approvals
	svc       *agents.Service
}

func newFixture(t *testing.T, cat agents.Compatibility, opts ...func(*agents.ServiceConfig)) *fixture {
	t.Helper()
	f := &fixture{repo: &agentstest.MemRepo{}, authority: agentstest.NewAuthority(), approvals: agentstest.NewApprovals()}
	cfg := agents.ServiceConfig{Repository: f.repo, Authority: f.authority, Compatibility: cat, Approvals: f.approvals,
		Clock: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }}
	for _, o := range opts {
		o(&cfg)
	}
	svc, err := agents.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	f.authority.Grant(scopeA, engineer, agents.ActorUser, agents.PermList, agents.PermRegister)
	f.authority.Grant(scopeA, "u030_reader", agents.ActorUser, agents.PermList)
	f.authority.Grant(scopeB, engineer, agents.ActorUser, agents.PermList, agents.PermRegister)
	return f
}

func (f *fixture) register(scope domain.Scope, principal string, d agents.Draft) (agents.RegisterResult, error) {
	return f.svc.Register(context.Background(), agents.RegisterCommand{Scope: scope, Caller: agents.Caller{PrincipalID: principal, RequestID: "req_t030"}, Draft: d})
}

func TestNewServiceRequiresPorts(t *testing.T) {
	if _, err := agents.NewService(agents.ServiceConfig{}); err == nil {
		t.Fatal("a service without repository and authority must be refused")
	}
}

func TestRegisterUseCase(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	f := newFixture(t, agentstest.Catalogue(t, d))

	first, err := f.register(scopeA, engineer, d)
	if err != nil || !first.Created || first.Profile.Version != 1 || first.Profile.Ref() != "coding@1" {
		t.Fatalf("first registration: %+v %v", first, err)
	}
	if first.Profile.CreatedBy != (agents.Actor{ID: engineer, Type: agents.ActorUser}) {
		t.Fatalf("the actor comes from the authority: %+v", first.Profile.CreatedBy)
	}
	again, err := f.register(scopeA, engineer, d)
	if err != nil || again.Created || again.Profile.ID != first.Profile.ID {
		t.Fatalf("identical content returns the existing version: %+v %v", again, err)
	}
	changed := agentstest.Draft(t, "coding")
	changed.Instructions += " Also write tests."
	third, err := f.register(scopeA, engineer, changed)
	if err != nil || !third.Created || third.Profile.Version != 2 {
		t.Fatalf("changed content is the next version: %+v %v", third, err)
	}
	other, err := f.register(scopeB, engineer, d)
	if err != nil || !other.Created || other.Profile.Version != 1 || other.Profile.Scope != scopeB {
		t.Fatalf("another project has its own versions: %+v %v", other, err)
	}

	t.Run("a stored version is read by its exact reference only", func(t *testing.T) {
		p, err := f.svc.Stored(context.Background(), scopeA, "coding@2")
		if err != nil || p.ID != third.Profile.ID {
			t.Fatalf("stored: %+v %v", p, err)
		}
		if _, err := f.svc.Stored(context.Background(), scopeA, "coding"); !errors.Is(err, agents.ErrInvalid) {
			t.Fatalf("a moving alias must be refused: %v", err)
		}
		if _, err := f.svc.Stored(context.Background(), scopeA, "coding@9"); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("an unknown version is not found: %v", err)
		}
		if _, err := f.svc.Stored(context.Background(), domain.Scope{TenantID: "t030_other", ProjectID: "p030_inventory"}, "coding@1"); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("a foreign scope finds nothing: %v", err)
		}
	})
}

func TestRegisterAuthorizesBeforeAnythingElse(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	f := newFixture(t, agentstest.Catalogue(t, d))
	broken := d
	broken.Name = "Not Valid"

	if _, err := f.register(scopeA, "u030_stranger", broken); !errors.Is(err, agents.ErrHidden) {
		t.Fatalf("an unknown principal learns nothing: %v", err)
	}
	if _, err := f.register(scopeA, "u030_reader", broken); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("a reader may not register, and invalid content is not even looked at: %v", err)
	}
	if _, err := f.register(scopeA, "u030_reader", d); !errors.Is(err, agents.ErrDenied) {
		t.Fatalf("a reader may not register: %v", err)
	}
	if f.repo.Calls != 0 {
		t.Fatalf("nothing may reach the repository: %d calls", f.repo.Calls)
	}
	f.authority.Err = errors.New("authority down")
	if _, err := f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnavailable) {
		t.Fatalf("an authority outage is unavailability, never access: %v", err)
	}
	f.authority.Err = nil

	f.authority.Grant(scopeA, "svc030", agents.ActorWorkload, agents.PermRegister)
	// A workload actor holding a permission in a test table still registers
	// attributably: the real identity adapter is what denies workloads.
	if res, err := f.register(scopeA, "svc030", d); err != nil || res.Profile.CreatedBy.Type != agents.ActorWorkload {
		t.Fatalf("attribution follows the authority: %+v %v", res, err)
	}
	if _, err := f.register(domain.Scope{}, engineer, d); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("an invalid scope is invalid: %v", err)
	}
	if _, err := f.svc.Register(context.Background(), agents.RegisterCommand{Scope: scopeA, Draft: d}); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("a missing principal is invalid: %v", err)
	}
}

func TestRegisterFailsClosedWithoutPolicy(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	for name, tc := range map[string]struct {
		opt  func(*agents.ServiceConfig)
		want error
	}{
		"no compatibility catalogue": {func(c *agents.ServiceConfig) { c.Compatibility = nil }, agents.ErrUnavailable},
		"no approval policy":         {func(c *agents.ServiceConfig) { c.Approvals = nil }, agents.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, agentstest.Catalogue(t, d), tc.opt)
			if _, err := f.register(scopeA, engineer, d); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if f.repo.Calls != 0 {
				t.Fatal("nothing may be written")
			}
		})
	}

	t.Run("an empty catalogue rejects every profile", func(t *testing.T) {
		f := newFixture(t, agentstest.Catalogue(t))
		if _, err := f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("expected unsupported, got %v", err)
		}
		if f.repo.Calls != 0 {
			t.Fatal("nothing may be written")
		}
	})

	t.Run("an outage of the catalogue is unavailability", func(t *testing.T) {
		f := newFixture(t, failingCompat{errors.New("catalogue backend down")})
		if _, err := f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnavailable) || errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	})

	t.Run("an entry without evidence at call time is no support", func(t *testing.T) {
		f := newFixture(t, noEvidenceCompat{})
		if _, err := f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnsupported) {
			t.Fatalf("expected unsupported, got %v", err)
		}
	})

	t.Run("approval refusals are validation diagnostics and outages are unavailable", func(t *testing.T) {
		f := newFixture(t, agentstest.Catalogue(t, d))
		bad := d
		bad.Tools = []string{"repo.read", "unlisted.tool"}
		_, err := f.register(scopeA, engineer, bad)
		var ve *agents.ValidationError
		if !errors.As(err, &ve) || ve.Diagnostics[0].Code != agents.CodeUnapprovedTool {
			t.Fatalf("an unapproved tool is a diagnostic: %v", err)
		}
		bad = d
		bad.Model.Name = "unapproved-model"
		if _, err = f.register(scopeA, engineer, bad); !errors.As(err, &ve) || ve.Diagnostics[0].Code != agents.CodeUnapprovedModel {
			t.Fatalf("an unapproved model is a diagnostic: %v", err)
		}
		f.approvals.Err = errors.New("policy backend down")
		if _, err = f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("an approval outage is unavailability: %v", err)
		}
		if f.repo.Calls != 0 {
			t.Fatal("nothing may be written")
		}
	})
}

func TestRegisterRejectsUnsupportedControls(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	entry := agentstest.Entry(d)
	entry.Controls = []agents.Control{agents.ControlModel, agents.ControlToolAllowlist, agents.ControlTranscript} // no request/token/tool-call enforcement
	cat, err := agents.NewCatalogue(entry)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, cat)
	if _, err := f.register(scopeA, engineer, d); !errors.Is(err, agents.ErrUnsupported) {
		t.Fatalf("a claimed limit without enforcement must fail registration: %v", err)
	}
	narrow := d
	narrow.Limits.MaxRequests, narrow.Limits.MaxTokens, narrow.Limits.MaxToolCalls = nil, nil, nil
	if res, err := f.register(scopeA, engineer, narrow); err != nil || !res.Created {
		t.Fatalf("a profile claiming only what is enforced registers: %+v %v", res, err)
	}
	if f.repo.Calls != 1 {
		t.Fatalf("only the supported registration reached the repository: %d", f.repo.Calls)
	}
}

func TestResolveRequalifies(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	f := newFixture(t, agentstest.Catalogue(t, d))
	res, err := f.register(scopeA, engineer, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Resolve(context.Background(), scopeA, res.Profile.Ref())
	if err != nil || got.Profile.ID != res.Profile.ID || got.Qualification.Evidence == "" {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	if _, err := f.svc.Resolve(context.Background(), scopeB, res.Profile.Ref()); !errors.Is(err, agents.ErrNotFound) {
		t.Fatalf("a foreign project does not resolve: %v", err)
	}
	if _, err := f.svc.Resolve(context.Background(), scopeA, "coding"); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("a moving alias does not resolve: %v", err)
	}
	f.approvals.WithdrawModel("t030-model")
	if _, err := f.svc.Resolve(context.Background(), scopeA, res.Profile.Ref()); !errors.Is(err, agents.ErrUnsupported) {
		t.Fatalf("a withdrawn approval fails closed for new runs: %v", err)
	}

	// A catalogue that no longer covers the combination fails closed as well,
	// without altering the stored profile.
	svc, err := agents.NewService(agents.ServiceConfig{Repository: f.repo, Authority: f.authority, Compatibility: agentstest.Catalogue(t), Approvals: agentstest.NewApprovals()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(context.Background(), scopeA, res.Profile.Ref()); !errors.Is(err, agents.ErrUnsupported) {
		t.Fatalf("an unqualified combination fails closed: %v", err)
	}
	if p, err := svc.Stored(context.Background(), scopeA, res.Profile.Ref()); err != nil || p.Digest != res.Profile.Digest {
		t.Fatalf("the stored profile is unchanged: %v", err)
	}
}

func TestListUseCase(t *testing.T) {
	d := agentstest.Draft(t, "coding")
	f := newFixture(t, agentstest.Catalogue(t, d))
	if _, err := f.register(scopeA, engineer, d); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := f.svc.List(ctx, scopeA, agents.Caller{PrincipalID: "u030_reader"}, agents.ListQuery{Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v %v", got, err)
	}
	if _, err := f.svc.List(ctx, scopeA, agents.Caller{PrincipalID: "u030_reader"}, agents.ListQuery{Limit: 0}); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("a page size must be given: %v", err)
	}
	if _, err := f.svc.List(ctx, scopeA, agents.Caller{PrincipalID: "u030_reader"}, agents.ListQuery{Limit: 10, Name: "Bad Name"}); !errors.Is(err, agents.ErrInvalid) {
		t.Fatalf("a bad name filter is invalid: %v", err)
	}
	if _, err := f.svc.List(ctx, scopeA, agents.Caller{PrincipalID: "u030_stranger"}, agents.ListQuery{Limit: 10}); !errors.Is(err, agents.ErrHidden) {
		t.Fatalf("a stranger learns nothing: %v", err)
	}
	if _, err := f.svc.List(ctx, scopeB, agents.Caller{PrincipalID: "u030_reader"}, agents.ListQuery{Limit: 10}); !errors.Is(err, agents.ErrHidden) {
		t.Fatalf("a reader of one project learns nothing of another: %v", err)
	}
}

type failingCompat struct{ err error }

func (f failingCompat) Qualify(context.Context, agents.Tuple) (agents.Qualification, error) {
	return agents.Qualification{}, f.err
}

type noEvidenceCompat struct{}

func (noEvidenceCompat) Qualify(context.Context, agents.Tuple) (agents.Qualification, error) {
	return agents.Qualification{Controls: agents.Controls()}, nil
}
