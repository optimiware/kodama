package app

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
)

func TestObserverReadsCurrentRef(t *testing.T) {
	executor := &fakeExecutor{result: git.CommandResult{Stdout: []byte("abc123\n")}}
	observer, err := NewObserver(ObserverConfig{
		TenantID: "tenant-1",
		Storage:  fakeStorage{root: t.TempDir()},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	state, err := observer.Observe(context.Background(), "repo", "refs/heads/main")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state.Ref != "refs/heads/main" || state.Commit != "abc123" {
		t.Fatalf("state = %+v", state)
	}
	command := executor.last()
	if len(command.Args) < 4 || command.Args[2] != "for-each-ref" {
		t.Fatalf("command = %+v", command)
	}
}

func TestObserverReportsMissingRefAsEmpty(t *testing.T) {
	executor := &fakeExecutor{result: git.CommandResult{}}
	observer, err := NewObserver(ObserverConfig{
		TenantID: "tenant-1",
		Storage:  fakeStorage{root: t.TempDir()},
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	state, err := observer.Observe(context.Background(), "repo", "refs/heads/missing")
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if state.Commit != "" {
		t.Fatalf("missing ref commit = %q; want empty", state.Commit)
	}
}

func TestObserverRefusesInvalidRepository(t *testing.T) {
	observer, err := NewObserver(ObserverConfig{
		TenantID: "tenant-1",
		Storage:  fakeStorage{},
		Executor: &fakeExecutor{},
	})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	if _, err := observer.Observe(context.Background(), "", "refs/heads/main"); err == nil {
		t.Fatal("Observe accepted an empty repository path")
	}
}
