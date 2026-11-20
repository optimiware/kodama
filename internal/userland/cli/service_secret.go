package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/optimiware/kodema/api"
)

const defaultServiceCredentialTTLSeconds = 3600

// runService dispatches `kodema service <create|list|update|enable|disable|credential>`.
func runService(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "service requires a subcommand (create|list|update|enable|disable|credential)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"service"}, args[1:]...))
	}
	switch args[0] {
	case "create":
		return runServiceCreate(ctx, config, args[1:], ports, deps)
	case "list", "ls":
		return runServiceList(ctx, config, args[1:], ports, deps)
	case "update":
		return runServiceUpdate(ctx, config, args[1:], ports, deps)
	case "enable":
		return runServiceToggle(ctx, config, args[1:], ports, deps, true)
	case "disable":
		return runServiceToggle(ctx, config, args[1:], ports, deps, false)
	case "credential", "cred":
		return runServiceCredential(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown service subcommand %q", args[0]))
	}
}

func runServiceCreate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", "create"})
	}
	var (
		projectFlag        string
		roleFlags          []string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.listFlag(&roleFlags, "--role", "--roles", "-r")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "service create requires <service-id>")
	}
	serviceID := strings.TrimSpace(positionals[0])
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "service create requires --project <id> or KODEMA_PROJECT")
	}
	roles := parseRoleList(roleFlags)
	if roles == nil {
		roles = []string{}
	}
	req := api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: serviceID,
		Roles:     roles,
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
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "service.create", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "service.create",
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
	var created api.ServiceCreateResult
	if err := json.Unmarshal(result.Body, &created); err != nil || created.ServiceID == "" {
		return renderTableFallback(ports, result)
	}
	fmt.Fprintf(ports.Out, "service_id\t%s\nversion\t%d\n", created.ServiceID, created.Version)
	return ExitSuccess
}

func runServiceList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", "list"})
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
		return usage(ports.Err, "service list requires --project <id> or KODEMA_PROJECT")
	}
	if err := (api.ServiceListRequest{ProjectID: projectID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	services, firstBody, pages, err := fetchAllServices(ctx, ports, projectID)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "service.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if services == nil {
			services = []api.ServiceSummaryView{}
		}
		combined, err := json.Marshal(api.ServiceListResult{Services: services})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "service.list", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSTATE\tROLES\tVERSION")
	for _, svc := range services {
		rolesText := "-"
		if len(svc.Roles) > 0 {
			rolesText = strings.Join(svc.Roles, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", svc.ServiceID, orDash(svc.State), rolesText, svc.Version)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runServiceUpdate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", "update"})
	}
	var (
		projectFlag        string
		roleFlags          []string
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.listFlag(&roleFlags, "--role", "--roles", "-r")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "service update requires <service-id>")
	}
	serviceID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	roles := parseRoleList(roleFlags)
	if roles == nil {
		roles = []string{}
	}
	req := api.ServiceUpdateRequest{ServiceID: serviceID, Roles: roles}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	if expectedVer == nil {
		ver, code := lookupServiceVersion(ctx, config, ports, deps, projectFlag, serviceID, "service update")
		if code != ExitSuccess {
			return code
		}
		expectedVer = &ver
	}
	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "service.update", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "service.update",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var updated api.ServiceUpdateResult
	_ = json.Unmarshal(result.Body, &updated)
	fmt.Fprintf(ports.Out, "updated service %s (version=%d)\n", serviceID, updated.Version)
	return ExitSuccess
}

