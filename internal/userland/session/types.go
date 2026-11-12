// Package session declares versioned durable checkpoint references and the
// restoration error vocabulary.
//
// A checkpoint is an external reference owned by Userland-controlled storage,
// never a process-local session ID or a worktree path. The declarations open no
// storage and start no agent process.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Checkpoint reference format constants.
const (
	// RefPrefix is the fixed checkpoint reference prefix.
	RefPrefix = "kodama-checkpoint/"
	// RefVersion is the current checkpoint format version.
	RefVersion = "v1"
	// MaxRefBytes bounds one checkpoint reference.
	MaxRefBytes = 512
)

// Ref is one durable checkpoint reference.
type Ref string

// ParseRef validates and converts a checkpoint reference.
func ParseRef(value string) (Ref, error) {
	if len(value) > MaxRefBytes {
		return "", fmt.Errorf("checkpoint reference is %d bytes; at most %d are allowed", len(value), MaxRefBytes)
	}
	if !strings.HasPrefix(value, RefPrefix) {
		return "", fmt.Errorf("checkpoint reference %q does not start with %q", value, RefPrefix)
	}
	rest := strings.TrimPrefix(value, RefPrefix)
	separator := strings.IndexByte(rest, '/')
	if separator < 0 {
		return "", fmt.Errorf("checkpoint reference %q has no version", value)
	}
	version := rest[:separator]
	payload := rest[separator+1:]
	if version != RefVersion {
		return "", fmt.Errorf("checkpoint version %q is not %q", version, RefVersion)
	}
	if !validPayload(payload) {
		return "", fmt.Errorf("checkpoint payload %q is invalid", payload)
	}
	return Ref(value), nil
}

