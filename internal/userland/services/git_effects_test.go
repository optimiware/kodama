package services_test

// The concrete Git merge effects.
//
// The effects fetch the exact base and candidate, choose a fast-forward or a
// rebase, push the result to the assignment ref with the expected base as the
// lease, and observe only what the remote repository reports. The executor is
// injected: these unit tests script the native Git exchanges, while the real
// services-binary test runs the same effects against a scratch repository.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/userland/services"
)

var (
	gitTestBase      = strings.Repeat("1", 40)
	gitTestCandidate = strings.Repeat("2", 40)
	gitTestRebased   = strings.Repeat("3", 40)
)

// scriptedGitExecutor is an injected native Git boundary. The handler decides
// each exchange and records the exact command for the assertions.
type scriptedGitExecutor struct {
	mu     sync.Mutex
	calls  []services.GitCommand
	handle func(services.GitCommand) (services.GitCommandResult, error)
	remote string
}

func (executor *scriptedGitExecutor) Run(_ context.Context, command services.GitCommand) (services.GitCommandResult, error) {
	executor.mu.Lock()
	executor.calls = append(executor.calls, command)
	handler := executor.handle
	executor.mu.Unlock()
	if handler != nil {
		return handler(command)
	}
	return services.GitCommandResult{}, nil
}

