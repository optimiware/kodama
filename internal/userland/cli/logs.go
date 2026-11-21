package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodema/api"
)

// inspectedAction is a lenient projection of api.ExecutionAction used by CLI
// inspection helpers so both full server responses and concise test fixtures
// decode cleanly.
type inspectedAction struct {
	ActionID    string                     `json:"action_id"`
	Index       int                        `json:"index"`
	Name        string                     `json:"name"`
	Kind        string                     `json:"kind"`
	State       string                     `json:"state"`
	Status      string                     `json:"status"`
	Version     uint64                     `json:"version"`
	ActivatedAt *time.Time                 `json:"activated_at,omitempty"`
	Outputs     map[string]string          `json:"outputs,omitempty"`
	Subject     *api.Subject               `json:"subject,omitempty"`
	Assignment  string                     `json:"assignment,omitempty"`
	Stop        *api.ExecutionStopEvidence `json:"stop,omitempty"`
	Decision    string                     `json:"decision,omitempty"`
	DecisionID  string                     `json:"decision_id,omitempty"`
	Actor       string                     `json:"actor,omitempty"`
	AnsweredAt  *time.Time                 `json:"answered_at,omitempty"`
	Note        string                     `json:"note,omitempty"`
}

func (action inspectedAction) effectiveState() string {
	if action.State != "" {
		return action.State
	}
	return action.Status
}

// inspectedExecution is a lenient projection of api.ExecutionInspectResult.
type inspectedExecution struct {
	ExecutionID   string                   `json:"execution_id"`
	TaskID        string                   `json:"task_id,omitempty"`
	PipelineID    string                   `json:"pipeline_id,omitempty"`
	StartedBy     string                   `json:"started_by,omitempty"`
	Inputs        map[string]string        `json:"inputs,omitempty"`
	Trigger       *api.ExecutionTrigger    `json:"trigger,omitempty"`
	CreatedAt     *time.Time               `json:"created_at,omitempty"`
	State         string                   `json:"state"`
	Status        string                   `json:"status"`
	Version       uint64                   `json:"version"`
	CurrentAction int                      `json:"current_action"`
	Actions       []inspectedAction        `json:"actions"`
	ModelUsage    *api.ExecutionModelUsage `json:"model_usage,omitempty"`
	Failure       *api.ExecutionFailure    `json:"failure,omitempty"`
	Cleanup       api.ExecutionCleanup     `json:"cleanup"`
}

func (execution inspectedExecution) effectiveState() string {
	if execution.State != "" {
		return execution.State
	}
	return execution.Status
}

// logsJSONPage is the structured JSON output for `kodema logs --output json`.
type logsJSONPage struct {
	ExecutionID string         `json:"execution_id"`
	ActionID    string         `json:"action_id"`
	ActionName  string         `json:"action_name,omitempty"`
	Stream      string         `json:"stream"`
	Data        string         `json:"data"`
	Chunks      []api.LogChunk `json:"chunks"`
	Truncated   bool           `json:"truncated"`
}

