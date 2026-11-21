package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
)

// runTask dispatches the ergonomic `kodema task` subcommands.
func runTask(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "task requires a subcommand (create|run|list|show|start|restart|ask|reply|cancel|complete)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"task"}, args[1:]...))
	}
	switch args[0] {
	case "create":
		return runTaskCreate(ctx, config, args[1:], ports, deps, false)
	case "run":
		return runTaskCreate(ctx, config, args[1:], ports, deps, true)
	case "show", "read":
		return runTaskShow(ctx, config, args[1:], ports)
	case "start":
		return runTaskStart(ctx, config, args[1:], ports, deps)
	case "restart":
		return runTaskRestart(ctx, config, args[1:], ports, deps)
	case "ask":
		return runTaskAsk(ctx, config, args[1:], ports, deps)
	case "reply":
		return runTaskReply(ctx, config, args[1:], ports, deps)
	case "cancel":
		return runTaskCancel(ctx, config, args[1:], ports, deps)
	case "complete":
		return runTaskComplete(ctx, config, args[1:], ports, deps)
	case "list", "ls":
		return runTaskList(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown task subcommand %q", args[0]))
	}
}

func runTaskCreate(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps, start bool) int {
	subcmd := "create"
	if start {
		subcmd = "run"
	}
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", subcmd})
	}
	var (
		projectFlag        string
		parentFlag         string
		titleFlag          string
		descFlag           string
		pipelineFlag       string
		outputFlag         string
		idempotencyKeyFlag string
		inputPairs         []string
		metaPairs          []string
		completesTaskFlag  bool
		noCompleteFlag     bool
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&parentFlag, "--parent", "--parent-id")
	spec.stringFlag(&titleFlag, "--title", "-t")
	spec.stringFlag(&descFlag, "--description", "-d")
	spec.stringFlag(&pipelineFlag, "--pipeline")
	spec.listFlag(&inputPairs, "--input")
	spec.listFlag(&metaPairs, "--meta", "--metadata")
	if start {
		spec.boolFlag(&completesTaskFlag, "--completes-task")
		spec.boolFlag(&noCompleteFlag, "--no-complete")
	}
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	completesTask, err := resolveCompletesTaskFlag(seen, completesTaskFlag, noCompleteFlag)
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
		return usage(ports.Err, fmt.Sprintf("task %s requires --project <id> or KODEMA_PROJECT", subcmd))
	}
	if strings.TrimSpace(titleFlag) == "" {
		return usage(ports.Err, fmt.Sprintf("task %s requires --title <text>", subcmd))
	}
	pipelineTarget := strings.TrimSpace(pipelineFlag)
	if start && pipelineTarget == "" {
		return usage(ports.Err, "task run requires --pipeline <name|id>")
	}
	inputs, err := parseKeyValues(inputPairs, "input")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	metadata, err := parseKeyValues(metaPairs, "metadata")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	// Pipeline names may contain dots (e.g. "release.v1"), which are valid
	// pipeline names but not valid opaque IDs. Resolve those upfront.
	resolvedByName := false
	if pipelineTarget != "" && strings.Contains(pipelineTarget, ".") {
		resolved, found, lookupErr := resolvePipelineIDByName(ctx, ports, projectID, pipelineTarget)
		if lookupErr != nil {
			return failure(ports.Err, lookupErr)
		}
		if !found {
			return failure(ports.Err, fmt.Errorf("pipeline %q not found in project %q", pipelineTarget, projectID))
		}
		pipelineTarget = resolved
		resolvedByName = true
	}

	buildInvocation := func(pipelineID, explicitKey string) (api.Invocation, int) {
		var (
			operation string
			rawInput  []byte
			err       error
		)
		if start {
			operation = "task.create_and_start"
			req := api.TaskCreateAndStartRequest{
				ProjectID:     projectID,
				ParentID:      strings.TrimSpace(parentFlag),
				Title:         titleFlag,
				Description:   descFlag,
				PipelineID:    pipelineID,
				Inputs:        inputs,
				Metadata:      metadata,
				CompletesTask: completesTask,
			}
			if err := req.Validate(); err != nil {
				return api.Invocation{}, usage(ports.Err, publicMessage(err))
			}
			rawInput, err = json.Marshal(req)
			if err != nil {
				return api.Invocation{}, failure(ports.Err, err)
			}
		} else {
			operation = "task.create"
			req := api.TaskCreateRequest{
				ProjectID:   projectID,
				ParentID:    strings.TrimSpace(parentFlag),
				Title:       titleFlag,
				Description: descFlag,
				PipelineID:  pipelineID,
				Inputs:      inputs,
				Metadata:    metadata,
			}
			if err := req.Validate(); err != nil {
				return api.Invocation{}, usage(ports.Err, publicMessage(err))
			}
			rawInput, err = json.Marshal(req)
			if err != nil {
				return api.Invocation{}, failure(ports.Err, err)
			}
		}
		key, err := makeIdempotencyKey(explicitKey, operation, deps.Login.Random)
		if err != nil {
			return api.Invocation{}, failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: operation,
			Request: api.Request{
				Input:          rawInput,
				IdempotencyKey: key,
			},
		}
		if err := invocation.Validate(); err != nil {
			return api.Invocation{}, usage(ports.Err, publicMessage(err))
		}
		return invocation, ExitSuccess
	}

	invocation, status := buildInvocation(pipelineTarget, idempotencyKeyFlag)
	if status != ExitSuccess {
		return status
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil && pipelineTarget != "" && !resolvedByName && isNotFoundError(err) {
		if resolved, found, lookupErr := resolvePipelineIDByName(ctx, ports, projectID, pipelineTarget); lookupErr == nil && found && resolved != pipelineTarget {
			retryInv, retryStatus := buildInvocation(resolved, idempotencyKeyFlag)
			if retryStatus != ExitSuccess {
				return retryStatus
			}
			result, err = ports.Client.Invoke(ctx, retryInv)
		}
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

	if start {
		var created api.TaskCreateAndStartResult
		if err := json.Unmarshal(result.Body, &created); err != nil {
			return renderTableFallback(ports, result)
		}
		fmt.Fprintf(ports.Out, "task_id\t%s\nexecution_id\t%s\nlifecycle_version\t%d\n",
			created.TaskID, created.ExecutionID, created.LifecycleVersion)
		return ExitSuccess
	}
	var created api.TaskCreateResult
	if err := json.Unmarshal(result.Body, &created); err != nil {
		return renderTableFallback(ports, result)
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\ncontent_version\t%d\nlifecycle_version\t%d\n",
		created.TaskID, created.ContentVersion, created.LifecycleVersion)
	return ExitSuccess
}

func runTaskShow(ctx context.Context, config Config, args []string, ports Ports) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "show"})
	}
	var outputFlag string
	spec := newArgSpec()
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task show requires <task-id>")
	}
	taskID := strings.TrimSpace(positionals[0])
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	req := api.TaskReadRequest{TaskID: taskID}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	rawResult, readView, err := fetchTaskRead(ctx, ports, taskID)
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, rawResult); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	if readView.TaskID == "" {
		return renderTableFallback(ports, rawResult)
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\ntitle\t%s\nstate\t%s\n", readView.TaskID, readView.Title, readView.State)
	if readView.ParentID != "" {
		fmt.Fprintf(ports.Out, "parent_id\t%s\n", readView.ParentID)
	}
	if readView.Description != "" {
		fmt.Fprintf(ports.Out, "description\t%s\n", singleLine(readView.Description, 120))
	}
	if readView.SelectedPipeline != "" {
		fmt.Fprintf(ports.Out, "selected_pipeline\t%s\n", readView.SelectedPipeline)
	}
	if readView.CurrentExecution != "" {
		fmt.Fprintf(ports.Out, "current_execution\t%s\n", readView.CurrentExecution)
	}
	fmt.Fprintf(ports.Out, "content_version\t%d\nlifecycle_version\t%d\n", readView.ContentVersion, readView.LifecycleVersion)
	if readView.Question != nil {
		fmt.Fprintf(ports.Out, "question_id\t%s\nquestion_assignment\t%s\nquestion_version\t%d\nquestion\t%s\n",
			readView.Question.QuestionID, readView.Question.Assignment, readView.Question.Version, singleLine(readView.Question.Body, 160))
		if refs := formatReferences(readView.Question.References); refs != "" {
			fmt.Fprintf(ports.Out, "question_references\t%s\n", refs)
		}
	}
	return ExitSuccess
}

