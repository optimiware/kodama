// Package gitkeep implements the retained-commit keeper over the Userland
// Git service's managed keep refs. It is the composition driver that turns the
// authoritative commit set from the retention store into refs under
// refs/kodama/keep/<sha> in every served repository of a tenant.
//
// The driver carries no retention window: the caller supplies the retained
// commits. It lives in the composition layer because it wires a Userland
// capability to Shell-owned retention evidence.
package gitkeep

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// ErrStorageRootUnavailable reports a configured Git storage root that does
// not exist as a directory. The keeper never creates it: a missing mount must
// stop composition startup instead of silently sweeping an empty directory.
var ErrStorageRootUnavailable = errors.New("git keep refs: storage root is not a mounted directory")

// Config is the explicit keeper wiring.
type Config struct {
	// StorageRoot is the tenant-scoped Git storage root exactly as `git serve`
	// receives it. It must already exist as a directory.
	StorageRoot string
	// Repositories optionally narrows the sweep to explicit tenant-relative
	// names. Empty enumerates every bare repository under the tenant directory
	// on each sweep, so a newly provisioned repository is picked up without a
	// restart or a static list.
	Repositories []string
	// Executor is an optional injected native boundary. A nil value builds the
	// isolated production executor.
	Executor git.CommandExecutor
}

// Keeper reconciles managed keep refs for one tenant and its served
// repositories.
type Keeper struct {
	storage      *git.LocalStorage
	store        *git.KeepRefStore
	repositories []string
}

// New validates the wiring and returns the keeper.
func New(config Config) (*Keeper, error) {
	if config.StorageRoot == "" {
		return nil, fmt.Errorf("git keep refs require a storage root")
	}
	// NewLocalStorage creates a missing root, which is exactly what a silently
	// unmounted PVC looks like. The keeper therefore proves the mount exists
	// before that boundary is constructed.
	info, err := os.Stat(config.StorageRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorageRootUnavailable, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: not a directory", ErrStorageRootUnavailable)
	}
	storage, err := git.NewLocalStorage(config.StorageRoot)
	if err != nil {
		return nil, err
	}
	executor := config.Executor
	if executor == nil {
		// The keeper reads refs and object existence only, but it must still be
		// isolated from inherited Git configuration like `git serve` is.
		executor, err = gitapp.NewExecutor(gitapp.ExecutorConfig{Env: isolatedEnvironment()})
		if err != nil {
			return nil, err
		}
	}
	store, err := git.NewKeepRefStore(storage, executor)
	if err != nil {
		return nil, err
	}
	repositories := append([]string(nil), config.Repositories...)
	return &Keeper{storage: storage, store: store, repositories: repositories}, nil
}

// KeepRefs reconciles every served repository of the tenant with the
// authoritative retained set. An empty configured list enumerates the tenant's
// bare repositories from storage. A repository that disappears between the
// enumeration and the sweep is a no-op, never an error. One repository's
// failure is reported after the others are attempted, so a single damaged
// repository cannot strand another's refs; the caller retries the whole tenant
// on the next sweep.
func (keeper *Keeper) KeepRefs(ctx context.Context, tenant string, retained []string) error {
	typed, err := id.ParseTenantID(tenant)
	if err != nil {
		return err
	}
	repositories := keeper.repositories
	if len(repositories) == 0 {
		repositories, err = keeper.storage.Repositories(typed)
		if err != nil {
			return err
		}
	}
	var failures []error
	for _, repository := range repositories {
		path, err := keeper.storage.RepositoryPath(typed, repository)
		if err != nil {
			failures = append(failures, fmt.Errorf("repository %s: %w", repository, err))
			continue
		}
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				// The repository was removed after enumeration. Its keep refs
				// are gone with it; the next sweep observes the new layout.
				continue
			}
			failures = append(failures, fmt.Errorf("repository %s: %w", repository, err))
			continue
		}
		if _, err := keeper.store.KeepRefs(ctx, typed, repository, retained); err != nil {
			failures = append(failures, fmt.Errorf("repository %s: %w", repository, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

// isolatedEnvironment drops inherited Git configuration and keeps only the
// executable search path. Retained refs must be written with the same
// configuration isolation the served process uses.
func isolatedEnvironment() []string {
	environment := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if path := os.Getenv("PATH"); path != "" {
		environment = append(environment, "PATH="+path)
	}
	return environment
}
