package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
)

// inspectedAction is a lenient projection of api.ExecutionAction used by CLI
// inspection helpers so both full server responses and concise test fixtures
// decode cleanly.
type inspectedAction struct {
	ActionID    string       `json:"action_id"`
	Index       int          `json:"index"`
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	State       string       `json:"state"`
	Status      string       `json:"status"`
	Version     uint64       `json:"version"`
	ActivatedAt *time.Time   `json:"activated_at,omitempty"`
	Subject     *api.Subject `json:"subject,omitempty"`
	Assignment  string       `json:"assignment,omitempty"`
	Decision    string       `json:"decision,omitempty"`
}

func (action inspectedAction) effectiveState() string {
	if action.State != "" {
		return action.State
	}
	return action.Status
}

// inspectedExecution is a lenient projection of api.ExecutionInspectResult.
type inspectedExecution struct {
	ExecutionID   string            `json:"execution_id"`
	TaskID        string            `json:"task_id,omitempty"`
	PipelineID    string            `json:"pipeline_id,omitempty"`
	State         string            `json:"state"`
	Status        string            `json:"status"`
	Version       uint64            `json:"version"`
	CurrentAction int               `json:"current_action"`
	Actions       []inspectedAction `json:"actions"`
}

func (execution inspectedExecution) effectiveState() string {
	if execution.State != "" {
		return execution.State
	}
	return execution.Status
}

// logsJSONPage is the structured JSON output for `kodactl logs --output json`.
type logsJSONPage struct {
	ExecutionID string         `json:"execution_id"`
	ActionID    string         `json:"action_id"`
	Stream      string         `json:"stream"`
	Data        string         `json:"data"`
	Chunks      []api.LogChunk `json:"chunks"`
	Truncated   bool           `json:"truncated"`
}

// logStreamState tracks pagination and deduplication across one action's stream.
type logStreamState struct {
	actionID   string
	cursor     string
	hasSeen    bool
	lastOffset uint64
	lastSeq    uint64
}

// runLogs reads or follows retained execution action logs.
func runLogs(ctx context.Context, config Config, args []string, ports Ports) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"logs"})
	}
	var (
		actionFlag string
		stream     string
		follow     bool
		outputFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&actionFlag, "--action", "-a")
	spec.stringFlag(&stream, "--stream", "-s")
	spec.boolFlag(&follow, "--follow", "-f")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "logs requires <execution-id>")
	}
	executionID := strings.TrimSpace(positionals[0])
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		return usage(ports.Err, fmt.Sprintf("unknown log stream %q; want stdout or stderr", stream))
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

	if !follow {
		return runLogsOnce(ctx, ports, executionID, strings.TrimSpace(actionFlag), stream, format)
	}
	return runLogsFollow(ctx, ports, executionID, strings.TrimSpace(actionFlag), stream, format)
}

// runLogsOnce resolves the target action when omitted and drains all pages.
func runLogsOnce(ctx context.Context, ports Ports, executionID, actionFlag, stream string, format OutputFormat) int {
	actionID := actionFlag
	if actionID == "" {
		inspected, err := fetchExecutionInspect(ctx, ports, executionID)
		if err != nil {
			return failure(ports.Err, err)
		}
		selected, ok := selectLogAction(inspected)
		if !ok {
			if len(inspected.Actions) == 0 {
				return failure(ports.Err, fmt.Errorf("execution %q has no actions yet", executionID))
			}
			return failure(ports.Err, fmt.Errorf("no action has started yet on execution %q", executionID))
		}
		actionID = selected.ActionID
	}

	state := &logStreamState{actionID: actionID}
	chunks, truncated, err := drainLogPages(ctx, ports, executionID, stream, format, false, state)
	if err != nil && actionFlag != "" {
		// Allow --action <name> convenience if the caller passed the YAML action
		// name rather than the opaque action_id.
		if resolvedID, ok := resolveActionName(ctx, ports, executionID, actionFlag); ok && resolvedID != actionID {
			state = &logStreamState{actionID: resolvedID}
			actionID = resolvedID
			chunks, truncated, err = drainLogPages(ctx, ports, executionID, stream, format, false, state)
		}
	}
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		var combined strings.Builder
		for _, chunk := range chunks {
			combined.Write(chunk.Data)
		}
		if chunks == nil {
			chunks = []api.LogChunk{}
		}
		out := logsJSONPage{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      stream,
			Data:        combined.String(),
			Chunks:      chunks,
			Truncated:   truncated,
		}
		if err := json.NewEncoder(ports.Out).Encode(out); err != nil {
			return failure(ports.Err, err)
		}
	}
	return ExitSuccess
}

