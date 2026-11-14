package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodama/api"
)

// runInbox queries task.inbox and renders a human-readable table or raw JSON.
func runInbox(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"inbox"})
	}
	var (
		projectFlag string
		outputFlag  string
		kindFlags   []string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&outputFlag, "--output")
	spec.listFlag(&kindFlags, "--kind")
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
		return usage(ports.Err, "inbox requires --project <id> or KODAMA_PROJECT")
	}
	var kinds []string
	for _, raw := range kindFlags {
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				kinds = append(kinds, trimmed)
			}
		}
	}
	probe := api.TaskInboxRequest{ProjectID: projectID, Kinds: kinds}
	if err := probe.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	var (
		allItems  []api.InboxItem
		firstBody json.RawMessage
		pages     int
		cursor    string
	)
	for {
		req := api.TaskInboxRequest{ProjectID: projectID, Kinds: kinds}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "task.inbox",
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
		var page api.TaskInboxResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return failure(ports.Err, fmt.Errorf("invalid task.inbox response: %w", err))
			}
		}
		allItems = append(allItems, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if format == OutputJSON {
		if pages == 1 {
			if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "task.inbox", Body: firstBody}); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		if allItems == nil {
			allItems = []api.InboxItem{}
		}
		combined, err := json.Marshal(api.TaskInboxResult{Items: allItems})
		if err != nil {
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, OutputJSON, api.Result{Operation: "task.inbox", Body: combined}); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTASK\tEXECUTION\tACTION\tPROMPT/SUMMARY")
	for _, item := range allItems {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			orDash(item.Kind),
			orDash(item.TaskID),
			orDash(item.ExecutionID),
			orDash(item.ActionID),
			summarizeInboxItem(item),
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func summarizeInboxItem(item api.InboxItem) string {
	switch item.Kind {
	case "waiting_task":
		summary := singleLine(item.Question, 96)
		if item.Assignment != "" {
			return fmt.Sprintf("[%s] %s", item.Assignment, summary)
		}
		return summary
	case "request":
		var parts []string
		if item.Subject != nil {
			for _, field := range item.Subject.Fields {
				parts = append(parts, field.Name+"="+singleLine(field.Value, 40))
			}
		}
		summary := strings.Join(parts, " ")
		if summary == "" {
			summary = "pending request approval"
		}
		if item.Assignment != "" {
			return fmt.Sprintf("[%s] %s", item.Assignment, summary)
		}
		return summary
	case "failed_task":
		if item.Assignment != "" {
			return fmt.Sprintf("[%s] task failed", item.Assignment)
		}
		return "task failed"
	default:
		if item.Question != "" {
			return singleLine(item.Question, 96)
		}
		return "-"
	}
}

func singleLine(value string, maxRunes int) string {
	collapsed := strings.Join(strings.Fields(value), " ")
	if collapsed == "" {
		return "-"
	}
	runes := []rune(collapsed)
	if maxRunes > 3 && len(runes) > maxRunes {
		return string(runes[:maxRunes-3]) + "..."
	}
	return collapsed
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// runApprove resolves a waiting request action with decision "pass".
func runApprove(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"approve"})
	}
	return runResolveDecision(ctx, config, args, ports, deps, true)
}

// runReject resolves a waiting request action with decision "fail".
func runReject(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"reject"})
	}
	return runResolveDecision(ctx, config, args, ports, deps, false)
}

