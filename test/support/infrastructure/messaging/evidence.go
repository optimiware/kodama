// Package messaging is component-test support for the messaging conformance
// suite.
//
// It starts the pinned NATS (JetStream enabled) and registry images through the
// infrastructure harness, builds a real nats.go client for stream/consumer
// inspection, provides a mutable credential directory for the
// live-invalidation scenarios, a bounded drop-ack TCP proxy and evidence
// recording.
//
// It is test support code under test/, never product code, and it makes no
// production claim.
package messaging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

const (
	// EvidenceDirEnv names the supplied evidence parent directory. Each test
	// process creates its own run directory below it and exports that run
	// directory so the infrastructure harness records image digests beside the
	// summary. This matches the PostgreSQL evidence lane's per-process
	// convention: the environment variable is a parent, never a directory
	// shared by twins.
	EvidenceDirEnv = "KODAMA_EVIDENCE_DIR"

	// summaryFileName is the results document published once per process.
	summaryFileName = "messaging-results.json"
	// summaryTempGlob stages a complete summary in a unique temporary file in
	// the same directory as the target; the "*" is replaced by the random
	// suffix os.CreateTemp generates.
	summaryTempGlob = "messaging-results-*.json.tmp"
	// runDirPrefix names one process's run directory under its evidence parent.
	runDirPrefix = "run-"
)

// GroupResult is one recorded test group result.
type GroupResult struct {
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	DurationMS int64             `json:"duration_ms"`
	Notes      map[string]string `json:"notes,omitempty"`
}

// ImageEvidence is one pinned image tag and the digest that served the suite.
type ImageEvidence struct {
	Tag     string `json:"tag"`
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
}

// Summary is the results file written next to the harness images.jsonl record.
type Summary struct {
	Card        string            `json:"card"`
	GeneratedAt time.Time         `json:"generated_at"`
	EvidenceDir string            `json:"evidence_dir"`
	ExitCode    int               `json:"exit_code"`
	Images      []ImageEvidence   `json:"images"`
	Groups      []GroupResult     `json:"groups"`
	Notes       map[string]string `json:"notes,omitempty"`
}

var (
	evidenceMu      sync.Mutex
	evidenceRoot    string
	evidenceInitErr error

	groups    []GroupResult
	images    map[string]ImageEvidence
	testNotes = map[string]map[string]string{}

	// Publication seams. Production always uses os.CreateTemp and os.Rename;
	// the unit tests observe that a complete document is staged in a unique
	// temporary file and renamed into place instead of being overwritten in
	// place, and inject write/chmod/close failures. Only tests replace them,
	// and never concurrently.
	createEvidenceTemp = func(dir, pattern string) (stagedFile, error) {
		return os.CreateTemp(dir, pattern)
	}
	renameEvidenceFile = os.Rename
)

// stagedFile is the subset of *os.File publishSummary needs for the staged
// summary document. The seam exists so tests can inject a write, chmod or close
// failure and prove every error path removes the staging file.
type stagedFile interface {
	Write([]byte) (int, error)
	Name() string
	Chmod(os.FileMode) error
	Close() error
}

// InitEvidence creates this process's unique run directory under the supplied
// evidence parent and exports it as KODAMA_EVIDENCE_DIR so the infrastructure
// harness records image digests into the same directory as the summary. It is
// idempotent: the directory is created and exported once per process.
func InitEvidence() (string, error) {
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	if evidenceRoot != "" {
		return evidenceRoot, nil
	}
	if evidenceInitErr != nil {
		return "", evidenceInitErr
	}
	root, err := createRunDir(evidenceParent())
	if err != nil {
		evidenceInitErr = err
		return "", err
	}
	if err := os.Setenv(EvidenceDirEnv, root); err != nil {
		evidenceInitErr = fmt.Errorf("export messaging evidence directory: %w", err)
		return "", evidenceInitErr
	}
	evidenceRoot = root
	return root, nil
}

// EvidenceDir returns this process's initialized run directory, or "" when
// initialization has not succeeded. A caller that requires evidence uses
// WriteSummary or Finalize, which report the failure.
func EvidenceDir() string {
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	return evidenceRoot
}

// RunMain is the component test binary's main sequence: it initializes this
// process's run directory before the tests, runs them, and publishes the
// required summary. Failed initialization or publication turns the run into a
// failure: a run with missing or incomplete evidence never reports success.
func RunMain(run func() int) int {
	if _, err := InitEvidence(); err != nil {
		fmt.Fprintf(os.Stderr, "messaging evidence: %v\n", err)
		return 1
	}
	return Finalize(run())
}

