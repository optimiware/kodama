package gateway

import "encoding/json"

// Role is a chat message role of the supported subset.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a function call an assistant message carries.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Message is one bounded text message. Content is nil only for an assistant
// message that carries tool calls.
type Message struct {
	Role       Role
	Content    *string
	Name       string
	ToolCallID string
	ToolCalls  []ToolCall
}

// Tool declares one function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object, kept as validated, bounded JSON.
	Parameters json.RawMessage
}

// ToolChoice selects how the model may use the declared tools. Mode is "" (not
// set), "none", "auto", "required" or "function" (with Function naming a
// declared tool).
type ToolChoice struct {
	Mode     string
	Function string
}

// Completion is the validated, transport-neutral form of one chat completion
// request: exactly the supported subset and nothing else. The provider
// adapter builds its outbound request from these fields only, so no caller
// supplied member, header or URL is ever forwarded.
type Completion struct {
	Model             string
	Messages          []Message
	Tools             []Tool
	ToolChoice        ToolChoice
	ParallelToolCalls *bool
	// MaxTokens is the caller's output limit; 0 means "use the run's limit".
	MaxTokens   int64
	Temperature *float64
	TopP        *float64
	Stop        []string
}