func runServiceToggle(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps, enable bool) int {
	subcmd := "disable"
	op := "service.disable"
	if enable {
		subcmd = "enable"
		op = "service.enable"
	}
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", subcmd})
	}
	var (
		projectFlag        string
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, fmt.Sprintf("service %s requires <service-id>", subcmd))
	}
	serviceID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := (api.ServiceEnableRequest{ServiceID: serviceID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	if expectedVer == nil {
		ver, code := lookupServiceVersion(ctx, config, ports, deps, projectFlag, serviceID, "service "+subcmd)
		if code != ExitSuccess {
			return code
		}
		expectedVer = &ver
	}
	input, err := json.Marshal(api.ServiceEnableRequest{ServiceID: serviceID})
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, op, deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: op,
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var toggled api.ServiceEnableResult
	_ = json.Unmarshal(result.Body, &toggled)
	fmt.Fprintf(ports.Out, "service_id\t%s\nstate\t%s\nversion\t%d\nrevoked_credentials\t%d\n",
		serviceID, orDash(toggled.State), toggled.Version, toggled.RevokedCredentials)
	return ExitSuccess
}

func runServiceCredential(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "service credential requires a subcommand (issue|rotate|revoke)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"service", "credential"}, args[1:]...))
	}
	switch args[0] {
	case "issue":
		return runServiceCredentialIssueOrRotate(ctx, config, args[1:], ports, deps, false)
	case "rotate":
		return runServiceCredentialIssueOrRotate(ctx, config, args[1:], ports, deps, true)
	case "revoke":
		return runServiceCredentialRevoke(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown service credential subcommand %q", args[0]))
	}
}

func runServiceCredentialIssueOrRotate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps, rotate bool) int {
	subcmd := "issue"
	op := "service.credential.issue"
	if rotate {
		subcmd = "rotate"
		op = "service.credential.rotate"
	}
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", "credential", subcmd})
	}
	var (
		projectFlag        string
		ttlFlag            string
		ttlSecondsFlag     string
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&ttlFlag, "--ttl")
	spec.stringFlag(&ttlSecondsFlag, "--ttl-seconds")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, fmt.Sprintf("service credential %s requires <service-id>", subcmd))
	}
	serviceID := strings.TrimSpace(positionals[0])
	ttlSeconds, err := parseTTLSeconds(ttlFlag, ttlSecondsFlag)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	req := api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: ttlSeconds,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	if expectedVer == nil {
		ver, code := lookupServiceVersion(ctx, config, ports, deps, projectFlag, serviceID, "service credential "+subcmd)
		if code != ExitSuccess {
			return code
		}
		expectedVer = &ver
	}
	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, op, deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: op,
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var issued api.ServiceCredentialIssueResult
	if err := json.Unmarshal(result.Body, &issued); err != nil || issued.Version == 0 {
		return renderTableFallback(ports, result)
	}
	fmt.Fprintf(ports.Out, "service_id\t%s\nstate\t%s\nversion\t%d\n", serviceID, orDash(issued.State), issued.Version)
	if issued.Token != nil && *issued.Token != "" {
		fmt.Fprintf(ports.Out, "token\t%s\n", *issued.Token)
	}
	return ExitSuccess
}

func runServiceCredentialRevoke(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"service", "credential", "revoke"})
	}
	var (
		projectFlag        string
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "service credential revoke requires <service-id>")
	}
	serviceID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	req := api.ServiceCredentialRevokeRequest{ServiceID: serviceID}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	if expectedVer == nil {
		ver, code := lookupServiceVersion(ctx, config, ports, deps, projectFlag, serviceID, "service credential revoke")
		if code != ExitSuccess {
			return code
		}
		expectedVer = &ver
	}
	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "service.credential.revoke", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "service.credential.revoke",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var revoked api.ServiceCredentialRevokeResult
	_ = json.Unmarshal(result.Body, &revoked)
	invalidated := false
	if revoked.Invalidated != nil {
		invalidated = *revoked.Invalidated
	}
	fmt.Fprintf(ports.Out, "service_id\t%s\ninvalidated\t%t\nversion\t%d\n", serviceID, invalidated, revoked.Version)
	return ExitSuccess
}

