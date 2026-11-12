package app_test

// The concrete Git effects against the real native Git binary and a
// scratch repository. This is the fast unit-level proof that the command
// sequence is valid; the build-tagged services test proves the same effects
// through the real `kodama services` entry and the public assignment edge.
//
// It lives beside the userland app that owns the OS Git executor: only an app
// package may compose the concrete executor with the userland effects, so the
// test drives the real binary from here instead of from the userland package.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
	servicesapp "github.com/optimiware/kodama/internal/userland/services/app"
)

// runScratchGit runs one native Git command in one scratch directory.
func runScratchGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.test",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// writeScratchFile writes one file and stages it.
func writeScratchFile(t *testing.T, directory, name, content string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runScratchGit(t, directory, "add", name)
}

// seedMergeRepository creates one bare repository with a main base and a
// descendant work candidate.
func seedMergeRepository(t *testing.T, root string) (repository, base, candidate string) {
	t.Helper()
	repository = "shop/web"
	bare := filepath.Join(root, repository)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runScratchGit(t, filepath.Dir(bare), "init", "--bare", "--quiet", "--initial-branch=main", bare)
	seed := t.TempDir()
	runScratchGit(t, seed, "init", "--quiet", "--initial-branch=main")
	writeScratchFile(t, seed, "README.md", "base\n")
	runScratchGit(t, seed, "commit", "--quiet", "-m", "base")
	base = runScratchGit(t, seed, "rev-parse", "HEAD")
	runScratchGit(t, seed, "checkout", "--quiet", "-b", "work")
	writeScratchFile(t, seed, "feature.txt", "candidate\n")
	runScratchGit(t, seed, "commit", "--quiet", "-m", "candidate")
	candidate = runScratchGit(t, seed, "rev-parse", "HEAD")
	runScratchGit(t, seed, "push", "--quiet", bare, "main", "work")
	return repository, base, candidate
}

// TestGitMergeEffectsRealFastForwardMerge proves the real native Git effect on
// a scratch repository before the composed integration run.
func TestGitMergeEffectsRealFastForwardMerge(t *testing.T) {
	root := t.TempDir()
	repository, base, candidate := seedMergeRepository(t, root)
	executor, err := servicesapp.NewOSGitExecutor(servicesapp.OSGitExecutorConfig{})
	if err != nil {
		t.Fatalf("NewOSGitExecutor: %v", err)
	}
	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  "file://" + filepath.ToSlash(root),
		StateDirectory: t.TempDir(),
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	operation := services.MergeOperation{
		Key:         services.MergeKey{Repository: repository, Ref: "refs/heads/main"},
		OperationID: "merge-real-1",
		Base:        base,
		Candidate:   candidate,
	}
	observation, err := effects.Execute(context.Background(), operation)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if observation.State != services.EffectApplied {
		t.Fatalf("observation = %+v; want applied", observation)
	}
	if observation.Base != base || observation.Candidate != candidate || observation.Output != candidate {
		t.Fatalf("observation = %+v; want the fast-forward result", observation)
	}
	bare := filepath.Join(root, repository)
	if head := runScratchGit(t, bare, "rev-parse", "refs/heads/main"); head != candidate {
		t.Fatalf("remote main = %s; want the merged candidate %s", head, candidate)
	}
	content := runScratchGit(t, bare, "show", "refs/heads/main:feature.txt")
	if content != "candidate" {
		t.Fatalf("merged feature content = %q; want candidate", content)
	}
	observed, err := effects.Observe(context.Background(), operation)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.State != services.EffectApplied || observed.Output != candidate {
		t.Fatalf("observed = %+v; want the settled fast-forward", observed)
	}
}

// TestGitMergeEffectsRealRebaseMerge proves a divergent candidate is rebased
// onto the base and the rebased commit is what the remote settles.
func TestGitMergeEffectsRealRebaseMerge(t *testing.T) {
	root := t.TempDir()
	repository, _, candidate := seedMergeRepository(t, root)
	// Advance main past the candidate's base so the candidate cannot fast
	// forward.
	bare := filepath.Join(root, repository)
	advance := t.TempDir()
	runScratchGit(t, advance, "clone", "--quiet", bare, ".")
	runScratchGit(t, advance, "checkout", "--quiet", "main")
	writeScratchFile(t, advance, "later.txt", "later\n")
	runScratchGit(t, advance, "commit", "--quiet", "-m", "later")
	newBase := runScratchGit(t, advance, "rev-parse", "HEAD")
	runScratchGit(t, advance, "push", "--quiet", "origin", "main")

	executor, err := servicesapp.NewOSGitExecutor(servicesapp.OSGitExecutorConfig{})
	if err != nil {
		t.Fatalf("NewOSGitExecutor: %v", err)
	}
	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  "file://" + filepath.ToSlash(root),
		StateDirectory: t.TempDir(),
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	operation := services.MergeOperation{
		Key:         services.MergeKey{Repository: repository, Ref: "refs/heads/main"},
		OperationID: "merge-real-2",
		Base:        newBase,
		Candidate:   candidate,
	}
	observation, err := effects.Execute(context.Background(), operation)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if observation.State != services.EffectApplied {
		t.Fatalf("observation = %+v; want applied", observation)
	}
	if observation.Base != newBase || observation.Candidate != candidate {
		t.Fatalf("observation = %+v; want the fetched revisions", observation)
	}
	if observation.Output == candidate || observation.Output == newBase {
		t.Fatalf("observation output = %s; want the rebased commit", observation.Output)
	}
	if head := runScratchGit(t, bare, "rev-parse", "refs/heads/main"); head != observation.Output {
		t.Fatalf("remote main = %s; want the rebased result %s", head, observation.Output)
	}
	if content := runScratchGit(t, bare, "show", "refs/heads/main:feature.txt"); content != "candidate" {
		t.Fatalf("merged feature content = %q; want candidate", content)
	}
}

