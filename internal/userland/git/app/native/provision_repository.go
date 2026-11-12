package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// Sentinel refusals of repository provisioning. They are exported so the app
// boundary and the command can report a stable cause without parsing text.
var (
	// ErrRepositoryProvisionDestination reports an existing repository
	// destination that is not the bare repository this command creates. It is
	// never repaired, reseeded or deleted.
	ErrRepositoryProvisionDestination = errors.New("repository destination is not provisionable")
	// ErrRepositoryProvisionContainment reports a tenant, destination or
	// staging path that escapes the bound tenant directory.
	ErrRepositoryProvisionContainment = errors.New("repository provisioning path escapes its tenant")
)

// provisioningRepositoryStagingPrefix bounds every private repository staging
// directory this command may create. A later attempt never reads or removes one
// it did not create, so a crash leftover stays private and inert.
const provisioningRepositoryStagingPrefix = ".provision-repository-"

// RepositoryProvisionConfig is the native repository-provisioning input.
type RepositoryProvisionConfig struct {
	// TenantID is the tenant the repository belongs to.
	TenantID id.TenantID
	// TenantDirectory is the tenant storage directory the admin repository
	// and every tenant repository live under.
	TenantDirectory string
	// RepositoryPath is the canonical tenant-relative repository destination.
	RepositoryPath string
	// AdminRepositoryPath is the tenant's trusted admin repository.
	AdminRepositoryPath string
	// HookBinaryPath is the absolute executable the installed hooks run.
	HookBinaryPath string
	// Executor is the exact native git command boundary.
	Executor git.CommandExecutor
}

// RepositoryProvisionResult reports one provisioning attempt.
type RepositoryProvisionResult struct {
	// RepositoryPath is the canonical repository path.
	RepositoryPath string
	// Repository is the canonical tenant-relative repository name.
	Repository string
	// Created is true only when this attempt initialized the bare repository.
	Created bool
}

// repositoryPolicyCommitMessage is the appended policy commit's message.
func repositoryPolicyCommitMessage(repository string) string {
	return "repo " + repository
}

// defaultRepositoryPolicy is the exact `conf/git.conf` block KODAMA.md defines
// for a provisioned repository: read for every principal in scope, forced
// writes below refs/heads/work/ for engineers and executions, fast-forward
// writes to the default branch for executions, and reviewer approval for every
// update of the default branch. The default branch is the same `main` the
// provisioned admin repository and every bare repository use.
func defaultRepositoryPolicy(repository string) string {
	return "repo " + repository + "\n" +
		"    R = @all\n" +
		"    RW+ refs/heads/work/ = @engineer @execution\n" +
		"    RW refs/heads/" + provisioningInitialBranch + " = @execution\n" +
		"    option approve.refs/heads/" + provisioningInitialBranch + " = reviewer\n"
}

// requireDefaultRepositoryPolicy proves the compile-time default block parses,
// grants exactly the documented access and never widens it, before any of it
// can become durable state.
func requireDefaultRepositoryPolicy(repository string) error {
	config, err := git.ParseConfig([]byte(defaultRepositoryPolicy(repository)))
	if err != nil {
		return fmt.Errorf("default repository policy does not parse: %w", err)
	}
	policy, ok := config.Policy(repository)
	if !ok {
		return fmt.Errorf("default repository policy does not declare the %q repository", repository)
	}
	engineer := git.Identity{Roles: []string{"engineer"}}
	execution := git.Identity{Execution: true}
	readDecision, err := policy.AuthorizeRead(git.ReadRequest{Identity: engineer})
	if err != nil {
		return fmt.Errorf("default repository policy read check: %w", err)
	}
	if readDecision != git.DecisionAllow {
		return fmt.Errorf("default repository policy does not allow an engineer to read the %q repository", repository)
	}
	for _, check := range []struct {
		name    string
		request git.AccessRequest
	}{
		{
			name: "engineer work branch",
			request: git.AccessRequest{
				Identity: engineer,
				Ref:      "refs/heads/work/feature",
				Update:   git.UpdateCreate,
			},
		},
		{
			name: "execution work branch",
			request: git.AccessRequest{
				Identity: execution,
				Ref:      "refs/heads/work/feature",
				Update:   git.UpdateCreate,
			},
		},
		{
			name: "execution default branch",
			request: git.AccessRequest{
				Identity: execution,
				Ref:      "refs/heads/" + provisioningInitialBranch,
				Update:   git.UpdateFastForward,
			},
		},
	} {
		decision, err := policy.Authorize(check.request)
		if err != nil {
			return fmt.Errorf("default repository policy %s check: %w", check.name, err)
		}
		if decision != git.DecisionAllow {
			return fmt.Errorf("default repository policy does not allow the %s", check.name)
		}
	}
	denied, err := policy.Authorize(git.AccessRequest{
		Identity: engineer,
		Ref:      "refs/heads/" + provisioningInitialBranch,
		Update:   git.UpdateCreate,
	})
	if err != nil {
		return fmt.Errorf("default repository policy engineer default-branch check: %w", err)
	}
	if denied != git.DecisionDeny {
		return fmt.Errorf("default repository policy widens the default branch to engineers")
	}
	role, guarded := policy.ApprovalFor("refs/heads/" + provisioningInitialBranch)
	if !guarded || role != "reviewer" {
		return fmt.Errorf("default repository policy does not require reviewer approval on the default branch")
	}
	return nil
}

