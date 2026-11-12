//go:build unix

package native

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// provisionRealExecutor is the real native Git command boundary the umask test
// uses. It mirrors the app executor's explicit isolated environment without
// importing the app package (which imports this one).
type provisionRealExecutor struct {
	environment []string
}

func (executor provisionRealExecutor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if command.Name != "git" {
		return git.CommandResult{}, fmt.Errorf("unexpected command %q", command.Name)
	}
	child := exec.CommandContext(ctx, "git", command.Args...)
	environment := append(append([]string(nil), executor.environment...), CommandEnvironment(ctx)...)
	child.Env = environment
	var stdout, stderr bytes.Buffer
	child.Stdout = &stdout
	child.Stderr = &stderr
	err := child.Run()
	result := git.CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return git.CommandResult{}, err
}

// TestProvisionAdminRepositoryPreservesUmask pins that provisioning never
// changes the process-global umask and still produces private modes under both
// a restrictive 077 and an ordinary 022 umask. Modes are set explicitly with
// Chmod, so the caller's umask cannot widen the provisioned tree.
func TestProvisionAdminRepositoryPreservesUmask(t *testing.T) {
	for _, mask := range []int{0o077, 0o022} {
		mask := mask
		t.Run(fmt.Sprintf("umask-%03o", mask), func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatalf("create home: %v", err)
			}
			tenantDirectory := filepath.Join(root, "repos", "tenant-umask")
			journalDirectory := filepath.Join(root, "journal")
			config := AdminProvisionConfig{
				TenantID:         id.TenantID("tenant-umask"),
				RepositoryPath:   filepath.Join(tenantDirectory, AdminRepository),
				TenantDirectory:  tenantDirectory,
				JournalDirectory: journalDirectory,
				HookBinaryPath:   os.Args[0],
				Executor: provisionRealExecutor{environment: []string{
					"LC_ALL=C",
					"GIT_TERMINAL_PROMPT=0",
					"GIT_CONFIG_NOSYSTEM=1",
					"GIT_CONFIG_GLOBAL=" + os.DevNull,
					"GIT_CONFIG_SYSTEM=" + os.DevNull,
					"HOME=" + home,
					"PATH=" + os.Getenv("PATH"),
				}},
			}

			original := syscall.Umask(0)
			syscall.Umask(original)
			syscall.Umask(mask)
			defer syscall.Umask(original)

			result, err := ProvisionAdminRepository(context.Background(), config)
			if err != nil {
				t.Fatalf("ProvisionAdminRepository: %v", err)
			}
			if !result.Created {
				t.Fatal("Created = false; want true")
			}
			observed := syscall.Umask(0)
			syscall.Umask(observed)
			if observed != mask {
				t.Fatalf("umask after provisioning = %04o; want the unchanged %04o", observed, mask)
			}

			requirePrivateProvisionedTree(t, tenantDirectory, journalDirectory)
		})
	}
}

// requirePrivateProvisionedTree walks the tenant directory and asserts the
// provisioned modes: directories 0700, hooks 0700 and every other ordinary
// file at most 0600.
func requirePrivateProvisionedTree(t *testing.T, tenantDirectory, journalDirectory string) {
	t.Helper()
	for _, directory := range []string{tenantDirectory, journalDirectory} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatalf("stat directory %q: %v", directory, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", directory)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory %q mode = %04o; want 0700", directory, got)
		}
	}
	hooksPrefix := filepath.Join(tenantDirectory, AdminRepository, "hooks") + string(os.PathSeparator)
	err := filepath.WalkDir(tenantDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if got := info.Mode().Perm(); got != 0o700 {
				t.Errorf("directory %q mode = %04o; want 0700", path, got)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%q is not an ordinary file (mode %s)", path, info.Mode())
			return nil
		}
		permission := info.Mode().Perm()
		if strings.HasPrefix(path, hooksPrefix) {
			if permission != 0o700 {
				t.Errorf("hook %q mode = %04o; want 0700", path, permission)
			}
			return nil
		}
		if permission&^os.FileMode(0o600) != 0 {
			t.Errorf("ordinary file %q mode = %04o; want at most 0600", path, permission)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk provisioned tree: %v", err)
	}
	lockPath := filepath.Join(tenantDirectory, provisioningLockName)
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat provisioning lock: %v", err)
	}
	if !lockInfo.Mode().IsRegular() {
		t.Fatalf("provisioning lock mode = %s; want a regular file", lockInfo.Mode())
	}
	if got := lockInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("provisioning lock mode = %04o; want 0600", got)
	}
}
