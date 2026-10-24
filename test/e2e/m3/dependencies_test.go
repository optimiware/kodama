package m3_test

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api/client"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	kit "github.com/optimiware/kodama/test/e2e/m3/kit"
)

// TestM3Dependencies (m3_dependencies) is WM-A04 and the admission half of
// WM-A03 on the assembled core, over real PostgreSQL and the real routes:
// same-project business dependency links, transactional cycle rejection (also
// when two edits race), admission of a dependent that waits for its
// prerequisite, links changed before admission, permission-aware errors, and
// one active slot contended by concurrent admissions (blocked work still
// holds it). A refused admission writes nothing: no history, no event.
// A prerequisite becoming done (which releases the dependent) and the rework
// and reopen entries into active need a work item in review, which the
// assembly cannot reach (TestM3ReviewGuardNotWired).
func TestM3Dependencies(t *testing.T) {
	e := kit.New(t, kit.Options{NoQA: true, NoChatOps: true})
	e.SetWIP(1)
	mk := func(title string) client.WorkItem { return e.NewBareItem(title, "# "+title) }
	a, b, c := mk("A: resolve gaps"), mk("B: audit record"), mk("C: warehouse filter")

	// ---- links: same project, acyclic, permission-aware ----
	if code, body := e.Depend("engineer", a, a.Id); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "dependency_cycle" {
		t.Fatalf("a self link: %d %s", code, body)
	}
	if code, body := e.Depend("viewer", a, b.Id); code != http.StatusForbidden {
		t.Fatalf("a viewer added a link: %d %s", code, body)
	}
	if code, body := e.Depend("outsider", a, b.Id); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Fatalf("an unbound caller added a link: %d %s", code, body)
	}
	if code, body := e.Depend("engineer", a, "wi_does_not_exist"); code != http.StatusNotFound {
		t.Fatalf("an unknown prerequisite: %d %s", code, body)
	}
	// Foreign references are the same answer as an absent one: another project of the tenant, another tenant.
	other := e.NewProject("second")
	foreign := e.NewItemIn(other, "elsewhere")
	if code, body := e.Depend("engineer", a, foreign.Id); code != http.StatusNotFound {
		t.Fatalf("a prerequisite of another project: %d %s", code, body)
	}
	tb := e.ForeignItem()
	if code, body := e.Depend("engineer", a, tb.Id); code != http.StatusNotFound {
		t.Fatalf("a prerequisite of another tenant: %d %s", code, body)
	}
	if n := e.WorkEvents("work_item.updated", a.Id); n != 0 {
		t.Fatalf("%d update events after refused links", n)
	}

	// A depends on B: one version bump, one history entry, one event.
	ev0 := e.WorkEvents("work_item.updated", a.Id)
	code, body := e.Depend("engineer", a, b.Id)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("link A -> B: %d %s", code, body)
	}
	a = e.GetItem(a.Id)
	if a.Version != 2 || a.ContentRevision != 1 {
		t.Fatalf("the link left A at version %d, content revision %d", a.Version, a.ContentRevision)
	}
	if e.WorkEvents("work_item.updated", a.Id) != ev0+1 {
		t.Fatal("the link wrote no event")
	}
	if h := e.History(a.Id); len(h) != 2 || h[1].ChangedFields[0] != "dependencies" {
		t.Fatalf("history %+v", h)
	}
	// A repeat is a no-op.
	if code, _ := e.Depend("engineer", a, b.Id); code != http.StatusOK || e.GetItem(a.Id).Version != 2 {
		t.Fatal("a repeated link changed the item")
	}
	// Cycles: direct, transitive.
	if code, body := e.Depend("engineer", b, a.Id); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "dependency_cycle" {
		t.Fatalf("a direct cycle: %d %s", code, body)
	}
	if code, body := e.Depend("engineer", c, a.Id); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("link C -> A: %d %s", code, body)
	}
	if code, body := e.Depend("engineer", e.GetItem(b.Id), c.Id); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "dependency_cycle" {
		t.Fatalf("a transitive cycle B -> C -> A -> B: %d %s", code, body)
	}

	// ---- concurrent cycle insert: two edits that are each fine alone, a cycle together ----
	x, y := mk("X"), mk("Y")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, pair := range [][2]client.WorkItem{{x, y}, {y, x}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.Depend("engineer", pair[0], pair[1].Id)
		}()
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK || c == http.StatusCreated {
			ok++
		} else if c != http.StatusUnprocessableEntity {
			t.Fatalf("a racing link answered %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("racing links X <-> Y: %v, want exactly one accepted", codes)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM work_item_dependencies WHERE dependent_id IN ($1, $2)`, x.Id, y.Id); n != 1 {
		t.Fatalf("%d links between X and Y, a cycle would be 2", n)
	}

	// ---- admission: a dependent waits for its prerequisite ----
	a = e.Ready(e.GetItem(a.Id))
	trans0 := e.WorkEvents("work_item.transitioned", a.Id)
	if code, body := e.Transition("engineer", a, "active", nil); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "prerequisite_unmet" {
		t.Fatalf("admitting A while B is not done: %d %s", code, body)
	}
	if got := e.GetItem(a.Id); got.State != "ready" || got.Version != a.Version || e.WorkEvents("work_item.transitioned", a.Id) != trans0 || len(e.History(a.Id)) != 3 {
		t.Fatalf("a refused admission wrote something: %+v", got)
	}
	// The prerequisite itself is admitted (it has no prerequisites) and does not release the dependent.
	b = e.Admit(e.GetItem(b.Id))
	if code, body := e.Transition("engineer", a, "active", nil); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "prerequisite_unmet" {
		t.Fatalf("admitting A while B is only active: %d %s", code, body)
	}

	// ---- the link is removed before admission: the dependent is admitted (when the slot is free) ----
	// B holds the only slot: the dependency is no longer the reason, the WIP limit is.
	if code, body := e.Undepend("viewer", e.GetItem(a.Id), b.Id); code != http.StatusForbidden {
		t.Fatalf("a viewer removed a link: %d %s", code, body)
	}
	if code, body := e.Undepend("engineer", e.GetItem(a.Id), b.Id); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("remove A -> B: %d %s", code, body)
	}
	a = e.GetItem(a.Id)
	if code, body := e.Transition("engineer", a, "active", nil); code != http.StatusConflict || m2.ErrorCode(body) != "wip_limit_reached" {
		t.Fatalf("A with B holding the slot: %d %s", code, body)
	}

	// ---- one slot, a blocked item still holds it, concurrent admissions ----
	b = e.Block(e.GetItem(b.Id), "waiting for the ERP sandbox")
	if code, body := e.Transition("engineer", a, "active", nil); code != http.StatusConflict || m2.ErrorCode(body) != "wip_limit_reached" {
		t.Fatalf("a blocked item released its slot: %d %s", code, body)
	}
	b = e.MustTransition("engineer", e.Block(e.GetItem(b.Id), ""), "ready", map[string]any{"reason": "paused for the ERP sandbox"}) // leaves active, slot free
	c = e.GetItem(c.Id)
	// C -> A is still a link, so C waits for A; the racing pair is A and a fresh free item.
	d := e.Ready(mk("D"))
	var wins, refused int
	var mu sync.Mutex
	for _, it := range []client.WorkItem{a, d} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := e.Transition("engineer", it, "active", nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case code == http.StatusOK:
				wins++
			case code == http.StatusConflict && m2.ErrorCode(body) == "wip_limit_reached":
				refused++
			default:
				t.Errorf("a racing admission answered %d %s", code, body)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || refused != 1 {
		t.Fatalf("two admissions for one slot: %d admitted, %d refused", wins, refused)
	}
	if n := e.OwnerCount(`SELECT count(*) FROM work_items WHERE project_id = $1 AND state = 'active'`, e.World.Project); n != 1 {
		t.Fatalf("%d active items under a limit of 1", n)
	}
	// The dependent C still waits for A (A is not done) whatever the slots say.
	c = e.Ready(c)
	e.SetWIP(5)
	if code, body := e.Transition("engineer", c, "active", nil); code != http.StatusUnprocessableEntity || m2.ErrorCode(body) != "prerequisite_unmet" {
		t.Fatalf("C while A is not done: %d %s", code, body)
	}
	var page struct {
		Items []struct{ DependencyId string }
	}
	if err := json.Unmarshal(e.MustDo("viewer", "GET", e.ProjectPath()+"/work-items/"+c.Id+"/dependencies", nil, nil, http.StatusOK), &page); err != nil || len(page.Items) != 1 || page.Items[0].DependencyId != a.Id {
		t.Fatalf("C's prerequisites %+v %v", page, err)
	}
}