// ProvisionRepository initializes one tenant repository with the bare layout
// the Git service expects and appends its default rules to the trusted admin
// policy, or converges on an existing valid repository and policy.
//
// The trusted admin repository is validated and its accepted policy read before
// any repository effect, so a tenant without provisioning is refused without an
// orphan repository. Creation and the policy append run under the same tenant
// advisory lock tenant provisioning uses; the policy ref moves with a
// compare-and-swap against the accepted commit, so an intervening accepted admin
// push is never clobbered. This is a locally seeded commit, not a remote push:
// it writes no journal entry and needs no verification identity.
func ProvisionRepository(ctx context.Context, config RepositoryProvisionConfig) (RepositoryProvisionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if config.Executor == nil {
		return RepositoryProvisionResult{}, fmt.Errorf("repository provisioning has no git command executor")
	}
	if strings.TrimSpace(config.HookBinaryPath) == "" {
		return RepositoryProvisionResult{}, fmt.Errorf("repository provisioning has no hook binary path")
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return RepositoryProvisionResult{}, err
	}
	tenantDirectory, repositoryPath, adminRepositoryPath, repository, err := repositoryProvisionPaths(config)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	if err := RequireGitVersion(ctx, config.Executor); err != nil {
		return RepositoryProvisionResult{}, err
	}
	// The admin repository is the only policy source. Validate it and read the
	// accepted policy before the first repository effect.
	if err := validateRepositoryProvisionAdmin(ctx, adminRepositoryPath, config.Executor); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return RepositoryProvisionResult{}, err
	}
	info, err := os.Lstat(tenantDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			return RepositoryProvisionResult{}, fmt.Errorf("%w: tenant %q is not provisioned", ErrRepositoryProvisionDestination, config.TenantID)
		}
		return RepositoryProvisionResult{}, fmt.Errorf("inspect tenant directory %q: %w", tenantDirectory, err)
	}
	if !info.IsDir() {
		return RepositoryProvisionResult{}, fmt.Errorf("%w: tenant directory %q is not a directory", ErrRepositoryProvisionContainment, tenantDirectory)
	}
	release, err := acquireProvisionLock(ctx, tenantDirectory)
	if err != nil {
		return RepositoryProvisionResult{}, fmt.Errorf("lock tenant repository provisioning: %w", err)
	}
	defer release()

	// Recheck the trusted policy under the lock: an accepted admin push may
	// have landed while this attempt waited.
	if err := validateRepositoryProvisionAdmin(ctx, adminRepositoryPath, config.Executor); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if err := requireRepositoryProvisionContainment(tenantDirectory, repositoryPath); err != nil {
		return RepositoryProvisionResult{}, err
	}
	if err := refuseNestedRepositoryDestination(ctx, tenantDirectory, repositoryPath, config.Executor); err != nil {
		return RepositoryProvisionResult{}, err
	}
	created, err := ensureProvisionedRepository(ctx, tenantDirectory, repositoryPath, config.HookBinaryPath, config.Executor)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	policy, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: adminRepositoryPath,
		Executor:       config.Executor,
	})
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	readContext := WithoutCommandEnvironment(ctx)
	parent, err := policy.acceptedCommit(readContext)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	accepted, err := policy.readCommittedPolicy(readContext, parent)
	if err != nil {
		return RepositoryProvisionResult{}, err
	}
	parsed, err := git.ParseConfig(accepted)
	if err != nil {
		return RepositoryProvisionResult{}, fmt.Errorf("trusted admin policy %s does not parse: %w", CommittedPolicyFile, err)
	}
	if _, ok := parsed.Policy(repository); ok {
		// The repository already has rules. Converge without appending a
		// duplicate block or adding the defaults over an Admin's restriction.
		return RepositoryProvisionResult{RepositoryPath: repositoryPath, Repository: repository, Created: created}, nil
	}
	if err := requireDefaultRepositoryPolicy(repository); err != nil {
		return RepositoryProvisionResult{}, err
	}
	candidate := appendRepositoryPolicy(accepted, repository)
	if _, err := policy.installCommittedPolicy(ctx, parent, candidate, repository); err != nil {
		return RepositoryProvisionResult{RepositoryPath: repositoryPath, Repository: repository, Created: created}, err
	}
	return RepositoryProvisionResult{RepositoryPath: repositoryPath, Repository: repository, Created: created}, nil
}

