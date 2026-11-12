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

// Sentinel refusals of tenant provisioning. They are exported so the app
// boundary and the command can report a stable cause without parsing text.
var (
	// ErrProvisionDestination reports an existing admin destination that is not
	// the exact repository provisioning created. It is never repaired, reseeded
	// or deleted.
	ErrProvisionDestination = errors.New("admin repository destination is not provisionable")
	// ErrProvisionContainment reports a tenant, destination or staging path
	// that escapes the bound tenant directory.
	ErrProvisionContainment = errors.New("tenant provisioning path escapes its tenant")
)

// ProvisionStep names one provisioning boundary. The step hook is invoked
// immediately before the named step; StepPublished runs after the atomic
// publication rename so a failure there models a crash after publication.
type ProvisionStep string

const (
	// StepStage creates the private sibling staging directory and initializes
	// the bare repository inside it.
	StepStage ProvisionStep = "stage"
	// StepHooks installs the managed hooks before publication.
	StepHooks ProvisionStep = "hooks"
	// StepCommit writes the initial policy commit.
	StepCommit ProvisionStep = "commit"
	// StepSync flushes the staged tree to disk.
	StepSync ProvisionStep = "sync"
	// StepJournal prepares the receive-journal directory before publication.
	StepJournal ProvisionStep = "journal"
	// StepRename publishes the complete directory by rename.
	StepRename ProvisionStep = "rename"
	// StepPublished observes the completed publication. An error here reports
	// uncertainty but never deletes the published repository.
	StepPublished ProvisionStep = "published"
)

// StepHook observes one provisioning boundary. Returning an error aborts the
// attempt at that boundary. Production passes nil; it is the failure-injection
// seam.
type StepHook func(ctx context.Context, step ProvisionStep) error

// AdminProvisionConfig is the native admin-repository creation input.
type AdminProvisionConfig struct {
	// TenantID is the tenant the destination belongs to.
	TenantID id.TenantID
	// RepositoryPath is the canonical admin repository destination.
	RepositoryPath string
	// TenantDirectory holds the destination and the provisioning lock. It is
	// the only directory staging is created under.
	TenantDirectory string
	// JournalDirectory is the receive-journal directory prepared before
	// publication.
	JournalDirectory string
	// HookBinaryPath is the absolute executable the installed hooks run.
	HookBinaryPath string
	// Executor is the exact native git command boundary.
	Executor git.CommandExecutor
	// Step is the optional boundary seam.
	Step StepHook
}

// AdminProvisionResult reports one provisioning attempt.
type AdminProvisionResult struct {
	// RepositoryPath is the canonical admin repository path.
	RepositoryPath string
	// Created is true only when this attempt published a new repository.
	Created bool
}

// provisioning constants. The policy bytes are the one durable initial state
// decision 31(b) records: tenant provisioning creates the `admin` repository
// with `repo admin / RW = @admin` as its first, parentless commit.
const (
	// initialAdminPolicy is the exact `conf/git.conf` blob of the first commit.
	// It uses the declared rule grammar and grants only the admin role a
	// fast-forward of the trusted policy ref (see requireAdminWriteOfPolicyRef).
	initialAdminPolicy = "repo admin\n    RW = @admin\n"
	// provisioningLockName is the regular advisory-lock file inside the tenant
	// directory. It is never a repository and is deliberately left behind: it
	// carries no provisioning state.
	provisioningLockName = ".provision-tenant.lock"
	// provisioningStagingPrefix bounds every private staging directory this
	// invocation may create. A later attempt never reads or removes one it did
	// not create, so a crash leftover stays private and inert.
	provisioningStagingPrefix = ".provision-admin-"
	// provisioningPolicyFile, provisioningIndexFile and
	// provisioningTemplateDirectory are private names inside the staging
	// directory. All three are removed before publication.
	provisioningPolicyFile        = ".provision-policy"
	provisioningIndexFile         = ".provision-index"
	provisioningTemplateDirectory = ".provision-template"
	// provisioningInitialBranch is the explicit default branch the bare
	// repository and its HEAD are created with.
	provisioningInitialBranch = "main"
	// provisioningCommitMessage is the first commit's message.
	provisioningCommitMessage = "repo admin"
	// provisioningAgentName and provisioningAgentEmail are the explicit
	// author/committer identity. They are never inherited from host config.
	provisioningAgentName  = "Kodama Provisioning"
	provisioningAgentEmail = "kodama@example.invalid"
)

