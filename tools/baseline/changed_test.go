// Package baseline_test proves the Makefile's `baseline` target selects the
// changed Go packages against its base revision.
package baseline_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// baselineCommands are the baseline commands that `make baseline` must print.
var baselineCommands = []string{
	"gofmt -l .",
	"go vet ./...",
	"go vet -tags=integration ./test/...",
	"go vet -tags=component ./test/...",
	"go vet -tags=e2e ./test/e2e/...",
	"go vet -tags=qualification ./test/qualification/...",
	"go test -count=1 ./...",
	"go run ./tools/check -root . imports docs",
}

type makeResult struct {
	stdout string
	stderr string
	code   int
}

// cleanEnv is the parent environment without the make and git variables that
// would change the child's behavior. `make baseline` exports command-line
// variables (BASE, CHANGED) and MAKEFLAGS to its recipes, so a test running
// inside it would otherwise inherit the outer base selection instead of the
// scratch repository's own branches.
func cleanEnv() []string {
	blocked := []string{"BASE=", "CHANGED=", "MAKEFLAGS=", "MFLAGS=", "GNUMAKEFLAGS=", "GOFLAGS="}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		skip := false
		for _, prefix := range blocked {
			if strings.HasPrefix(entry, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			env = append(env, entry)
		}
	}
	return env
}

// runMake runs the real Makefile in dir with an isolated environment. No
// recipe is executed here: every call is a dry run (-n).
func runMake(t *testing.T, dir string, args ...string) makeResult {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Fatalf("make is required: %v", err)
	}
	command := exec.Command("make", args...)
	command.Dir = dir
	command.Env = append(cleanEnv(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.test",
		"MAKEFLAGS=",
		"MFLAGS=",
		"GNUMAKEFLAGS=",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	code := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run make %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return makeResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func (r makeResult) combined() string { return r.stdout + r.stderr }

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if fileExists(filepath.Join(dir, "Makefile")) && fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find the repository root above the test directory")
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type scratchRepo struct {
	dir string
}

func (repo *scratchRepo) git(t *testing.T, args ...string) {
	t.Helper()
	base := []string{
		"-C", repo.dir,
		"-c", "user.name=Test",
		"-c", "user.email=test@example.test",
		"-c", "commit.gpgsign=false",
	}
	command := exec.Command("git", append(base, args...)...)
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func (repo *scratchRepo) commit(t *testing.T, message string) {
	t.Helper()
	repo.git(t, "add", "-A")
	repo.git(t, "commit", "-q", "-m", message)
}

// newScratchRepo builds a throwaway Go module with two branches:
//
//	master          internal/alpha
//	feature         + internal/gamma  (one commit ahead of master, checked out)
//
// The real Makefile and the real changed-package script are copied in, so the
// test exercises the shipped behavior rather than a copy of the logic.
func newScratchRepo(t *testing.T) *scratchRepo {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	copyFile(t, filepath.Join(root, "Makefile"), filepath.Join(dir, "Makefile"), 0o644)
	copyFile(t, filepath.Join(root, "tools", "baseline", "changed.sh"), filepath.Join(dir, "tools", "baseline", "changed.sh"), 0o755)
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.test/scratch\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "internal", "alpha", "alpha.go"), "package alpha\n")

	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	repo := &scratchRepo{dir: dir}
	repo.git(t, "init", "-q", "-b", "master")
	repo.commit(t, "base")

	repo.git(t, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(dir, "internal", "gamma", "gamma.go"), "package gamma\n")
	repo.commit(t, "feature")
	return repo
}

func assertBaselineCommands(t *testing.T, stdout string) {
	t.Helper()
	for _, command := range baselineCommands {
		if !strings.Contains(stdout, command) {
			t.Fatalf("make -n baseline does not print %q; output:\n%s", command, stdout)
		}
	}
}

func assertRacePackages(t *testing.T, stdout string, want, unwanted []string) {
	t.Helper()
	line := raceLine(stdout)
	if line == "" {
		t.Fatalf("make -n baseline printed no 'go test -race' line; output:\n%s", stdout)
	}
	for _, packagePath := range want {
		if !strings.Contains(line, packagePath) {
			t.Fatalf("race line %q does not cover %q", line, packagePath)
		}
	}
	for _, packagePath := range unwanted {
		if strings.Contains(line, packagePath) {
			t.Fatalf("race line %q covers unchanged package %q", line, packagePath)
		}
	}
}

func raceLine(stdout string) string {
	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "go test -race") {
			return line
		}
	}
	return ""
}

