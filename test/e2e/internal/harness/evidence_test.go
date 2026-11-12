//go:build e2e

package harness

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHarnessEvidenceWriterAllowlist proves evidence is allowlisted JSON written
// at 0600, and that an unknown key or a credential-shaped value is refused
// before any file exists.
func TestHarnessEvidenceWriterAllowlist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	writer, err := NewEvidenceWriter(root)
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	if info, err := os.Stat(root); err != nil {
		t.Fatalf("stat evidence directory: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("evidence directory mode = %o, want 700", info.Mode().Perm())
	}

	recorded := time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC)
	path, err := writer.Write("ac01", map[string]any{
		"probe":            "TestE2E_AC01KubernetesPipeline",
		"ac":               "AC01",
		"trace_row":        "1.2",
		"harness_revision": "be43abd",
		"image_digest":     "sha256:" + strings.Repeat("a", 64),
		"yaml_sha256":      "sha256:" + strings.Repeat("b", 64),
		"execution_id":     "exec-01",
		"resource_id":      "res-01",
		"event_id":         "evt-01",
		"execution_state":  "succeeded",
		"cleanup_state":    "complete",
		"recorded_at":      recorded,
		"started_at":       recorded.Add(-time.Second),
		"finished_at":      recorded,
		"duration_ms":      int64(1000),
		"outcome":          "passed",
		"restoration":      "not_applicable",
	})
	if err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	if filepath.Ext(path) != ".json" {
		t.Fatalf("evidence path %q is not JSON", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat evidence file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("evidence mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("evidence is not JSON: %v", err)
	}
	if decoded["recorded_at"] != "2026-11-08T01:02:03Z" {
		t.Errorf("recorded_at = %v, want the UTC RFC3339 timestamp", decoded["recorded_at"])
	}
	if decoded["ac"] != "AC01" || decoded["trace_row"] != "1.2" {
		t.Errorf("evidence identity = ac %v row %v; want AC01/1.2", decoded["ac"], decoded["trace_row"])
	}
	if decoded["execution_state"] != "succeeded" || decoded["cleanup_state"] != "complete" {
		t.Errorf("evidence states = %v/%v; want succeeded/complete", decoded["execution_state"], decoded["cleanup_state"])
	}
	if _, err := writer.Write("unpinned-image", map[string]any{
		"outcome":      "passed",
		"image_digest": "unpinned",
	}); err != nil {
		t.Fatalf("the explicit unpinned image marker was refused: %v", err)
	}

	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read evidence directory: %v", err)
	}
	for _, rejection := range []struct {
		name   string
		fields map[string]any
	}{
		{
			name:   "unknown-key",
			fields: map[string]any{"outcome": "passed", "access_token": "not-allowed"},
		},
		{
			name:   "bearer-value",
			fields: map[string]any{"outcome": "passed", "resource_id": "Bearer abc.def.ghi"},
		},
		{
			name:   "jwt-value",
			fields: map[string]any{"outcome": "passed", "execution_id": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZG1pbiJ9.signature"},
		},
		{
			name:   "unknown-outcome",
			fields: map[string]any{"outcome": "maybe"},
		},
		{
			name:   "unknown-ac",
			fields: map[string]any{"outcome": "passed", "ac": "AC21"},
		},
		{
			name:   "unknown-trace-row",
			fields: map[string]any{"outcome": "passed", "trace_row": "1"},
		},
		{
			name:   "unknown-execution-state",
			fields: map[string]any{"outcome": "passed", "execution_state": "done"},
		},
		{
			name:   "unknown-cleanup-state",
			fields: map[string]any{"outcome": "passed", "cleanup_state": "done"},
		},
		{
			name:   "invalid-yaml-digest",
			fields: map[string]any{"outcome": "passed", "yaml_sha256": "sha256:not-hex"},
		},
		{
			name:   "non-utc-timestamp",
			fields: map[string]any{"outcome": "passed", "recorded_at": "2026-11-08T01:02:03+01:00"},
		},
	} {
		t.Run(rejection.name, func(t *testing.T) {
			if _, err := writer.Write("rejected-"+rejection.name, rejection.fields); err == nil {
				t.Fatal("disallowed evidence was accepted")
			}
			after, err := os.ReadDir(root)
			if err != nil {
				t.Fatalf("read evidence directory after refusal: %v", err)
			}
			if len(after) != len(before) {
				t.Fatalf("a refused evidence record left a file: %d files before, %d after", len(before), len(after))
			}
		})
	}
}

// TestHarnessEvidenceWriterIsRunUnique proves a rerun into the same evidence
// directory gets distinct file names and that an existing path is never
// overwritten.
func TestHarnessEvidenceWriterIsRunUnique(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	writer, err := NewEvidenceWriter(root)
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	first, err := writer.Write("rerun", map[string]any{"outcome": "passed"})
	if err != nil {
		t.Fatalf("first evidence write: %v", err)
	}
	second, err := writer.Write("rerun", map[string]any{"outcome": "passed"})
	if err != nil {
		t.Fatalf("second evidence write: %v", err)
	}
	if first == second {
		t.Fatalf("two evidence writes reused the same name %s", first)
	}
	for _, path := range []string{first, second} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("evidence file %s is missing or not 0600: %v", path, err)
		}
	}

	fixed := time.Date(2026, time.November, 8, 1, 2, 3, 0, time.UTC)
	writer.Now = func() time.Time { return fixed }
	writer.sequence.Store(0)
	expected := evidenceFileName("collision", fixed, 1)
	existing := []byte("existing evidence")
	if err := os.WriteFile(filepath.Join(root, expected), existing, 0o600); err != nil {
		t.Fatalf("precreate evidence file: %v", err)
	}
	if _, err := writer.Write("collision", map[string]any{"outcome": "passed"}); err == nil {
		t.Fatal("an existing evidence file was overwritten")
	}
	data, err := os.ReadFile(filepath.Join(root, expected))
	if err != nil || string(data) != string(existing) {
		t.Fatalf("existing evidence changed: data=%q err=%v", data, err)
	}

	writer.sequence.Store(0)
	directoryName := evidenceFileName("collision-dir", fixed, 1)
	if err := os.Mkdir(filepath.Join(root, directoryName), 0o700); err != nil {
		t.Fatalf("precreate evidence directory name: %v", err)
	}
	if _, err := writer.Write("collision-dir", map[string]any{"outcome": "passed"}); err == nil {
		t.Fatal("an existing subdirectory name was accepted")
	}
}

