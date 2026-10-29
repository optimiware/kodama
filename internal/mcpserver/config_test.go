package mcpserver

import (
	"io"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

func TestParseServeConfig(t *testing.T) {
	env := map[string]string{
		"KODAMA_MCP_API_URL":  "https://kodama.example/api/v1",
		"KODAMA_MCP_RESOURCE": "https://mcp.example/mcp",
		"KODAMA_MCP_ISSUER":   "https://id.example",
		"KODAMA_MCP_AUDIENCE": "https://api.example",
	}
	cfg, err := ParseServeConfig([]string{"--read-only", "--toolsets", "work,runs", "--http-timeout", "5s"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReadOnly || len(cfg.Toolsets) != 2 || cfg.Toolsets[0] != "work" || cfg.Toolsets[1] != "runs" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Audience != "https://api.example" || cfg.HTTPTimeout != 5*time.Second {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestParseServeConfigAudienceDefaultsToResource(t *testing.T) {
	cfg, err := ParseServeConfig([]string{
		"--api-url", "https://kodama.example/api/v1",
		"--resource", "https://mcp.example/mcp/",
		"--issuer", "https://id.example",
	}, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resource != "https://mcp.example/mcp" || cfg.Audience != cfg.Resource {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestParseServeConfigRejectsIncompleteAndInsecure(t *testing.T) {
	for name, args := range map[string][]string{
		"missing api":      {"--resource", "https://mcp.example/mcp", "--issuer", "https://id.example"},
		"missing resource": {"--api-url", "https://kodama.example/api/v1", "--issuer", "https://id.example"},
		"missing issuer":   {"--api-url", "https://kodama.example/api/v1", "--resource", "https://mcp.example/mcp"},
		"insecure api":     {"--api-url", "http://kodama.example/api/v1", "--resource", "https://mcp.example/mcp", "--issuer", "https://id.example"},
		"default project":  {"--api-url", "https://kodama.example/api/v1", "--resource", "https://mcp.example/mcp", "--issuer", "https://id.example", "--default-project", "p"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseServeConfig(args, func(string) string { return "" }, io.Discard); err == nil {
				t.Fatal("config accepted")
			}
		})
	}
}

func TestFilterOperations(t *testing.T) {
	all, err := BuildOperations(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := filterOperations(all, Config{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range readOnly {
		if !op.ReadOnly {
			t.Errorf("read-only mode exposed %s", op.ID)
		}
	}
	work, err := filterOperations(all, Config{Toolsets: []string{"work"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) == 0 || len(work) >= len(all) {
		t.Fatalf("work toolset = %d, all = %d", len(work), len(all))
	}
	for _, op := range work {
		if op.Toolset != "work" {
			t.Errorf("work toolset exposed %s (%s)", op.ID, op.Toolset)
		}
	}
	if _, err := filterOperations(all, Config{Toolsets: []string{"unknown"}}); err == nil {
		t.Fatal("unknown toolset accepted")
	}
	withSecrets, err := filterOperations(all, Config{EnableSecretWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withSecrets) != len(all) {
		t.Fatalf("secret-write mode = %d, want %d", len(withSecrets), len(all))
	}
	withoutSecrets, err := filterOperations(all, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutSecrets) != len(all)-2 {
		t.Fatalf("default mode = %d, want %d", len(withoutSecrets), len(all)-2)
	}
}
