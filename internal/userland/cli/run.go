package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
)

// watchInterval is the deterministic delay between execution.inspect polls.
const watchInterval = time.Second

// Run executes one kodema command and returns its exit code. It never calls
// os.Exit; every collaborator is injected through ports. Run is the legacy
// entrypoint: the real OAuth login/logout/refresh commands need the runtime
// collaborators that RunWithDeps accepts.
func Run(ctx context.Context, config Config, args []string, ports Ports) int {
	return RunWithDeps(ctx, config, args, ports, CommandDeps{})
}

// RunWithDeps executes one kodema command with the optional command-runtime
// collaborators injected. It never calls os.Exit.
func RunWithDeps(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if ports.Out == nil {
		ports.Out = io.Discard
	}
	if ports.Err == nil {
		ports.Err = io.Discard
	}
	if ports.Now == nil {
		ports.Now = time.Now
	}
	if ports.Sleep == nil {
		ports.Sleep = time.Sleep
	}
	if ports.Client != nil {
		if _, wrapped := ports.Client.(projectScopeHintTransport); !wrapped {
			ports.Client = projectScopeHintTransport{
				inner:  ports.Client,
				config: config,
				ports:  ports,
				deps:   deps,
			}
		}
	}
	if len(args) == 0 {
		code := usage(ports.Err, "a command is required")
		fmt.Fprintln(ports.Err)
		writeCommandSummary(ports.Err)
		return code
	}
	switch args[0] {
	case "help", "--help", "-h":
		return runHelp(ports.Out, ports.Err, args[1:])
	case "call":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"call"})
		}
		return runCall(ctx, config, args[1:], ports, deps)
	case "inspect":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"inspect"})
		}
		return runInspect(ctx, config, args[1:], ports)
	case "events":
		return runEvents(ctx, config, args[1:], ports, deps)
	case "watch":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"watch"})
		}
		return runWatch(ctx, config, args[1:], ports, deps)
	case "cancel":
		return runCancel(ctx, config, args[1:], ports, deps)
	case "execution":
		return runExecution(ctx, config, args[1:], ports, deps)
	case "run":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"run"})
		}
		return runRun(ctx, config, args[1:], ports, deps)
	case "logs":
		return runLogs(ctx, config, args[1:], ports)
	case "inbox":
		return runInbox(ctx, config, args[1:], ports, deps)
	case "approve":
		return runApprove(ctx, config, args[1:], ports, deps)
	case "reject":
		return runReject(ctx, config, args[1:], ports, deps)
	case "revise":
		return runRevise(ctx, config, args[1:], ports, deps)
	case "task":
		return runTask(ctx, config, args[1:], ports, deps)
	case "service":
		return runService(ctx, config, args[1:], ports, deps)
	case "secret":
		return runSecret(ctx, config, args[1:], ports, deps)
	case "kv":
		return runKV(ctx, config, args[1:], ports, deps)
	case "whoami":
		return runWhoami(ctx, config, args[1:], ports, deps)
	case "member":
		return runMember(ctx, config, args[1:], ports, deps)
	case "project":
		return runProject(ctx, config, args[1:], ports, deps)
	case "repo", "repository":
		return runRepo(ctx, config, args[1:], ports, deps)
	case "mcp":
		return runMCP(ctx, config, args[1:], ports, deps)
	case "docs":
		return runDocs(ctx, config, args[1:], ports, deps)
	case "context":
		return runContext(ctx, config, args[1:], ports, deps)
	case "login":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"login"})
		}
		return runLogin(ctx, config, args[1:], ports, deps.Login)
	case "logout":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"logout"})
		}
		return runLogout(ctx, config, args[1:], ports, deps.Login)
	case "refresh":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"refresh"})
		}
		return runRefresh(ctx, config, args[1:], ports, deps.Login)
	case "session":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"session"})
		}
		return runSession(ctx, config, args[1:], ports, deps.Login)
	case "pipeline":
		return runPipeline(ctx, config, args[1:], ports, deps)
	case "git":
		return runGit(ctx, config, args[1:], ports, deps)
	case "output":
		return runOutput(ctx, config, args[1:], ports, deps)
	case "harness":
		return runHarness(ctx, config, args[1:], ports, deps)
	case "version":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"version"})
		}
		return usage(ports.Err, fmt.Sprintf("unknown command %q", args[0]))
	default:
		return usage(ports.Err, fmt.Sprintf("unknown command %q", args[0]))
	}
}

