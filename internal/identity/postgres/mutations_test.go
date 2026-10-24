package postgres_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

func (f *fixture) projectVersion(project domain.ProjectID) int64 {
	var v int64
	if err := f.Owner.QueryRow(ctx, `SELECT version FROM projects WHERE tenant_id = $1 AND id = $2`, tenantA, string(project)).Scan(&v); err != nil {
		f.T.Fatal(err)
	}
	return v
}

func (f *fixture) auditCount(action string) int {
	return f.CountIn(tenantA, `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2`, string(tenantA), action)
}

func (f *fixture) eventCount(typ string) int { return len(f.Outbox(tenantA, typ)) }

func TestMembershipAndProjectConcurrency(t *testing.T) {
	t.Run("grant, duplicate, stale version and revocation", func(t *testing.T) {
		f := newFixture(t, idtest0())
		b := must(f.grant(f.admin, f.engineer, "", identity.RoleEngineer))
		if b.Version != 1 || b.User != f.engineer || b.Tenant != tenantA || b.Project != "" {
			t.Fatalf("%+v", b)
		}
		if got := f.Revision(tenantA, f.engineer); got != 1 {
			t.Fatalf("revision %d", got)
		}
		audit, events := f.auditCount("membership.granted"), f.eventCount(eventsv1.TypeMembershipChanged)

		if _, err := f.grant(f.admin, f.engineer, "", identity.RoleEngineer); !is(err, identity.ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		if f.auditCount("membership.granted") != audit || f.eventCount(eventsv1.TypeMembershipChanged) != events || f.Revision(tenantA, f.engineer) != 1 {
			t.Fatal("a duplicate grant left audit, event or revision behind")
		}

		// Composition: a second binding of the same user in the same scope is distinct.
		second := must(f.grant(f.admin, f.engineer, "", identity.RoleReviewer))
		if f.Revision(tenantA, f.engineer) != 2 {
			t.Fatal("second grant did not advance the revision")
		}

		if _, err := f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 5, Role: identity.RoleViewer}); !is(err, identity.ErrVersionConflict) {
			t.Fatalf("stale: %v", err)
		}
		if _, err := f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 1, Role: identity.RoleEngineer}); !is(err, identity.ErrConflict) {
			t.Fatalf("role change onto an existing binding: %v", err)
		}
		changed := must(f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 1, Role: identity.RoleViewer}))
		if changed.Version != 2 || changed.Role != identity.RoleViewer || f.Revision(tenantA, f.engineer) != 3 {
			t.Fatalf("%+v rev=%d", changed, f.Revision(tenantA, f.engineer))
		}
		// Same role: no write.
		same := must(f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 2, Role: identity.RoleViewer}))
		if same.Version != 2 || f.Revision(tenantA, f.engineer) != 3 {
			t.Fatalf("no-op wrote: %+v", same)
		}
		if _, err := f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 1}); !is(err, identity.ErrVersionConflict) {
			t.Fatalf("stale revoke: %v", err)
		}
		must(f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 2}))
		if _, err := f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: second.ID, Expected: 2}); !is(err, identity.ErrNotFound) {
			t.Fatalf("second revoke: %v", err)
		}
		if f.Revision(tenantA, f.engineer) != 4 {
			t.Fatalf("revision %d", f.Revision(tenantA, f.engineer))
		}

		t.Run("revision survives the last revocation and keeps rising", func(t *testing.T) {
			first := must(f.Service.ListBindings(ctx, identity.ListBindingsCommand{Caller: f.caller(f.admin), Tenant: tenantA, User: f.engineer, Limit: 10}))
			if len(first) != 1 {
				t.Fatalf("%+v", first)
			}
			must(f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: first[0].ID, Expected: first[0].Version}))
			if rev := f.Revision(tenantA, f.engineer); rev != 5 {
				t.Fatalf("revision after last revoke: %d", rev)
			}
			if f.Count(`SELECT count(*) FROM memberships WHERE user_id = $1`, string(f.engineer)) != 0 {
				t.Fatal("bindings remain")
			}
			must(f.grant(f.admin, f.engineer, "", identity.RoleViewer))
			if rev := f.Revision(tenantA, f.engineer); rev != 6 {
				t.Fatalf("revision after regrant: %d (must not reset)", rev)
			}
		})

		t.Run("events carry typed payloads with the effective version", func(t *testing.T) {
			var last eventsv1.MembershipChanged
			payloads := f.Outbox(tenantA, eventsv1.TypeMembershipChanged)
			env, err := eventsv1.Decode([]byte(payloads[len(payloads)-1]))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(env.Data, &last); err != nil {
				t.Fatal(err)
			}
			if last.UserID != string(f.engineer) || last.EffectiveVersion != 6 || len(last.BindingIDs) != 1 ||
				env.Resource.Type != "membership" || env.Resource.Version != 6 || env.Actor.ID != string(f.admin) || env.Actor.Type != eventsv1.ActorUser {
				t.Fatalf("%+v %+v", last, env)
			}
		})
	})

	t.Run("concurrent identical grants commit once", func(t *testing.T) {
		f := newFixture(t, idtest0())
		const n = 16
		var wg sync.WaitGroup
		results := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, results[i] = f.grant(f.admin, f.engineer, "", identity.RoleEngineer)
			}()
		}
		close(start)
		wg.Wait()
		ok, dup := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				ok++
			case is(err, identity.ErrConflict):
				dup++
			default:
				t.Fatalf("unexpected: %v", err)
			}
		}
		if ok != 1 || dup != n-1 {
			t.Fatalf("ok=%d dup=%d", ok, dup)
		}
		if f.Revision(tenantA, f.engineer) != 1 || f.auditCount("membership.granted") != 1 { // fixture seeds are not audited
			t.Fatalf("revision=%d audit=%d", f.Revision(tenantA, f.engineer), f.auditCount("membership.granted"))
		}
		if got := f.eventCount(eventsv1.TypeMembershipChanged); got != 1 {
			t.Fatalf("%d events", got)
		}
	})

	t.Run("simultaneous revoke and role update leave one consistent result", func(t *testing.T) {
		for round := 0; round < 8; round++ {
			f := newFixture(t, idtest0())
			b := must(f.grant(f.admin, f.engineer, "", identity.RoleViewer))
			var wg sync.WaitGroup
			var revokeErr, updateErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, revokeErr = f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: b.ID, Expected: 1})
			}()
			go func() {
				defer wg.Done()
				_, updateErr = f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(f.admin), Tenant: tenantA, ID: b.ID, Expected: 1, Role: identity.RoleReviewer})
			}()
			wg.Wait()
			rows := f.Count(`SELECT count(*) FROM memberships WHERE id = $1`, string(b.ID))
			switch {
			case revokeErr == nil && updateErr == nil:
				// Update first (version 2), revoke then needs version 1: impossible. Revoke first, update finds nothing.
				t.Fatalf("both succeeded on the same version")
			case revokeErr == nil:
				if rows != 0 || !is(updateErr, identity.ErrNotFound) && !is(updateErr, identity.ErrVersionConflict) {
					t.Fatalf("rows=%d update=%v", rows, updateErr)
				}
			case updateErr == nil:
				if rows != 1 || !is(revokeErr, identity.ErrVersionConflict) {
					t.Fatalf("rows=%d revoke=%v", rows, revokeErr)
				}
			default:
				t.Fatalf("both failed: %v / %v", revokeErr, updateErr)
			}
			// Exactly one change advanced the revision beyond the grant.
			if rev := f.Revision(tenantA, f.engineer); rev != 2 {
				t.Fatalf("revision %d", rev)
			}
		}
	})

	t.Run("scope escalation is refused without disclosure", func(t *testing.T) {
		f := newFixture(t, idtest0())
		pa := f.User("https://issuer.test", "project-admin")
		viewer := f.User("https://issuer.test", "viewer")
		must(f.grant(f.admin, pa, projectA, identity.RoleAdmin))
		must(f.grant(f.admin, viewer, "", identity.RoleViewer))
		target := f.User("https://issuer.test", "target")

		// Tenant-wide grant by a project administrator: a known tenant member, so denied.
		if _, err := f.grant(pa, target, "", identity.RoleViewer); !is(err, identity.ErrDenied) {
			t.Fatalf("tenant-wide by project admin: %v", err)
		}
		// Another project: hidden, exactly as a missing project is.
		if _, err := f.grant(pa, target, projectA2, identity.RoleViewer); !is(err, identity.ErrHidden) {
			t.Fatalf("other project: %v", err)
		}
		if _, err := f.grant(pa, target, "p010_missing", identity.RoleViewer); !is(err, identity.ErrNotFound) {
			t.Fatalf("missing project: %v", err)
		}
		// Own project works and stays inside it.
		in := must(f.grant(pa, target, projectA, identity.RoleAnalyst))
		if in.Project != projectA {
			t.Fatalf("%+v", in)
		}
		// A viewer administers nothing.
		if _, err := f.grant(viewer, target, projectA, identity.RoleViewer); !is(err, identity.ErrDenied) {
			t.Fatalf("viewer: %v", err)
		}
		// An outsider learns nothing about the tenant.
		if _, err := f.grant(f.outsider, target, projectA, identity.RoleViewer); !is(err, identity.ErrHidden) {
			t.Fatalf("outsider: %v", err)
		}
		// Binding of another project or the tenant scope cannot be changed or revoked by the project admin.
		tenantWide := must(f.grant(f.admin, target, "", identity.RoleViewer))
		other := must(f.grant(f.admin, target, projectA2, identity.RoleViewer))
		for name, b := range map[string]identity.Binding{"tenant-wide": tenantWide, "other project": other} {
			_, err := f.Service.ChangeRole(ctx, identity.ChangeRoleCommand{Caller: f.caller(pa), Tenant: tenantA, ID: b.ID, Expected: b.Version, Role: identity.RoleAdmin})
			if !is(err, identity.ErrDenied) && !is(err, identity.ErrHidden) {
				t.Fatalf("%s change: %v", name, err)
			}
			_, err = f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(pa), Tenant: tenantA, ID: b.ID, Expected: b.Version})
			if !is(err, identity.ErrDenied) && !is(err, identity.ErrHidden) {
				t.Fatalf("%s revoke: %v", name, err)
			}
		}
		// A foreign tenant's binding ID is simply not there.
		if _, err := f.Service.Revoke(ctx, identity.RevokeCommand{Caller: f.caller(f.admin), Tenant: tenantB, ID: tenantWide.ID, Expected: 1}); !is(err, identity.ErrHidden) && !is(err, identity.ErrNotFound) {
			t.Fatalf("foreign tenant: %v", err)
		}
		// Unknown users cannot be bound.
		if _, err := f.grant(f.admin, "u_unknown", "", identity.RoleViewer); !is(err, identity.ErrInvalid) {
			t.Fatalf("unknown user: %v", err)
		}
		// Custom roles never reach the database.
		if _, err := f.grant(f.admin, target, "", "owner"); !is(err, identity.ErrInvalid) {
			t.Fatalf("custom role: %v", err)
		}
		// The project admin sees only bindings of the administered project.
		listed := must(f.Service.ListBindings(ctx, identity.ListBindingsCommand{Caller: f.caller(pa), Tenant: tenantA, Limit: 100}))
		for _, b := range listed {
			if b.Project != projectA {
				t.Fatalf("project admin sees %+v", b)
			}
		}
		if len(listed) != 2 { // pa's own binding and the analyst
			t.Fatalf("%d listed", len(listed))
		}
		if got := must(f.Service.ListBindings(ctx, identity.ListBindingsCommand{Caller: f.caller(pa), Tenant: tenantA, Project: projectA2, Limit: 100})); len(got) != 0 {
			t.Fatalf("filter widened: %+v", got)
		}
		if _, err := f.Service.ListBindings(ctx, identity.ListBindingsCommand{Caller: f.caller(viewer), Tenant: tenantA, Limit: 10}); !is(err, identity.ErrDenied) {
			t.Fatalf("viewer list: %v", err)
		}
	})

	t.Run("project binding changes keep one consistent project version", func(t *testing.T) {
		f := newFixture(t, idtest0())
		if v := f.projectVersion(projectA); v != 1 {
			t.Fatal(v)
		}
		// Entry point 1: membership route.
		b := must(f.grant(f.admin, f.engineer, projectA, identity.RoleEngineer))
		if v := f.projectVersion(projectA); v != 2 {
			t.Fatalf("project version %d after a membership grant", v)
		}
		if f.projectVersion(projectA2) != 1 {
			t.Fatal("another project's version moved")
		}
		// Tenant-wide bindings do not touch any project.
		must(f.grant(f.admin, f.reviewer, "", identity.RoleReviewer))
		if v := f.projectVersion(projectA); v != 2 {
			t.Fatalf("tenant-wide grant moved the project version to %d", v)
		}
		// Entry point 2: project update with settings and bindings together: one step.
		name := "Inventory"
		p := must(f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{
			Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: 2,
			Patch:  identity.ProjectPatch{DisplayName: &name},
			Grant:  []identity.BindingGrant{{User: f.reviewer, Role: identity.RoleReviewer}, {User: f.outsider, Role: identity.RoleViewer}},
			Revoke: []identity.BindingID{b.ID},
		}))
		if p.Project.Version != 3 || p.Project.DisplayName != name {
			t.Fatalf("%+v", p)
		}
		if f.Count(`SELECT count(*) FROM memberships WHERE tenant_id = $1 AND project_id = $2`, tenantA, string(projectA)) != 2 {
			t.Fatal("unexpected bindings after the project update")
		}
		// Three users were affected: one membership.changed each, one project event.
		events := f.Outbox(tenantA, eventsv1.TypeProjectSettingsChanged)
		env, err := eventsv1.Decode([]byte(events[len(events)-1]))
		if err != nil {
			t.Fatal(err)
		}
		var psc eventsv1.ProjectSettingsChanged
		_ = json.Unmarshal(env.Data, &psc)
		if psc.Version != 3 || strings.Join(psc.ChangedSettings, ",") != "displayName,roleBindings" {
			t.Fatalf("%+v", psc)
		}
		// Stale expected version, then a refused patch leaves everything unchanged.
		if _, err := f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: 2, Patch: identity.ProjectPatch{DisplayName: &name}}); !is(err, identity.ErrVersionConflict) {
			t.Fatalf("stale: %v", err)
		}
		// A binding of another project cannot be revoked through this project.
		foreign := must(f.grant(f.admin, f.reviewer, projectA2, identity.RoleViewer))
		before := f.projectVersion(projectA)
		if _, err := f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: domain.Version(before), Revoke: []identity.BindingID{foreign.ID}}); !is(err, identity.ErrNotFound) {
			t.Fatalf("foreign revoke: %v", err)
		}
		if f.Count(`SELECT count(*) FROM memberships WHERE id = $1`, string(foreign.ID)) != 1 || f.projectVersion(projectA) != before {
			t.Fatal("a refused update left effects")
		}
		// Unchanged patch is a no-op.
		same := must(f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.admin), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: domain.Version(before), Patch: identity.ProjectPatch{DisplayName: &name}}))
		if same.Project.Version != domain.Version(before) {
			t.Fatalf("no-op bumped: %+v", same)
		}
		// A project administrator may edit settings but a viewer may not.
		viewer := f.User("https://issuer.test", "viewer2")
		must(f.grant(f.admin, viewer, projectA, identity.RoleViewer))
		n2 := "x"
		if _, err := f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(viewer), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA}, Expected: domain.Version(f.projectVersion(projectA)), Patch: identity.ProjectPatch{DisplayName: &n2}}); !is(err, identity.ErrDenied) {
			t.Fatalf("viewer update: %v", err)
		}
		// A project settings admin without membership authority cannot smuggle bindings... (admin has both, so use an engineer holding neither)
		if _, err := f.Service.UpdateProject(ctx, identity.UpdateProjectCommand{Caller: f.caller(f.engineer), Scope: domain.Scope{TenantID: tenantA, ProjectID: projectA2}, Expected: 1, Patch: identity.ProjectPatch{DisplayName: &n2}}); !is(err, identity.ErrHidden) && !is(err, identity.ErrDenied) {
			t.Fatalf("engineer elsewhere: %v", err)
		}
	})

	t.Run("concurrent project-scoped grants advance the project version once each", func(t *testing.T) {
		f := newFixture(t, idtest0())
		const n = 12
		users := make([]identity.UserID, n)
		for i := range users {
			users[i] = f.User("https://issuer.test", "bulk-"+string(rune('a'+i)))
		}
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = f.grant(f.admin, users[i], projectA, identity.RoleViewer)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("grant %d: %v", i, err)
			}
		}
		if v := f.projectVersion(projectA); v != n+1 {
			t.Fatalf("project version %d, want %d", v, n+1)
		}
		if got := f.eventCount(eventsv1.TypeProjectSettingsChanged); got != n {
			t.Fatalf("%d project events", got)
		}
	})

	t.Run("project creation", func(t *testing.T) {
		f := newFixture(t, idtest0())
		p := must(f.Service.CreateProject(ctx, identity.CreateProjectCommand{Caller: f.caller(f.admin), Tenant: tenantA, Key: "new-app", DisplayName: "New", ActiveWIPLimit: 3}))
		if p.Project.Version != 1 || p.Project.Key != "new-app" || p.Project.ActiveWIPLimit != 3 || p.Project.Scope.TenantID != tenantA {
			t.Fatalf("%+v", p)
		}
		before := f.eventCount(eventsv1.TypeProjectSettingsChanged)
		if _, err := f.Service.CreateProject(ctx, identity.CreateProjectCommand{Caller: f.caller(f.admin), Tenant: tenantA, Key: "new-app"}); !is(err, identity.ErrConflict) {
			t.Fatalf("duplicate key: %v", err)
		}
		if f.eventCount(eventsv1.TypeProjectSettingsChanged) != before {
			t.Fatal("a duplicate create emitted an event")
		}
		pa := f.User("https://issuer.test", "pa")
		must(f.grant(f.admin, pa, projectA, identity.RoleAdmin))
		if _, err := f.Service.CreateProject(ctx, identity.CreateProjectCommand{Caller: f.caller(pa), Tenant: tenantA, Key: "x"}); !is(err, identity.ErrDenied) {
			t.Fatalf("project admin created a project: %v", err)
		}
		if _, err := f.Service.CreateProject(ctx, identity.CreateProjectCommand{Caller: f.caller(f.outsider), Tenant: tenantA, Key: "y"}); !is(err, identity.ErrHidden) {
			t.Fatalf("outsider: %v", err)
		}
		// The new project is readable through the tenant admin and listed only for those who reach it.
		view := must(f.Service.GetProject(ctx, f.caller(f.admin), p.Project.Scope))
		if view.Project.Key != "new-app" || len(view.CallerRoles) != 1 {
			t.Fatalf("%+v", view)
		}
		if _, err := f.Service.GetProject(ctx, f.caller(pa), p.Project.Scope); !is(err, identity.ErrHidden) {
			t.Fatalf("project admin reads another project: %v", err)
		}
		list := must(f.Service.ListProjects(ctx, f.caller(pa), tenantA, "", 100))
		if len(list) != 1 || list[0].Project.Scope.ProjectID != projectA {
			t.Fatalf("project admin lists %+v", list)
		}
		all := must(f.Service.ListProjects(ctx, f.caller(f.admin), tenantA, "", 100))
		if len(all) != 3 {
			t.Fatalf("tenant admin lists %d", len(all))
		}
		if _, err := f.Service.ListProjects(ctx, f.caller(f.outsider), tenantA, "", 100); !is(err, identity.ErrHidden) {
			t.Fatalf("outsider list: %v", err)
		}
		if _, err := f.Service.GetProject(ctx, f.caller(f.admin), domain.Scope{TenantID: tenantA, ProjectID: projectB}); !is(err, identity.ErrNotFound) {
			t.Fatalf("foreign project id: %v", err)
		}
	})
}
