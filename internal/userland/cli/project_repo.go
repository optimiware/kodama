package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodema/api"
)

// runProject dispatches `kodema project <create|list>`.
func runProject(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "project requires a subcommand (create|list)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"project"}, args[1:]...))
	}
	switch args[0] {
	case "create":
		return runProjectCreate(ctx, config, args[1:], ports, deps)
	case "list", "ls":
		return runProjectList(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown project subcommand %q (want create or list)", args[0]))
	}
}

func runProjectCreate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"project", "create"})
	}
	var (
		nameFlag           string
		copyMembersFlag    string
		tenantFlag         string
		idempotencyKeyFlag string
		outputFlag         string
		creatorRolesFlags  []string
	)
	spec := newArgSpec()
	spec.stringFlag(&nameFlag, "--name")
	spec.stringFlag(&copyMembersFlag, "--copy-members-from")
	spec.stringFlag(&tenantFlag, "--tenant")
	spec.listFlag(&creatorRolesFlags, "--creator-role", "--creator-roles")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "project create requires <id>")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}

	projectID := strings.TrimSpace(positionals[0])
	name := strings.TrimSpace(nameFlag)
	if name == "" {
		name = projectID
	}
	req := api.ProjectCreateRequest{
		TenantID:        strings.TrimSpace(tenantFlag),
		ProjectID:       projectID,
		Name:            name,
		CopyMembersFrom: strings.TrimSpace(copyMembersFlag),
	}
	if seen["--creator-role"] || seen["--creator-roles"] {
		roles := parseRoleList(creatorRolesFlags)
		if roles == nil {
			roles = []string{}
		}
		req.CreatorRoles = &roles
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "project.create", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "project.create",
		Request: api.Request{
			Input:          input,
			IdempotencyKey: key,
		},
	}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	var created api.ProjectCreateResult
	_ = json.Unmarshal(result.Body, &created)
	if created.ProjectID == "" {
		created.ProjectID = projectID
	}
	fmt.Fprintf(ports.Out, "project_id\t%s\nname\t%s\nversion\t%d\n",
		created.ProjectID, name, created.Version)
	if req.CopyMembersFrom != "" {
		fmt.Fprintf(ports.Out, "copied_members_from\t%s\n", req.CopyMembersFrom)
	}
	return ExitSuccess
}

