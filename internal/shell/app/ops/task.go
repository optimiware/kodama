package opsapp

import (
	"context"
	"sort"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	shellrequest "github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// taskHandlers binds task creation, conversation, lifecycle and reads.
func taskHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"task.create": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			return createTask(ctx, deps, call, false)
		}),
		"task.create_and_start": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			return createTask(ctx, deps, call, true)
		}),
		"task.read": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskReadRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			current, err := deps.Tasks.Read(ctx, task.ReadCommand{
				Actor:  call.Principal,
				Scope:  call.Scope,
				TaskID: id.TaskID(request.TaskID),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, taskReadResult(current))
		}),
		"task.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskListRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			if err := confirmReadAuthority(ctx, deps, call, call.Scope); err != nil {
				return api.Result{}, err
			}
			scanner, err := requireScanner(deps)
			if err != nil {
				return api.Result{}, err
			}
			equals := map[string]any{}
			if request.Status != "" {
				equals["state"] = request.Status
			}
			page := pageOrDefault(request.Page)
			committed, err := scanner.Query(ctx, call.Scope, store.ScanQuery{
				Table:   "tasks",
				Equals:  equals,
				OrderBy: []store.Order{{Column: "id"}},
				After:   page.Cursor,
				Limit:   page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			tasks := make([]api.TaskSummaryView, 0, len(committed.Rows))
			for _, row := range committed.Rows {
				taskID, err := scanRowString(row, "id")
				if err != nil {
					return api.Result{}, err
				}
				title, err := scanRowString(row, "title")
				if err != nil {
					return api.Result{}, err
				}
				state, err := scanRowString(row, "state")
				if err != nil {
					return api.Result{}, err
				}
				selectedPipeline, err := scanRowNullableString(row, "pipeline_id")
				if err != nil {
					return api.Result{}, err
				}
				currentExecution, err := scanRowNullableString(row, "current_execution_id")
				if err != nil {
					return api.Result{}, err
				}
				contentVersion, err := scanRowUint(row, "content_version")
				if err != nil {
					return api.Result{}, err
				}
				lifecycleVersion, err := scanRowUint(row, "lifecycle_version")
				if err != nil {
					return api.Result{}, err
				}
				tasks = append(tasks, api.TaskSummaryView{
					TaskID:           taskID,
					Title:            title,
					State:            state,
					SelectedPipeline: selectedPipeline,
					CurrentExecution: currentExecution,
					ContentVersion:   contentVersion,
					LifecycleVersion: lifecycleVersion,
				})
			}
			return respond(call, api.TaskListResult{
				Tasks:      tasks,
				NextCursor: committed.Next,
			})
		}),
		"task.edit": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskEditRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			metadata, err := metadataBytes(request.Metadata)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Edit(ctx, task.EditCommand{
				Actor:                  call.Principal,
				Scope:                  call.Scope,
				TaskID:                 id.TaskID(request.TaskID),
				Kind:                   task.EditContent,
				ExpectedContentVersion: version,
				Title:                  request.Title,
				Description:            request.Description,
				Metadata:               metadata,
				CallerKey:              call.Request.IdempotencyKey,
				Content:                receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			lifecycle, err := currentLifecycleVersion(ctx, deps, call, request.TaskID)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskEditResult{
				ContentVersion:   committed.ContentVersion,
				LifecycleVersion: lifecycle,
			})
		}),
		"task.comment": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskCommentRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Edit(ctx, task.EditCommand{
				Actor:                  call.Principal,
				Scope:                  call.Scope,
				TaskID:                 id.TaskID(request.TaskID),
				Kind:                   task.EditComment,
				ExpectedContentVersion: version,
				Body:                   request.Body,
				References:             append([]string(nil), request.References...),
				CallerKey:              call.Request.IdempotencyKey,
				Content:                receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			lifecycle, err := currentLifecycleVersion(ctx, deps, call, request.TaskID)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskCommentResult{
				ContentVersion:   committed.ContentVersion,
				LifecycleVersion: lifecycle,
			})
		}),
		"task.select_pipeline": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskSelectPipelineRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Edit(ctx, task.EditCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				TaskID:                   id.TaskID(request.TaskID),
				Kind:                     task.EditSelectPipeline,
				ExpectedLifecycleVersion: version,
				PipelineID:               pipelineIDOf(request.PipelineID),
				Inputs:                   inputsOf(request.Inputs),
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskSelectPipelineResult{LifecycleVersion: committed.LifecycleVersion})
		}),
		"task.start": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskStartRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Start(ctx, task.StartCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				TaskID:                   id.TaskID(request.TaskID),
				ExpectedLifecycleVersion: version,
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskStartResult{
				ExecutionID:      string(committed.ExecutionID),
				LifecycleVersion: committed.LifecycleVersion,
			})
		}),
		"task.ask": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskAskRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			assignment, err := shellrequest.ParseAssignment(request.Assignment)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Ask(ctx, task.AskCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				TaskID:                   id.TaskID(request.TaskID),
				ExpectedLifecycleVersion: version,
				Question:                 request.Question,
				Assignment:               assignment,
				References:               append([]string(nil), request.References...),
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			// The capability result carries no question identity. Fill it from
			// the committed question when it is still outstanding; a replay
			// after the question was answered omits it (the public field is
			// optional) instead of failing.
			result := api.TaskAskResult{
				LifecycleVersion: committed.LifecycleVersion,
				QuestionVersion:  committed.QuestionVersion,
			}
			// An execution's own ask cancels that execution and clears its
			// mounted credential in the same commit. Re-reading the task
			// through the stopped execution authority would be refused after a
			// successful handoff, so the optional question identity is omitted
			// for an execution principal. Clients learn it from task.read.
			if call.Principal.Kind != identity.KindExecution {
				current, err := deps.Tasks.Read(ctx, task.ReadCommand{
					Actor:  call.Principal,
					Scope:  call.Scope,
					TaskID: id.TaskID(request.TaskID),
				})
				if err != nil {
					return api.Result{}, err
				}
				if current.Question != nil {
					result.QuestionID = string(current.Question.ID)
				}
			}
			return respond(call, result)
		}),
		"task.reply": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskReplyRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Reply(ctx, task.ReplyCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				TaskID:                   id.TaskID(request.TaskID),
				ExpectedLifecycleVersion: version,
				QuestionID:               task.QuestionID(request.QuestionID),
				ExpectedQuestionVersion:  request.ExpectedQuestionVersion,
				Answer:                   request.Answer,
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskReplyResult{
				ExecutionID:      string(committed.ExecutionID),
				LifecycleVersion: committed.LifecycleVersion,
				QuestionVersion:  committed.QuestionVersion,
			})
		}),
		"task.cancel": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskCancelRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tasks.Cancel(ctx, task.CancelCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				TaskID:                   id.TaskID(request.TaskID),
				ExpectedLifecycleVersion: version,
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.TaskCancelResult{LifecycleVersion: committed.LifecycleVersion})
		}),
		"task.history": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskHistoryRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			committed, err := deps.Tasks.History(ctx, task.HistoryCommand{
				Actor:  call.Principal,
				Scope:  call.Scope,
				TaskID: id.TaskID(request.TaskID),
				Limit:  page.EffectiveLimit(),
				Cursor: page.Cursor,
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.TaskHistoryResult{NextCursor: committed.NextCursor}
			for _, entry := range committed.Entries {
				result.Entries = append(result.Entries, historyEntry(entry))
			}
			return respond(call, result)
		}),
		"task.inbox": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.TaskInboxRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			committed, err := deps.Inbox.Query(ctx, inbox.QueryCommand{
				Actor:  call.Principal,
				Scope:  call.Scope,
				Kinds:  inboxKinds(request.Kinds),
				Cursor: page.Cursor,
				Limit:  page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.TaskInboxResult{NextCursor: committed.NextCursor}
			for _, item := range committed.Items {
				mapped, err := inboxItem(item)
				if err != nil {
					return api.Result{}, err
				}
				result.Items = append(result.Items, mapped)
			}
			return respond(call, result)
		}),
	}
}

