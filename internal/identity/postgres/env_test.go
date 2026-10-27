package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	tenantA, projectA, projectA2 = "t010_acme", "p010_inventory", "p010_billing"
	tenantB, projectB            = "t010_globex", "p010_ledger"
)

var ctx = context.Background()

type fixture struct {
	*idtest.Stack
	admin, engineer, reviewer, outsider identity.UserID
	adminBinding                        identity.BindingID
}

// newFixture seeds two tenants, a tenant admin, and unbound/other users.
func newFixture(t *testing.T, o idtest.Options) *fixture {
	t.Helper()
	s := idtest.New(t, o)
	s.SeedProject(tenantA, projectA, 5)
	s.SeedProject(tenantA, projectA2, 5)
	s.SeedProject(tenantB, projectB, 5)
	f := &fixture{Stack: s}
	f.admin = s.User("https://issuer.test", "admin")
	f.engineer = s.User("https://issuer.test", "engineer")
	f.reviewer = s.User("https://issuer.test", "reviewer")
	f.outsider = s.User("https://issuer.test", "outsider")
	f.adminBinding = s.Bind(tenantA, f.admin, "", identity.RoleAdmin)
	return f
}

func (f *fixture) caller(u identity.UserID) identity.Caller { return idtest.Caller(u) }

func (f *fixture) grant(actor, user identity.UserID, project domain.ProjectID, role identity.Role) (identity.Binding, error) {
	return f.Service.Grant(ctx, identity.GrantCommand{Caller: f.caller(actor), Tenant: tenantA, User: user, Project: project, Role: role})
}

// must returns v or fails the test through a panic with the error.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func is(err error, target error) bool { return errors.Is(err, target) }

func projectOrEmpty() domain.ProjectID      { return "" }
func domainTenant(s string) domain.TenantID { return domain.TenantID(s) }

func idtest0() idtestOptions { return idtestOptions{} }

type idtestOptions = idtest.Options

func manifestOf(t *testing.T) storage.Manifest {
	t.Helper()
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	return m
}
