// Package agent declares the native-agent model, tool, checkpoint, knowledge
// and public-client ports and the runtime that drives them.
//
// The agent runs as an ordinary container command and gets no extra authority.
// These declarations call no provider and start no process; tests drive them
// with deterministic provider fixtures and fake ports.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Agent model bounds.
const (
	// MaxModelTokens bounds requested completion tokens.
	MaxModelTokens = 32768
	// MaxMessages bounds one model request message list.
	MaxMessages = 128
	// MaxToolDeclarations bounds the tools advertised by one request.
	MaxToolDeclarations = 32
	// MaxToolCalls bounds the tool calls of one assistant message.
	MaxToolCalls = 32
	// MaxToolNameBytes bounds an OpenAI-compatible function name.
	MaxToolNameBytes = 64
	// MaxToolSchemaBytes bounds one tool parameters schema.
	MaxToolSchemaBytes = 64 << 10
	// MaxToolSchemasBytes bounds the aggregate parameters schemas of a request.
	MaxToolSchemasBytes = 256 << 10
	// MaxToolArgumentsBytes bounds one tool call arguments string.
	MaxToolArgumentsBytes = 64 << 10
)

// Message is one model conversation message. Role and Content keep the legacy
// shape; ToolCalls carries an assistant message's provider tool calls with their
// identities, and ToolCallID is the call identity a tool message answers. An
// absent correlation member is the legacy uncorrelated shape, so a provider
// fixture or an uncorrelated tool message decodes and validates as before.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID ToolCallID `json:"tool_call_id,omitempty"`
}

// messageWire is the strict on-wire shape of Message. Decoding refuses unknown
// members at every nesting level while absent correlation members stay absent.
type messageWire struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID ToolCallID `json:"tool_call_id,omitempty"`
}

// UnmarshalJSON strictly decodes one conversation message. A member this
// declaration does not model is refused rather than silently dropped.
func (message *Message) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire messageWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing data after one message")
	}
	*message = Message{
		Role:       wire.Role,
		Content:    wire.Content,
		ToolCalls:  wire.ToolCalls,
		ToolCallID: wire.ToolCallID,
	}
	return nil
}

// Validate checks the message role and the role-specific correlation members.
// A present correlation member follows the api rule: only an assistant message
// carries tool calls, only a tool message carries a tool-call id, call ids are
// unique within the message, and the call count, identities and arguments obey
// the declared bounds. An absent member is the legacy shape.
func (message Message) Validate() error {
	if !validMessageRole(message.Role) {
		return fmt.Errorf("unknown message role %q", message.Role)
	}
	if message.Role == "assistant" {
		if message.ToolCallID != "" {
			return fmt.Errorf("an assistant message must not carry tool_call_id")
		}
		if len(message.ToolCalls) > MaxToolCalls {
			return fmt.Errorf("assistant tool calls are %d; at most %d are allowed", len(message.ToolCalls), MaxToolCalls)
		}
		ids := make(map[string]struct{}, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			if err := call.Validate(); err != nil {
				return err
			}
			if _, duplicate := ids[call.ID]; duplicate {
				return fmt.Errorf("assistant tool call id %q is duplicated", call.ID)
			}
			ids[call.ID] = struct{}{}
		}
		return nil
	}
	if message.ToolCalls != nil {
		return fmt.Errorf("only an assistant message may carry tool_calls")
	}
	if message.ToolCallID != "" {
		if message.Role != "tool" {
			return fmt.Errorf("only a tool message may carry tool_call_id")
		}
		if !message.ToolCallID.Valid() {
			return fmt.Errorf("tool message tool_call_id is invalid")
		}
	}
	return nil
}

// messageRoles is the message role vocabulary.
var messageRoles = []string{"system", "user", "assistant", "tool"}

// validMessageRole reports whether role is a supported message role.
func validMessageRole(role string) bool {
	for _, candidate := range messageRoles {
		if candidate == role {
			return true
		}
	}
	return false
}