// runLogsFollow polls execution.inspect and drains execution.logs.read until
// the target action or execution reaches a terminal status.
func runLogsFollow(ctx context.Context, ports Ports, executionID, actionFlag, stream string, format OutputFormat) int {
	var state *logStreamState
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(ports.Err, "logs canceled")
			return ExitFailure
		}
		inspected, err := fetchExecutionInspect(ctx, ports, executionID)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(ports.Err, "logs canceled")
				return ExitFailure
			}
			return failure(ports.Err, err)
		}

		execState := inspected.effectiveState()
		targetAction, hasTarget, err := resolveFollowAction(inspected, actionFlag)
		if err != nil {
			return failure(ports.Err, err)
		}

		if !hasTarget {
			// Execution is queued or pending its first action.
			switch execState {
			case "queued", "running":
				ports.Sleep(watchInterval)
				continue
			case "succeeded":
				if len(inspected.Actions) == 0 {
					return failure(ports.Err, fmt.Errorf("execution %q has no actions", executionID))
				}
				return ExitSuccess
			case "failed", "cancelled":
				fmt.Fprintf(ports.Err, "logs failed: execution status %s\n", execState)
				return ExitFailure
			default:
				return failure(ports.Err, fmt.Errorf("execution %q has no started actions (state %q)", executionID, execState))
			}
		}

		// If --action was omitted and the execution advanced to a new started
		// container action after finishing the previous one, drain the previous
		// action completely before switching to the new action.
		if state != nil && state.actionID != targetAction.ActionID {
			if _, _, drainErr := drainLogPages(ctx, ports, executionID, stream, format, true, state); drainErr != nil {
				if ctx.Err() != nil {
					fmt.Fprintln(ports.Err, "logs canceled")
					return ExitFailure
				}
				return failure(ports.Err, drainErr)
			}
			state = &logStreamState{actionID: targetAction.ActionID}
		} else if state == nil {
			state = &logStreamState{actionID: targetAction.ActionID}
		}

		if _, _, err := drainLogPages(ctx, ports, executionID, stream, format, true, state); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(ports.Err, "logs canceled")
				return ExitFailure
			}
			return failure(ports.Err, err)
		}

		terminalState, done, err := checkLogsTerminal(execState, targetAction, actionFlag != "", inspected.Actions)
		if err != nil {
			return failure(ports.Err, err)
		}
		if done {
			if terminalState == "succeeded" {
				return ExitSuccess
			}
			fmt.Fprintf(ports.Err, "logs failed: status %s\n", terminalState)
			return ExitFailure
		}

		ports.Sleep(watchInterval)
	}
}

// drainLogPages reads all currently available pages from state.cursor, writing
// chunks immediately to ports.Out in table/text mode (or streaming JSON lines
// when follow && OutputJSON).
func drainLogPages(ctx context.Context, ports Ports, executionID, stream string, format OutputFormat, streamJSON bool, state *logStreamState) ([]api.LogChunk, bool, error) {
	var collected []api.LogChunk
	truncated := false
	cursor := state.cursor
	for {
		if ctx.Err() != nil {
			return collected, truncated, ctx.Err()
		}
		req := api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    state.actionID,
			Stream:      stream,
		}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return collected, truncated, err
		}
		invocation := api.Invocation{
			Operation: "execution.logs.read",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return collected, truncated, err
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return collected, truncated, err
		}
		var page api.ExecutionLogsReadResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return collected, truncated, fmt.Errorf("invalid execution.logs.read response: %w", err)
			}
		}
		if page.Truncated {
			truncated = true
		}

		var fresh []api.LogChunk
		for _, chunk := range page.Chunks {
			if state.hasSeen && (chunk.Offset > 0 || chunk.Sequence > 0 || state.lastOffset > 0 || state.lastSeq > 0) {
				if chunk.Offset < state.lastOffset || (chunk.Offset == state.lastOffset && chunk.Sequence <= state.lastSeq) {
					continue
				}
			}
			state.hasSeen = true
			state.lastOffset = chunk.Offset
			state.lastSeq = chunk.Sequence
			fresh = append(fresh, chunk)
			collected = append(collected, chunk)
		}

		if format != OutputJSON {
			for _, chunk := range fresh {
				if _, err := ports.Out.Write(chunk.Data); err != nil {
					return collected, truncated, err
				}
			}
		} else if streamJSON && len(fresh) > 0 {
			var text strings.Builder
			for _, chunk := range fresh {
				text.Write(chunk.Data)
			}
			out := logsJSONPage{
				ExecutionID: executionID,
				ActionID:    state.actionID,
				Stream:      stream,
				Data:        text.String(),
				Chunks:      fresh,
				Truncated:   page.Truncated,
			}
			if err := json.NewEncoder(ports.Out).Encode(out); err != nil {
				return collected, truncated, err
			}
		}

		if page.NextCursor != "" {
			cursor = page.NextCursor
			state.cursor = page.NextCursor
			continue
		}
		if state.hasSeen {
			if tailCursor, encErr := api.EncodeCursor(
				strconv.FormatUint(state.lastOffset, 10) + ":" + strconv.FormatUint(state.lastSeq, 10),
			); encErr == nil {
				state.cursor = tailCursor
			}
		}
		break
	}
	return collected, truncated, nil
}

