package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	runHelperEnv      = "I02C_EVIDENCE_RUN_HELPER"
	failHelperEnv     = "I02C_EVIDENCE_FAIL_HELPER"
	initFailHelperEnv = "I02C_EVIDENCE_INIT_FAIL_HELPER"
	initFailMarkerEnv = "I02C_EVIDENCE_INIT_FAIL_MARKER"
	runRootMarker     = "evidence-run-root="
)

// TestMain initializes this process's evidence run directory before any test
// runs so image records and the summary share it, then publishes the required
// summary. A run whose summary cannot be published must fail, never pass with
// only a logged warning.
//
// A plain `go test ./...` must also work in a read-only checkout, so the unit
// package never inherits the module-local .harness parent: it points the
// evidence parent at a scratch directory outside the module and removes it
// afterwards. An explicitly supplied parent (a test child, a collector) is
// authoritative. The initialization-failure helper bypasses this setup because
// it drives RunMain with a deliberately unusable parent itself.
func TestMain(m *testing.M) {
	if os.Getenv(initFailHelperEnv) == "1" {
		os.Exit(m.Run())
	}
	scratch := ""
	if os.Getenv(EvidenceDirEnv) == "" {
		parent, err := os.MkdirTemp("", "kodama-i02c-evidence-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "messaging evidence: create unit evidence parent: %v\n", err)
			os.Exit(1)
		}
		if err := os.Setenv(EvidenceDirEnv, parent); err != nil {
			fmt.Fprintf(os.Stderr, "messaging evidence: set %s: %v\n", EvidenceDirEnv, err)
			_ = os.RemoveAll(parent)
			os.Exit(1)
		}
		scratch = parent
	}
	os.Exit(runAndCleanup(m, scratch))
}

// runAndCleanup runs the tests through RunMain but keeps m.Run's own result
// authoritative: this package's tests assert RunMain's exit-code contract, so
// letting RunMain decide the process exit would let a masking defect turn a
// red suite green. The scratch parent is removed on every exit path, including
// a panic escaping m.Run.
func runAndCleanup(m *testing.M, scratch string) (code int) {
	if scratch != "" {
		defer func() { _ = os.RemoveAll(scratch) }()
	}
	called := false
	ran := 0
	code = RunMain(func() int {
		called = true
		ran = m.Run()
		return ran
	})
	// A failing test run must never be reported as success, even when RunMain
	// drops or rewrites its wrapped exit code. A run that never called the
	// wrapped function and still claims success is equally a masked failure.
	switch {
	case !called:
		if code == 0 {
			code = 1
		}
	case ran != 0 && code == 0:
		code = ran
	}
	return code
}

