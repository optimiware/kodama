package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/optimiware/kodema/api"
)

// runCancel cancels one running or queued execution via execution.cancel.
// When --expected-version is omitted, it inspects the execution first to read
// its current version and generates a fresh idempotency key.
func runCancel(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"cancel"})
	}
	var (
		expectedVerRaw     string
		idempotencyKeyFlag string
		outputFlag         string
	)
	spec := newArgSpec()
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "cancel requires <execution-id>")
	}
	executionID := strings.TrimSpace(positionals[0])
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
	req := api.ExecutionCancelRequest{ExecutionID: executionID}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		inspected, inspectErr := fetchExecutionInspect(ctx, ports, executionID)
		if inspectErr != nil {
			return failure(ports.Err, inspectErr)
		}
		if inspected.Version == 0 {
			return failure(ports.Err, fmt.Errorf("execution %q inspect returned no version; pass --expected-version", executionID))
		}
		ver := inspected.Version
		expectedVer = &ver
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "execution.cancel", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "execution.cancel",
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
	var cancelled api.ExecutionCancelResult
	_ = json.Unmarshal(result.Body, &cancelled)
	fmt.Fprintf(ports.Out, "cancelled execution %s (execution_version=%d)\n", executionID, cancelled.ExecutionVersion)
	return ExitSuccess
}

// runExecution dispatches `kodema execution <list|inspect|watch|cancel>`.
func runExecution(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "execution requires a subcommand (list|inspect|watch|cancel)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"execution"}, args[1:]...))
	}
	switch args[0] {
	case "list", "ls":
		return runExecutionList(ctx, config, args[1:], ports, deps)
	case "inspect":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"inspect"})
		}
		return runInspect(ctx, config, args[1:], ports)
	case "watch":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"watch"})
		}
		return runWatch(ctx, config, args[1:], ports)
	case "cancel":
		return runCancel(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown execution subcommand %q", args[0]))
	}
}

