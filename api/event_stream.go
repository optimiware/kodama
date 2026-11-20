package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// EventStreamMediaType is the MIME media type for the project HTTP Server-Sent
// Events stream.
const EventStreamMediaType = "text/event-stream"

// StreamEvent is one decoded project event emitted on the HTTP SSE stream
// (GET /api/v1/projects/{project}/events/stream) and rendered by CLI event
// commands. Unlike Event (whose Data is base64-encoded []byte in JSON),
// StreamEvent decodes JSON payloads into raw JSON in Data so CLI, TUI, and Web
// UI clients can consume event fields directly.
type StreamEvent struct {
	ID             string          `json:"id"`
	Cursor         string          `json:"cursor"`
	Sequence       uint64          `json:"sequence"`
	Type           string          `json:"type"`
	ReportType     string          `json:"report_type,omitempty"`
	Source         EventSource     `json:"source"`
	At             time.Time       `json:"at"`
	Scope          EventScope      `json:"scope"`
	SubjectID      string          `json:"subject_id,omitempty"`
	SubjectVersion uint64          `json:"subject_version,omitempty"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	CausationID    string          `json:"causation_id,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// ProjectEventStreamPath returns the HTTP SSE path for one project's live event
// stream. It sits outside the 61-operation JSON RPC route table alongside /mcp
// and /.well-known/kodema-configuration.
func ProjectEventStreamPath(projectID string) (string, error) {
	if !validID(projectID) {
		return "", invalid("project_id is invalid")
	}
	return "/api/v1/projects/" + projectID + "/events/stream", nil
}

// ParseProjectEventStreamPath matches /api/v1/projects/{project}/events/stream
// and returns the raw project path segment. Callers validate authentication
// before resolving or rejecting project visibility.
func ParseProjectEventStreamPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/api/v1/projects/") || !strings.HasSuffix(path, "/events/stream") {
		return "", false
	}
	if strings.ContainsAny(path, "?#* ") {
		return "", false
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) != 6 {
		return "", false
	}
	if segments[0] != "api" || segments[1] != "v1" || segments[2] != "projects" ||
		segments[4] != "events" || segments[5] != "stream" {
		return "", false
	}
	if segments[3] == "" {
		return "", false
	}
	return segments[3], true
}

// DecodeEventData converts raw committed event data bytes into a JSON value for
// human/TUI/UI consumers. Empty bytes return nil; valid JSON payloads are
// compacted and preserved as raw JSON; valid UTF-8 non-JSON text is encoded as
// a JSON string; arbitrary binary data falls back to {"raw_base64":"..."}.
func DecodeEventData(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, trimmed); err == nil {
			return json.RawMessage(compact.Bytes())
		}
	}
	if utf8.Valid(raw) {
		if encoded, err := json.Marshal(string(raw)); err == nil {
			return json.RawMessage(encoded)
		}
	}
	encoded, err := json.Marshal(map[string]string{
		"raw_base64": base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}

// EncodeEventCursor encodes a durable event sequence as the canonical opaque
// public cursor used by event.read, event.subscribe, SSE id frames, and
// Last-Event-ID reconnects.
func EncodeEventCursor(sequence uint64) (string, error) {
	return EncodeCursor(strconv.FormatUint(sequence, 10))
}

// DecodeEventCursor decodes a canonical opaque public event cursor into its
// durable 63-bit store sequence.
func DecodeEventCursor(cursor string) (uint64, error) {
	sequence, err := eventSequenceCursor(cursor)
	if err != nil {
		return 0, err
	}
	if sequence > (1<<63 - 1) {
		return 0, invalid("event cursor sequence is out of range")
	}
	return sequence, nil
}

// ValidateEventTypes validates a public lifecycle event type filter slice.
func ValidateEventTypes(types []string) error {
	return validateEventTypeFilter(types)
}