// ProvisionAdminRepository creates the tenant's trusted admin repository
// atomically, or validates and preserves the existing one.
//
// The version probe runs on the actual executor before the first filesystem
// effect. Creation happens in a private sibling staging directory that is
// initialized with the files ref backend, committed through bounded plumbing,
// given the managed hooks, normalized to private modes and flushed to disk.
// Publication is one same-filesystem rename, so a reader either sees the whole
// committed provisioning state or no admin repository at all. A failure after
// publication reports uncertainty and keeps the repository; a retry validates
// it and returns Created=false.
func ProvisionAdminRepository(ctx context.Context, config AdminProvisionConfig) (AdminProvisionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return AdminProvisionResult{}, err
	}
	if config.Executor == nil {
		return AdminProvisionResult{}, fmt.Errorf("admin repository provisioning has no git command executor")
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return AdminProvisionResult{}, err
	}
	if strings.TrimSpace(config.HookBinaryPath) == "" {
		return AdminProvisionResult{}, fmt.Errorf("admin repository provisioning has no hook binary path")
	}
	tenantDirectory, repositoryPath, journalDirectory, err := provisionPaths(config)
	if err != nil {
		return AdminProvisionResult{}, err
	}
	// The initial policy is a compile-time constant; proving it satisfies the
	// parser and the admin-write rule before any effect keeps a broken
	// constant from ever becoming durable state.
	if err := requireInitialAdminPolicy(); err != nil {
		return AdminProvisionResult{}, err
	}
	// Read-only containment validation, then the mandatory version probe.
	// Neither creates a directory, so an unsupported executor leaves the
	// filesystem untouched.
	if err := validateProvisionContainment(tenantDirectory, repositoryPath); err != nil {
		return AdminProvisionResult{}, err
	}
	if err := RequireGitVersion(ctx, config.Executor); err != nil {
		return AdminProvisionResult{}, err
	}
	provisioner := &adminProvisioner{
		tenantID:         config.TenantID,
		tenantDirectory:  tenantDirectory,
		repositoryPath:   repositoryPath,
		journalDirectory: journalDirectory,
		hookBinary:       config.HookBinaryPath,
		executor:         config.Executor,
		step:             config.Step,
	}
	return provisioner.provision(ctx)
}

// provisionPaths resolves the three provisioning paths and proves the admin
// destination is exactly `<tenant>/admin`. A caller cannot point provisioning
// at another tenant or another path shape.
func provisionPaths(config AdminProvisionConfig) (string, string, string, error) {
	if strings.TrimSpace(config.TenantDirectory) == "" {
		return "", "", "", fmt.Errorf("%w: tenant directory is empty", ErrProvisionContainment)
	}
	if strings.TrimSpace(config.RepositoryPath) == "" {
		return "", "", "", fmt.Errorf("%w: admin repository path is empty", ErrProvisionContainment)
	}
	tenantDirectory, err := filepath.Abs(config.TenantDirectory)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: resolve tenant directory: %v", ErrProvisionContainment, err)
	}
	repositoryPath, err := filepath.Abs(config.RepositoryPath)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: resolve admin repository path: %v", ErrProvisionContainment, err)
	}
	if filepath.Clean(repositoryPath) != filepath.Join(filepath.Clean(tenantDirectory), AdminRepository) {
		return "", "", "", fmt.Errorf("%w: admin repository %q is not directly under tenant directory %q",
			ErrProvisionContainment, repositoryPath, tenantDirectory)
	}
	if strings.TrimSpace(config.JournalDirectory) == "" {
		return "", "", "", fmt.Errorf("admin repository provisioning has no journal directory")
	}
	journalDirectory, err := filepath.Abs(config.JournalDirectory)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve receive-journal directory: %w", err)
	}
	return filepath.Clean(tenantDirectory), filepath.Clean(repositoryPath), filepath.Clean(journalDirectory), nil
}

