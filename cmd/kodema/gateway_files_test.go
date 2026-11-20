package main

// Both `kodema serve` profiles read the optional model
// gateway and external MCP tool proxy from the trusted files named by
// KODEMA_MODELS_FILE and KODEMA_TOOL_PROXY_FILE. A named-but-unreadable or
// malformed file is a configuration refusal that names the setting; an unnamed
// surface stays disabled and never receives an implicit configuration.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	shellapp "github.com/optimiware/kodema/internal/shell/app"
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
	t.Setenv("KODEMA_MODELS_FILE", modelsPath)
	t.Setenv("KODEMA_TOOL_PROXY_FILE", toolsPath)

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
	t.Setenv("KODEMA_MODELS_FILE", "")
	t.Setenv("KODEMA_TOOL_PROXY_FILE", "")
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

func TestSecretRulesFileAcrossProfiles(t *testing.T) {
	exactPath := writeGatewaySettingsFile(t, "secret-rules-exact.json", `{
  "secret_rules": [
    {"tenant_id": "tenant-1", "project_id": "project-1", "names": ["API_KEY"]}
  ]
}`)
	allowAllPath := writeGatewaySettingsFile(t, "secret-rules-allow-all.json", `{
  "secret_rules": [
    {"tenant_id": "tenant-local", "project_id": "project-local", "allow_all": true}
  ]
}`)

	t.Run("development reads KODEMA_SECRET_RULES_FILE", func(t *testing.T) {
		t.Setenv(shellapp.SecretRulesFileSetting, allowAllPath)
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
		if len(config.SecretRules) != 1 || !config.SecretRules[0].AllowAll {
			t.Fatalf("SecretRules = %+v; want allow_all rule", config.SecretRules)
		}
	})

	t.Run("test profile stays deny-by-default when unset and loads KODEMA_SECRET_RULES_FILE", func(t *testing.T) {
		fixture := newTestProfileFixture(t)
		build, err := buildTestProfile(fixture.options, fixture.getenv)
		if err != nil {
			t.Fatalf("buildTestProfile default: %v", err)
		}
		if len(build.Config.SecretRules) != 0 {
			t.Fatalf("default test profile SecretRules = %+v; want empty (deny-by-default)", build.Config.SecretRules)
		}

		fixture.values[shellapp.SecretRulesFileSetting] = allowAllPath
		build, err = buildTestProfile(fixture.options, fixture.getenv)
		if err != nil {
			t.Fatalf("buildTestProfile with allow_all: %v", err)
		}
		if len(build.Config.SecretRules) != 1 || !build.Config.SecretRules[0].AllowAll ||
			build.Config.SecretRules[0].Scope.TenantID != "tenant-local" ||
			build.Config.SecretRules[0].Scope.ProjectID != "project-local" {
			t.Fatalf("test profile SecretRules = %+v", build.Config.SecretRules)
		}
	})

	t.Run("production stays deny-by-default, accepts exact rules, refuses allow_all", func(t *testing.T) {
		getenv, kube, options := productionFixture(t)
		build, err := buildProduction(options, getenv, kube)
		if err != nil {
			t.Fatalf("buildProduction default: %v", err)
		}
		if len(build.Config.SecretRules) != 0 {
			t.Fatalf("default production SecretRules = %+v; want empty (deny-by-default)", build.Config.SecretRules)
		}

		options.SecretRulesFile = exactPath
		build, err = buildProduction(options, getenv, kube)
		if err != nil {
			t.Fatalf("buildProduction exact secret rules: %v", err)
		}
		if len(build.Config.SecretRules) != 1 || len(build.Config.SecretRules[0].Names) != 1 || build.Config.SecretRules[0].Names[0] != "API_KEY" {
			t.Fatalf("production exact SecretRules = %+v", build.Config.SecretRules)
		}

		options.SecretRulesFile = allowAllPath
		if _, err := buildProduction(options, getenv, kube); err == nil || !strings.Contains(err.Error(), shellapp.SecretRulesFileSetting) {
			t.Fatalf("production accepted wildcard allow_all secret rules: %v", err)
		}
	})

	t.Run("flag and JSON config file wire secret_rules_file", func(t *testing.T) {
		t.Setenv(shellapp.SecretRulesFileSetting, "")
		if err := os.Unsetenv(shellapp.SecretRulesFileSetting); err != nil {
			t.Fatal(err)
		}
		configJSON := writeGatewaySettingsFile(t, "server.json", `{
  "profile": "development",
  "secret_rules_file": "`+filepath.ToSlash(exactPath)+`"
}`)
		var stderr lockedBuffer
		options, code := parseServerOptions([]string{
			"--config", configJSON,
			"--data-dir", t.TempDir(),
			"--dev-token", "dev-token",
		}, &stderr)
		if code != 0 {
			t.Fatalf("parseServerOptions(--config) exited %d: %s", code, stderr.String())
		}
		if options.SecretRulesFile != filepath.ToSlash(exactPath) {
			t.Fatalf("SecretRulesFile from --config = %q; want %q", options.SecretRulesFile, filepath.ToSlash(exactPath))
		}
		// CLI flag overrides config file.
		options, code = parseServerOptions([]string{
			"--config", configJSON,
			"--secret-rules-file", allowAllPath,
			"--data-dir", t.TempDir(),
			"--dev-token", "dev-token",
		}, &stderr)
		if code != 0 {
			t.Fatalf("parseServerOptions(--secret-rules-file) exited %d: %s", code, stderr.String())
		}
		if options.SecretRulesFile != allowAllPath {
			t.Fatalf("SecretRulesFile from flag = %q; want %q", options.SecretRulesFile, allowAllPath)
		}
	})
}