func runResolveDecision(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps, approve bool) int {
	var (
		actionFlag         string
		commentFlag        string
		reasonFlag         string
		expectedVerRaw     string
		expectedActVerRaw  string
		idempotencyKeyFlag string
		rawOutputs         []string
		outputPairs        []string
	)
	spec := newArgSpec()
	spec.stringFlag(&actionFlag, "--action", "-a")
	if approve {
		spec.stringFlag(&commentFlag, "--comment", "-m")
	} else {
		spec.stringFlag(&reasonFlag, "--reason", "-r")
	}
	spec.listFlag(&rawOutputs, "--output")
	spec.listFlag(&outputPairs, "--response-output", "--action-output")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&expectedActVerRaw, "--expected-action-version")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	cmdName := "approve"
	if !approve {
		cmdName = "reject"
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, fmt.Sprintf("%s requires <execution-id>", cmdName))
	}
	executionID := strings.TrimSpace(positionals[0])
	if !approve && strings.TrimSpace(reasonFlag) == "" {
		return usage(ports.Err, "reject requires --reason <text>")
	}
	outputFlag := ""
	for _, raw := range rawOutputs {
		if !strings.Contains(raw, "=") {
			outputFlag = raw
			continue
		}
		outputPairs = append(outputPairs, raw)
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedExecVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedActionVer, err := parsePositiveVersion(expectedActVerRaw, "--expected-action-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	outputs, err := parseKeyValues(outputPairs, "output")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	actionID := strings.TrimSpace(actionFlag)
	var actionVersion uint64
	if expectedActionVer != nil {
		actionVersion = *expectedActionVer
	}

	if actionID == "" || actionVersion == 0 {
		inspected, err := fetchExecutionInspect(ctx, ports, executionID)
		if err != nil {
			return failure(ports.Err, err)
		}
		if expectedExecVer == nil && inspected.Version > 0 {
			ver := inspected.Version
			expectedExecVer = &ver
		}
		selected, err := selectWaitingRequestAction(inspected, actionID)
		if err != nil {
			return failure(ports.Err, err)
		}
		actionID = selected.ActionID
		if actionVersion == 0 {
			actionVersion = selected.Version
			if actionVersion == 0 {
				actionVersion = 1
			}
		}
	}

	implicitKey := ""
	if approve && strings.TrimSpace(commentFlag) != "" {
		if outputs == nil {
			outputs = make(map[string]string, 1)
		}
		if _, exists := outputs["comment"]; !exists {
			outputs["comment"] = commentFlag
			implicitKey = "comment"
		}
	}
	if !approve {
		if outputs == nil {
			outputs = make(map[string]string, 1)
		}
		if _, exists := outputs["reason"]; !exists {
			outputs["reason"] = reasonFlag
			implicitKey = "reason"
		}
	}

	decision := "pass"
	if !approve {
		decision = "fail"
	}

	req := api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              actionID,
		ExpectedActionVersion: actionVersion,
		Decision:              decision,
		Outputs:               outputs,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}

	result, err := invokeResolveRequest(ctx, ports, deps, req, expectedExecVer, idempotencyKeyFlag)
	if err != nil && implicitKey != "" && idempotencyKeyFlag == "" &&
		strings.Contains(err.Error(), fmt.Sprintf("response key %q is not declared", implicitKey)) {
		// If the pipeline's request schema declared no comment/reason output key,
		// retry once without the undeclared key so standard approval gates work.
		delete(outputs, implicitKey)
		if len(outputs) == 0 {
			outputs = nil
		}
		req.Outputs = outputs
		result, err = invokeResolveRequest(ctx, ports, deps, req, expectedExecVer, "")
	}
	if err != nil {
		return failure(ports.Err, err)
	}

	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	var resolved api.ExecutionRequestResolveResult
	_ = json.Unmarshal(result.Body, &resolved)
	if approve {
		fmt.Fprintf(ports.Out, "approved execution %s action %s (decision=%s, execution_version=%d, action_version=%d)\n",
			executionID, actionID, decision, resolved.ExecutionVersion, resolved.ActionVersion)
	} else {
		fmt.Fprintf(ports.Out, "rejected execution %s action %s (decision=%s, reason=%q, execution_version=%d, action_version=%d)\n",
			executionID, actionID, decision, reasonFlag, resolved.ExecutionVersion, resolved.ActionVersion)
	}
	return ExitSuccess
}

func invokeResolveRequest(
	ctx context.Context,
	ports Ports,
	deps CommandDeps,
	req api.ExecutionRequestResolveRequest,
	expectedExecVer *uint64,
	explicitKey string,
) (api.Result, error) {
	input, err := json.Marshal(req)
	if err != nil {
		return api.Result{}, err
	}
	key, err := makeIdempotencyKey(explicitKey, "execution.request.resolve", deps.Login.Random)
	if err != nil {
		return api.Result{}, err
	}
	invocation := api.Invocation{
		Operation: "execution.request.resolve",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedExecVer,
			IdempotencyKey:  key,
		},
	}
	if err := invocation.Validate(); err != nil {
		return api.Result{}, err
	}
	return ports.Client.Invoke(ctx, invocation)
}

func selectWaitingRequestAction(inspected inspectedExecution, explicitAction string) (inspectedAction, error) {
	if explicitAction != "" {
		for _, action := range inspected.Actions {
			if action.ActionID == explicitAction || action.Name == explicitAction {
				if action.Kind != "" && action.Kind != "request" {
					return inspectedAction{}, fmt.Errorf("action %q is %q, not a request action", explicitAction, action.Kind)
				}
				state := action.effectiveState()
				if (state != "" && state != "running" && state != "waiting") || action.Decision != "" {
					return inspectedAction{}, fmt.Errorf("request action %q is not currently waiting (state=%q)", explicitAction, state)
				}
				return action, nil
			}
		}
		if len(inspected.Actions) == 0 {
			return inspectedAction{ActionID: explicitAction, Kind: "request", State: "running", Version: 1}, nil
		}
		return inspectedAction{}, fmt.Errorf("action %q is not part of execution %q", explicitAction, inspected.ExecutionID)
	}

	var waiting []inspectedAction
	for _, action := range inspected.Actions {
		state := action.effectiveState()
		if action.Kind == "request" && (state == "running" || state == "waiting") && action.Decision == "" {
			waiting = append(waiting, action)
		}
	}
	switch len(waiting) {
	case 0:
		return inspectedAction{}, fmt.Errorf("execution %q has no waiting request action", inspected.ExecutionID)
	case 1:
		return waiting[0], nil
	default:
		ids := make([]string, len(waiting))
		for i, act := range waiting {
			ids[i] = act.ActionID
		}
		return inspectedAction{}, fmt.Errorf(
			"execution %q has %d waiting request actions (%s); specify --action <id>",
			inspected.ExecutionID, len(waiting), strings.Join(ids, ", "),
		)
	}
}
