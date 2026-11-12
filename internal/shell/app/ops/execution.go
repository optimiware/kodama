package opsapp

import (
	"context"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/log"
	shellrequest "github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// executionHandlers binds execution inspection, cancellation, staged outputs,
// request resolution and retained logs.
func executionHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"execution.inspect": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionInspectRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			view, err := deps.Executions.Inspect(ctx, execution.InspectCommand{
				Actor:       call.Principal,
				Scope:       call.Scope,
				ExecutionID: id.ExecutionID(request.ExecutionID),
			})
			if err != nil {
				return api.Result{}, err
			}
			result, err := inspectResult(view)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, result)
		}),
		"execution.list": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionListRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			committed, err := deps.Executions.List(ctx, execution.ListCommand{
				Actor: call.Principal,
				Scope: call.Scope,
				Filter: execution.ListFilter{
					TaskID:     id.TaskID(request.TaskID),
					PipelineID: pipelineIDOf(request.PipelineID),
					State:      kernel.ExecutionState(request.State),
					Limit:      page.EffectiveLimit(),
					Cursor:     page.Cursor,
				},
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.ExecutionListResult{NextCursor: committed.Next}
			for _, view := range committed.Items {
				result.Executions = append(result.Executions, api.ExecutionSummary{
					ExecutionID: string(view.ExecutionID),
					TaskID:      string(view.TaskID),
					PipelineID:  string(view.PipelineID),
					State:       string(view.State),
					Version:     view.Version,
				})
			}
			return respond(call, result)
		}),
		"execution.cancel": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionCancelRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Executions.Cancel(ctx, execution.CancelCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				ExecutionID:              id.ExecutionID(request.ExecutionID),
				ExpectedExecutionVersion: version,
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ExecutionCancelResult{ExecutionVersion: committed.ExecutionVersion})
		}),
		"execution.action.outputs": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionActionOutputsRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Executions.StageOutputs(ctx, execution.OutputsCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				ExecutionID:              id.ExecutionID(request.ExecutionID),
				ActionID:                 id.ActionID(request.ActionID),
				ExpectedExecutionVersion: version,
				Outputs:                  kernel.Outputs(outputsOf(request.Outputs)),
				CallerKey:                call.Request.IdempotencyKey,
				Content:                  receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ExecutionActionOutputsResult{
				ExecutionVersion: committed.ExecutionVersion,
				ActionVersion:    committed.ActionVersion,
			})
		}),
		"execution.request.resolve": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionRequestResolveRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			// The body is action-only. An explicitly supplied shared envelope
			// expected_version remains the expected execution version and is
			// checked against the observed state inside the commit; an omitted
			// envelope version derives the internal commit versions from that
			// observed state.
			committed, err := deps.Requests.Resolve(ctx, shellrequest.ResolveCommand{
				Actor:                    call.Principal,
				Scope:                    call.Scope,
				ExecutionID:              id.ExecutionID(request.ExecutionID),
				ActionID:                 id.ActionID(request.ActionID),
				ExpectedExecutionVersion: expectedVersion(call),
				ExpectedActionVersion:    request.ExpectedActionVersion,
				Decision:                 shellrequest.Decision(request.Decision),
				Response:                 kernel.Outputs(outputsOf(request.Outputs)),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ExecutionRequestResolveResult{
				ExecutionVersion: committed.ExecutionVersion,
				ActionVersion:    committed.ActionVersion,
			})
		}),
		"execution.logs.read": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ExecutionLogsReadRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			page := request.Page
			if page == nil {
				page = &api.Page{}
			}
			// A human or service principal must address an execution and action
			// visible in its resolved project scope. FileSink returns an empty
			// page when no stream file exists on disk, which is valid for an
			// in-scope action before collection, so visibility is checked
			// against the scoped execution store first; an execution principal
			// keeps its own-task check inside deps.Logs.Read.
			if call.Principal.Kind != identity.KindExecution {
				view, err := deps.Executions.Inspect(ctx, execution.InspectCommand{
					Actor:       call.Principal,
					Scope:       call.Scope,
					ExecutionID: id.ExecutionID(request.ExecutionID),
				})
				if err != nil {
					return api.Result{}, err
				}
				if !hasAction(view.Actions, id.ActionID(request.ActionID)) {
					return api.Result{}, fault.New(fault.NotFound, "action is not visible in this execution")
				}
			}
			committed, err := deps.Logs.Read(ctx, log.ReadCommand{
				Actor:       call.Principal,
				Scope:       call.Scope,
				ExecutionID: id.ExecutionID(request.ExecutionID),
				ActionID:    id.ActionID(request.ActionID),
				Stream:      log.Stream(request.Stream),
				Cursor:      page.Cursor,
				Limit:       page.EffectiveLimit(),
			})
			if err != nil {
				return api.Result{}, err
			}
			result := api.ExecutionLogsReadResult{
				NextCursor: committed.NextCursor,
				Truncated:  committed.Truncated,
			}
			for _, chunk := range committed.Chunks {
				result.Chunks = append(result.Chunks, api.LogChunk{
					ActionID: string(chunk.ActionID),
					Stream:   string(chunk.Stream),
					Offset:   chunk.Offset,
					Sequence: chunk.Sequence,
					Data:     append([]byte(nil), chunk.Data...),
				})
			}
			return respond(call, result)
		}),
	}
}

