package postgres_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/idtest"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
)

type tokenVerifier struct {
	principals map[string]identity.Principal
	err        error
	calls      atomic.Int32
}

func (v *tokenVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	v.calls.Add(1)
	if v.err != nil {
		return identity.Principal{}, v.err
	}
	p, ok := v.principals[token]
	if !ok {
		return identity.Principal{}, identity.ErrUnauthenticated
	}
	return p, nil
}

type countingStore struct {
	inner identity.BootstrapStore
	calls atomic.Int32
}

func (c *countingStore) Complete(ctx context.Context, p identity.BootstrapPlan) (identity.BootstrapResult, error) {
	c.calls.Add(1)
	return c.inner.Complete(ctx, p)
}

var operator = identity.Principal{Issuer: "https://issuer.test", Subject: "initial-admin"}

func bootstrapConfig() identity.BootstrapConfig {
	return identity.BootstrapConfig{
		Profile: identity.ProfileStandalone, Eligible: operator, OperatorID: "op_bootstrap",
		TenantID: "t010_boot", TenantKey: "boot", DisplayName: "Bootstrap Org",
	}
}

func newBootstrap(t *testing.T, s *idtest.Stack, cfg identity.BootstrapConfig, v *tokenVerifier) (*identity.BootstrapService, *countingStore) {
	t.Helper()
	store := must(identitypg.NewBootstrapStore(s.Owner, s.Events))
	counting := &countingStore{inner: store}
	svc, err := identity.NewBootstrapService(cfg, v, counting)
	if err != nil {
		t.Fatal(err)
	}
	return svc, counting
}

