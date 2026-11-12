package session

import "fmt"

// ContextLimits bounds one restored conversation window.
type ContextLimits struct {
	MaxMessages int
	MaxBytes    int
}

// DefaultContextLimits returns the bounded default history window.
func DefaultContextLimits() ContextLimits {
	return ContextLimits{MaxMessages: 128, MaxBytes: 256 << 10}
}

// ContextWindow is one bounded, reference-preserving history window.
type ContextWindow struct {
	Messages []Message
	Refs     []Ref
	Trimmed  int
}

// Window returns the newest messages that fit both limits in original order.
// The oldest messages are dropped first and their count is reported in Trimmed.
// Context references are copied verbatim: they are never trimmed and their
// content is never inlined.
func (record Record) Window(limits ContextLimits) (ContextWindow, error) {
	if limits.MaxMessages <= 0 {
		return ContextWindow{}, fmt.Errorf("context max messages %d must be positive", limits.MaxMessages)
	}
	if limits.MaxBytes <= 0 {
		return ContextWindow{}, fmt.Errorf("context max bytes %d must be positive", limits.MaxBytes)
	}

	window := ContextWindow{Refs: append([]Ref(nil), record.ContextRefs...)}
	if len(record.Messages) == 0 {
		return window, nil
	}

	newest := messageCost(record.Messages[len(record.Messages)-1])
	if newest > limits.MaxBytes {
		return ContextWindow{}, fmt.Errorf("newest context message is %d bytes; at most %d are allowed", newest, limits.MaxBytes)
	}

	total := 0
	start := len(record.Messages)
	for start > 0 {
		if len(record.Messages)-start >= limits.MaxMessages {
			break
		}
		cost := messageCost(record.Messages[start-1])
		if total+cost > limits.MaxBytes {
			break
		}
		total += cost
		start--
	}

	window.Messages = copyMessages(record.Messages[start:])
	window.Trimmed = start
	return window, nil
}

// messageCost is the exact byte cost of one message: its role and content plus
// the tool-call id it answers and every assistant call's id, name and arguments
// string. Correlation is charged so a window cannot keep more bytes than it
// reports.
func messageCost(message Message) int {
	cost := len(message.Role) + len(message.Content) + len(message.ToolCallID)
	for _, call := range message.ToolCalls {
		cost += len(call.ID) + len(call.Name) + len(call.Arguments)
	}
	return cost
}