// inspectResult maps one committed execution view onto the public result.
func inspectResult(view execution.View) (api.ExecutionInspectResult, error) {
	result := api.ExecutionInspectResult{
		ExecutionID:   string(view.ExecutionID),
		TaskID:        string(view.TaskID),
		PipelineID:    string(view.PipelineID),
		State:         string(view.State),
		Version:       view.Version,
		CurrentAction: view.CurrentAction,
		Cleanup: api.ExecutionCleanup{
			State:       string(view.Cleanup.State),
			WorkloadID:  string(view.Cleanup.WorkloadID),
			WorkspaceID: string(view.Cleanup.WorkspaceID),
			Attempts:    view.Cleanup.Attempts,
			LastError:   view.Cleanup.LastError,
		},
	}
	for _, action := range view.Actions {
		converted, err := inspectAction(action)
		if err != nil {
			return api.ExecutionInspectResult{}, err
		}
		result.Actions = append(result.Actions, converted)
	}
	if view.Failure != nil {
		result.Failure = &api.ExecutionFailure{
			Kind:   string(view.Failure.Kind),
			Reason: view.Failure.Reason,
			Detail: view.Failure.Detail,
		}
	}
	return result, nil
}

// inspectAction maps one action view. Staged outputs, workspace identities and
// the declared runtime lookups are never exposed: a request action exposes only
// its immutable resolved subject and its exact assignment. A subject that still
// carries an unresolved lookup, or a running request without one, is corruption
// and fails the read closed instead of fabricating a partial subject.
func inspectAction(action execution.ActionView) (api.ExecutionAction, error) {
	converted := api.ExecutionAction{
		ActionID:       string(action.ID),
		Index:          action.Index,
		Name:           string(action.Name),
		Kind:           string(action.Kind),
		State:          string(action.State),
		Version:        action.Version,
		TimeoutSeconds: int64(action.Timeout / time.Second),
		Outputs:        stringsOf(action.Outputs),
		Decision:       action.Decision,
		Actor:          string(action.Actor),
	}
	if action.Kind == definition.ActionKindRequest {
		if action.State == kernel.ActionRunning && action.Subject == nil {
			return api.ExecutionAction{}, fault.New(fault.InternalFailure, "a running request action has no stored subject")
		}
		subject, err := publicSubject(action.Subject)
		if err != nil {
			return api.ExecutionAction{}, err
		}
		converted.Subject = subject
		converted.Assignment = action.Assignment.String()
	}
	if !action.ActivatedAt.IsZero() {
		activated := action.ActivatedAt
		converted.ActivatedAt = &activated
	}
	if !action.Deadline.IsZero() {
		deadline := action.Deadline
		converted.Deadline = &deadline
	}
	if action.Stop.State != "" {
		converted.Stop = &api.ExecutionStopEvidence{
			State:  string(action.Stop.State),
			Detail: action.Stop.Detail,
		}
	}
	if !action.AnsweredAt.IsZero() {
		answered := action.AnsweredAt
		converted.AnsweredAt = &answered
	}
	return converted, nil
}

// hasAction reports whether actions contains target.
func hasAction(actions []execution.ActionView, target id.ActionID) bool {
	for _, action := range actions {
		if action.ID == target {
			return true
		}
	}
	return false
}
