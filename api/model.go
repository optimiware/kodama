package api

import (
	"bytes"
	"encoding/json"
	"io"
)

// ModelSubsetVersion pins the OpenAI-compatible completion subset. The subset
// is non-streaming: it has no chunk schema, stream end marker or stream
// byte/chunk limits, and a request with stream:true refuses. The subset
// includes standard OpenAI tool calling.
const ModelSubsetVersion = "openai-chat-completions.v1"

// Tool-calling bounds. Every bound is a byte or item count; a schema and an
// argument payload are never counted in runes.
const (
	// MaxToolDeclarations bounds the tools advertised by one request.
	MaxToolDeclarations = 32
	// MaxToolCallsPerMessage bounds the tool calls of one assistant message.
	MaxToolCallsPerMessage = 32
	// MaxToolParametersBytes bounds one tool's parameters schema.
	MaxToolParametersBytes = 64 << 10
	// MaxToolSchemasBytes bounds the aggregate parameters schemas of a request.
	MaxToolSchemasBytes = 256 << 10
	// MaxToolArgumentsBytes bounds one tool call's arguments string.
	MaxToolArgumentsBytes = 64 << 10
	// MaxToolNameBytes bounds an OpenAI-compatible function name.
	MaxToolNameBytes = 64
)

// ChatMessage is one OpenAI-compatible chat message.
//
// Content is nullable for an assistant message. The Go representation keeps
// Content a string so existing string-literal consumers compile; ContentNull
// is the explicit non-wire null marker. The custom JSON handling below writes
// JSON null only when ContentNull is set, so an empty string is never inferred
// to be null. ToolCalls is legal only on an assistant message and ToolCallID
// only on a tool message.
type ChatMessage struct {
	Role        string         `json:"role"`
	Content     string         `json:"content" schema:"nullable"`
	ContentNull bool           `json:"-"`
	ToolCalls   []ChatToolCall `json:"tool_calls,omitempty" schema:"tool_calls"`
	ToolCallID  string         `json:"tool_call_id,omitempty" schema:"id"`
}

// chatMessageWire is the strict on-wire shape of ChatMessage. Content is raw so
// an explicit null is distinguishable from an empty string.
type chatMessageWire struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// UnmarshalJSON decodes one message leniently. A provider response message may
// carry standard members this subset does not model (for example `refusal` or
// `annotations`), and refusing them would turn a billed provider call into a
// dependency failure. Request messages stay strict through Messages, which
// decodes each element with ChatMessage.decode(strict=true); the request codecs
// never take this lenient path.
func (message *ChatMessage) UnmarshalJSON(data []byte) error {
	return message.decode(data, false)
}

// decode decodes one message. strict refuses unknown members at every nesting
// level; lenient ignores them. An explicit null content sets the null marker
// while an empty string does not.
func (message *ChatMessage) decode(data []byte, strict bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	var wire chatMessageWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid("trailing data after one chat message")
	}
	message.Role = wire.Role
	message.ToolCalls = wire.ToolCalls
	message.ToolCallID = wire.ToolCallID
	content := bytes.TrimSpace(wire.Content)
	if len(content) == 0 || bytes.Equal(content, []byte("null")) {
		message.Content = ""
		message.ContentNull = true
		return nil
	}
	var value string
	if err := json.Unmarshal(content, &value); err != nil {
		return err
	}
	message.Content = value
	message.ContentNull = false
	return nil
}

// Messages is the request-side chat message list. It decodes strictly even
// though ChatMessage itself decodes leniently, so a request message with an
// unknown member refuses and a provider response message with an unknown
// member still decodes.
type Messages []ChatMessage

// UnmarshalJSON decodes one message list strictly.
func (messages *Messages) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(data)))
	var raws []json.RawMessage
	if err := decoder.Decode(&raws); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return invalid("trailing data after the message list")
	}
	decoded := make(Messages, 0, len(raws))
	for _, raw := range raws {
		var message ChatMessage
		if err := message.decode(raw, true); err != nil {
			return err
		}
		decoded = append(decoded, message)
	}
	*messages = decoded
	return nil
}

// chatMessageOut is the on-wire shape written by MarshalJSON.
type chatMessageOut struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// MarshalJSON writes an explicit null content only when ContentNull is set.
func (message ChatMessage) MarshalJSON() ([]byte, error) {
	content := any(message.Content)
	if message.ContentNull {
		content = nil
	}
	return json.Marshal(chatMessageOut{
		Role:       message.Role,
		Content:    content,
		ToolCalls:  message.ToolCalls,
		ToolCallID: message.ToolCallID,
	})
}

// ChatTool is one advertised tool. Only the function type is supported.
type ChatTool struct {
	Type     string           `json:"type"`
	Function ChatToolFunction `json:"function"`
}

