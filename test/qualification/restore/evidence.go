package restorequal

// Q03 drill evidence. The record is an allowlist of the facts a drill may
// publish: timeline milestones, durable IDs, image digests, measured durations
// and the outcome. It carries no environment value, credential, token or
// connection string.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// MilestoneRecord is one evidence milestone.
type MilestoneRecord struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// WatermarkRecord is one evidence watermark.
type WatermarkRecord struct {
	At  time.Time `json:"at"`
	IDs []string  `json:"ids"`
}

// ImageRecord is one pinned image identity.
type ImageRecord struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// RPOBase names how the recorded RPO was obtained. It is mandatory so a reader
// never mistakes a same-commit local drill for a measured backup-age RPO.
type RPOBase string

const (
	// RPOBaseLocalSameCommit is the local drill: the committed and restored
	// watermarks are the same commit, so rpo_ms is 0 by construction.
	RPOBaseLocalSameCommit RPOBase = "local-same-commit"
	// RPOBaseBackupAge is the stage drill: RPO is the age of the most recent
	// recoverable backup at the failure point.
	RPOBaseBackupAge RPOBase = "backup-age"
)

func validRPOBase(basis RPOBase) bool {
	return basis == RPOBaseLocalSameCommit || basis == RPOBaseBackupAge
}

// Evidence is the allowlisted record of one drill. The JSON keys are exactly
// these fields; nothing else may be written.
type Evidence struct {
	Drill                    string            `json:"drill"`
	RunID                    string            `json:"run_id"`
	Environment              string            `json:"environment"`
	Outcome                  string            `json:"outcome"`
	SourceRevision           string            `json:"source_revision,omitempty"`
	Milestones               []MilestoneRecord `json:"milestones"`
	CommittedWatermark       *WatermarkRecord  `json:"committed_watermark,omitempty"`
	RestoredWatermark        *WatermarkRecord  `json:"restored_watermark,omitempty"`
	LostIDs                  []string          `json:"lost_ids"`
	UnresolvedResources      []string          `json:"unresolved_resources,omitempty"`
	UnrecoverableWorkspaces  []string          `json:"unrecoverable_workspaces,omitempty"`
	RPOBase                  RPOBase           `json:"rpo_basis"`
	RPOMilliseconds          int64             `json:"rpo_ms"`
	RTOMilliseconds          int64             `json:"rto_ms"`
	PostgreSQLRetentionHours *float64          `json:"postgresql_retention_hours,omitempty"`
	MeetsRPO                 *bool             `json:"meets_rpo,omitempty"`
	MeetsRTO                 bool              `json:"meets_rto"`
	MeetsPostgreSQLRecovery  *bool             `json:"meets_postgresql_recovery,omitempty"`
	Images                   []ImageRecord     `json:"images,omitempty"`
}

// evidenceAllowedKeys is the closed set of top-level JSON keys an Evidence
// document may contain.
var evidenceAllowedKeys = map[string]bool{
	"committed_watermark":        true,
	"drill":                      true,
	"environment":                true,
	"images":                     true,
	"lost_ids":                   true,
	"meets_postgresql_recovery":  true,
	"meets_rpo":                  true,
	"meets_rto":                  true,
	"milestones":                 true,
	"outcome":                    true,
	"postgresql_retention_hours": true,
	"restored_watermark":         true,
	"rpo_basis":                  true,
	"rpo_ms":                     true,
	"rto_ms":                     true,
	"run_id":                     true,
	"source_revision":            true,
	"unrecoverable_workspaces":   true,
	"unresolved_resources":       true,
}

var (
	safeEvidenceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	jwtEvidencePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
)

func validEvidenceID(value string) bool {
	return safeEvidenceIDPattern.MatchString(value) && !jwtEvidencePattern.MatchString(value)
}

