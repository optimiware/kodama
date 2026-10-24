package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The statements of the M2 joins are optional files of the secrets directory;
// absent they leave the capability disabled, present they are strict.
func TestLoadCoreConfigM2Statements(t *testing.T) {
	dir := t.TempDir()
	put := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{
		"database-runtime-url": "postgres://kodama_runtime:pw@db:5432/kodama", "s3-access-key": "access", "s3-secret-key": "secret", "nats-token": "tok",
		"cursor-key": strings.Repeat("c", 32), "idempotency-hash-key": strings.Repeat("h", 32), "master-key": strings.Repeat("m", 32),
	} {
		put(name, value)
	}
	env := map[string]string{
		"KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": "nats://nats:4222", "KODAMA_S3_ENDPOINT": "http://s3:4566", "KODAMA_S3_BUCKET": "bucket",
		"KODAMA_OIDC_ISSUER": "https://issuer.example", "KODAMA_OIDC_AUDIENCE": "https://api.example", "KODAMA_INSTALLATION_ID": "inst",
		"KODAMA_SECRETS_ADAPTER": "keyfile",
		"KODAMA_ENV":             "test", "KODAMA_RUNTIME_URL": "http://127.0.0.1:8080", "KODAMA_RUNTIME_STATE_DIR": "/tmp/rt",
		"KODAMA_ENTRYPOINT_BINARY": "/usr/bin/kodama", "KODAMA_RUNTIME_KEY_GENERATION": "1",
	}
	getenv := func(k string) string { return env[k] }
	process, err := LoadConfig(func(k string) string {
		if k == "KODAMA_ASSEMBLY" {
			return "core"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadCoreConfig(getenv, process)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActionCeiling.Configured() || cfg.Registry.Configured() || cfg.GitTriggers.Configured() {
		t.Fatalf("an installation without statements configured %+v %+v %+v", cfg.ActionCeiling, cfg.Registry, cfg.GitTriggers)
	}

	put("action-ceiling.json", ceilingStatement)
	put("registry-auth", "Bearer t101-registry")
	put("registry.json", `{"endpoints": [{"host": "registry.example.test", "authorizationFile": "registry-auth", "tenants": ["t_acme"], "repositories": ["team"]}]}`)
	put("git-triggers.json", `{"rateLimitPerMinute": 90}`)
	cfg, err = LoadCoreConfig(getenv, process)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ActionCeiling.Configured() || !cfg.Registry.Configured() || cfg.Registry.Endpoints[0].Authorization != "Bearer t101-registry" || cfg.GitTriggers.RateLimitPerMinute != 90 {
		t.Fatalf("loaded %+v %+v %+v", cfg.ActionCeiling, cfg.Registry, cfg.GitTriggers)
	}

	// The file locations are overridable, and a malformed statement stops startup.
	other := filepath.Join(dir, "elsewhere.json")
	for variable, tc := range map[string]struct {
		file, content string
	}{
		"KODAMA_ACTION_CEILING_FILE": {"action-ceiling.json", `{"kinds": {"agent": {"grants": ["secret.resolve"]}}}`},
		"KODAMA_REGISTRY_FILE":       {"registry.json", `{"endpoints": [{"host": "registry.example.test", "tenants": ["*"]}]}`},
		"KODAMA_GIT_TRIGGERS_FILE":   {"git-triggers.json", `{"rateLimitPerMinute": 0}`},
	} {
		if err := os.WriteFile(other, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		env[variable] = other
		if _, err := LoadCoreConfig(getenv, process); err == nil {
			t.Errorf("%s: a malformed statement was accepted", variable)
		}
		delete(env, variable)
	}
	put("git-triggers.json", `{"rateLimitPerMinute": 90, "extra": true}`)
	if _, err := LoadCoreConfig(getenv, process); err == nil {
		t.Error("an unknown member of git-triggers.json was accepted")
	}
	put("git-triggers.json", `{"rateLimitPerMinute": 90}`)

	// Production cannot honour a per-kind ceiling yet and says so.
	c := validConfig()
	c.ActionCeiling = ceilingConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("the Podman composition takes a ceiling: %v", err)
	}
	c.Registry = RegistryConfig{Endpoints: []RegistryEndpoint{{Host: "registry.example.test", Tenants: []string{"*"}, Repositories: []string{"team"}}}}
	c.GitTriggers = GitTriggerConfig{RateLimitPerMinute: 60}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid statements: %v", err)
	}
	c.GitTriggers.RateLimitPerMinute = -3
	c.Registry.Endpoints[0].Tenants = nil
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "tenants") {
		t.Fatalf("invalid statements: %v", err)
	}
}
