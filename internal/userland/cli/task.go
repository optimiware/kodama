package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/optimiware/kodama/api"
)

// runTask dispatches the ergonomic `kodactl task` subcommands.
func runTask(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "task requires a subcommand (create|run|list|show|start|reply|cancel)")
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
	case "reply":
		return runTaskReply(ctx, config, args[1:], ports, deps)
	case "cancel":
		return runTaskCancel(ctx, config, args[1:], ports, deps)
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
		titleFlag          string
		descFlag           string
		pipelineFlag       string
		outputFlag         string
		idempotencyKeyFlag string
		inputPairs         []string
		metaPairs          []string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&titleFlag, "--title", "-t")
	spec.stringFlag(&descFlag, "--description", "-d")
	spec.stringFlag(&pipelineFlag, "--pipeline")
	spec.listFlag(&inputPairs, "--input")
	spec.listFlag(&metaPairs, "--meta", "--metadata")
	spec.stringFlag(&outputFlag, "--output")
	spec.stringFlag(&idempotencyKeyFlag, "--idempotency-key")
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
		return usage(ports.Err, fmt.Sprintf("task %s requires --project <id> or KODAMA_PROJECT", subcmd))
	}
	if strings.TrimSpace(titleFlag) == "" {
		return usage(ports.Err, fmt.Sprintf("task %s requires --title <text>", subcmd))
	}
	if start && strings.TrimSpace(pipelineFlag) == "" {
		return usage(ports.Err, "task run requires --pipeline <id>")
	}
	inputs, err := parseKeyValues(inputPairs, "input")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	metadata, err := parseKeyValues(metaPairs, "metadata")
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	var (
		operation string
		rawInput  []byte
	)
	if start {
		operation = "task.create_and_start"
		req := api.TaskCreateAndStartRequest{
			ProjectID:   projectID,
			Title:       titleFlag,
			Description: descFlag,
			PipelineID:  strings.TrimSpace(pipelineFlag),
			Inputs:      inputs,
			Metadata:    metadata,
		}
		if err := req.Validate(); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
		rawInput, err = json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
	} else {
		operation = "task.create"
		req := api.TaskCreateRequest{
			ProjectID:   projectID,
			Title:       titleFlag,
			Description: descFlag,
			PipelineID:  strings.TrimSpace(pipelineFlag),
			Inputs:      inputs,
			Metadata:    metadata,
		}
		if err := req.Validate(); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
		rawInput, err = json.Marshal(req)
		if err != nil {
			return failure(ports.Err, err)
		}
	}

	key, err := makeIdempotencyKey(idempotencyKeyFlag, operation, deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: operation,
		Request: api.Request{
			Input:          rawInput,
			IdempotencyKey: key,
		},
	}
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
	}
	return ExitSuccess
}

func runTaskStart(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "start"})
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
		return usage(ports.Err, "task start requires <task-id>")
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
	req := api.TaskStartRequest{TaskID: taskID}
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
	key, err := makeIdempotencyKey(idempotencyKeyFlag, "task.start", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
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
	var started api.TaskStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil || started.ExecutionID == "" {
		fmt.Fprintf(ports.Out, "started task %s\n", taskID)
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "task_id\t%s\nexecution_id\t%s\nlifecycle_version\t%d\n",
		taskID, started.ExecutionID, started.LifecycleVersion)
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

func runTaskList(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"task", "list"})
	}
	var (
		projectFlag string
		stateFlag   string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
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
		return usage(ports.Err, "task list requires --project <id> or KODAMA_PROJECT")
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if _, ok := api.LookupRoute("task.list"); ok {
		return runRegisteredTaskList(ctx, ports, projectID, stateFlag, format)
	}
	return runFallbackTaskList(ctx, ports, projectID, stateFlag, format)
}

func runRegisteredTaskList(ctx context.Context, ports Ports, projectID, stateFlag string, format OutputFormat) int {
	req := api.TaskListRequest{
		ProjectID: projectID,
		Status:    stateFlag,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
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
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	return renderTaskListTable(ports, result)
}

func runFallbackTaskList(ctx context.Context, ports Ports, projectID, stateFlag string, format OutputFormat) int {
	req := api.ExecutionListRequest{
		ProjectID: projectID,
		State:     stateFlag,
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
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
	if format == OutputJSON {
		if err := renderResult(ports.Out, OutputJSON, result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	var list api.ExecutionListResult
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &list); err != nil {
			return renderTableFallback(ports, result)
		}
	}
	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tEXECUTION\tPIPELINE\tSTATE\tVERSION")
	for _, exec := range list.Executions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			orDash(exec.TaskID),
			orDash(exec.ExecutionID),
			orDash(exec.PipelineID),
			orDash(exec.State),
			exec.Version,
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
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