// validateProvisionContainment refuses a symlinked tenant directory, a
// symlinked admin destination and a tenant path that is not a directory. It is
// read-only: it is called before any effect and again under the lock.
func validateProvisionContainment(tenantDirectory, repositoryPath string) error {
	if err := refuseSymlinkedPath(tenantDirectory, "tenant directory"); err != nil {
		return err
	}
	if err := refuseSymlinkedPath(repositoryPath, "admin repository"); err != nil {
		return err
	}
	return nil
}

// refuseSymlinkedPath refuses a symbolic link or a non-directory tenant path.
// A path that does not exist is contained by construction.
func refuseSymlinkedPath(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%w: inspect %s %q: %v", ErrProvisionContainment, label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s %q is a symbolic link", ErrProvisionContainment, label, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s %q is not a directory", ErrProvisionContainment, label, path)
	}
	return nil
}

// requireInitialAdminPolicy proves the constant initial policy parses and
// leaves the trusted ref writable by the admin role without an approval guard.
func requireInitialAdminPolicy() error {
	config, err := git.ParseConfig([]byte(initialAdminPolicy))
	if err != nil {
		return fmt.Errorf("initial admin policy does not parse: %w", err)
	}
	if err := requireAdminWriteOfPolicyRef(config); err != nil {
		return fmt.Errorf("initial admin policy is not accepted: %w", err)
	}
	return nil
}

// adminProvisioner owns one provisioning attempt's private state.
type adminProvisioner struct {
	tenantID         id.TenantID
	tenantDirectory  string
	repositoryPath   string
	journalDirectory string
	hookBinary       string
	executor         git.CommandExecutor
	step             StepHook
}

// provision serializes one attempt, then creates or validates the destination.
func (provisioner *adminProvisioner) provision(ctx context.Context) (AdminProvisionResult, error) {
	// The tenant directory is the only directory provisioning writes outside
	// the canonical destination. MkdirAll creates the storage root with the
	// same private mode when it is absent.
	if err := os.MkdirAll(provisioner.tenantDirectory, 0o700); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("create tenant directory: %w", err)
	}
	// The tenant directory is deliberately narrowed to 0700: it holds the
	// provisioning lock and, through the repository, tenant policy. The serve
	// path may keep its own directories at 0750, so this is a deployment note,
	// not a contradiction: the tenant directory is private to the provisioning
	// user, and the Git worker must run as that user (docs/specs/09-userland.md).
	if err := os.Chmod(provisioner.tenantDirectory, 0o700); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("set tenant directory mode: %w", err)
	}
	release, err := acquireProvisionLock(ctx, provisioner.tenantDirectory)
	if err != nil {
		return AdminProvisionResult{}, fmt.Errorf("lock tenant provisioning: %w", err)
	}
	defer release()

	// Recheck containment and the destination under the lock: another process
	// may have published while this attempt waited, and a path may have been
	// replaced by a symbolic link after the first check.
	if err := validateProvisionContainment(provisioner.tenantDirectory, provisioner.repositoryPath); err != nil {
		return AdminProvisionResult{}, err
	}
	info, err := os.Lstat(provisioner.repositoryPath)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return AdminProvisionResult{}, fmt.Errorf("%w: admin repository %q is a symbolic link",
				ErrProvisionContainment, provisioner.repositoryPath)
		}
		if !info.IsDir() {
			return AdminProvisionResult{}, fmt.Errorf("%w: admin destination %q is not a directory",
				ErrProvisionDestination, provisioner.repositoryPath)
		}
		if err := provisioner.validateExistingRepository(ctx); err != nil {
			return AdminProvisionResult{}, err
		}
		return AdminProvisionResult{RepositoryPath: provisioner.repositoryPath, Created: false}, nil
	case os.IsNotExist(err):
		return provisioner.create(ctx)
	default:
		return AdminProvisionResult{}, fmt.Errorf("inspect admin destination %q: %w", provisioner.repositoryPath, err)
	}
}