// TestMessagingEvidenceRootsAreRunUnique proves that each process creates its
// own run directory under the supplied parent (KODAMA_EVIDENCE_DIR) and keeps a
// complete summary. Two concurrent child processes share one parent.
func TestMessagingEvidenceRootsAreRunUnique(t *testing.T) {
	if os.Getenv(runHelperEnv) == "1" {
		TrackTest(t)
		root := EvidenceDir()
		if root == "" {
			t.Fatal("evidence run directory is not initialized")
		}
		if exported := os.Getenv(EvidenceDirEnv); exported != root {
			t.Fatalf("exported %s = %q; want the run directory %q so child image records land beside the summary", EvidenceDirEnv, exported, root)
		}
		fmt.Printf("%s%s\n", runRootMarker, root)
		return
	}

	parent := t.TempDir()
	exe := testBinary(t)

	command := func() (*exec.Cmd, *strings.Builder) {
		cmd := exec.Command(exe,
			"-test.run=^TestMessagingEvidenceRootsAreRunUnique$",
			"-test.count=1",
			"-test.timeout=120s",
		)
		cmd.Env = childEnv(map[string]string{
			runHelperEnv:   "1",
			EvidenceDirEnv: parent,
		})
		out := &strings.Builder{}
		cmd.Stdout = out
		cmd.Stderr = out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start evidence twin: %v", err)
		}
		return cmd, out
	}

	// Both processes are started before either is awaited so they share the
	// parent concurrently. No sleep is used as a synchronization barrier.
	first, firstOut := command()
	second, secondOut := command()
	firstErr := first.Wait()
	secondErr := second.Wait()

	firstRoot := parseRunRoot(t, firstOut.String(), firstErr)
	secondRoot := parseRunRoot(t, secondOut.String(), secondErr)

	if firstRoot == secondRoot {
		t.Fatalf("twin evidence roots are not run-unique: both = %q", firstRoot)
	}
	for name, root := range map[string]string{"first": firstRoot, "second": secondRoot} {
		if got := filepath.Dir(root); got != parent {
			t.Fatalf("%s evidence root %q is not a direct child of the supplied parent %q", name, root, parent)
		}
		if base := filepath.Base(root); !strings.HasPrefix(base, runDirPrefix) {
			t.Fatalf("%s evidence run directory %q does not use the %q prefix", name, base, runDirPrefix)
		}
		summary := readSummary(t, filepath.Join(root, summaryFileName))
		if summary.Card != "I02c" {
			t.Fatalf("%s summary card = %q; want I02c", name, summary.Card)
		}
		if summary.EvidenceDir != root {
			t.Fatalf("%s summary evidence_dir = %q; want its own run directory %q", name, summary.EvidenceDir, root)
		}
		if len(summary.Groups) != 1 {
			t.Fatalf("%s summary groups = %+v; want exactly the helper test group", name, summary.Groups)
		}
		if summary.Groups[0].Name != "TestMessagingEvidenceRootsAreRunUnique" {
			t.Fatalf("%s summary group name = %q", name, summary.Groups[0].Name)
		}
		if summary.Groups[0].Status != "pass" {
			t.Fatalf("%s summary group status = %q; want pass", name, summary.Groups[0].Status)
		}
	}

	// The supplied parent is a directory of runs, never the run directory
	// itself, so no summary is published directly under it.
	if _, err := os.Stat(filepath.Join(parent, summaryFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("summary published directly under the evidence parent: %v", err)
	}
}

// TestMessagingSummaryAtomicPublication proves a summary is published by
// staging a complete document in a unique temporary file in the same directory
// and renaming it into place, never by overwriting the target in place.
func TestMessagingSummaryAtomicPublication(t *testing.T) {
	dir := t.TempDir()
	first := []byte("{\n  \"card\": \"first\"\n}\n")
	second := []byte("{\n  \"card\": \"second\"\n}\n")

	originalCreate, originalRename := createEvidenceTemp, renameEvidenceFile
	t.Cleanup(func() {
		createEvidenceTemp, renameEvidenceFile = originalCreate, originalRename
	})

	var mu sync.Mutex
	var temporary []string
	var staged [][]byte
	var renames [][2]string

	createEvidenceTemp = func(dir, pattern string) (stagedFile, error) {
		file, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		temporary = append(temporary, file.Name())
		mu.Unlock()
		return file, nil
	}
	renameEvidenceFile = func(from, to string) error {
		payload, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		mu.Lock()
		staged = append(staged, payload)
		renames = append(renames, [2]string{from, to})
		mu.Unlock()
		return os.Rename(from, to)
	}

	pathA, err := publishSummary(dir, first)
	if err != nil {
		t.Fatalf("publish first summary: %v", err)
	}
	pathB, err := publishSummary(dir, second)
	if err != nil {
		t.Fatalf("publish second summary: %v", err)
	}

	wantPath := filepath.Join(dir, summaryFileName)
	if pathA != wantPath || pathB != wantPath {
		t.Fatalf("published paths = %q, %q; want %q", pathA, pathB, wantPath)
	}
	if len(temporary) < 2 {
		t.Fatalf("publication staged %d temporary files; want at least 2", len(temporary))
	}
	seen := map[string]bool{}
	for i, temp := range temporary {
		if seen[temp] {
			t.Fatalf("temporary file %q was reused; publication must use a unique temporary file", temp)
		}
		seen[temp] = true
		if base := filepath.Base(temp); base == summaryFileName {
			t.Fatalf("temporary file %q overwrites the summary target", base)
		}
		if !strings.HasPrefix(filepath.Base(temp), "messaging-results-") ||
			!strings.HasSuffix(filepath.Base(temp), ".json.tmp") {
			t.Fatalf("temporary file %q does not use the summary staging pattern", temp)
		}
		if filepath.Dir(temp) != dir && i < 2 {
			t.Fatalf("temporary file %q is not in the summary directory %q", temp, dir)
		}
	}
	published := [][]byte{first, second}
	for i := range 2 {
		if renames[i][0] != temporary[i] {
			t.Fatalf("rename %d source = %q; want the staged temporary file %q", i, renames[i][0], temporary[i])
		}
		if renames[i][1] != wantPath {
			t.Fatalf("rename %d target = %q; want %q", i, renames[i][1], wantPath)
		}
		if string(staged[i]) != string(published[i]) {
			t.Fatalf("rename %d staged incomplete payload %q; want %q", i, staged[i], published[i])
		}
	}
	final, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read published summary: %v", err)
	}
	if string(final) != string(second) {
		t.Fatalf("published summary = %q; want the last complete document %q", final, second)
	}

	// The package's real summary path uses the same atomic publication and
	// leaves no staging file behind.
	path, err := WriteSummary(0)
	if err != nil {
		t.Fatalf("write summary: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written summary: %v", err)
	}
	var summary Summary
	if err := json.Unmarshal(payload, &summary); err != nil {
		t.Fatalf("written summary is not a complete JSON document: %v", err)
	}
	if summary.Card != "I02c" || summary.EvidenceDir != EvidenceDir() {
		t.Fatalf("written summary = %+v; want card I02c and its run directory", summary)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, summaryTempGlob))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("publication left staging files behind: %v", leftovers)
	}
}