// ToolDeclaration is one provider-neutral advertised tool. Parameters is a
// bounded JSON object schema that is stored and forwarded as written.
type ToolDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Validate checks the function name, a present bounded object parameters
// schema and the per-schema byte bound.
func (declaration ToolDeclaration) Validate() error {
	if !validToolFunctionName(declaration.Name) {
		return fmt.Errorf("tool name %q is not a 1..%d character [A-Za-z0-9_-] name", declaration.Name, MaxToolNameBytes)
	}
	if !boundedJSONObject(declaration.Parameters, MaxToolSchemaBytes) {
		return fmt.Errorf("tool %q parameters must be a JSON object of at most %d bytes", declaration.Name, MaxToolSchemaBytes)
	}
	return nil
}

// ToolCallID identifies one assistant tool call for correlation with a tool
// message.
type ToolCallID string

// ParseToolCallID validates and converts a call identity.
func ParseToolCallID(value string) (ToolCallID, error) {
	if !validCallID(value) {
		return "", fmt.Errorf("tool call id is invalid")
	}
	return ToolCallID(value), nil
}

// Valid reports whether the identity is a bounded ASCII call id.
func (id ToolCallID) Valid() bool {
	return validCallID(string(id))
}

// AssistantMessage is the explicit role-shaped mirror of one assistant
// tool-call turn. Message carries the same correlation; this mirror remains
// for the callers that use it. ContentNull is the explicit null marker
// for a message that carries calls only.
type AssistantMessage struct {
	Content     string     `json:"content"`
	ContentNull bool       `json:"content_null,omitempty"`
	Calls       []ToolCall `json:"tool_calls,omitempty"`
}

