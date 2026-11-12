package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Checkpoint is one durable conversation checkpoint. It carries context and
// effect bookkeeping only: never credentials, approvals or decisions.
type Checkpoint struct {
	Version     int       `json:"version"`
	TaskID      string    `json:"task_id"`
	ExecutionID string    `json:"execution_id"`
	Messages    []Message `json:"messages"`
	Completed   []string  `json:"completed,omitempty"`
	InFlight    []string  `json:"in_flight,omitempty"`
	Uncertain   []string  `json:"uncertain,omitempty"`
	Usage       Usage     `json:"usage,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// checkpointWire is the explicit durable JSON shape. Usage is omitted when it is
// zero so a context-only checkpoint never carries usage token vocabulary.
type checkpointWire struct {
	Version     int       `json:"version"`
	TaskID      string    `json:"task_id"`
	ExecutionID string    `json:"execution_id"`
	Messages    []Message `json:"messages"`
	Completed   []string  `json:"completed,omitempty"`
	InFlight    []string  `json:"in_flight,omitempty"`
	Uncertain   []string  `json:"uncertain,omitempty"`
	Usage       *Usage    `json:"usage,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// MarshalJSON renders the durable checkpoint context shape.
func (checkpoint Checkpoint) MarshalJSON() ([]byte, error) {
	wire := checkpointWire{
		Version:     checkpoint.Version,
		TaskID:      checkpoint.TaskID,
		ExecutionID: checkpoint.ExecutionID,
		Messages:    checkpoint.Messages,
		Completed:   checkpoint.Completed,
		InFlight:    checkpoint.InFlight,
		Uncertain:   checkpoint.Uncertain,
		CreatedAt:   checkpoint.CreatedAt,
	}
	if checkpoint.Usage != (Usage{}) {
		usage := checkpoint.Usage
		wire.Usage = &usage
	}
	return json.Marshal(wire)
}

// Validate checks the version, identity, conversation, effect lists and time.
func (checkpoint Checkpoint) Validate() error {
	if checkpoint.Version != CheckpointVersion {
		return fmt.Errorf("checkpoint version %d is not %d", checkpoint.Version, CheckpointVersion)
	}
	if !validAgentID(checkpoint.TaskID) {
		return fmt.Errorf("checkpoint task id %q is invalid", checkpoint.TaskID)
	}
	if !validAgentID(checkpoint.ExecutionID) {
		return fmt.Errorf("checkpoint execution id %q is invalid", checkpoint.ExecutionID)
	}
	if len(checkpoint.Messages) == 0 {
		return fmt.Errorf("checkpoint has no messages")
	}
	for _, message := range checkpoint.Messages {
		if !validMessageRole(message.Role) {
			return fmt.Errorf("checkpoint message role %q is unknown", message.Role)
		}
	}
	if checkpoint.CreatedAt.IsZero() {
		return fmt.Errorf("checkpoint creation time is zero")
	}
	if err := checkpoint.Usage.Validate(); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, effects := range [][]string{checkpoint.Completed, checkpoint.InFlight, checkpoint.Uncertain} {
		for _, effect := range effects {
			if effect == "" {
				return fmt.Errorf("checkpoint effect identity is empty")
			}
			if seen[effect] {
				return fmt.Errorf("checkpoint effect identity %q appears more than once", effect)
			}
			seen[effect] = true
		}
	}
	return nil
}

// ValidCheckpointRef reports whether ref is a bounded versioned checkpoint
// reference. It never accepts a process-local id, path or URI.
func ValidCheckpointRef(ref string) bool {
	if ref == "" || len(ref) > MaxCheckpointRefBytes {
		return false
	}
	if !strings.HasPrefix(ref, CheckpointRefPrefix) {
		return false
	}
	rest := strings.TrimPrefix(ref, CheckpointRefPrefix)
	before, after, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	if before != CheckpointRefVersion {
		return false
	}
	payload := after
	if payload == "" || payload == "." || payload == ".." {
		return false
	}
	if strings.Contains(payload, "..") {
		return false
	}
	for index := 0; index < len(payload); index++ {
		character := payload[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.':
		default:
			return false
		}
	}
	return true
}

// effectIdentity derives a stable effect identity from the tool name and its
// exact arguments so a restored execution can recognize a completed effect.
// Provider call ids are correlation only and never participate in the identity.
func effectIdentity(call ToolRequest) string {
	sum := sha256.Sum256([]byte(call.Name + "\x00" + string(call.Arguments)))
	return "effect-" + hex.EncodeToString(sum[:16])
}

// ValidateConversation checks that a conversation is a closed, fully
// correlated provider transcript: every assistant call is answered by exactly
// one tool message with its id, no turn is left open, provider names reverse to
// the native grammar, and no restored turn replays the task.ask handoff. A tool
// message without a call identity and an assistant turn with neither content
// nor calls are refused: both are the residue of withholding correlation, which
// cannot be replayed to a provider.
func ValidateConversation(messages []Message) error {
	open := make(map[string]struct{})
	for index, message := range messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("conversation message %d is invalid: %w", index, err)
		}
		if len(open) > 0 && message.Role != "tool" {
			return fmt.Errorf("conversation message %d follows an unanswered tool call", index)
		}
		switch message.Role {
		case "user", "system":
			if strings.TrimSpace(message.Content) == "" {
				return fmt.Errorf("conversation message %d is an empty %s message", index, message.Role)
			}
		case "assistant":
			if len(message.ToolCalls) == 0 && strings.TrimSpace(message.Content) == "" {
				return fmt.Errorf("conversation message %d is an empty assistant turn", index)
			}
			for _, call := range message.ToolCalls {
				native, err := NativeToolName(call.Name)
				if err != nil {
					return fmt.Errorf("conversation message %d tool call %q: %w", index, call.ID, err)
				}
				if native == "task.ask" {
					return fmt.Errorf("conversation message %d replays the task.ask handoff", index)
				}
				open[call.ID] = struct{}{}
			}
		case "tool":
			if message.ToolCallID == "" {
				return fmt.Errorf("conversation message %d is a tool message without a call identity", index)
			}
			if _, exists := open[string(message.ToolCallID)]; !exists {
				return fmt.Errorf("conversation message %d has no open tool call %q", index, message.ToolCallID)
			}
			delete(open, string(message.ToolCallID))
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("conversation ends with %d unanswered tool call(s)", len(open))
	}
	return nil
}

