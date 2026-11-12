package runtimesupport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ResultRecord is one conformance result written to the harness evidence
// directory.
type ResultRecord struct {
	Adapter      string    `json:"adapter"`
	Scenario     string    `json:"scenario"`
	Result       string    `json:"result"`
	ImageTag     string    `json:"image_tag,omitempty"`
	ImageDigest  string    `json:"image_digest,omitempty"`
	Podman       string    `json:"podman_version,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	RecordedAt   time.Time `json:"recorded_at"`
	RequiredMode string    `json:"required_mode,omitempty"`
}

// RecordResult registers a cleanup that writes the scenario result to
// <KODAMA_EVIDENCE_DIR>/runtime-results.jsonl and logs it. A missing evidence
// directory leaves logging only; it never fabricates a file.
func RecordResult(t testing.TB, record ResultRecord) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			record.Result = "failed"
		} else if record.Result == "" {
			record.Result = "passed"
		}
		record.RecordedAt = time.Now().UTC()
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Logf("runtime evidence: %v", err)
			return
		}
		t.Logf("runtime evidence: %s", encoded)
		dir := os.Getenv("KODAMA_EVIDENCE_DIR")
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("runtime evidence dir: %v", err)
			return
		}
		path := filepath.Join(dir, "runtime-results.jsonl")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Logf("runtime evidence open: %v", err)
			return
		}
		defer func() { _ = file.Close() }()
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Logf("runtime evidence write: %v", err)
		}
	})
}
