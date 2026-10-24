package kit

import (
	"encoding/json"
	"fmt"
	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/api/client"
)

// Criteria is the acceptance criteria every item of the scenarios carries: the
// QA report cites AC-1, the reviewer accepts them all.
var Criteria = []client.AcceptanceCriterion{
	{Id: "AC-1", Text: "A warehouse worker resolves an inventory gap with an audited, idempotent ERP write."},
	{Id: "AC-2", Text: "Totals follow the selected warehouse."},
}

// NewItem creates a story with the criteria as the engineer and provisions its
// workspace (the run admission needs the retained source).
func (e *Env) NewItem(title, spec string) client.WorkItem {
	t := e.T
	t.Helper()
	it := e.NewBareItem(title, spec)
	e.World.Provision(it.Id)
	return it
}

// NewBareItem creates the story without a workspace.
func (e *Env) NewBareItem(title, spec string) client.WorkItem {
	t := e.T
	t.Helper()
	body := map[string]any{"kind": "story", "title": title, "spec": spec, "acceptanceCriteria": Criteria}
	var it client.WorkItem
	if err := json.Unmarshal(e.MustDo("engineer", "POST", e.ProjectPath()+"/work-items", body, nil, http.StatusCreated), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

// Transition posts a transition as the persona at the item's version and
// reports the raw answer.
func (e *Env) Transition(persona string, it client.WorkItem, to string, extra map[string]any) (int, []byte) {
	body := map[string]any{"to": to}
	for k, v := range extra {
		body[k] = v
	}
	return e.Do(persona, "POST", e.ProjectPath()+"/work-items/"+it.Id+"/transitions", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, it.Version)})
}

// MustTransition is Transition requiring success and returning the new item.
func (e *Env) MustTransition(persona string, it client.WorkItem, to string, extra map[string]any) client.WorkItem {
	t := e.T
	t.Helper()
	code, b := e.Transition(persona, it, to, extra)
	if code != http.StatusOK {
		t.Fatalf("%s -> %s as %s: HTTP %d: %s", it.State, to, persona, code, b)
	}
	var out client.WorkItem
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Admit moves a backlog item to active (ready, then active) as the engineer.
func (e *Env) Admit(it client.WorkItem) client.WorkItem {
	e.T.Helper()
	it = e.MustTransition("engineer", it, "ready", nil)
	return e.MustTransition("engineer", it, "active", nil)
}

// History lists the item's history entries, oldest first.
func (e *Env) History(id string) []client.HistoryEntry {
	t := e.T
	t.Helper()
	var out []client.HistoryEntry
	cursor := ""
	for {
		path := e.ProjectPath() + "/work-items/" + id + "/history?limit=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var page client.HistoryPage
		if err := json.Unmarshal(e.MustDo("engineer", "GET", path, nil, nil, http.StatusOK), &page); err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Items...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return out
		}
		cursor = *page.NextCursor
	}
}

// SetWIP sets the project's active work limit as the administrator.
func (e *Env) SetWIP(limit int) {
	t := e.T
	t.Helper()
	var p client.Project
	if err := json.Unmarshal(e.MustDo("admin", "GET", fmt.Sprintf("/api/v1/tenants/%s/projects/%s", tenantA(), e.World.Project), nil, nil, http.StatusOK), &p); err != nil {
		t.Fatal(err)
	}
	e.MustDo("admin", "PATCH", fmt.Sprintf("/api/v1/tenants/%s/projects/%s", tenantA(), e.World.Project), map[string]any{"activeWipLimit": limit},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, p.Version)}, http.StatusOK)
}

var _ testing.TB