// closedConversation returns the longest closed prefix of messages. When an
// assistant call turn still has unanswered calls, it is trimmed to the calls
// already answered and their tool messages; the unrun calls, and every message
// after them, are dropped. A fully answered conversation is returned unchanged
// as a deep copy. Persisting and restoring that prefix keeps the model's
// answered work without forwarding an open turn or replaying the handoff.
func closedConversation(messages []Message) []Message {
	for index := range messages {
		if messages[index].Role != "assistant" || len(messages[index].ToolCalls) == 0 {
			continue
		}
		answered := make(map[string]struct{}, len(messages[index].ToolCalls))
		end := index + 1
		for end < len(messages) && messages[end].Role == "tool" {
			if messages[end].ToolCallID != "" {
				answered[string(messages[end].ToolCallID)] = struct{}{}
			}
			end++
		}
		open := false
		for _, call := range messages[index].ToolCalls {
			if _, exists := answered[call.ID]; !exists {
				open = true
				break
			}
		}
		if !open {
			continue
		}
		closed := copyMessages(messages[:index])
		kept := make([]ToolCall, 0, len(messages[index].ToolCalls))
		keptIDs := make(map[string]struct{}, len(messages[index].ToolCalls))
		for _, call := range messages[index].ToolCalls {
			if _, exists := answered[call.ID]; exists {
				kept = append(kept, call)
				keptIDs[call.ID] = struct{}{}
			}
		}
		if len(kept) == 0 {
			return closed
		}
		turn := messages[index]
		turn.ToolCalls = kept
		closed = append(closed, turn)
		for _, message := range messages[index+1 : end] {
			if _, exists := keptIDs[string(message.ToolCallID)]; exists {
				closed = append(closed, message)
			}
		}
		return closed
	}
	return copyMessages(messages)
}

// copyMessages returns a deep copy of a conversation, including every nested
// assistant call slice, so a message never aliases the caller's or a port's
// memory across a model or checkpoint boundary.
func copyMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	copied := make([]Message, len(messages))
	for index := range messages {
		copied[index] = messages[index]
		if messages[index].ToolCalls != nil {
			copied[index].ToolCalls = append([]ToolCall(nil), messages[index].ToolCalls...)
		}
	}
	return copied
}

// copyToolDeclarations returns a deep copy of the advertised declarations,
// including each parameters byte slice, so a model port cannot change the
// schemas a later request advertises.
func copyToolDeclarations(declarations []ToolDeclaration) []ToolDeclaration {
	if declarations == nil {
		return nil
	}
	copied := make([]ToolDeclaration, len(declarations))
	for index := range declarations {
		copied[index] = declarations[index]
		if declarations[index].Parameters != nil {
			copied[index].Parameters = append([]byte(nil), declarations[index].Parameters...)
		}
	}
	return copied
}

// durableMessages copies the conversation for a durable checkpoint. Raw tool
// output, raw call arguments and the provider's correlation members are
// withheld unless retention is explicitly enabled. A withheld turn keeps a
// closed, provider-valid shape instead: every call receives a deterministic
// runtime-local id, its arguments are the empty object and its tool message
// carries the withholding placeholder with the same id. The synthetic id is
// derived from a counter, never from raw content, so it leaks nothing and a
// restored conversation never reaches the model bridge as an orphan tool turn.
func (runtime *Runtime) durableMessages(messages []Message) []Message {
	copied := copyMessages(messages)
	if runtime.config.RetainRaw {
		return copied
	}
	correlation := 0
	var durableIDs map[string]ToolCallID
	for index := range copied {
		switch copied[index].Role {
		case "assistant":
			durableIDs = nil
			if len(copied[index].ToolCalls) == 0 {
				copied[index].Content = "[assistant output not retained]"
				continue
			}
			copied[index].Content = ""
			durableIDs = make(map[string]ToolCallID, len(copied[index].ToolCalls))
			calls := make([]ToolCall, len(copied[index].ToolCalls))
			for callIndex, call := range copied[index].ToolCalls {
				correlation++
				id := ToolCallID(fmt.Sprintf("call-durable-%d", correlation))
				calls[callIndex] = ToolCall{ID: string(id), Name: call.Name, Arguments: "{}"}
				if call.ID != "" {
					durableIDs[call.ID] = id
				}
			}
			copied[index].ToolCalls = calls
		case "tool":
			copied[index].Content = "[tool output not retained]"
			oldID := string(copied[index].ToolCallID)
			if id, exists := durableIDs[oldID]; exists {
				copied[index].ToolCallID = id
				delete(durableIDs, oldID)
			} else {
				copied[index].ToolCallID = ""
			}
		default:
			durableIDs = nil
		}
	}
	return copied
}
