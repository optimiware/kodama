//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// EvidenceWriter writes one allowlisted evidence record per file. Files are
// 0600 and the directory is 0700; a refused record never creates a file.
type EvidenceWriter struct {
	Dir string
	Now func() time.Time
	// Secrets are configured credentials; any string value containing one is
	// refused.
	Secrets []Secret

	sequence atomic.Uint64
}

// evidenceFields is the allowlist of evidence keys. The e2e rows carry the
// acceptance-criterion and trace-row identities plus the observed execution
// and cleanup states.
var evidenceFields = map[string]bool{
	"harness_revision":        true,
	"deployed_revision":       true,
	"image_digest":            true,
	"yaml_sha256":             true,
	"execution_id":            true,
	"resource_id":             true,
	"event_id":                true,
	"recorded_at":             true,
	"started_at":              true,
	"finished_at":             true,
	"duration_ms":             true,
	"outcome":                 true,
	"restoration":             true,
	"probe":                   true,
	"ac":                      true,
	"trace_row":               true,
	"execution_state":         true,
	"cleanup_state":           true,
	"scenario":                true,
	"installation_id":         true,
	"profile":                 true,
	"runtime":                 true,
	"server_image_digest":     true,
	"tools_image_digest":      true,
	"external_harness_digest": true,
	"task_id":                 true,
	"workspace_id":            true,
	"action_id":               true,
	"request_id":              true,
	"verification_id":         true,
	"report_id":               true,
	"target_ref":              true,
	"candidate":               true,
	"expected_base":           true,
	"decision":                true,
	"actor":                   true,
}

var (
	evidenceNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,80}$`)
	evidenceIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	evidenceProbePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,159}$`)
	imageDigestPattern   = regexp.MustCompile(`^(unpinned|sha256:[a-f0-9]{64}|[^@\s]+@sha256:[a-f0-9]{64})$`)
	yamlDigestPattern    = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	jwtPattern           = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	acPattern            = regexp.MustCompile(`^AC(0[1-9]|1[0-9]|20)$`)
	traceRowPattern      = regexp.MustCompile(`^[0-9]{1,2}\.[0-9]{1,2}[a-z]?$`)
	evidenceRefPattern   = regexp.MustCompile(`^refs/[A-Za-z0-9._/-]{1,200}$`)
	expectedBasePattern  = regexp.MustCompile(`^(none|[a-f0-9]{7,64})$`)
	evidenceLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

var evidenceOutcomes = map[string]bool{
	"passed":  true,
	"failed":  true,
	"skipped": true,
	"refused": true,
	"error":   true,
}

var evidenceRestorations = map[string]bool{
	"not_applicable": true,
	"restored":       true,
	"unrestored":     true,
	"failed":         true,
}

var evidenceExecutionStates = map[string]bool{
	"queued":    true,
	"running":   true,
	"succeeded": true,
	"failed":    true,
	"cancelled": true,
}

var evidenceCleanupStates = map[string]bool{
	"none":     true,
	"pending":  true,
	"complete": true,
}

// NewEvidenceWriter prepares an evidence directory. Existing evidence is never
// overwritten.
func NewEvidenceWriter(dir string) (*EvidenceWriter, error) {
	cleaned, err := validateEvidenceDir(dir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(cleaned); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("the evidence directory must not be a symlink")
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("the evidence path is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot inspect the evidence directory: %v", err)
	} else if err := os.MkdirAll(cleaned, 0o700); err != nil {
		return nil, fmt.Errorf("create the evidence directory: %v", err)
	}
	return &EvidenceWriter{Dir: cleaned, Now: time.Now}, nil
}

// Write validates one record and writes it as name.json at mode 0600.
func (writer *EvidenceWriter) Write(name string, fields map[string]any) (string, error) {
	if writer == nil || writer.Dir == "" {
		return "", fmt.Errorf("the evidence writer has no directory")
	}
	if !evidenceNamePattern.MatchString(name) {
		return "", fmt.Errorf("evidence name is not a bounded lower-case file name")
	}
	if len(fields) == 0 {
		return "", fmt.Errorf("evidence record has no fields")
	}
	now := time.Now
	if writer.Now != nil {
		now = writer.Now
	}
	at := now().UTC()
	record := make(map[string]any, len(fields))
	for key, value := range fields {
		if !evidenceFields[key] {
			return "", fmt.Errorf("evidence field is not allowlisted")
		}
		if text, ok := value.(string); ok && writer.containsSecret(text) {
			return "", fmt.Errorf("evidence value contains configured credential material")
		}
		normalized, err := normalizeEvidenceValue(key, value)
		if err != nil {
			return "", fmt.Errorf("evidence field: %w", err)
		}
		record[key] = normalized
	}
	if _, ok := record["recorded_at"]; !ok {
		record["recorded_at"] = at.Format(time.RFC3339Nano)
	}

	fileName := writer.fileName(name, at)
	path := filepath.Join(writer.Dir, fileName)
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("evidence file already exists")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("cannot inspect evidence file")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create evidence file")
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("set evidence file mode")
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
		return "", fmt.Errorf("write evidence file")
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync evidence file")
	}
	cleanup = false
	return path, nil
}

