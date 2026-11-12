package opsapp

import (
	"context"
	"encoding/json"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// pipelineHandlers binds pipeline authoring and the immutable task-free start.
func pipelineHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"pipeline.apply": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.PipelineApplyRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Pipelines.Apply(ctx, pipeline.ApplyCommand{
				Actor:     call.Principal,
				Scope:     call.Scope,
				Name:      request.Name,
				YAML:      append([]byte(nil), request.YAML...),
				CallerKey: call.Request.IdempotencyKey,
				Content:   receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PipelineApplyResult{
				PipelineID: string(committed.PipelineID),
				Version:    committed.Version,
			})
		}),
		"pipeline.read": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.PipelineReadRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			definition, err := deps.Pipelines.Read(ctx, pipeline.ReadCommand{
				Actor:      call.Principal,
				Scope:      call.Scope,
				PipelineID: pipeline.PipelineID(request.PipelineID),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PipelineReadResult{
				PipelineID: string(definition.ID),
				Name:       definition.Name,
				Version:    definition.Version,
				YAML:       append([]byte(nil), definition.YAML...),
			})
		}),
		"pipeline.update": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.PipelineUpdateRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Pipelines.Update(ctx, pipeline.UpdateCommand{
				Actor:           call.Principal,
				Scope:           call.Scope,
				PipelineID:      pipeline.PipelineID(request.PipelineID),
				Name:            request.Name,
				YAML:            append([]byte(nil), request.YAML...),
				ExpectedVersion: version,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PipelineUpdateResult{Version: committed.Version})
		}),
		"pipeline.delete": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.PipelineDeleteRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			version, err := requiredVersion(call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Pipelines.Delete(ctx, pipeline.DeleteCommand{
				Actor:           call.Principal,
				Scope:           call.Scope,
				PipelineID:      pipeline.PipelineID(request.PipelineID),
				ExpectedVersion: version,
				CallerKey:       call.Request.IdempotencyKey,
				Content:         receiptContent(call, request),
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PipelineDeleteResult{Version: committed.Version})
		}),
		"pipeline.start": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.PipelineStartRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			// The idempotency content pins the exclusive selection before any
			// admission or identity allocation: an inline document contributes
			// only its exact SHA-256 and inputs, while a stored selection
			// requires the envelope's positive expected pipeline version.
			content, err := api.PipelineStartIdempotencyContent(request, call.Request.ExpectedVersion)
			if err != nil {
				return api.Result{}, wireRefusal(err)
			}
			// Receipt-first: an equal retry of an already committed start must
			// return the original execution before the edited definition is
			// admitted. Without this, a later pipeline.apply/update (or delete)
			// would make the current definition, inputs or version decide the
			// replay and turn an idempotent retry into a refusal.
			if replayed, found, err := replayPipelineStart(ctx, deps, call, content); err != nil {
				return api.Result{}, err
			} else if found {
				return respond(call, replayed)
			}
			// A stored selection admits the reader-resolved definition; inline
			// YAML is admitted from its copied exact bytes with no stored
			// pipeline identity or version and no task context.
			definition := pipeline.Definition{}
			if request.PipelineID != "" {
				definition.ID = pipeline.PipelineID(request.PipelineID)
			} else {
				definition.YAML = append([]byte(nil), request.YAML...)
			}
			// Admit next: the admission service validates the selection, inputs
			// and image digests and returns the immutable snapshot and ordered
			// action rows. No effect happens before it.
			admitted, err := deps.Admission.Admit(ctx, pipeline.AdmitRequest{
				Scope:          call.Scope,
				Definition:     definition,
				Inputs:         inputsOf(request.Inputs),
				DefaultTimeout: deps.ActionTimeout,
			})
			if err != nil {
				return api.Result{}, err
			}
			// The execution principal is a fresh capability identity; the
			// authenticated principal is the initiator.
			executionPrincipal, err := deps.IDs.NewPrincipalID()
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Executions.Start(ctx, execution.StartCommand{
				Actor:                   call.Principal,
				Scope:                   call.Scope,
				Admission:               admitted,
				ExpectedPipelineVersion: call.Request.ExpectedVersion,
				Initiator:               call.Principal.ID,
				ExecutionPrincipal:      executionPrincipal,
				CallerKey:               call.Request.IdempotencyKey,
				Content:                 receipt.Content{MediaType: api.MediaType, Bytes: content},
			})
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.PipelineStartResult{
				ExecutionID: string(committed.ExecutionID),
				Version:     committed.ExecutionVersion,
			})
		}),
	}
}

// replayPipelineStart answers an equal pipeline.start retry from the durable
// receipt before any admission or new identity is attempted. It returns
// found=false only when no receipt exists for the exact scoped caller key, in
// which case the handler continues with a fresh admission. A reused key with
// different content is refused and never replays.
func replayPipelineStart(
	ctx context.Context,
	deps Dependencies,
	call ops.AuthorizedCall,
	content []byte,
) (api.PipelineStartResult, bool, error) {
	if call.Request.IdempotencyKey == "" {
		return api.PipelineStartResult{}, false, nil
	}
	key := receipt.Key{
		TenantID:    call.Scope.TenantID,
		ProjectID:   call.Scope.ProjectID,
		PrincipalID: call.Principal.ID,
		Operation:   string(call.Operation.Name),
		CallerKey:   call.Request.IdempotencyKey,
	}
	record, found, err := deps.Receipts.Lookup(ctx, key)
	if err != nil || !found {
		return api.PipelineStartResult{}, false, err
	}
	fingerprint, err := receipt.Fingerprint(receipt.Content{MediaType: api.MediaType, Bytes: content})
	if err != nil {
		return api.PipelineStartResult{}, false, err
	}
	outcome, err := receipt.Match(&record, receipt.Record{Key: key, ContentHash: fingerprint})
	if err != nil {
		return api.PipelineStartResult{}, false, err
	}
	if outcome != receipt.OutcomeReplay {
		return api.PipelineStartResult{}, false, fault.New(fault.InternalFailure,
			"pipeline.start receipt lookup returned a different identity than its key")
	}
	var committed execution.StartResult
	if len(record.Result) == 0 {
		return api.PipelineStartResult{}, false, fault.New(fault.InternalFailure,
			"replayed pipeline.start receipt has no committed result")
	}
	if err := json.Unmarshal(record.Result, &committed); err != nil {
		return api.PipelineStartResult{}, false, fault.Wrap(fault.InternalFailure,
			"replayed pipeline.start result is malformed", err)
	}
	if committed.ExecutionID == "" || committed.ExecutionVersion == 0 {
		return api.PipelineStartResult{}, false, fault.New(fault.InternalFailure,
			"replayed pipeline.start result has no execution identity or version")
	}
	return api.PipelineStartResult{
		ExecutionID: string(committed.ExecutionID),
		Version:     committed.ExecutionVersion,
	}, true, nil
}