// inboxItem maps one inbox observation onto the public wire item. The waiting
// question projection is copied only for a waiting_task item: a request or
// failed_task item never carries the question identity, body, references or
// lifecycle version, and Version stays the question version for a waiting item.
func inboxItem(item inbox.Item) (api.InboxItem, error) {
	mapped := api.InboxItem{
		Kind:        string(item.Kind),
		TaskID:      string(item.TaskID),
		ExecutionID: string(item.ExecutionID),
		ActionID:    string(item.ActionID),
		Assignment:  item.Assignment.String(),
		Version:     item.Version,
		At:          item.At,
	}
	switch item.Kind {
	case inbox.KindRequest:
		subject, err := publicSubject(item.Subject)
		if err != nil {
			return api.InboxItem{}, err
		}
		mapped.Subject = subject
		mapped.Outputs = publicOutputNames(item.Outputs)
	case inbox.KindWaitingTask:
		mapped.QuestionID = string(item.QuestionID)
		mapped.Question = item.Question
		mapped.References = item.References
		mapped.LifecycleVersion = item.LifecycleVersion
	}
	return mapped, nil
}

// publicSubject maps the immutable resolved subject onto the ordered public
// typed field form. The stored subject is a map, so the fields are sorted by
// name for a deterministic wire projection; only a resolved literal is ever
// exposed. An unresolved runtime lookup is corruption: it fails the read closed
// rather than being dropped, partially projected or fabricated as an empty
// value.
func publicSubject(subject *definition.Subject) (*api.Subject, error) {
	if subject == nil {
		return nil, nil
	}
	fields := subject.Fields()
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	mapped := &api.Subject{Fields: make([]api.SubjectField, 0, len(names))}
	for _, name := range names {
		value := fields[name]
		if value.Kind != definition.SubjectLiteral {
			return nil, fault.New(fault.InternalFailure, "a request subject carries an unresolved lookup")
		}
		mapped.Fields = append(mapped.Fields, api.SubjectField{
			Name:  name,
			Kind:  "string",
			Value: value.Literal,
		})
	}
	if len(mapped.Fields) == 0 {
		return nil, fault.New(fault.InternalFailure, "a request subject is empty")
	}
	return mapped, nil
}

