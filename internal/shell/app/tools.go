package app

// Composition of the optional external MCP tool proxy.
//
// One concrete mcpproxy.Proxy is built per explicitly configured project scope
// with the mcphttp transport, the request-local binding authenticator,
// the MCP-consumer secret-name policy, the shared authority reader, a platform
// execution reader that grants only ExecConfiguredTools, the neutral usage store
// and the shared identity/clock seams. The public port selects a project by the
// authenticated principal's tenant plus the request's project and maps the
// public alias to the configured upstream server/tool. No caller-supplied
// credential, endpoint, secret name or permission can widen that selection.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/app/drivers/mcphttp"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/mcpproxy"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
)

// toolProxyDependencies is the complete set of trusted configuration and
// injected boundaries one enabled tool proxy needs. The disabled path ignores
// every field.
type toolProxyDependencies struct {
	Config         *ToolProxyConfig
	InstallationID string
	Controller     store.Controller
	Credentials    *bindingAuthenticator
	Authorizer     authorize.Authorizer
	Authority      authorize.AuthorityReader
	Lookup         secret.NameLookup
	Cipher         secret.EnvelopeCipher
	Store          store.Executor
	IDs            mcpproxy.IDSource
	Clock          Clock
}

// toolProjectKey is the trusted selection key: the authenticated tenant plus the
// requested project. Project ids are not assumed globally unique across tenants.
type toolProjectKey struct {
	tenant  id.TenantID
	project id.ProjectID
}

// toolRoute is one public alias resolved to one configured upstream identity.
type toolRoute struct {
	server string
	tool   string
}

// toolProjectProxy is one project's immutable proxy and alias namespace.
type toolProjectProxy struct {
	proxy   *mcpproxy.Proxy
	aliases map[string]toolRoute
}

// configuredTools is the enabled opsapp.ToolInvocation port.
type configuredTools struct {
	credentials *bindingAuthenticator
	projects    map[toolProjectKey]*toolProjectProxy
}

var _ opsapp.ToolInvocation = (*configuredTools)(nil)

// newConfiguredTools builds the optional tool port. A nil configuration keeps
// the existing disabled refusal and constructs no policy, resolver, execution
// reader or transport. An enabled but incomplete configuration is a
// construction failure.
func newConfiguredTools(deps toolProxyDependencies) (opsapp.ToolInvocation, error) {
	if deps.Config == nil {
		return disabledTools{}, nil
	}
	if err := deps.Config.validate(); err != nil {
		return nil, err
	}
	switch {
	case deps.InstallationID == "":
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an installation id")
	case deps.Controller == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a controller")
	case deps.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires the request-local binding authenticator")
	case deps.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an authorizer")
	case deps.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires an authority reader")
	case deps.Store == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a usage store")
	case deps.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "tool proxy requires a clock")
	}

	policy, err := bridges.NewMCPNamePolicy(deps.Config.SecretRules)
	if err != nil {
		return nil, err
	}
	secrets, err := newToolSecretResolver(deps, policy)
	if err != nil {
		return nil, err
	}
	permissions, err := bridges.NewPermissionSource(authorize.ExecConfiguredTools, deps.Clock)
	if err != nil {
		return nil, err
	}
	executions, err := platform.NewExecutionReader(deps.Controller, permissions)
	if err != nil {
		return nil, err
	}

	tools := &configuredTools{
		credentials: deps.Credentials,
		projects:    make(map[toolProjectKey]*toolProjectProxy, len(deps.Config.Projects)),
	}
	proxyClock := toolProxyClock{clock: deps.Clock}
	for _, project := range deps.Config.Projects {
		assembled, err := newProjectToolProxy(deps, project, executions, secrets, proxyClock)
		if err != nil {
			return nil, err
		}
		tools.projects[toolProjectKey{tenant: project.Scope.TenantID, project: project.Scope.ProjectID}] = assembled
	}
	return tools, nil
}