// repositoryProvisionPaths resolves and validates the three provisioning paths.
func repositoryProvisionPaths(config RepositoryProvisionConfig) (string, string, string, string, error) {
	if strings.TrimSpace(config.TenantDirectory) == "" {
		return "", "", "", "", fmt.Errorf("%w: tenant directory is empty", ErrRepositoryProvisionContainment)
	}
	if strings.TrimSpace(config.RepositoryPath) == "" {
		return "", "", "", "", fmt.Errorf("%w: repository path is empty", ErrRepositoryProvisionContainment)
	}
	if strings.TrimSpace(config.AdminRepositoryPath) == "" {
		return "", "", "", "", fmt.Errorf("%w: admin repository path is empty", ErrRepositoryProvisionContainment)
	}
	tenantDirectory, err := filepath.Abs(config.TenantDirectory)
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: resolve tenant directory: %v", ErrRepositoryProvisionContainment, err)
	}
	tenantDirectory = filepath.Clean(tenantDirectory)
	repositoryPath, err := filepath.Abs(config.RepositoryPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: resolve repository path: %v", ErrRepositoryProvisionContainment, err)
	}
	repositoryPath = filepath.Clean(repositoryPath)
	adminRepositoryPath, err := filepath.Abs(config.AdminRepositoryPath)
	if err != nil {
		return "", "", "", "", fmt.Errorf("%w: resolve admin repository path: %v", ErrRepositoryProvisionContainment, err)
	}
	adminRepositoryPath = filepath.Clean(adminRepositoryPath)
	if adminRepositoryPath != filepath.Join(tenantDirectory, AdminRepository) {
		return "", "", "", "", fmt.Errorf("%w: admin repository %q is not directly under tenant directory %q",
			ErrRepositoryProvisionContainment, adminRepositoryPath, tenantDirectory)
	}
	relative, err := filepath.Rel(tenantDirectory, repositoryPath)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", "", "", fmt.Errorf("%w: repository %q is not inside tenant directory %q",
			ErrRepositoryProvisionContainment, repositoryPath, tenantDirectory)
	}
	repository := filepath.ToSlash(relative)
	if repository == AdminRepository || strings.HasPrefix(repository, AdminRepository+"/") {
		return "", "", "", "", fmt.Errorf("%w: %q is the trusted admin repository",
			ErrRepositoryProvisionDestination, repository)
	}
	if err := requireRepositoryProvisionContainment(tenantDirectory, repositoryPath); err != nil {
		return "", "", "", "", err
	}
	return tenantDirectory, repositoryPath, adminRepositoryPath, repository, nil
}