// ChatToolFunction is one function declaration. Parameters is a bounded JSON
// object schema; it is advertised and stored as written, never fetched or
// evaluated.
type ChatToolFunction struct {
	Name        string          `json:"name" schema:"tool_name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters" schema:"tool_parameters"`
}

// ChatToolCall is one assistant-requested tool call. Function.Arguments is a
// JSON STRING carrying a JSON object, never an object on the wire.
type ChatToolCall struct {
	ID       string               `json:"id" schema:"id"`
	Type     string               `json:"type"`
	Function ChatToolCallFunction `json:"function"`
}

// ChatToolCallFunction is the function identity of one tool call.
type ChatToolCallFunction struct {
	Name      string `json:"name" schema:"tool_name"`
	Arguments string `json:"arguments" schema:"tool_arguments"`
}

// chatToolType is the only supported tool type.
const chatToolType = "function"

// validChatToolType reports whether the tool type is the supported function
// type.
func validChatToolType(value string) bool {
	return value == chatToolType
}

// validToolFunctionName reports whether name is the OpenAI-compatible
// 1–64-character [A-Za-z0-9_-] function-name grammar.
func validToolFunctionName(name string) bool {
	if name == "" || len(name) > MaxToolNameBytes {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// boundedJSONObject reports whether value is a bounded JSON object. It is used
// for tool parameters and for the JSON string payload of call arguments.
func boundedJSONObject(value []byte, limit int) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || len(trimmed) > limit {
		return false
	}
	if !json.Valid(trimmed) {
		return false
	}
	return trimmed[0] == '{'
}

// Validate checks one advertised tool: the function type, the name grammar, a
// present bounded object parameters schema and the per-schema bound.
func (tool ChatTool) Validate() error {
	if !validChatToolType(tool.Type) {
		return invalid("tool type %q is not function", tool.Type)
	}
	if !validToolFunctionName(tool.Function.Name) {
		return invalid("tool function name %q is not a 1..%d character [A-Za-z0-9_-] name", tool.Function.Name, MaxToolNameBytes)
	}
	if len(bytes.TrimSpace(tool.Function.Parameters)) == 0 {
		return invalid("tool %q has no parameters schema", tool.Function.Name)
	}
	if !boundedJSONObject(tool.Function.Parameters, MaxToolParametersBytes) {
		return invalid("tool %q parameters must be a JSON object of at most %d bytes", tool.Function.Name, MaxToolParametersBytes)
	}
	return nil
}

// validateTools checks the declaration count, every declaration and the
// aggregate parameters-schema bytes of a request.
func validateTools(tools []ChatTool) error {
	if len(tools) > MaxToolDeclarations {
		return invalid("tools are %d; at most %d are allowed", len(tools), MaxToolDeclarations)
	}
	aggregate := 0
	declared := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if err := tool.Validate(); err != nil {
			return err
		}
		if _, duplicate := declared[tool.Function.Name]; duplicate {
			return invalid("tool %q is declared more than once", tool.Function.Name)
		}
		declared[tool.Function.Name] = struct{}{}
		aggregate += len(bytes.TrimSpace(tool.Function.Parameters))
	}
	if aggregate > MaxToolSchemasBytes {
		return invalid("tool parameters schemas total %d bytes; at most %d are allowed", aggregate, MaxToolSchemasBytes)
	}
	return nil
}

// Validate checks one tool call identity and its bounded JSON-object
// arguments string.
func (call ChatToolCall) Validate() error {
	if !validID(call.ID) {
		return invalid("tool call id is invalid")
	}
	if !validChatToolType(call.Type) {
		return invalid("tool call type %q is not function", call.Type)
	}
	if !validToolFunctionName(call.Function.Name) {
		return invalid("tool call function name %q is not a 1..%d character [A-Za-z0-9_-] name", call.Function.Name, MaxToolNameBytes)
	}
	if !boundedJSONObject([]byte(call.Function.Arguments), MaxToolArgumentsBytes) {
		return invalid("tool call %q arguments must be a JSON object string of at most %d bytes", call.ID, MaxToolArgumentsBytes)
	}
	return nil
}