// runCall dispatches one served operation with optional input, version and key,
// or answers offline operation discovery (--list / --schema <op>).
func runCall(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		listFlag        bool
		schemaFlag      string
		inputFlag       string
		expectedVerRaw  string
		idempotencyFlag string
		projectFlag     string
		outputFlag      string
	)
	spec := newArgSpec()
	spec.boolFlag(&listFlag, "--list")
	spec.stringFlag(&schemaFlag, "--schema")
	spec.stringFlag(&inputFlag, "--input")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyFlag, "--idempotency-key")
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	if listFlag && seen["--schema"] {
		return usage(ports.Err, "specify only one of --list or --schema")
	}
	if listFlag {
		if len(positionals) > 0 {
			return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
		}
		return runCallList(ports, format)
	}
	if seen["--schema"] {
		op := strings.TrimSpace(schemaFlag)
		if op == "" && len(positionals) == 1 {
			op = strings.TrimSpace(positionals[0])
			positionals = nil
		}
		if len(positionals) > 0 {
			return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
		}
		if op == "" {
			return usage(ports.Err, "call --schema requires an operation name")
		}
		return runCallSchema(ports, op)
	}

	if len(positionals) == 0 {
		return usage(ports.Err, "call requires an operation (or --list / --schema <op>)")
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	operation := positionals[0]
	if _, ok := api.LookupRoute(operation); !ok {
		return usage(ports.Err, fmt.Sprintf("unknown operation %q (run 'kodema call --list' to see available operations)", operation))
	}

	var request api.Request
	if seen["--input"] {
		request.Input = json.RawMessage(inputFlag)
	}
	if seen["--expected-version"] {
		version, verErr := parsePositiveVersion(expectedVerRaw, "--expected-version")
		if verErr != nil {
			return usage(ports.Err, verErr.Error())
		}
		request.ExpectedVersion = version
	}
	if seen["--idempotency-key"] {
		request.IdempotencyKey = idempotencyFlag
	}

	if field := operationRouteParamField(operation, "project"); field != "" {
		if projectID := resolveProject(projectFlag, config, deps); projectID != "" {
			if updated, ok := injectDefaultStringField(request.Input, field, projectID); ok {
				request.Input = updated
			}
		}
	}

	invocation := api.Invocation{Operation: operation, Request: request}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if err := renderResult(ports.Out, format, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runCallList(ports Ports, format OutputFormat) int {
	type opSummary struct {
		Operation               string `json:"operation"`
		Method                  string `json:"method"`
		Path                    string `json:"path"`
		MCPTool                 string `json:"mcp_tool"`
		RequiresIdempotencyKey  bool   `json:"requires_idempotency_key"`
		RequiresExpectedVersion bool   `json:"requires_expected_version"`
	}
	codecs := make(map[string]api.OperationCodec)
	for _, codec := range api.OperationCodecs() {
		codecs[codec.Operation] = codec
	}
	routes := api.Routes()
	items := make([]opSummary, 0, len(routes))
	for _, route := range routes {
		codec := codecs[route.Operation]
		items = append(items, opSummary{
			Operation:               route.Operation,
			Method:                  string(route.Method),
			Path:                    route.Path,
			MCPTool:                 route.MCPTool,
			RequiresIdempotencyKey:  codec.RequiresIdempotencyKey,
			RequiresExpectedVersion: codec.RequiresExpectedVersion,
		})
	}
	if format == OutputJSON {
		payload := struct {
			Operations []opSummary `json:"operations"`
		}{Operations: items}
		if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "OPERATION\tMETHOD\tPATH\tFLAGS")
	for _, item := range items {
		var flags []string
		if item.RequiresIdempotencyKey {
			flags = append(flags, "idempotency-key")
		}
		if item.RequiresExpectedVersion {
			flags = append(flags, "expected-version")
		}
		flagText := "-"
		if len(flags) > 0 {
			flagText = strings.Join(flags, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.Operation, item.Method, item.Path, flagText)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runCallSchema(ports Ports, operation string) int {
	raw, err := api.OperationInputSchema(operation)
	if err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		if _, werr := ports.Out.Write(append(raw, '\n')); werr != nil {
			return failure(ports.Err, werr)
		}
		return ExitSuccess
	}
	pretty.WriteByte('\n')
	if _, err := ports.Out.Write(pretty.Bytes()); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func operationRouteParamField(operation, paramName string) string {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation != operation {
			continue
		}
		for _, p := range codec.RouteParams {
			if p.Name == paramName {
				return p.Field
			}
		}
	}
	return ""
}

// injectDefaultStringField fills a top-level string field into an empty or
// valid JSON object input when the field is absent. Invalid JSON or non-object
// inputs are returned unchanged so normal validation reports the error.
func injectDefaultStringField(raw json.RawMessage, field, value string) (json.RawMessage, bool) {
	if field == "" || strings.Contains(field, ".") || value == "" {
		return raw, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		encoded, err := json.Marshal(map[string]string{field: value})
		if err != nil {
			return raw, false
		}
		return encoded, true
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return raw, false
	}
	if _, exists := object[field]; exists {
		return raw, false
	}
	encodedVal, err := json.Marshal(value)
	if err != nil {
		return raw, false
	}
	object[field] = encodedVal
	encoded, err := json.Marshal(object)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

// runInspect reads one execution once and renders it.
func runInspect(ctx context.Context, config Config, args []string, ports Ports) int {
	var outputFlag string
	spec := newArgSpec()
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || positionals[0] == "" {
		return usage(ports.Err, "inspect requires exactly one execution")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	invocation, err := executionInvocation(positionals[0])
	if err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputTable {
		if err := renderExecutionInspectTable(ports.Out, result); err != nil {
			return failure(ports.Err, err)
		}
		var inspected inspectedExecution
		if len(result.Body) > 0 && json.Unmarshal(result.Body, &inspected) == nil && inspected.effectiveState() == "failed" {
			executionID := inspected.ExecutionID
			if executionID == "" {
				executionID = strings.TrimSpace(positionals[0])
			}
			_ = printFailedActionStderr(ctx, ports, executionID, inspected.Actions)
		}
		return ExitSuccess
	}
	if err := renderResult(ports.Out, OutputJSON, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// runWatch either polls execution.inspect for one execution ID until a terminal
// status (preserving single-execution behavior and failed-action stderr tails),
// or—when --project / --until is supplied without an execution ID—watches a
// project's live event stream.
func runWatch(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		projectFlag string
		untilFlag   string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&untilFlag, "--until")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
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

	until := strings.TrimSpace(untilFlag)
	if seen["--until"] && until != "request" {
		return usage(ports.Err, fmt.Sprintf("unknown --until condition %q; want request", until))
	}

	if len(positionals) > 1 {
		return usage(ports.Err, "watch requires exactly one execution or --project <id>")
	}

	explicitProject := seen["--project"] || seen["-p"] || deps.ExplicitProject
	if len(positionals) == 0 {
		if !explicitProject && !seen["--until"] {
			return usage(ports.Err, "watch requires exactly one execution or --project <id>")
		}
		projectID := resolveProject(projectFlag, config, deps)
		if projectID == "" {
			return usage(ports.Err, "watch --project requires a project ID (or KODEMA_PROJECT)")
		}
		if ports.Client == nil {
			return failure(ports.Err, fmt.Errorf("no public client is configured"))
		}
		return runProjectWatch(ctx, ports, projectID, until, format)
	}

	if positionals[0] == "" {
		return usage(ports.Err, "watch requires exactly one execution")
	}
	if seen["--until"] {
		return usage(ports.Err, "--until is only supported for project-wide watch (omit <execution-id> and pass --project <id>)")
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	invocation, err := executionInvocation(positionals[0])
	if err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	var (
		lastFingerprint string
		rendered        bool
		settling        int
	)
	for {
		if ctx.Err() != nil {
			return watchCanceled(ports.Err)
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			if ctx.Err() != nil {
				return watchCanceled(ports.Err)
			}
			return failure(ports.Err, err)
		}
		fp := executionInspectFingerprint(result.Body)
		if !rendered || fp != lastFingerprint {
			if format == OutputTable {
				if rendered {
					fmt.Fprintln(ports.Out)
				}
				if err := renderExecutionInspectTable(ports.Out, result); err != nil {
					return failure(ports.Err, err)
				}
			} else {
				if err := renderResult(ports.Out, OutputJSON, result); err != nil {
					return failure(ports.Err, err)
				}
			}
			lastFingerprint = fp
			rendered = true
		}
		status, ok := executionStatus(result.Body)
		if !ok {
			return failure(ports.Err, fmt.Errorf("execution status is missing or unknown"))
		}
		switch status {
		case "succeeded":
			return ExitSuccess
		case "failed":
			if format == OutputTable {
				var inspected inspectedExecution
				if len(result.Body) > 0 && json.Unmarshal(result.Body, &inspected) == nil {
					if hasFailedContainerAction(inspected.Actions) && logsSettling(inspected, &settling) {
						ports.Sleep(watchInterval)
						continue
					}
					executionID := inspected.ExecutionID
					if executionID == "" {
						executionID = strings.TrimSpace(positionals[0])
					}
					if err := printFailedActionStderr(ctx, ports, executionID, inspected.Actions); err != nil && ctx.Err() != nil {
						return watchCanceled(ports.Err)
					}
				}
			}
			fmt.Fprintf(ports.Err, "watch failed: execution status %s\n", status)
			return ExitFailure
		case "cancelled":
			fmt.Fprintf(ports.Err, "watch failed: execution status %s\n", status)
			return ExitFailure
		case "queued", "running":
			ports.Sleep(watchInterval)
		default:
			return failure(ports.Err, fmt.Errorf("execution status %q is unknown", status))
		}
	}
}

// runLogin parses the login flags and runs the configured OAuth runtime, or
// keeps the seeded-session compatibility path when no runtime was
// injected. The compatibility path only re-persists an already-current human
// credential; the real flow discovers the API-bound provider configuration and
// obtains a fresh version 2 session.
func runLogin(ctx context.Context, config Config, args []string, ports Ports, runtime LoginRuntime) int {
	forceDevice, err := parseLoginFlags(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if !loginRuntimeConfigured(runtime) {
		if len(args) != 0 {
			return usage(ports.Err, "login takes no arguments")
		}
		return runLegacyLogin(ctx, config, ports)
	}
	return runConfiguredLogin(ctx, config, ports, runtime, forceDevice)
}

// runLegacyLogin reads a fresh human credential and persists it as the durable
// session. It is the entrypoint compatibility path; a source, kind,
// expiry or save failure writes nothing to stdout.
func runLegacyLogin(ctx context.Context, config Config, ports Ports) int {
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Credential == nil {
		return failure(ports.Err, fmt.Errorf("no credential source is configured"))
	}
	credential, err := ports.Credential.Credential(ctx)
	if err != nil {
		return failure(ports.Err, err)
	}
	if credential.Kind != client.KindSession {
		return failure(ports.Err, fault.New(fault.Unauthenticated, "login requires a human session credential"))
	}
	if !credential.Usable(ports.Now()) {
		return failure(ports.Err, fault.New(fault.Unauthenticated, "the session is missing, expired or unusable"))
	}
	if ports.Session == nil {
		return failure(ports.Err, fmt.Errorf("no session store is configured"))
	}
	if err := ports.Session.Save(ctx, credential); err != nil {
		return failure(ports.Err, err)
	}
	fmt.Fprintln(ports.Out, "logged in")
	return ExitSuccess
}

// executionInvocation builds the execution.inspect request.
func executionInvocation(execution string) (api.Invocation, error) {
	input, err := json.Marshal(map[string]string{"execution_id": execution})
	if err != nil {
		return api.Invocation{}, err
	}
	invocation := api.Invocation{
		Operation: "execution.inspect",
		Request:   api.Request{Input: input},
	}
	if err := invocation.Validate(); err != nil {
		return api.Invocation{}, err
	}
	return invocation, nil
}

// executionStatus reads the top-level status string from an execution body.
func executionStatus(body json.RawMessage) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false
	}
	raw, ok := fields["status"]
	if !ok {
		raw, ok = fields["state"]
	}
	if !ok {
		return "", false
	}
	var status string
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", false
	}
	return status, true
}

// validateTrustedOrigins checks only the origins a helper invocation needs.
func validateTrustedOrigins(config Config) error {
	for _, raw := range config.TrustedOrigins {
		if _, err := ParseOrigin(raw); err != nil {
			return err
		}
	}
	return nil
}

// watchCanceled reports a canceled watch without rendering a success.
func watchCanceled(errOut io.Writer) int {
	fmt.Fprintln(errOut, "watch canceled")
	return ExitFailure
}

// usage writes a usage diagnostic and returns the usage exit code.
func usage(errOut io.Writer, message string) int {
	fmt.Fprintf(errOut, "usage error: %s\n", message)
	return ExitUsage
}

// failure writes a public diagnostic and optional client-side hint to errOut
// and returns the failure exit code.
func failure(errOut io.Writer, err error) int {
	fmt.Fprintf(errOut, "error: %v\n", err)
	if hint := errorHint(err); hint != "" {
		fmt.Fprintf(errOut, "hint: %s\n", hint)
	}
	return ExitFailure
}

// publicMessage returns the public message of an api.Error or the error text.
func publicMessage(err error) string {
	if public, ok := errors.AsType[*api.Error](err); ok {
		return public.Message
	}
	return err.Error()
}
