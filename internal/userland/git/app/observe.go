package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// ObserverConfig configures the native ref observer.
type ObserverConfig struct {
	TenantID id.TenantID
	Storage  git.Storage
	Executor git.CommandExecutor
}

// Observer implements git.RefObserver with `git for-each-ref`. A missing ref is
// reported as an empty commit, never as an error; an invalid repository name or
// a failed command is an error.
type Observer struct {
	tenant   id.TenantID
	storage  git.Storage
	executor git.CommandExecutor
}

var _ git.RefObserver = (*Observer)(nil)

// NewObserver validates the boundaries and returns the observer.
func NewObserver(config ObserverConfig) (*Observer, error) {
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return nil, fmt.Errorf("ref observer tenant: %w", err)
	}
	if config.Storage == nil {
		return nil, fmt.Errorf("ref observer storage is missing")
	}
	if config.Executor == nil {
		return nil, fmt.Errorf("ref observer executor is missing")
	}
	return &Observer{tenant: config.TenantID, storage: config.Storage, executor: config.Executor}, nil
}

// Observe returns the current commit of one repository ref.
func (observer *Observer) Observe(ctx context.Context, repository, ref string) (git.RefState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return git.RefState{}, err
	}
	path, err := observer.storage.RepositoryPath(observer.tenant, repository)
	if err != nil {
		return git.RefState{}, err
	}
	result, err := observer.executor.Run(ctx, git.Command{
		Name: "git",
		Args: []string{"-C", path, "for-each-ref", "--format=%(objectname)", ref},
	})
	if err != nil {
		return git.RefState{}, err
	}
	if result.ExitCode != 0 {
		return git.RefState{}, fmt.Errorf("for-each-ref exited %d", result.ExitCode)
	}
	commit := strings.TrimSpace(string(result.Stdout))
	return git.RefState{Ref: ref, Commit: commit}, nil
}
