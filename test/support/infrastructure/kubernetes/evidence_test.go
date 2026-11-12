package kubesupport

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestClusterEvidencePreservesSkippedStatus proves a test that skipped its
// readiness barrier is never recorded as a passing cluster. A skip means the
// scenario did not run; recording it as "passed" would manufacture evidence.
func TestClusterEvidencePreservesSkippedStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KODAMA_EVIDENCE_DIR", dir)

	t.Run("skipped", func(t *testing.T) {
		RecordCluster(t, ClusterRecord{Cluster: "kodama-t-100-abcdef01"})
		t.Skip("kind readiness skipped: KODAMA_REQUIRE_INFRA is not 1")
	})
	t.Run("passed", func(t *testing.T) {
		RecordCluster(t, ClusterRecord{Cluster: "kodama-t-100-abcdef02"})
	})

	records := readClusterEvidence(t, filepath.Join(dir, "kubernetes-results.jsonl"))
	if len(records) != 2 {
		t.Fatalf("recorded clusters = %d; want 2 (%+v)", len(records), records)
	}
	byCluster := make(map[string]ClusterRecord, len(records))
	for _, record := range records {
		byCluster[record.Cluster] = record
	}
	skipped, ok := byCluster["kodama-t-100-abcdef01"]
	if !ok {
		t.Fatalf("skipped cluster was not recorded: %+v", records)
	}
	if skipped.Result != "skipped" {
		t.Fatalf("skipped cluster result = %q; want %q", skipped.Result, "skipped")
	}
	passed, ok := byCluster["kodama-t-100-abcdef02"]
	if !ok {
		t.Fatalf("passed cluster was not recorded: %+v", records)
	}
	if passed.Result != "passed" {
		t.Fatalf("passed cluster result = %q; want %q", passed.Result, "passed")
	}
}

// readClusterEvidence decodes the JSONL evidence file.
func readClusterEvidence(t *testing.T, path string) []ClusterRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open evidence file: %v", err)
	}
	defer func() { _ = file.Close() }()
	var records []ClusterRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record ClusterRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode evidence line %q: %v", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan evidence file: %v", err)
	}
	return records
}