func parseTTLSeconds(ttlFlag, ttlSecondsFlag string) (int, error) {
	ttlFlag = strings.TrimSpace(ttlFlag)
	ttlSecondsFlag = strings.TrimSpace(ttlSecondsFlag)
	if ttlFlag != "" && ttlSecondsFlag != "" {
		return 0, fmt.Errorf("specify only one of --ttl or --ttl-seconds")
	}
	if ttlSecondsFlag != "" {
		secs, err := strconv.Atoi(ttlSecondsFlag)
		if err != nil || secs <= 0 {
			return 0, fmt.Errorf("--ttl-seconds %q must be a positive integer", ttlSecondsFlag)
		}
		return secs, nil
	}
	if ttlFlag != "" {
		if secs, err := strconv.Atoi(ttlFlag); err == nil {
			if secs <= 0 {
				return 0, fmt.Errorf("--ttl %q must be positive", ttlFlag)
			}
			return secs, nil
		}
		dur, err := time.ParseDuration(ttlFlag)
		if err != nil || dur < time.Second || dur%time.Second != 0 {
			return 0, fmt.Errorf("--ttl %q must be a positive duration in whole seconds (e.g. 1h, 3600s, or 3600)", ttlFlag)
		}
		return int(dur / time.Second), nil
	}
	return defaultServiceCredentialTTLSeconds, nil
}

func fetchAllServices(ctx context.Context, ports Ports, projectID string) ([]api.ServiceSummaryView, json.RawMessage, int, error) {
	var (
		all       []api.ServiceSummaryView
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.ServiceListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, nil, pages, err
		}
		invocation := api.Invocation{
			Operation: "service.list",
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
		var page api.ServiceListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, nil, pages, fmt.Errorf("invalid service.list response: %w", err)
			}
		}
		all = append(all, page.Services...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, firstBody, pages, nil
}

func lookupServiceVersion(ctx context.Context, config Config, ports Ports, deps CommandDeps, projectFlag, serviceID, label string) (uint64, int) {
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return 0, usage(ports.Err, fmt.Sprintf("%s requires --expected-version <n> or --project <id> / KODEMA_PROJECT to look up version", label))
	}
	services, _, _, err := fetchAllServices(ctx, ports, projectID)
	if err != nil {
		return 0, failure(ports.Err, err)
	}
	for _, svc := range services {
		if svc.ServiceID == serviceID {
			if svc.Version == 0 {
				return 0, failure(ports.Err, fmt.Errorf("service %q returned no version; pass --expected-version", serviceID))
			}
			return svc.Version, ExitSuccess
		}
	}
	return 0, failure(ports.Err, fmt.Errorf("service %q not found in project %q", serviceID, projectID))
}

// runSecret dispatches `kodema secret <set|list|delete>`.
func runSecret(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "secret requires a subcommand (set|list|delete)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"secret"}, args[1:]...))
	}
	switch args[0] {
	case "set", "write":
		return runSecretSet(ctx, config, args[1:], ports, deps)
	case "list", "ls":
		return runSecretList(ctx, config, args[1:], ports, deps)
	case "delete", "rm":
		return runSecretDelete(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown secret subcommand %q", args[0]))
	}
}