// create builds and publishes one admin repository.
func (provisioner *adminProvisioner) create(ctx context.Context) (AdminProvisionResult, error) {
	if err := provisioner.invokeStep(ctx, StepStage); err != nil {
		return AdminProvisionResult{}, err
	}
	staging, err := os.MkdirTemp(provisioner.tenantDirectory, provisioningStagingPrefix)
	if err != nil {
		return AdminProvisionResult{}, fmt.Errorf("create private staging directory: %w", err)
	}
	published := false
	// Only this invocation's staging directory is ever removed. A crash
	// leftover from another attempt is never read, adopted or deleted.
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("set staging directory mode: %w", err)
	}

	templateDirectory := filepath.Join(staging, provisioningTemplateDirectory)
	if err := os.Mkdir(templateDirectory, 0o700); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("create empty init template: %w", err)
	}
	if _, err := provisioner.require(ctx, "init", "--bare", "--quiet",
		"--ref-format="+refStorageFiles, "--initial-branch="+provisioningInitialBranch,
		"--template="+templateDirectory, staging); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("initialize bare admin repository: %w", err)
	}
	if err := os.RemoveAll(templateDirectory); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("remove empty init template: %w", err)
	}
	if err := provisioner.verifyFreshRepository(ctx, staging); err != nil {
		return AdminProvisionResult{}, err
	}
	// The served repository must accept a smart-HTTP receive-pack and advertise
	// atomic support exactly like every other provisioned repository. Ref policy
	// still belongs to Kodama's ordered rules and the hooks, never to git config.
	if _, err := provisioner.require(ctx, "--git-dir="+staging, "config", "receive.advertiseAtomic", "true"); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("configure atomic receives: %w", err)
	}
	if _, err := provisioner.require(ctx, "--git-dir="+staging, "config", "http.receivepack", "true"); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("configure smart-HTTP receives: %w", err)
	}

	if err := provisioner.invokeStep(ctx, StepCommit); err != nil {
		return AdminProvisionResult{}, err
	}
	if _, err := provisioner.writeInitialCommit(ctx, staging); err != nil {
		return AdminProvisionResult{}, err
	}

	if err := provisioner.invokeStep(ctx, StepHooks); err != nil {
		return AdminProvisionResult{}, err
	}
	if _, err := InstallHooks(HooksConfig{RepositoryPath: staging, BinaryPath: provisioner.hookBinary}); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("install managed admin hooks: %w", err)
	}

	if err := provisioner.invokeStep(ctx, StepJournal); err != nil {
		return AdminProvisionResult{}, err
	}
	if err := prepareJournalDirectory(provisioner.journalDirectory); err != nil {
		return AdminProvisionResult{}, err
	}

	if err := normalizeProvisionedTree(staging); err != nil {
		return AdminProvisionResult{}, err
	}

	if err := provisioner.invokeStep(ctx, StepSync); err != nil {
		return AdminProvisionResult{}, err
	}
	if err := syncProvisionedTree(staging); err != nil {
		return AdminProvisionResult{}, err
	}

	if err := provisioner.invokeStep(ctx, StepRename); err != nil {
		return AdminProvisionResult{}, err
	}
	// Publication is this single same-filesystem rename. The destination is
	// known absent under the lock, so the whole committed tree appears at once.
	// The under-lock re-check plus this rename's ENOTEMPTY rule are the guards:
	// rename replaces only an absent or empty destination, so a non-empty
	// directory makes it fail instead of merging. The residual window is an
	// empty directory created by a process outside the lock, which this rename
	// would replace; closing it needs renameat2(RENAME_NOREPLACE), which
	// requires golang.org/x/sys as a direct dependency.
	if err := os.Rename(staging, provisioner.repositoryPath); err != nil {
		return AdminProvisionResult{}, fmt.Errorf("publish admin repository: %w", err)
	}
	published = true
	if err := syncDirectory(provisioner.tenantDirectory); err != nil {
		return AdminProvisionResult{RepositoryPath: provisioner.repositoryPath, Created: true},
			fmt.Errorf("admin repository published but the publication parent was not flushed: %w", err)
	}
	if err := syncDirectory(filepath.Dir(provisioner.tenantDirectory)); err != nil {
		return AdminProvisionResult{RepositoryPath: provisioner.repositoryPath, Created: true},
			fmt.Errorf("admin repository published but the storage root was not flushed: %w", err)
	}
	if err := provisioner.invokeStep(ctx, StepPublished); err != nil {
		// The repository is durable. Reporting uncertainty is correct; deleting
		// it would destroy committed provisioning state.
		return AdminProvisionResult{RepositoryPath: provisioner.repositoryPath, Created: true}, err
	}
	return AdminProvisionResult{RepositoryPath: provisioner.repositoryPath, Created: true}, nil
}

