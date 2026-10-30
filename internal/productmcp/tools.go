package productmcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverName and serverVersion identify this MCP server, distinct from the
// MCP proxy (internal/mcpproxy), which relays tools of external servers to a
// run. This server exposes Kodama's own product use cases.
const (
	serverName    = "kodama-product-mcp"
	serverVersion = "1"
)

// Tool names. They are the contract with agents and are stable.
const (
	ToolProjectCreate     = "kodama_project_create"
	ToolProjectGet        = "kodama_project_get"
	ToolProjectList       = "kodama_project_list"
	ToolRepositoryCreate  = "kodama_repository_create"
	ToolRepositoryGet     = "kodama_repository_get"
	ToolRepositoryList    = "kodama_repository_list"
	ToolRepositorySeed    = "kodama_repository_request_seed"
	ToolWorkItemCreate    = "kodama_work_item_create"
	ToolWorkItemGet       = "kodama_work_item_get"
	ToolWorkItemList      = "kodama_work_item_list"
	ToolProvisioningStart = "kodama_provisioning_run_start"
)

type toolSpec struct {
	name       string
	personOnly bool // never offered to a run's attempt
	add        func(srv *mcp.Server, svc *Service, c Caller)
}

func boolp(b bool) *bool { return &b }

// spec builds one tool. Authorization is never decided by the listing: every
// call is authorized again by the use case with the caller's current authority.
func spec[In, Out any](name, title, description string, personOnly, readOnly bool, call func(*Service) func(context.Context, Caller, In) (Out, error)) toolSpec {
	return toolSpec{name: name, personOnly: personOnly, add: func(srv *mcp.Server, svc *Service, c Caller) {
		fn := call(svc)
		mcp.AddTool(srv, &mcp.Tool{
			Name: name, Title: title, Description: description,
			Annotations: &mcp.ToolAnnotations{
				Title: title, ReadOnlyHint: readOnly, DestructiveHint: boolp(false),
				IdempotentHint: !readOnly, OpenWorldHint: boolp(false),
			},
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			out, err := fn(ctx, c, in)
			if err != nil {
				// The SDK renders a regular error as the tool's error result. Only
				// the stable code and constant text are ever returned.
				var zero Out
				code, msg := classify(err)
				return nil, zero, &Error{Code: code, Message: msg}
			}
			return nil, out, nil
		})
	}}
}

// catalogue is the complete tool set, in listing order.
var catalogue = []toolSpec{
	spec(ToolProjectCreate, "Create project",
		"Create a project in your tenant. Requires the project:create permission (tenant admin). Idempotent on idempotencyKey.",
		true, false, func(s *Service) func(context.Context, Caller, ProjectCreateIn) (ProjectOut, error) {
			return s.CreateProject
		}),
	spec(ToolProjectGet, "Get project", "Read one project you may read.",
		true, true, func(s *Service) func(context.Context, Caller, ProjectGetIn) (ProjectOut, error) { return s.GetProject }),
	spec(ToolProjectList, "List projects", "List the projects you may read.",
		true, true, func(s *Service) func(context.Context, Caller, ProjectListIn) (ProjectListOut, error) {
			return s.ListProjects
		}),
	spec(ToolRepositoryCreate, "Create project repository",
		"Create an empty internally hosted Git repository in a project. Requires the engineer role on the project. Idempotent on idempotencyKey. To bring an existing codebase in, create the repository, then request a seed and push the history with git.",
		false, false, func(s *Service) func(context.Context, Caller, RepositoryCreateIn) (RepositoryOut, error) {
			return s.CreateRepository
		}),
	spec(ToolRepositoryGet, "Get repository", "Read one repository of a project.",
		false, true, func(s *Service) func(context.Context, Caller, RepositoryGetIn) (RepositoryOut, error) {
			return s.GetRepository
		}),
	spec(ToolRepositoryList, "List repositories", "List the repositories of a project.",
		false, true, func(s *Service) func(context.Context, Caller, RepositoryListIn) (RepositoryListOut, error) {
			return s.ListRepositories
		}),
	spec(ToolRepositorySeed, "Request repository seed",
		"For a person with the engineer role: request a short-lived single-use credential and push instructions to bootstrap an empty, ready repository with an existing codebase by git push. The password is shown once; hand it only to Git, never to files or logs.",
		true, false, func(s *Service) func(context.Context, Caller, SeedIn) (SeedOut, error) { return s.RequestSeed }),
	spec(ToolWorkItemCreate, "Create work item",
		"Create a work item (a task by default) in a project's backlog. Idempotent on idempotencyKey.",
		false, false, func(s *Service) func(context.Context, Caller, WorkItemCreateIn) (WorkItemOut, error) {
			return s.CreateWorkItem
		}),
	spec(ToolWorkItemGet, "Get work item", "Read one work item of a project.",
		false, true, func(s *Service) func(context.Context, Caller, WorkItemGetIn) (WorkItemOut, error) {
			return s.GetWorkItem
		}),
	spec(ToolWorkItemList, "List work items", "List the work items of a project, optionally by state or kind.",
		false, true, func(s *Service) func(context.Context, Caller, WorkItemListIn) (WorkItemListOut, error) {
			return s.ListWorkItems
		}),
	spec(ToolProvisioningStart, "Start provisioning run",
		"Start a bounded provisioning run (a pipeline that creates and seeds repositories) in one project. Requires run:manage on the project. Limited in inputs and in concurrently active runs; the run receives its own seed authority only for repositories it creates, and this tool returns no credential. Idempotent on idempotencyKey.",
		true, false, func(s *Service) func(context.Context, Caller, ProvisioningStartIn) (RunOut, error) {
			return s.StartProvisioningRun
		}),
}

// ToolNames lists the tools offered to the caller, in order. A run's attempt is
// offered only the repository and work item tools of its own project; offering
// is not authorization.
func ToolNames(c Caller) []string {
	var out []string
	for _, t := range catalogue {
		if t.personOnly && c.IsWorkload() {
			continue
		}
		out = append(out, t.name)
	}
	return out
}

var schemas = mcp.NewSchemaCache()

// NewServer builds the MCP server of one authenticated caller. The caller is
// fixed in the tools' closures: no argument, header or session can change it.
func NewServer(svc *Service, c Caller) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, &mcp.ServerOptions{
		// Only tools are advertised: resources, prompts and sampling would sit
		// outside tool authorization.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		SchemaCache:  schemas,
	})
	for _, t := range catalogue {
		if t.personOnly && c.IsWorkload() {
			continue
		}
		t.add(srv, svc, c)
	}
	return srv
}
