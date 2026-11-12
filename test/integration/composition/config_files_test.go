//go:build integration

package composition

// Composition evidence: both optional Shell gateways are configured from
// the trusted files an operator names (KODAMA_MODELS_FILE and
// KODAMA_TOOL_PROXY_FILE) and served through the real router and public edges,
// and a named-but-malformed file is refused with no configuration returned
// instead of silently falling back to a disabled or default surface.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app"
)

// modelsConfigFile renders one valid trusted model-gateway file for the
// fixture provider. Every member is explicit, like the operator file the entry
// reads.
func modelsConfigFile(endpoint string) string {
	return fmt.Sprintf(`{
  "models": [
    {
      "tenant_id": %q,
      "project_id": %q,
      "model": %q,
      "upstream_model": %q,
      "endpoint": %q,
      "secret_name": %q,
      "max_request_bytes": 1048576,
      "max_response_bytes": 1048576,
      "max_completion_tokens": 1024,
      "timeout": "5s"
    }
  ],
  "secret_rules": [
    {"tenant_id": %q, "project_id": %q, "names": [%q]}
  ]
}`, tenantID, projectID, modelAlias, modelUpstream, endpoint,
		string(modelSecretName), tenantID, projectID, string(modelSecretName))
}

// toolProxyConfigFile renders one valid trusted tool-proxy file for the
// fixture upstream.
func toolProxyConfigFile(endpoint string) string {
	return fmt.Sprintf(`{
  "timeout": "30s",
  "projects": [
    {
      "tenant_id": %q,
      "project_id": %q,
      "servers": [
        {
          "name": "search",
          "endpoint": %q,
          "secrets": [%q],
          "credentials": [
            {"secret": %q, "header": "Authorization", "scheme": "Bearer"}
          ],
          "tools": [
            {"alias": %q, "name": %q, "description": "search the docs", "validator": "json_object"}
          ]
        }
      ]
    }
  ],
  "secret_rules": [
    {"tenant_id": %q, "project_id": %q, "names": [%q]}
  ]
}`, tenantID, projectID, endpoint, toolSecretName, toolSecretName,
		toolAlias, toolUpstreamName, tenantID, projectID, toolSecretName)
}

func writeGatewayFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestConfiguredGatewayFiles proves the two entry files carry the trusted
// gateway configuration all the way to the served composition: the configured
// model and tool proxy answer through the real router with their configured
// upstream mapping and Shell-resolved credentials, and a malformed file is
// refused before any composition exists.
//
// Mutation reasoning: dropping either file mapping leaves that operation
// not_found; forwarding the public alias instead of the configured upstream
// name changes the provider/tool request; ignoring the file's secret rule
// drops the Shell-resolved credential; ignoring a decode error would start a
// composition from a malformed file.
func TestConfiguredGatewayFiles(t *testing.T) {
	t.Run("serves the configured model and tool proxy", func(t *testing.T) {
		modelProvider := newModelEdgeUpstream(t)
		toolUpstream := newToolCallUpstream(t)

		modelsPath := writeGatewayFile(t, "models.json",
			modelsConfigFile(modelProvider.url()+"/v1/chat/completions"))
		toolsPath := writeGatewayFile(t, "tools.json",
			toolProxyConfigFile(toolUpstream.server.URL))

		models, err := app.LoadModelsConfigFile(modelsPath, app.ProfileDevelopment)
		if err != nil {
			t.Fatalf("LoadModelsConfigFile: %v", err)
		}
		// A development composition adopts the fixture provider transport; the
		// file itself never supplies a transport.
		models.Transport = modelProvider.transport()
		toolProxy, err := app.LoadToolProxyConfigFile(toolsPath)
		if err != nil {
			t.Fatalf("LoadToolProxyConfigFile: %v", err)
		}

		harness := newHarness(t)
		harness.config.Models = models
		harness.config.ToolProxy = toolProxy
		environment := harness.start(newFakeRuntime(modeComplete))
		environment.grantEngineer("admin-1", projectID)
		for _, secret := range []struct {
			name  id.SecretName
			value string
			key   string
		}{
			{modelSecretName, modelSecretValue, "gateway-file-model-secret"},
			{toolSecretName, secretValue, "gateway-file-tool-secret"},
		} {
			environment.mustInvoke(projectToken, "secret.write", api.SecretWriteRequest{
				ProjectID: projectID,
				SecretID:  string(secret.name),
				Value:     secret.value,
			}, nil, secret.key)
		}

		modelResult, modelError := environment.call(projectToken, "model.invoke", modelEdgeRequest(modelAlias), nil, "")
		if modelError.Code != "" {
			t.Fatalf("model.invoke from the trusted file refused: %s %s", modelError.Code, modelError.Message)
		}
		var completion api.ChatCompletionResponse
		environment.decode(modelResult, &completion)
		if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "edge completion" {
			t.Fatalf("completion = %+v; want the configured provider's completion", completion)
		}
		modelCalls := modelProvider.received()
		if len(modelCalls) != 1 {
			t.Fatalf("provider calls = %d; want 1", len(modelCalls))
		}
		if modelCalls[0].Model != modelUpstream {
			t.Fatalf("provider model = %q; want the file's configured upstream %q", modelCalls[0].Model, modelUpstream)
		}
		if modelCalls[0].Authorization != "Bearer "+modelSecretValue {
			t.Fatalf("provider authorization = %q; want the file's declared model secret", modelCalls[0].Authorization)
		}

		toolResult, toolError := environment.call(projectToken, "tool.invoke", api.ToolInvokeRequest{
			ProjectID: projectID,
			Tool:      toolAlias,
			Arguments: json.RawMessage(toolCallArgument),
		}, nil, "")
		if toolError.Code != "" {
			t.Fatalf("tool.invoke from the trusted file refused: %s %s", toolError.Code, toolError.Message)
		}
		var invoked api.ToolInvokeResult
		environment.decode(toolResult, &invoked)
		if invoked.IsError || invoked.Content == "" {
			t.Fatalf("tool result = %+v; want the configured upstream's result", invoked)
		}
		toolCalls := toolUpstream.received()
		if len(toolCalls) != 1 {
			t.Fatalf("tool upstream calls = %d; want 1", len(toolCalls))
		}
		if toolCalls[0].Name != toolUpstreamName {
			t.Fatalf("tool upstream name = %q; want the file's configured upstream %q", toolCalls[0].Name, toolUpstreamName)
		}
		if got := toolCalls[0].Header.Get("Authorization"); got != "Bearer "+secretValue {
			t.Fatalf("tool upstream authorization = %q; want the file's declared credential mapping", got)
		}
	})

	t.Run("refuses a malformed file", func(t *testing.T) {
		malformed := writeGatewayFile(t, "malformed.json", `{"models": [`)
		if models, err := app.LoadModelsConfigFile(malformed, app.ProfileDevelopment); err == nil || models != nil {
			t.Fatalf("a malformed models file loaded: %+v", models)
		} else if !strings.Contains(err.Error(), app.ModelsFileSetting) {
			t.Fatalf("models refusal = %q; want %s named", err, app.ModelsFileSetting)
		}
		if toolProxy, err := app.LoadToolProxyConfigFile(malformed); err == nil || toolProxy != nil {
			t.Fatalf("a malformed tool proxy file loaded: %+v", toolProxy)
		} else if !strings.Contains(err.Error(), app.ToolProxyFileSetting) {
			t.Fatalf("tool proxy refusal = %q; want %s named", err, app.ToolProxyFileSetting)
		}

		absent := filepath.Join(t.TempDir(), "absent.json")
		if models, err := app.LoadModelsConfigFile(absent, app.ProfileDevelopment); err == nil || models != nil {
			t.Fatalf("an absent models file loaded: %+v", models)
		}
	})
}