// validPayload reports whether payload is a bounded lowercase checkpoint name.
func validPayload(payload string) bool {
	if payload == "" || len(payload) > MaxRefBytes {
		return false
	}
	if payload == "." || payload == ".." {
		return false
	}
	for index := 0; index < len(payload); index++ {
		character := payload[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// Version returns the checkpoint format version of the reference.
func (ref Ref) Version() string {
	value := string(ref)
	if !strings.HasPrefix(value, RefPrefix) {
		return ""
	}
	rest := strings.TrimPrefix(value, RefPrefix)
	separator := strings.IndexByte(rest, '/')
	if separator < 0 {
		return ""
	}
	return rest[:separator]
}

// Reason is one checkpoint restoration failure reason.
type Reason string

const (
	// ReasonMissing reports an absent checkpoint.
	ReasonMissing Reason = "missing"
	// ReasonVersionMismatch reports an unsupported checkpoint version.
	ReasonVersionMismatch Reason = "version_mismatch"
	// ReasonCorrupt reports unreadable checkpoint content.
	ReasonCorrupt Reason = "corrupt"
	// ReasonContextMissing reports missing durable context for a restore.
	ReasonContextMissing Reason = "context_missing"
)

// reasons is the declaration order.
var reasons = []Reason{ReasonMissing, ReasonVersionMismatch, ReasonCorrupt, ReasonContextMissing}

// Valid reports whether the reason is one of the declared reasons.
func (reason Reason) Valid() bool {
	for _, candidate := range reasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

// RestoreError is a typed checkpoint restoration failure.
type RestoreError struct {
	Reason Reason `json:"reason"`
	Ref    Ref    `json:"ref"`
}

// Error renders the restoration failure.
func (err RestoreError) Error() string {
	return fmt.Sprintf("checkpoint restore failed (%s): %s", err.Reason, err.Ref)
}

// Recorded tool-call bounds. They mirror the public model subset and the
// provider-neutral agent declarations.
const (
	// MaxToolCalls bounds the tool calls of one recorded assistant message.
	MaxToolCalls = 32
	// MaxToolNameBytes bounds one recorded provider function name.
	MaxToolNameBytes = 64
	// MaxToolArgumentsBytes bounds one recorded call arguments string.
	MaxToolArgumentsBytes = 64 << 10
)

// ToolCallID identifies one recorded assistant tool call for correlation with a
// tool message.
type ToolCallID string

// Valid reports whether the identity is a bounded ASCII call id.
func (id ToolCallID) Valid() bool {
	return validCallID(string(id))
}

// ToolCall is one recorded assistant tool call. Arguments is the provider's
// JSON-object arguments string, never an object.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Validate checks the call identity, the provider function name and the
// bounded JSON-object arguments string.
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

// Message is one recorded conversation message. Role and Content keep the
// legacy shape; ToolCalls carries an assistant message's provider tool calls
// with their identities, and ToolCallID is the call identity a tool message
// answers. An absent correlation member is the legacy uncorrelated shape.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID ToolCallID `json:"tool_call_id,omitempty"`
}

// messageWire is the strict on-wire shape of Message.
type messageWire struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID ToolCallID `json:"tool_call_id,omitempty"`
}

// UnmarshalJSON strictly decodes one recorded message. An unknown member at any
// nesting level is refused; an absent correlation member decodes as before.
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

// Validate checks the role and the role-specific correlation members. An
// absent member keeps the legacy shape; a present member obeys the api rule:
// only an assistant message carries tool calls, only a tool message carries a
// tool-call id, call ids are unique, and the count, identities and arguments
// obey the declared bounds.
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

// Record is one versioned checkpoint record.
//
// SessionID and Revision name the durable session stream and the numeric
// revision a save expects to follow; Revision is never the format version. An
// empty SessionID stays valid, while Save is the boundary that requires one.
type Record struct {
	Version     string          `json:"version"`
	TaskID      string          `json:"task_id"`
	ExecutionID string          `json:"execution_id"`
	SessionID   SessionID       `json:"session_id,omitempty"`
	Revision    uint64          `json:"revision,omitempty"`
	Messages    []Message       `json:"messages"`
	ContextRefs []Ref           `json:"context_refs,omitempty"`
	Retention   RetentionPolicy `json:"retention,omitempty"`
	Usage       []Usage         `json:"usage,omitempty"`
	Outcome     Outcome         `json:"outcome,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// messageRoles is the recorded-message role vocabulary.
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

// Validate checks the version, identity, messages and creation time.
func (record Record) Validate() error {
	if record.Version != RefVersion {
		return fmt.Errorf("checkpoint record version %q is not %q", record.Version, RefVersion)
	}
	if !validRecordID(record.TaskID) {
		return fmt.Errorf("checkpoint task id %q is invalid", record.TaskID)
	}
	if !validRecordID(record.ExecutionID) {
		return fmt.Errorf("checkpoint execution id %q is invalid", record.ExecutionID)
	}
	if len(record.Messages) == 0 {
		return fmt.Errorf("checkpoint record has no messages")
	}
	for _, message := range record.Messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("checkpoint message: %w", err)
		}
	}
	if record.CreatedAt.IsZero() {
		return fmt.Errorf("checkpoint creation time is zero")
	}
	if record.SessionID != "" {
		if _, err := ParseSessionID(string(record.SessionID)); err != nil {
			return fmt.Errorf("checkpoint session id %q is invalid", record.SessionID)
		}
	}
	for _, contextRef := range record.ContextRefs {
		if _, err := ParseRef(string(contextRef)); err != nil {
			return fmt.Errorf("checkpoint context reference %q is invalid", contextRef)
		}
	}
	if err := record.Retention.Validate(); err != nil {
		return fmt.Errorf("checkpoint retention: %w", err)
	}
	for _, usage := range record.Usage {
		if usage.PrincipalID == "" {
			return fmt.Errorf("checkpoint usage has no principal id")
		}
		if usage.Model == "" {
			return fmt.Errorf("checkpoint usage has no model")
		}
		if usage.Prompt < 0 || usage.Completion < 0 {
			return fmt.Errorf("checkpoint usage has a negative count")
		}
	}
	if len(record.Outcome.Status) > 64 {
		return fmt.Errorf("checkpoint outcome status is %d bytes; at most 64 are allowed", len(record.Outcome.Status))
	}
	if len(record.Outcome.Summary) > 4096 {
		return fmt.Errorf("checkpoint outcome summary is %d bytes; at most 4096 are allowed", len(record.Outcome.Summary))
	}
	return nil
}

// validRecordID reports whether value is a bounded ASCII identity.
func validRecordID(value string) bool {
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

// Store persists and restores checkpoints. A missing context is a typed
// restoration error, never an empty success.
type Store interface {
	Save(ctx context.Context, record Record) (Ref, error)
	Restore(ctx context.Context, ref Ref) (Record, error)
}