// TestMessagingSummaryPreservesSkip proves a skipped group is recorded as
// skipped, never folded into a passing result.
func TestMessagingSummaryPreservesSkip(t *testing.T) {
	const skipped = "skipped-group"
	t.Run(skipped, func(t *testing.T) {
		TrackTest(t)
		t.Skip("evidence status probe")
	})

	path, err := WriteSummary(0)
	if err != nil {
		t.Fatalf("write summary: %v", err)
	}
	summary := readSummary(t, path)
	name := "TestMessagingSummaryPreservesSkip/" + skipped
	var found *GroupResult
	for i := range summary.Groups {
		if summary.Groups[i].Name == name {
			found = &summary.Groups[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("summary does not contain the skipped group %q: %+v", name, summary.Groups)
	}
	if found.Status != "skip" {
		t.Fatalf("skipped group status = %q; want skip (a skipped result must never be recorded as passed)", found.Status)
	}
}

// TestRequiredMessagingEvidenceFailureFailsRun proves a run fails when its
// required summary cannot be published. The child process breaks its own run
// directory after running normally; publishing the summary must then produce a
// failing exit code instead of a logged warning over a passing run.
func TestRequiredMessagingEvidenceFailureFailsRun(t *testing.T) {
	if os.Getenv(failHelperEnv) == "1" {
		dir := EvidenceDir()
		if dir == "" {
			t.Fatal("evidence run directory is not initialized")
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove run directory: %v", err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("replace run directory with a regular file: %v", err)
		}
		return
	}

	parent := t.TempDir()
	cmd := exec.Command(testBinary(t),
		"-test.run=^TestRequiredMessagingEvidenceFailureFailsRun$",
		"-test.count=1",
		"-test.timeout=120s",
	)
	cmd.Env = childEnv(map[string]string{
		failHelperEnv:  "1",
		EvidenceDirEnv: parent,
	})
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run with an unpublished required summary must fail; err = %v\n%s", err, out)
	}
	if exit.ExitCode() != 1 {
		t.Fatalf("failed evidence publication exit code = %d; want 1\n%s", exit.ExitCode(), out)
	}
	if !strings.Contains(string(out), "messaging evidence") {
		t.Fatalf("failed evidence publication did not report the evidence failure:\n%s", out)
	}
	summaries, err := filepath.Glob(filepath.Join(parent, "*", summaryFileName))
	if err != nil {
		t.Fatalf("glob published summaries: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("failed publication left a summary behind: %v", summaries)
	}
}

// TestRunMainPreservesWrappedExitCode proves RunMain reports the wrapped run
// function's exit code after publishing the summary; a failing run is never
// masked as a successful one.
func TestRunMainPreservesWrappedExitCode(t *testing.T) {
	called := false
	got := RunMain(func() int {
		called = true
		return 3
	})
	if !called {
		t.Fatal("RunMain did not call the wrapped run function")
	}
	if got != 3 {
		t.Fatalf("RunMain(->3) = %d; want the wrapped run's exit code 3", got)
	}
}

// TestFinalizePreservesExitCode proves a successful publication returns the
// run's own exit code (a failed run stays failed) and a passing run stays 0.
func TestFinalizePreservesExitCode(t *testing.T) {
	for _, code := range []int{0, 3} {
		if got := Finalize(code); got != code {
			t.Fatalf("Finalize(%d) = %d; want the run's own exit code", code, got)
		}
	}
}

// TestRunMainFailsWhenEvidenceInitFails proves required evidence is a
// precondition: when the run directory cannot be created RunMain exits 1,
// reports the evidence failure and never calls the wrapped run function, so a
// run without its required evidence cannot report success.
func TestRunMainFailsWhenEvidenceInitFails(t *testing.T) {
	if os.Getenv(initFailHelperEnv) == "1" {
		marker := os.Getenv(initFailMarkerEnv)
		if marker == "" {
			fmt.Fprintln(os.Stderr, "messaging evidence: helper marker path is empty")
			os.Exit(2)
		}
		os.Exit(RunMain(func() int {
			if err := os.WriteFile(marker, []byte("run"), 0o644); err != nil {
				return 2
			}
			return 0
		}))
	}

	base := t.TempDir()
	blocker := filepath.Join(base, "regular-file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("create regular-file blocker: %v", err)
	}
	// The parent is below a regular file, so creating the evidence parent and
	// run directory fails with ENOTDIR.
	parent := filepath.Join(blocker, "evidence")
	marker := filepath.Join(base, "run-called")

	cmd := exec.Command(testBinary(t),
		"-test.run=^TestRunMainFailsWhenEvidenceInitFails$",
		"-test.count=1",
		"-test.timeout=120s",
	)
	cmd.Env = childEnv(map[string]string{
		initFailHelperEnv: "1",
		EvidenceDirEnv:    parent,
		initFailMarkerEnv: marker,
	})
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run with an unusable evidence parent must fail; err = %v\n%s", err, out)
	}
	if exit.ExitCode() != 1 {
		t.Fatalf("failed evidence initialization exit code = %d; want 1\n%s", exit.ExitCode(), out)
	}
	if !strings.Contains(string(out), "messaging evidence") {
		t.Fatalf("failed evidence initialization did not report the evidence failure:\n%s", out)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("wrapped run function ran despite failed evidence initialization: %v", statErr)
	}
}

// TestStatusOfFailureWinsOverSkip proves the status precedence: a failure wins
// over a skip, and a skip is never folded into a pass.
func TestStatusOfFailureWinsOverSkip(t *testing.T) {
	cases := []struct {
		name    string
		failed  bool
		skipped bool
		want    string
	}{
		{"failed and skipped", true, true, "fail"},
		{"failed", true, false, "fail"},
		{"skipped", false, true, "skip"},
		{"passed", false, false, "pass"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusOf(tc.failed, tc.skipped); got != tc.want {
				t.Fatalf("statusOf(failed=%t, skipped=%t) = %q; want %q", tc.failed, tc.skipped, got, tc.want)
			}
		})
	}
}

