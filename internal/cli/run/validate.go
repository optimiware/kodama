package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// cursorPattern is the Cursor schema of api/common.yaml. A cursor that does not
// match is never adopted: it could not be a valid Last-Event-ID, and adopting
// it would turn the next reconnect into a request the server must reject.
var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

const maxCursorBytes = 2048

func validCursor(c string) bool { return len(c) <= maxCursorBytes && cursorPattern.MatchString(c) }

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// maxSafeVersion is the largest integer api/observation.yaml allows.
const maxSafeVersion = 9007199254740991

// positiveVersion reads a resource version: an integer from 1 to 2^53-1.
func positiveVersion(n json.Number) (int64, bool) {
	if !integerLiteral.MatchString(n.String()) {
		return 0, false
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	return v, err == nil && v >= 1 && v <= maxSafeVersion
}

// strictObject decodes one JSON object, accepting additive members, and
// returns its members so required ones can be checked for presence.
func strictObject(data []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return m, true
}

// decodeSnapshot validates and decodes a snapshot document: a cursor and a
// resources array whose entries carry the members the CLI applies. Unknown
// members are accepted.
func decodeSnapshot(data []byte) (Snapshot, bool) {
	members, ok := strictObject(data)
	if !ok {
		return Snapshot{}, false
	}
	if _, present := members["resources"]; !present {
		return Snapshot{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var s Snapshot
	if err := dec.Decode(&s); err != nil || s.Resources == nil {
		return Snapshot{}, false
	}
	if !validSnapshot(s) {
		return Snapshot{}, false
	}
	return s, true
}

// validSnapshot checks a snapshot however it was obtained. Observer
// implementations are injectable, so the watcher checks them again.
func validSnapshot(s Snapshot) bool {
	if !validCursor(s.Cursor) {
		return false
	}
	for _, r := range s.Resources {
		switch r.Type {
		case "work_item", "run", "step_attempt":
		default:
			return false
		}
		if !idPattern.MatchString(r.ID) || r.State == "" {
			return false
		}
		if _, ok := positiveVersion(r.Version); !ok {
			return false
		}
	}
	return true
}

// decodeCheckpoint validates a checkpoint frame's payload.
func decodeCheckpoint(data []byte) (string, bool) {
	members, ok := strictObject(data)
	if !ok {
		return "", false
	}
	var cursor string
	if raw, present := members["cursor"]; !present || json.Unmarshal(raw, &cursor) != nil || !validCursor(cursor) {
		return "", false
	}
	return cursor, true
}

// validResync checks the resync_required payload: code cursor_expired and a
// message, per api/observation.yaml.
func validResync(data []byte) bool {
	members, ok := strictObject(data)
	if !ok {
		return false
	}
	var code, msg string
	if json.Unmarshal(members["code"], &code) != nil || code != "cursor_expired" {
		return false
	}
	return json.Unmarshal(members["message"], &msg) == nil
}

// parseRetryAfter reads Retry-After as the shared client does: a strict
// non-negative number of seconds, or an HTTP date in the future. Anything
// else is no guidance.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 || secs > 1<<31 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
