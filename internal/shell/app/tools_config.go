package app

// Trusted configuration of the optional external MCP tool proxy.
//
// The configuration is app-owned and project-scoped: every public tool alias is
// bound to exactly one project, server and upstream tool. It never invents a
// public naming protocol (the alias is the existing tool.invoke route
// segment / MCP tool field), never carries a secret value and never reuses the
// workload secret allow-list. A nil configuration keeps the optional surface
// disabled with no upstream dependency.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// DefaultToolProxyTimeout bounds one configured upstream tool call when no
// explicit timeout is set.
const DefaultToolProxyTimeout = 30 * time.Second

// ToolProxyConfig is the complete trusted configuration of the optional
// external MCP tool proxy. The zero value is not enabled; a non-nil value with
// no projects is incomplete and refused at construction.
type ToolProxyConfig struct {
	// Projects lists the explicit project-scoped tool surfaces. At least one
	// project, and a unique project scope per entry, is required.
	Projects []ProjectToolProxy
	// SecretRules is the MCP-consumer secret-name allow-list. It is a separate
	// consumer policy from Config.SecretRules (the workload allow-list), so a
	// workload rule can never authorize an MCP upstream credential.
	SecretRules []secret.NameRule
	// Timeout bounds one upstream call. Zero selects DefaultToolProxyTimeout;
	// a negative value is refused.
	Timeout time.Duration
}

// ProjectToolProxy is one project-scoped proxy surface.
type ProjectToolProxy struct {
	// Scope names the one tenant/project the configured servers serve. It must
	// be project-scoped; a tenant-wide entry is refused.
	Scope tenancy.Scope
	// Servers lists the trusted upstreams of this project. The same server name
	// may be used by a different project; it is unique inside one project.
	Servers []ToolServer
}

// ToolServer is one trusted external MCP upstream and its public aliases.
type ToolServer struct {
	// Name is the configured upstream identity the proxy selects.
	Name string
	// Endpoint is the trusted absolute MCP endpoint of the upstream.
	Endpoint string
	// Secrets is the exact set of configured secret names one call resolves.
	Secrets []id.SecretName
	// Credentials maps exactly the configured secret names onto request
	// headers. A missing, extra or duplicate mapping is a construction failure,
	// never a silently dropped credential.
	Credentials []ToolCredential
	// Tools lists the public aliases of this upstream. At least one is required.
	Tools []ToolDefinition
}

// ToolCredential is one explicit secret-to-header mapping.
type ToolCredential struct {
	// Secret is the configured secret name the proxy resolves.
	Secret id.SecretName
	// Header is the request header the resolved value is written to.
	Header string
	// Scheme is an optional authentication scheme prefix.
	Scheme string
}

// ToolDefinition maps one public tool alias to one configured upstream tool.
type ToolDefinition struct {
	// Alias is the public tool name the tool.invoke operation presents. It is
	// unique inside one project and is never the upstream name as well.
	Alias string
	// Name is the configured upstream MCP tool name. It is never public.
	Name string
	// Description is optional configured metadata.
	Description string
	// Validate admits the arguments before any secret resolution or effect. A
	// configured tool without one is refused at construction.
	Validate func(json.RawMessage) error
}

// validate checks the complete trusted configuration before any adapter exists.
// Every ambiguity is a refusal: last-entry-wins, a defaulted validator or a
// dropped credential mapping are never resolved silently.
func (config *ToolProxyConfig) validate() error {
	if config == nil {
		return nil
	}
	if config.Timeout < 0 {
		return fault.New(fault.InvalidInput, "tool proxy timeout is negative")
	}
	if len(config.Projects) == 0 {
		return fault.New(fault.InvalidInput, "enabled tool proxy has no configured projects")
	}
	// The MCP allow-list is validated as the MCP consumer even when no server
	// declares a secret, so a malformed rule cannot hide behind an unused
	// policy.
	if _, err := bridges.NewMCPNamePolicy(config.SecretRules); err != nil {
		return err
	}
	seenProjects := make(map[tenancy.Scope]bool, len(config.Projects))
	for index, project := range config.Projects {
		if err := project.Scope.Validate(); err != nil {
			return fault.Wrap(fault.InvalidInput, fmt.Sprintf("tool proxy project %d scope is invalid", index+1), err)
		}
		if !project.Scope.ProjectScoped() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("tool proxy project %d must name one project", index+1))
		}
		if seenProjects[project.Scope] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("tool proxy project %d repeats an earlier project scope", index+1))
		}
		seenProjects[project.Scope] = true
		if len(project.Servers) == 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("tool proxy project %d has no configured servers", index+1))
		}
		if err := validateToolServers(index, project.Servers); err != nil {
			return err
		}
	}
	return nil
}