// TestHarnessEvidenceWriterRefusesConfiguredTokenAndDoesNotEcho proves a value
// containing a configured token is refused and no rejected value is echoed.
func TestHarnessEvidenceWriterRefusesConfiguredTokenAndDoesNotEcho(t *testing.T) {
	writer, err := NewEvidenceWriter(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	writer.Secrets = []Secret{"configured-token-value"}
	rejected := "prefix-configured-token-value-suffix"
	_, err = writer.Write("token-value", map[string]any{
		"outcome":     "passed",
		"resource_id": rejected,
	})
	if err == nil {
		t.Fatal("a value containing a configured token was accepted")
	}
	if strings.Contains(err.Error(), "configured-token-value") || strings.Contains(err.Error(), rejected) {
		t.Fatalf("evidence error echoed the rejected value: %v", err)
	}
}

// TestHarnessEvidenceWriterRefusesBasicConfiguredToken proves a Basic-encoded
// form of a configured credential is refused without echoing the material.
func TestHarnessEvidenceWriterRefusesBasicConfiguredToken(t *testing.T) {
	writer, err := NewEvidenceWriter(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	writer.Secrets = []Secret{"project-token-value"}
	for _, basic := range []string{
		base64.StdEncoding.EncodeToString([]byte("kodama:project-token-value")),
		base64.StdEncoding.EncodeToString([]byte("token:project-token-value")),
	} {
		_, err := writer.Write("basic-token", map[string]any{
			"outcome":     "passed",
			"resource_id": "prefix-" + basic,
		})
		if err == nil {
			t.Fatalf("Basic-encoded configured token %q was accepted", basic)
		}
		if strings.Contains(err.Error(), basic) || strings.Contains(err.Error(), "project-token-value") {
			t.Fatalf("evidence error echoed the rejected Basic material: %v", err)
		}
	}
}

// TestHarnessEvidenceReleaseFields proves the release-smoke allowlist carries
// the installation, scenario, task/workspace and Git report identities while
// still refusing unknown keys.
func TestHarnessEvidenceReleaseFields(t *testing.T) {
	writer, err := NewEvidenceWriter(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	installationDigest := "sha256:" + strings.Repeat("c", 64)
	_, err = writer.Write("release-row", map[string]any{
		"scenario":                "ProtectedTask",
		"installation_id":         "installation-w49-test",
		"profile":                 "test",
		"runtime":                 "podman",
		"server_image_digest":     installationDigest,
		"tools_image_digest":      installationDigest,
		"external_harness_digest": installationDigest,
		"execution_id":            "exec-01",
		"task_id":                 "task-01",
		"workspace_id":            "ws-0123456789abcdef0123456789abcdef",
		"action_id":               "act-01",
		"request_id":              "req-01",
		"verification_id":         "ver-01",
		"report_id":               "rep-01",
		"resource_id":             "res-01",
		"candidate":               "0123456789abcdef0123456789abcdef01234567",
		"expected_base":           "none",
		"target_ref":              "refs/heads/main",
		"decision":                "pass",
		"actor":                   "oidc-0123456789abcdef0123456789abcdef",
		"outcome":                 "passed",
		"image_digest":            "unpinned",
	})
	if err != nil {
		t.Fatalf("release evidence fields were refused: %v", err)
	}
	for _, rejected := range []struct {
		name   string
		fields map[string]any
	}{
		{name: "unknown", fields: map[string]any{"outcome": "passed", "database_url": "postgres://x"}},
		{name: "bad-ref", fields: map[string]any{"outcome": "passed", "target_ref": "main"}},
		{name: "bad-base", fields: map[string]any{"outcome": "passed", "expected_base": "zeroes"}},
		{name: "bad-digest", fields: map[string]any{"outcome": "passed", "server_image_digest": "latest"}},
	} {
		t.Run(rejected.name, func(t *testing.T) {
			if _, err := writer.Write("reject-"+rejected.name, rejected.fields); err == nil {
				t.Fatal("disallowed release evidence was accepted")
			}
		})
	}
}

// TestHarnessEvidenceWriterRefusesEscapes proves the evidence name cannot leave
// the configured directory.
func TestHarnessEvidenceWriterRefusesEscapes(t *testing.T) {
	writer, err := NewEvidenceWriter(filepath.Join(t.TempDir(), "evidence"))
	if err != nil {
		t.Fatalf("create evidence writer: %v", err)
	}
	for _, name := range []string{"../escape", "nested/escape", "", ".hidden"} {
		if _, err := writer.Write(name, map[string]any{"outcome": "passed"}); err == nil {
			t.Errorf("evidence name %q was accepted", name)
		}
	}
}