// logStreamState tracks pagination and deduplication across one action's stream.
type logStreamState struct {
	actionID   string
	actionName string
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

// logsSettling reports whether the output of an ended execution may still be
// on its way: the server retains a workload's output when it removes the
// workload, which is after the outcome is known. It counts the waits in
// waited and gives up after 30.
func logsSettling(inspected inspectedExecution, waited *int) bool {
	switch inspected.effectiveState() {
	case "succeeded", "failed", "cancelled":
	default:
		return false
	}
	if inspected.Cleanup.State != "pending" || *waited >= 30 {
		return false
	}
	*waited++
	return true
}

// runLogsOnce resolves the target action when omitted and drains all pages.
func runLogsOnce(ctx context.Context, ports Ports, executionID, actionFlag, stream string, format OutputFormat) int {
	var inspected inspectedExecution
	for waited := 0; ; ports.Sleep(watchInterval) {
		var err error
		if inspected, err = fetchExecutionInspect(ctx, ports, executionID); err != nil {
			return failure(ports.Err, err)
		}
		if !logsSettling(inspected, &waited) {
			break
		}
	}
	actionID := actionFlag
	if actionID == "" {
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
// the target action or execution reaches a terminal status (or pauses at a
// request action when following across all actions without --action).
func runLogsFollow(ctx context.Context, ports Ports, executionID, actionFlag, stream string, format OutputFormat) int {
	if actionFlag != "" {
		return runLogsFollowExplicit(ctx, ports, executionID, actionFlag, stream, format)
	}
	return runLogsFollowMulti(ctx, ports, executionID, stream, format)
}

// runLogsFollowExplicit preserves single-action follow behavior when --action
// is explicitly specified.
func runLogsFollowExplicit(ctx context.Context, ports Ports, executionID, actionFlag, stream string, format OutputFormat) int {
	var state *logStreamState
	for settling := 0; ; {
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

		if state == nil || state.actionID != targetAction.ActionID {
			state = &logStreamState{actionID: targetAction.ActionID, actionName: targetAction.Name}
		}

		if _, _, err := drainLogPages(ctx, ports, executionID, stream, format, true, state); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(ports.Err, "logs canceled")
				return ExitFailure
			}
			return failure(ports.Err, err)
		}

		terminalState, done, err := checkLogsTerminal(execState, targetAction, true, inspected.Actions)
		if err != nil {
			return failure(ports.Err, err)
		}
		if done {
			if logsSettling(inspected, &settling) {
				ports.Sleep(watchInterval)
				continue
			}
			if terminalState == "succeeded" {
				return ExitSuccess
			}
			fmt.Fprintf(ports.Err, "logs failed: status %s\n", terminalState)
			return ExitFailure
		}

		ports.Sleep(watchInterval)
	}
}

// runLogsFollowMulti follows logs seamlessly across all sequential container
// actions as an execution advances, draining each completed container action
// before transitioning to the next, skipping non-container request actions, and
// stopping cleanly on terminal execution states or when paused at a waiting
// request action.
func runLogsFollowMulti(ctx context.Context, ports Ports, executionID, stream string, format OutputFormat) int {
	states := make(map[string]*logStreamState)
	drainedComplete := make(map[string]bool)
	headerPrinted := make(map[string]bool)
	anyHeaderPrinted := false
	lastByteNewline := true

	for settling := 0; ; {
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
		startedContainers := orderedStartedContainerActions(inspected.Actions)
		execTerminal := execState == "succeeded" || execState == "failed" || execState == "cancelled"
		showHeaders := len(startedContainers) > 1 || (!execTerminal && countContainerActions(inspected.Actions) > 1)
		cleanupPending := execTerminal && inspected.Cleanup.State == "pending"

		for _, action := range startedContainers {
			if drainedComplete[action.ActionID] {
				continue
			}
			state := states[action.ActionID]
			if state == nil {
				state = &logStreamState{actionID: action.ActionID, actionName: action.Name}
				states[action.ActionID] = state
			} else if state.actionName == "" && action.Name != "" {
				state.actionName = action.Name
			}

			if format != OutputJSON && showHeaders && !headerPrinted[action.ActionID] {
				if anyHeaderPrinted && !lastByteNewline {
					if _, err := fmt.Fprintln(ports.Out); err != nil {
						return failure(ports.Err, err)
					}
				}
				if _, err := fmt.Fprintf(ports.Out, "==> action %s <==\n", formatLogActionLabel(action)); err != nil {
					return failure(ports.Err, err)
				}
				headerPrinted[action.ActionID] = true
				anyHeaderPrinted = true
				lastByteNewline = true
			}

			chunks, _, err := drainLogPages(ctx, ports, executionID, stream, format, true, state)
			if err != nil {
				if ctx.Err() != nil {
					fmt.Fprintln(ports.Err, "logs canceled")
					return ExitFailure
				}
				return failure(ports.Err, err)
			}
			if format != OutputJSON && len(chunks) > 0 {
				for _, chunk := range chunks {
					if len(chunk.Data) > 0 {
						lastByteNewline = chunk.Data[len(chunk.Data)-1] == '\n'
					}
				}
			}

			if isActionTerminal(action.effectiveState()) && !cleanupPending {
				drainedComplete[action.ActionID] = true
			}
		}

		switch execState {
		case "succeeded", "failed", "cancelled":
			if logsSettling(inspected, &settling) {
				ports.Sleep(watchInterval)
				continue
			}
			if execState == "succeeded" {
				if len(inspected.Actions) == 0 {
					return failure(ports.Err, fmt.Errorf("execution %q has no actions", executionID))
				}
				return ExitSuccess
			}
			fmt.Fprintf(ports.Err, "logs failed: status %s\n", execState)
			return ExitFailure
		case "queued", "running":
			if waitingReq, paused := activeWaitingRequestAction(inspected); paused {
				assignSuffix := ""
				if strings.TrimSpace(waitingReq.Assignment) != "" {
					assignSuffix = fmt.Sprintf(" (%s)", waitingReq.Assignment)
				}
				fmt.Fprintf(ports.Err, "logs paused: waiting on request action %s%s\n", formatLogActionLabel(waitingReq), assignSuffix)
				return ExitSuccess
			}
			if len(inspected.Actions) == 1 && inspected.Actions[0].Kind != "request" {
				onlyState := inspected.Actions[0].effectiveState()
				if isActionTerminal(onlyState) {
					if logsSettling(inspected, &settling) {
						ports.Sleep(watchInterval)
						continue
					}
					if onlyState == "succeeded" {
						return ExitSuccess
					}
					fmt.Fprintf(ports.Err, "logs failed: status %s\n", onlyState)
					return ExitFailure
				}
			}
			ports.Sleep(watchInterval)
		case "":
			if len(startedContainers) > 0 {
				last := startedContainers[len(startedContainers)-1]
				if isActionTerminal(last.effectiveState()) {
					if last.effectiveState() == "succeeded" {
						return ExitSuccess
					}
					fmt.Fprintf(ports.Err, "logs failed: status %s\n", last.effectiveState())
					return ExitFailure
				}
				ports.Sleep(watchInterval)
				continue
			}
			return failure(ports.Err, fmt.Errorf("execution status is missing or unknown"))
		default:
			return failure(ports.Err, fmt.Errorf("execution status %q is unknown", execState))
		}
	}
}

func countContainerActions(actions []inspectedAction) int {
	count := 0
	for _, action := range actions {
		if action.Kind != "request" {
			count++
		}
	}
	return count
}

func orderedStartedContainerActions(actions []inspectedAction) []inspectedAction {
	started := make([]inspectedAction, 0, len(actions))
	for _, action := range actions {
		if action.Kind == "request" {
			continue
		}
		if actionWasStarted(action) {
			started = append(started, action)
		}
	}
	return started
}

func activeWaitingRequestAction(inspected inspectedExecution) (inspectedAction, bool) {
	if idx := inspected.CurrentAction; idx >= 0 && idx < len(inspected.Actions) {
		candidate := inspected.Actions[idx]
		state := candidate.effectiveState()
		if candidate.Kind == "request" && (state == "running" || state == "waiting") && candidate.Decision == "" {
			return candidate, true
		}
	}
	for _, action := range inspected.Actions {
		state := action.effectiveState()
		if action.Kind == "request" && (state == "running" || state == "waiting") && action.Decision == "" {
			return action, true
		}
	}
	return inspectedAction{}, false
}

func isActionTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func formatLogActionLabel(action inspectedAction) string {
	if action.Name != "" && action.Name != action.ActionID {
		return fmt.Sprintf("%s (%s)", action.Name, action.ActionID)
	}
	return orDash(action.ActionID)
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
				ActionName:  state.actionName,
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

// actionWasStarted reports whether an action actually ran (was activated).
// A never-started action cancelled because an earlier action failed or timed
// out has ActivatedAt == nil and is excluded; minimal fixtures that omit
// activated_at still treat running, succeeded, and failed actions as started.
func actionWasStarted(action inspectedAction) bool {
	if action.ActivatedAt != nil && !action.ActivatedAt.IsZero() {
		return true
	}
	switch action.effectiveState() {
	case "running", "succeeded", "failed":
		return true
	default:
		return false
	}
}

// selectLogAction picks the currently running container action, or else the
// last started container action, falling back to request actions only when no
// container action has started.
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
	for index := len(inspected.Actions) - 1; index >= 0; index-- {
		action := inspected.Actions[index]
		if action.Kind != "request" && actionWasStarted(action) {
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
		if actionWasStarted(action) {
			return action, true
		}
	}
	return inspectedAction{}, false
}

// resolveFollowAction locates the explicit action or auto-selects the active
// action during `kodema logs --follow`.
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
