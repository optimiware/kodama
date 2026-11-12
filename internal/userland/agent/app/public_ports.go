package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// decodePublicResult checks the operation identity and decodes one result body.
func decodePublicResult(result api.Result, operation string, target any) error {
	if result.Operation != operation {
		return fault.New(fault.InternalFailure, fmt.Sprintf("public result names operation %q, not %q", result.Operation, operation))
	}
	if len(result.Body) == 0 {
		return fault.New(fault.InternalFailure, fmt.Sprintf("public result %q has no body", operation))
	}
	if err := json.Unmarshal(result.Body, target); err != nil {
		return fault.Wrap(fault.InternalFailure, fmt.Sprintf("decode public result %q", operation), err)
	}
	return nil
}

// PublicModel implements agent.ModelPort over the public `model.invoke`
// operation. The public subset is non-streaming. The provider-neutral
// declarations already carry the provider-compatible function names, so
// the bridge copies advertised declarations, the assistant call list and the
// tool-result identities unchanged and reverses the name translation only on
// the way back to the native runtime.
type PublicModel struct {
	transport client.Transport
	projectID string
}

var _ agent.ModelPort = (*PublicModel)(nil)

// NewPublicModel validates the boundaries and returns the model port.
func NewPublicModel(transport client.Transport, projectID string) (*PublicModel, error) {
	if transport == nil {
		return nil, fmt.Errorf("model port transport is missing")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("model port project is empty")
	}
	return &PublicModel{transport: transport, projectID: projectID}, nil
}