// GatePipeline is a pipeline of two container steps around a human gate: a test
// step, the review gate (no evidence: an artifact produced without a workspace
// cannot be bound to a candidate), and a build step the gate protects. It needs
// no registry, Git checkout or cluster, only the Podman runtime.
func (e *Env) GatePipeline(name string) client.PipelineDefinition {
	return e.Register(name, map[string]any{},
		m2.ContainerStep("test", `printf '{"outcome":"completed","outputs":{"passed":"yes"}}' > "$KODAMA_RESULTS_FILE"`,
			map[string]any{"passed": map[string]any{"type": "string"}}),
		m2.GateStep("review", nil, "test"),
		m2.ContainerStep("build", `printf '{"outcome":"completed","outputs":{"built":"yes"}}' > "$KODAMA_RESULTS_FILE"`,
			map[string]any{"built": map[string]any{"type": "string"}}, "review"),
	)
}

// Depend adds the prerequisite link of the dependent as the persona at the
// dependent's version.
func (e *Env) Depend(persona string, dependent client.WorkItem, prerequisite string) (int, []byte) {
	return e.Do(persona, "POST", e.ProjectPath()+"/work-items/"+dependent.Id+"/dependencies", map[string]any{"dependencyId": prerequisite},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, dependent.Version)})
}

// Undepend removes the link.
func (e *Env) Undepend(persona string, dependent client.WorkItem, prerequisite string) (int, []byte) {
	return e.Do(persona, "DELETE", e.ProjectPath()+"/work-items/"+dependent.Id+"/dependencies/"+prerequisite, nil,
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, dependent.Version)})
}

// Block sets the item's blocked reason as the engineer ("" clears it).
func (e *Env) Block(it client.WorkItem, reason string) client.WorkItem {
	t := e.T
	t.Helper()
	var v any = reason
	if reason == "" {
		v = nil
	}
	var out client.WorkItem
	if err := json.Unmarshal(e.MustDo("engineer", "PATCH", e.ProjectPath()+"/work-items/"+it.Id, map[string]any{"blockedReason": v},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, it.Version)}, http.StatusOK), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Ready moves a backlog item to ready.
func (e *Env) Ready(it client.WorkItem) client.WorkItem {
	return e.MustTransition("engineer", it, "ready", nil)
}

// WorkEvents counts the committed work-item events of the item in the tenant's outbox.
func (e *Env) WorkEvents(eventType, item string) int {
	return e.TenantCount(m2.TenantA, `SELECT count(*) FROM outbox WHERE event_type = $1 AND position($2 in convert_from(payload, 'UTF8')) > 0`, eventType, item)
}

// NewProject creates another project of tenant A as the administrator and binds
// the engineer to it, returning its identifier.
func (e *Env) NewProject(key string) string {
	t := e.T
	t.Helper()
	var p client.Project
	if err := json.Unmarshal(e.MustDo("admin", "POST", "/api/v1/tenants/"+m2.TenantA+"/projects", map[string]any{"key": key, "settings": map[string]any{}}, nil, http.StatusCreated), &p); err != nil {
		t.Fatal(err)
	}
	e.MustDo("admin", "POST", "/api/v1/tenants/"+m2.TenantA+"/memberships", map[string]any{"userId": e.World.EngineerID, "role": "engineer", "projectId": p.Id}, nil, http.StatusCreated)
	return p.Id
}

// NewItemIn creates a story in the project as the engineer.
func (e *Env) NewItemIn(project, title string) client.WorkItem {
	t := e.T
	t.Helper()
	var it client.WorkItem
	body := map[string]any{"kind": "story", "title": title, "spec": "# " + title, "acceptanceCriteria": Criteria}
	if err := json.Unmarshal(e.MustDo("engineer", "POST", "/api/v1/tenants/"+m2.TenantA+"/projects/"+project+"/work-items", body, nil, http.StatusCreated), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

// ForeignItem creates a story in tenant B's project as its engineer.
func (e *Env) ForeignItem() client.WorkItem {
	t := e.T
	t.Helper()
	var it client.WorkItem
	body := map[string]any{"kind": "story", "title": "billing", "spec": "# billing", "acceptanceCriteria": Criteria}
	if err := json.Unmarshal(e.MustDo("analyst", "POST", "/api/v1/tenants/"+m2.TenantB+"/projects/"+e.World.ProjectB+"/work-items", body, nil, http.StatusCreated), &it); err != nil {
		t.Fatal(err)
	}
	return it
}
