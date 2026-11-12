//go:build integration

package composition

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestMCPToolsListAdvertisesFieldLevelSchemas pins that every one of the 51
// tools/list entries advertises exactly api.OperationInputSchema, a strict
// field-level envelope schema with the declared title.
func TestMCPToolsListAdvertisesFieldLevelSchemas(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))

	response, err := environment.mcpRaw(api.MCPMethodToolsList, nil, "")
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("tools/list refused: %+v", response.Error)
	}
	var catalog api.MCPToolsListResult
	if err := json.Unmarshal(response.Result, &catalog); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	routes := api.Routes()
	if len(catalog.Tools) != len(routes) {
		t.Fatalf("tools/list returned %d tools; want %d", len(catalog.Tools), len(routes))
	}
	for index, route := range routes {
		if catalog.Tools[index].Name != route.Operation {
			t.Fatalf("tools/list[%d] = %q; want %q", index, catalog.Tools[index].Name, route.Operation)
		}
		wantRaw, err := api.OperationInputSchema(route.Operation)
		if err != nil {
			t.Fatalf("OperationInputSchema(%q): %v", route.Operation, err)
		}
		var want, got any
		if err := json.Unmarshal(wantRaw, &want); err != nil {
			t.Fatalf("decode declared schema: %v", err)
		}
		if err := json.Unmarshal(catalog.Tools[index].InputSchema, &got); err != nil {
			t.Fatalf("decode advertised schema: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s inputSchema = %s; want the field-level %s",
				route.Operation, string(catalog.Tools[index].InputSchema), string(wantRaw))
		}
	}
}

// TestPublicClientExpandsCanonicalRouteParameters pins that the public client
// reads route parameters from the canonical request fields the REST edge
// decodes (project -> project_id) and leaves them in the body, including a
// base64-encoded pipeline YAML document.
func TestPublicClientExpandsCanonicalRouteParameters(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	result, err := clientInvoke(t, environment, adminToken, "membership.list", api.MembershipListRequest{
		ProjectID: projectID,
	}, nil, "")
	if err != nil {
		t.Fatalf("public client membership.list: %v", err)
	}
	if result.Operation != "membership.list" {
		t.Fatalf("public client result operation = %q", result.Operation)
	}

	applied, err := clientInvoke(t, environment, projectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: projectID,
		Name:      "client-build",
		YAML:      []byte(smokePipeline),
	}, nil, "client-pipeline-apply")
	if err != nil {
		t.Fatalf("public client pipeline.apply: %v", err)
	}
	var pipeline api.PipelineApplyResult
	environment.decode(applied, &pipeline)
	if pipeline.PipelineID == "" || pipeline.Version == 0 {
		t.Fatalf("public client pipeline.apply = %+v", pipeline)
	}

	read, err := clientInvoke(t, environment, projectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  projectID,
		PipelineID: pipeline.PipelineID,
	}, nil, "")
	if err != nil {
		t.Fatalf("public client pipeline.read: %v", err)
	}
	var definition api.PipelineReadResult
	environment.decode(read, &definition)
	if string(definition.YAML) != smokePipeline {
		t.Fatalf("public client pipeline.read YAML = %q; want the applied document", string(definition.YAML))
	}
}

// clientInvoke performs one operation through the public client transport.
func clientInvoke(t *testing.T, environment *runningEnv, token, operation string, input any, expected *uint64, key string) (api.Result, error) {
	t.Helper()
	return environment.transport(token).Invoke(context.Background(), api.Invocation{
		Operation: operation,
		Request: api.Request{
			ExpectedVersion: expected,
			IdempotencyKey:  key,
			Input:           encodeInput(t, input),
		},
	})
}