// Validate checks the role, the role-specific tool fields and the call bounds.
func (message ChatMessage) Validate() error {
	if !validChatRole(message.Role) {
		return invalid("unknown chat role %q", message.Role)
	}
	if message.ContentNull && message.Role != "assistant" {
		return invalid("only an assistant message may have null content")
	}
	switch message.Role {
	case "assistant":
		if message.ToolCallID != "" {
			return invalid("an assistant message must not carry tool_call_id")
		}
		if len(message.ToolCalls) > MaxToolCallsPerMessage {
			return invalid("assistant tool calls are %d; at most %d are allowed", len(message.ToolCalls), MaxToolCallsPerMessage)
		}
		callIDs := make(map[string]struct{}, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			if err := call.Validate(); err != nil {
				return err
			}
			if _, duplicate := callIDs[call.ID]; duplicate {
				return invalid("assistant tool call id %q is duplicated", call.ID)
			}
			callIDs[call.ID] = struct{}{}
		}
	case "tool":
		if message.ToolCalls != nil {
			return invalid("a tool message must not carry tool_calls")
		}
		if !validID(message.ToolCallID) {
			return invalid("tool message tool_call_id is invalid")
		}
	default:
		if message.ToolCalls != nil {
			return invalid("only an assistant message may carry tool_calls")
		}
		if message.ToolCallID != "" {
			return invalid("only a tool message may carry tool_call_id")
		}
	}
	return nil
}

// ChatCompletionRequest is the OpenAI-compatible completion subset.
type ChatCompletionRequest struct {
	Model       string     `json:"model"`
	Messages    Messages   `json:"messages" schema:"messages"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stream      bool       `json:"stream,omitempty"`
	Tools       []ChatTool `json:"tools,omitempty" schema:"tools"`
}

// ModelInvokeRequest is the model.invoke input: the completion subset
// plus the project route identity. The project is carried explicitly for MCP
// callers and is optional for REST callers whose route supplies it.
type ModelInvokeRequest struct {
	ProjectID   string     `json:"project_id,omitempty" schema:"id"`
	Model       string     `json:"model"`
	Messages    Messages   `json:"messages" schema:"messages"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	Stream      bool       `json:"stream,omitempty"`
	Tools       []ChatTool `json:"tools,omitempty" schema:"tools"`
}

// Validate checks the optional project identity and the completion subset.
func (request ModelInvokeRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	return (ChatCompletionRequest{
		Model:       request.Model,
		Messages:    request.Messages,
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
		Stream:      request.Stream,
		Tools:       request.Tools,
	}).Validate()
}

// chatRoles is the message role subset.
var chatRoles = []string{"system", "user", "assistant", "tool"}

// validChatRole reports whether role is a supported chat role.
func validChatRole(role string) bool {
	for _, candidate := range chatRoles {
		if candidate == role {
			return true
		}
	}
	return false
}

// Validate checks the model, message count, roles and role-specific tool
// fields, the tool declarations, token/temperature bounds and the
// non-streaming subset.
func (request ChatCompletionRequest) Validate() error {
	if request.Model == "" {
		return invalid("model is empty")
	}
	if request.Stream {
		return invalid("stream is not supported; %s is non-streaming only", ModelSubsetVersion)
	}
	if len(request.Messages) == 0 {
		return invalid("messages are empty")
	}
	if len(request.Messages) > MaxModelMessages {
		return invalid("messages are %d; at most %d are allowed", len(request.Messages), MaxModelMessages)
	}
	for _, message := range request.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
	}
	if err := validateTools(request.Tools); err != nil {
		return err
	}
	if request.MaxTokens < 0 || request.MaxTokens > MaxCompletionTokens {
		return invalid("max_tokens %d is outside 0..%d", request.MaxTokens, MaxCompletionTokens)
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return invalid("temperature must be within 0..2")
	}
	return nil
}

// DecodeChatCompletionRequest decodes one bounded OpenAI-compatible request.
func DecodeChatCompletionRequest(data []byte) (ChatCompletionRequest, error) {
	if len(data) > MaxModelBodyBytes {
		return ChatCompletionRequest{}, invalid("model body is %d bytes; at most %d are allowed", len(data), MaxModelBodyBytes)
	}
	var request ChatCompletionRequest
	if err := decodeStrict(data, &request); err != nil {
		return ChatCompletionRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return ChatCompletionRequest{}, err
	}
	return request, nil
}

// ChatUsage is one completion usage record.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Validate checks non-negative counts that sum to the total.
func (usage ChatUsage) Validate() error {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return invalid("usage counts must not be negative")
	}
	if usage.PromptTokens+usage.CompletionTokens != usage.TotalTokens {
		return invalid("usage prompt+completion tokens do not equal total tokens")
	}
	return nil
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionResponse is the OpenAI-compatible completion response.
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// Validate checks the response identity, choices (including their nullable
// content and tool calls) and usage.
func (response ChatCompletionResponse) Validate() error {
	if response.ID == "" {
		return invalid("completion id is empty")
	}
	if response.Model == "" {
		return invalid("completion model is empty")
	}
	if len(response.Choices) == 0 {
		return invalid("completion choices are empty")
	}
	for _, choice := range response.Choices {
		if err := choice.Message.Validate(); err != nil {
			return err
		}
	}
	return response.Usage.Validate()
}