func runTaskStart(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "start"})
	}
	var (
		projectFlag        string
		pipelineFlag       string
		inputPairs         []string
		outputInputPairs   []string
		completesTaskFlag  bool
		noCompleteFlag     bool
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&pipelineFlag, "--pipeline")
	spec.listFlag(&inputPairs, "--input")
	spec.listFlag(&outputInputPairs, "--input-from-output")
	spec.boolFlag(&completesTaskFlag, "--completes-task")
	spec.boolFlag(&noCompleteFlag, "--no-complete")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task start requires <task-id>")
	}
	completesTask, err := resolveCompletesTaskFlag(seen, completesTaskFlag, noCompleteFlag)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	taskID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	inputs, err := parseKeyValues(inputPairs, "input")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	outputRefs, err := parseOutputInputRefs(outputInputPairs)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := validateOutputInputRefCollisions(inputs, outputRefs); err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := (api.TaskReadRequest{TaskID: taskID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	projectID := resolveProject(projectFlag, config, deps)
	pipelineTarget, resolvedByName, resolveErr := resolveTaskPipelineTarget(ctx, ports, projectID, strings.TrimSpace(pipelineFlag))
	if resolveErr != nil {
		return failure(ports.Err, resolveErr)
	}

	var readView api.TaskReadResult
	if expectedVer == nil || len(outputRefs) > 0 {
		_, fetched, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		readView = fetched
		if expectedVer == nil && readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
	}
	if len(outputRefs) > 0 {
		resolvedInputs, refErr := resolveInputsFromLatestSucceeded(ctx, ports, taskID, readView, outputRefs, inputs)
		if refErr != nil {
			return failure(ports.Err, refErr)
		}
		inputs = resolvedInputs
	}

	buildInvocation := func(pipelineID string) (api.Invocation, int) {
		req := api.TaskStartRequest{
			TaskID:        taskID,
			PipelineID:    pipelineID,
			Inputs:        inputs,
			CompletesTask: completesTask,
		}
		if err := req.Validate(); err != nil {
			return api.Invocation{}, usage(ports.Err, publicMessage(err))
		}
		input, err := json.Marshal(req)
		if err != nil {
			return api.Invocation{}, failure(ports.Err, err)
		}
		key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.start", deps.Login.Random)
		if err != nil {
			return api.Invocation{}, failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "task.start",
			Request: api.Request{
				Input:           input,
				ExpectedVersion: expectedVer,
				IdempotencyKey:  key,
			},
		}
		if err := invocation.Validate(); err != nil {
			return api.Invocation{}, usage(ports.Err, publicMessage(err))
		}
		return invocation, ExitSuccess
	}

	invocation, status := buildInvocation(pipelineTarget)
	if status != ExitSuccess {
		return status
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil && pipelineTarget != "" && !resolvedByName && projectID != "" && isNotFoundError(err) {
		if resolved, found, lookupErr := resolvePipelineIDByName(ctx, ports, projectID, pipelineTarget); lookupErr == nil && found && resolved != pipelineTarget {
			retryInv, retryStatus := buildInvocation(resolved)
			if retryStatus != ExitSuccess {
				return retryStatus
			}
			result, err = ports.Client.Invoke(ctx, retryInv)
		}
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
	var started api.TaskStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil || started.ExecutionID == "" {
		fmt.Fprintf(ports.Out, "started task %s\n", taskID)
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\nexecution_id\t%s\nlifecycle_version\t%d\n",
		taskID, started.ExecutionID, started.LifecycleVersion)
	return ExitSuccess
}

func runTaskRestart(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "restart"})
	}
	var (
		reasonFlag         string
		projectFlag        string
		pipelineFlag       string
		inputPairs         []string
		outputInputPairs   []string
		completesTaskFlag  bool
		noCompleteFlag     bool
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&reasonFlag, "--reason")
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&pipelineFlag, "--pipeline")
	spec.listFlag(&inputPairs, "--input")
	spec.listFlag(&outputInputPairs, "--input-from-output")
	spec.boolFlag(&completesTaskFlag, "--completes-task")
	spec.boolFlag(&noCompleteFlag, "--no-complete")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task restart requires <task-id>")
	}
	completesTask, err := resolveCompletesTaskFlag(seen, completesTaskFlag, noCompleteFlag)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	taskID := strings.TrimSpace(positionals[0])
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	inputs, err := parseKeyValues(inputPairs, "input")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	outputRefs, err := parseOutputInputRefs(outputInputPairs)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := validateOutputInputRefCollisions(inputs, outputRefs); err != nil {
		return usage(ports.Err, err.Error())
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := (api.TaskReadRequest{TaskID: taskID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	projectID := resolveProject(projectFlag, config, deps)
	pipelineTarget, resolvedByName, resolveErr := resolveTaskPipelineTarget(ctx, ports, projectID, strings.TrimSpace(pipelineFlag))
	if resolveErr != nil {
		return failure(ports.Err, resolveErr)
	}

	var readView api.TaskReadResult
	if expectedVer == nil || len(outputRefs) > 0 {
		_, fetched, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		readView = fetched
		if expectedVer == nil && readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
	}
	if len(outputRefs) > 0 {
		resolvedInputs, refErr := resolveInputsFromLatestSucceeded(ctx, ports, taskID, readView, outputRefs, inputs)
		if refErr != nil {
			return failure(ports.Err, refErr)
		}
		inputs = resolvedInputs
	}

	buildInvocation := func(pipelineID string) (api.Invocation, int) {
		req := api.TaskRestartRequest{
			TaskID:        taskID,
			Reason:        reasonFlag,
			PipelineID:    pipelineID,
			Inputs:        inputs,
			CompletesTask: completesTask,
		}
		if err := req.Validate(); err != nil {
			return api.Invocation{}, usage(ports.Err, publicMessage(err))
		}
		input, err := json.Marshal(req)
		if err != nil {
			return api.Invocation{}, failure(ports.Err, err)
		}
		key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.restart", deps.Login.Random)
		if err != nil {
			return api.Invocation{}, failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "task.restart",
			Request: api.Request{
				Input:           input,
				ExpectedVersion: expectedVer,
				IdempotencyKey:  key,
			},
		}
		if err := invocation.Validate(); err != nil {
			return api.Invocation{}, usage(ports.Err, publicMessage(err))
		}
		return invocation, ExitSuccess
	}

	invocation, status := buildInvocation(pipelineTarget)
	if status != ExitSuccess {
		return status
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil && pipelineTarget != "" && !resolvedByName && projectID != "" && isNotFoundError(err) {
		if resolved, found, lookupErr := resolvePipelineIDByName(ctx, ports, projectID, pipelineTarget); lookupErr == nil && found && resolved != pipelineTarget {
			retryInv, retryStatus := buildInvocation(resolved)
			if retryStatus != ExitSuccess {
				return retryStatus
			}
			result, err = ports.Client.Invoke(ctx, retryInv)
		}
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
	var restarted api.TaskRestartResult
	if err := json.Unmarshal(result.Body, &restarted); err != nil || restarted.ExecutionID == "" {
		fmt.Fprintf(ports.Out, "restarted task %s\n", taskID)
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\nexecution_id\t%s\n", taskID, restarted.ExecutionID)
	if restarted.SupersededExecution != "" {
		fmt.Fprintf(ports.Out, "superseded_execution\t%s\n", restarted.SupersededExecution)
	}
	fmt.Fprintf(ports.Out, "lifecycle_version\t%d\n", restarted.LifecycleVersion)
	return ExitSuccess
}

type outputInputRef struct {
	inputKey   string
	actionName string
	outputKey  string
}

// resolveTaskPipelineTarget resolves dotted pipeline names upfront (dots are
// valid pipeline names but not valid opaque IDs). Other targets are sent as-is
// and retried via resolvePipelineIDByName if the server reports not_found.
func resolveTaskPipelineTarget(ctx context.Context, ports Ports, projectID, target string) (string, bool, error) {
	if target == "" || !strings.Contains(target, ".") {
		return target, false, nil
	}
	if projectID == "" {
		return "", false, fmt.Errorf("resolving pipeline name %q requires --project <id> or KODEMA_PROJECT", target)
	}
	resolved, found, err := resolvePipelineIDByName(ctx, ports, projectID, target)
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, fmt.Errorf("pipeline %q not found in project %q", target, projectID)
	}
	return resolved, true, nil
}

// parseOutputInputRefs parses repeatable `<input-key>=<action>.<output-key>`
// flags used to populate task inputs from the task's latest succeeded execution.
func parseOutputInputRefs(entries []string) ([]outputInputRef, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(entries))
	refs := make([]outputInputRef, 0, len(entries))
	for _, entry := range entries {
		inputKey, rhs, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(inputKey) == "" || inputKey != strings.TrimSpace(inputKey) {
			return nil, fmt.Errorf("input-from-output %q must be <input-key>=<action>.<output-key>", entry)
		}
		if seen[inputKey] {
			return nil, fmt.Errorf("input-from-output key %q is repeated", inputKey)
		}
		actionName, outputKey, dotOK := strings.Cut(rhs, ".")
		if !dotOK || actionName == "" || outputKey == "" || strings.Contains(outputKey, ".") ||
			actionName != strings.TrimSpace(actionName) || outputKey != strings.TrimSpace(outputKey) {
			return nil, fmt.Errorf("input-from-output %q must reference <action>.<output-key>", entry)
		}
		seen[inputKey] = true
		refs = append(refs, outputInputRef{
			inputKey:   inputKey,
			actionName: actionName,
			outputKey:  outputKey,
		})
	}
	return refs, nil
}

func validateOutputInputRefCollisions(inputs map[string]string, refs []outputInputRef) error {
	for _, ref := range refs {
		if _, exists := inputs[ref.inputKey]; exists {
			return fmt.Errorf("input key %q is specified by both --input and --input-from-output", ref.inputKey)
		}
	}
	return nil
}

// resolveInputsFromLatestSucceeded inspects executions in reverse history order
// to locate the task's latest succeeded execution and resolves each declared
// output reference into the outgoing inputs map.
func resolveInputsFromLatestSucceeded(
	ctx context.Context,
	ports Ports,
	taskID string,
	readView api.TaskReadResult,
	refs []outputInputRef,
	base map[string]string,
) (map[string]string, error) {
	if err := validateOutputInputRefCollisions(base, refs); err != nil {
		return nil, err
	}
	merged := make(map[string]string, len(base)+len(refs))
	for k, v := range base {
		merged[k] = v
	}
	var latest *api.ExecutionInspectResult
	for i := len(readView.ExecutionHistory) - 1; i >= 0; i-- {
		execID := readView.ExecutionHistory[i]
		reqBytes, err := json.Marshal(api.ExecutionInspectRequest{ExecutionID: execID})
		if err != nil {
			return nil, err
		}
		res, err := ports.Client.Invoke(ctx, api.Invocation{
			Operation: "execution.inspect",
			Request:   api.Request{Input: reqBytes},
		})
		if err != nil {
			return nil, err
		}
		var inspected api.ExecutionInspectResult
		if err := json.Unmarshal(res.Body, &inspected); err != nil {
			return nil, fmt.Errorf("invalid execution.inspect response for %q: %w", execID, err)
		}
		if inspected.State == "succeeded" {
			latest = &inspected
			break
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("task %q has no succeeded execution to read outputs from", taskID)
	}
	for _, ref := range refs {
		foundAction := false
		for _, action := range latest.Actions {
			if action.Name != ref.actionName {
				continue
			}
			foundAction = true
			if action.State != "succeeded" {
				return nil, fmt.Errorf("action %q in execution %q has state %q, not succeeded", ref.actionName, latest.ExecutionID, action.State)
			}
			val, ok := action.Outputs[ref.outputKey]
			if !ok {
				return nil, fmt.Errorf("action %q in execution %q has no output %q", ref.actionName, latest.ExecutionID, ref.outputKey)
			}
			merged[ref.inputKey] = val
			break
		}
		if !foundAction {
			return nil, fmt.Errorf("execution %q has no action named %q", latest.ExecutionID, ref.actionName)
		}
	}
	return merged, nil
}

func isNotFoundError(err error) bool {
	if public, ok := errors.AsType[*api.Error](err); ok && public.Code == api.CodeNotFound {
		return true
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return true
	}
	return false
}

// runTaskAsk pauses a running task with an assigned question and optional
// durable restoration references via task.ask. When <task-id> is omitted,
// $KODEMA_TASK is used; when --expected-version is omitted, task.read is
// queried automatically.
func runTaskAsk(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "ask"})
	}
	var (
		questionFlag       string
		assignmentFlag     string
		refFlags           []string
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&questionFlag, "--question", "-q")
	spec.stringFlag(&assignmentFlag, "--to", "--assignment")
	spec.listFlag(&refFlags, "--ref", "--reference")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}
	taskID := ""
	if len(positionals) == 1 {
		taskID = strings.TrimSpace(positionals[0])
	}
	if taskID == "" {
		getenv := deps.Login.Environment
		if getenv == nil {
			getenv = os.Getenv
		}
		taskID = strings.TrimSpace(getenv("KODEMA_TASK"))
	}
	if taskID == "" {
		return usage(ports.Err, "task ask requires <task-id> or KODEMA_TASK")
	}
	if strings.TrimSpace(questionFlag) == "" {
		return usage(ports.Err, "task ask requires --question <text>")
	}
	if strings.TrimSpace(assignmentFlag) == "" {
		return usage(ports.Err, "task ask requires --to <role/<role>|service/<id>>")
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
	req := api.TaskAskRequest{
		TaskID:     taskID,
		Question:   questionFlag,
		Assignment: strings.TrimSpace(assignmentFlag),
		References: refFlags,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		_, readView, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		if readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.ask", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "task.ask",
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
	var asked api.TaskAskResult
	if err := json.Unmarshal(result.Body, &asked); err != nil || asked.LifecycleVersion == 0 {
		fmt.Fprintf(ports.Out, "asked question on task %s\n", taskID)
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\n", taskID)
	if asked.QuestionID != "" {
		fmt.Fprintf(ports.Out, "question_id\t%s\n", asked.QuestionID)
	}
	fmt.Fprintf(ports.Out, "lifecycle_version\t%d\nquestion_version\t%d\n", asked.LifecycleVersion, asked.QuestionVersion)
	return ExitSuccess
}

func runTaskReply(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "reply"})
	}
	var (
		answerFlag         string
		questionFlag       string
		expectedVerRaw     string
		expectedQVerRaw    string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&answerFlag, "--answer", "-a")
	spec.stringFlag(&questionFlag, "--question", "--question-id")
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&expectedQVerRaw, "--expected-question-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task reply requires <task-id>")
	}
	taskID := strings.TrimSpace(positionals[0])
	if strings.TrimSpace(answerFlag) == "" {
		return usage(ports.Err, "task reply requires --answer <text>")
	}
	expectedVer, err := parsePositiveVersion(expectedVerRaw, "--expected-version")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	expectedQVer, err := parsePositiveVersion(expectedQVerRaw, "--expected-question-version")
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
	if err := (api.TaskReadRequest{TaskID: taskID}).Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	questionID := strings.TrimSpace(questionFlag)
	var questionVersion uint64
	if expectedQVer != nil {
		questionVersion = *expectedQVer
	}

	if questionID == "" || questionVersion == 0 || expectedVer == nil {
		_, readView, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		if expectedVer == nil && readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
		if questionID == "" || questionVersion == 0 {
			if readView.Question == nil {
				return failure(ports.Err, fmt.Errorf("task %q has no outstanding question", taskID))
			}
			if questionID == "" {
				questionID = readView.Question.QuestionID
			}
			if questionVersion == 0 {
				questionVersion = readView.Question.Version
			}
		}
	}

	req := api.TaskReplyRequest{
		TaskID:                  taskID,
		QuestionID:              questionID,
		ExpectedQuestionVersion: questionVersion,
		Answer:                  answerFlag,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.reply", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "task.reply",
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
	var replied api.TaskReplyResult
	if err := json.Unmarshal(result.Body, &replied); err != nil || replied.ExecutionID == "" {
		fmt.Fprintf(ports.Out, "replied to task %s\n", taskID)
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\nexecution_id\t%s\nlifecycle_version\t%d\nquestion_version\t%d\n",
		taskID, replied.ExecutionID, replied.LifecycleVersion, replied.QuestionVersion)
	return ExitSuccess
}