// newProjectToolProxy builds one project-scoped proxy and its concrete
// transport. Server endpoints and credential mappings are trusted configuration.
func newProjectToolProxy(
	deps toolProxyDependencies,
	project ProjectToolProxy,
	executions mcpproxy.ExecutionReader,
	secrets secret.Resolver,
	clock toolProxyClock,
) (*toolProjectProxy, error) {
	servers := make([]mcpproxy.Server, 0, len(project.Servers))
	transportServers := make([]mcphttp.Server, 0, len(project.Servers))
	aliases := make(map[string]toolRoute)
	for _, server := range project.Servers {
		proxyTools := make([]mcpproxy.Tool, 0, len(server.Tools))
		for _, tool := range server.Tools {
			proxyTools = append(proxyTools, mcpproxy.Tool{
				Name:        tool.Name,
				Description: tool.Description,
				Validate:    tool.Validate,
			})
			aliases[tool.Alias] = toolRoute{server: server.Name, tool: tool.Name}
		}
		servers = append(servers, mcpproxy.Server{
			Name:     server.Name,
			Endpoint: server.Endpoint,
			Secrets:  append([]id.SecretName(nil), server.Secrets...),
			Tools:    proxyTools,
		})
		credentials := make([]mcphttp.Credential, 0, len(server.Credentials))
		for _, credential := range server.Credentials {
			credentials = append(credentials, mcphttp.Credential{
				Secret: credential.Secret,
				Header: credential.Header,
				Scheme: credential.Scheme,
			})
		}
		transportServers = append(transportServers, mcphttp.Server{
			Name:        server.Name,
			Endpoint:    server.Endpoint,
			Credentials: credentials,
		})
	}
	transport, err := mcphttp.New(mcphttp.Config{Servers: transportServers})
	if err != nil {
		return nil, err
	}
	proxy, err := mcpproxy.New(mcpproxy.Config{
		Enabled:     true,
		Scope:       project.Scope,
		Servers:     servers,
		Timeout:     deps.Config.timeout(),
		Retention:   mcpproxy.Retention{},
		Credentials: deps.Credentials,
		Authorizer:  deps.Authorizer,
		Authority:   deps.Authority,
		Executions:  executions,
		Secrets:     secrets,
		Transport:   transport,
		Store:       deps.Store,
		Clock:       clock,
		IDs:         deps.IDs,
	})
	if err != nil {
		return nil, err
	}
	return &toolProjectProxy{proxy: proxy, aliases: aliases}, nil
}

// newToolSecretResolver builds the MCP-consumer resolver. A configuration with
// no declared secrets uses a resolver that refuses every resolution, so the
// proxy still has a non-nil boundary while no secret lookup or cipher is
// required.
func newToolSecretResolver(deps toolProxyDependencies, policy secret.NamePolicy) (secret.Resolver, error) {
	if !toolConfigDeclaresSecrets(deps.Config) {
		return noToolSecrets{}, nil
	}
	return secret.NewResolver(secret.ResolverConfig{
		InstallationID: deps.InstallationID,
		Policy:         policy,
		Lookup:         deps.Lookup,
		Cipher:         deps.Cipher,
	})
}

// toolConfigDeclaresSecrets reports whether any configured server names a
// secret.
func toolConfigDeclaresSecrets(config *ToolProxyConfig) bool {
	if config == nil {
		return false
	}
	for _, project := range config.Projects {
		for _, server := range project.Servers {
			if len(server.Secrets) > 0 {
				return true
			}
		}
	}
	return false
}

// noToolSecrets is the fail-closed resolver of a configuration without declared
// secrets. The proxy never asks it for a value because every configured server
// declares none.
type noToolSecrets struct{}

var _ secret.Resolver = noToolSecrets{}

func (noToolSecrets) Resolve(context.Context, secret.ResolveRequest) ([]secret.Resolved, error) {
	return nil, fault.New(fault.DependencyUnavailable, "tool proxy has no configured secrets")
}

// toolProxyClock adapts the composition clock (authoritative time plus
// cancellable sleep) to the proxy's timeout seam.
type toolProxyClock struct {
	clock Clock
}

func (clock toolProxyClock) Now() time.Time { return clock.clock.Now() }

func (clock toolProxyClock) After(duration time.Duration) <-chan time.Time {
	return time.After(duration)
}

// Call resolves one configured public alias inside the authenticated caller's
// tenant and the requested project, binds the already authenticated principal to
// a request-local credential, and delegates to that project's proxy. The
// upstream effect happens only inside the proxy, after it re-reads current
// authority and resolves the configured credential.
func (tools *configuredTools) Call(
	ctx context.Context,
	principal authorize.Principal,
	projectID id.ProjectID,
	alias string,
	arguments json.RawMessage,
) (mcpproxy.CallResult, error) {
	if tools == nil {
		return mcpproxy.CallResult{}, fault.New(fault.NotFound, "the optional MCP tool proxy is not enabled")
	}
	if _, err := id.ParseProjectID(string(projectID)); err != nil {
		return mcpproxy.CallResult{}, fault.New(fault.NotFound, "tool is not configured")
	}
	project, ok := tools.projects[toolProjectKey{tenant: principal.Scope.TenantID, project: projectID}]
	if !ok {
		return mcpproxy.CallResult{}, fault.New(fault.NotFound, "tool is not configured")
	}
	route, ok := project.aliases[alias]
	if !ok {
		return mcpproxy.CallResult{}, fault.New(fault.NotFound, "tool is not configured")
	}

	var result mcpproxy.CallResult
	bindErr := tools.credentials.withPrincipal(principal, func(credential string) error {
		var callErr error
		result, callErr = project.proxy.Call(ctx, mcpproxy.CallRequest{
			Credential: credential,
			Server:     route.server,
			Tool:       route.tool,
			Arguments:  arguments,
		})
		return callErr
	})
	if bindErr != nil {
		return mcpproxy.CallResult{}, bindErr
	}
	return result, nil
}

// ensure the request-local binding authenticator satisfies the proxy's
// credential port.
var _ authorize.CurrentPrincipal = (*bindingAuthenticator)(nil)