func TestBootstrapVerifiedOnce(t *testing.T) {
	newVerifier := func() *tokenVerifier {
		return &tokenVerifier{principals: map[string]identity.Principal{
			"admin-token": operator, "other-token": {Issuer: "https://issuer.test", Subject: "someone-else"},
		}}
	}

	t.Run("configuration is explicit", func(t *testing.T) {
		s := idtest.New(t, idtest.Options{})
		store := must(identitypg.NewBootstrapStore(s.Owner, s.Events))
		v := newVerifier()
		for name, mut := range map[string]func(*identity.BootstrapConfig){
			"no profile":     func(c *identity.BootstrapConfig) { c.Profile = "" },
			"hosted profile": func(c *identity.BootstrapConfig) { c.Profile = identity.ProfileHosted },
			"no operator":    func(c *identity.BootstrapConfig) { c.Eligible = identity.Principal{} },
			"no operator id": func(c *identity.BootstrapConfig) { c.OperatorID = "" },
			"bad tenant":     func(c *identity.BootstrapConfig) { c.TenantID = "a b" },
			"bad key":        func(c *identity.BootstrapConfig) { c.TenantKey = "a/b" },
		} {
			cfg := bootstrapConfig()
			mut(&cfg)
			if _, err := identity.NewBootstrapService(cfg, v, store); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if _, err := identity.NewBootstrapService(bootstrapConfig(), nil, store); err == nil {
			t.Error("no verifier accepted")
		}
		if _, err := identity.NewBootstrapService(bootstrapConfig(), v, nil); err == nil {
			t.Error("no store accepted")
		}
	})

	t.Run("an unverified or wrong identity never opens the administrative transaction", func(t *testing.T) {
		s := idtest.New(t, idtest.Options{})
		v := newVerifier()
		svc, store := newBootstrap(t, s, bootstrapConfig(), v)
		if _, err := svc.Run(ctx, "bogus"); !is(err, identity.ErrUnauthenticated) {
			t.Fatalf("bad token: %v", err)
		}
		if _, err := svc.Run(ctx, "other-token"); !is(err, identity.ErrBootstrapNotEligible) {
			t.Fatalf("wrong operator: %v", err)
		}
		v.err = errors.New("issuer down")
		if _, err := svc.Run(ctx, "admin-token"); !is(err, identity.ErrUnavailable) || is(err, identity.ErrUnauthenticated) {
			t.Fatalf("verifier outage: %v", err)
		}
		if store.calls.Load() != 0 {
			t.Fatal("the store was invoked for an unverified or ineligible identity")
		}
		if s.Count(`SELECT count(*) FROM tenants`) != 0 || s.Count(`SELECT count(*) FROM identity_bootstrap`) != 0 {
			t.Fatal("rows were provisioned")
		}
	})

	t.Run("concurrent invocations provision once; retries observe the commit", func(t *testing.T) {
		s := idtest.New(t, idtest.Options{Conns: 4})
		svc, _ := newBootstrap(t, s, bootstrapConfig(), newVerifier())
		const n = 10
		results := make([]identity.BootstrapResult, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i], errs[i] = svc.Run(ctx, "admin-token")
			}()
		}
		close(start)
		wg.Wait()
		created := 0
		for i, err := range errs {
			if err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
			if results[i].Created {
				created++
			}
			if results[i].UserID != results[0].UserID || results[i].BindingID != results[0].BindingID || results[i].Tenant != "t010_boot" {
				t.Fatalf("diverging results: %+v vs %+v", results[i], results[0])
			}
		}
		if created != 1 {
			t.Fatalf("%d invocations created", created)
		}
		if s.Count(`SELECT count(*) FROM tenants WHERE id = 't010_boot'`) != 1 ||
			s.Count(`SELECT count(*) FROM memberships WHERE tenant_id = 't010_boot'`) != 1 ||
			s.Count(`SELECT count(*) FROM identity_bootstrap`) != 1 ||
			s.CountIn("t010_boot", `SELECT count(*) FROM audit WHERE tenant_id = 't010_boot'`) != 1 ||
			len(s.Outbox("t010_boot", eventsv1.TypeMembershipChanged)) != 1 {
			t.Fatal("bootstrap duplicated an effect")
		}
		var role, project *string
		var actor string
		if err := s.Owner.QueryRow(ctx, `SELECT role, project_id FROM memberships WHERE tenant_id = 't010_boot'`).Scan(&role, &project); err != nil || *role != "admin" || project != nil {
			t.Fatalf("binding: %v %v %v", role, project, err)
		}
		_ = actor
		if got := s.Revision("t010_boot", results[0].UserID); got != 1 {
			t.Fatalf("revision %d", got)
		}

		t.Run("changed parameters conflict", func(t *testing.T) {
			for name, mut := range map[string]func(*identity.BootstrapConfig){
				"key":    func(c *identity.BootstrapConfig) { c.TenantKey = "other" },
				"name":   func(c *identity.BootstrapConfig) { c.DisplayName = "Other" },
				"tenant": func(c *identity.BootstrapConfig) { c.TenantID = "t010_second" },
				"subject": func(c *identity.BootstrapConfig) {
					c.Eligible = identity.Principal{Issuer: "https://issuer.test", Subject: "someone-else"}
				},
			} {
				cfg := bootstrapConfig()
				mut(&cfg)
				v := newVerifier()
				v.principals["admin-token"] = cfg.Eligible
				other, _ := newBootstrap(t, s, cfg, v)
				if _, err := other.Run(ctx, "admin-token"); !is(err, identity.ErrBootstrapConflict) {
					t.Errorf("%s: %v", name, err)
				}
			}
			if s.Count(`SELECT count(*) FROM tenants`) != 1 {
				t.Fatal("a conflicting bootstrap provisioned a second tenant")
			}
		})

		t.Run("revoking the initial admin does not re-enable bootstrap", func(t *testing.T) {
			second := s.User("https://issuer.test", "second-admin")
			caller := idtest.Caller(results[0].UserID)
			must(s.Service.Grant(ctx, identity.GrantCommand{Caller: caller, Tenant: "t010_boot", User: second, Role: identity.RoleAdmin}))
			must(s.Service.Revoke(ctx, identity.RevokeCommand{Caller: idtest.Caller(second), Tenant: "t010_boot", ID: results[0].BindingID, Expected: 1}))
			if s.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(results[0].UserID)) != 0 {
				t.Fatal("the initial admin keeps access")
			}
			res, err := svc.Run(ctx, "admin-token")
			if err != nil || res.Created || res.BindingID != results[0].BindingID {
				t.Fatalf("%+v %v", res, err)
			}
			if s.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(results[0].UserID)) != 0 {
				t.Fatal("bootstrap re-granted the revoked administrator")
			}
		})

		t.Run("the runtime role holds no bootstrap or tenant-provisioning privilege", func(t *testing.T) {
			for _, sql := range []string{
				`SELECT * FROM identity_bootstrap`,
				`INSERT INTO identity_bootstrap (tenant_id, user_id, binding_id, params_digest, operator_id) VALUES ('t010_boot', 'u', 'b', repeat('a', 64), 'o')`,
				`DELETE FROM identity_bootstrap`,
				`INSERT INTO tenants (id, key) VALUES ('t010_runtime', 'runtime')`,
				`UPDATE tenants SET key = 'x'`,
				`DELETE FROM tenants`,
			} {
				if _, err := s.Runtime.Exec(ctx, sql); err == nil {
					t.Errorf("runtime role executed: %s", sql)
				}
			}
		})
	})

	t.Run("an earlier login is reused and a rolled-back bootstrap leaves the way open", func(t *testing.T) {
		s := idtest.New(t, idtest.Options{})
		prior := s.User(operator.Issuer, operator.Subject)
		s.OwnerExec(`ALTER TABLE audit ADD CONSTRAINT t010_block CHECK (action <> 'membership.granted')`)
		svc, _ := newBootstrap(t, s, bootstrapConfig(), newVerifier())
		if _, err := svc.Run(ctx, "admin-token"); !is(err, identity.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if s.Count(`SELECT count(*) FROM tenants`) != 0 || s.Count(`SELECT count(*) FROM identity_bootstrap`) != 0 ||
			s.Count(`SELECT count(*) FROM memberships`) != 0 || s.Count(`SELECT count(*) FROM identity_authz_revisions`) != 0 {
			t.Fatal("a failed bootstrap left rows")
		}
		s.OwnerExec(`ALTER TABLE audit DROP CONSTRAINT t010_block`)
		res, err := svc.Run(ctx, "admin-token")
		if err != nil || !res.Created || res.UserID != prior {
			t.Fatalf("%+v %v (prior %s)", res, err, prior)
		}
		if s.Count(`SELECT count(*) FROM users WHERE issuer = $1 AND subject = $2`, operator.Issuer, operator.Subject) != 1 {
			t.Fatal("a second user row was created for the administrator")
		}
	})

	t.Run("an unknown commit outcome is unavailable; the retry observes the completion", func(t *testing.T) {
		s := idtest.New(t, idtest.Options{})
		db := &lostCommitDB{inner: s.Owner, armed: true}
		store := must(identitypg.NewBootstrapStore(db, s.Events))
		svc := must(identity.NewBootstrapService(bootstrapConfig(), newVerifier(), store))
		if _, err := svc.Run(ctx, "admin-token"); !is(err, identity.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if s.Count(`SELECT count(*) FROM identity_bootstrap`) != 1 {
			t.Fatal("the commit did not happen; the scenario is moot")
		}
		db.armed = false
		res, err := svc.Run(ctx, "admin-token")
		if err != nil || res.Created {
			t.Fatalf("retry: %+v %v", res, err)
		}
		if s.Count(`SELECT count(*) FROM memberships WHERE tenant_id = 't010_boot'`) != 1 || s.Revision("t010_boot", res.UserID) != 1 {
			t.Fatal("the retry granted again")
		}
	})
}