// TimelineEvidence converts a measured timeline into the evidence record. The
// basis decides whether meets_rpo is published: a local same-commit drill has no
// backup-age measurement to report, so its meets_rpo is omitted.
func TimelineEvidence(drill, runID, environment, outcome string, basis RPOBase, timeline *Timeline) (Evidence, error) {
	assessment, err := timeline.Assess()
	if err != nil {
		return Evidence{}, err
	}
	milestones := make([]MilestoneRecord, 0, len(timeline.milestones))
	for _, milestone := range timeline.milestones {
		milestones = append(milestones, MilestoneRecord(milestone))
	}
	evidence := Evidence{
		Drill:           drill,
		RunID:           runID,
		Environment:     environment,
		Outcome:         outcome,
		Milestones:      milestones,
		LostIDs:         append([]string{}, assessment.LostIDs...),
		RPOBase:         basis,
		RPOMilliseconds: int64(assessment.RPO / time.Millisecond),
		RTOMilliseconds: int64(assessment.RTO / time.Millisecond),
		MeetsRTO:        assessment.MeetsRTO,
	}
	if basis == RPOBaseBackupAge {
		meetsRPO := assessment.MeetsRPO
		evidence.MeetsRPO = &meetsRPO
	}
	if assessment.PostgreSQLRetention != nil && assessment.MeetsPostgreSQLRecovery != nil {
		hours := assessment.PostgreSQLRetention.Hours()
		meetsPG := *assessment.MeetsPostgreSQLRecovery
		evidence.PostgreSQLRetentionHours = &hours
		evidence.MeetsPostgreSQLRecovery = &meetsPG
	}
	if timeline.committed != nil {
		evidence.CommittedWatermark = &WatermarkRecord{
			At:  timeline.committed.At,
			IDs: append([]string(nil), timeline.committed.IDs...),
		}
	}
	if timeline.restored != nil {
		evidence.RestoredWatermark = &WatermarkRecord{
			At:  timeline.restored.At,
			IDs: append([]string(nil), timeline.restored.IDs...),
		}
	}
	if err := evidence.validate(); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

func (evidence Evidence) validate() error {
	if !validEvidenceID(evidence.Drill) {
		return fmt.Errorf("%w: drill is required and must be a bounded identifier", ErrInvalidEvidence)
	}
	if !validEvidenceID(evidence.RunID) {
		return fmt.Errorf("%w: run ID is required and must be a bounded identifier", ErrInvalidEvidence)
	}
	if !validEvidenceID(evidence.Environment) {
		return fmt.Errorf("%w: environment is required and must be a bounded identifier", ErrInvalidEvidence)
	}
	if !validEvidenceID(evidence.Outcome) {
		return fmt.Errorf("%w: outcome is required and must be a bounded identifier", ErrInvalidEvidence)
	}
	if evidence.SourceRevision != "" && !validEvidenceID(evidence.SourceRevision) {
		return fmt.Errorf("%w: source_revision is not a safe revision identifier", ErrInvalidEvidence)
	}
	if !validRPOBase(evidence.RPOBase) {
		return fmt.Errorf("%w: rpo_basis %q is not a known basis", ErrInvalidEvidence, evidence.RPOBase)
	}
	for _, list := range []struct {
		field  string
		values []string
	}{
		{"lost_ids", evidence.LostIDs},
		{"unresolved_resources", evidence.UnresolvedResources},
		{"unrecoverable_workspaces", evidence.UnrecoverableWorkspaces},
	} {
		for _, value := range list.values {
			if !validEvidenceID(value) {
				return fmt.Errorf("%w: %s contains an unsafe identifier", ErrInvalidEvidence, list.field)
			}
		}
	}
	for _, watermark := range []*WatermarkRecord{evidence.CommittedWatermark, evidence.RestoredWatermark} {
		if watermark == nil {
			continue
		}
		for _, value := range watermark.IDs {
			if !validEvidenceID(value) {
				return fmt.Errorf("%w: watermark contains an unsafe identifier", ErrInvalidEvidence)
			}
		}
	}
	if evidence.PostgreSQLRetentionHours != nil {
		hours := *evidence.PostgreSQLRetentionHours
		if math.IsNaN(hours) || math.IsInf(hours, 0) || hours < 0 {
			return fmt.Errorf("%w: postgresql_retention_hours must be a finite non-negative number", ErrInvalidEvidence)
		}
	}
	return nil
}

// ErrInvalidEvidence reports an incomplete evidence record or unsafe path.
var ErrInvalidEvidence = errors.New("invalid drill evidence")

// WriteEvidence writes one allowlisted evidence record atomically into
// directory and returns the file path. name must be a single path component.
func WriteEvidence(directory, name string, record Evidence) (string, error) {
	if directory == "" {
		return "", fmt.Errorf("%w: evidence directory is required", ErrInvalidEvidence)
	}
	if !validEvidenceName(name) {
		return "", fmt.Errorf("%w: evidence name %q is not a single path component", ErrInvalidEvidence, name)
	}
	if err := record.validate(); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode drill evidence: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("verify drill evidence allowlist: %w", err)
	}
	for key := range raw {
		if !evidenceAllowedKeys[key] {
			return "", fmt.Errorf("%w: evidence key %q is not allowlisted", ErrInvalidEvidence, key)
		}
	}
	data = append(data, '\n')
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create evidence directory: %w", err)
	}
	temp, err := os.CreateTemp(directory, ".evidence-*")
	if err != nil {
		return "", fmt.Errorf("create evidence temp file: %w", err)
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("set evidence mode: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("write evidence: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close evidence: %w", err)
	}
	path := filepath.Join(directory, name)
	if err := os.Rename(tempPath, path); err != nil {
		return "", fmt.Errorf("publish evidence: %w", err)
	}
	return path, nil
}

// validEvidenceName reports whether name is a single, non-special component.
func validEvidenceName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return filepath.Base(name) == name && !filepath.IsAbs(name)
}
