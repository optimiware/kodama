package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

// RepositoryProvisionConfig is the complete `kodama git provision-repository`
// input. The app boundary builds the operating-system dependencies around it.
type RepositoryProvisionConfig struct {
	// TenantID is the tenant the repository belongs to. Its admin repository
	// must already be provisioned.
	TenantID id.TenantID
	// StorageRoot is the tenant storage root.
	StorageRoot string
	// Repository is the tenant-relative repository name.
	Repository string
	// ResourceID is the optional registered resource id written as `option
	// resource = <id>` when appending default rules.
	ResourceID string
	// GitPath is the native git binary. It defaults to "git".
	GitPath string
	// Environment is the exact child environment. A nil slice selects the
	// documented isolated environment.
	Environment []string
	// HookBinary is the absolute kodama binary the installed hooks exec. A
	// zero value uses the running executable.
	HookBinary string
	// Executor is an optional injected command boundary.
	Executor git.CommandExecutor
}

// RepositoryProvisionResult reports one repository provisioning attempt.
type RepositoryProvisionResult struct {
	// RepositoryPath is the canonical repository path.
	RepositoryPath string
	// Repository is the canonical tenant-relative repository name.
	Repository string
	// Created is true only when this attempt initialized the bare repository.
	Created bool
}

// ProvisionRepository initializes the tenant repository the Git service serves
// and appends its default rules to the trusted admin policy. It starts no
// server, installs no listener and writes no journal entry: the locally seeded
// policy commit is not a remote push and needs no verification identity.
func ProvisionRepository(ctx context.Context, config RepositoryProvisionConfig) (RepositoryProvisionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if strings.TrimSpace(config.StorageRoot) == "" {
		return RepositoryProvisionResult{}, fmt.Errorf("git provision-repository storage root is empty")
	}
	if strings.TrimSpace(config.Repository) == "" {
		return RepositoryProvisionResult{}, fmt.Errorf("git provision-repository repository is empty")
	}
	storageRoot, err := filepath.Abs(config.StorageRoot)
	if err != nil {
		return RepositoryProvisionResult{}, fmt.Errorf("resolve git provision-repository storage root: %w", err)
	}
	storageRoot = filepath.Clean(storageRoot)
	// LocalStorage validates the tenant-relative name exactly like tenant
	// provisioning; SafeStorage additionally refuses a path that resolves
	// outside the tenant through a symbolic link.
	localStorage, err := git.NewLocalStorage(storageRoot)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	storage, err := native.NewSafeStorage(localStorage, storageRoot, config.TenantID)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	repositoryPath, err := storage.RepositoryPath(config.TenantID, config.Repository)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	adminRepositoryPath, err := storage.RepositoryPath(config.TenantID, native.AdminRepository)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}

	executor := config.Executor
	if executor == nil {
		executor, err = NewExecutor(ExecutorConfig{
			GitPath: config.GitPath,
			Env:     provisionGitEnvironment(config.Environment),
		})
		if err != nil {
			return RepositoryProvisionResult{}, fmt.Errorf("git provision-repository executor: %w", err)
		}
	}
	hookBinary := strings.TrimSpace(config.HookBinary)
	if hookBinary == "" {
		hookBinary, err = os.Executable()
		if err != nil {
			return RepositoryProvisionResult{}, fmt.Errorf("resolve kodama hook binary: %w", err)
		}
	}
	result, err := native.ProvisionRepository(ctx, native.RepositoryProvisionConfig{
		TenantID:            config.TenantID,
		TenantDirectory:     filepath.Join(storageRoot, string(config.TenantID)),
		RepositoryPath:      repositoryPath,
		AdminRepositoryPath: adminRepositoryPath,
		ResourceID:          config.ResourceID,
		HookBinaryPath:      hookBinary,
		Executor:            executor,
	})
	if err != nil {
		return RepositoryProvisionResult{
			RepositoryPath: result.RepositoryPath,
			Repository:     result.Repository,
			Created:        result.Created,
		}, err
	}
	return RepositoryProvisionResult{
		RepositoryPath: result.RepositoryPath,
		Repository:     result.Repository,
		Created:        result.Created,
	}, nil
}
