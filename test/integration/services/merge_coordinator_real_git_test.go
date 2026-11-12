//go:build integration

package services

// The merge coordinator entry with the real Git merge effects.
//
// The whole path is real except the public assignment itself, which is created
// through the served factory edge exactly like the other merge scenarios:
//
//   - the `kodama services --kind merge_coordinator` binary is the real entry;
//   - the Git effects run the real native git binary against a scratch bare
//     repository and push the observed result;
//   - the durable state directory is a real file coordination store.
//
// One assigned candidate is merged. The test proves the request is answered
// with the observed result and that the remote ref actually advanced to the
// candidate.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	mergeRealServiceID  = "merge-real-svc"
	mergeRealRepository = "shop/web"
	mergeRealRef        = "refs/heads/main"
)

// mergeRealOutput is one captured child stream.
type mergeRealOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (output *mergeRealOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Write(data)
}

func (output *mergeRealOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

// mergeRealRunGit runs one native git command in a scratch directory.
func mergeRealRunGit(t *testing.T, directory string, args ...string) string {
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

// mergeRealWriteFile writes and stages one scratch file.
func mergeRealWriteFile(t *testing.T, directory, name, content string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mergeRealRunGit(t, directory, "add", name)
}

// mergeRealSeedRepository creates one bare repository with a main base and a
// descendant candidate.
func mergeRealSeedRepository(t *testing.T, root string) (base, candidate string) {
	t.Helper()
	bare := filepath.Join(root, mergeRealRepository)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mergeRealRunGit(t, filepath.Dir(bare), "init", "--bare", "--quiet", "--initial-branch=main", bare)
	seed := t.TempDir()
	mergeRealRunGit(t, seed, "init", "--quiet", "--initial-branch=main")
	mergeRealWriteFile(t, seed, "README.md", "base\n")
	mergeRealRunGit(t, seed, "commit", "--quiet", "-m", "base")
	base = mergeRealRunGit(t, seed, "rev-parse", "HEAD")
	mergeRealRunGit(t, seed, "checkout", "--quiet", "-b", "work")
	mergeRealWriteFile(t, seed, "feature.txt", "candidate\n")
	mergeRealRunGit(t, seed, "commit", "--quiet", "-m", "candidate")
	candidate = mergeRealRunGit(t, seed, "rev-parse", "HEAD")
	mergeRealRunGit(t, seed, "push", "--quiet", bare, "main", "work")
	return base, candidate
}

// mergeRealBuildBinary builds the real command binary once per test.
func mergeRealBuildBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "kodama")
	build := exec.Command("go", "build", "-o", binary, "./cmd/kodama")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kodama: %v\n%s", err, output)
	}
	return binary
}