func runSecretSet(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"secret", "set"})
	}
	var (
		projectFlag        string
		valueFlag          string
		valueFileFlag      string
		stdinFlag          bool
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&valueFlag, "--value")
	spec.stringFlag(&valueFileFlag, "--value-file")
	spec.boolFlag(&stdinFlag, "--stdin")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) < 1 || len(positionals) > 2 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "secret set requires <name> [<value> | --value <v> | --value-file <path> | --stdin]")
	}
	secretID := strings.TrimSpace(positionals[0])
	valueSources := 0
	if len(positionals) == 2 {
		valueSources++
	}
	if seen["--value"] {
		valueSources++
	}
	if seen["--value-file"] {
		valueSources++
	}
	if stdinFlag {
		valueSources++
	}
	if valueSources == 0 {
		return usage(ports.Err, "secret set requires a value via <value>, --value, --value-file, or --stdin")
	}
	if valueSources > 1 {
		return usage(ports.Err, "specify only one of positional <value>, --value, --value-file, or --stdin")
	}
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
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
		return usage(ports.Err, "secret set requires --project <id> or KODEMA_PROJECT")
	}

	var secretValue string
	switch {
	case len(positionals) == 2:
		secretValue = positionals[1]
	case seen["--value"]:
		secretValue = valueFlag
	case seen["--value-file"]:
		if ports.Files == nil {
			return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
		}
		data, readErr := ports.Files.ReadFile(valueFileFlag)
		if readErr != nil {
			return failure(ports.Err, fmt.Errorf("cannot read secret value file: %w", readErr))
		}
		secretValue = trimSingleTrailingNewline(string(data))
	case stdinFlag:
		if ports.In == nil {
			return failure(ports.Err, fmt.Errorf("stdin is not configured"))
		}
		data, readErr := io.ReadAll(io.LimitReader(ports.In, int64(api.MaxEventDataBytes)+2))
		if readErr != nil {
			return failure(ports.Err, fmt.Errorf("cannot read secret value from stdin: %w", readErr))
		}
		secretValue = trimSingleTrailingNewline(string(data))
	}

	req := api.SecretWriteRequest{
		ProjectID: projectID,
		SecretID:  secretID,
		Value:     secretValue,
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
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "secret.write", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "secret.write",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var written api.SecretWriteResult
	if err := json.Unmarshal(result.Body, &written); err != nil || written.SecretID == "" {
		return renderTableFallback(ports, result)
	}
	fmt.Fprintf(ports.Out, "secret_id\t%s\nname\t%s\nversion\t%d\n", written.SecretID, written.Name, written.Version)
	return ExitSuccess
}

func runSecretList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"secret", "list"})
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
		return usage(ports.Err, "secret list requires --project <id> or KODEMA_PROJECT")
	}
	if err := (api.SecretMetadataListRequest{ProjectID: projectID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	var (
		secrets   = []api.SecretMetadataView{}
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.SecretMetadataListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "secret.metadata.list",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return failure(ports.Err, err)
		}
		pages++
		if pages == 1 {
			firstBody = result.Body
		}
		var page api.SecretMetadataListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return failure(ports.Err, fmt.Errorf("invalid secret.metadata.list response: %w", err))
			}
		}
		secrets = append(secrets, page.Secrets...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "secret.metadata.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		combined, err := json.Marshal(api.SecretMetadataListResult{Secrets: secrets})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "secret.metadata.list", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SECRET\tNAME\tVERSION\tUPDATED_AT")
	for _, item := range secrets {
		updated := "-"
		if !item.UpdatedAt.IsZero() {
			updated = item.UpdatedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", orDash(item.SecretID), orDash(item.Name), item.Version, updated)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runSecretDelete(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"secret", "delete"})
	}
	var (
		projectFlag        string
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "secret delete requires <name>")
	}
	secretID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
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
		return usage(ports.Err, "secret delete requires --project <id> or KODEMA_PROJECT")
	}
	req := api.SecretDeleteRequest{
		ProjectID: projectID,
		SecretID:  secretID,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		readInput, err := json.Marshal(api.SecretMetadataReadRequest{
			ProjectID: projectID,
			SecretID:  secretID,
		})
		if err != nil {
			return failure(ports.Err, err)
		}
		readRes, err := ports.Client.Invoke(ctx, api.Invocation{
			Operation: "secret.metadata.read",
			Request:   api.Request{Input: readInput},
		})
		if err != nil {
			return failure(ports.Err, err)
		}
		var meta api.SecretMetadataReadResult
		if err := json.Unmarshal(readRes.Body, &meta); err != nil || meta.Version == 0 {
			return failure(ports.Err, fmt.Errorf("secret %q metadata returned no version; pass --expected-version", secretID))
		}
		ver := meta.Version
		expectedVer = &ver
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "secret.delete", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "secret.delete",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVer,
			IdempotencyKey:  key,
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
	var deleted api.SecretDeleteResult
	_ = json.Unmarshal(result.Body, &deleted)
	fmt.Fprintf(ports.Out, "deleted secret %s (version=%d)\n", secretID, deleted.Version)
	return ExitSuccess
}

func trimSingleTrailingNewline(value string) string {
	if strings.HasSuffix(value, "\n") {
		value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
	}
	return value
}
