// Behavior tests for the per-consumer-kind secret name policy constructors.
//
// Each constructor fixes the consumer kind and delegates to the
// deny-by-default ConfiguredNamePolicy. Empty configuration is a valid policy
// that allows nothing, so a misconfigured installation fails closed.
package bridges_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func namePolicyRules(consumerName id.SecretName) []secret.NameRule {
	return []secret.NameRule{{
		Scope: testScope(),
		Names: []id.SecretName{consumerName},
	}}
}

func TestNamePolicyConstructorsSeparateConsumerKinds(t *testing.T) {
	workload, err := bridges.NewWorkloadNamePolicy(namePolicyRules("WORKLOAD_SECRET"))
	if err != nil {
		t.Fatalf("NewWorkloadNamePolicy: %v", err)
	}
	gateway, err := bridges.NewGatewayNamePolicy(namePolicyRules("GATEWAY_SECRET"))
	if err != nil {
		t.Fatalf("NewGatewayNamePolicy: %v", err)
	}
	mcp, err := bridges.NewMCPNamePolicy(namePolicyRules("MCP_SECRET"))
	if err != nil {
		t.Fatalf("NewMCPNamePolicy: %v", err)
	}
	if workload.Consumer() != secret.ConsumerWorkload || gateway.Consumer() != secret.ConsumerGateway || mcp.Consumer() != secret.ConsumerMCP {
		t.Fatalf("consumer kinds = %q/%q/%q", workload.Consumer(), gateway.Consumer(), mcp.Consumer())
	}

	if err := workload.Check(context.Background(), testScope(), "WORKLOAD_SECRET"); err != nil {
		t.Fatalf("workload allowed name refused: %v", err)
	}
	if err := workload.Check(context.Background(), testScope(), "GATEWAY_SECRET"); err == nil {
		t.Fatal("workload policy authorized a gateway-only name")
	}
	if err := gateway.Check(context.Background(), testScope(), "GATEWAY_SECRET"); err != nil {
		t.Fatalf("gateway allowed name refused: %v", err)
	}
	if err := mcp.Check(context.Background(), testScope(), "GATEWAY_SECRET"); err == nil {
		t.Fatal("mcp policy authorized a gateway-only name")
	}
	if err := mcp.Check(context.Background(), tenancy.Scope{TenantID: "tenant-b"}, "MCP_SECRET"); err == nil {
		t.Fatal("mcp policy authorized a name in another tenant")
	}
}

func TestNamePolicyEmptyConfigurationDeniesEverything(t *testing.T) {
	policies := []secret.NamePolicy{
		mustWorkloadPolicy(t, nil),
		mustGatewayPolicy(t, nil),
		mustMCPPolicy(t, nil),
	}
	for index, policy := range policies {
		if err := policy.Check(context.Background(), testScope(), "ANY_SECRET"); err == nil {
			t.Fatalf("empty policy %d authorized a name", index)
		}
	}
}

func TestNamePolicyConstructorsRefuseInvalidConfiguration(t *testing.T) {
	invalid := []secret.NameRule{{Scope: tenancy.Scope{}, Names: []id.SecretName{"ANY_SECRET"}}}
	if _, err := bridges.NewWorkloadNamePolicy(invalid); err == nil {
		t.Fatal("NewWorkloadNamePolicy accepted an invalid rule")
	}
	if _, err := bridges.NewGatewayNamePolicy([]secret.NameRule{{Scope: testScope()}}); err == nil {
		t.Fatal("NewGatewayNamePolicy accepted a rule with no names")
	}
	if _, err := bridges.NewMCPNamePolicy([]secret.NameRule{{Scope: testScope(), Names: []id.SecretName{"bad name"}}}); err == nil {
		t.Fatal("NewMCPNamePolicy accepted an invalid secret name")
	}
}

func mustWorkloadPolicy(t *testing.T, rules []secret.NameRule) secret.NamePolicy {
	t.Helper()
	policy, err := bridges.NewWorkloadNamePolicy(rules)
	if err != nil {
		t.Fatalf("NewWorkloadNamePolicy: %v", err)
	}
	return policy
}

func mustGatewayPolicy(t *testing.T, rules []secret.NameRule) secret.NamePolicy {
	t.Helper()
	policy, err := bridges.NewGatewayNamePolicy(rules)
	if err != nil {
		t.Fatalf("NewGatewayNamePolicy: %v", err)
	}
	return policy
}

func mustMCPPolicy(t *testing.T, rules []secret.NameRule) secret.NamePolicy {
	t.Helper()
	policy, err := bridges.NewMCPNamePolicy(rules)
	if err != nil {
		t.Fatalf("NewMCPNamePolicy: %v", err)
	}
	return policy
}