func runTaskCancel(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "cancel"})
	}
	var (
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&expectedVerRaw, "--expected-version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task cancel requires <task-id>")
	}
	taskID := strings.TrimSpace(positionals[0])
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
	req := api.TaskCancelRequest{TaskID: taskID}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		_, readView, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		if readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.cancel", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "task.cancel",
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
	var cancelled api.TaskCancelResult
	_ = json.Unmarshal(result.Body, &cancelled)
	fmt.Fprintf(ports.Out, "cancelled task %s (lifecycle_version=%d)\n", taskID, cancelled.LifecycleVersion)
	return ExitSuccess
}

func runTaskComplete(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "complete"})
	}
	var (
		reasonFlag         string
		expectedVerRaw     string
		outputFlag         string
		idempotencyKeyFlag string
	)
	spec := newArgSpec()
	spec.stringFlag(&reasonFlag, "--reason")
	spec.stringFlag(&expectedVerRaw, "--expected-version", "--version")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "task complete requires <task-id>")
	}
	taskID := strings.TrimSpace(positionals[0])
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
	req := api.TaskCompleteRequest{TaskID: taskID, Reason: reasonFlag}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if expectedVer == nil {
		_, readView, readErr := fetchTaskRead(ctx, ports, taskID)
		if readErr != nil {
			return failure(ports.Err, readErr)
		}
		if readView.LifecycleVersion > 0 {
			v := readView.LifecycleVersion
			expectedVer = &v
		}
	}

	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.complete", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "task.complete",
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
	var completed api.TaskCompleteResult
	_ = json.Unmarshal(result.Body, &completed)
	fmt.Fprintf(ports.Out, "completed task %s (lifecycle_version=%d)\n", taskID, completed.LifecycleVersion)
	return ExitSuccess
}

func resolveCompletesTaskFlag(seen map[string]bool, completesTaskFlag, noCompleteFlag bool) (*bool, error) {
	if seen["--completes-task"] && seen["--no-complete"] {
		return nil, fmt.Errorf("specify only one of --completes-task or --no-complete")
	}
	if seen["--no-complete"] {
		v := !noCompleteFlag
		return &v, nil
	}
	if seen["--completes-task"] {
		v := completesTaskFlag
		return &v, nil
	}
	return nil, nil
}

func runTaskList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "list"})
	}
	var (
		projectFlag string
		stateFlag   string
		parentFlag  string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&stateFlag, "--state", "--status")
	spec.stringFlag(&parentFlag, "--parent", "--parent-id")
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
		return usage(ports.Err, "task list requires --project <id> or KODEMA_PROJECT")
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	req := api.TaskListRequest{
		ProjectID: projectID,
		Status:    stateFlag,
		ParentID:  strings.TrimSpace(parentFlag),
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	tasks := []api.TaskSummaryView{}
	for {
		input, err := json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
		invocation := api.Invocation{
			Operation: "task.list",
			Request:   api.Request{Input: input},
		}
		if err := invocation.Validate(); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			return failure(ports.Err, err)
		}
		var page api.TaskListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return failure(ports.Err, fmt.Errorf("invalid task.list response: %w", err))
			}
		}
		tasks = append(tasks, page.Tasks...)
		if page.NextCursor == "" {
			break
		}
		req.Page = &api.Page{Cursor: page.NextCursor}
	}
	body, err := json.Marshal(api.TaskListResult{Tasks: tasks})
	if err != nil {
		return failure(ports.Err, err)
	}
	result := api.Result{Operation: "task.list", Body: body}
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	return renderTaskListTable(ports, result)
}