// requireRepositoryProvisionContainment refuses a repository path whose
// existing ancestors resolve outside the tenant directory, including a
// symlinked parent or destination.
func requireRepositoryProvisionContainment(tenantDirectory, repositoryPath string) error {
	resolvedTenant, err := resolveExisting(tenantDirectory)
	if err != nil {
		return fmt.Errorf("%w: resolve tenant directory: %v", ErrRepositoryProvisionContainment, err)
	}
	resolvedRepository, err := resolveExisting(repositoryPath)
	if err != nil {
		return fmt.Errorf("%w: resolve repository path: %v", ErrRepositoryProvisionContainment, err)
	}
	if !within(resolvedTenant, resolvedRepository) {
		return fmt.Errorf("%w: repository %q resolves outside tenant directory %q",
			ErrRepositoryProvisionContainment, repositoryPath, tenantDirectory)
	}
	return nil
}

// validateRepositoryProvisionAdmin proves the trusted admin repository is the
// provisioned one and its accepted policy is readable and acceptable.
func validateRepositoryProvisionAdmin(ctx context.Context, adminRepositoryPath string, executor git.CommandExecutor) error {
	provisioner := &adminProvisioner{repositoryPath: adminRepositoryPath, executor: executor}
	if err := provisioner.validateExistingRepository(ctx); err != nil {
		return err
	}
	policy, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: adminRepositoryPath,
		Executor:       executor,
	})
	if err != nil {
		return err
	}
	_, err = policy.Current(ctx)
	return err
}

// ensureProvisionedRepository initializes the bare repository when the
// destination is absent or an empty directory, adopts an existing valid one,
// and refuses every other existing destination. It returns whether this attempt
// initialized the repository.
func ensureProvisionedRepository(ctx context.Context, tenantDirectory, repositoryPath, hookBinary string, executor git.CommandExecutor) (bool, error) {
	info, err := os.Lstat(repositoryPath)
	switch {
	case os.IsNotExist(err):
		if err := publishProvisionedRepository(ctx, tenantDirectory, repositoryPath, hookBinary, executor); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, fmt.Errorf("inspect repository destination %q: %w", repositoryPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: repository destination %q is a symbolic link",
			ErrRepositoryProvisionContainment, repositoryPath)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: repository destination %q is not a directory",
			ErrRepositoryProvisionDestination, repositoryPath)
	}
	entries, err := os.ReadDir(repositoryPath)
	if err != nil {
		return false, fmt.Errorf("inspect repository destination %q: %w", repositoryPath, err)
	}
	if len(entries) == 0 {
		if err := publishProvisionedRepository(ctx, tenantDirectory, repositoryPath, hookBinary, executor); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := validateProvisionedRepository(ctx, repositoryPath, executor); err != nil {
		return false, err
	}
	// An established repository is adopted as it is, but the managed hooks are
	// the only enforcement point and must be present before the next push.
	// InstallHooks preserves a matching hook and refuses a foreign one.
	if _, err := InstallHooks(HooksConfig{RepositoryPath: repositoryPath, BinaryPath: hookBinary}); err != nil {
		return false, fmt.Errorf("install managed repository hooks: %w", err)
	}
	if err := normalizeProvisionedTree(repositoryPath); err != nil {
		return false, fmt.Errorf("normalize provisioned repository: %w", err)
	}
	return false, nil
}

