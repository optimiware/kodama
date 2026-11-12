package app

// Trusted-file configuration tests for the optional Shell gateways.
//
// Both `kodama server` profiles read the model gateway and the external MCP
// tool proxy from the files named by KODAMA_MODELS_FILE and
// KODAMA_TOOL_PROXY_FILE. These tests pin the file contract: every configured
// member reaches its own value, an unreadable, malformed, unknown-member,
// trailing or semantically incomplete file is a named refusal with no
// configuration returned, and the argument validator is a closed, explicit
// vocabulary rather than a silently defaulted no-op.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const validModelsFile = `{
  "models": [
    {
      "tenant_id": "tenant-1",
      "project_id": "project-1",
      "model": "small-model",
      "upstream_model": "upstream-small",
      "endpoint": "https://models.example.test/v1/chat/completions",
      "secret_name": "MODEL_API_KEY",
      "max_request_bytes": 1048576,
      "max_response_bytes": 1048576,
      "max_completion_tokens": 1024,
      "timeout": "5s",
      "record_content": true,
      "content_retention": "240h"
    }
  ],
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["MODEL_API_KEY"]}
  ]
}`

// duplicateModelsFile declares the same configured model name twice in one
// project; the trusted configuration refuses it instead of last-entry-wins.
const duplicateModelsFile = `{
  "models": [
    {
      "tenant_id": "tenant-1",
      "project_id": "project-1",
      "model": "small-model",
      "upstream_model": "upstream-small",
      "endpoint": "https://models.example.test/v1/chat/completions",
      "secret_name": "MODEL_API_KEY",
      "max_request_bytes": 1048576,
      "max_response_bytes": 1048576,
      "max_completion_tokens": 1024,
      "timeout": "5s"
    },
    {
      "tenant_id": "tenant-1",
      "project_id": "project-1",
      "model": "small-model",
      "upstream_model": "upstream-other",
      "endpoint": "https://models.example.test/v1/chat/completions",
      "secret_name": "MODEL_API_KEY",
      "max_request_bytes": 1048576,
      "max_response_bytes": 1048576,
      "max_completion_tokens": 1024,
      "timeout": "5s"
    }
  ],
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["MODEL_API_KEY"]}
  ]
}`

func TestLoadModelsConfigFileMapsEveryMember(t *testing.T) {
	path := writeConfigFile(t, "models.json", validModelsFile)
	config, err := LoadModelsConfigFile(path, ProfileDevelopment)
	if err != nil {
		t.Fatalf("LoadModelsConfigFile: %v", err)
	}
	if config == nil {
		t.Fatal("a named and valid models file returned no configuration")
	}
	if len(config.Models) != 1 {
		t.Fatalf("models = %d; want 1", len(config.Models))
	}
	model := config.Models[0]
	if model.Scope.TenantID != "tenant-1" || model.Scope.ProjectID != "project-1" {
		t.Fatalf("scope = %+v; want tenant-1/project-1", model.Scope)
	}
	if model.Model != "small-model" || model.UpstreamModel != "upstream-small" {
		t.Fatalf("model/upstream = %q/%q", model.Model, model.UpstreamModel)
	}
	if model.Endpoint != "https://models.example.test/v1/chat/completions" {
		t.Fatalf("endpoint = %q", model.Endpoint)
	}
	if model.SecretName != "MODEL_API_KEY" {
		t.Fatalf("secret name = %q", model.SecretName)
	}
	if model.MaxRequestBytes != 1<<20 || model.MaxResponseBytes != 1<<20 || model.MaxCompletionTokens != 1024 {
		t.Fatalf("bounds = %d/%d/%d", model.MaxRequestBytes, model.MaxResponseBytes, model.MaxCompletionTokens)
	}
	if model.Timeout != 5*time.Second || !model.RecordContent || model.ContentRetention != 240*time.Hour {
		t.Fatalf("timeout/record/retention = %s/%v/%s", model.Timeout, model.RecordContent, model.ContentRetention)
	}
	if config.Transport != nil {
		t.Fatal("the file configuration adopted a transport; the composition owns that boundary")
	}
	if len(config.SecretRules) != 1 || config.SecretRules[0].Scope != model.Scope {
		t.Fatalf("secret rules = %+v; want the configured project scope", config.SecretRules)
	}
	if names := config.SecretRules[0].Names; len(names) != 1 || names[0] != "MODEL_API_KEY" {
		t.Fatalf("secret rule names = %v", names)
	}
}

func TestLoadModelsConfigFileAppliesTheProfileRule(t *testing.T) {
	path := writeConfigFile(t, "models.json", validModelsFile)
	if _, err := LoadModelsConfigFile(path, ProfileProduction); err != nil {
		t.Fatalf("production refused a verified https endpoint: %v", err)
	}
	httpEndpoint := strings.Replace(validModelsFile,
		"https://models.example.test/v1/chat/completions",
		"http://models.example.test/v1/chat/completions", 1)
	if _, err := LoadModelsConfigFile(writeConfigFile(t, "http.json", httpEndpoint), ProfileProduction); err == nil {
		t.Fatal("production accepted a plain-http provider endpoint")
	}
}

func TestLoadModelsConfigFileRefuses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		content  string
		contains string
	}{
		{"empty file", "", "empty"},
		{"malformed json", `{"models": [`, "malformed"},
		{"unknown member", `{"models": [], "extra": true}`, "malformed"},
		{"trailing data", validModelsFile + `{"models": []}`, "trailing"},
		{"no models", `{"models": [], "secret_rules": []}`, "no configured models"},
		{"bad timeout", strings.Replace(validModelsFile, `"5s"`, `"soon"`, 1), "timeout"},
		{"duplicate model", duplicateModelsFile, "declared twice"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfigFile(t, "models.json", testCase.content)
			config, err := LoadModelsConfigFile(path, ProfileDevelopment)
			if err == nil {
				t.Fatalf("the file loaded: %+v", config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ModelsFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ModelsFileSetting)
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("refusal = %q; want %q", err, testCase.contains)
			}
		})
	}

	t.Run("absent file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.json")
		config, err := LoadModelsConfigFile(path, ProfileDevelopment)
		if err == nil || config != nil {
			t.Fatalf("absent file loaded a configuration: %+v", config)
		}
		if !strings.Contains(err.Error(), ModelsFileSetting) {
			t.Fatalf("refusal = %q; want the setting named", err)
		}
	})
}

const validToolProxyFile = `{
  "timeout": "30s",
  "projects": [
    {
      "tenant_id": "tenant-1",
      "project_id": "project-1",
      "servers": [
        {
          "name": "search",
          "endpoint": "https://tools.example.test/mcp",
          "secrets": ["TOOL_TOKEN"],
          "credentials": [
            {"secret": "TOOL_TOKEN", "header": "Authorization", "scheme": "Bearer"}
          ],
          "tools": [
            {"alias": "search-docs", "name": "docs.search", "description": "search the docs", "validator": "json_object"}
          ]
        }
      ]
    }
  ],
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["TOOL_TOKEN"]}
  ]
}`

func TestLoadToolProxyConfigFileMapsEveryMember(t *testing.T) {
	path := writeConfigFile(t, "tools.json", validToolProxyFile)
	config, err := LoadToolProxyConfigFile(path)
	if err != nil {
		t.Fatalf("LoadToolProxyConfigFile: %v", err)
	}
	if config == nil {
		t.Fatal("a named and valid tool proxy file returned no configuration")
	}
	if config.Timeout != 30*time.Second {
		t.Fatalf("timeout = %s; want 30s", config.Timeout)
	}
	if len(config.Projects) != 1 {
		t.Fatalf("projects = %d; want 1", len(config.Projects))
	}
	project := config.Projects[0]
	if project.Scope.TenantID != "tenant-1" || project.Scope.ProjectID != "project-1" {
		t.Fatalf("scope = %+v", project.Scope)
	}
	if len(project.Servers) != 1 {
		t.Fatalf("servers = %d; want 1", len(project.Servers))
	}
	server := project.Servers[0]
	if server.Name != "search" || server.Endpoint != "https://tools.example.test/mcp" {
		t.Fatalf("server = %q/%q", server.Name, server.Endpoint)
	}
	if len(server.Secrets) != 1 || server.Secrets[0] != "TOOL_TOKEN" {
		t.Fatalf("server secrets = %v", server.Secrets)
	}
	if len(server.Credentials) != 1 || server.Credentials[0].Header != "Authorization" || server.Credentials[0].Scheme != "Bearer" {
		t.Fatalf("credentials = %+v", server.Credentials)
	}
	if len(server.Tools) != 1 {
		t.Fatalf("tools = %d; want 1", len(server.Tools))
	}
	tool := server.Tools[0]
	if tool.Alias != "search-docs" || tool.Name != "docs.search" || tool.Description != "search the docs" {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.Validate == nil {
		t.Fatal("the configured validator did not reach the tool definition")
	}
	if err := tool.Validate(json.RawMessage(`{"query":"needle"}`)); err != nil {
		t.Fatalf("json_object refused an object: %v", err)
	}
	if err := tool.Validate(json.RawMessage(`"needle"`)); err == nil {
		t.Fatal("json_object accepted a non-object argument")
	}
	if len(config.SecretRules) != 1 || len(config.SecretRules[0].Names) != 1 || config.SecretRules[0].Names[0] != "TOOL_TOKEN" {
		t.Fatalf("secret rules = %+v", config.SecretRules)
	}
}

func TestLoadModelsConfigFileRefusesDuplicateMembers(t *testing.T) {
	duplicateEndpoint := strings.Replace(validModelsFile,
		`"endpoint": "https://models.example.test/v1/chat/completions",`,
		`"endpoint": "https://models.example.test/v1/chat/completions", "endpoint": "https://other.example.test/v1/chat/completions",`,
		1)
	path := writeConfigFile(t, "models.json", duplicateEndpoint)
	config, err := LoadModelsConfigFile(path, ProfileDevelopment)
	if err == nil {
		t.Fatalf("a models file with a duplicate object member loaded: %+v", config)
	}
	if config != nil {
		t.Fatalf("a refused file returned a configuration: %+v", config)
	}
	if !strings.Contains(err.Error(), ModelsFileSetting) {
		t.Fatalf("refusal = %q; want the %s setting named", err, ModelsFileSetting)
	}
	if !strings.Contains(err.Error(), "duplicate object member") {
		t.Fatalf("refusal = %q; want the duplicate object member named", err)
	}
}

func TestLoadModelsConfigFileRefusesCaseVariantMembers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{"endpoint", strings.Replace(validModelsFile, `"endpoint"`, `"Endpoint"`, 1)},
		{"secret name", strings.Replace(validModelsFile, `"secret_name"`, `"SECRET_NAME"`, 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfigFile(t, "models.json", testCase.content)
			config, err := LoadModelsConfigFile(path, ProfileDevelopment)
			if err == nil {
				t.Fatalf("a case-variant member bound to the declared member: %+v", config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ModelsFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ModelsFileSetting)
			}
			if !strings.Contains(err.Error(), "does not exactly match") {
				t.Fatalf("refusal = %q; want the non-exact member named", err)
			}
		})
	}
}

func TestLoadModelsConfigFileRefusesEndpointQueryOrFragment(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		endpoint string
	}{
		{"query", "https://models.example.test/v1/chat/completions?key=secret"},
		{"fragment", "https://models.example.test/v1/chat/completions#key=secret"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			content := strings.Replace(validModelsFile,
				"https://models.example.test/v1/chat/completions", testCase.endpoint, 1)
			path := writeConfigFile(t, "models.json", content)
			config, err := LoadModelsConfigFile(path, ProfileDevelopment)
			if err == nil {
				t.Fatalf("a models file endpoint with a %s loaded: %+v", testCase.name, config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ModelsFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ModelsFileSetting)
			}
			if !strings.Contains(err.Error(), "query or fragment") {
				t.Fatalf("refusal = %q; want the endpoint query or fragment named", err)
			}
		})
	}
}

func TestLoadModelsConfigFileRefusesUnboundedInput(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		content := strings.Repeat(" ", maxTrustedConfigFileBytes+1)
		path := writeConfigFile(t, "models.json", content)
		config, err := LoadModelsConfigFile(path, ProfileDevelopment)
		if err == nil || config != nil {
			t.Fatalf("an oversized models file loaded: %+v", config)
		}
		if !strings.Contains(err.Error(), ModelsFileSetting) || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("refusal = %q; want the %s bound named", err, ModelsFileSetting)
		}
	})

	t.Run("not a regular file", func(t *testing.T) {
		path := t.TempDir()
		config, err := LoadModelsConfigFile(path, ProfileDevelopment)
		if err == nil || config != nil {
			t.Fatalf("a directory loaded as a models file: %+v", config)
		}
		if !strings.Contains(err.Error(), ModelsFileSetting) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("refusal = %q; want the %s regular-file requirement named", err, ModelsFileSetting)
		}
	})
}

// trustedDepthNode is a recursive document shape used only to exercise the
// decoder's explicit nesting bound.
type trustedDepthNode struct {
	Child *trustedDepthNode `json:"child"`
}

func TestValidateTrustedConfigDocumentRefusesDeepNesting(t *testing.T) {
	depth := maxTrustedConfigDepth + 1
	document := strings.Repeat(`{"child":`, depth) + "null" + strings.Repeat("}", depth)
	var target trustedDepthNode
	err := validateTrustedConfigDocument([]byte(document), &target)
	if err == nil {
		t.Fatal("a document nested past the depth bound was accepted")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Fatalf("refusal = %q; want the depth bound named", err)
	}
}

func TestLoadToolProxyConfigFileRefusesDuplicateMembers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{
			"duplicate server endpoint",
			strings.Replace(validToolProxyFile,
				`"endpoint": "https://tools.example.test/mcp",`,
				`"endpoint": "https://tools.example.test/mcp", "endpoint": "https://other.example.test/mcp",`,
				1),
		},
		{
			"duplicate top-level timeout",
			strings.Replace(validToolProxyFile, `"timeout": "30s",`, `"timeout": "30s", "timeout": "31s",`, 1),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfigFile(t, "tools.json", testCase.content)
			config, err := LoadToolProxyConfigFile(path)
			if err == nil {
				t.Fatalf("a tool proxy file with a duplicate object member loaded: %+v", config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ToolProxyFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ToolProxyFileSetting)
			}
			if !strings.Contains(err.Error(), "duplicate object member") {
				t.Fatalf("refusal = %q; want the duplicate object member named", err)
			}
		})
	}
}

func TestLoadToolProxyConfigFileRefusesCaseVariantMembers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{"endpoint", strings.Replace(validToolProxyFile, `"endpoint"`, `"Endpoint"`, 1)},
		{"secrets", strings.Replace(validToolProxyFile, `"secrets"`, `"SECRETS"`, 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfigFile(t, "tools.json", testCase.content)
			config, err := LoadToolProxyConfigFile(path)
			if err == nil {
				t.Fatalf("a case-variant member bound to the declared member: %+v", config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ToolProxyFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ToolProxyFileSetting)
			}
			if !strings.Contains(err.Error(), "does not exactly match") {
				t.Fatalf("refusal = %q; want the non-exact member named", err)
			}
		})
	}
}

func TestLoadToolProxyConfigFileRefuses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		content  string
		contains string
	}{
		{"empty file", "", "empty"},
		{"malformed json", `{"projects": [`, "malformed"},
		{"unknown member", `{"projects": [], "extra": true}`, "malformed"},
		{"no projects", `{"projects": [], "secret_rules": []}`, "no configured projects"},
		{"unknown validator", strings.Replace(validToolProxyFile, `"json_object"`, `"trust_me"`, 1), "validator"},
		{"missing validator", strings.Replace(validToolProxyFile, `"validator": "json_object"`, `"validator": ""`, 1), "argument validator"},
		{"dropped credential", strings.Replace(validToolProxyFile, `"secrets": ["TOOL_TOKEN"],`, `"secrets": [],`, 1), "credential mapping names an undeclared secret"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeConfigFile(t, "tools.json", testCase.content)
			config, err := LoadToolProxyConfigFile(path)
			if err == nil {
				t.Fatalf("the file loaded: %+v", config)
			}
			if config != nil {
				t.Fatalf("a refused file returned a configuration: %+v", config)
			}
			if !strings.Contains(err.Error(), ToolProxyFileSetting) {
				t.Fatalf("refusal = %q; want the %s setting named", err, ToolProxyFileSetting)
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("refusal = %q; want %q", err, testCase.contains)
			}
		})
	}

	t.Run("absent file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.json")
		config, err := LoadToolProxyConfigFile(path)
		if err == nil || config != nil {
			t.Fatalf("absent file loaded a configuration: %+v", config)
		}
		if !strings.Contains(err.Error(), ToolProxyFileSetting) {
			t.Fatalf("refusal = %q; want the setting named", err)
		}
	})
}
