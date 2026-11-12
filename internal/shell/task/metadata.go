package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/optimiware/kodama/internal/fault"
)

// Task metadata and conversation bounds. Metadata is one opaque JSON object of
// string values and is never interpreted; the conversation is the admitted
// message history snapshot.
const (
	maxMetadataBytes     = 16 << 10
	maxConversationBytes = 64 << 10
)

// emptyConversationJSON is the present conversation snapshot of a task-bound
// admission with zero messages. It is the valid JSON empty array, the same
// encoding a non-empty conversation uses (a JSON array of messages), so a
// workload can parse /kodama/task.json. It is non-nil: presence is distinct
// from the nil task-free case, which materializes no file.
const emptyConversationJSON = "[]"

// parseMetadata validates and parses one opaque task metadata object. Invalid
// JSON, a non-object, a non-string value, a duplicate key, trailing bytes and
// oversize input are refused as invalid_input.
func parseMetadata(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxMetadataBytes {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata is %d bytes; at most %d are allowed", len(raw), maxMetadataBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fault.New(fault.InvalidInput, "task metadata must be a JSON object of string values")
	}
	metadata := make(map[string]string)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fault.New(fault.InvalidInput, "task metadata must be a JSON object of string values")
		}
		if _, duplicate := metadata[key]; duplicate {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata key %q is duplicated", key))
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		value, ok := valueToken.(string)
		if !ok {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task metadata value for %q must be a string", key))
		}
		metadata[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "task metadata must be a JSON object of string values", err)
		}
		_ = token
		return nil, fault.New(fault.InvalidInput, "task metadata has more than one JSON value")
	}
	return metadata, nil
}

// marshalMetadata re-encodes stored metadata for a fresh admission. Metadata is
// never interpreted, so the canonical object bytes are the only requirement.
func marshalMetadata(metadata map[string]string) []byte {
	if len(metadata) == 0 {
		return nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil
	}
	return encoded
}

// conversationBytes builds the admitted conversation snapshot from the stored
// messages plus, for a reply, the new answer message. Every caller is a
// task-bound admission, so a zero-message combined conversation is the present
// JSON empty array (non-nil, "[]"), never nil: nil is reserved for a task-free
// execution, and a task-bound run with nil bytes materializes no
// /kodama/task.json while KODAMA_TASK is still set. The empty array keeps the
// file valid JSON and consistent with the non-empty message-array encoding.
func conversationBytes(messages []Message, answer *Message) ([]byte, error) {
	combined := append([]Message(nil), messages...)
	if answer != nil {
		combined = append(combined, *answer)
	}
	if len(combined) == 0 {
		return []byte(emptyConversationJSON), nil
	}
	encoded, err := json.Marshal(combined)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "task conversation cannot be encoded", err)
	}
	if len(encoded) > maxConversationBytes {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("task conversation is %d bytes; at most %d are allowed", len(encoded), maxConversationBytes))
	}
	return encoded, nil
}