func (executor *scriptedGitExecutor) called(prefix ...string) bool {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	for _, call := range executor.calls {
		if len(call.Args) < len(prefix) {
			continue
		}
		matches := true
		for index, value := range prefix {
			if call.Args[index] != value {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func okGit() (services.GitCommandResult, error) {
	return services.GitCommandResult{ExitCode: 0}, nil
}

func gitOutput(value string) (services.GitCommandResult, error) {
	return services.GitCommandResult{ExitCode: 0, Stdout: []byte(value + "\n")}, nil
}

// newScriptedMergeEffects wires one effects instance over the scripted
// executor and a scratch state directory.
func newScriptedMergeEffects(t *testing.T, executor services.GitCommandExecutor) *services.GitMergeEffects {
	t.Helper()
	effects, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
		RemoteBaseURL:  "file:///srv/git",
		StateDirectory: t.TempDir(),
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewGitMergeEffects: %v", err)
	}
	return effects
}

func gitTestOperation() services.MergeOperation {
	return services.MergeOperation{
		Key:         services.MergeKey{Repository: "shop/web", Ref: "refs/heads/main"},
		OperationID: "merge-op-1",
		Base:        gitTestBase,
		Candidate:   gitTestCandidate,
	}
}

// TestGitMergeEffectsFastForwardPushesExpectedBase proves the fast-forward path:
// the fetched candidate is the result, the push leases the exact expected base,
// and the applied observation reports only the fetched revisions and result.
func TestGitMergeEffectsFastForwardPushesExpectedBase(t *testing.T) {
	executor := &scriptedGitExecutor{remote: gitTestBase}
	executor.handle = func(command services.GitCommand) (services.GitCommandResult, error) {
		switch command.Args[0] {
		case "init", "remote", "fetch", "checkout":
			return okGit()
		case "rev-parse":
			switch {
			case strings.HasPrefix(command.Args[len(command.Args)-1], gitTestBase):
				return gitOutput(gitTestBase)
			case strings.HasPrefix(command.Args[len(command.Args)-1], gitTestCandidate):
				return gitOutput(gitTestCandidate)
			}
		case "merge-base":
			if command.Args[2] == gitTestCandidate && command.Args[3] == gitTestBase {
				return services.GitCommandResult{ExitCode: 1}, nil
			}
			return okGit()
		case "push":
			executor.mu.Lock()
			executor.remote = gitTestCandidate
			executor.mu.Unlock()
			return okGit()
		case "ls-remote":
			executor.mu.Lock()
			remote := executor.remote
			executor.mu.Unlock()
			return gitOutput(remote + "\trefs/heads/main")
		}
		t.Fatalf("unexpected git command %+v", command.Args)
		return services.GitCommandResult{}, nil
	}

	effects := newScriptedMergeEffects(t, executor)
	observation, err := effects.Execute(context.Background(), gitTestOperation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if observation.State != services.EffectApplied {
		t.Fatalf("observation state = %q; want applied", observation.State)
	}
	if observation.Base != gitTestBase || observation.Candidate != gitTestCandidate || observation.Output != gitTestCandidate {
		t.Fatalf("observation = %+v; want the observed fast-forward result", observation)
	}
	if !executor.called("push", "--porcelain", "--force-with-lease=refs/heads/main:"+gitTestBase) {
		t.Fatalf("git calls = %+v; want a push leased on the expected base", executor.calls)
	}
	if executor.called("rebase") {
		t.Fatal("a fast-forward candidate was rebased")
	}

	observed, err := effects.Observe(context.Background(), gitTestOperation())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if observed.State != services.EffectApplied || observed.Output != gitTestCandidate {
		t.Fatalf("observed = %+v; want the applied fast-forward result", observed)
	}
}

// TestGitMergeEffectsRebasesDivergentCandidate proves the rebase path and that
// the observed output is the rebased commit, not the original candidate.
func TestGitMergeEffectsRebasesDivergentCandidate(t *testing.T) {
	executor := &scriptedGitExecutor{remote: gitTestBase}
	executor.handle = func(command services.GitCommand) (services.GitCommandResult, error) {
		switch command.Args[0] {
		case "init", "remote", "fetch":
			return okGit()
		case "checkout", "rebase":
			return okGit()
		case "rev-parse":
			switch command.Args[len(command.Args)-1] {
			case "HEAD":
				return gitOutput(gitTestRebased)
			case gitTestBase + "^{commit}":
				return gitOutput(gitTestBase)
			case gitTestCandidate + "^{commit}":
				return gitOutput(gitTestCandidate)
			}
		case "merge-base":
			return services.GitCommandResult{ExitCode: 1}, nil
		case "push":
			executor.mu.Lock()
			executor.remote = gitTestRebased
			executor.mu.Unlock()
			return okGit()
		case "ls-remote":
			executor.mu.Lock()
			remote := executor.remote
			executor.mu.Unlock()
			return gitOutput(remote + "\trefs/heads/main")
		}
		t.Fatalf("unexpected git command %+v", command.Args)
		return services.GitCommandResult{}, nil
	}

	effects := newScriptedMergeEffects(t, executor)
	observation, err := effects.Execute(context.Background(), gitTestOperation())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if observation.State != services.EffectApplied || observation.Output != gitTestRebased {
		t.Fatalf("observation = %+v; want the applied rebased result", observation)
	}
	if !executor.called("rebase", gitTestBase) {
		t.Fatalf("git calls = %+v; want the candidate rebased onto the base", executor.calls)
	}
	if !executor.called("push", "--porcelain", "--force-with-lease=refs/heads/main:"+gitTestBase) {
		t.Fatalf("git calls = %+v; want the rebased result pushed with the expected base", executor.calls)
	}
}

// TestGitMergeEffectsLocalFailureIsNotStarted proves a failure before the push
// mutates nothing and is reported as not_started.
func TestGitMergeEffectsLocalFailureIsNotStarted(t *testing.T) {
	executor := &scriptedGitExecutor{remote: gitTestBase}
	executor.handle = func(command services.GitCommand) (services.GitCommandResult, error) {
		switch command.Args[0] {
		case "init", "remote", "fetch":
			return okGit()
		case "rev-parse":
			if strings.HasPrefix(command.Args[len(command.Args)-1], gitTestCandidate) {
				return services.GitCommandResult{ExitCode: 128, Stderr: []byte("unknown revision")}, nil
			}
			return gitOutput(gitTestBase)
		}
		return okGit()
	}

	effects := newScriptedMergeEffects(t, executor)
	observation, err := effects.Execute(context.Background(), gitTestOperation())
	if err == nil {
		t.Fatal("Execute reported success for an unfetchable candidate")
	}
	if observation.State != services.EffectNotStarted {
		t.Fatalf("observation = %+v; want not_started", observation)
	}
	if executor.called("push") {
		t.Fatal("a local failure still pushed")
	}
}

// TestGitMergeEffectsLostPushResponseIsObserved proves a lost push response is
// classified from the remote ref, never repeated and never guessed.
func TestGitMergeEffectsLostPushResponseIsObserved(t *testing.T) {
	executor := &scriptedGitExecutor{remote: gitTestBase}
	executor.handle = func(command services.GitCommand) (services.GitCommandResult, error) {
		switch command.Args[0] {
		case "init", "remote", "fetch", "checkout":
			return okGit()
		case "rev-parse":
			if strings.HasPrefix(command.Args[len(command.Args)-1], gitTestCandidate) {
				return gitOutput(gitTestCandidate)
			}
			return gitOutput(gitTestBase)
		case "merge-base":
			if command.Args[2] == gitTestCandidate {
				return services.GitCommandResult{ExitCode: 1}, nil
			}
			return okGit()
		case "push":
			// The remote applied the push but the local process saw an error.
			executor.mu.Lock()
			executor.remote = gitTestCandidate
			executor.mu.Unlock()
			return services.GitCommandResult{ExitCode: 128, Stderr: []byte("connection reset")}, errors.New("connection reset")
		case "ls-remote":
			executor.mu.Lock()
			remote := executor.remote
			executor.mu.Unlock()
			return gitOutput(remote + "\trefs/heads/main")
		}
		t.Fatalf("unexpected git command %+v", command.Args)
		return services.GitCommandResult{}, nil
	}

	effects := newScriptedMergeEffects(t, executor)
	observation, err := effects.Execute(context.Background(), gitTestOperation())
	if err != nil {
		t.Fatalf("Execute: %v; want the observed applied result", err)
	}
	if observation.State != services.EffectApplied || observation.Output != gitTestCandidate {
		t.Fatalf("observation = %+v; want applied", observation)
	}
}

// TestNewGitMergeEffectsRefusesURLCredentials proves an embedded URL userinfo
// is refused at construction without echoing the credential anywhere.
func TestNewGitMergeEffectsRefusesURLCredentials(t *testing.T) {
	for _, remote := range []string{
		"https://user:secret@example.test/git",
		"https://token@example.test/git",
	} {
		_, err := services.NewGitMergeEffects(services.GitMergeEffectsConfig{
			RemoteBaseURL:  remote,
			StateDirectory: t.TempDir(),
			Executor:       &scriptedGitExecutor{},
		})
		if err == nil {
			t.Fatalf("NewGitMergeEffects accepted %q", remote)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), remote) {
			t.Fatalf("error %q echoes the URL credential", err)
		}
	}
}