// TestPublishSummaryRemovesStagingOnError proves every publication error path
// removes the staged temporary document and leaves no summary target behind.
func TestPublishSummaryRemovesStagingOnError(t *testing.T) {
	payload := []byte("{\n  \"card\": \"I02c\"\n}\n")
	boom := errors.New("injected evidence failure")

	faults := []struct {
		name  string
		fault func(*faultyStagedFile)
		want  string
	}{
		{"write", func(f *faultyStagedFile) { f.writeErr = boom }, "write messaging evidence"},
		{"chmod", func(f *faultyStagedFile) { f.chmodErr = boom }, "set messaging evidence permissions"},
		{"close", func(f *faultyStagedFile) { f.closeErr = boom }, "close messaging evidence"},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			restorePublicationSeams(t)
			createEvidenceTemp = func(dir, pattern string) (stagedFile, error) {
				file, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				staged := &faultyStagedFile{file: file}
				tc.fault(staged)
				return staged, nil
			}

			path, err := publishSummary(dir, payload)
			if err == nil {
				t.Fatalf("publishSummary with a %s fault returned no error and path %q", tc.name, path)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("publishSummary %s fault error = %v; want it to mention %q", tc.name, err, tc.want)
			}
			assertNoStaging(t, dir)
		})
	}

	t.Run("rename", func(t *testing.T) {
		dir := t.TempDir()
		restorePublicationSeams(t)
		renameEvidenceFile = func(from, to string) error { return boom }

		path, err := publishSummary(dir, payload)
		if err == nil {
			t.Fatalf("publishSummary with a rename fault returned no error and path %q", path)
		}
		if !strings.Contains(err.Error(), "publish messaging evidence") {
			t.Fatalf("publishSummary rename fault error = %v; want it to mention the publish failure", err)
		}
		assertNoStaging(t, dir)
		if _, statErr := os.Stat(filepath.Join(dir, summaryFileName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed rename left a summary target behind: %v", statErr)
		}
	})
}

