package idtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// The tenants and projects of the contract. Open seeds nothing; the contract
// seeds what it uses.
const (
	TenantA  domain.TenantID  = "t088_acme"
	TenantB  domain.TenantID  = "t088_globex"
	ProjectA domain.ProjectID = "p088_web"
	ProjectB domain.ProjectID = "p088_api"
)

const issuer = "https://issuer.t088.test"

// Env is the identity module under contract: the persistence ports and the
// seeding the contract needs.
type Env interface {
	Repository() identity.Repository
	Directory() identity.Directory
	Mapper() identity.PrincipalMapper
	Access() identity.AccessReader
	// SeedProject creates the tenant (if new) and a project in it.
	SeedProject(t testing.TB, tenant domain.TenantID, project domain.ProjectID)
	// Bind seeds a binding with its authorization revision, as an earlier
	// committed grant would have left them.
	Bind(t testing.TB, tenant domain.TenantID, user identity.UserID, project domain.ProjectID, role identity.Role) identity.BindingID
	// Revision is the user's authorization revision in the tenant (0 if none).
	Revision(t testing.TB, tenant domain.TenantID, user identity.UserID) int64
	// EventIDs lists the tenant's outbox event IDs, sorted.
	EventIDs(t testing.TB, tenant domain.TenantID) []string
	// AuditActions lists the tenant's identity audit actions, sorted.
	AuditActions(t testing.TB, tenant domain.TenantID) []string
	// FailEvents makes every event append fail with err after the event was
	// written in the mutation's transaction; nil restores it.
	FailEvents(err error)
}

// Open builds an empty Env.
type Open func(testing.TB) Env

// failRecord wraps a repository so the Record of the next mutation fails: the
// last step of every identity mutation, after all of its other writes.
type failRecord struct {
	identity.Repository
	err error
}

func (f *failRecord) Transact(ctx context.Context, tenant domain.TenantID, fn func(context.Context, identity.Tx) error) error {
	return f.Repository.Transact(ctx, tenant, func(ctx context.Context, tx identity.Tx) error {
		return fn(ctx, failingTx{Tx: tx, f: f})
	})
}

type failingTx struct {
	identity.Tx
	f *failRecord
}

func (t failingTx) Record(ctx context.Context, c identity.Change) error {
	if t.f.err != nil {
		return t.f.err
	}
	return t.Tx.Record(ctx, c)
}