// TestGitMergeEffectsRealStaleBaseIsNotStarted proves a remote target ref that
// advanced past the request base is refused before any effect record or push.
// Observe then proves the effect never started, so a held assignment can be
// released instead of staying uncertain forever.
func TestGitMergeEffectsRealStaleBaseIsNotStarted(t *testing.T) {
	root := t.TempDir()
	repository, base, candidate := seedMergeRepository(t, root)
	bare := filepath.Join(root, repository)
	// Advance the target ref after the request base was computed: the push
	// lease would reject the merge, but nothing must be recorded or pushed.
	advance := t.TempDir()
	runScratchGit(t, advance, "clone", "--quiet", bare, ".")
	runScratchGit(t, advance, "checkout", "--quiet", "main")
	writeScratchFile(t, advance, "later.txt", "later\n")
	runScratchGit(t, advance, "commit", "--quiet", "-m", "later")
	advanced := runScratchGit(t, advance, "rev-parse", "HEAD")
	runScratchGit(t, advance, "push", "--quiet", "origin", "main")

	executor, err := servicesapp.NewOSGitExecutor(servicesapp.OSGitExecutorConfig{})
	if err != nil {
		t.Fatalf("NewOSGitExecutor: %v", err)
	}
	stateDirectory := t.TempDir()
	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  "file://" + filepath.ToSlash(root),
		StateDirectory: stateDirectory,
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	operation := services.MergeOperation{
		Key:         services.MergeKey{Repository: repository, Ref: "refs/heads/main"},
		OperationID: "merge-real-stale-1",
		Base:        base,
		Candidate:   candidate,
	}
	observation, err := effects.Execute(context.Background(), operation)
	if err == nil {
		t.Fatal("Execute accepted a request whose remote ref advanced past its base")
	}
	if !errors.Is(err, services.ErrMergeBaseMoved) {
		t.Fatalf("Execute error = %v; want ErrMergeBaseMoved", err)
	}
	if observation.State != services.EffectNotStarted {
		t.Fatalf("observation = %+v; want not_started", observation)
	}
	if head := runScratchGit(t, bare, "rev-parse", "refs/heads/main"); head != advanced {
		t.Fatalf("remote main = %s; want the advanced commit %s (nothing pushed)", head, advanced)
	}
	if head := runScratchGit(t, bare, "rev-parse", "refs/heads/work"); head != candidate {
		t.Fatalf("remote work = %s; want the unchanged candidate %s", head, candidate)
	}
	observed, err := effects.Observe(context.Background(), operation)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.State != services.EffectNotStarted {
		t.Fatalf("observed = %+v; want not_started because no record was written", observed)
	}
	records, err := filepath.Glob(filepath.Join(stateDirectory, "*.json"))
	if err != nil {
		t.Fatalf("Glob effect records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("effect records = %v; want none before any push", records)
	}

	// The same stale request through the coordinator leaves an uncertain
	// in-flight marker; observation proves it never started, so the held
	// assignment is released rather than wedged.
	store, err := services.NewFileCoordinationStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileCoordinationStore: %v", err)
	}
	request := services.MergeRequest{
		Key:        operation.Key,
		Assignment: services.MergeAssignment{ExecutionID: "exec-stale-1", ActionID: "action-stale-1"},
		Base:       base,
		Candidate:  candidate,
	}
	coordinator, err := services.NewCoordinator(services.CoordinatorConfig{
		TenantID: id.TenantID("tenant-stale"),
		Owner:    "stale-base-test",
		Lease:    time.Minute,
		Store:    store,
		Effects:  effects,
		Clock:    servicesapp.SystemClock{},
		NewOperationID: func() (id.OperationID, error) {
			return "merge-op-stale-1", nil
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	result, err := coordinator.Merge(context.Background(), request)
	if err == nil {
		t.Fatalf("Merge settled a stale-base request as %q", result.State)
	}
	if result.State != services.EffectUncertain {
		t.Fatalf("held result = %+v; want uncertain pending release", result)
	}
	if head := runScratchGit(t, bare, "rev-parse", "refs/heads/main"); head != advanced {
		t.Fatalf("remote main = %s; want the advanced commit %s (nothing pushed)", head, advanced)
	}
	released, err := coordinator.ReleaseObserved(context.Background(), request)
	if err != nil {
		t.Fatalf("ReleaseObserved: %v", err)
	}
	if released.State != services.EffectNotStarted {
		t.Fatalf("released result = %+v; want not_started", released)
	}
	record, ok, err := store.Load(context.Background(), id.TenantID("tenant-stale"), request.Key)
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !ok || !record.Released {
		t.Fatalf("record = %+v; want the assignment released", record)
	}
}
