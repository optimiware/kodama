package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/internal/work"
)

const projectA2 = "p_billing_a"

func (e *env) seedProject(tenant, project string) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 5)`, tenant, project)
}

func (e *env) member(tenant, user string, project *string) {
	e.t.Helper()
	e.ownerExec(`INSERT INTO users (id, issuer, subject) VALUES ($1::text, 'https://issuer.example', $1::text) ON CONFLICT DO NOTHING`, user)
	e.ownerExec(`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ($1::text, $1::text || '_' || $2::text || '_' || coalesce($3::text, 'all'), $2::text, $3::text, 'engineer')`, tenant, user, project)
}

// Test_tenant_ownership: cross-tenant and cross-project parents, evidence and
// assignee references are rejected without disclosure; predicates, composite
// foreign keys, row level security and pooled connection context isolate
// tenants. Real PostgreSQL. WM-01, WM-A01.
func Test_tenant_ownership(t *testing.T) {
	e := newEnvWith(t, envOptions{conns: 1}) // one connection: pooled context reuse is certain
	e.seedProject(tenantA, projectA2)
	e.principal(tenantA, "u_other_project", work.ActorUser, engineerPerms...)
	ctx := context.Background()

	foreignTenantEpic := e.create(scopeB, work.KindEpic, "other tenant epic")
	scopeA2 := domain.Scope{TenantID: tenantA, ProjectID: projectA2}
	otherProjectEpic := e.create(scopeA2, work.KindEpic, "other project epic")
	epic := e.create(scopeA, work.KindEpic, "epic")
	story := e.create(scopeA, work.KindStory, "story")

	t.Run("parents", func(t *testing.T) {
		create := func(parent domain.WorkItemID) error {
			_, err := e.service.Create(ctx, work.CreateCommand{Scope: scopeA, Caller: e.caller(engineer), Kind: work.KindStory, Title: "child", ParentID: parent})
			return err
		}
		for name, parent := range map[string]work.Item{"another tenant": foreignTenantEpic, "another project": otherProjectEpic} {
			if err := create(parent.ID); !is(err, work.ErrNotFound) {
				t.Errorf("%s parent: %v", name, err)
			}
		}
		if err := create("wi_missing"); !is(err, work.ErrNotFound) {
			t.Errorf("missing parent: %v", err)
		}
		if err := create(story.ID); !is(err, work.ErrInvalid) {
			t.Errorf("a story cannot be a parent: %v", err)
		}
		if err := create(epic.ID); err != nil {
			t.Errorf("same-project epic: %v", err)
		}

		// Cycles and self-parenting are impossible.
		e2 := e.create(scopeA, work.KindEpic, "epic 2")
		e3 := e.create(scopeA, work.KindEpic, "epic 3")
		set := func(it work.Item, parent domain.WorkItemID) (work.Item, error) {
			return e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: it.ID, Expected: it.Version, Patch: work.Patch{Parent: work.Some(parent)}})
		}
		e2, err := set(e2, epic.ID)
		if err != nil {
			t.Fatal(err)
		}
		e3, err = set(e3, e2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := set(epic, e3.ID); !is(err, work.ErrInvalid) {
			t.Errorf("a cycle epic <- e2 <- e3 <- epic was accepted: %v", err)
		}
		if _, err := set(epic, epic.ID); !is(err, work.ErrInvalid) {
			t.Errorf("self parent: %v", err)
		}
		if _, err := set(e3, foreignTenantEpic.ID); !is(err, work.ErrNotFound) {
			t.Errorf("reparent to a foreign epic: %v", err)
		}
	})

	t.Run("evidence_links", func(t *testing.T) {
		e.artifact(scopeB, "art_foreign")
		e.artifact(scopeA2, "art_other_project")
		e.artifact(scopeA, "art_ok")
		link := func(id string) error {
			links := []work.EvidenceRef{{Type: work.EvidenceArtifact, ID: id}}
			_, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: story.ID, Expected: e.get(story).Version, Patch: work.Patch{EvidenceLinks: &links}})
			return err
		}
		for _, id := range []string{"art_foreign", "art_other_project", "art_missing"} {
			if err := link(id); !is(err, work.ErrNotFound) {
				t.Errorf("%s: %v", id, err)
			}
		}
		if err := link("art_ok"); err != nil {
			t.Errorf("own artifact: %v", err)
		}
		if got := e.get(story); len(got.EvidenceLinks) != 1 {
			t.Errorf("links = %+v", got.EvidenceLinks)
		}
	})

	t.Run("assignees_need_current_scoped_membership", func(t *testing.T) {
		pA2 := projectA2
		pA := projectA
		e.member(tenantA, "u_member_a", &pA)
		e.member(tenantA, "u_member_a2", &pA2)
		e.member(tenantA, "u_member_all", nil)
		e.member(tenantB, "u_member_b", nil)
		e.ownerExec(`INSERT INTO users (id, issuer, subject) VALUES ('u_global', 'https://issuer.example', 'global')`)
		assign := func(user string) error {
			_, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA, Caller: e.caller(engineer), ID: story.ID, Expected: e.get(story).Version, Patch: work.Patch{Assignee: work.Some(user)}})
			return err
		}
		for _, user := range []string{"u_member_a2", "u_member_b", "u_global", "u_nobody"} {
			if err := assign(user); !is(err, work.ErrNotFound) {
				t.Errorf("assignee %s: %v", user, err)
			}
		}
		for _, user := range []string{"u_member_a", "u_member_all"} {
			if err := assign(user); err != nil {
				t.Errorf("assignee %s: %v", user, err)
			}
		}
		// Creating with an ineligible assignee needs the technical permission and a membership.
		if _, err := e.service.Create(ctx, work.CreateCommand{Scope: scopeA, Caller: e.caller(analyst), Kind: work.KindTask, Title: "x", AssigneeID: "u_member_a"}); !is(err, work.ErrDenied) {
			t.Errorf("analyst assigning: %v", err)
		}
	})

	t.Run("resources_are_scoped_by_tenant_and_project", func(t *testing.T) {
		for name, scope := range map[string]domain.Scope{"another tenant": scopeB, "another project": scopeA2} {
			if _, err := e.service.Get(ctx, scope, e.caller(engineer), story.ID); !is(err, work.ErrNotFound) && !is(err, work.ErrHidden) {
				t.Errorf("get from %s: %v", name, err)
			}
		}
		// Same principal and same tenant, other project: the item is not visible there.
		if _, err := e.service.Get(ctx, scopeA2, e.caller(engineer), story.ID); !is(err, work.ErrNotFound) {
			t.Errorf("get through another project: %v", err)
		}
		if _, err := e.service.History(ctx, scopeA2, e.caller(engineer), story.ID, work.HistoryQuery{Limit: 5}); !is(err, work.ErrNotFound) {
			t.Errorf("history through another project: %v", err)
		}
		if _, err := e.service.Edit(ctx, work.EditCommand{Scope: scopeA2, Caller: e.caller(engineer), ID: story.ID, Expected: story.Version, Patch: work.Patch{Title: str("hijack")}}); !is(err, work.ErrNotFound) {
			t.Errorf("edit through another project: %v", err)
		}
		if _, err := e.service.Transition(ctx, work.TransitionCommand{Scope: scopeA2, Caller: e.caller(engineer), ID: story.ID, Expected: story.Version, Request: work.TransitionRequest{To: work.StateReady}}); !is(err, work.ErrNotFound) {
			t.Errorf("transition through another project: %v", err)
		}
		if _, err := e.service.Get(ctx, scopeA, e.caller("u_other_tenant"), story.ID); err == nil {
			t.Errorf("an unknown principal read a work item")
		}
		items, err := e.service.List(ctx, scopeB, e.caller(engineer), work.ListQuery{Limit: 50})
		if err != nil || len(items) != 1 || items[0].ID != foreignTenantEpic.ID {
			t.Errorf("tenant B list: %v %+v", err, items)
		}
		if e.get(story).Title != "story" {
			t.Error("a cross-scope edit changed the item")
		}
	})

	t.Run("row_level_security_and_composite_keys", func(t *testing.T) {
		// Without tenant context the pooled connection sees nothing, including right
		// after another tenant's transaction used the same single connection.
		if n := e.count(tenantA, `SELECT count(*) FROM work_items`); n == 0 {
			t.Fatal("tenant A sees nothing")
		}
		var n int
		if err := e.rt.QueryRow(ctx, `SELECT count(*) FROM work_items`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("outside a tenant transaction: %d rows, %v", n, err)
		}
		// Tenant B cannot see tenant A's rows even by primary key.
		if n := e.count(tenantB, `SELECT count(*) FROM work_items WHERE id = $1`, string(story.ID)); n != 0 {
			t.Fatalf("tenant B sees tenant A's item")
		}
		insert := func(tenant, project, parent string) error {
			return storage.WithTenant(ctx, e.rt, domain.TenantID(tenant), func(ctx context.Context, tx pgx.Tx) error {
				var p *string
				if parent != "" {
					p = &parent
				}
				_, err := tx.Exec(ctx, `INSERT INTO work_items (tenant_id, project_id, id, kind, title, parent_id, created_by_type, created_by_id, created_at, updated_at)
					VALUES ($1, $2, 'wi_raw', 'task', 't', $3, 'user', 'u', now(), now())`, tenant, project, p)
				return err
			})
		}
		if err := insert(tenantA, projectB, ""); err == nil {
			t.Error("a work item in tenant A pointing at tenant B's project was accepted")
		}
		if err := insert(tenantB, projectB, ""); err != nil {
			t.Errorf("valid raw insert: %v", err)
		}
		if err := insert(tenantA, projectA, string(foreignTenantEpic.ID)); err == nil {
			t.Error("a cross-tenant parent passed the composite foreign key")
		}
		if err := insert(tenantA, projectA, string(otherProjectEpic.ID)); err == nil {
			t.Error("a cross-project parent passed the composite foreign key")
		}
		// RLS: tenant A's context cannot write tenant B rows.
		err := storage.WithTenant(ctx, e.rt, tenantA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO work_items (tenant_id, project_id, id, kind, title, created_by_type, created_by_id, created_at, updated_at)
				VALUES ($1, $2, 'wi_leak', 'task', 't', 'user', 'u', now(), now())`, tenantB, projectB)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("cross-tenant write under RLS: %v", err)
		}
	})
}