// Invoke performs one public model invocation.
func (model *PublicModel) Invoke(ctx context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	if model == nil || model.transport == nil {
		return agent.ModelResponse{}, fault.New(fault.InternalFailure, "model port is not configured")
	}
	if err := request.Validate(); err != nil {
		return agent.ModelResponse{}, fault.Wrap(fault.InvalidInput, "model request is invalid", err)
	}
	messages := make([]api.ChatMessage, len(request.Messages))
	for index, message := range request.Messages {
		mapped := api.ChatMessage{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: string(message.ToolCallID),
		}
		// The agent message has no null marker: an empty string stays an empty
		// string, never an inferred JSON null.
		if len(message.ToolCalls) > 0 {
			calls := make([]api.ChatToolCall, len(message.ToolCalls))
			for callIndex, call := range message.ToolCalls {
				calls[callIndex] = api.ChatToolCall{
					ID:   call.ID,
					Type: "function",
					Function: api.ChatToolCallFunction{
						Name:      call.Name,
						Arguments: call.Arguments,
					},
				}
			}
			mapped.ToolCalls = calls
		}
		messages[index] = mapped
	}
	tools := make([]api.ChatTool, len(request.Tools))
	for index, tool := range request.Tools {
		tools[index] = api.ChatTool{
			Type: "function",
			Function: api.ChatToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		}
	}
	publicRequest := api.ModelInvokeRequest{
		ProjectID:   model.projectID,
		Model:       request.Model,
		Messages:    messages,
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
		Tools:       tools,
	}
	if err := publicRequest.Validate(); err != nil {
		return agent.ModelResponse{}, fault.Wrap(fault.InvalidInput, "mapped model request is invalid", err)
	}
	input, err := json.Marshal(publicRequest)
	if err != nil {
		return agent.ModelResponse{}, fault.Wrap(fault.InternalFailure, "model request cannot be encoded", err)
	}
	result, err := model.transport.Invoke(ctx, api.Invocation{
		Operation: "model.invoke",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		return agent.ModelResponse{}, err
	}
	var completion api.ChatCompletionResponse
	if err := decodePublicResult(result, "model.invoke", &completion); err != nil {
		return agent.ModelResponse{}, err
	}
	if err := completion.Validate(); err != nil {
		return agent.ModelResponse{}, fault.Wrap(fault.InternalFailure, "public model result is invalid", err)
	}
	choice := completion.Choices[0]
	finishReason := choice.FinishReason
	switch finishReason {
	case "":
		finishReason = "stop"
	case "stop", "tool_calls", "length", "content_filter":
	default:
		return agent.ModelResponse{}, fault.New(fault.InternalFailure, "public model result has an unknown finish reason")
	}
	// Nullable provider content carries no separate marker into the native
	// runtime: it stays the existing empty-string representation.
	var calls []agent.ToolRequest
	if len(choice.Message.ToolCalls) > 0 {
		calls = make([]agent.ToolRequest, len(choice.Message.ToolCalls))
		for index, call := range choice.Message.ToolCalls {
			native, err := agent.NativeToolName(call.Function.Name)
			if err != nil {
				return agent.ModelResponse{}, fault.Wrap(fault.InternalFailure, "public model result names an untranslatable tool", err)
			}
			mapped := agent.ToolRequest{
				ID:        call.ID,
				Name:      native,
				Arguments: json.RawMessage(call.Function.Arguments),
			}
			if err := mapped.Validate(); err != nil {
				return agent.ModelResponse{}, fault.Wrap(fault.InternalFailure, "public model result carries an invalid tool call", err)
			}
			calls[index] = mapped
		}
	}
	response := agent.ModelResponse{
		Content: choice.Message.Content,
		Usage: agent.Usage{
			PromptTokens:     completion.Usage.PromptTokens,
			CompletionTokens: completion.Usage.CompletionTokens,
			TotalTokens:      completion.Usage.TotalTokens,
		},
		FinishReason: finishReason,
		ToolCalls:    calls,
	}
	if err := response.Validate(); err != nil {
		return agent.ModelResponse{}, fault.Wrap(fault.InternalFailure, "public model response is invalid", err)
	}
	return response, nil
}

// PublicTools implements agent.ToolPort over the public `tool.invoke`
// operation.
type PublicTools struct {
	transport client.Transport
	projectID string
}

var _ agent.ToolPort = (*PublicTools)(nil)

// NewPublicTools validates the boundaries and returns the tool port.
func NewPublicTools(transport client.Transport, projectID string) (*PublicTools, error) {
	if transport == nil {
		return nil, fmt.Errorf("tool port transport is missing")
	}
	if strings.TrimSpace(projectID) == "" {
		return nil, fmt.Errorf("tool port project is empty")
	}
	return &PublicTools{transport: transport, projectID: projectID}, nil
}

// Invoke performs one public tool invocation.
func (tools *PublicTools) Invoke(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	if tools == nil || tools.transport == nil {
		return agent.ToolResult{}, fault.New(fault.InternalFailure, "tool port is not configured")
	}
	if err := request.Validate(); err != nil {
		return agent.ToolResult{}, fault.Wrap(fault.InvalidInput, "tool request is invalid", err)
	}
	arguments := request.Arguments
	if len(arguments) == 0 || strings.TrimSpace(string(arguments)) == "null" {
		arguments = json.RawMessage("{}")
	}
	input, err := json.Marshal(api.ToolInvokeRequest{
		ProjectID: tools.projectID,
		Tool:      request.Name,
		Arguments: arguments,
	})
	if err != nil {
		return agent.ToolResult{}, fault.Wrap(fault.InternalFailure, "tool request cannot be encoded", err)
	}
	result, err := tools.transport.Invoke(ctx, api.Invocation{
		Operation: "tool.invoke",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	var invoked api.ToolInvokeResult
	if err := decodePublicResult(result, "tool.invoke", &invoked); err != nil {
		return agent.ToolResult{}, err
	}
	if err := invoked.Validate(); err != nil {
		return agent.ToolResult{}, fault.Wrap(fault.InternalFailure, "public tool result is invalid", err)
	}
	return agent.ToolResult{Name: request.Name, Content: invoked.Content, IsError: invoked.IsError}, nil
}

// FileKnowledge implements agent.KnowledgePort over a confined, bounded
// filesystem read. The root is pinned with an open handle and every leaf is
// opened through it, so a symlink or a replaced path cannot escape the root.
type FileKnowledge struct {
	root *os.Root
	path string
}

var _ agent.KnowledgePort = (*FileKnowledge)(nil)

// NewFileKnowledge validates the root and returns the knowledge port.
func NewFileKnowledge(root string) (*FileKnowledge, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("knowledge root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve knowledge root %q: %w", root, err)
	}
	absolute = filepath.Clean(absolute)
	opened, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open knowledge root %q: %w", absolute, err)
	}
	return &FileKnowledge{root: opened, path: absolute}, nil
}

// Close releases the pinned root handle.
func (knowledge *FileKnowledge) Close() error {
	if knowledge == nil || knowledge.root == nil {
		return nil
	}
	return knowledge.root.Close()
}

// Load reads one bounded knowledge body. Absolute paths, parent segments and
// paths that escape the root are refused before any read.
func (knowledge *FileKnowledge) Load(ctx context.Context, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if knowledge == nil || knowledge.root == nil {
		return nil, fault.New(fault.InternalFailure, "knowledge port is not configured")
	}
	if strings.TrimSpace(path) == "" {
		return nil, fault.New(fault.InvalidInput, "knowledge path is empty")
	}
	if filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') {
		return nil, fault.New(fault.InvalidInput, "knowledge path must be relative")
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fault.New(fault.InvalidInput, "knowledge path escapes the root")
	}
	info, err := knowledge.root.Lstat(cleaned)
	if err != nil {
		return nil, fault.Wrap(fault.NotFound, "knowledge body is unreadable", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput, "knowledge path is not a regular file")
	}
	if info.Size() > agent.MaxKnowledgeBytes {
		return nil, fault.New(fault.InvalidInput, "knowledge body exceeds the size bound")
	}
	file, err := knowledge.root.Open(cleaned)
	if err != nil {
		return nil, fault.Wrap(fault.NotFound, "knowledge body is unreadable", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, agent.MaxKnowledgeBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.NotFound, "knowledge body is unreadable", err)
	}
	if len(data) > agent.MaxKnowledgeBytes {
		return nil, fault.New(fault.InvalidInput, "knowledge body exceeds the size bound")
	}
	return data, nil
}

// SystemClock is the production time boundary.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }
