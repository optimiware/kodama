package opsapp

import (
	"context"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// modelHandlers binds the optional model and external tool surfaces. Their
// public Invoke paths authenticate from a credential the router does not
// forward, so the injected consumer-owned ports receive the already
// authenticated principal and bind it to the real gateway/proxy request.
func modelHandlers(deps Dependencies) map[authorizeOperation]ops.Handler {
	return map[authorizeOperation]ops.Handler{
		"model.invoke": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ModelInvokeRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			// Copy the complete tool-calling vocabulary. The gateway
			// marshals the request it is handed, so every field dropped here is
			// dropped from the provider body; the string-valued call arguments
			// and the explicit null-content marker must stay byte-identical.
			invocation := api.ChatCompletionRequest{
				Model:       request.Model,
				MaxTokens:   request.MaxTokens,
				Temperature: request.Temperature,
				Stream:      request.Stream,
				Tools:       request.Tools,
			}
			for _, message := range request.Messages {
				invocation.Messages = append(invocation.Messages, api.ChatMessage{
					Role:        message.Role,
					Content:     message.Content,
					ContentNull: message.ContentNull,
					ToolCalls:   message.ToolCalls,
					ToolCallID:  message.ToolCallID,
				})
			}
			response, err := deps.Models.Invoke(ctx, call.Principal, id.ProjectID(request.ProjectID), invocation)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, response)
		}),
		"tool.invoke": handlerFunc(func(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
			request, err := decodeRequest[api.ToolInvokeRequest](call)
			if err != nil {
				return api.Result{}, err
			}
			committed, err := deps.Tools.Call(
				ctx,
				call.Principal,
				id.ProjectID(request.ProjectID),
				request.Tool,
				request.Arguments,
			)
			if err != nil {
				return api.Result{}, err
			}
			return respond(call, api.ToolInvokeResult{
				Content: string(committed.Content),
				IsError: committed.IsError,
			})
		}),
	}
}