// Finalize publishes the required evidence summary and returns the process
// exit code. A failed publication turns the run into a failure: a run with
// missing or incomplete evidence never reports success.
func Finalize(exitCode int) int {
	path, err := WriteSummary(exitCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "messaging evidence: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "messaging evidence: %s\n", path)
	return exitCode
}

// evidenceParent resolves the supplied parent directory. KODAMA_EVIDENCE_DIR
// wins; otherwise the ignored local scratch parent
// <module root>/.harness/evidence/I02c-messaging is used, so no tracked source
// path is created.
func evidenceParent() string {
	if dir := os.Getenv(EvidenceDirEnv); dir != "" {
		return dir
	}
	return defaultEvidenceParent()
}

func defaultEvidenceParent() string {
	dir, err := os.Getwd()
	if err == nil {
		for {
			if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
				return filepath.Join(dir, ".harness", "evidence", "I02c-messaging")
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return filepath.Join(os.TempDir(), "kodama-i02c-evidence")
}

// createRunDir creates one process-unique run directory under parent. The
// process id makes the owner visible; os.MkdirTemp's random suffix makes the
// directory unique even when processes share the same parent concurrently.
// os.MkdirTemp creates the directory 0700; the run directory is a collector
// input, so it is widened to the conventional 0755 directory mode.
func createRunDir(parent string) (string, error) {
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create messaging evidence parent: %w", err)
	}
	root, err := os.MkdirTemp(parent, fmt.Sprintf("%s%d-", runDirPrefix, os.Getpid()))
	if err != nil {
		return "", fmt.Errorf("create messaging evidence run directory: %w", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		_ = os.RemoveAll(root)
		return "", fmt.Errorf("set messaging evidence run directory permissions: %w", err)
	}
	return root, nil
}

// TrackTest records one test group's name, status and duration. Call it at the
// start of every component test.
func TrackTest(t *testing.T) {
	t.Helper()
	start := time.Now()
	t.Cleanup(func() {
		evidenceMu.Lock()
		defer evidenceMu.Unlock()
		group := GroupResult{
			Name:       t.Name(),
			Status:     groupStatus(t),
			DurationMS: time.Since(start).Milliseconds(),
			Notes:      testNotes[t.Name()],
		}
		groups = append(groups, group)
	})
}

// groupStatus records one test's result. A failing result wins over a skip, but
// a skip is never folded into a pass.
func groupStatus(t *testing.T) string {
	return statusOf(t.Failed(), t.Skipped())
}

// statusOf is the status decision behind groupStatus, factored out so fail-wins
// precedence is directly testable. Unlike a plain switch on testing.T markers,
// it keeps a failed-and-skipped test a failure.
func statusOf(failed, skipped bool) string {
	switch {
	case failed:
		return "fail"
	case skipped:
		return "skip"
	default:
		return "pass"
	}
}

// Note attaches one evidence key/value to the current test group.
func Note(t *testing.T, key, value string) {
	t.Helper()
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	if testNotes[t.Name()] == nil {
		testNotes[t.Name()] = map[string]string{}
	}
	testNotes[t.Name()][key] = value
}

// RecordImage records one pinned image tag, digest and optional version.
func RecordImage(tag, digest, version string) {
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	if images == nil {
		images = map[string]ImageEvidence{}
	}
	images[tag] = ImageEvidence{Tag: tag, Digest: digest, Version: version}
}

// WriteSummary atomically publishes <run dir>/messaging-results.json and
// returns its path. The complete document is staged in a unique temporary file
// in the same directory and renamed into place, so a reader never observes a
// partial summary and a twin process cannot overwrite this process's summary.
func WriteSummary(exitCode int) (string, error) {
	dir, err := InitEvidence()
	if err != nil {
		return "", err
	}

	evidenceMu.Lock()
	summary := Summary{
		Card:        "I02c",
		GeneratedAt: time.Now().UTC(),
		EvidenceDir: dir,
		ExitCode:    exitCode,
		Images:      make([]ImageEvidence, 0, len(images)),
		Groups:      append([]GroupResult(nil), groups...),
	}
	for _, image := range images {
		summary.Images = append(summary.Images, image)
	}
	evidenceMu.Unlock()

	sort.Slice(summary.Images, func(i, j int) bool { return summary.Images[i].Tag < summary.Images[j].Tag })
	sort.SliceStable(summary.Groups, func(i, j int) bool { return summary.Groups[i].Name < summary.Groups[j].Name })

	payload, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode messaging evidence: %w", err)
	}
	payload = append(payload, '\n')
	return publishSummary(dir, payload)
}

// publishSummary atomically publishes payload as <dir>/messaging-results.json.
// The staging file is created in dir so the rename stays on one filesystem.
func publishSummary(dir string, payload []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create messaging evidence run directory: %w", err)
	}
	file, err := createEvidenceTemp(dir, summaryTempGlob)
	if err != nil {
		return "", fmt.Errorf("stage messaging evidence: %w", err)
	}
	staging := file.Name()
	abort := func() {
		_ = file.Close()
		_ = os.Remove(staging)
	}
	if _, err := file.Write(payload); err != nil {
		abort()
		return "", fmt.Errorf("write messaging evidence: %w", err)
	}
	if err := file.Chmod(0o644); err != nil {
		abort()
		return "", fmt.Errorf("set messaging evidence permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(staging)
		return "", fmt.Errorf("close messaging evidence: %w", err)
	}
	target := filepath.Join(dir, summaryFileName)
	if err := renameEvidenceFile(staging, target); err != nil {
		_ = os.Remove(staging)
		return "", fmt.Errorf("publish messaging evidence: %w", err)
	}
	return target, nil
}
