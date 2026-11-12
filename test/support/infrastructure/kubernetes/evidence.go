// Cluster evidence records for the Kubernetes component suite.
package kubesupport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ClusterRecord is one recorded cluster/scenario result.
type ClusterRecord struct {
	Cluster         string    `json:"cluster"`
	KindVersion     string    `json:"kind_version"`
	KindSHA256      string    `json:"kind_sha256"`
	NodeImage       string    `json:"node_image"`
	NodeImageDigest string    `json:"node_image_digest"`
	Provider        string    `json:"provider"`
	StorageClass    string    `json:"storage_class"`
	Result          string    `json:"result,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	RecordedAt      time.Time `json:"recorded_at"`
}

// EvidenceRecord returns the pinned identity of this cluster.
func (c *Cluster) EvidenceRecord() ClusterRecord {
	return ClusterRecord{
		Cluster:         c.Name,
		KindVersion:     KindVersion,
		KindSHA256:      KindSHA256,
		NodeImage:       NodeImage,
		NodeImageDigest: NodeImageDigest,
		Provider:        "podman",
		StorageClass:    StorageClassName,
		CreatedAt:       c.createdAt,
	}
}

// RecordCluster registers a cleanup that writes the scenario result to
// <KODAMA_EVIDENCE_DIR>/kubernetes-results.jsonl and logs it. A missing evidence
// directory leaves logging only; it never fabricates a file.
func RecordCluster(t testing.TB, record ClusterRecord) {
	t.Helper()
	t.Cleanup(func() {
		switch {
		case t.Failed():
			record.Result = "failed"
		case t.Skipped():
			record.Result = "skipped"
		case record.Result == "":
			record.Result = "passed"
		}
		record.RecordedAt = time.Now().UTC()
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Logf("kubernetes evidence: %v", err)
			return
		}
		t.Logf("kubernetes evidence: %s", encoded)
		dir := os.Getenv("KODAMA_EVIDENCE_DIR")
		if dir == "" {
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Logf("kubernetes evidence dir: %v", err)
			return
		}
		path := filepath.Join(dir, "kubernetes-results.jsonl")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Logf("kubernetes evidence open: %v", err)
			return
		}
		defer func() { _ = file.Close() }()
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			t.Logf("kubernetes evidence write: %v", err)
		}
	})
}