// mergeRealApplyPipeline applies one merge request for the scratch revisions.
func mergeRealApplyPipeline(t *testing.T, harness *factory.Harness, name, base, candidate string) api.PipelineApplyResult {
	t.Helper()
	document, err := yaml.Marshal(mergePipelineDocument{Actions: []mergePipelineAction{{
		Name:    "merge",
		Request: "service/" + mergeRealServiceID,
		Subject: map[string]any{
			"repository": mergeRealRepository,
			"ref":        mergeRealRef,
			"base":       base,
			"candidate":  candidate,
		},
		Outputs: []string{"base", "candidate", "output"},
		Timeout: responderRequestTimeout,
	}}})
	if err != nil {
		t.Fatalf("marshal merge pipeline: %v", err)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// mergeRealProcess is one running `kodama services` child.
type mergeRealProcess struct {
	command *exec.Cmd
	output  *mergeRealOutput
	done    chan struct{}
	mu      sync.Mutex
	err     error
	stopped bool
}

// startMergeRealProcess starts the real services binary.
func startMergeRealProcess(t *testing.T, binary string, arguments []string) *mergeRealProcess {
	t.Helper()
	command := exec.Command(binary, arguments...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	output := &mergeRealOutput{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start merge coordinator: %v", err)
	}
	process := &mergeRealProcess{command: command, output: output, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process
}

// exited reports whether the child has exited and its wait error.
func (process *mergeRealProcess) exited() (bool, error) {
	select {
	case <-process.done:
		process.mu.Lock()
		defer process.mu.Unlock()
		return true, process.err
	default:
		return false, nil
	}
}

// stop sends SIGTERM and joins the child, failing when it does not exit cleanly.
func (process *mergeRealProcess) stop(t *testing.T) {
	t.Helper()
	process.mu.Lock()
	if process.stopped {
		process.mu.Unlock()
		return
	}
	process.mu.Unlock()
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal merge coordinator: %v", err)
	}
	select {
	case <-process.done:
		process.mu.Lock()
		process.stopped = true
		err := process.err
		process.mu.Unlock()
		if err != nil {
			t.Fatalf("merge coordinator exit: %v\n%s", err, process.output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("merge coordinator did not stop after SIGTERM\n%s", process.output.String())
	}
}

// kill removes the child without requiring a graceful exit. It is the deferred
// failure path only.
func (process *mergeRealProcess) kill() {
	process.mu.Lock()
	if process.stopped {
		process.mu.Unlock()
		return
	}
	process.stopped = true
	process.mu.Unlock()
	if process.command.Process != nil {
		_ = process.command.Process.Kill()
	}
	<-process.done
}

// mergeRealWaitTerminal waits for one execution to settle while also failing
// fast when the child service exits.
func mergeRealWaitTerminal(t *testing.T, harness *factory.Harness, executionID string, process *mergeRealProcess) api.ExecutionInspectResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if exited, err := process.exited(); exited {
			t.Fatalf("merge coordinator exited before the execution settled: %v\n%s", err, process.output.String())
		}
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code == "" {
			var inspected api.ExecutionInspectResult
			harness.Decode(result, &inspected)
			switch inspected.State {
			case "succeeded", "failed", "cancelled":
				return inspected
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("execution %s did not settle\n%s", executionID, process.output.String())
	return api.ExecutionInspectResult{}
}

// TestMergeCoordinatorRealGitMergesAssignedCandidate drives the real services
// binary with the concrete Git merge effects against a scratch repository.
func TestMergeCoordinatorRealGitMergesAssignedCandidate(t *testing.T) {
	harness := factory.Start(t)
	root := t.TempDir()
	base, candidate := mergeRealSeedRepository(t, root)

	access := createResponderService(t, harness, mergeRealServiceID)
	applied := mergeRealApplyPipeline(t, harness, "merge-real-git", base, candidate)
	started := startPipeline(t, harness, applied.PipelineID, "start-merge-real-git")
	waitRequestRunning(t, harness, started.ExecutionID, "merge")

	binary := mergeRealBuildBinary(t)
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(access.token), 0o600); err != nil {
		t.Fatalf("WriteFile credential: %v", err)
	}
	state := t.TempDir()
	arguments := []string{
		"services",
		"--kind", "merge_coordinator",
		"--service", mergeRealServiceID,
		"--tenant", factory.TenantID,
		"--project", factory.ProjectID,
		"--base-url", harness.BaseURL,
		"--credential-file", credential,
		"--state-dir", state,
		"--git-url", "file://" + filepath.ToSlash(root),
		"--rescan", "100ms",
	}
	process := startMergeRealProcess(t, binary, arguments)
	defer process.kill()

	settled := mergeRealWaitTerminal(t, harness, started.ExecutionID, process)
	if settled.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded (failure=%+v)\n%s", settled.State, settled.Failure, process.output.String())
	}
	action := actionByName(t, settled, "merge")
	if action.Decision != "pass" || action.Actor != mergeRealServiceID {
		t.Fatalf("merge answer = %+v; want the service's observed answer", action)
	}
	if action.Outputs["base"] != base || action.Outputs["candidate"] != candidate {
		t.Fatalf("merge outputs = %v; want the exact observed revisions", action.Outputs)
	}
	if action.Outputs["output"] == "" {
		t.Fatalf("merge outputs = %v; want the observed result", action.Outputs)
	}
	if _, approved := action.Outputs["approval"]; approved {
		t.Fatal("the merge answer carries an approval")
	}
	bare := filepath.Join(root, mergeRealRepository)
	remote := mergeRealRunGit(t, bare, "rev-parse", mergeRealRef)
	if remote != action.Outputs["output"] {
		t.Fatalf("remote %s = %s; want the observed result %s", mergeRealRef, remote, action.Outputs["output"])
	}
	if content := mergeRealRunGit(t, bare, "show", mergeRealRef+":feature.txt"); content != "candidate" {
		t.Fatalf("merged feature content = %q; want the candidate", content)
	}
	if got := countResolvedEvents(t, harness, started.ExecutionID); got != 1 {
		t.Fatalf("committed request.resolved events = %d; want exactly one", got)
	}
	if _, err := os.Stat(filepath.Join(state, "coordination")); err != nil {
		t.Fatalf("coordination state directory: %v", err)
	}

	process.stop(t)
	if process.output.String() != "" {
		t.Logf("merge coordinator output: %s", process.output.String())
	}
}