// publicOutputNames maps the declared response output names in declaration
// order.
func publicOutputNames(outputs []id.OutputKey) []string {
	if len(outputs) == 0 {
		return nil
	}
	mapped := make([]string, len(outputs))
	for index, output := range outputs {
		mapped[index] = string(output)
	}
	return mapped
}

// createTask maps task.create and task.create_and_start onto one command.
func createTask(ctx context.Context, deps Dependencies, call ops.AuthorizedCall, start bool) (api.Result, error) {
	if start {
		request, err := decodeRequest[api.TaskCreateAndStartRequest](call)
		if err != nil {
			return api.Result{}, err
		}
		committed, err := deps.Tasks.Create(ctx, task.CreateCommand{
			Actor:       call.Principal,
			Scope:       call.Scope,
			Title:       request.Title,
			Description: request.Description,
			Metadata:    mustMetadata(request.Metadata),
			PipelineID:  pipelineIDOf(request.PipelineID),
			Inputs:      inputsOf(request.Inputs),
			Start:       true,
			CallerKey:   call.Request.IdempotencyKey,
			Content:     receiptContent(call, request),
		})
		if err != nil {
			return api.Result{}, err
		}
		return respond(call, api.TaskCreateAndStartResult{
			TaskID:           string(committed.TaskID),
			ExecutionID:      string(committed.ExecutionID),
			LifecycleVersion: committed.LifecycleVersion,
		})
	}
	request, err := decodeRequest[api.TaskCreateRequest](call)
	if err != nil {
		return api.Result{}, err
	}
	committed, err := deps.Tasks.Create(ctx, task.CreateCommand{
		Actor:       call.Principal,
		Scope:       call.Scope,
		Title:       request.Title,
		Description: request.Description,
		Metadata:    mustMetadata(request.Metadata),
		PipelineID:  pipelineIDOf(request.PipelineID),
		Inputs:      inputsOf(request.Inputs),
		Start:       false,
		CallerKey:   call.Request.IdempotencyKey,
		Content:     receiptContent(call, request),
	})
	if err != nil {
		return api.Result{}, err
	}
	return respond(call, api.TaskCreateResult{
		TaskID:           string(committed.TaskID),
		ContentVersion:   committed.ContentVersion,
		LifecycleVersion: committed.LifecycleVersion,
	})
}

