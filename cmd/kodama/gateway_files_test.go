package main

// Both `kodama server` profiles read the optional model
// gateway and external MCP tool proxy from the trusted files named by
// KODAMA_MODELS_FILE and KODAMA_TOOL_PROXY_FILE. A named-but-unreadable or
// malformed file is a configuration refusal that names the setting; an unnamed
// surface stays disabled and never receives an implicit configuration.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	shellapp "github.com/optimiware/kodama/internal/shell/app"
)

func writeGatewaySettingsFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

const entryModelsFile = `{
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
    }
  ],
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["MODEL_API_KEY"]}
  ]
}`

const entryToolProxyFile = `{
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
            {"alias": "search-docs", "name": "docs.search", "validator": "json_object"}
          ]
        }
      ]
    }
  ],
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["TOOL_TOKEN"]}
  ]
}`

// TestServerGatewayFilesFromEnvironment proves the development entry reads both
// settings from their own environment variable and attaches the loaded
// configuration to the composition input.
func TestServerGatewayFilesFromEnvironment(t *testing.T) {
	modelsPath := writeGatewaySettingsFile(t, "models.json", entryModelsFile)
	toolsPath := writeGatewaySettingsFile(t, "tools.json", entryToolProxyFile)
	t.Setenv("KODAMA_MODELS_FILE", modelsPath)
	t.Setenv("KODAMA_TOOL_PROXY_FILE", toolsPath)

	var stderr lockedBuffer
	options, code := parseServerOptions([]string{
		"--profile", "development",
		"--data-dir", t.TempDir(),
		"--dev-token", "dev-token",
	}, &stderr)
	if code != 0 {
		t.Fatalf("parseServerOptions exited %d: %s", code, stderr.String())
	}
	if options.ModelsFile != modelsPath || options.ToolProxyFile != toolsPath {
		t.Fatalf("files = %q/%q; want each environment setting in its own field", options.ModelsFile, options.ToolProxyFile)
	}
	config, err := options.config()
	if err != nil {
		t.Fatalf("development config: %v", err)
	}
	if config.Models == nil || len(config.Models.Models) != 1 || config.Models.Models[0].Model != "small-model" {
		t.Fatalf("models = %+v; want the file's configured model", config.Models)
	}
	if config.ToolProxy == nil || len(config.ToolProxy.Projects) != 1 {
		t.Fatalf("tool proxy = %+v; want the file's configured project", config.ToolProxy)
	}
}

// TestServerWithoutGatewayFilesLeavesThemDisabled proves an unnamed optional
// surface stays disabled: no file, no implicit default configuration.
func TestServerWithoutGatewayFilesLeavesThemDisabled(t *testing.T) {
	t.Setenv("KODAMA_MODELS_FILE", "")
	t.Setenv("KODAMA_TOOL_PROXY_FILE", "")
	var stderr lockedBuffer
	options, code := parseServerOptions([]string{
		"--profile", "development",
		"--data-dir", t.TempDir(),
		"--dev-token", "dev-token",
	}, &stderr)
	if code != 0 {
		t.Fatalf("parseServerOptions exited %d: %s", code, stderr.String())
	}
	config, err := options.config()
	if err != nil {
		t.Fatalf("development config: %v", err)
	}
	if config.Models != nil || config.ToolProxy != nil {
		t.Fatalf("unnamed gateway surfaces = %+v/%+v; want disabled", config.Models, config.ToolProxy)
	}
}

// TestServerGatewayFileRefusals proves a named-but-absent or malformed file is
// an exit-2 configuration refusal whose text names the setting, never a
// silently disabled or defaulted surface.
func TestServerGatewayFileRefusals(t *testing.T) {
	malformed := writeGatewaySettingsFile(t, "malformed.json", `{"models": [`)
	absent := filepath.Join(t.TempDir(), "absent.json")

	for _, testCase := range []struct {
		name     string
		models   string
		tools    string
		contains string
	}{
		{"malformed models", malformed, "", shellapp.ModelsFileSetting},
		{"absent models", absent, "", shellapp.ModelsFileSetting},
		{"malformed tool proxy", "", malformed, shellapp.ToolProxyFileSetting},
		{"absent tool proxy", "", absent, shellapp.ToolProxyFileSetting},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := defaultServerOptions()
			options.DataDir = t.TempDir()
			options.DevTokens = []string{"dev-token"}
			options.ModelsFile = testCase.models
			options.ToolProxyFile = testCase.tools
			config, err := options.config()
			if err == nil {
				t.Fatalf("the configuration loaded: %+v", config)
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Fatalf("refusal = %q; want %s named", err, testCase.contains)
			}
		})
	}
}

// TestBuildProductionReadsGatewayFiles proves the production entry attaches the
// trusted files and applies the production rule (a verified https provider).
func TestBuildProductionReadsGatewayFiles(t *testing.T) {
	modelsPath := writeGatewaySettingsFile(t, "models.json", entryModelsFile)
	toolsPath := writeGatewaySettingsFile(t, "tools.json", entryToolProxyFile)
	getenv, kube, options := productionFixture(t)
	options.ModelsFile = modelsPath
	options.ToolProxyFile = toolsPath
	build, err := buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if build.Config.Models == nil || build.Config.Models.Models[0].Endpoint != "https://models.example.test/v1/chat/completions" {
		t.Fatalf("production models = %+v", build.Config.Models)
	}
	if build.Config.ToolProxy == nil || build.Config.ToolProxy.Projects[0].Servers[0].Endpoint != "https://tools.example.test/mcp" {
		t.Fatalf("production tool proxy = %+v", build.Config.ToolProxy)
	}

	httpModels := writeGatewaySettingsFile(t, "http-models.json",
		strings.Replace(entryModelsFile, "https://models.example.test", "http://models.example.test", 1))
	options.ModelsFile = httpModels
	if _, err := buildProduction(options, getenv, kube); err == nil || !strings.Contains(err.Error(), shellapp.ModelsFileSetting) {
		t.Fatalf("production accepted a plain-http provider file: %v", err)
	}
}

// TestBuildTestProfileReadsGatewayFiles proves the durable test entry attaches
// the trusted files and refuses a malformed one by name.
func TestBuildTestProfileReadsGatewayFiles(t *testing.T) {
	modelsPath := writeGatewaySettingsFile(t, "models.json", entryModelsFile)
	toolsPath := writeGatewaySettingsFile(t, "tools.json", entryToolProxyFile)
	fixture := newTestProfileFixture(t)
	fixture.options.ModelsFile = modelsPath
	fixture.options.ToolProxyFile = toolsPath
	build, err := buildTestProfile(fixture.options, fixture.getenv)
	if err != nil {
		t.Fatalf("buildTestProfile: %v", err)
	}
	if build.Config.Models == nil || build.Config.ToolProxy == nil {
		t.Fatalf("test profile gateways = %+v/%+v; want both configured", build.Config.Models, build.Config.ToolProxy)
	}

	malformed := writeGatewaySettingsFile(t, "malformed.json", `{"projects": [`)
	fixture.options.ToolProxyFile = malformed
	if _, err := buildTestProfile(fixture.options, fixture.getenv); err == nil || !strings.Contains(err.Error(), shellapp.ToolProxyFileSetting) {
		t.Fatalf("test profile accepted a malformed tool proxy file: %v", err)
	}
}
