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

// ProvisionConfig is the complete `kodama git provision-tenant` input. The app
// boundary builds the operating-system dependencies around it.
type ProvisionConfig struct {
	// TenantID is the tenant to provision.
	TenantID id.TenantID
	// StorageRoot is the tenant storage root.
	StorageRoot string
	// JournalDirectory is the receive-journal directory.
	JournalDirectory string
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
	// Step is the optional failure-injection seam.
	Step native.StepHook
}

// ProvisionResult reports one provisioning attempt.
type ProvisionResult struct {
	// RepositoryPath is the canonical admin repository path.
	RepositoryPath string
	// Created is true only when this attempt published a new repository.
	Created bool
}

// ProvisionTenant creates the tenant's trusted admin repository, or validates
// and preserves the existing one.
//
// It builds the OS dependencies the native boundary needs: an isolated native
// Git executor and the hook binary path. It starts no server, installs no
// listener and writes no journal entry: the locally seeded commit is not a
// remote push and needs no verification identity or git.ref.updated event.
func ProvisionTenant(ctx context.Context, config ProvisionConfig) (ProvisionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ProvisionResult{}, err
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return ProvisionResult{}, err
	}
	if strings.TrimSpace(config.StorageRoot) == "" {
		return ProvisionResult{}, fmt.Errorf("git provision-tenant storage root is empty")
	}
	if strings.TrimSpace(config.JournalDirectory) == "" {
		return ProvisionResult{}, fmt.Errorf("git provision-tenant journal directory is empty")
	}
	storageRoot, err := filepath.Abs(config.StorageRoot)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("resolve git provision-tenant storage root: %w", err)
	}
	journalDirectory, err := filepath.Abs(config.JournalDirectory)
	if err != nil {
		return ProvisionResult{}, fmt.Errorf("resolve git provision-tenant journal directory: %w", err)
	}
	tenantDirectory := filepath.Join(filepath.Clean(storageRoot), string(config.TenantID))
	repositoryPath := filepath.Join(tenantDirectory, native.AdminRepository)
	// The journal must never be created below the admin repository: that makes
	// the journal preparation create admin/ before the publish rename, which
	// then fails and leaves a non-repository admin destination behind.
	if err := requireJournalOutsidePaths(journalDirectory, map[string]string{native.AdminRepository: repositoryPath}); err != nil {
		return ProvisionResult{}, err
	}

	executor := config.Executor
	if executor == nil {
		executor, err = NewExecutor(ExecutorConfig{
			GitPath: config.GitPath,
			Env:     provisionGitEnvironment(config.Environment),
		})
		if err != nil {
			return ProvisionResult{}, fmt.Errorf("git provision-tenant executor: %w", err)
		}
	}
	hookBinary := strings.TrimSpace(config.HookBinary)
	if hookBinary == "" {
		hookBinary, err = os.Executable()
		if err != nil {
			return ProvisionResult{}, fmt.Errorf("resolve kodama hook binary: %w", err)
		}
	}

	result, err := native.ProvisionAdminRepository(ctx, native.AdminProvisionConfig{
		TenantID:         config.TenantID,
		RepositoryPath:   repositoryPath,
		TenantDirectory:  tenantDirectory,
		JournalDirectory: journalDirectory,
		HookBinaryPath:   hookBinary,
		Executor:         executor,
		Step:             config.Step,
	})
	if err != nil {
		// The native boundary reports Created=true when this attempt published
		// the repository but a later step failed. Preserve that decision: the
		// repository exists because of this invocation, and reporting false
		// would invite a caller to treat it as foreign state.
		return ProvisionResult{
			RepositoryPath: result.RepositoryPath,
			Created:        result.Created,
		}, err
	}
	return ProvisionResult{RepositoryPath: result.RepositoryPath, Created: result.Created}, nil
}

// provisionGitEnvironment derives the isolated child environment for
// provisioning. Every inherited GIT_* variable is dropped so a hostile global,
// system, template, hooks-path, index, object-store or identity setting cannot
// reach the provisioning commands. Global and system configuration are pinned
// to /dev/null and no template is inherited; the native boundary supplies its
// own empty template explicitly.
func provisionGitEnvironment(inherited []string) []string {
	if len(inherited) == 0 {
		inherited = MinimalEnvironment()
	}
	values := make(map[string]string, len(inherited)+8)
	order := make([]string, 0, len(inherited)+8)
	set := func(name, value string) {
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range inherited {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.HasPrefix(name, "GIT_") {
			continue
		}
		set(name, value)
	}
	set("LC_ALL", "C")
	set("GIT_TERMINAL_PROMPT", "0")
	set("GIT_CONFIG_NOSYSTEM", "1")
	set("GIT_CONFIG_GLOBAL", os.DevNull)
	set("GIT_CONFIG_SYSTEM", os.DevNull)
	set("GIT_ATTR_NOSYSTEM", "1")
	environment := make([]string, 0, len(order))
	for _, name := range order {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}