// currentLifecycleVersion reads the committed lifecycle version of a task. The
// capability edit result carries only the version the edit changed, while the
// public result carries both.
func currentLifecycleVersion(ctx context.Context, deps Dependencies, call ops.AuthorizedCall, taskID string) (uint64, error) {
	current, err := deps.Tasks.Read(ctx, task.ReadCommand{
		Actor:  call.Principal,
		Scope:  call.Scope,
		TaskID: id.TaskID(taskID),
	})
	if err != nil {
		return 0, err
	}
	return current.LifecycleVersion, nil
}

// taskReadResult maps one committed task onto the public read result.
func taskReadResult(current task.Task) api.TaskReadResult {
	result := api.TaskReadResult{
		TaskID:           string(current.ID),
		Title:            current.Title,
		Description:      current.Description,
		State:            string(current.State),
		ContentVersion:   current.ContentVersion,
		LifecycleVersion: current.LifecycleVersion,
		SelectedPipeline: string(current.SelectedPipeline),
		Inputs:           inputsToWire(current.Inputs),
		Metadata:         cloneMetadata(current.Metadata),
		Initiator:        string(current.Initiator),
		CurrentExecution: string(current.CurrentExecution),
	}
	for _, message := range current.Messages {
		result.Messages = append(result.Messages, api.TaskMessage{
			Author:     string(message.Author),
			At:         message.At,
			Body:       message.Body,
			References: append([]string(nil), message.References...),
		})
	}
	for _, executionID := range current.ExecutionHistory {
		result.ExecutionHistory = append(result.ExecutionHistory, string(executionID))
	}
	if current.Question != nil {
		result.Question = &api.TaskQuestion{
			QuestionID: string(current.Question.ID),
			Body:       current.Question.Body,
			Assignment: current.Question.Assignment.String(),
			References: append([]string(nil), current.Question.References...),
			Version:    current.Question.Version,
		}
	}
	return result
}

// historyEntry maps one committed history entry.
func historyEntry(entry task.HistoryEntry) api.TaskHistoryEntry {
	converted := api.TaskHistoryEntry{
		Kind:        string(entry.Kind),
		At:          entry.At,
		ExecutionID: string(entry.ExecutionID),
		State:       string(entry.State),
	}
	if entry.Message != nil {
		converted.Message = &api.TaskMessage{
			Author:     string(entry.Message.Author),
			At:         entry.Message.At,
			Body:       entry.Message.Body,
			References: append([]string(nil), entry.Message.References...),
		}
	}
	if entry.Question != nil {
		converted.Question = &api.TaskQuestion{
			QuestionID: string(entry.Question.ID),
			Body:       entry.Question.Body,
			Assignment: entry.Question.Assignment.String(),
			References: append([]string(nil), entry.Question.References...),
			Version:    entry.Question.Version,
		}
	}
	return converted
}

// inboxKinds converts public inbox kind names.
func inboxKinds(names []string) []inbox.Kind {
	if names == nil {
		return nil
	}
	kinds := make([]inbox.Kind, len(names))
	for index, name := range names {
		kinds[index] = inbox.Kind(name)
	}
	return kinds
}

// inputsToWire converts domain inputs to public names.
func inputsToWire(inputs map[id.InputKey]string) map[string]string {
	if inputs == nil {
		return nil
	}
	converted := make(map[string]string, len(inputs))
	for key, value := range inputs {
		converted[string(key)] = value
	}
	return converted
}

// mustMetadata encodes metadata that the public DTO already bounded. A failure
// cannot happen for a string map; nil is returned defensively.
func mustMetadata(metadata map[string]string) []byte {
	encoded, err := metadataBytes(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

// pipelineIDOf converts a public pipeline identity.
func pipelineIDOf(value string) pipeline.PipelineID { return pipeline.PipelineID(value) }
