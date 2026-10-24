package dependencieshttp_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
	"github.com/optimiware/kodama/internal/work/dependencies/depstest"
)

// Test_foreign_dependency: cross-tenant and same-tenant cross-project endpoints
// are indistinguishable from missing references for add and delete, lists and
// copied cursors disclose nothing foreign, and the composite foreign keys
// enforce ownership independently of the application. Real PostgreSQL; no NATS,
// S3 or Git. WM-01, WM-A01, IT-02, IT-A02.
func Test_foreign_dependency(t *testing.T) {
	st := depstest.NewStack(t)
	a := newAPI(t, st)

	dep := st.Create(scopeA, work.KindStory, "dependent")
	p1, p2, p3 := st.Create(scopeA, work.KindStory, "p1"), st.Create(scopeA, work.KindStory, "p2"), st.Create(scopeA, work.KindStory, "p3")
	st.MustLink(dep, p1)
	st.MustLink(dep, p2)
	st.MustLink(dep, p3)
	otherProject := st.Create(scopeA2, work.KindStory, "other project")
	foreignTenant := st.Create(scopeB, work.KindStory, "other tenant")
	foreignDep := st.Create(scopeB, work.KindStory, "other tenant dependent")
	st.MustLink(foreignDep, st.Create(scopeB, work.KindStory, "other tenant prerequisite"))
	dep, foreignDep = st.Get(dep), st.Get(foreignDep)

	before := snapshotOf(st, dep)
	links := st.Links(tenantA) + st.Links(tenantB)

	// refusal reduces a response to what a client can observe, minus the request id.
	type refusal struct {
		Code, Message string
		Details       string
	}
	observe := func(r resp) refusal {
		t.Helper()
		if r.Code != http.StatusNotFound {
			t.Fatalf("status %d, want 404: %s", r.Code, r.Body.String())
		}
		e := r.errBody(t)
		d, _ := jsonString(e.Details)
		return refusal{e.Code, e.Message, d}
	}

	t.Run("add_cannot_tell_foreign_from_missing", func(t *testing.T) {
		attempt := func(depID, preID string, scope string) refusal {
			project := projectA
			tenant := tenantA
			if scope == "b" {
				tenant, project = tenantB, projectB
			}
			return observe(a.do("tok-engineer", http.MethodPost, depPath(tenant, project, depID), addBody(preID), "If-Match", etag(int64(dep.Version))))
		}
		missing := attempt(string(dep.ID), "wi_does_not_exist", "a")
		for name, got := range map[string]refusal{
			"cross-tenant prerequisite":  attempt(string(dep.ID), string(foreignTenant.ID), "a"),
			"cross-project prerequisite": attempt(string(dep.ID), string(otherProject.ID), "a"),
		} {
			if got != missing {
				t.Errorf("%s: %+v differs from a missing reference %+v", name, got, missing)
			}
		}
		// The dependent itself: foreign and missing look the same, too.
		missingDep := attempt("wi_does_not_exist", string(p1.ID), "a")
		for name, got := range map[string]refusal{
			"cross-project dependent": observe(a.do("tok-engineer", http.MethodPost, depPath(tenantA, projectA, string(otherProject.ID)), addBody(string(p1.ID)), "If-Match", `"1"`)),
			"cross-tenant dependent":  observe(a.do("tok-engineer", http.MethodPost, depPath(tenantA, projectA, string(foreignDep.ID)), addBody(string(p1.ID)), "If-Match", `"1"`)),
		} {
			if got != missingDep {
				t.Errorf("%s: %+v differs from a missing dependent %+v", name, got, missingDep)
			}
		}
		// A foreign prerequisite is a 404 even when a graph diagnostic would apply:
		// no cycle or self-link answer exists for an unresolved endpoint.
		if got := attempt(string(dep.ID), string(foreignTenant.ID), "a"); got != missing {
			t.Errorf("graph diagnostics leaked for a foreign endpoint: %+v", got)
		}
	})

	t.Run("delete_cannot_tell_foreign_from_missing", func(t *testing.T) {
		del := func(depID, preID string) refusal {
			return observe(a.do("tok-engineer", http.MethodDelete, memberPath(tenantA, projectA, depID, preID), "", "If-Match", etag(int64(dep.Version))))
		}
		missing := del(string(dep.ID), "wi_does_not_exist")
		for name, got := range map[string]refusal{
			"cross-tenant prerequisite":  del(string(dep.ID), string(foreignTenant.ID)),
			"cross-project prerequisite": del(string(dep.ID), string(otherProject.ID)),
		} {
			if got != missing {
				t.Errorf("%s: %+v differs from a missing reference %+v", name, got, missing)
			}
		}
		missingDep := del("wi_does_not_exist", string(p1.ID))
		for name, got := range map[string]refusal{
			"cross-project dependent": del(string(otherProject.ID), string(p1.ID)),
			"cross-tenant dependent":  del(string(foreignDep.ID), string(p1.ID)),
		} {
			if got != missingDep {
				t.Errorf("%s: %+v differs from a missing dependent %+v", name, got, missingDep)
			}
		}
	})

	t.Run("a_principal_of_another_tenant_learns_nothing", func(t *testing.T) {
		// u_only_a exists in tenant A only: in tenant B the authority hides it, and
		// the answer is the generic 404 whatever the path names.
		for _, path := range []string{depPath(tenantB, projectB, string(foreignDep.ID)), depPath(tenantB, projectB, string(dep.ID)), depPath(tenantB, projectB, "wi_does_not_exist")} {
			if got := observe(a.do("tok-only-a", http.MethodGet, path, "")); got != observe(a.do("tok-only-a", http.MethodGet, depPath(tenantB, projectB, "wi_does_not_exist"), "")) {
				t.Errorf("%s: %+v", path, got)
			}
		}
		if r := a.do("tok-only-a", http.MethodPost, depPath(tenantB, projectB, string(foreignDep.ID)), addBody("wi_x"), "If-Match", `"1"`); r.Code != http.StatusNotFound {
			t.Errorf("mutation by an unknown principal: %d", r.Code)
		}
	})

	t.Run("lists_disclose_nothing_foreign", func(t *testing.T) {
		for name, path := range map[string]string{
			"another project's item through this project":                    depPath(tenantA, projectA, string(otherProject.ID)),
			"another tenant's item through this tenant":                      depPath(tenantA, projectA, string(foreignDep.ID)),
			"another tenant's item through its tenant but the wrong project": depPath(tenantB, projectA, string(foreignDep.ID)),
			"a missing item": depPath(tenantA, projectA, "wi_does_not_exist"),
		} {
			if r := a.do("tok-engineer", http.MethodGet, path, ""); r.Code != http.StatusNotFound {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		// The legitimate list holds this dependent's own prerequisites and no others.
		r := a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, string(dep.ID)), "")
		if r.Code != http.StatusOK {
			t.Fatalf("%d %s", r.Code, r.Body.String())
		}
		var page struct {
			Items []struct{ DependentId, DependencyId string }
		}
		r.decode(t, &page)
		if len(page.Items) != 3 {
			t.Fatalf("items = %+v", page.Items)
		}
		for _, it := range page.Items {
			if it.DependentId != string(dep.ID) || (it.DependencyId != string(p1.ID) && it.DependencyId != string(p2.ID) && it.DependencyId != string(p3.ID)) {
				t.Errorf("foreign row in the list: %+v", it)
			}
		}
	})

	t.Run("copied_cursors_disclose_nothing", func(t *testing.T) {
		first := a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, string(dep.ID))+"?limit=1", "")
		var pg struct{ NextCursor *string }
		first.decode(t, &pg)
		if first.Code != http.StatusOK || pg.NextCursor == nil {
			t.Fatalf("first page: %d %s", first.Code, first.Body.String())
		}
		cur := url.QueryEscape(*pg.NextCursor)
		// Its own page continues.
		if r := a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, string(dep.ID))+"?limit=1&cursor="+cur, ""); r.Code != http.StatusOK {
			t.Fatalf("own continuation: %d %s", r.Code, r.Body.String())
		}
		// Copied to another dependent of the same project, another project, another tenant or
		// another endpoint shape, it is rejected as an invalid cursor before any data is read.
		other := st.Create(scopeA, work.KindStory, "same project, another dependent")
		for name, path := range map[string]string{
			"another dependent":   depPath(tenantA, projectA, string(other.ID)),
			"another project":     depPath(tenantA, projectA2, string(otherProject.ID)),
			"another tenant":      depPath(tenantB, projectB, string(foreignDep.ID)),
			"a nonexistent item":  depPath(tenantA, projectA, "wi_does_not_exist"),
			"a foreign tenant id": depPath(tenantB, projectB, string(dep.ID)),
		} {
			r := a.do("tok-engineer", http.MethodGet, path+"?limit=1&cursor="+cur, "")
			if r.Code != http.StatusBadRequest || r.errCode(t) != string(httpapi.CodeInvalidCursor) {
				t.Errorf("cursor copied to %s: %d %s", name, r.Code, r.Body.String())
			}
		}
		// Another principal may use the cursor (it grants no authority) but is authorized again.
		if r := a.do("tok-outsider", http.MethodGet, depPath(tenantA, projectA, string(dep.ID))+"?limit=1&cursor="+cur, ""); r.Code != http.StatusForbidden {
			t.Errorf("cursor with an unauthorized principal: %d %s", r.Code, r.Body.String())
		}
		// A tampered cursor is rejected.
		if r := a.do("tok-engineer", http.MethodGet, depPath(tenantA, projectA, string(dep.ID))+"?limit=1&cursor="+cur+"x", ""); r.Code != http.StatusBadRequest {
			t.Errorf("tampered cursor: %d", r.Code)
		}
	})

	t.Run("composite_foreign_keys_enforce_ownership_below_the_application", func(t *testing.T) {
		ctx := context.Background()
		for name, pre := range map[string]work.Item{"another project": otherProject, "another tenant": foreignTenant} {
			_, err := st.Owner.Exec(ctx, `INSERT INTO work_item_dependencies (tenant_id, project_id, dependent_id, prerequisite_id, history_id, created_by_type, created_by_id, created_at)
				VALUES ($1, $2, $3, $4, 'wih_fk', 'user', 'u', now())`, tenantA, projectA, string(dep.ID), string(pre.ID))
			if err == nil {
				t.Errorf("a link to a prerequisite in %s was stored", name)
			}
		}
	})

	t.Run("nothing_foreign_was_written", func(t *testing.T) {
		if after := snapshotOf(st, dep); after != before {
			t.Errorf("the dependent changed: %+v -> %+v", before, after)
		}
		if got := st.Links(tenantA) + st.Links(tenantB); got != links {
			t.Errorf("links %d -> %d", links, got)
		}
		for _, it := range []work.Item{otherProject, foreignTenant, foreignDep} {
			if got := st.Get(it); got.Version != it.Version {
				t.Errorf("%s changed", it.ID)
			}
		}
	})

}
