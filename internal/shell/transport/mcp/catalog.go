package mcp

import (
	"encoding/json"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// Tool is the MCP advertisement of one operation. The public API schema
// declares the one tool shape; this package only derives the catalog from the
// declared route table.
type Tool = api.MCPTool

// toolCatalog is the deterministic declaration built once from the declared
// route inventory: exactly the 58 registry operations in registry order, each
// with the field-level api.OperationInputSchema of its operation. A
// construction failure is retained so the edge refuses instead of advertising a
// placeholder.
var toolCatalog, toolCatalogErr = buildToolCatalog()

// toolDescriptions provides a concise, accurate human/LLM-friendly description
// for every registered operation in the MCP tool catalog.
var toolDescriptions = map[string]string{
	// Tenant, project and membership administration.
	"tenant.create":     "Create a new tenant and initialize its encrypted derivation key.",
	"tenant.rotate_key": "Rotate a tenant's encrypted credential derivation key and name.",
	"project.create":    "Create a new project inside a tenant.",
	"project.update":    "Rename an existing project at an expected version.",
	"project.list":      "List visible projects inside a tenant with optional pagination.",
	"membership.grant":  "Create or replace a human principal's role set in a project.",
	"membership.revoke": "Revoke a human principal's membership in a project.",
	"membership.list":   "List human principal memberships and roles in a project.",
	"principal.whoami":  "Inspect the authenticated principal ID, kind, tenant/project scope, roles and visible project memberships.",

	// Resource bindings.
	"resource.binding.register": "Register an external resource binding for an enforcing service in a project.",
	"resource.binding.update":   "Update a resource binding's origins, operations, reportable events or metadata.",
	"resource.binding.delete":   "Delete a resource binding at an expected version.",
	"resource.binding.read":     "Read a resource binding's configuration and version by binding ID.",
	"resource.binding.list":     "List resource bindings in a project, optionally filtered by kind.",

	// Pipelines and immutable execution start.
	"pipeline.apply":  "Create or replace a named pipeline definition from YAML in a project.",
	"pipeline.read":   "Read a stored pipeline definition and its YAML by pipeline ID.",
	"pipeline.list":   "List stored pipelines in a project with IDs, names and versions.",
	"pipeline.update": "Update an existing stored pipeline definition at an expected version.",
	"pipeline.delete": "Delete a stored pipeline definition at an expected version.",
	"pipeline.start":  "Admit and start a task-free execution from a stored pipeline ID or inline YAML.",

	// Secret metadata and writes.
	"secret.metadata.read": "Read secret metadata (ID, name, version, updated_at) without exposing secret values.",
	"secret.metadata.list": "List secret metadata in a project without exposing any secret values.",
	"secret.write":         "Create or update an encrypted secret value in a project.",
	"secret.delete":        "Delete a secret from a project at an expected version.",

	// Service accounts and credential lifecycle.
	"service.create":            "Create a project service account with non-admin roles.",
	"service.list":              "List service accounts in a project with states, roles and versions.",
	"service.update":            "Replace a service account's assigned roles at an expected version.",
	"service.enable":            "Enable a service account at an expected version.",
	"service.disable":           "Disable a service account and revoke its active credentials.",
	"service.credential.issue":  "Issue a fresh one-time bearer token for a service account.",
	"service.credential.rotate": "Rotate a service account's bearer token and return the new one-time token.",
	"service.credential.revoke": "Revoke a service account's active bearer credential.",

	// Tasks and conversation handoffs.
	"task.create":           "Create an open task in a project with optional pipeline and inputs.",
	"task.read":             "Read full task state, conversation messages, execution history and any open question.",
	"task.list":             "List tasks in a project with optional status filter and pagination.",
	"task.edit":             "Update a task's title, description and metadata at an expected version.",
	"task.comment":          "Append a durable comment and optional references to a task.",
	"task.select_pipeline":  "Select or update the pipeline and inputs bound to an open task.",
	"task.start":            "Admit and start an execution for an open task's selected pipeline.",
	"task.create_and_start": "Atomically create a task and start its initial pipeline execution.",
	"task.ask":              "Ask an assigned role or service question on a task and pause execution.",
	"task.reply":            "Answer a waiting task's outstanding question and queue a replacement execution.",
	"task.cancel":           "Cancel a task and any nonterminal execution bound to it.",
	"task.history":          "Read paginated task history entries across executions, messages and questions.",
	"task.inbox":            "Query actionable inbox items (requests, waiting tasks, failed tasks) in a project.",

	// Executions, outputs, request resolution and retained logs.
	"execution.inspect":         "Inspect an execution's state, actions, outputs, failure diagnostic and cleanup status.",
	"execution.list":            "List executions in a project with optional task, pipeline and state filters.",
	"execution.cancel":          "Cancel a running or queued execution at an expected version.",
	"execution.action.outputs":  "Submit declared outputs from the active running container action.",
	"execution.request.resolve": "Resolve an active assigned request action with pass or fail and optional outputs.",
	"execution.logs.read":       "Read retained stdout or stderr log chunks for an execution action.",

	// Verification and verified resource reports.
	"actor.verify":    "Verify a forwarded actor credential, roles and approval evidence for a bound resource effect.",
	"resource.report": "Submit a verified resource event report linked to a prior verification ID.",

	// Custom events and authorized reads/subscriptions.
	"event.publish":   "Publish an authenticated custom observation event in a project.",
	"event.read":      "Read committed lifecycle, report and custom events in a project.",
	"event.subscribe": "Authorize and issue NATS subscription grants for project event streams.",

	// Optional model/tool surfaces.
	"model.invoke": "Invoke a configured upstream chat completion model for a project.",
	"tool.invoke":  "Invoke a configured external MCP proxy tool for a project.",
}

// buildToolCatalog derives the catalog from api.Routes so the MCP tool inventory
// can never drift from the REST/registry inventory.
func buildToolCatalog() ([]Tool, error) {
	return catalogForRoutes(api.Routes(), api.OperationInputSchema)
}

// catalogForRoutes builds one tool per route, in route order, from the
// field-level schema producer. A producer failure is a construction error: the
// catalog is never partially advertised with an empty or title-only schema.
// Every schema is copied so the catalog owns its bytes.
func catalogForRoutes(routes []api.Route, schemaFor func(string) (json.RawMessage, error)) ([]Tool, error) {
	catalog := make([]Tool, len(routes))
	for index, route := range routes {
		schema, err := schemaFor(route.Operation)
		if err != nil {
			return nil, fault.Wrap(fault.InternalFailure,
				"MCP tool "+route.Operation+" has no field-level input schema", err)
		}
		description := toolDescriptions[route.Operation]
		if description == "" {
			return nil, fault.New(fault.InternalFailure,
				"MCP tool "+route.Operation+" has no description")
		}
		toolName := route.MCPTool
		if toolName == "" {
			toolName = route.Operation
		}
		catalog[index] = Tool{
			Name:        toolName,
			Description: description,
			InputSchema: append(json.RawMessage(nil), schema...),
		}
	}
	return catalog, nil
}

// ToolCatalog returns the complete MCP tool catalog in declaration order. Each
// call returns a copy of every schema's bytes, so a consumer cannot mutate the
// shared declaration. A construction failure yields no catalog; NewEdge and
// tools/list refuse rather than advertise an incomplete inventory.
func ToolCatalog() []Tool {
	if toolCatalogErr != nil {
		return nil
	}
	catalog := make([]Tool, len(toolCatalog))
	for index, tool := range toolCatalog {
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		catalog[index] = tool
	}
	return catalog
}