func runProjectList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"project", "list"})
	}
	var (
		tenantFlag string
		outputFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&tenantFlag, "--tenant")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	tenantID := strings.TrimSpace(tenantFlag)
	if err := (api.ProjectListRequest{TenantID: tenantID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	projects, firstBody, pages, err := fetchAllProjects(ctx, ports, tenantID)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "project.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if projects == nil {
			projects = []api.ProjectView{}
		}
		combined, err := json.Marshal(api.ProjectListResult{Projects: projects})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "project.list", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tNAME\tVERSION")
	for _, project := range projects {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", orDash(project.ProjectID), orDash(project.Name), project.Version)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func fetchAllProjects(ctx context.Context, ports Ports, tenantID string) ([]api.ProjectView, json.RawMessage, int, error) {
	var (
		all       []api.ProjectView
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.ProjectListRequest{TenantID: tenantID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, nil, pages, err
		}
		invocation := api.Invocation{
			Operation: "project.list",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return nil, nil, pages, err
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return nil, nil, pages, err
		}
		pages++
		if pages == 1 {
			firstBody = result.Body
		}
		var page api.ProjectListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, nil, pages, fmt.Errorf("invalid project.list response: %w", err)
			}
		}
		all = append(all, page.Projects...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, firstBody, pages, nil
}

// runRepo dispatches `kodema repo <create|list>`.
func runRepo(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "repo requires a subcommand (create|list)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"repo"}, args[1:]...))
	}
	switch args[0] {
	case "create":
		return runRepoCreate(ctx, config, args[1:], ports, deps)
	case "list", "ls":
		return runRepoList(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown repo subcommand %q (want create or list)", args[0]))
	}
}

func runRepoCreate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"repo", "create"})
	}
	var (
		projectFlag        string
		defaultBranchFlag  string
		resourceIDFlag     string
		bindingIDFlag      string
		serviceIDFlag      string
		trustedOriginFlags []string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&defaultBranchFlag, "--default-branch", "--branch")
	spec.stringFlag(&resourceIDFlag, "--resource-id")
	spec.stringFlag(&bindingIDFlag, "--binding-id")
	spec.stringFlag(&serviceIDFlag, "--service-id", "--enforcing-service")
	spec.listFlag(&trustedOriginFlags, "--trusted-origin")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "repo create requires <name>")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "repo create requires --project <id> or KODEMA_PROJECT")
	}

	req := api.RepositoryCreateRequest{
		ProjectID:        projectID,
		Name:             strings.TrimSuffix(strings.TrimSpace(positionals[0]), ".git"),
		DefaultBranch:    strings.TrimSpace(defaultBranchFlag),
		ResourceID:       strings.TrimSpace(resourceIDFlag),
		BindingID:        strings.TrimSpace(bindingIDFlag),
		EnforcingService: strings.TrimSpace(serviceIDFlag),
	}
	if seen["--trusted-origin"] {
		origins := make([]string, 0, len(trustedOriginFlags))
		for _, raw := range trustedOriginFlags {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				origins = append(origins, trimmed)
			}
		}
		req.TrustedOrigins = origins
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "repository.create", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "repository.create",
		Request: api.Request{
			Input:          input,
			IdempotencyKey: key,
		},
	}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	var created api.RepositoryCreateResult
	_ = json.Unmarshal(result.Body, &created)
	fmt.Fprintf(ports.Out, "repository\t%s\nresource_id\t%s\nbinding_id\t%s\nenforcing_service\t%s\ndefault_branch\t%s\n",
		orDash(created.Name),
		orDash(created.ResourceID),
		orDash(created.BindingID),
		orDash(created.EnforcingService),
		orDash(created.DefaultBranch),
	)
	if created.CloneURL != "" {
		fmt.Fprintf(ports.Out, "clone_url\t%s\n", created.CloneURL)
	}
	if len(created.TrustedOrigins) > 0 {
		fmt.Fprintf(ports.Out, "trusted_origins\t%s\n", strings.Join(created.TrustedOrigins, ","))
	}
	fmt.Fprintf(ports.Out, "version\t%d\ncreated\t%t\n", created.Version, created.Created)
	return ExitSuccess
}

func runRepoList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"repo", "list"})
	}
	var (
		projectFlag string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "repo list requires --project <id> or KODEMA_PROJECT")
	}
	if err := (api.RepositoryListRequest{ProjectID: projectID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	repos, firstBody, pages, err := fetchAllRepositories(ctx, ports, projectID)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "repository.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if repos == nil {
			repos = []api.RepositoryView{}
		}
		combined, err := json.Marshal(api.RepositoryListResult{Repositories: repos})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "repository.list", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tRESOURCE\tSERVICE\tBRANCH\tCLONE_URL\tVERSION")
	for _, repo := range repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
			orDash(repo.Name),
			orDash(repo.ResourceID),
			orDash(repo.EnforcingService),
			orDash(repo.DefaultBranch),
			orDash(repo.CloneURL),
			repo.Version,
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func fetchAllRepositories(ctx context.Context, ports Ports, projectID string) ([]api.RepositoryView, json.RawMessage, int, error) {
	var (
		all       []api.RepositoryView
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.RepositoryListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, nil, pages, err
		}
		invocation := api.Invocation{
			Operation: "repository.list",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return nil, nil, pages, err
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return nil, nil, pages, err
		}
		pages++
		if pages == 1 {
			firstBody = result.Body
		}
		var page api.RepositoryListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, nil, pages, fmt.Errorf("invalid repository.list response: %w", err)
			}
		}
		all = append(all, page.Repositories...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, firstBody, pages, nil
}
