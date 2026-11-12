package restorequal

// Q03 evidence tests. The drill evidence record is an allowlist: only the
// named fields below may be written, and no environment value can enter it.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// validEvidenceRecord is a complete record that passes validate().
func validEvidenceRecord() Evidence {
	return Evidence{
		Drill:       "local-restore",
		RunID:       "abc123def456",
		Environment: "local-podman",
		Outcome:     "confirmed",
		RPOBase:     RPOBaseLocalSameCommit,
	}
}

// recordDrillEvidence writes one drill record to KODAMA_EVIDENCE_DIR, or to the
// ignored module-root scratch path <module root>/.harness/evidence/Q03 when
// that variable is unset, and returns the written path. It lives in test code
// so the non-test evidence helper has no testing dependency.
func recordDrillEvidence(t testing.TB, name string, record Evidence) string {
	t.Helper()
	directory := os.Getenv("KODAMA_EVIDENCE_DIR")
	if directory == "" {
		directory = defaultEvidenceDirectory()
	}
	path, err := WriteEvidence(directory, name, record)
	if err != nil {
		t.Logf("Q03 evidence: %v", err)
		return ""
	}
	t.Logf("Q03 evidence: %s", path)
	return path
}

func defaultEvidenceDirectory() string {
	dir, err := os.Getwd()
	if err == nil {
		for {
			if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
				return filepath.Join(dir, ".harness", "evidence", "Q03")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return filepath.Join(os.TempDir(), "kodama-q03-evidence")
}

func TestEvidenceAllowlist(t *testing.T) {
	t.Setenv("SCW_SECRET_KEY", "EVIDENCE-SECRET-SENTINEL")

	directory := t.TempDir()
	meetsRPO := true
	meetsPG := true
	retentionHours := 720.0
	record := Evidence{
		Drill:          "local-restore",
		RunID:          "abc123def456",
		Environment:    "local-podman",
		Outcome:        "confirmed",
		SourceRevision: "e7196bf",
		Milestones: []MilestoneRecord{
			{Name: MilestoneBackupStarted, At: timelineEpoch},
			{Name: MilestoneBackupCompleted, At: timelineEpoch.Add(time.Minute)},
		},
		CommittedWatermark:       &WatermarkRecord{At: timelineEpoch, IDs: []string{"receipt-1"}},
		RestoredWatermark:        &WatermarkRecord{At: timelineEpoch, IDs: []string{"receipt-1"}},
		LostIDs:                  []string{},
		UnresolvedResources:      []string{"workload-unknown"},
		UnrecoverableWorkspaces:  []string{"workspace-i02f"},
		RPOBase:                  RPOBaseBackupAge,
		RPOMilliseconds:          0,
		RTOMilliseconds:          int64(time.Minute / time.Millisecond),
		PostgreSQLRetentionHours: &retentionHours,
		MeetsRPO:                 &meetsRPO,
		MeetsRTO:                 true,
		MeetsPostgreSQLRecovery:  &meetsPG,
		Images: []ImageRecord{
			{Tag: "postgres:18-alpine", Digest: "sha256:abc"},
		},
	}
	path, err := WriteEvidence(directory, "run-abc123def456.json", record)
	if err != nil {
		t.Fatalf("WriteEvidence: %v", err)
	}
	if filepath.Dir(path) != directory {
		t.Fatalf("evidence path = %q; want it under %q", path, directory)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if strings.Contains(string(data), "EVIDENCE-SECRET-SENTINEL") {
		t.Fatal("evidence file contains an environment credential")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	want := []string{
		"committed_watermark",
		"drill",
		"environment",
		"images",
		"lost_ids",
		"meets_postgresql_recovery",
		"meets_rpo",
		"meets_rto",
		"milestones",
		"outcome",
		"postgresql_retention_hours",
		"restored_watermark",
		"rpo_basis",
		"rpo_ms",
		"rto_ms",
		"run_id",
		"source_revision",
		"unrecoverable_workspaces",
		"unresolved_resources",
	}
	if got := sortedKeys(decoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence keys = %v; want the allowlist %v", got, want)
	}

	// The record is not mutated by writing.
	if record.RunID != "abc123def456" || record.SourceRevision != "e7196bf" {
		t.Fatalf("WriteEvidence mutated its record: %+v", record)
	}
}

// TestEvidenceRejectsUnsafeNames passes a valid record so the path checks, not
// record validation, are what the test exercises.
func TestEvidenceRejectsUnsafeNames(t *testing.T) {
	sandbox := t.TempDir()
	directory := filepath.Join(sandbox, "evidence")
	record := validEvidenceRecord()

	for _, name := range []string{
		"",
		".",
		"..",
		"../escape.json",
		"sub/dir.json",
		`sub\dir.json`,
		filepath.Join(sandbox, "absolute.json"),
	} {
		if _, err := WriteEvidence(directory, name, record); err == nil {
			t.Errorf("WriteEvidence accepted name %q", name)
		}
	}

	if _, err := WriteEvidence("", "run.json", record); err == nil {
		t.Fatal("WriteEvidence accepted an empty directory")
	} else if !strings.Contains(err.Error(), "evidence directory is required") {
		t.Fatalf("empty directory error = %q; want the explicit missing-directory refusal", err)
	}

	if _, err := os.Stat(filepath.Join(sandbox, "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("a traversal name wrote outside the evidence directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "absolute.json")); !os.IsNotExist(err) {
		t.Fatalf("an absolute name wrote outside the evidence directory: %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("refused writes still created %q: %v", directory, err)
	}
}

func TestRecordEvidenceUsesTheRunDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("KODAMA_EVIDENCE_DIR", directory)
	record := validEvidenceRecord()
	path := recordDrillEvidence(t, "run-abc123def456.json", record)
	if filepath.Dir(path) != directory {
		t.Fatalf("recorded path = %q; want a file in %q", path, directory)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recorded evidence is missing: %v", err)
	}

	fallback := defaultEvidenceDirectory()
	if !filepath.IsAbs(fallback) {
		t.Fatalf("default evidence directory %q must be absolute", fallback)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(fallback)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("default evidence directory %q is not anchored at the module root: %v", fallback, err)
	}
}

func TestEvidenceValidation(t *testing.T) {
	directory := t.TempDir()
	valid := validEvidenceRecord()
	negativeHours := -1.0
	nanHours := math.NaN()
	cases := map[string]Evidence{
		"missing drill": {
			RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
		},
		"missing run ID": {
			Drill: valid.Drill, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
		},
		"missing environment": {
			Drill: valid.Drill, RunID: valid.RunID, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
		},
		"missing outcome": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, RPOBase: valid.RPOBase,
		},
		"missing rpo basis": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome,
		},
		"unknown rpo basis": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: "guessed",
		},
		"connection string in unresolved resources": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
			UnresolvedResources: []string{"postgres://user:secret@host/kodama"},
		},
		"jwt in unresolved resources": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
			UnresolvedResources: []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature"},
		},
		"unsafe unrecoverable workspace": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
			UnrecoverableWorkspaces: []string{"workspace with spaces=secret"},
		},
		"negative postgresql retention hours": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
			PostgreSQLRetentionHours: &negativeHours,
		},
		"NaN postgresql retention hours": {
			Drill: valid.Drill, RunID: valid.RunID, Environment: valid.Environment, Outcome: valid.Outcome, RPOBase: valid.RPOBase,
			PostgreSQLRetentionHours: &nanHours,
		},
	}
	for name, record := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := WriteEvidence(directory, "run.json", record); err == nil {
				t.Fatalf("WriteEvidence accepted an incomplete record: %+v", record)
			}
		})
	}
}

