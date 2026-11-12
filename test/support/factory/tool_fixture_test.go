package factory

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestWithToolFixtureIsOptIn proves the external MCP proxy stays disabled by
// default and that selecting the fixture wires exactly its bounded endpoint and
// separate MCP secret policy.
func TestWithToolFixtureIsOptIn(t *testing.T) {
	var options Options
	if options.ToolFixture != nil {
		t.Fatal("zero Options selected a tool fixture")
	}
	if config := options.composeToolsConfig(tenancy.Scope{}); config != nil {
		t.Fatal("zero Options composed a tool proxy configuration")
	}

	fixture := NewToolFixture(t)
	options.apply(WithToolFixture(fixture))
	if options.ToolFixture != fixture {
		t.Fatal("WithToolFixture did not select the supplied fixture")
	}
	scope := tenancy.Scope{TenantID: TenantID, ProjectID: ProjectID}
	config := options.composeToolsConfig(scope)
	if config == nil {
		t.Fatal("WithToolFixture composed no tool proxy configuration")
	}
	if len(config.Projects) != 1 || config.Projects[0].Scope != scope {
		t.Fatalf("tool proxy projects = %+v; want exactly the harness scope", config.Projects)
	}
	servers := config.Projects[0].Servers
	if len(servers) != 1 {
		t.Fatalf("tool proxy servers = %d; want 1", len(servers))
	}
	server := servers[0]
	if server.Endpoint != fixture.Endpoint() {
		t.Fatalf("tool proxy endpoint = %q; want the fixture endpoint", server.Endpoint)
	}
	if len(server.Secrets) != 1 || server.Secrets[0] != ToolSecretName {
		t.Fatalf("tool proxy secrets = %v; want exactly %s", server.Secrets, ToolSecretName)
	}
	if len(server.Tools) != 1 || server.Tools[0].Alias != ToolAlias || server.Tools[0].Name != ToolUpstreamName {
		t.Fatalf("tool proxy tools = %+v; want exactly the configured alias", server.Tools)
	}
	if len(config.SecretRules) != 1 || config.SecretRules[0].Scope != scope ||
		len(config.SecretRules[0].Names) != 1 || config.SecretRules[0].Names[0] != ToolSecretName {
		t.Fatalf("tool proxy secret rules = %+v; want exactly the fixture scope and name", config.SecretRules)
	}
}
