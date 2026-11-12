package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/userland/git"
)

// provisionVersionExecutor scripts `git --version` and refuses every other
// command so a probe that runs after an effect is observable.
type provisionVersionExecutor struct {
	mutex    sync.Mutex
	commands []git.Command
	output   string
}

func (executor *provisionVersionExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mutex.Lock()
	executor.commands = append(executor.commands, command)
	executor.mutex.Unlock()
	if len(command.Args) == 1 && command.Args[0] == "--version" {
		return git.CommandResult{Stdout: []byte(executor.output)}, nil
	}
	return git.CommandResult{}, fmt.Errorf("unexpected command %v", command.Args)
}

func (executor *provisionVersionExecutor) recorded() []git.Command {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	return append([]git.Command(nil), executor.commands...)
}

// TestProvisionAdminRepositoryProbesVersionBeforeEffect pins the native
// boundary: an unsupported executor is refused before any directory or staging
// path exists, and the probe is the only command run.
func TestProvisionAdminRepositoryProbesVersionBeforeEffect(t *testing.T) {
	root := t.TempDir()
	executor := &provisionVersionExecutor{output: "git version 2.43.0\n"}
	tenantDirectory := filepath.Join(root, "tenant")
	config := AdminProvisionConfig{
		TenantID:         "tenant-native",
		RepositoryPath:   filepath.Join(tenantDirectory, AdminRepository),
		TenantDirectory:  tenantDirectory,
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinaryPath:   os.Args[0],
		Executor:         executor,
	}
	_, err := ProvisionAdminRepository(context.Background(), config)
	if err == nil {
		t.Fatal("ProvisionAdminRepository accepted Git 2.43.0")
	}
	if !strings.Contains(err.Error(), MinimumGitVersion) {
		t.Fatalf("error = %v; want the minimum Git version %s", err, MinimumGitVersion)
	}
	commands := executor.recorded()
	if len(commands) != 1 || strings.Join(commands[0].Args, " ") != "--version" {
		t.Fatalf("executor commands = %v; want exactly [--version] before any effect", commands)
	}
	if _, statErr := os.Stat(tenantDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("tenant directory exists after a refused probe: %v", statErr)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal directory exists after a refused probe: %v", statErr)
	}
}

// TestProvisionAdminRepositoryRefusesMissingExecutor pins that the probe cannot
// silently succeed without an executor.
func TestProvisionAdminRepositoryRefusesMissingExecutor(t *testing.T) {
	root := t.TempDir()
	_, err := ProvisionAdminRepository(context.Background(), AdminProvisionConfig{
		TenantID:         "tenant-native",
		RepositoryPath:   filepath.Join(root, "tenant", AdminRepository),
		TenantDirectory:  filepath.Join(root, "tenant"),
		JournalDirectory: filepath.Join(root, "journal"),
		HookBinaryPath:   os.Args[0],
	})
	if err == nil {
		t.Fatal("ProvisionAdminRepository accepted a missing executor")
	}
	if !strings.Contains(err.Error(), "executor") {
		t.Fatalf("error = %v; want a missing executor refusal", err)
	}
}

// TestProvisionLockIsExclusive pins that the tenant-directory advisory lock
// blocks a second holder until release.
func TestProvisionLockIsExclusive(t *testing.T) {
	directory := t.TempDir()
	release, err := acquireProvisionLock(context.Background(), directory)
	if err != nil {
		t.Fatalf("acquireProvisionLock: %v", err)
	}
	if release == nil {
		t.Fatal("acquireProvisionLock returned no release function")
	}
	acquired := make(chan struct{})
	released := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		secondRelease, err := acquireProvisionLock(ctx, directory)
		if err != nil {
			return
		}
		close(acquired)
		secondRelease()
		close(released)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock was acquired while the first was held")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	release = nil
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("second lock was not acquired after release")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("second holder did not release the lock")
	}
	if release != nil {
		release()
	}
}

// TestProvisionLockHonorsCancellation pins that a waiting acquisition observes
// context cancellation and does not steal the held lock.
func TestProvisionLockHonorsCancellation(t *testing.T) {
	directory := t.TempDir()
	release, err := acquireProvisionLock(context.Background(), directory)
	if err != nil {
		t.Fatalf("acquireProvisionLock: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := acquireProvisionLock(ctx, directory)
		result <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting acquisition error = %v; want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting acquisition ignored cancellation")
	}
}