// TestMessagingEvidenceRunDirectoryMode proves the per-process run directory is
// 0755 (not os.MkdirTemp's 0700), so a collector running as another user can
// read the summary and harness image records.
func TestMessagingEvidenceRunDirectoryMode(t *testing.T) {
	parent := t.TempDir()
	root, err := createRunDir(parent)
	if err != nil {
		t.Fatalf("create run directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(root) }()

	for label, dir := range map[string]string{
		"created":     root,
		"initialized": EvidenceDir(),
	} {
		if dir == "" {
			t.Fatalf("%s evidence run directory is empty", label)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s run directory %q: %v", label, dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Fatalf("%s run directory %q mode = %04o; want 0755 so a collector running as another user can read it", label, dir, perm)
		}
	}
}

// faultyStagedFile wraps a real staging file and injects one publication
// failure while keeping Name and the untouched operations real.
type faultyStagedFile struct {
	file     *os.File
	writeErr error
	chmodErr error
	closeErr error
}

func (f *faultyStagedFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.file.Write(p)
}

func (f *faultyStagedFile) Name() string { return f.file.Name() }

func (f *faultyStagedFile) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.file.Chmod(mode)
}

func (f *faultyStagedFile) Close() error {
	err := f.file.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func restorePublicationSeams(t *testing.T) {
	t.Helper()
	originalCreate, originalRename := createEvidenceTemp, renameEvidenceFile
	t.Cleanup(func() {
		createEvidenceTemp, renameEvidenceFile = originalCreate, originalRename
	})
}

func assertNoStaging(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, summaryTempGlob))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("publication error path left staging files behind: %v", leftovers)
	}
}

func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	return exe
}

func childEnv(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func parseRunRoot(t *testing.T, output string, runErr error) string {
	t.Helper()
	if runErr != nil {
		t.Fatalf("evidence twin failed: %v\n%s", runErr, output)
	}
	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), runRootMarker); ok {
			if value == "" {
				t.Fatalf("evidence twin printed an empty run root:\n%s", output)
			}
			return value
		}
	}
	t.Fatalf("evidence twin did not print its run root:\n%s", output)
	return ""
}

func readSummary(t *testing.T, path string) Summary {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read summary %q: %v", path, err)
	}
	var summary Summary
	if err := json.Unmarshal(payload, &summary); err != nil {
		t.Fatalf("decode summary %q: %v\n%s", path, err, payload)
	}
	return summary
}