// publishProvisionedRepository builds the complete repository, including its
// managed hooks and private modes, in a private sibling staging directory and
// publishes it with one same-filesystem rename. A concurrently running service
// therefore either sees no repository or sees one whose hooks are already
// installed, never a reachable hookless window.
func publishProvisionedRepository(ctx context.Context, tenantDirectory, repositoryPath, hookBinary string, executor git.CommandExecutor) error {
	staging, err := os.MkdirTemp(tenantDirectory, provisioningRepositoryStagingPrefix)
	if err != nil {
		return fmt.Errorf("create private repository staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return fmt.Errorf("set repository staging directory mode: %w", err)
	}
	if err := initializeBareRepository(ctx, staging, executor); err != nil {
		return err
	}
	format, err := runRequiredCommand(ctx, executor, "--git-dir="+staging, "rev-parse", "--show-ref-format")
	if err != nil || format != refStorageFiles {
		return fmt.Errorf("repository staging directory has ref storage %q; want %s", format, refStorageFiles)
	}
	head, err := runRequiredCommand(ctx, executor, "--git-dir="+staging, "symbolic-ref", "HEAD")
	if err != nil || head != CommittedPolicyRef {
		return fmt.Errorf("repository staging directory HEAD is %q; want %s", head, CommittedPolicyRef)
	}
	if _, err := InstallHooks(HooksConfig{RepositoryPath: staging, BinaryPath: hookBinary}); err != nil {
		return fmt.Errorf("install managed repository hooks: %w", err)
	}
	if err := normalizeProvisionedTree(staging); err != nil {
		return fmt.Errorf("normalize provisioned repository: %w", err)
	}
	if err := syncProvisionedTree(staging); err != nil {
		return err
	}
	// Publication is one rename. The destination is absent or an empty
	// directory under the tenant lock. os.Rename refuses to replace an existing
	// directory, so remove a destination this attempt classified as empty just
	// before publishing; a concurrent creator makes that remove or the rename
	// fail instead of merging trees.
	if err := os.MkdirAll(filepath.Dir(repositoryPath), 0o750); err != nil {
		return fmt.Errorf("create repository parent: %w", err)
	}
	if info, err := os.Lstat(repositoryPath); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: repository destination %q is not an empty directory",
				ErrRepositoryProvisionDestination, repositoryPath)
		}
		entries, err := os.ReadDir(repositoryPath)
		if err != nil || len(entries) != 0 {
			return fmt.Errorf("%w: repository destination %q is not empty",
				ErrRepositoryProvisionDestination, repositoryPath)
		}
		if err := os.Remove(repositoryPath); err != nil {
			return fmt.Errorf("%w: remove empty repository destination %q: %v",
				ErrRepositoryProvisionDestination, repositoryPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect repository destination %q: %w", repositoryPath, err)
	}
	if err := os.Rename(staging, repositoryPath); err != nil {
		return fmt.Errorf("publish repository: %w", err)
	}
	published = true
	if err := syncDirectory(tenantDirectory); err != nil {
		return fmt.Errorf("repository published but the tenant directory was not flushed: %w", err)
	}
	if err := syncDirectory(filepath.Dir(tenantDirectory)); err != nil {
		return fmt.Errorf("repository published but the storage root was not flushed: %w", err)
	}
	return nil
}

// refuseNestedRepositoryDestination refuses a destination below an existing
// bare repository. Without this a name such as shop/hooks/nested would create a
// repository inside shop's own directory, where the outer repository's hooks
// and policy lookups do not apply.
func refuseNestedRepositoryDestination(ctx context.Context, tenantDirectory, repositoryPath string, executor git.CommandExecutor) error {
	for current := filepath.Dir(repositoryPath); current != tenantDirectory; current = filepath.Dir(current) {
		if !within(tenantDirectory, current) {
			break
		}
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect repository ancestor %q: %w", current, err)
		}
		if !info.IsDir() {
			continue
		}
		result, err := executor.Run(ctx, git.Command{
			Name: "git",
			Args: []string{"--git-dir=" + current, "rev-parse", "--is-bare-repository"},
		})
		if err != nil {
			return err
		}
		if result.ExitCode == 0 && strings.TrimSpace(string(result.Stdout)) == "true" {
			return fmt.Errorf("%w: repository %q is inside the existing bare repository %q",
				ErrRepositoryProvisionDestination, repositoryPath, current)
		}
	}
	return nil
}

// initializeBareRepository creates the bare repository with the exact transport
// configuration and `main` HEAD the service expects. It sets no ref policy in
// Git configuration: access rules belong to the committed admin policy.
func initializeBareRepository(ctx context.Context, repositoryPath string, executor git.CommandExecutor) error {
	if err := os.MkdirAll(filepath.Dir(repositoryPath), 0o750); err != nil {
		return fmt.Errorf("create repository parent: %w", err)
	}
	commands := [][]string{
		{"init", "--bare", "--quiet",
			"--ref-format=" + refStorageFiles,
			"--initial-branch=" + provisioningInitialBranch, repositoryPath},
		{"--git-dir=" + repositoryPath, "symbolic-ref", "HEAD", CommittedPolicyRef},
		{"--git-dir=" + repositoryPath, "config", "receive.advertiseAtomic", "true"},
		{"--git-dir=" + repositoryPath, "config", "http.receivepack", "true"},
	}
	for _, args := range commands {
		if _, err := runRequiredCommand(ctx, executor, args...); err != nil {
			return fmt.Errorf("initialize bare repository: %w", err)
		}
	}
	return nil
}