func renderTaskListTable(ports Ports, result api.Result) int {
	var body struct {
		Tasks []struct {
			TaskID           string `json:"task_id"`
			ID               string `json:"id"`
			Title            string `json:"title"`
			State            string `json:"state"`
			SelectedPipeline string `json:"selected_pipeline"`
			PipelineID       string `json:"pipeline_id"`
			CurrentExecution string `json:"current_execution"`
			LifecycleVersion uint64 `json:"lifecycle_version"`
		} `json:"tasks"`
	}
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &body); err != nil {
			return renderTableFallback(ports, result)
		}
	}
	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tSTATE\tPIPELINE\tEXECUTION\tTITLE")
	for _, item := range body.Tasks {
		id := item.TaskID
		if id == "" {
			id = item.ID
		}
		pipeline := item.SelectedPipeline
		if pipeline == "" {
			pipeline = item.PipelineID
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			orDash(id),
			orDash(item.State),
			orDash(pipeline),
			orDash(item.CurrentExecution),
			singleLine(item.Title, 80),
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func fetchTaskRead(ctx context.Context, ports Ports, taskID string) (api.Result, api.TaskReadResult, error) {
	input, err := json.Marshal(api.TaskReadRequest{TaskID: taskID})
	if err != nil {
		return api.Result{}, api.TaskReadResult{}, err
	}
	invocation := api.Invocation{
		Operation: "task.read",
		Request:   api.Request{Input: input},
	}
	if err := invocation.Validate(); err != nil {
		return api.Result{}, api.TaskReadResult{}, err
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return api.Result{}, api.TaskReadResult{}, err
	}
	var readView api.TaskReadResult
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &readView); err != nil {
			return result, api.TaskReadResult{}, fmt.Errorf("invalid task.read response: %w", err)
		}
	}
	return result, readView, nil
}

func renderTableFallback(ports Ports, result api.Result) int {
	if err := renderTable(ports.Out, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}