// Contract is the behavior every identity.Repository adapter shares, exercised
// through the use cases: one user per verified principal, principal-filtered
// discovery, binding grant/role change/revoke with versions and authorization
// revisions, scoped authority, duplicate and unique-key conflicts, projects with
// version checks, all-or-nothing mutations and tenant isolation. It asserts
// outcomes, not lock order or isolation levels.
func Contract(t *testing.T, open Open) {
	t.Helper()
	ctx := context.Background()

	type rig struct {
		env      Env
		svc      *identity.Service
		fault    *failRecord
		faulty   *identity.Service
		admin    identity.UserID
		engineer identity.UserID
		reviewer identity.UserID
		outsider identity.UserID
	}
	user := func(t *testing.T, env Env, subject string) identity.UserID {
		t.Helper()
		id, err := env.Mapper().MapPrincipal(ctx, identity.Principal{Issuer: issuer, Subject: subject})
		if err != nil {
			t.Fatalf("map %s: %v", subject, err)
		}
		return id
	}
	fresh := func(t *testing.T) *rig {
		t.Helper()
		env := open(t)
		env.SeedProject(t, TenantA, ProjectA)
		env.SeedProject(t, TenantA, ProjectB)
		env.SeedProject(t, TenantB, "p088_ops")
		r := &rig{env: env}
		r.admin, r.engineer, r.reviewer, r.outsider = user(t, env, "admin"), user(t, env, "engineer"), user(t, env, "reviewer"), user(t, env, "outsider")
		env.Bind(t, TenantA, r.admin, "", identity.RoleAdmin)
		var err error
		if r.svc, err = identity.NewService(identity.Config{Repository: env.Repository(), Directory: env.Directory()}); err != nil {
			t.Fatal(err)
		}
		r.fault = &failRecord{Repository: env.Repository()}
		if r.faulty, err = identity.NewService(identity.Config{Repository: r.fault, Directory: env.Directory()}); err != nil {
			t.Fatal(err)
		}
		return r
	}
	caller := func(u identity.UserID) identity.Caller { return identity.Caller{UserID: u, RequestID: "req_t088"} }
	grant := func(r *rig, by, to identity.UserID, project domain.ProjectID, role identity.Role) (identity.Binding, error) {
		return r.svc.Grant(ctx, identity.GrantCommand{Caller: caller(by), Tenant: TenantA, User: to, Project: project, Role: role})
	}
	bindings := func(t *testing.T, r *rig, tenant domain.TenantID, u identity.UserID) []identity.Binding {
		t.Helper()
		b, err := r.env.Access().CurrentBindings(ctx, tenant, u)
		if err != nil {
			t.Fatalf("current bindings: %v", err)
		}
		return b
	}

	t.Run("one user per verified principal and a login grants nothing", func(t *testing.T) {
		env := open(t)
		first := user(t, env, "first|login")
		if err := first.Validate(); err != nil {
			t.Fatalf("user id: %v", err)
		}
		for i := 0; i < 3; i++ {
			if again := user(t, env, "first|login"); again != first {
				t.Fatalf("login %d mapped to %q, first was %q", i, again, first)
			}
		}
		seen := map[identity.UserID]bool{first: true}
		for _, p := range []identity.Principal{
			{Issuer: issuer, Subject: "First|login"}, {Issuer: issuer + "/", Subject: "first|login"},
			{Issuer: "https://other.test", Subject: "first|login"}, {Issuer: issuer, Subject: "first|login "},
		} {
			id, err := env.Mapper().MapPrincipal(ctx, p)
			if err != nil || seen[id] {
				t.Fatalf("%+v collided with another user: %q %v", p, id, err)
			}
			seen[id] = true
		}
		for _, p := range []identity.Principal{{}, {Issuer: "i"}, {Subject: "s"}, {Issuer: "i", Subject: strings.Repeat("x", 513)}, {Issuer: "i\x00", Subject: "s"}} {
			if _, err := env.Mapper().MapPrincipal(ctx, p); !errors.Is(err, identity.ErrInvalid) {
				t.Errorf("%+v: %v", p, err)
			}
		}
		u, err := env.Directory().User(ctx, first)
		if err != nil || u.ID != first || u.Issuer != issuer || u.Subject != "first|login" || u.CreatedAt.IsZero() {
			t.Fatalf("own record: %+v %v", u, err)
		}
		if _, err := env.Directory().User(ctx, "usr_missing"); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("unknown user: %v", err)
		}
		if tenants, err := env.Directory().Tenants(ctx, first, "", 50); err != nil || len(tenants) != 0 {
			t.Fatalf("a login produced membership: %v %v", tenants, err)
		}
	})

	t.Run("discovery is filtered by the principal and pages by tenant ID", func(t *testing.T) {
		r := fresh(t)
		u := user(t, r.env, "pager")
		for i := 0; i < 5; i++ {
			tn := domain.TenantID(fmt.Sprintf("t088_page%d", i))
			r.env.SeedProject(t, tn, "p088_page")
			r.env.Bind(t, tn, u, "", identity.RoleViewer)
		}
		r.env.Bind(t, TenantA, r.engineer, ProjectA, identity.RoleEngineer)
		var seen []string
		after := ""
		for {
			page, err := r.env.Directory().Tenants(ctx, u, after, 2)
			if err != nil {
				t.Fatal(err)
			}
			for _, tn := range page {
				seen = append(seen, string(tn.ID))
				if len(tn.Roles) != 1 || tn.Roles[0] != identity.RoleViewer {
					t.Fatalf("roles of %s: %v", tn.ID, tn.Roles)
				}
			}
			if len(page) < 2 {
				break
			}
			after = string(page[len(page)-1].ID)
		}
		if len(seen) != 5 || seen[0] != "t088_page0" || seen[4] != "t088_page4" {
			t.Fatalf("pages: %v", seen)
		}
		// A project-only member sees the tenant, with no tenant-wide role.
		mine, err := r.env.Directory().Tenants(ctx, r.engineer, "", 50)
		if err != nil || len(mine) != 1 || mine[0].ID != TenantA || len(mine[0].Roles) != 0 {
			t.Fatalf("project-only member: %+v %v", mine, err)
		}
		if other, err := r.env.Directory().Tenants(ctx, r.outsider, "", 50); err != nil || len(other) != 0 {
			t.Fatalf("outsider: %+v %v", other, err)
		}
	})

	t.Run("grant, role change and revoke keep versions and revisions", func(t *testing.T) {
		r := fresh(t)
		b, err := grant(r, r.admin, r.engineer, "", identity.RoleViewer)
		if err != nil || b.Version != 1 || b.Role != identity.RoleViewer || b.User != r.engineer || b.Project != "" || b.Tenant != TenantA {
			t.Fatalf("grant: %+v %v", b, err)
		}
		if got := bindings(t, r, TenantA, r.engineer); len(got) != 1 || got[0].ID != b.ID {
			t.Fatalf("bindings after grant: %+v", got)
		}
		rev1 := r.env.Revision(t, TenantA, r.engineer)
		if rev1 < 1 {
			t.Fatalf("revision after grant: %d", rev1)
		}
		if _, err := grant(r, r.admin, r.engineer, "", identity.RoleViewer); !errors.Is(err, identity.ErrConflict) {
			t.Fatalf("a duplicate binding: %v", err)
		}
		if r.env.Revision(t, TenantA, r.engineer) != rev1 {
			t.Fatal("a refused grant advanced the revision")
		}
		if _, err := grant(r, r.admin, identity.UserID("usr_unknown"), "", identity.RoleViewer); !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("a grant to an unknown user: %v", err)
		}
		changed, err := r.svc.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 1, Role: identity.RoleEngineer})
		if err != nil || changed.Version != 2 || changed.Role != identity.RoleEngineer {
			t.Fatalf("role change: %+v %v", changed, err)
		}
		if rev2 := r.env.Revision(t, TenantA, r.engineer); rev2 != rev1+1 {
			t.Fatalf("revision after the change: %d, want %d", rev2, rev1+1)
		}
		if _, err := r.svc.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 1, Role: identity.RoleReviewer}); !errors.Is(err, identity.ErrVersionConflict) {
			t.Fatalf("a stale version: %v", err)
		}
		same, err := r.svc.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 2, Role: identity.RoleEngineer})
		if err != nil || same.Version != 2 || r.env.Revision(t, TenantA, r.engineer) != rev1+1 {
			t.Fatalf("the same role is a no-op: %+v %v", same, err)
		}
		if _, err := r.svc.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 1}); !errors.Is(err, identity.ErrVersionConflict) {
			t.Fatalf("revoking a stale version: %v", err)
		}
		if _, err := r.svc.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 2}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if got := bindings(t, r, TenantA, r.engineer); len(got) != 0 {
			t.Fatalf("access survived the revocation: %+v", got)
		}
		if rev3 := r.env.Revision(t, TenantA, r.engineer); rev3 != rev1+2 {
			t.Fatalf("the revision after revocation: %d, want %d", rev3, rev1+2)
		}
		if _, err := r.svc.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 2}); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("revoking a revoked binding: %v", err)
		}
	})

	t.Run("authority is scoped to the tenant and project of the binding", func(t *testing.T) {
		r := fresh(t)
		if _, err := grant(r, r.outsider, r.engineer, "", identity.RoleViewer); !errors.Is(err, identity.ErrHidden) {
			t.Fatalf("a user with no binding in the tenant: %v", err)
		}
		r.env.Bind(t, TenantA, r.reviewer, "", identity.RoleViewer)
		if _, err := grant(r, r.reviewer, r.engineer, "", identity.RoleViewer); !errors.Is(err, identity.ErrDenied) {
			t.Fatalf("a viewer granting: %v", err)
		}
		// A project administrator manages that project only.
		r.env.Bind(t, TenantA, r.engineer, ProjectA, identity.RoleAdmin)
		if _, err := grant(r, r.engineer, r.outsider, ProjectA, identity.RoleViewer); err != nil {
			t.Fatalf("project admin in its project: %v", err)
		}
		if _, err := grant(r, r.engineer, r.outsider, ProjectB, identity.RoleViewer); !errors.Is(err, identity.ErrHidden) {
			t.Fatalf("project admin in another project: %v", err)
		}
		if _, err := grant(r, r.engineer, r.outsider, "", identity.RoleViewer); !errors.Is(err, identity.ErrDenied) {
			t.Fatalf("project admin at tenant scope: %v", err)
		}
		if _, err := grant(r, r.admin, r.outsider, "p088_missing", identity.RoleViewer); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("a binding to a missing project: %v", err)
		}
		// Tenants do not share bindings: tenant B knows neither admin nor outsider.
		if got := bindings(t, r, TenantB, r.admin); len(got) != 0 {
			t.Fatalf("tenant A's admin has bindings in B: %+v", got)
		}
		if _, err := r.svc.ListBindings(ctx, identity.ListBindingsCommand{Caller: caller(r.admin), Tenant: TenantB, Limit: 10}); !errors.Is(err, identity.ErrHidden) {
			t.Fatalf("listing another tenant's bindings: %v", err)
		}
		list, err := r.svc.ListBindings(ctx, identity.ListBindingsCommand{Caller: caller(r.engineer), Tenant: TenantA, Limit: 10})
		if err != nil || len(list) != 2 { // the project admin's own binding and the one it granted
			t.Fatalf("a project admin lists its project only: %+v %v", list, err)
		}
		for _, b := range list {
			if b.Project != ProjectA {
				t.Fatalf("a binding outside the administered project: %+v", b)
			}
		}
	})

	t.Run("binding lists are ordered and paged by ID", func(t *testing.T) {
		r := fresh(t)
		for i := 0; i < 4; i++ {
			if _, err := grant(r, r.admin, user(t, r.env, fmt.Sprintf("member%d", i)), "", identity.RoleViewer); err != nil {
				t.Fatal(err)
			}
		}
		var all []identity.Binding
		after := identity.BindingID("")
		for {
			page, err := r.svc.ListBindings(ctx, identity.ListBindingsCommand{Caller: caller(r.admin), Tenant: TenantA, After: after, Limit: 2})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, page...)
			if len(page) < 2 {
				break
			}
			after = page[len(page)-1].ID
		}
		if len(all) != 5 { // four members and the admin
			t.Fatalf("%d bindings", len(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i-1].ID >= all[i].ID {
				t.Fatalf("not ordered by ID: %+v", all)
			}
		}
	})

	t.Run("projects are created once per key and updated under their version", func(t *testing.T) {
		r := fresh(t)
		pv, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.admin), Tenant: TenantA, Key: "billing", DisplayName: "Billing", ActiveWIPLimit: 3})
		if err != nil || pv.Project.Version != 1 || pv.Project.Key != "billing" || pv.Project.ActiveWIPLimit != 3 || len(pv.CallerRoles) != 1 {
			t.Fatalf("create: %+v %v", pv, err)
		}
		if _, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.admin), Tenant: TenantA, Key: "billing", DisplayName: "Again"}); !errors.Is(err, identity.ErrConflict) {
			t.Fatalf("a duplicate key: %v", err)
		}
		// The key is unique per tenant, not globally.
		r.env.Bind(t, TenantB, r.admin, "", identity.RoleAdmin)
		if _, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.admin), Tenant: TenantB, Key: "billing", DisplayName: "Billing B"}); err != nil {
			t.Fatalf("the same key in another tenant: %v", err)
		}
		r.env.Bind(t, TenantA, r.reviewer, "", identity.RoleViewer)
		if _, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.reviewer), Tenant: TenantA, Key: "other", DisplayName: "Other"}); !errors.Is(err, identity.ErrDenied) {
			t.Fatalf("a viewer created a project: %v", err)
		}
		scope := pv.Project.Scope
		name := "Billing and invoicing"
		up, err := r.svc.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: caller(r.admin), Scope: scope, Expected: 1, Patch: identity.ProjectPatch{DisplayName: &name}})
		if err != nil || up.Project.Version != 2 || up.Project.DisplayName != name {
			t.Fatalf("update: %+v %v", up, err)
		}
		if _, err := r.svc.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: caller(r.admin), Scope: scope, Expected: 1, Patch: identity.ProjectPatch{DisplayName: &name}}); !errors.Is(err, identity.ErrVersionConflict) {
			t.Fatalf("a stale project version: %v", err)
		}
		// One update changes settings and bindings in one version step.
		wip := 7
		both, err := r.svc.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: caller(r.admin), Scope: scope, Expected: 2,
			Patch: identity.ProjectPatch{ActiveWIPLimit: &wip}, Grant: []identity.BindingGrant{{User: r.engineer, Role: identity.RoleEngineer}}})
		if err != nil || both.Project.Version != 3 || both.Project.ActiveWIPLimit != 7 {
			t.Fatalf("settings and bindings: %+v %v", both, err)
		}
		if got := bindings(t, r, TenantA, r.engineer); len(got) != 1 || got[0].Project != scope.ProjectID || got[0].Role != identity.RoleEngineer {
			t.Fatalf("binding granted through the project: %+v", got)
		}
		got, err := r.svc.GetProject(ctx, caller(r.engineer), scope)
		if err != nil || got.Project.Version != 3 || len(got.CallerRoles) != 1 || got.CallerRoles[0] != identity.RoleEngineer {
			t.Fatalf("project as the engineer sees it: %+v %v", got, err)
		}
		if _, err := r.svc.GetProject(ctx, caller(r.engineer), domain.Scope{TenantID: TenantA, ProjectID: ProjectA}); !errors.Is(err, identity.ErrHidden) {
			t.Fatalf("a project the engineer is not bound to: %v", err)
		}
		list, err := r.svc.ListProjects(ctx, caller(r.engineer), TenantA, "", 10)
		if err != nil || len(list) != 1 || list[0].Project.Scope != scope {
			t.Fatalf("project-only member lists: %+v %v", list, err)
		}
		all, err := r.svc.ListProjects(ctx, caller(r.admin), TenantA, "", 10)
		if err != nil || len(all) != 3 {
			t.Fatalf("tenant admin lists: %d %v", len(all), err)
		}
	})

	t.Run("a mutation records its audit rows and events with its state", func(t *testing.T) {
		r := fresh(t)
		baseEvents, baseAudit := r.env.EventIDs(t, TenantA), r.env.AuditActions(t, TenantA)
		eventsOf := func() []string { return r.env.EventIDs(t, TenantA) }
		hasPrefix := func(ids []string, p string) int {
			n := 0
			for _, id := range ids {
				if strings.HasPrefix(id, p) {
					n++
				}
			}
			return n
		}

		// A tenant-wide grant: one binding audit row and one membership.changed.
		b, err := grant(r, r.admin, r.engineer, "", identity.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		ev := eventsOf()
		if len(ev) != len(baseEvents)+1 {
			t.Fatalf("a tenant-wide grant wrote %d events: %v", len(ev)-len(baseEvents), ev)
		}
		audit := r.env.AuditActions(t, TenantA)
		if len(audit) != len(baseAudit)+1 || !slices.Contains(audit, string(identity.ChangeBindingGranted)) {
			t.Fatalf("audit after the grant: %v", audit)
		}
		// A project grant also bumps the project: both events and both audit rows.
		before := len(ev)
		pb, err := grant(r, r.admin, r.reviewer, ProjectA, identity.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(eventsOf()); got != before+2 {
			t.Fatalf("a project grant wrote %d events", got-before)
		}
		if got := len(r.env.AuditActions(t, TenantA)); got != len(audit)+2 {
			t.Fatalf("a project grant wrote %d audit rows", got-len(audit))
		}
		if _, err := r.svc.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: pb.ID, Expected: 1, Role: identity.RoleEngineer}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.svc.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: b.ID, Expected: 1}); err != nil {
			t.Fatal(err)
		}
		actions := r.env.AuditActions(t, TenantA)
		for _, want := range []string{string(identity.ChangeBindingGranted), string(identity.ChangeBindingRoleChanged), string(identity.ChangeBindingRevoked), string(identity.ChangeProjectUpdated)} {
			if !slices.Contains(actions, want) {
				t.Fatalf("audit lacks %s: %v", want, actions)
			}
		}
		if _, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.admin), Tenant: TenantA, Key: "events", DisplayName: "Events"}); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(r.env.AuditActions(t, TenantA), string(identity.ChangeProjectCreated)) {
			t.Fatalf("audit lacks the project creation: %v", r.env.AuditActions(t, TenantA))
		}
		all := eventsOf()
		if hasPrefix(all, "evt_chg_") != len(all) || len(all) != len(baseEvents)+1+2+2+1+1 {
			t.Fatalf("events: %v", all)
		}
		// Refused requests wrote nothing, in any tenant.
		n := len(all)
		if _, err := grant(r, r.admin, r.engineer, "", identity.RoleViewer); err != nil {
			t.Fatal(err)
		}
		if _, err := grant(r, r.admin, r.engineer, "", identity.RoleViewer); !errors.Is(err, identity.ErrConflict) {
			t.Fatalf("a duplicate: %v", err)
		}
		if _, err := r.svc.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: pb.ID, Expected: 1}); !errors.Is(err, identity.ErrVersionConflict) {
			t.Fatalf("a stale revoke: %v", err)
		}
		if got := len(eventsOf()); got != n+1 {
			t.Fatalf("refused requests wrote events: %d -> %d", n, got)
		}
		if got := r.env.EventIDs(t, TenantB); len(got) != 0 {
			t.Fatalf("another tenant holds the events: %v", got)
		}
	})

	t.Run("a transaction writes only its own tenant's records", func(t *testing.T) {
		r := fresh(t)
		repo := r.env.Repository()
		now := time.Now().UTC().Truncate(time.Microsecond)
		events0, audit0 := r.env.EventIDs(t, TenantA), r.env.AuditActions(t, TenantA)
		eventsB0, auditB0 := r.env.EventIDs(t, TenantB), r.env.AuditActions(t, TenantB)

		// A foreign binding or project is refused as invalid, and with it the valid
		// writes of the same unit that came before.
		foreignBinding, err := identity.Grant("mb_foreign", TenantB, r.engineer, "", identity.RoleViewer, now)
		if err != nil {
			t.Fatal(err)
		}
		foreignProject, err := identity.NewProject(domain.Scope{TenantID: TenantB, ProjectID: "p088_foreign"}, "foreign", "Foreign", nil, 0, now)
		if err != nil {
			t.Fatal(err)
		}
		ownBinding, err := identity.Grant("mb_own", TenantA, r.engineer, "", identity.RoleViewer, now)
		if err != nil {
			t.Fatal(err)
		}
		for name, write := range map[string]func(ctx context.Context, tx identity.Tx) error{
			"binding": func(ctx context.Context, tx identity.Tx) error { return tx.InsertBinding(ctx, foreignBinding) },
			"project": func(ctx context.Context, tx identity.Tx) error { return tx.InsertProject(ctx, foreignProject) },
		} {
			err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) error {
				if err := tx.LockAuthorization(ctx, []identity.UserID{r.engineer}, nil); err != nil {
					return err
				}
				if err := tx.InsertBinding(ctx, ownBinding); err != nil {
					return err
				}
				if _, err := tx.BumpAuthorization(ctx, r.engineer); err != nil {
					return err
				}
				return write(ctx, tx)
			})
			if !errors.Is(err, identity.ErrInvalid) {
				t.Fatalf("a foreign %s in this tenant's transaction: %v", name, err)
			}
		}
		if got := bindings(t, r, TenantA, r.engineer); len(got) != 0 {
			t.Fatalf("a refused unit left bindings in the tenant: %+v", got)
		}
		if got := bindings(t, r, TenantB, r.engineer); len(got) != 0 {
			t.Fatalf("a foreign binding was stored: %+v", got)
		}
		if got := r.env.Revision(t, TenantA, r.engineer); got != 0 {
			t.Fatalf("a refused unit advanced the revision: %d", got)
		}
		if pv, err := r.svc.ListProjects(ctx, caller(r.admin), TenantA, "", 50); err != nil {
			t.Fatal(err)
		} else {
			for _, p := range pv {
				if p.Project.Key == "foreign" || p.Project.Scope.ProjectID == "p088_foreign" {
					t.Fatalf("a foreign project is visible in the tenant: %+v", p.Project)
				}
			}
		}
		if got := r.env.EventIDs(t, TenantA); !slices.Equal(got, events0) {
			t.Fatalf("events changed: %v -> %v", events0, got)
		}
		if got := r.env.AuditActions(t, TenantA); !slices.Equal(got, audit0) {
			t.Fatalf("audit changed: %v -> %v", audit0, got)
		}
		if got := r.env.EventIDs(t, TenantB); !slices.Equal(got, eventsB0) {
			t.Fatalf("the other tenant's events changed: %v", got)
		}
		if got := r.env.AuditActions(t, TenantB); !slices.Equal(got, auditB0) {
			t.Fatalf("the other tenant's audit changed: %v", got)
		}

		// An update changes the role or the editable fields only: ownership, scope
		// and creation time are immutable.
		granted, err := grant(r, r.admin, r.engineer, ProjectA, identity.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		// The baseline is the persisted binding: a store may keep a coarser clock
		// than the service that created it (PostgreSQL keeps microseconds).
		var b identity.Binding
		if err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) (err error) {
			b, err = tx.Binding(ctx, granted.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		hijack := b
		hijack.Tenant, hijack.User, hijack.Project, hijack.CreatedAt = TenantB, r.outsider, ProjectB, now.Add(-time.Hour)
		hijack.Role, hijack.Version = identity.RoleEngineer, b.Version+1
		if err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) error {
			return tx.UpdateBinding(ctx, hijack, b.Version)
		}); err != nil {
			t.Fatal(err)
		}
		got := bindings(t, r, TenantA, r.engineer)
		if len(got) != 1 || got[0].ID != b.ID || got[0].Tenant != TenantA || got[0].User != r.engineer || got[0].Project != ProjectA ||
			got[0].Role != identity.RoleEngineer || got[0].Version != b.Version+1 || !got[0].CreatedAt.Equal(b.CreatedAt) {
			t.Fatalf("an update changed immutable binding fields: %+v (was %+v)", got, b)
		}
		if len(bindings(t, r, TenantA, r.outsider)) != 0 || len(bindings(t, r, TenantB, r.outsider)) != 0 {
			t.Fatal("an update moved the binding to another user or tenant")
		}

		var cur identity.Project
		if err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) (err error) {
			cur, err = tx.Project(ctx, ProjectA)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		moved := cur
		moved.Scope, moved.Key, moved.CreatedAt = domain.Scope{TenantID: TenantB, ProjectID: ProjectA}, "renamed-key", now.Add(-time.Hour)
		moved.DisplayName, moved.Version, moved.UpdatedAt = "Renamed", cur.Version+1, now
		if err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) error {
			return tx.UpdateProject(ctx, moved, cur.Version)
		}); err != nil {
			t.Fatal(err)
		}
		var after identity.Project
		if err := repo.Transact(ctx, TenantA, func(ctx context.Context, tx identity.Tx) (err error) {
			after, err = tx.Project(ctx, ProjectA)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if after.Scope != cur.Scope || after.Key != cur.Key || !after.CreatedAt.Equal(cur.CreatedAt) || after.DisplayName != "Renamed" || after.Version != cur.Version+1 {
			t.Fatalf("an update changed immutable project fields: %+v (was %+v)", after, cur)
		}
	})

	t.Run("a mutation commits all of its writes or none", func(t *testing.T) {
		r := fresh(t)
		r.env.Bind(t, TenantA, r.reviewer, ProjectA, identity.RoleViewer)
		keep, err := grant(r, r.admin, r.engineer, ProjectA, identity.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		before := bindings(t, r, TenantA, r.engineer)
		rev := r.env.Revision(t, TenantA, r.engineer)
		projectVersion := func() domain.Version {
			pv, err := r.svc.GetProject(ctx, caller(r.admin), domain.Scope{TenantID: TenantA, ProjectID: ProjectA})
			if err != nil {
				t.Fatalf("get project: %v", err)
			}
			return pv.Project.Version
		}
		v0 := projectVersion()

		r.fault.err = errors.New("audit write failed")
		if _, err := r.faulty.Grant(ctx, identity.GrantCommand{Caller: caller(r.admin), Tenant: TenantA, User: r.outsider, Project: ProjectA, Role: identity.RoleEngineer}); err == nil {
			t.Fatal("a grant reported success although recording failed")
		}
		if _, err := r.faulty.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: keep.ID, Expected: 1, Role: identity.RoleEngineer}); err == nil {
			t.Fatal("a role change reported success although recording failed")
		}
		if _, err := r.faulty.Revoke(ctx, identity.RevokeCommand{Caller: caller(r.admin), Tenant: TenantA, ID: keep.ID, Expected: 1}); err == nil {
			t.Fatal("a revocation reported success although recording failed")
		}
		if got := bindings(t, r, TenantA, r.outsider); len(got) != 0 {
			t.Fatalf("a failed grant left a binding: %+v", got)
		}
		after := bindings(t, r, TenantA, r.engineer)
		if len(after) != len(before) || after[0].Role != identity.RoleViewer || after[0].Version != 1 {
			t.Fatalf("failed mutations changed the binding: %+v -> %+v", before, after)
		}
		if got := r.env.Revision(t, TenantA, r.engineer); got != rev || r.env.Revision(t, TenantA, r.outsider) != 0 {
			t.Fatalf("failed mutations advanced a revision: %d -> %d", rev, got)
		}
		if v := projectVersion(); v != v0 {
			t.Fatalf("failed mutations advanced the project version: %d -> %d", v0, v)
		}
		// A failing event append, after the binding, revision, project bump and audit
		// rows were written, undoes all of them.
		r.fault.err = nil
		events0, audit0 := r.env.EventIDs(t, TenantA), r.env.AuditActions(t, TenantA)
		r.env.FailEvents(errors.New("event bus unavailable"))
		if _, err := r.svc.Grant(ctx, identity.GrantCommand{Caller: caller(r.admin), Tenant: TenantA, User: r.outsider, Project: ProjectA, Role: identity.RoleEngineer}); err == nil {
			t.Fatal("a grant reported success although its event could not be appended")
		}
		if _, err := r.svc.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: caller(r.admin), Tenant: TenantA, ID: keep.ID, Expected: 1, Role: identity.RoleEngineer}); err == nil {
			t.Fatal("a role change reported success although its event could not be appended")
		}
		if _, err := r.svc.CreateProject(ctx, identity.CreateProjectCommand{Caller: caller(r.admin), Tenant: TenantA, Key: "never", DisplayName: "Never"}); err == nil {
			t.Fatal("a project was created although its event could not be appended")
		}
		r.env.FailEvents(nil)
		if got := bindings(t, r, TenantA, r.outsider); len(got) != 0 {
			t.Fatalf("a failed event append left a binding: %+v", got)
		}
		if got := bindings(t, r, TenantA, r.engineer); len(got) != len(before) || got[0].Role != identity.RoleViewer || got[0].Version != 1 {
			t.Fatalf("a failed event append changed a binding: %+v", got)
		}
		if got := r.env.Revision(t, TenantA, r.engineer); got != rev || r.env.Revision(t, TenantA, r.outsider) != 0 {
			t.Fatalf("a failed event append advanced a revision: %d", got)
		}
		if v := projectVersion(); v != v0 {
			t.Fatalf("a failed event append advanced the project version: %d -> %d", v0, v)
		}
		if got := r.env.EventIDs(t, TenantA); !slices.Equal(got, events0) {
			t.Fatalf("a failed unit left events: %v -> %v", events0, got)
		}
		if got := r.env.AuditActions(t, TenantA); !slices.Equal(got, audit0) {
			t.Fatalf("a failed unit left audit rows: %v -> %v", audit0, got)
		}
		if pv, err := r.svc.ListProjects(ctx, caller(r.admin), TenantA, "", 20); err != nil {
			t.Fatal(err)
		} else {
			for _, p := range pv {
				if p.Project.Key == "never" {
					t.Fatal("a failed event append left a project")
				}
			}
		}
		// Nothing was replayed: the same grant now applies exactly once.
		if _, err := r.faulty.Grant(ctx, identity.GrantCommand{Caller: caller(r.admin), Tenant: TenantA, User: r.outsider, Project: ProjectA, Role: identity.RoleEngineer}); err != nil {
			t.Fatalf("retry: %v", err)
		}
		if got := bindings(t, r, TenantA, r.outsider); len(got) != 1 || r.env.Revision(t, TenantA, r.outsider) != 1 {
			t.Fatalf("the retry did not apply exactly once: %+v", got)
		}
		if v := projectVersion(); v != v0+1 {
			t.Fatalf("the retry advanced the project version to %d, want %d", v, v0+1)
		}
	})
}