// verifyFreshRepository proves the explicit initialization produced the files
// ref backend and the `main` HEAD this service requires.
func (provisioner *adminProvisioner) verifyFreshRepository(ctx context.Context, staging string) error {
	format, err := provisioner.require(ctx, "--git-dir="+staging, "rev-parse", "--show-ref-format")
	if err != nil {
		return fmt.Errorf("verify admin repository ref backend: %w", err)
	}
	if format != refStorageFiles {
		return fmt.Errorf("admin repository ref storage is %q; want %s", format, refStorageFiles)
	}
	head, err := provisioner.require(ctx, "--git-dir="+staging, "symbolic-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("verify admin repository HEAD: %w", err)
	}
	if head != CommittedPolicyRef {
		return fmt.Errorf("admin repository HEAD is %q; want %s", head, CommittedPolicyRef)
	}
	return nil
}

// writeInitialCommit writes the one parentless policy commit through bounded
// plumbing. It never uses a worktree, a shell or stdin: the policy and index
// are private files inside the staging directory and the per-command index is
// carried on the executor's documented context environment seam.
func (provisioner *adminProvisioner) writeInitialCommit(ctx context.Context, staging string) (string, error) {
	policyPath := filepath.Join(staging, provisioningPolicyFile)
	if err := os.WriteFile(policyPath, []byte(initialAdminPolicy), 0o600); err != nil {
		return "", fmt.Errorf("write initial policy: %w", err)
	}
	blob, err := provisioner.require(ctx, "--git-dir="+staging, "hash-object", "-w", "--no-filters", policyPath)
	if err != nil {
		return "", fmt.Errorf("store initial policy blob: %w", err)
	}
	if err := os.Remove(policyPath); err != nil {
		return "", fmt.Errorf("remove initial policy file: %w", err)
	}
	if !validPolicyCommit(blob) {
		return "", fmt.Errorf("initial policy blob id %q is not an object id", blob)
	}

	indexPath := filepath.Join(staging, provisioningIndexFile)
	indexContext := WithCommandEnvironment(ctx, []string{"GIT_INDEX_FILE=" + indexPath})
	if _, err := provisioner.require(indexContext, "--git-dir="+staging,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+","+CommittedPolicyFile); err != nil {
		return "", fmt.Errorf("index initial policy: %w", err)
	}
	tree, err := provisioner.require(indexContext, "--git-dir="+staging, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write initial policy tree: %w", err)
	}
	if err := os.Remove(indexPath); err != nil {
		return "", fmt.Errorf("remove private provisioning index: %w", err)
	}
	if !validPolicyCommit(tree) {
		return "", fmt.Errorf("initial policy tree id %q is not an object id", tree)
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
	commit, err := provisioner.require(identityContext, "--git-dir="+staging,
		"-c", "user.name="+provisioningAgentName,
		"-c", "user.email="+provisioningAgentEmail,
		"commit-tree", tree, "-m", provisioningCommitMessage)
	if err != nil {
		return "", fmt.Errorf("commit initial admin policy: %w", err)
	}
	if !validPolicyCommit(commit) {
		return "", fmt.Errorf("initial policy commit id %q is not an object id", commit)
	}
	if _, err := provisioner.require(ctx, "--git-dir="+staging,
		"update-ref", CommittedPolicyRef, commit); err != nil {
		return "", fmt.Errorf("point %s at the initial policy commit: %w", CommittedPolicyRef, err)
	}

	// Prove the committed blob, not the file that was hashed, is exactly the
	// documented policy, and that the parser accepts it.
	result, err := provisioner.invoke(ctx, "--git-dir="+staging,
		"cat-file", "blob", CommittedPolicyRef+":"+CommittedPolicyFile)
	if err != nil {
		return "", fmt.Errorf("read committed initial policy: %w", err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("committed initial policy is unreadable: %s", strings.TrimSpace(string(result.Stderr)))
	}
	if string(result.Stdout) != initialAdminPolicy {
		return "", fmt.Errorf("committed initial policy %q differs from the documented policy", result.Stdout)
	}
	parsed, err := git.ParseConfig(result.Stdout)
	if err != nil {
		return "", fmt.Errorf("committed initial policy does not parse: %w", err)
	}
	if err := requireAdminWriteOfPolicyRef(parsed); err != nil {
		return "", fmt.Errorf("committed initial policy is not accepted: %w", err)
	}
	return commit, nil
}

// validateExistingRepository proves an existing admin destination is exactly
// the durable provisioning state this command creates. It never writes.
func (provisioner *adminProvisioner) validateExistingRepository(ctx context.Context) error {
	repositoryPath := provisioner.repositoryPath
	// A files-backend proof requires that no reftable store is present.
	if _, err := os.Lstat(filepath.Join(repositoryPath, "reftable")); err == nil {
		return fmt.Errorf("%w: admin repository %q has a reftable ref store", ErrProvisionDestination, repositoryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%w: inspect admin repository %q ref store: %v", ErrProvisionDestination, repositoryPath, err)
	}
	format, err := provisioner.require(ctx, "--git-dir="+repositoryPath, "rev-parse", "--show-ref-format")
	if err != nil {
		return fmt.Errorf("%w: admin repository %q is not the provisioned files-backend repository: %v",
			ErrProvisionDestination, repositoryPath, err)
	}
	if format != refStorageFiles {
		return fmt.Errorf("%w: admin repository %q uses ref storage %q", ErrProvisionDestination, repositoryPath, format)
	}
	bare, err := provisioner.require(ctx, "--git-dir="+repositoryPath, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return fmt.Errorf("%w: admin repository %q is not a bare repository", ErrProvisionDestination, repositoryPath)
	}
	head, err := provisioner.require(ctx, "--git-dir="+repositoryPath, "symbolic-ref", "HEAD")
	if err != nil || head != CommittedPolicyRef {
		return fmt.Errorf("%w: admin repository %q HEAD is not %s", ErrProvisionDestination, repositoryPath, CommittedPolicyRef)
	}
	// A missing trusted ref means "not provisioned" for a tenant; because the
	// destination already exists it is refused rather than reseeded.
	result, err := provisioner.invoke(ctx, "--git-dir="+repositoryPath, "rev-parse", "--verify", "--quiet", CommittedPolicyRef)
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(result.Stdout))
	if result.ExitCode != 0 || commit == "" {
		return fmt.Errorf("%w: admin repository %q has no trusted ref %s", ErrProvisionDestination, repositoryPath, CommittedPolicyRef)
	}
	if !validPolicyCommit(commit) {
		return fmt.Errorf("%w: admin repository %q trusted ref is not an object id", ErrProvisionDestination, repositoryPath)
	}
	kind, err := provisioner.require(ctx, "--git-dir="+repositoryPath, "cat-file", "-t", commit)
	if err != nil || kind != "commit" {
		return fmt.Errorf("%w: admin repository %q trusted ref does not point at a commit", ErrProvisionDestination, repositoryPath)
	}
	result, err = provisioner.invoke(ctx, "--git-dir="+repositoryPath, "cat-file", "blob", commit+":"+CommittedPolicyFile)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%w: admin repository %q commit %s has no %s", ErrProvisionDestination, repositoryPath, commit, CommittedPolicyFile)
	}
	parsed, err := git.ParseConfig(result.Stdout)
	if err != nil {
		return fmt.Errorf("%w: admin repository %q committed policy does not parse: %v", ErrProvisionDestination, repositoryPath, err)
	}
	if err := requireAdminWriteOfPolicyRef(parsed); err != nil {
		return fmt.Errorf("%w: admin repository %q committed policy is not accepted: %v", ErrProvisionDestination, repositoryPath, err)
	}
	// The managed hooks are part of the provisioned state. A foreign or missing
	// hook is refused and never overwritten, because rewriting hooks on an
	// established repository would be a repair.
	for _, name := range []string{string(PhasePreReceive), string(PhasePostReceive)} {
		hookPath := filepath.Join(repositoryPath, "hooks", name)
		info, err := os.Lstat(hookPath)
		if err != nil {
			return fmt.Errorf("%w: admin repository %q has no managed %s hook", ErrProvisionDestination, repositoryPath, name)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: admin repository %q %s hook is not a regular file", ErrProvisionDestination, repositoryPath, name)
		}
		content, err := os.ReadFile(hookPath)
		if err != nil || !strings.Contains(string(content), managedMarker) {
			return fmt.Errorf("%w: admin repository %q has a foreign %s hook", ErrProvisionDestination, repositoryPath, name)
		}
	}
	// No update hook is installed by provisioning; an existing one is a
	// foreign mutation path and is refused, never removed.
	if _, err := os.Lstat(filepath.Join(repositoryPath, "hooks", "update")); err == nil {
		return fmt.Errorf("%w: admin repository %q has an update hook", ErrProvisionDestination, repositoryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%w: inspect admin repository %q update hook: %v", ErrProvisionDestination, repositoryPath, err)
	}
	return nil
}

// invokeStep runs the optional failure-injection seam before a step.
func (provisioner *adminProvisioner) invokeStep(ctx context.Context, step ProvisionStep) error {
	if provisioner.step == nil {
		return nil
	}
	return provisioner.step(ctx, step)
}

// invoke runs one native git command through the exact executor seam.
func (provisioner *adminProvisioner) invoke(ctx context.Context, args ...string) (git.CommandResult, error) {
	return provisioner.executor.Run(ctx, git.Command{Name: "git", Args: args})
}

// require runs one command and requires a zero exit code. Diagnostics are
// bounded to the command's own stderr; no credential is ever echoed.
func (provisioner *adminProvisioner) require(ctx context.Context, args ...string) (string, error) {
	result, err := provisioner.invoke(ctx, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), result.ExitCode,
			strings.TrimSpace(string(result.Stderr)))
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}

// prepareJournalDirectory creates the receive-journal directory before
// publication when it is absent and preserves every existing entry and mode.
func prepareJournalDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("receive-journal directory %q is a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("receive-journal path %q is not a directory", path)
		}
		return nil
	case os.IsNotExist(err):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create receive-journal directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("set receive-journal directory mode: %w", err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("flush receive-journal parent: %w", err)
		}
		return syncDirectory(path)
	default:
		return fmt.Errorf("inspect receive-journal directory %q: %w", path, err)
	}
}