func runExecutionList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"execution", "list"})
	}
	var (
		projectFlag  string
		taskFlag     string
		pipelineFlag string
		stateFlag    string
		outputFlag   string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&taskFlag, "--task")
	spec.stringFlag(&pipelineFlag, "--pipeline")
	spec.stringFlag(&stateFlag, "--state", "--status")
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
		return usage(ports.Err, "execution list requires --project <id> or KODEMA_PROJECT")
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	pipelineID := strings.TrimSpace(pipelineFlag)
	if pipelineID != "" && !strings.HasPrefix(pipelineID, "pipeline-") {
		resolved, found, lookupErr := resolvePipelineIDByName(ctx, ports, projectID, pipelineID)
		if lookupErr == nil && found {
			pipelineID = resolved
		} else if strings.Contains(pipelineID, ".") {
			if lookupErr != nil {
				return failure(ports.Err, lookupErr)
			}
			return failure(ports.Err, fmt.Errorf("pipeline %q was not found in project %s", pipelineID, projectID))
		}
	}

	req := api.ExecutionListRequest{
		ProjectID:  projectID,
		TaskID:     strings.TrimSpace(taskFlag),
		PipelineID: pipelineID,
		State:      strings.TrimSpace(stateFlag),
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}

	executions := []api.ExecutionSummary{}
	var firstBody json.RawMessage
	pages := 0
	for {
		input, err := json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "execution.list",
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
		var page api.ExecutionListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return failure(ports.Err, fmt.Errorf("invalid execution.list response: %w", err))
			}
		}
		executions = append(executions, page.Executions...)
		if page.NextCursor == "" {
			break
		}
		req.Page = &api.Page{Cursor: page.NextCursor}
	}

	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "execution.list", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		body, err := json.Marshal(api.ExecutionListResult{Executions: executions})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "execution.list", Body: body}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EXECUTION\tSTATE\tVERSION\tTASK\tPIPELINE")
	for _, item := range executions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n",
			orDash(item.ExecutionID),
			orDash(item.State),
			item.Version,
			orDash(item.TaskID),
			orDash(item.PipelineID),
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// renderExecutionInspectTable writes a human-readable execution summary and
// one-row-per-action table without raw minified JSON inside TSV cells.
func renderExecutionInspectTable(out io.Writer, result api.Result) error {
	var inspected inspectedExecution
	if len(result.Body) == 0 || json.Unmarshal(result.Body, &inspected) != nil ||
		(inspected.ExecutionID == "" && len(inspected.Actions) == 0) {
		return renderTable(out, result)
	}
	summary := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if inspected.ExecutionID != "" {
		fmt.Fprintf(summary, "execution_id\t%s\n", inspected.ExecutionID)
	}
	if state := inspected.effectiveState(); state != "" {
		fmt.Fprintf(summary, "state\t%s\n", state)
	}
	if inspected.Version > 0 {
		fmt.Fprintf(summary, "version\t%d\n", inspected.Version)
	}
	if inspected.TaskID != "" {
		fmt.Fprintf(summary, "task_id\t%s\n", inspected.TaskID)
	}
	if inspected.PipelineID != "" {
		fmt.Fprintf(summary, "pipeline_id\t%s\n", inspected.PipelineID)
	}
	if inspected.StartedBy != "" {
		fmt.Fprintf(summary, "started_by\t%s\n", inspected.StartedBy)
	}
	if inspected.CreatedAt != nil && !inspected.CreatedAt.IsZero() {
		fmt.Fprintf(summary, "created_at\t%s\n", inspected.CreatedAt.UTC().Format(time.RFC3339))
	}
	if inspected.Trigger != nil {
		var parts []string
		if inspected.Trigger.EventID != "" {
			parts = append(parts, "event_id="+inspected.Trigger.EventID)
		}
		if inspected.Trigger.Repository != "" {
			parts = append(parts, "repository="+inspected.Trigger.Repository)
		}
		if inspected.Trigger.Ref != "" {
			parts = append(parts, "ref="+inspected.Trigger.Ref)
		}
		if inspected.Trigger.Commit != "" {
			parts = append(parts, "commit="+inspected.Trigger.Commit)
		}
		if inspected.Trigger.PreviousCommit != "" {
			parts = append(parts, "previous_commit="+inspected.Trigger.PreviousCommit)
		}
		if len(parts) > 0 {
			fmt.Fprintf(summary, "trigger\t%s\n", strings.Join(parts, " "))
		}
	}
	if len(inspected.Inputs) > 0 {
		keys := make([]string, 0, len(inspected.Inputs))
		for key := range inspected.Inputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var pairs []string
		for _, key := range keys {
			pairs = append(pairs, key+"="+singleLine(inspected.Inputs[key], 80))
		}
		fmt.Fprintf(summary, "inputs\t%s\n", strings.Join(pairs, " "))
	}
	if inspected.Cleanup.State != "" {
		cleanupDesc := inspected.Cleanup.State
		if inspected.Cleanup.LastError != "" {
			cleanupDesc += " (" + singleLine(inspected.Cleanup.LastError, 120) + ")"
		}
		fmt.Fprintf(summary, "cleanup\t%s\n", cleanupDesc)
	}
	if inspected.ModelUsage != nil && (inspected.ModelUsage.Requests > 0 || inspected.ModelUsage.TotalTokens > 0 || len(inspected.ModelUsage.ByModel) > 0) {
		fmt.Fprintf(summary, "model_usage\trequests=%d input_tokens=%d output_tokens=%d total_tokens=%d\n",
			inspected.ModelUsage.Requests,
			inspected.ModelUsage.InputTokens,
			inspected.ModelUsage.OutputTokens,
			inspected.ModelUsage.TotalTokens,
		)
	}
	if inspected.Failure != nil {
		var parts []string
		for _, part := range []string{inspected.Failure.Kind, inspected.Failure.Reason, inspected.Failure.Detail} {
			if strings.TrimSpace(part) != "" {
				parts = append(parts, singleLine(part, 120))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(summary, "failure\t%s\n", strings.Join(parts, ": "))
		}
	}
	if err := summary.Flush(); err != nil {
		return err
	}
	if len(inspected.Actions) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "INDEX\tACTION\tNAME\tKIND\tSTATE\tVERSION\tDETAIL")
		for _, action := range inspected.Actions {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%s\n",
				action.Index,
				orDash(action.ActionID),
				orDash(action.Name),
				orDash(action.Kind),
				orDash(action.effectiveState()),
				action.Version,
				formatInspectedActionDetail(action),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if inspected.ModelUsage != nil && len(inspected.ModelUsage.ByModel) > 0 {
		fmt.Fprintln(out)
		usageTable := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(usageTable, "PROVIDER\tMODEL\tREQUESTS\tINPUT_TOKENS\tOUTPUT_TOKENS\tTOTAL_TOKENS")
		for _, entry := range inspected.ModelUsage.ByModel {
			fmt.Fprintf(usageTable, "%s\t%s\t%d\t%d\t%d\t%d\n",
				orDash(entry.Provider),
				orDash(entry.Model),
				entry.Requests,
				entry.InputTokens,
				entry.OutputTokens,
				entry.TotalTokens,
			)
		}
		if err := usageTable.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func formatInspectedActionDetail(action inspectedAction) string {
	var parts []string
	if action.Assignment != "" {
		parts = append(parts, action.Assignment)
	}
	if action.Decision != "" {
		decision := "decision=" + action.Decision
		if action.Actor != "" {
			decision += " by " + action.Actor
		}
		parts = append(parts, decision)
	}
	if strings.TrimSpace(action.Note) != "" {
		parts = append(parts, fmt.Sprintf("note=%q", singleLine(action.Note, 120)))
	}
	if action.Subject != nil && len(action.Subject.Fields) > 0 {
		var fields []string
		for _, field := range action.Subject.Fields {
			fields = append(fields, field.Name+"="+singleLine(field.Value, 80))
		}
		parts = append(parts, "subject["+strings.Join(fields, " ")+"]")
	}
	if len(action.Outputs) > 0 {
		keys := make([]string, 0, len(action.Outputs))
		for key := range action.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var pairs []string
		for _, key := range keys {
			pairs = append(pairs, key+"="+singleLine(action.Outputs[key], 80))
		}
		parts = append(parts, "outputs["+strings.Join(pairs, " ")+"]")
	}
	if action.Stop != nil && action.Stop.State != "" {
		stopDesc := "stop=" + action.Stop.State
		if action.Stop.Detail != "" {
			stopDesc += "(" + singleLine(action.Stop.Detail, 60) + ")"
		}
		parts = append(parts, stopDesc)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}

// executionInspectFingerprint returns a canonical string capturing the
// observable state of an execution.inspect body so `watch` emits a snapshot
// only when version, state, action state/version/decision, failure or cleanup
// state actually changes.
func executionInspectFingerprint(body json.RawMessage) string {
	var inspected inspectedExecution
	if len(body) > 0 && json.Unmarshal(body, &inspected) == nil && inspected.Version > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "v=%d|s=%s|c=%d|cl=%s",
			inspected.Version,
			inspected.effectiveState(),
			inspected.CurrentAction,
			inspected.Cleanup.State,
		)
		if inspected.Failure != nil {
			fmt.Fprintf(&b, "|f=%s:%s:%s", inspected.Failure.Kind, inspected.Failure.Reason, inspected.Failure.Detail)
		}
		for _, act := range inspected.Actions {
			fmt.Fprintf(&b, "|a=%s:%s:%d:%s", act.ActionID, act.effectiveState(), act.Version, act.Decision)
		}
		return b.String()
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body) == nil {
		return compact.String()
	}
	return string(body)
}