func TestTimelineEvidenceRPOBase(t *testing.T) {
	newTimeline := func(t *testing.T) *Timeline {
		t.Helper()
		now := timelineEpoch
		timeline, err := NewTimeline(func() time.Time { return now }, DefaultObjectives())
		if err != nil {
			t.Fatalf("NewTimeline: %v", err)
		}
		for _, milestone := range []struct {
			name string
			at   time.Time
		}{
			{name: MilestoneFailureInjected, at: timelineEpoch},
			{name: MilestoneRestoreCompleted, at: timelineEpoch.Add(time.Minute)},
			{name: MilestoneServiceResumed, at: timelineEpoch.Add(2 * time.Minute)},
		} {
			now = milestone.at
			if _, err := timeline.Mark(milestone.name); err != nil {
				t.Fatalf("Mark(%s): %v", milestone.name, err)
			}
		}
		if err := timeline.Commit(Watermark{At: timelineEpoch, IDs: []string{"receipt-1"}}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := timeline.Restore(Watermark{At: timelineEpoch, IDs: []string{"receipt-1"}}); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		return timeline
	}

	t.Run("local same-commit omits meets_rpo", func(t *testing.T) {
		evidence, err := TimelineEvidence("local-restore", "run-local", "local-podman", "confirmed", RPOBaseLocalSameCommit, newTimeline(t))
		if err != nil {
			t.Fatalf("TimelineEvidence: %v", err)
		}
		if evidence.RPOBase != RPOBaseLocalSameCommit {
			t.Fatalf("rpo basis = %q; want %q", evidence.RPOBase, RPOBaseLocalSameCommit)
		}
		if evidence.MeetsRPO != nil {
			t.Fatalf("local evidence publishes meets_rpo %v; want it omitted", *evidence.MeetsRPO)
		}
		if !evidence.MeetsRTO {
			t.Fatal("local evidence does not publish the measured RTO")
		}
	})

	t.Run("backup age publishes meets_rpo", func(t *testing.T) {
		evidence, err := TimelineEvidence("stage-restore", "run-stage", "scaleway-stage", "confirmed", RPOBaseBackupAge, newTimeline(t))
		if err != nil {
			t.Fatalf("TimelineEvidence: %v", err)
		}
		if evidence.RPOBase != RPOBaseBackupAge {
			t.Fatalf("rpo basis = %q; want %q", evidence.RPOBase, RPOBaseBackupAge)
		}
		if evidence.MeetsRPO == nil {
			t.Fatal("backup-age evidence omits the measured meets_rpo")
		}
		if !*evidence.MeetsRPO {
			t.Fatal("backup-age evidence reports meets_rpo false for a same-commit measurement")
		}
	})

	t.Run("unknown basis is refused", func(t *testing.T) {
		if _, err := TimelineEvidence("local-restore", "run-local", "local-podman", "confirmed", "guessed", newTimeline(t)); err == nil {
			t.Fatal("TimelineEvidence accepted an unknown rpo basis")
		}
	})
}
