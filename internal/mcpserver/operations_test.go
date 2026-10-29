package mcpserver

import (
	"testing"

	"github.com/optimiware/kodama/api"
)

func TestBuildOperationsCoversPublicAPI(t *testing.T) {
	ops, err := BuildOperations(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(ops), 74; got != want {
		t.Fatalf("operations = %d, want %d", got, want)
	}
	byID := map[string]Operation{}
	for _, op := range ops {
		if _, exists := byID[op.ID]; exists {
			t.Fatalf("duplicate operation %s", op.ID)
		}
		byID[op.ID] = op
		if op.Tool != toolName(op.ID) || op.InputSchema["type"] != "object" {
			t.Errorf("%s: tool/schema not built: %+v", op.ID, op)
		}
		if op.Toolset == "other" {
			t.Errorf("%s: no toolset", op.ID)
		}
	}
	for _, id := range []string{"getMe", "createRun", "decideGate", "observeProjectEvents", "getRunLogs", "createSecretVersion"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("missing operation %s", id)
		}
	}
	if op := byID["createRun"]; !op.BodyRequired {
		t.Error("createRun body should be required")
	}
	if op := byID["getMe"]; !op.ReadOnly || op.Method != "GET" {
		t.Errorf("getMe = %+v", op)
	}
	if op := byID["createSecretVersion"]; !op.Sensitive {
		t.Error("createSecretVersion should be sensitive")
	}
	if op := byID["observeRunEvents"]; !op.Streaming {
		t.Error("observeRunEvents should be marked streaming")
	}
}

func TestOperationSchemaShape(t *testing.T) {
	op := operation(t, "createRun")
	body, _ := op.InputSchema["properties"].(map[string]any)["body"].(map[string]any)
	props, _ := body["properties"].(map[string]any)
	for _, name := range []string{"sourceCommit", "workItemId", "definitionId"} {
		if _, ok := props[name]; !ok {
			t.Errorf("createRun body lacks %s; got %v", name, props)
		}
	}
	req, _ := op.InputSchema["required"].([]string)
	if len(req) == 0 {
		t.Fatal("createRun has no required arguments")
	}
	t.Logf("createRun required: %v", req)
	t.Logf("createRun properties: %v", op.InputSchema["properties"])
}

func TestSnakeArguments(t *testing.T) {
	for in, want := range map[string]string{
		"getMe": "get_me", "listWorkItemDependencies": "list_work_item_dependencies",
		"If-Match": "if_match", "Idempotency-Key": "idempotency_key", "Last-Event-ID": "last_event_id",
		"runID": "run_id", "McpServerId": "mcp_server_id",
	} {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
}
