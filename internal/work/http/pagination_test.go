package workhttp_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/optimiware/kodama/internal/work"
	workhttp "github.com/optimiware/kodama/internal/work/http"
	"github.com/optimiware/kodama/internal/work/worktest"
)

type page struct {
	Items []struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Kind  string `json:"kind"`
	} `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func (p page) ids() []string {
	out := make([]string, 0, len(p.Items))
	for _, i := range p.Items {
		out = append(out, i.ID)
	}
	return out
}

// Test_cursor_scope: copied cursors fail across item, project, tenant, endpoint
// and filter scopes; ordering has a unique tie-breaker; limits are validated; a
// revoked reader cannot continue pagination. Real PostgreSQL-backed HTTP.
// WM-10, WM-A01, AC-04.
func Test_cursor_scope(t *testing.T) {
	s := worktest.NewStack(t)
	s.SeedProject(tenantA, projectA, 10)
	s.SeedProject(tenantA, "p_other", 10)
	s.SeedProject(tenantB, projectB, 10)
	perms := []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec}
	for _, tenant := range []string{tenantA, tenantB} {
		s.Principal(tenant, "u_reader", work.ActorUser, perms...)
	}
	a := newAPI(t, s.Service, tokens{"tok-reader": "u_reader"})
	list := func(tenant, project, query string) resp {
		return a.do("tok-reader", http.MethodGet, base(tenant, project)+query, "")
	}
	create := func(tenant, project, kind, title string) string {
		r := a.do("tok-reader", http.MethodPost, base(tenant, project), fmt.Sprintf(`{"kind":%q,"title":%q}`, kind, title))
		if r.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", r.Code, r.Body.String())
		}
		var it struct{ ID string }
		r.decode(t, &it)
		return it.ID
	}
	var created []string
	for i := 0; i < 5; i++ {
		kind := "story"
		if i%2 == 1 {
			kind = "task"
		}
		created = append(created, create(tenantA, projectA, kind, fmt.Sprintf("item %d", i)))
	}
	create(tenantA, "p_other", "story", "elsewhere")
	create(tenantB, projectB, "story", "foreign")
	// Identical creation instants force the identifier tie-breaker to decide.
	s.OwnerExec(`UPDATE work_items SET created_at = '2026-10-19T10:00:00Z' WHERE project_id = $1`, projectA)

	t.Run("pages_cover_every_item_once_in_a_total_order", func(t *testing.T) {
		seen := map[string]bool{}
		var order []string
		cursor := ""
		for i := 0; i < 10; i++ {
			q := "?limit=2"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			r := list(tenantA, projectA, q)
			if r.Code != http.StatusOK {
				t.Fatalf("page %d: %d %s", i, r.Code, r.Body.String())
			}
			var p page
			r.decode(t, &p)
			for _, id := range p.ids() {
				if seen[id] {
					t.Fatalf("%s returned twice", id)
				}
				seen[id] = true
				order = append(order, id)
			}
			if p.NextCursor == nil {
				break
			}
			cursor = *p.NextCursor
		}
		if len(seen) != 5 {
			t.Fatalf("saw %d of 5 items", len(seen))
		}
		for i := 1; i < len(order); i++ {
			if order[i-1] >= order[i] { // identical timestamps: identifier ascending
				t.Fatalf("not in (created_at, id) order: %v", order)
			}
		}
		_ = created
	})

	first := func(tenant, project, query string) string {
		r := list(tenant, project, query)
		var p page
		r.decode(t, &p)
		if p.NextCursor == nil {
			t.Fatalf("no cursor for %s%s: %s", project, query, r.Body.String())
		}
		return *p.NextCursor
	}

	t.Run("copied_cursors_fail", func(t *testing.T) {
		cursor := first(tenantA, projectA, "?limit=2")
		other := first(tenantA, projectA, "?limit=2&kind=story") // distinct filter set
		use := func(path, query string) resp { return a.do("tok-reader", http.MethodGet, path+query, "") }
		for name, r := range map[string]resp{
			"another project":      use(base(tenantA, "p_other"), "?limit=2&cursor="+url.QueryEscape(cursor)),
			"another tenant":       use(base(tenantB, projectB), "?limit=2&cursor="+url.QueryEscape(cursor)),
			"added filter":         use(base(tenantA, projectA), "?limit=2&kind=story&cursor="+url.QueryEscape(cursor)),
			"removed filter":       use(base(tenantA, projectA), "?limit=2&cursor="+url.QueryEscape(other)),
			"changed filter value": use(base(tenantA, projectA), "?limit=2&kind=task&cursor="+url.QueryEscape(other)),
			"history endpoint":     use(base(tenantA, projectA)+"/"+created[0]+"/history", "?limit=2&cursor="+url.QueryEscape(cursor)),
		} {
			if r.Code != http.StatusBadRequest || r.errCode(t) != "invalid_cursor" {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		// The same filter set in another order is the same scope.
		same := first(tenantA, projectA, "?limit=1&kind=story&kind=task")
		if r := list(tenantA, projectA, "?limit=1&kind=task&kind=story&cursor="+url.QueryEscape(same)); r.Code != http.StatusOK {
			t.Errorf("normalized filters: %d %s", r.Code, r.Body.String())
		}
		for name, tok := range map[string]string{
			"tampered": cursor[:len(cursor)-2] + "AA", "truncated": cursor[:10], "no signature": "AAAA", "garbage": "!!!.!!!",
		} {
			if r := list(tenantA, projectA, "?cursor="+url.QueryEscape(tok)); r.Code != http.StatusBadRequest || r.errCode(t) != "invalid_cursor" {
				t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
			}
		}
		if r := list(tenantA, projectA, "?cursor=a&cursor=b"); r.Code != http.StatusBadRequest {
			t.Errorf("repeated cursor: %d", r.Code)
		}
	})

	t.Run("history_cursors_are_bound_to_one_item", func(t *testing.T) {
		item := created[0]
		// Give the item some history first.
		for i := 0; i < 3; i++ {
			r := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+item, "")
			var it struct{ Version int64 }
			r.decode(t, &it)
			p := a.do("tok-reader", http.MethodPatch, base(tenantA, projectA)+"/"+item, fmt.Sprintf(`{"title":"edit %d"}`, i), "If-Match", fmt.Sprintf(`"%d"`, it.Version))
			if p.Code != http.StatusOK {
				t.Fatalf("patch: %d %s", p.Code, p.Body.String())
			}
		}
		r := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+item+"/history?limit=2", "")
		var h struct {
			Items      []struct{ Version int64 } `json:"items"`
			NextCursor *string                   `json:"nextCursor"`
		}
		r.decode(t, &h)
		if len(h.Items) != 2 || h.NextCursor == nil || h.Items[0].Version != 1 || h.Items[1].Version != 2 {
			t.Fatalf("first history page: %s", r.Body.String())
		}
		r = a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+item+"/history?limit=2&cursor="+url.QueryEscape(*h.NextCursor), "")
		var h2 struct {
			Items      []struct{ Version int64 } `json:"items"`
			NextCursor *string                   `json:"nextCursor"`
		}
		r.decode(t, &h2)
		if len(h2.Items) != 2 || h2.Items[0].Version != 3 || h2.NextCursor != nil {
			t.Fatalf("second history page: %s", r.Body.String())
		}
		// The same cursor on another item of the same project is refused.
		if r := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+created[1]+"/history?limit=2&cursor="+url.QueryEscape(*h.NextCursor), ""); r.Code != http.StatusBadRequest || r.errCode(t) != "invalid_cursor" {
			t.Errorf("history cursor on another item: %d %s", r.Code, r.Body.String())
		}
	})

	t.Run("limits_and_filters_are_validated", func(t *testing.T) {
		for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc", "?limit=1&limit=2", "?limit=-1", "?bogus=1", "?state=archived", "?kind=feature", "?blocked=maybe", "?parentId=bad%20id"} {
			if r := list(tenantA, projectA, q); r.Code != http.StatusBadRequest {
				t.Errorf("%s: %d %s", q, r.Code, r.Body.String())
			}
		}
		if r := list(tenantA, projectA, "?limit=200"); r.Code != http.StatusOK {
			t.Errorf("limit 200: %d", r.Code)
		}
		r := list(tenantA, projectA, "?kind=task&state=backlog")
		var p page
		r.decode(t, &p)
		if len(p.Items) != 2 {
			t.Errorf("filtered list: %s", r.Body.String())
		}
		for _, it := range p.Items {
			if it.Kind != "task" || it.State != "backlog" {
				t.Errorf("filter leak: %+v", it)
			}
		}
	})

	t.Run("a_revoked_reader_cannot_continue", func(t *testing.T) {
		cursor := first(tenantA, projectA, "?limit=2")
		hcursor := ""
		{
			r := a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+created[0]+"/history?limit=1", "")
			var h struct {
				NextCursor *string `json:"nextCursor"`
			}
			r.decode(t, &h)
			if h.NextCursor == nil {
				t.Fatal("no history cursor")
			}
			hcursor = *h.NextCursor
		}
		s.Revoke(tenantA, "u_reader", work.PermRead)
		for name, r := range map[string]resp{
			"list":    list(tenantA, projectA, "?limit=2&cursor="+url.QueryEscape(cursor)),
			"history": a.do("tok-reader", http.MethodGet, base(tenantA, projectA)+"/"+created[0]+"/history?limit=1&cursor="+url.QueryEscape(hcursor), ""),
			"first":   list(tenantA, projectA, "?limit=2"),
		} {
			if r.Code != http.StatusForbidden {
				t.Errorf("%s after revocation: %d %s", name, r.Code, r.Body.String())
			}
		}
	})
	_ = workhttp.ActionList
}