// validateProvisionedRepository proves an existing destination is the bare
// repository initializeBareRepository creates. It never writes.
func validateProvisionedRepository(ctx context.Context, repositoryPath string, executor git.CommandExecutor) error {
	if _, err := os.Lstat(filepath.Join(repositoryPath, "reftable")); err == nil {
		return fmt.Errorf("%w: repository %q has a reftable ref store",
			ErrRepositoryProvisionDestination, repositoryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%w: inspect repository %q ref store: %v",
			ErrRepositoryProvisionDestination, repositoryPath, err)
	}
	format, err := runRequiredCommand(ctx, executor, "--git-dir="+repositoryPath, "rev-parse", "--show-ref-format")
	if err != nil || format != refStorageFiles {
		return fmt.Errorf("%w: repository %q is not the provisioned files-backend bare repository",
			ErrRepositoryProvisionDestination, repositoryPath)
	}
	bare, err := runRequiredCommand(ctx, executor, "--git-dir="+repositoryPath, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return fmt.Errorf("%w: repository %q is not a bare repository",
			ErrRepositoryProvisionDestination, repositoryPath)
	}
	head, err := runRequiredCommand(ctx, executor, "--git-dir="+repositoryPath, "symbolic-ref", "HEAD")
	if err != nil || head != CommittedPolicyRef {
		return fmt.Errorf("%w: repository %q HEAD is not %s",
			ErrRepositoryProvisionDestination, repositoryPath, CommittedPolicyRef)
	}
	for name, key := range map[string][2]string{
		"atomic receives":     {"receive.advertiseAtomic", "true"},
		"smart-HTTP receives": {"http.receivepack", "true"},
	} {
		value, err := runRequiredCommand(ctx, executor, "--git-dir="+repositoryPath, "config", "--bool", key[0])
		if err != nil || value != key[1] {
			return fmt.Errorf("%w: repository %q does not enable %s",
				ErrRepositoryProvisionDestination, repositoryPath, name)
		}
	}
	return nil
}