// normalizeProvisionedTree sets the private modes without touching the process
// umask: every directory is 0700, every hook is 0700 and every other ordinary
// file is exactly 0600. Git writes its objects read-only (0444), so an explicit
// Chmod is required rather than a create-time mode.
func normalizeProvisionedTree(root string) error {
	hooksPrefix := filepath.Join(root, "hooks") + string(os.PathSeparator)
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect provisioned path %q: %w", path, err)
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o700); err != nil {
				return fmt.Errorf("set provisioned directory %q mode: %w", path, err)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("provisioned path %q is not an ordinary file", path)
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(path, hooksPrefix) {
			mode = 0o700
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("set provisioned file %q mode: %w", path, err)
		}
		return nil
	})
}

// syncProvisionedTree flushes every staged file, then every directory deepest
// first, so the complete tree is durable before the publication rename.
func syncProvisionedTree(root string) error {
	directories := make([]string, 0, 16)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open provisioned file %q: %w", path, err)
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return fmt.Errorf("flush provisioned file %q: %w", path, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close provisioned file %q: %w", path, closeErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := syncDirectory(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

// syncDirectory flushes one directory entry so a rename or a newly created
// entry survives a crash.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %q for flush: %w", path, err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("flush directory %q: %w", path, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory %q: %w", path, closeErr)
	}
	return nil
}
