package api

import (
	"strings"
)

// routeSpec is one registry row: the operation name and its one REST binding.
// The MCP tool name is the underscore-sanitized operation name and both schema
// names are derived, so a single table keeps the REST and MCP inventories in
// parity with the Shell operation registry.
type routeSpec struct {
	operation string
	method    Method
	path      string
}

// routeSpecs is the route inventory: exactly the 58 registered operations,
// one method/path and one MCP tool each. The six product-fixed S08 endpoints
// appear verbatim.
var routeSpecs = []routeSpec{
	// Tenant, project and membership administration.
	{"tenant.create", MethodPost, "/api/v1/tenants"},
	{"tenant.rotate_key", MethodPost, "/api/v1/tenants/{tenant}/rotate-key"},
	{"project.create", MethodPost, "/api/v1/tenants/{tenant}/projects"},
	{"project.update", MethodPatch, "/api/v1/projects/{project}"},
	{"project.list", MethodGet, "/api/v1/tenants/{tenant}/projects"},
	{"membership.grant", MethodPut, "/api/v1/projects/{project}/memberships/{principal}"},
	{"membership.revoke", MethodDelete, "/api/v1/projects/{project}/memberships/{principal}"},
	{"membership.list", MethodGet, "/api/v1/projects/{project}/memberships"},
	{"principal.whoami", MethodGet, "/api/v1/whoami"},

	// Resource bindings.
	{"resource.binding.register", MethodPost, "/api/v1/projects/{project}/bindings"},
	{"resource.binding.update", MethodPatch, "/api/v1/bindings/{binding}"},
	{"resource.binding.delete", MethodDelete, "/api/v1/bindings/{binding}"},
	{"resource.binding.read", MethodGet, "/api/v1/bindings/{binding}"},
	{"resource.binding.list", MethodGet, "/api/v1/projects/{project}/bindings"},

	// Pipelines and immutable execution start.
	{"pipeline.apply", MethodPut, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.read", MethodGet, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.list", MethodGet, "/api/v1/projects/{project}/pipelines"},
	{"pipeline.update", MethodPatch, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.delete", MethodDelete, "/api/v1/projects/{project}/pipelines/{pipeline}"},
	{"pipeline.start", MethodPost, "/api/v1/projects/{project}/pipelines/{pipeline}/start"},

	// Secret metadata and writes.
	{"secret.metadata.read", MethodGet, "/api/v1/projects/{project}/secrets/{secret}"},
	{"secret.metadata.list", MethodGet, "/api/v1/projects/{project}/secrets"},
	{"secret.write", MethodPut, "/api/v1/projects/{project}/secrets/{secret}"},
	{"secret.delete", MethodDelete, "/api/v1/projects/{project}/secrets/{secret}"},

	// Service accounts and credential lifecycle.
	{"service.create", MethodPost, "/api/v1/projects/{project}/services"},
	{"service.list", MethodGet, "/api/v1/projects/{project}/services"},
	{"service.update", MethodPatch, "/api/v1/services/{service}"},
	{"service.enable", MethodPost, "/api/v1/services/{service}/enable"},
	{"service.disable", MethodPost, "/api/v1/services/{service}/disable"},
	{"service.credential.issue", MethodPost, "/api/v1/services/{service}/credentials"},
	{"service.credential.rotate", MethodPost, "/api/v1/services/{service}/credentials/rotate"},
	{"service.credential.revoke", MethodDelete, "/api/v1/services/{service}/credentials"},

	// Tasks and conversation handoffs.
	{"task.create", MethodPost, "/api/v1/projects/{project}/tasks"},
	{"task.read", MethodGet, "/api/v1/tasks/{task}"},
	{"task.list", MethodGet, "/api/v1/projects/{project}/tasks"},
	{"task.edit", MethodPatch, "/api/v1/tasks/{task}"},
	{"task.comment", MethodPost, "/api/v1/tasks/{task}/comments"},
	{"task.select_pipeline", MethodPut, "/api/v1/tasks/{task}/pipeline"},
	{"task.start", MethodPost, "/api/v1/tasks/{task}/start"},
	{"task.create_and_start", MethodPost, "/api/v1/projects/{project}/tasks/create-and-start"},
	{"task.ask", MethodPost, "/api/v1/tasks/{task}/ask"},
	{"task.reply", MethodPost, "/api/v1/tasks/{task}/reply"},
	{"task.cancel", MethodPost, "/api/v1/tasks/{task}/cancel"},
	{"task.history", MethodGet, "/api/v1/tasks/{task}/history"},
	{"task.inbox", MethodGet, "/api/v1/projects/{project}/inbox"},

	// Executions, outputs, request resolution and retained logs.
	{"execution.inspect", MethodGet, "/api/v1/executions/{execution}"},
	{"execution.list", MethodGet, "/api/v1/projects/{project}/executions"},
	{"execution.cancel", MethodPost, "/api/v1/executions/{execution}/cancel"},
	{"execution.action.outputs", MethodPut, "/api/v1/executions/{execution}/actions/{action}/outputs"},
	{"execution.request.resolve", MethodPost, "/api/v1/executions/{execution}/actions/{action}/resolve"},
	{"execution.logs.read", MethodGet, "/api/v1/executions/{execution}/logs"},

	// Verification and verified resource reports.
	{"actor.verify", MethodPost, "/api/v1/verify"},
	{"resource.report", MethodPost, "/api/v1/reports"},

	// Custom events and authorized reads/subscriptions.
	{"event.publish", MethodPost, "/api/v1/projects/{project}/events"},
	{"event.read", MethodGet, "/api/v1/projects/{project}/events"},
	{"event.subscribe", MethodPost, "/api/v1/projects/{project}/events/subscribe"},

	// Optional model/tool surfaces.
	{"model.invoke", MethodPost, "/api/v1/projects/{project}/models/{model}/invoke"},
	{"tool.invoke", MethodPost, "/api/v1/projects/{project}/tools/{tool}/invoke"},
}

// alternateRouteSpecs is the additive second REST binding of an operation. An
// alternate is a second binding, never a second operation: it keeps the
// operation name, MCP tool and schema names, and api.Routes() stays exactly the
// 58 canonical operations.
var alternateRouteSpecs = []routeSpec{
	// pipeline.start accepts inline YAML on a task-free executions route; the
	// canonical stored-pipeline route is unchanged.
	{"pipeline.start", MethodPost, "/api/v1/projects/{project}/executions"},
}

// routes is the derived immutable canonical route table.
var routes = buildRoutes(routeSpecs)

// alternateRoutes is the derived immutable alternate binding table.
var alternateRoutes = buildRoutes(alternateRouteSpecs)

// allRoutes is the canonical table followed by the alternate bindings. Matching
// searches both; LookupRoute and Routes expose only the canonical table.
var allRoutes = func() []Route {
	out := make([]Route, 0, len(routes)+len(alternateRoutes))
	out = append(out, routes...)
	out = append(out, alternateRoutes...)
	return out
}()

// routesByOperation indexes the canonical table by operation name.
var routesByOperation = func() map[string]Route {
	byOperation := make(map[string]Route, len(routes))
	for _, route := range routes {
		byOperation[route.Operation] = route
	}
	return byOperation
}()

// mcpToolOperations maps both the advertised underscore MCP tool name and the
// canonical dotted operation name back to the canonical operation name.
var mcpToolOperations = func() map[string]string {
	byName := make(map[string]string, len(routes)*2)
	for _, route := range routes {
		byName[route.Operation] = route.Operation
		byName[route.MCPTool] = route.Operation
	}
	return byName
}()

// buildRoutes derives each route's MCP tool and schema names from its
// operation name.
func buildRoutes(specs []routeSpec) []Route {
	out := make([]Route, len(specs))
	for index, spec := range specs {
		out[index] = Route{
			Operation:    spec.operation,
			Method:       spec.method,
			Path:         spec.path,
			MCPTool:      strings.ReplaceAll(spec.operation, ".", "_"),
			InputSchema:  spec.operation + ".input.v1",
			ResultSchema: spec.operation + ".result.v1",
		}
	}
	return out
}

// MCPToolOperationName resolves either an advertised underscore MCP tool name
// (such as "task_create_and_start") or a canonical dotted operation name (such
// as "task.create_and_start") to the canonical operation name. Unknown and
// empty names report false.
func MCPToolOperationName(name string) (string, bool) {
	operation, ok := mcpToolOperations[name]
	return operation, ok
}

// Routes returns every canonical route in declaration order. Alternate
// bindings are exposed separately by AlternateRoutes.
func Routes() []Route {
	return append([]Route(nil), routes...)
}

// AlternateRoutes returns the additive second REST bindings. The slice is a
// copy; callers cannot mutate the table.
func AlternateRoutes() []Route {
	return append([]Route(nil), alternateRoutes...)
}

// LookupRoute returns the canonical route for an operation name. Unknown and
// empty names are refused; a resource operation is never a public route.
func LookupRoute(operation string) (Route, bool) {
	route, ok := routesByOperation[operation]
	return route, ok
}

// Match resolves one concrete method/path to its declared route and extracts
// the path parameters. Both the canonical table and the alternate bindings are
// searched. A trailing slash, a query or fragment, a glob, an invalid method or
// an unknown path refuses. The most literal matching route wins so a literal
// segment is never captured as another route's parameter.
func Match(method Method, path string) (Route, map[string]string, bool) {
	if !method.Valid() || !validMatchPath(path) {
		return Route{}, nil, false
	}
	segments := splitPath(path)
	var best Route
	bestParams := map[string]string(nil)
	bestLiteral := -1
	for _, route := range allRoutes {
		if route.Method != method {
			continue
		}
		params, literal, ok := matchPath(route.Path, segments)
		if !ok {
			continue
		}
		if literal > bestLiteral {
			best = route
			bestParams = params
			bestLiteral = literal
		}
	}
	if bestLiteral < 0 {
		return Route{}, nil, false
	}
	return best, bestParams, true
}

// validMatchPath checks the concrete request path syntax.
func validMatchPath(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") {
		return false
	}
	if strings.HasSuffix(path, "/") {
		return false
	}
	if strings.ContainsAny(path, "?#* ") {
		return false
	}
	for _, segment := range splitPath(path) {
		if segment == "" {
			return false
		}
	}
	return true
}

// splitPath splits an absolute path into non-empty segments.
func splitPath(path string) []string {
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

// matchPath matches one route template against concrete segments. It returns the
// captured parameters and the number of literal segments matched.
func matchPath(template string, segments []string) (map[string]string, int, bool) {
	templateSegments := splitPath(template)
	if len(templateSegments) != len(segments) {
		return nil, 0, false
	}
	params := make(map[string]string)
	literal := 0
	for index, templateSegment := range templateSegments {
		value := segments[index]
		if value == "" {
			return nil, 0, false
		}
		if strings.HasPrefix(templateSegment, "{") && strings.HasSuffix(templateSegment, "}") {
			params[strings.TrimSuffix(strings.TrimPrefix(templateSegment, "{"), "}")] = value
			continue
		}
		if templateSegment != value {
			return nil, 0, false
		}
		literal++
	}
	return params, literal, true
}