// validateToolServers validates one project's server and alias namespace.
func validateToolServers(projectIndex int, servers []ToolServer) error {
	seenServers := make(map[string]bool, len(servers))
	seenAliases := make(map[string]bool)
	for serverIndex, server := range servers {
		position := fmt.Sprintf("tool proxy project %d server %d", projectIndex+1, serverIndex+1)
		if strings.TrimSpace(server.Name) == "" {
			return fault.New(fault.InvalidInput, position+" name is empty")
		}
		if seenServers[server.Name] {
			return fault.New(fault.InvalidInput, position+" server name is configured twice in one project")
		}
		seenServers[server.Name] = true
		if strings.TrimSpace(server.Endpoint) == "" {
			return fault.New(fault.InvalidInput, position+" endpoint is empty")
		}
		if len(server.Tools) == 0 {
			return fault.New(fault.InvalidInput, position+" has no configured tools")
		}
		if err := validateToolCredentials(position, server); err != nil {
			return err
		}
		seenTools := make(map[string]bool, len(server.Tools))
		for toolIndex, tool := range server.Tools {
			alias := strings.TrimSpace(tool.Alias)
			if alias == "" {
				return fault.New(fault.InvalidInput, fmt.Sprintf("%s tool %d has an empty public alias", position, toolIndex+1))
			}
			if seenAliases[alias] {
				return fault.New(fault.InvalidInput, fmt.Sprintf("%s tool %d repeats the public alias %q in this project", position, toolIndex+1, alias))
			}
			seenAliases[alias] = true
			upstream := strings.TrimSpace(tool.Name)
			if upstream == "" {
				return fault.New(fault.InvalidInput, fmt.Sprintf("%s tool %d alias %q has no upstream tool name", position, toolIndex+1, alias))
			}
			if seenTools[upstream] {
				return fault.New(fault.InvalidInput, fmt.Sprintf("%s tool %d repeats upstream tool %q", position, toolIndex+1, upstream))
			}
			seenTools[upstream] = true
			if tool.Validate == nil {
				return fault.New(fault.InvalidInput, fmt.Sprintf("%s tool %d alias %q has no argument validator", position, toolIndex+1, alias))
			}
		}
	}
	return nil
}

// validateToolCredentials requires an exact bijection between the configured
// secret names and the credential mappings.
func validateToolCredentials(position string, server ToolServer) error {
	declared := make(map[id.SecretName]bool, len(server.Secrets))
	for index, name := range server.Secrets {
		if _, err := id.ParseSecretName(string(name)); err != nil {
			return fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s secret %d name is invalid", position, index+1), err)
		}
		if declared[name] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s secret name %q is configured twice", position, name))
		}
		declared[name] = true
	}
	mapped := make(map[id.SecretName]bool, len(server.Credentials))
	for index, credential := range server.Credentials {
		if _, err := id.ParseSecretName(string(credential.Secret)); err != nil {
			return fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s credential %d secret name is invalid", position, index+1), err)
		}
		if !declared[credential.Secret] {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("%s credential mapping names an undeclared secret", position))
		}
		if mapped[credential.Secret] {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("%s credential mapping repeats secret %q", position, credential.Secret))
		}
		mapped[credential.Secret] = true
		if strings.TrimSpace(credential.Header) == "" {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("%s credential %d has an empty header", position, index+1))
		}
	}
	for name := range declared {
		if !mapped[name] {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("%s credential mapping drops the declared secret %q", position, name))
		}
	}
	return nil
}

// timeout returns the configured call bound or the trusted default.
func (config *ToolProxyConfig) timeout() time.Duration {
	if config == nil || config.Timeout <= 0 {
		return DefaultToolProxyTimeout
	}
	return config.Timeout
}
