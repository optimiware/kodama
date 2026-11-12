// Per-consumer secret-name policy constructors.
//
// A secret.NamePolicy is trusted installation configuration, never a
// store table or grant row. Composition builds one policy per consumer kind so
// a workload policy cannot authorize a Shell-internal gateway or MCP name and
// an internal policy cannot widen what a workload resolves. Each constructor
// fixes its consumer kind; empty configuration remains a valid policy that
// authorizes nothing, so a missing allow-list fails closed.
package bridges

import (
	"github.com/optimiware/kodama/internal/shell/secret"
)

// NewWorkloadNamePolicy builds the activation-path policy.
func NewWorkloadNamePolicy(rules []secret.NameRule) (*secret.ConfiguredNamePolicy, error) {
	return secret.NewConfiguredNamePolicy(secret.NamePolicyConfig{Consumer: secret.ConsumerWorkload, Rules: rules})
}

// NewGatewayNamePolicy builds the model-gateway policy.
func NewGatewayNamePolicy(rules []secret.NameRule) (*secret.ConfiguredNamePolicy, error) {
	return secret.NewConfiguredNamePolicy(secret.NamePolicyConfig{Consumer: secret.ConsumerGateway, Rules: rules})
}

// NewMCPNamePolicy builds the MCP-proxy policy.
func NewMCPNamePolicy(rules []secret.NameRule) (*secret.ConfiguredNamePolicy, error) {
	return secret.NewConfiguredNamePolicy(secret.NamePolicyConfig{Consumer: secret.ConsumerMCP, Rules: rules})
}
