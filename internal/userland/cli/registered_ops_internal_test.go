package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
)

func TestRenderWhoamiServerResultAndTaskListTable(t *testing.T) {
	whoamiBody := json.RawMessage(`{"principal_id":"oidc-00112233445566778899aabbccddeeff","kind":"human","tenant_id":"tenant-1","project_id":"proj-1","roles":["admin","engineer"]}`)
	var out bytes.Buffer
	if err := renderWhoamiServerResult(&out, OutputTable, api.Result{Operation: "principal.whoami", Body: whoamiBody}, whoamiView{Server: "https://kodema.example.com"}); err != nil {
		t.Fatalf("renderWhoamiServerResult(table): %v", err)
	}
	for _, want := range []string{
		"server\thttps://kodema.example.com",
		"principal_id\toidc-00112233445566778899aabbccddeeff",
		"kind\thuman",
		"scope\tproject (proj-1)",
		"tenant_id\ttenant-1",
		"project_id\tproj-1",
		"roles\tadmin,engineer",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("whoami table missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	tenantWhoamiBody := json.RawMessage(`{"principal_id":"oidc-00112233445566778899aabbccddeeff","kind":"human","tenant_id":"tenant-1","roles":["admin"]}`)
	if err := renderWhoamiServerResult(&out, OutputTable, api.Result{Operation: "principal.whoami", Body: tenantWhoamiBody}, whoamiView{}); err != nil {
		t.Fatalf("renderWhoamiServerResult(tenant table): %v", err)
	}
	for _, want := range []string{
		"scope\ttenant (tenant-1)",
		"tenant_id\ttenant-1",
		"roles\tadmin",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("tenant whoami table missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := renderWhoamiServerResult(&out, OutputJSON, api.Result{Operation: "principal.whoami", Body: whoamiBody}, whoamiView{}); err != nil {
		t.Fatalf("renderWhoamiServerResult(json): %v", err)
	}
	if strings.TrimSpace(out.String()) != string(whoamiBody) {
		t.Fatalf("whoami json = %q; want %q", out.String(), whoamiBody)
	}

	taskListBody := json.RawMessage(`{"tasks":[{"task_id":"task-10","title":"Ship kodema","state":"open","selected_pipeline":"pipe-1","current_execution":"exec-5","lifecycle_version":2}]}`)
	var tableOut, tableErr bytes.Buffer
	code := renderTaskListTable(Ports{Out: &tableOut, Err: &tableErr}, api.Result{Operation: "task.list", Body: taskListBody})
	if code != ExitSuccess {
		t.Fatalf("renderTaskListTable exit = %d; stderr=%q", code, tableErr.String())
	}
	for _, want := range []string{"TASK", "STATE", "PIPELINE", "EXECUTION", "TITLE", "task-10", "open", "pipe-1", "exec-5", "Ship kodema"} {
		if !strings.Contains(tableOut.String(), want) {
			t.Fatalf("task.list table missing %q:\n%s", want, tableOut.String())
		}
	}

	var callListOut, callListErr bytes.Buffer
	if code := runCallList(Ports{Out: &callListOut, Err: &callListErr}, OutputTable); code != ExitSuccess {
		t.Fatalf("runCallList exit = %d; stderr=%q", code, callListErr.String())
	}
	for _, want := range []string{"kv.put", "/api/v1/projects/{project}/kv", "kv.get", "/api/v1/projects/{project}/kv/{key}"} {
		if !strings.Contains(callListOut.String(), want) {
			t.Fatalf("call --list missing %q:\n%s", want, callListOut.String())
		}
	}
}