func TestServerModelsFileAndLLMProviderFlags(t *testing.T) {
	modelsPath := writeGatewaySettingsFile(t, "models.json", entryModelsFile)
	t.Setenv("KODEMA_MODELS_FILE", "")
	if err := os.Unsetenv("KODEMA_MODELS_FILE"); err != nil {
		t.Fatal(err)
	}

	var stderr lockedBuffer
	options, code := parseServerOptions([]string{
		"--profile", "development",
		"--data-dir", t.TempDir(),
		"--dev-token", "dev-token",
		"--models-file", modelsPath,
		"--llm-provider", "opencode=https://opencode.ai/inference/openai/v1",
		"--llm-provider", "custom=https://llm.example.test/v1,secret=CUSTOM_KEY",
	}, &stderr)
	if code != 0 {
		t.Fatalf("parseServerOptions exited %d: %s", code, stderr.String())
	}
	cfg, err := options.config()
	if err != nil {
		t.Fatalf("options.config: %v", err)
	}
	if cfg.Models == nil || len(cfg.Models.Models) != 1 || len(cfg.Models.Providers) != 2 {
		t.Fatalf("cfg.Models = %+v; want 1 model and 2 providers", cfg.Models)
	}
	if cfg.Models.Providers[0].Name != "opencode" || cfg.Models.Providers[0].SecretName != "OPENCODE_API_KEY" || cfg.Models.Providers[0].Scope.TenantID != "tenant-1" {
		t.Fatalf("provider 0 = %+v", cfg.Models.Providers[0])
	}
	if cfg.Models.Providers[1].Name != "custom" || cfg.Models.Providers[1].SecretName != "CUSTOM_KEY" {
		t.Fatalf("provider 1 = %+v", cfg.Models.Providers[1])
	}

	// Production requires explicit tenant=<id> and verified https URLs.
	getenv, kube, prodOpts := productionFixture(t)
	prodOpts.LLMProviders = []string{"openrouter=https://openrouter.ai/api/v1"}
	if _, err := buildProduction(prodOpts, getenv, kube); err == nil || !strings.Contains(err.Error(), "tenant=") {
		t.Fatalf("production accepted --llm-provider without tenant: %v", err)
	}
	prodOpts.LLMProviders = []string{"openrouter=http://openrouter.ai/api/v1,tenant=tenant-1"}
	if _, err := buildProduction(prodOpts, getenv, kube); err == nil {
		t.Fatal("production accepted plaintext http --llm-provider")
	}
	prodOpts.LLMProviders = []string{"openrouter=https://openrouter.ai/api/v1,tenant=tenant-1"}
	built, err := buildProduction(prodOpts, getenv, kube)
	if err != nil {
		t.Fatalf("production valid --llm-provider failed: %v", err)
	}
	if built.Config.Models == nil || len(built.Config.Models.Providers) != 1 || built.Config.Models.Providers[0].Name != "openrouter" {
		t.Fatalf("production models = %+v", built.Config.Models)
	}
}
