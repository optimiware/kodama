package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
)

func TestRenderWhoamiServerResultAndTaskListTable(t *testing.T) {
	whoamiBody := json.RawMessage(`{"principal_id":"oidc-00112233445566778899aabbccddeeff","kind":"human","tenant_id":"tenant-1","project_id":"proj-1","roles":["admin","engineer"]}`)
	var out bytes.Buffer
	if err := renderWhoamiServerResult(&out, OutputTable, api.Result{Operation: "principal.whoami", Body: whoamiBody}, whoamiView{}); err != nil {
		t.Fatalf("renderWhoamiServerResult(table): %v", err)
	}
	for _, want := range []string{
		"principal_id\toidc-00112233445566778899aabbccddeeff",
		"kind\thuman",
		"tenant_id\ttenant-1",
		"project_id\tproj-1",
		"roles\tadmin,engineer",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("whoami table missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := renderWhoamiServerResult(&out, OutputJSON, api.Result{Operation: "principal.whoami", Body: whoamiBody}, whoamiView{}); err != nil {
		t.Fatalf("renderWhoamiServerResult(json): %v", err)
	}
	if strings.TrimSpace(out.String()) != string(whoamiBody) {
		t.Fatalf("whoami json = %q; want %q", out.String(), whoamiBody)
	}

	taskListBody := json.RawMessage(`{"tasks":[{"task_id":"task-10","title":"Ship kodactl","state":"open","selected_pipeline":"pipe-1","current_execution":"exec-5","lifecycle_version":2}]}`)
	var tableOut, tableErr bytes.Buffer
	code := renderTaskListTable(Ports{Out: &tableOut, Err: &tableErr}, api.Result{Operation: "task.list", Body: taskListBody})
	if code != ExitSuccess {
		t.Fatalf("renderTaskListTable exit = %d; stderr=%q", code, tableErr.String())
	}
	for _, want := range []string{"TASK", "STATE", "PIPELINE", "EXECUTION", "TITLE", "task-10", "open", "pipe-1", "exec-5", "Ship kodactl"} {
		if !strings.Contains(tableOut.String(), want) {
			t.Fatalf("task.list table missing %q:\n%s", want, tableOut.String())
		}
	}
}
