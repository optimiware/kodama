// Configuration-backed secret-name authorization.
//
// NamePolicy is deny-by-default trusted installation configuration, not
// storage: it is never a store table, a per-name grant row or a scan, and it
// holds no secret value. A policy can only grant the right to resolve one exact
// declared name in one scope; it can never produce material.
//
// A ConfiguredNamePolicy is built for exactly one consumer kind. Composition
// builds one policy per kind (workload activation, model gateway, MCP proxy),
// so a workload policy can never authorize a Shell-internal gateway or MCP name
// and a Shell-internal policy can never widen what a workload may resolve.
//
// The policy is immutable after construction: every rule scope and every name
// is copied into private maps, and Check only reads them. Concurrent Check
// calls are therefore safe without a lock, and rule order never changes a
// result.
package secret

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// ConsumerKind identifies the Shell capability that resolves declared names.
// Each kind has its own trusted configuration and its own policy instance.
type ConsumerKind string

const (
	// ConsumerWorkload is the activation path that materializes declared
	// secret names for one workload.
	ConsumerWorkload ConsumerKind = "workload"
	// ConsumerGateway is the optional model gateway resolving an upstream
	// provider credential inside the Shell.
	ConsumerGateway ConsumerKind = "gateway"
	// ConsumerMCP is the optional MCP proxy resolving an upstream server
	// credential inside the Shell.
	ConsumerMCP ConsumerKind = "mcp"
)

// consumerKinds is the single ordered consumer vocabulary. ConsumerKinds and
// Valid derive from it so the list cannot drift.
var consumerKinds = [...]ConsumerKind{ConsumerWorkload, ConsumerGateway, ConsumerMCP}

// ConsumerKinds returns every consumer kind in declaration order.
func ConsumerKinds() []ConsumerKind {
	return append([]ConsumerKind(nil), consumerKinds[:]...)
}

// Valid reports whether the kind is one of the declared consumer kinds.
func (kind ConsumerKind) Valid() bool {
	for _, candidate := range consumerKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// NameRule is one explicit allow rule: exact declared names in one scope. An
// empty ProjectID is a tenant-wide rule that covers the tenant scope and every
// project inside that tenant. A present ProjectID covers only that exact
// project and never widens to the tenant or to a sibling project.
type NameRule struct {
	Scope tenancy.Scope
	Names []id.SecretName
}

// NamePolicyConfig is the trusted installation configuration for one consumer
// kind. The zero value is a valid, empty policy: it allows no name at all.
// Rules are copied at construction; later caller mutation has no effect.
type NamePolicyConfig struct {
	Consumer ConsumerKind
	Rules    []NameRule
}

// ConfiguredNamePolicy is an immutable, deep-copied policy. It is safe for
// concurrent use and deterministic: it only reads construction-time maps.
type ConfiguredNamePolicy struct {
	consumer ConsumerKind
	rules    map[policyScope]map[id.SecretName]struct{}
}

// The configured policy is the production implementation of the declared
// activation-time NamePolicy seam consumed by SecretResolver, gateway and the
// MCP proxy.
var _ NamePolicy = (*ConfiguredNamePolicy)(nil)

// policyScope is the exact lookup key of one rule. The project is empty for a
// tenant-wide rule, so a project rule can never match another project.
type policyScope struct {
	tenant  id.TenantID
	project id.ProjectID
}

// NewConfiguredNamePolicy validates and copies the trusted configuration. An
// empty consumer means the workload activation path. Invalid scopes, invalid
// secret names, empty rules and unknown consumers are refused rather than
// silently ignored, so a misconfigured installation cannot fail open.
func NewConfiguredNamePolicy(config NamePolicyConfig) (*ConfiguredNamePolicy, error) {
	consumer := config.Consumer
	if consumer == "" {
		consumer = ConsumerWorkload
	}
	if !consumer.Valid() {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("unknown secret name policy consumer %q", consumer))
	}
	rules := make(map[policyScope]map[id.SecretName]struct{}, len(config.Rules))
	for index, rule := range config.Rules {
		if err := rule.Scope.Validate(); err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "secret name policy rule scope is invalid", err)
		}
		if len(rule.Names) == 0 {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("secret name policy rule %d declares no secret names", index))
		}
		key := policyScope{tenant: rule.Scope.TenantID, project: rule.Scope.ProjectID}
		names := rules[key]
		if names == nil {
			names = make(map[id.SecretName]struct{}, len(rule.Names))
			rules[key] = names
		}
		for _, name := range rule.Names {
			if _, err := id.ParseSecretName(string(name)); err != nil {
				return nil, fault.Wrap(fault.InvalidInput, "secret name policy rule declares an invalid secret name", err)
			}
			names[name] = struct{}{}
		}
	}
	return &ConfiguredNamePolicy{consumer: consumer, rules: rules}, nil
}

// Consumer returns the consumer kind this policy was built for.
func (policy *ConfiguredNamePolicy) Consumer() ConsumerKind {
	if policy == nil {
		return ""
	}
	return policy.consumer
}

// Check authorizes one exact declared name in one scope. It returns nil only
// for an explicit allow rule. A tenant-wide rule also authorizes a project
// request in that tenant, because the resolver may legitimately fall back to
// the tenant-wide value; a project rule never authorizes its tenant scope or a
// sibling project. Every unknown name, scope or consumer configuration is
// denied. The denial is a generic forbidden fault: it never echoes the name or
// any secret material.
func (policy *ConfiguredNamePolicy) Check(_ context.Context, scope tenancy.Scope, name id.SecretName) error {
	if policy == nil || policy.rules == nil {
		return fault.New(fault.Forbidden, "secret name is not authorized")
	}
	if names, ok := policy.rules[policyScope{tenant: scope.TenantID, project: scope.ProjectID}]; ok {
		if _, allowed := names[name]; allowed {
			return nil
		}
	}
	if scope.ProjectScoped() {
		if names, ok := policy.rules[policyScope{tenant: scope.TenantID}]; ok {
			if _, allowed := names[name]; allowed {
				return nil
			}
		}
	}
	return fault.New(fault.Forbidden, "secret name is not authorized")
}