// Validate checks the call count and every call identity.
func (message AssistantMessage) Validate() error {
	if len(message.Calls) > MaxToolCalls {
		return fmt.Errorf("assistant tool calls are %d; at most %d are allowed", len(message.Calls), MaxToolCalls)
	}
	for _, call := range message.Calls {
		if err := call.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ToolMessage is the provider-neutral mirror of one tool result: the call
// identity plus the tool content.
type ToolMessage struct {
	CallID  ToolCallID `json:"tool_call_id"`
	Content string     `json:"content"`
}

// Validate checks the correlated call identity.
func (message ToolMessage) Validate() error {
	if !message.CallID.Valid() {
		return fmt.Errorf("tool message tool_call_id is invalid")
	}
	return nil
}

// ToolCall is one provider-neutral assistant tool call. Arguments is a JSON
// object encoded as a string, mirroring the public vocabulary.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Validate checks the call identity, the function name and the bounded JSON
// object string arguments.
func (call ToolCall) Validate() error {
	if !validCallID(call.ID) {
		return fmt.Errorf("tool call id is invalid")
	}
	if !validToolFunctionName(call.Name) {
		return fmt.Errorf("tool call function name %q is not a 1..%d character [A-Za-z0-9_-] name", call.Name, MaxToolNameBytes)
	}
	if !boundedJSONObject([]byte(call.Arguments), MaxToolArgumentsBytes) {
		return fmt.Errorf("tool call %q arguments must be a JSON object string of at most %d bytes", call.ID, MaxToolArgumentsBytes)
	}
	return nil
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

// Provider function names accept only [A-Za-z0-9_-], while native Kodama tool
// names are dotted operation names such as "task.ask". The translation
// below is reversible and collision-free; a naive dot-to-underscore alias is
// not, because "task.ask" and "task_ask" would merge.
const (
	// ProviderToolNamePrefix marks a translated native tool name.
	ProviderToolNamePrefix = "kd_"
	// providerToolDotEscape writes one native dot.
	providerToolDotEscape = "_0"
	// providerToolUnderscoreEscape writes one native underscore.
	providerToolUnderscoreEscape = "_1"
)

// ProviderToolName translates one native dotted tool name into the provider
// function-name grammar. It prefixes ProviderToolNamePrefix, writes every dot
// as "_0" and every underscore as "_1", and copies every other native
// character. The escape set is prefix-free, so the mapping is injective over
// every valid native name; the encoded name must fit MaxToolNameBytes. The
// later public model bridge applies this translation; the provider boundary
// itself accepts only the encoded form.
func ProviderToolName(native string) (string, error) {
	if !validToolName(native) {
		return "", fmt.Errorf("tool name %q is not a dotted lowercase tool name", native)
	}
	var builder strings.Builder
	builder.Grow(len(ProviderToolNamePrefix) + len(native))
	builder.WriteString(ProviderToolNamePrefix)
	for index := 0; index < len(native); index++ {
		switch native[index] {
		case '.':
			builder.WriteString(providerToolDotEscape)
		case '_':
			builder.WriteString(providerToolUnderscoreEscape)
		default:
			builder.WriteByte(native[index])
		}
	}
	encoded := builder.String()
	if len(encoded) > MaxToolNameBytes {
		return "", fmt.Errorf("provider tool name for %q is %d bytes; at most %d are allowed", native, len(encoded), MaxToolNameBytes)
	}
	return encoded, nil
}

// NativeToolName reverses ProviderToolName. It refuses a name without the
// declared prefix, a malformed escape, a non-canonical form and a decoded name
// that is not a valid native dotted tool name.
func NativeToolName(provider string) (string, error) {
	if !strings.HasPrefix(provider, ProviderToolNamePrefix) {
		return "", fmt.Errorf("provider tool name %q does not start with %q", provider, ProviderToolNamePrefix)
	}
	rest := strings.TrimPrefix(provider, ProviderToolNamePrefix)
	var builder strings.Builder
	builder.Grow(len(rest))
	for index := 0; index < len(rest); index++ {
		character := rest[index]
		if character != '_' {
			builder.WriteByte(character)
			continue
		}
		if index+1 >= len(rest) {
			return "", fmt.Errorf("provider tool name %q ends with an escape prefix", provider)
		}
		switch rest[index+1] {
		case '0':
			builder.WriteByte('.')
		case '1':
			builder.WriteByte('_')
		default:
			return "", fmt.Errorf("provider tool name %q has an unknown escape", provider)
		}
		index++
	}
	native := builder.String()
	if !validToolName(native) {
		return "", fmt.Errorf("provider tool name %q does not decode to a dotted lowercase tool name", provider)
	}
	encoded, err := ProviderToolName(native)
	if err != nil || encoded != provider {
		return "", fmt.Errorf("provider tool name %q is not canonical", provider)
	}
	return native, nil
}

// validCallID reports whether value is a bounded ASCII call identity.
func validCallID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
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

// boundedJSONObject reports whether value is a bounded JSON object.
func boundedJSONObject(value []byte, limit int) bool {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" || len(trimmed) > limit {
		return false
	}
	if !json.Valid([]byte(trimmed)) {
		return false
	}
	return strings.HasPrefix(trimmed, "{")
}

// ModelRequest is one provider-neutral model request.
type ModelRequest struct {
	Model       string            `json:"model"`
	Messages    []Message         `json:"messages"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	Stream      bool              `json:"stream,omitempty"`
	Tools       []ToolDeclaration `json:"tools,omitempty"`
}

// Validate checks the model, message list (including role-specific tool
// fields), tool declarations and token/temperature bounds.
func (request ModelRequest) Validate() error {
	if request.Model == "" {
		return fmt.Errorf("model request has no model")
	}
	if len(request.Messages) == 0 {
		return fmt.Errorf("model request has no messages")
	}
	if len(request.Messages) > MaxMessages {
		return fmt.Errorf("model request has %d messages; at most %d are allowed", len(request.Messages), MaxMessages)
	}
	for _, message := range request.Messages {
		if err := message.Validate(); err != nil {
			return err
		}
	}
	if len(request.Tools) > MaxToolDeclarations {
		return fmt.Errorf("model request has %d tools; at most %d are allowed", len(request.Tools), MaxToolDeclarations)
	}
	aggregate := 0
	for _, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return err
		}
		aggregate += len(strings.TrimSpace(string(tool.Parameters)))
	}
	if aggregate > MaxToolSchemasBytes {
		return fmt.Errorf("model request tool schemas total %d bytes; at most %d are allowed", aggregate, MaxToolSchemasBytes)
	}
	if request.MaxTokens < 0 || request.MaxTokens > MaxModelTokens {
		return fmt.Errorf("max tokens %d is outside 0..%d", request.MaxTokens, MaxModelTokens)
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return fmt.Errorf("temperature must be within 0..2")
	}
	return nil
}

// ToolRequest is one model-requested tool invocation. ID is the optional
// provider call identity; the legacy deterministic fixtures omit it.
type ToolRequest struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Validate checks the optional call identity, the tool name and that present
// arguments are a JSON object.
func (request ToolRequest) Validate() error {
	if request.ID != "" && !validCallID(request.ID) {
		return fmt.Errorf("tool call id is invalid")
	}
	if !validToolName(request.Name) {
		return fmt.Errorf("invalid tool name %q", request.Name)
	}
	if len(request.Arguments) > 0 {
		trimmed := strings.TrimSpace(string(request.Arguments))
		if trimmed != "" && trimmed != "null" {
			if !json.Valid([]byte(trimmed)) || !strings.HasPrefix(trimmed, "{") {
				return fmt.Errorf("tool %q arguments must be a JSON object", request.Name)
			}
		}
	}
	return nil
}

// validToolName reports whether value is a dotted lowercase tool name.
func validToolName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for index := 0; index < len(segment); index++ {
			character := segment[index]
			switch {
			case character >= 'a' && character <= 'z':
			case character >= '0' && character <= '9':
			case character == '-', character == '_':
			default:
				return false
			}
		}
	}
	return true
}

// ToolResult is one tool outcome returned to the model.
type ToolResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Usage is one model usage record.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Validate checks non-negative counts that sum to the total.
func (usage Usage) Validate() error {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return fmt.Errorf("usage counts must not be negative")
	}
	if usage.PromptTokens+usage.CompletionTokens != usage.TotalTokens {
		return fmt.Errorf("usage prompt+completion tokens do not equal total tokens")
	}
	return nil
}

// ModelResponse is one provider-neutral model response.
type ModelResponse struct {
	Content      string        `json:"content,omitempty"`
	ToolCalls    []ToolRequest `json:"tool_calls,omitempty"`
	Usage        Usage         `json:"usage"`
	FinishReason string        `json:"finish_reason"`
}

// finishReasons is the finish-reason vocabulary.
var finishReasons = []string{"stop", "tool_calls", "length", "content_filter"}

// validFinishReason reports whether reason is a supported finish reason.
func validFinishReason(reason string) bool {
	for _, candidate := range finishReasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

// Validate checks the finish reason, usage and requested tool calls. A present
// response call id is unique within the response; a call without an id stays
// valid, and the runtime assigns it a deterministic local correlation id.
func (response ModelResponse) Validate() error {
	if !validFinishReason(response.FinishReason) {
		return fmt.Errorf("unknown finish reason %q", response.FinishReason)
	}
	if len(response.ToolCalls) > MaxToolCalls {
		return fmt.Errorf("response tool calls are %d; at most %d are allowed", len(response.ToolCalls), MaxToolCalls)
	}
	ids := make(map[string]struct{}, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		if err := call.Validate(); err != nil {
			return err
		}
		if call.ID == "" {
			continue
		}
		if _, duplicate := ids[call.ID]; duplicate {
			return fmt.Errorf("response tool call id %q is duplicated", call.ID)
		}
		ids[call.ID] = struct{}{}
	}
	return response.Usage.Validate()
}

// ModelPort is the provider-neutral model boundary.
type ModelPort interface {
	Invoke(ctx context.Context, request ModelRequest) (ModelResponse, error)
}

// ToolPort is the tool boundary the agent calls.
type ToolPort interface {
	Invoke(ctx context.Context, request ToolRequest) (ToolResult, error)
}

// Clock is the injected time boundary.
type Clock interface {
	Now() time.Time
}