// TestMakeBaselineSelectsAMeaningfulBase covers the default base selection on
// a branch ahead of master, the loud failure for an unusable BASE, and the
// explicit CHANGED overrides.
func TestMakeBaselineSelectsAMeaningfulBase(t *testing.T) {
	t.Run("branch ahead of master lists only the changed packages", func(t *testing.T) {
		repo := newScratchRepo(t)
		result := runMake(t, repo.dir, "-n", "baseline")
		if result.code != 0 {
			t.Fatalf("make -n baseline exited %d; output:\n%s", result.code, result.combined())
		}
		assertBaselineCommands(t, result.stdout)
		assertRacePackages(t, result.stdout, []string{"./internal/gamma"}, []string{"./internal/alpha"})
		if strings.Contains(result.stdout, "skipping go test -race") {
			t.Fatalf("the race step was skipped on a branch with changes:\n%s", result.stdout)
		}
	})

	t.Run("invalid BASE is refused with a clear error", func(t *testing.T) {
		repo := newScratchRepo(t)
		result := runMake(t, repo.dir, "-n", "baseline", "BASE=no-such-revision")
		if result.code == 0 {
			t.Fatalf("make -n baseline accepted an invalid BASE; output:\n%s", result.combined())
		}
		if !strings.Contains(result.combined(), "no-such-revision") {
			t.Fatalf("the error does not name the invalid BASE; output:\n%s", result.combined())
		}
		if !strings.Contains(result.combined(), "CHANGED") {
			t.Fatalf("the error does not say how to override the base; output:\n%s", result.combined())
		}
	})

	t.Run("explicit CHANGED overrides the base selection", func(t *testing.T) {
		repo := newScratchRepo(t)
		result := runMake(t, repo.dir, "-n", "baseline", "BASE=no-such-revision", "CHANGED=./internal/alpha ./internal/gamma")
		if result.code != 0 {
			t.Fatalf("make -n baseline with explicit CHANGED exited %d; output:\n%s", result.code, result.combined())
		}
		assertBaselineCommands(t, result.stdout)
		assertRacePackages(t, result.stdout, []string{"./internal/alpha", "./internal/gamma"}, []string{"./internal/beta"})
		if strings.Contains(result.stderr, "merge-base") {
			t.Fatalf("an explicit CHANGED still consulted a base; stderr:\n%s", result.stderr)
		}
	})

	t.Run("explicit empty CHANGED skips with a note", func(t *testing.T) {
		repo := newScratchRepo(t)
		result := runMake(t, repo.dir, "-n", "baseline", "CHANGED=")
		if result.code != 0 {
			t.Fatalf("make -n baseline CHANGED= exited %d; output:\n%s", result.code, result.combined())
		}
		assertBaselineCommands(t, result.stdout)
		if !strings.Contains(result.stdout, "skipping go test -race") {
			t.Fatalf("an explicit empty CHANGED does not print the skip note; output:\n%s", result.stdout)
		}
		if strings.Contains(result.stdout, "go test -race -count=1") {
			t.Fatalf("an explicit empty CHANGED still runs go test -race; output:\n%s", result.stdout)
		}
	})
}

// TestMakeTestE2EHermeticTarget proves the additive hermetic e2e target runs
// both hermetic selections directly, without an infrastructure slot, and that
// the dry run prints exactly those commands.
func TestMakeTestE2EHermeticTarget(t *testing.T) {
	repo := newScratchRepo(t)
	result := runMake(t, repo.dir, "-n", "test-e2e-hermetic")
	if result.code != 0 {
		t.Fatalf("make -n test-e2e-hermetic exited %d; output:\n%s", result.code, result.combined())
	}
	for _, command := range []string{
		"go test -tags=e2e -race -count=1 ./test/e2e/internal/...",
		"go test -tags=e2e -race -count=1 -run '^TestE2E_WorkloadScriptsKeepTokensOutOfArgv$' ./test/e2e/",
	} {
		if !strings.Contains(result.stdout, command) {
			t.Fatalf("make -n test-e2e-hermetic does not print %q; output:\n%s", command, result.stdout)
		}
	}
	for _, forbidden := range []string{"infra-slot", "KODAMA_REQUIRE_INFRA", "make baseline"} {
		if strings.Contains(result.stdout, forbidden) {
			t.Fatalf("the hermetic e2e target prints %q:\n%s", forbidden, result.stdout)
		}
	}
}

// TestMakeUnifiedSuiteTargets proves the unified-suite targets all go through
// scripts/test-suite and pass the installation, keep-going and extra flags on.
func TestMakeUnifiedSuiteTargets(t *testing.T) {
	repo := newScratchRepo(t)
	for _, target := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"suites-parallel"}, "scripts/test-suite --target=local-all"},
		{[]string{"suites-parallel", "KEEP_GOING=1", "SUITE_FLAGS=--dry-run"}, "scripts/test-suite --keep-going --dry-run --target=local-all"},
		{[]string{"suite-podman", "INSTALLATION=/tmp/installation.json"}, "scripts/test-suite --installation=/tmp/installation.json --target=podman"},
		{[]string{"suite-kind"}, "scripts/test-suite --target=kind"},
		{[]string{"suite-stage", "SUITE_FLAGS=--skip-chaos --q03-mode=restore"}, "scripts/test-suite --skip-chaos --q03-mode=restore --target=stage"},
	} {
		result := runMake(t, repo.dir, append([]string{"-n"}, target.arguments...)...)
		if result.code != 0 {
			t.Fatalf("make -n %v exited %d:\n%s", target.arguments, result.code, result.combined())
		}
		// A recursive make adds its own directory lines around the recipe.
		if got := strings.Join(strings.Fields(result.stdout), " "); !strings.Contains(got, target.want) {
			t.Fatalf("make -n %v printed %q; want the recipe %q", target.arguments, got, target.want)
		}
	}
}
