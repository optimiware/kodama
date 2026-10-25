package stage

import (
	"encoding/json"
	"strings"
)

// HasEvent matches all fields in one event, never unrelated lines in a log dump.
func HasEvent(logs string, fields map[string]string) bool {
	for _, line := range strings.Split(logs, "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		match := true
		for k, v := range fields {
			if ev[k] != v {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