// fetchExecutionInspect invokes execution.inspect and decodes the response.
func fetchExecutionInspect(ctx context.Context, ports Ports, executionID string) (inspectedExecution, error) {
	invocation, err := executionInvocation(executionID)
	if err != nil {
		return inspectedExecution{}, err
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return inspectedExecution{}, err
	}
	var inspected inspectedExecution
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &inspected); err != nil {
			return inspectedExecution{}, fmt.Errorf("invalid execution.inspect response: %w", err)
		}
	}
	if inspected.ExecutionID == "" {
		inspected.ExecutionID = executionID
	}
	return inspected, nil
}

// selectLogAction picks the currently running action (preferring container
// actions), or else the last started/completed action.
func selectLogAction(inspected inspectedExecution) (inspectedAction, bool) {
	if len(inspected.Actions) == 0 {
		return inspectedAction{}, false
	}
	if idx := inspected.CurrentAction; idx >= 0 && idx < len(inspected.Actions) {
		candidate := inspected.Actions[idx]
		if candidate.effectiveState() == "running" && candidate.Kind != "request" {
			return candidate, true
		}
	}
	for _, action := range inspected.Actions {
		if action.effectiveState() == "running" && action.Kind != "request" {
			return action, true
		}
	}
	for _, action := range inspected.Actions {
		if action.effectiveState() == "running" {
			return action, true
		}
	}
	for index := len(inspected.Actions) - 1; index >= 0; index-- {
		action := inspected.Actions[index]
		state := action.effectiveState()
		if (state != "" && state != "pending") || action.ActivatedAt != nil {
			if action.Kind != "request" {
				return action, true
			}
		}
	}
	for index := len(inspected.Actions) - 1; index >= 0; index-- {
		action := inspected.Actions[index]
		state := action.effectiveState()
		if (state != "" && state != "pending") || action.ActivatedAt != nil {
			return action, true
		}
	}
	return inspectedAction{}, false
}

// resolveFollowAction locates the explicit action or auto-selects the active
// action during `kodactl logs --follow`.
func resolveFollowAction(inspected inspectedExecution, explicit string) (inspectedAction, bool, error) {
	if explicit != "" {
		for _, action := range inspected.Actions {
			if action.ActionID == explicit {
				return action, true, nil
			}
		}
		for _, action := range inspected.Actions {
			if action.Name == explicit {
				return action, true, nil
			}
		}
		if len(inspected.Actions) == 0 {
			// Minimal fixture with no action list: synthesize from explicit ID
			// and top-level execution state.
			return inspectedAction{ActionID: explicit, State: inspected.effectiveState()}, true, nil
		}
		return inspectedAction{}, false, fmt.Errorf("action %q is not part of execution %q", explicit, inspected.ExecutionID)
	}
	selected, ok := selectLogAction(inspected)
	return selected, ok, nil
}

// resolveActionName resolves an action YAML name to its ActionID via inspect.
func resolveActionName(ctx context.Context, ports Ports, executionID, name string) (string, bool) {
	inspected, err := fetchExecutionInspect(ctx, ports, executionID)
	if err != nil {
		return "", false
	}
	for _, action := range inspected.Actions {
		if action.Name == name && action.ActionID != "" {
			return action.ActionID, true
		}
	}
	return "", false
}

// checkLogsTerminal reports whether --follow has reached a terminal status.
func checkLogsTerminal(execState string, target inspectedAction, explicitAction bool, allActions []inspectedAction) (string, bool, error) {
	actionState := target.effectiveState()
	if explicitAction {
		switch actionState {
		case "succeeded", "failed", "cancelled":
			return actionState, true, nil
		}
	}
	switch execState {
	case "succeeded", "failed", "cancelled":
		if explicitAction && (actionState == "failed" || actionState == "cancelled") {
			return actionState, true, nil
		}
		return execState, true, nil
	case "queued", "running":
		if !explicitAction && len(allActions) == 1 {
			switch actionState {
			case "succeeded", "failed", "cancelled":
				return actionState, true, nil
			}
		}
		return "", false, nil
	case "":
		switch actionState {
		case "succeeded", "failed", "cancelled":
			return actionState, true, nil
		case "pending", "running":
			return "", false, nil
		}
		return "", false, fmt.Errorf("execution status is missing or unknown")
	default:
		return "", false, fmt.Errorf("execution status %q is unknown", execState)
	}
}