// appendRepositoryPolicy appends one default block to the accepted policy
// bytes, keeping a blank separator and never altering the existing text.
func appendRepositoryPolicy(accepted []byte, repository string) []byte {
	var builder strings.Builder
	builder.Write(accepted)
	if len(accepted) > 0 {
		if accepted[len(accepted)-1] != '\n' {
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}
	builder.WriteString(defaultRepositoryPolicy(repository))
	return []byte(builder.String())
}

// installCommittedPolicy writes the candidate policy as one commit whose parent
// is the accepted commit and moves the trusted ref with a compare-and-swap. It
// uses the same bounded plumbing and explicit service identity tenant
// provisioning uses, and proves the committed blob before reporting success.
func (policy *CommittedPolicy) installCommittedPolicy(ctx context.Context, parent string, candidate []byte, repository string) (string, error) {
	// The candidate is checked before anything becomes durable, and in
	// particular before the trusted ref moves: a policy that is too large or
	// does not parse, or that would lock the admin role out of its own policy
	// ref, must leave the accepted policy exactly as it was. The later
	// post-commit verification repeats these checks against what was actually
	// stored, so it proves the stored bytes rather than the candidate.
	if len(candidate) > MaxCommittedPolicyBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds %d", ErrCommittedPolicyTooLarge, len(candidate), MaxCommittedPolicyBytes)
	}
	parsedCandidate, err := git.ParseConfig(candidate)
	if err != nil {
		return "", fmt.Errorf("candidate repository policy does not parse: %w", err)
	}
	if err := requireAdminWriteOfPolicyRef(parsedCandidate); err != nil {
		return "", fmt.Errorf("candidate repository policy is not accepted: %w", err)
	}
	repositoryPath := policy.repositoryPath
	workDirectory, err := os.MkdirTemp(filepath.Dir(repositoryPath), ".provision-repository-")
	if err != nil {
		return "", fmt.Errorf("create private repository-provisioning directory: %w", err)
	}
	defer os.RemoveAll(workDirectory)
	if err := os.Chmod(workDirectory, 0o700); err != nil {
		return "", fmt.Errorf("set repository-provisioning directory mode: %w", err)
	}
	policyPath := filepath.Join(workDirectory, "git.conf")
	if err := os.WriteFile(policyPath, candidate, 0o600); err != nil {
		return "", fmt.Errorf("write repository policy: %w", err)
	}
	blob, err := runRequiredCommand(ctx, policy.executor,
		"--git-dir="+repositoryPath, "hash-object", "-w", "--no-filters", policyPath)
	if err != nil {
		return "", fmt.Errorf("store repository policy blob: %w", err)
	}
	if !validPolicyCommit(blob) {
		return "", fmt.Errorf("repository policy blob id %q is not an object id", blob)
	}
	indexPath := filepath.Join(workDirectory, "index")
	indexContext := WithCommandEnvironment(ctx, []string{"GIT_INDEX_FILE=" + indexPath})
	if _, err := runRequiredCommand(indexContext, policy.executor, "--git-dir="+repositoryPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+","+CommittedPolicyFile); err != nil {
		return "", fmt.Errorf("index repository policy: %w", err)
	}
	tree, err := runRequiredCommand(indexContext, policy.executor, "--git-dir="+repositoryPath, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write repository policy tree: %w", err)
	}
	if !validPolicyCommit(tree) {
		return "", fmt.Errorf("repository policy tree id %q is not an object id", tree)
	}
	date := time.Now().UTC().Format(time.RFC3339)
	identityContext := WithCommandEnvironment(ctx, []string{
		"GIT_AUTHOR_NAME=" + provisioningAgentName,
		"GIT_AUTHOR_EMAIL=" + provisioningAgentEmail,
		"GIT_COMMITTER_NAME=" + provisioningAgentName,
		"GIT_COMMITTER_EMAIL=" + provisioningAgentEmail,
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	})
	commit, err := runRequiredCommand(identityContext, policy.executor, "--git-dir="+repositoryPath,
		"-c", "user.name="+provisioningAgentName,
		"-c", "user.email="+provisioningAgentEmail,
		"commit-tree", tree, "-p", parent, "-m", repositoryPolicyCommitMessage(repository))
	if err != nil {
		return "", fmt.Errorf("commit repository policy: %w", err)
	}
	if !validPolicyCommit(commit) {
		return "", fmt.Errorf("repository policy commit id %q is not an object id", commit)
	}
	if _, err := runRequiredCommand(ctx, policy.executor, "--git-dir="+repositoryPath,
		"update-ref", CommittedPolicyRef, commit, parent); err != nil {
		return "", fmt.Errorf("point %s at the repository policy commit: %w", CommittedPolicyRef, err)
	}
	committed, err := policy.executor.Run(ctx, git.Command{Name: "git", Args: []string{
		"--git-dir=" + repositoryPath, "cat-file", "blob", CommittedPolicyRef + ":" + CommittedPolicyFile,
	}})
	if err != nil {
		return "", fmt.Errorf("read committed repository policy: %w", err)
	}
	if committed.ExitCode != 0 {
		return "", fmt.Errorf("committed repository policy is unreadable: %s",
			strings.TrimSpace(string(committed.Stderr)))
	}
	if string(committed.Stdout) != string(candidate) {
		return "", fmt.Errorf("committed repository policy differs from the candidate")
	}
	parsed, err := git.ParseConfig(committed.Stdout)
	if err != nil {
		return "", fmt.Errorf("committed repository policy does not parse: %w", err)
	}
	if err := requireAdminWriteOfPolicyRef(parsed); err != nil {
		return "", fmt.Errorf("committed repository policy is not accepted: %w", err)
	}
	return commit, nil
}

// runRequiredCommand runs one native git command through the exact executor
// seam and requires a zero exit code. Diagnostics are bounded to the command's
// own stderr; no credential is ever echoed.
func runRequiredCommand(ctx context.Context, executor git.CommandExecutor, args ...string) (string, error) {
	result, err := executor.Run(ctx, git.Command{Name: "git", Args: args})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), result.ExitCode,
			strings.TrimSpace(string(result.Stderr)))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}