// fileName returns a run-unique evidence file name: the timestamp makes a
// rerun in the same directory distinct, and the process-local sequence makes
// two writes in the same instant distinct.
func (writer *EvidenceWriter) fileName(name string, at time.Time) string {
	return evidenceFileName(name, at, writer.sequence.Add(1))
}

// evidenceFileName builds the deterministic name for one sequence number.
func evidenceFileName(name string, at time.Time, sequence uint64) string {
	base := strings.TrimSuffix(name, ".json")
	return fmt.Sprintf("%s-%s-%06d.json", base, at.Format("20060102T150405.000000000Z"), sequence)
}

// containsSecret reports whether a string value contains any configured
// credential, including a Basic-encoded form.
func (writer *EvidenceWriter) containsSecret(value string) bool {
	return ScanCredentials(value, writer.Secrets...) != nil
}

// normalizeEvidenceValue validates and canonicalizes one allowlisted value.
func normalizeEvidenceValue(key string, value any) (any, error) {
	switch typed := value.(type) {
	case string:
		if len(typed) == 0 || len(typed) > 512 {
			return nil, fmt.Errorf("string value must be 1..512 bytes")
		}
		if strings.ContainsAny(typed, "\x00\n\r\t") {
			return nil, fmt.Errorf("string value contains a control character")
		}
		if looksLikeCredential(typed) {
			return nil, fmt.Errorf("value looks like credential material")
		}
		switch key {
		case "harness_revision", "deployed_revision":
			if !revisionPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a git revision")
			}
		case "image_digest":
			if !imageDigestPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not an image digest")
			}
		case "yaml_sha256":
			if !yamlDigestPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a document digest")
			}
		case "execution_id", "resource_id", "event_id", "installation_id", "task_id",
			"workspace_id", "action_id", "request_id", "verification_id", "report_id", "actor":
			if !evidenceIDPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a bounded identity")
			}
		case "profile", "runtime", "decision":
			if !evidenceLabelPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a bounded label")
			}
		case "server_image_digest", "tools_image_digest", "external_harness_digest":
			if !imageDigestPattern.MatchString(typed) || typed == "unpinned" {
				return nil, fmt.Errorf("value is not a pinned image digest")
			}
		case "target_ref":
			if !evidenceRefPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a full git ref")
			}
		case "candidate":
			if !revisionPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a git revision")
			}
		case "expected_base":
			if !expectedBasePattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a git revision or none")
			}
		case "probe", "scenario":
			if !evidenceProbePattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a bounded probe name")
			}
		case "ac":
			if !acPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not an acceptance criterion identity")
			}
		case "trace_row":
			if !traceRowPattern.MatchString(typed) {
				return nil, fmt.Errorf("value is not a trace row identity")
			}
		case "outcome":
			if !evidenceOutcomes[typed] {
				return nil, fmt.Errorf("value is not a known outcome")
			}
		case "restoration":
			if !evidenceRestorations[typed] {
				return nil, fmt.Errorf("value is not a known restoration result")
			}
		case "execution_state":
			if !evidenceExecutionStates[typed] {
				return nil, fmt.Errorf("value is not a known execution state")
			}
		case "cleanup_state":
			if !evidenceCleanupStates[typed] {
				return nil, fmt.Errorf("value is not a known cleanup state")
			}
		case "recorded_at", "started_at", "finished_at":
			parsed, err := time.Parse(time.RFC3339Nano, typed)
			if err != nil {
				return nil, fmt.Errorf("value is not an RFC3339 timestamp")
			}
			if _, offset := parsed.Zone(); offset != 0 {
				return nil, fmt.Errorf("value is not a UTC timestamp")
			}
			return parsed.UTC().Format(time.RFC3339Nano), nil
		default:
			return nil, fmt.Errorf("field has no string validator")
		}
		return typed, nil
	case time.Time:
		switch key {
		case "recorded_at", "started_at", "finished_at":
			if _, offset := typed.Zone(); offset != 0 {
				return nil, fmt.Errorf("timestamp is not UTC")
			}
			return typed.UTC().Format(time.RFC3339Nano), nil
		default:
			return nil, fmt.Errorf("field does not accept a timestamp")
		}
	case int:
		return evidenceDuration(key, int64(typed))
	case int64:
		return evidenceDuration(key, typed)
	case uint64:
		if typed > uint64(1<<62) {
			return nil, fmt.Errorf("duration is too large")
		}
		return evidenceDuration(key, int64(typed))
	case time.Duration:
		return evidenceDuration(key, typed.Milliseconds())
	default:
		return nil, fmt.Errorf("unsupported value type %T", value)
	}
}

// evidenceDuration accepts only the duration field.
func evidenceDuration(key string, value int64) (any, error) {
	if key != "duration_ms" {
		return nil, fmt.Errorf("field does not accept a duration")
	}
	if value < 0 {
		return nil, fmt.Errorf("duration must not be negative")
	}
	return value, nil
}

// looksLikeCredential refuses values that carry credential material even under
// an allowlisted key.
func looksLikeCredential(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"bearer ", "authorization:", "password", "secret", "access_code",
		"-----begin", "token=", "token:",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return jwtPattern.MatchString(value)
}
