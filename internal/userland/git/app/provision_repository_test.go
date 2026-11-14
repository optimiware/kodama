package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

const repositoryProvisionName = "shop/web"

// repositoryProvisionDefaultRules is the exact block KODAMA.md defines for a
// provisioned repository: read for every principal, work-branch writes for
// engineers and executions, execution writes to the default branch, and review
// approval for the default branch.
const repositoryProvisionDefaultRules = "\n" +
	"repo shop/web\n" +
	"    R = @all\n" +
	"    RW+ refs/heads/work/ = @engineer @execution\n" +
	"    RW refs/heads/main = @execution\n" +
	"    option approve.refs/heads/main = reviewer\n"

func repositoryProvisionConfig(layout provisionLayout, repository string) RepositoryProvisionConfig {
	return RepositoryProvisionConfig{
		TenantID:    provisionTenantID,
		StorageRoot: layout.storageRoot,
		Repository:  repository,
		Environment: provisionChildEnvironment(layout.home),
	}
}

func mustProvisionRepository(t *testing.T, config RepositoryProvisionConfig) RepositoryProvisionResult {
	t.Helper()
	result, err := ProvisionRepository(context.Background(), config)
	if err != nil {
		t.Fatalf("ProvisionRepository: %v", err)
	}
	return result
}

func repositoryProvisionPath(layout provisionLayout, repository string) string {
	return filepath.Join(layout.tenantDirectory(), filepath.FromSlash(repository))
}

func repositoryProvisionPolicy(t *testing.T, environment []string, adminPath string) string {
	t.Helper()
	return requireProvisionGit(t, environment,
		"--git-dir="+adminPath, "cat-file", "blob", native.CommittedPolicyRef+":"+native.CommittedPolicyFile)
}

func repositoryProvisionCommitCount(t *testing.T, environment []string, adminPath string) string {
	t.Helper()
	return strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+adminPath, "rev-list", "--count", native.CommittedPolicyRef))
}

// requireManagedRepositoryHooks proves the command installed the two managed
// hooks before the repository became reachable: no sample-only or hookless
// repository can pass this check, and no serve start or harness helper runs.
func requireManagedRepositoryHooks(t *testing.T, repositoryPath string) {
	t.Helper()
	for _, phase := range []string{"pre-receive", "post-receive"} {
		path := filepath.Join(repositoryPath, "hooks", phase)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("managed %s hook: %v", phase, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s hook mode = %04o; want 0700", phase, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s hook: %v", phase, err)
		}
		if !strings.Contains(string(data), "managed by kodama") {
			t.Fatalf("%s hook is not the managed hook: %q", phase, data)
		}
		if !strings.Contains(string(data), "git hook "+phase) {
			t.Fatalf("%s hook does not run the %s callback: %q", phase, phase, data)
		}
	}
	if _, err := os.Lstat(filepath.Join(repositoryPath, "hooks", "update")); !os.IsNotExist(err) {
		t.Fatalf("managed repository has an update hook: %v", err)
	}
}

// writeRepositoryProvisionPolicy appends one accepted admin policy commit on
// top of the provisioned trusted commit, modeling an Admin policy update.
func writeRepositoryProvisionPolicy(t *testing.T, environment []string, adminPath, policy string) string {
	t.Helper()
	parent := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef))
	directory := t.TempDir()
	file := filepath.Join(directory, "git.conf")
	if err := os.WriteFile(file, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	blob := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+adminPath, "hash-object", "-w", "--no-filters", file))
	index := filepath.Join(directory, "index")
	indexEnvironment := append(append([]string(nil), environment...), "GIT_INDEX_FILE="+index)
	requireProvisionGit(t, indexEnvironment, "--git-dir="+adminPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+",conf/git.conf")
	tree := strings.TrimSpace(requireProvisionGit(t, indexEnvironment,
		"--git-dir="+adminPath, "write-tree"))
	commit := strings.TrimSpace(requireProvisionGit(t, environment,
		"-c", "user.name=Kodama", "-c", "user.email=kodama@example.invalid",
		"--git-dir="+adminPath, "commit-tree", tree, "-p", parent, "-m", "policy update"))
	requireProvisionGit(t, environment, "--git-dir="+adminPath,
		"update-ref", native.CommittedPolicyRef, commit, parent)
	return commit
}

// TestProvisionRepositoryWritesDefaultRulesOnce pins the first provisioning:
// the bare repository the service expects appears at the tenant-scoped path and
// the documented default rules are committed exactly once by the same service
// identity tenant provisioning uses. A second run converges without another
// commit or repository rewrite.
func TestProvisionRepositoryWritesDefaultRulesOnce(t *testing.T) {
	layout := newProvisionLayout(t)
	tenantConfig := layout.config()
	mustProvisionTenant(t, tenantConfig)
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	adminPath := layout.repositoryPath()

	first := mustProvisionRepository(t, config)
	wantPath := repositoryProvisionPath(layout, repositoryProvisionName)
	if !first.Created {
		t.Fatal("Created = false on first repository provisioning; want true")
	}
	if first.RepositoryPath != wantPath {
		t.Fatalf("RepositoryPath = %q; want %q", first.RepositoryPath, wantPath)
	}
	if bare := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+wantPath, "rev-parse", "--is-bare-repository")); bare != "true" {
		t.Fatalf("repository is bare = %q; want true", bare)
	}
	if head := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+wantPath, "symbolic-ref", "HEAD")); head != native.CommittedPolicyRef {
		t.Fatalf("repository HEAD = %q; want %q", head, native.CommittedPolicyRef)
	}
	requireManagedRepositoryHooks(t, wantPath)

	wantPolicy := adminInitialPolicy + repositoryProvisionDefaultRules
	policy := repositoryProvisionPolicy(t, config.Environment, adminPath)
	if policy != wantPolicy {
		t.Fatalf("committed policy = %q; want %q", policy, wantPolicy)
	}
	if count := strings.Count(policy, "repo "+repositoryProvisionName); count != 1 {
		t.Fatalf("default rules appear %d times; want exactly once", count)
	}
	if count := repositoryProvisionCommitCount(t, config.Environment, adminPath); count != "2" {
		t.Fatalf("policy history length = %s; want the initial commit plus one repository commit", count)
	}

	body := requireProvisionGit(t, config.Environment,
		"--git-dir="+adminPath, "cat-file", "-p", native.CommittedPolicyRef)
	for _, identity := range []string{
		"author Kodama Provisioning <kodama@example.invalid>",
		"committer Kodama Provisioning <kodama@example.invalid>",
	} {
		if !strings.Contains(body, identity) {
			t.Fatalf("repository policy commit does not carry %q: %q", identity, body)
		}
	}
	if !strings.Contains(body, "parent ") {
		t.Fatal("repository policy commit is parentless; want it appended to the trusted history")
	}

	second := mustProvisionRepository(t, config)
	if second.Created {
		t.Fatal("Created = true on an existing valid repository; want false")
	}
	if second.RepositoryPath != wantPath {
		t.Fatalf("second RepositoryPath = %q; want %q", second.RepositoryPath, wantPath)
	}
	if observed := repositoryProvisionPolicy(t, config.Environment, adminPath); observed != wantPolicy {
		t.Fatalf("second run changed the policy to %q; want %q", observed, wantPolicy)
	}
	if count := repositoryProvisionCommitCount(t, config.Environment, adminPath); count != "2" {
		t.Fatalf("policy history after the second run = %s; want 2", count)
	}
}

// TestProvisionRepositoryNeverWidensExistingPolicy pins convergence: a policy
// that already declares the repository is preserved byte for byte, so the
// command never duplicates the block or grants the default rules on top of
// rules an Admin already restricted.
func TestProvisionRepositoryNeverWidensExistingPolicy(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	adminPath := layout.repositoryPath()
	restrictedPolicy := "repo admin\n" +
		"    RW = @admin\n" +
		"\n" +
		"repo shop/web\n" +
		"    R = @all\n" +
		"    RW refs/heads/work/ = @engineer\n"
	restrictedCommit := writeRepositoryProvisionPolicy(t, config.Environment, adminPath, restrictedPolicy)

	result := mustProvisionRepository(t, config)
	if !result.Created {
		t.Fatal("Created = false; the repository itself is still new")
	}
	if observed := repositoryProvisionPolicy(t, config.Environment, adminPath); observed != restrictedPolicy {
		t.Fatalf("existing policy was rewritten to %q; want %q", observed, restrictedPolicy)
	}
	head := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef))
	if head != restrictedCommit {
		t.Fatalf("trusted ref = %q; want the preserved commit %q", head, restrictedCommit)
	}
	if count := repositoryProvisionCommitCount(t, config.Environment, adminPath); count != "2" {
		t.Fatalf("policy history length = %s; want the preserved history", count)
	}

	parsed, err := git.ParseConfig([]byte(restrictedPolicy))
	if err != nil {
		t.Fatalf("parse restricted policy: %v", err)
	}
	policy, ok := parsed.Policy(repositoryProvisionName)
	if !ok {
		t.Fatalf("restricted policy lost the %q repository", repositoryProvisionName)
	}
	decision, err := policy.Authorize(git.AccessRequest{
		Identity: git.Identity{Execution: true},
		Ref:      native.CommittedPolicyRef,
		Update:   git.UpdateCreate,
	})
	if err != nil {
		t.Fatalf("evaluate restricted policy: %v", err)
	}
	if decision != git.DecisionDeny {
		t.Fatalf("provisioning widened execution writes to the default branch: %s", decision)
	}
	if _, guarded := policy.ApprovalFor(native.CommittedPolicyRef); guarded {
		t.Fatal("provisioning added the default approval rule over existing rules")
	}
}

// oversizedRepositoryProvisionPolicy builds a valid accepted policy just below
// the committed-policy size bound, so appending one default repository block
// pushes the candidate over it.
func oversizedRepositoryProvisionPolicy(t *testing.T) string {
	t.Helper()
	target := native.MaxCommittedPolicyBytes - 100
	var builder strings.Builder
	builder.WriteString(adminInitialPolicy)
	line := "# padding keeps the accepted policy below the committed-policy size bound\n"
	for builder.Len()+len(line) <= target {
		builder.WriteString(line)
	}
	if remaining := target - builder.Len(); remaining > 0 {
		builder.WriteString(strings.Repeat("#", remaining-1))
		builder.WriteString("\n")
	}
	policy := builder.String()
	if len(policy) != target {
		t.Fatalf("oversized accepted policy length = %d; want %d", len(policy), target)
	}
	return policy
}

// TestProvisionRepositoryRefusesOversizedPolicyWithoutMovingTrustedRef pins the
// ordering of the trusted-ref write: the candidate policy (the accepted policy
// plus the default block) is size-bounded, parsed and admin-write-checked
// BEFORE the compare-and-set ref update. An oversized candidate must leave the
// trusted admin ref exactly where it was, with no policy commit reachable from
// it.
func TestProvisionRepositoryRefusesOversizedPolicyWithoutMovingTrustedRef(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	adminPath := layout.repositoryPath()
	accepted := oversizedRepositoryProvisionPolicy(t)
	writeRepositoryProvisionPolicy(t, config.Environment, adminPath, accepted)

	beforeHead := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef))
	beforeCount := repositoryProvisionCommitCount(t, config.Environment, adminPath)

	_, err := ProvisionRepository(context.Background(), config)
	if !errors.Is(err, native.ErrCommittedPolicyTooLarge) {
		t.Fatalf("ProvisionRepository error = %v; want ErrCommittedPolicyTooLarge", err)
	}

	afterHead := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef))
	if afterHead != beforeHead {
		t.Fatalf("trusted admin ref = %q after the refused candidate; want %q: the ref moved before the candidate was checked", afterHead, beforeHead)
	}
	if afterCount := repositoryProvisionCommitCount(t, config.Environment, adminPath); afterCount != beforeCount {
		t.Fatalf("policy history length = %s after the refused candidate; want %s", afterCount, beforeCount)
	}
	if observed := repositoryProvisionPolicy(t, config.Environment, adminPath); observed != accepted {
		t.Fatalf("trusted policy changed after the refused candidate: %d bytes; want the accepted %d bytes", len(observed), len(accepted))
	}
	if strings.Contains(repositoryProvisionPolicy(t, config.Environment, adminPath), "repo "+repositoryProvisionName) {
		t.Fatal("the refused candidate's default block became the trusted policy")
	}
}

// TestProvisionRepositoryAdoptsExistingBareRepository pins idempotence over a
// repository already created exactly as the service expects: it is preserved
// and only the missing policy is committed.
func TestProvisionRepositoryAdoptsExistingBareRepository(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	repositoryPath := repositoryProvisionPath(layout, repositoryProvisionName)
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{
		Path: repositoryPath,
		Env:  config.Environment,
	}); err != nil {
		t.Fatalf("InitBareRepository: %v", err)
	}

	result := mustProvisionRepository(t, config)
	if result.Created {
		t.Fatal("Created = true for an existing bare repository; want false")
	}
	if result.RepositoryPath != repositoryPath {
		t.Fatalf("RepositoryPath = %q; want %q", result.RepositoryPath, repositoryPath)
	}
	requireManagedRepositoryHooks(t, repositoryPath)
	wantPolicy := adminInitialPolicy + repositoryProvisionDefaultRules
	if observed := repositoryProvisionPolicy(t, config.Environment, layout.repositoryPath()); observed != wantPolicy {
		t.Fatalf("committed policy = %q; want %q", observed, wantPolicy)
	}

	// An update hook is a foreign mutation path: a repository carrying one is
	// not adopted.
	if err := os.WriteFile(filepath.Join(repositoryPath, "hooks", "update"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write update hook: %v", err)
	}
	if _, err := ProvisionRepository(context.Background(), config); !errors.Is(err, native.ErrRepositoryProvisionDestination) {
		t.Fatalf("error = %v with an update hook; want ErrRepositoryProvisionDestination", err)
	}
}

// TestProvisionRepositoryInitializesExistingEmptyDirectory pins that the only
// acceptable pre-existing destination besides a valid bare repository is an
// empty directory: it is initialized in place.
func TestProvisionRepositoryInitializesExistingEmptyDirectory(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	repositoryPath := repositoryProvisionPath(layout, repositoryProvisionName)
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatalf("create empty destination: %v", err)
	}
	result := mustProvisionRepository(t, config)
	if !result.Created {
		t.Fatal("Created = false for an empty destination; want true")
	}
	if bare := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-parse", "--is-bare-repository")); bare != "true" {
		t.Fatalf("repository is bare = %q; want true", bare)
	}
	wantPolicy := adminInitialPolicy + repositoryProvisionDefaultRules
	if observed := repositoryProvisionPolicy(t, config.Environment, layout.repositoryPath()); observed != wantPolicy {
		t.Fatalf("committed policy = %q; want %q", observed, wantPolicy)
	}
}

// TestProvisionRepositoryReportsCanonicalName pins that the printed mapping
// uses the canonical tenant-relative name, not the operator's spelling.
func TestProvisionRepositoryReportsCanonicalName(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, "./shop/web")
	result := mustProvisionRepository(t, config)
	if result.Repository != repositoryProvisionName {
		t.Fatalf("Repository = %q; want %q", result.Repository, repositoryProvisionName)
	}
	if result.RepositoryPath != repositoryProvisionPath(layout, repositoryProvisionName) {
		t.Fatalf("RepositoryPath = %q; want %q", result.RepositoryPath, repositoryProvisionPath(layout, repositoryProvisionName))
	}
	policy := repositoryProvisionPolicy(t, config.Environment, layout.repositoryPath())
	if !strings.Contains(policy, "repo "+repositoryProvisionName+"\n") {
		t.Fatalf("policy does not declare the canonical repository: %q", policy)
	}
	if strings.Contains(policy, "repo ./shop/web") {
		t.Fatalf("policy declared the operator spelling: %q", policy)
	}
}

// TestProvisionRepositoryRefusesNestedDestination pins that a repository name
// that would live inside an existing bare repository is refused before any
// directory or policy change.
func TestProvisionRepositoryRefusesNestedDestination(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	parent := mustProvisionRepository(t, repositoryProvisionConfig(layout, "shop"))
	if !parent.Created {
		t.Fatal("parent repository Created = false")
	}
	adminPath := layout.repositoryPath()
	head := strings.TrimSpace(requireProvisionGit(t, repositoryProvisionConfig(layout, "shop").Environment,
		"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef))
	policy := repositoryProvisionPolicy(t, repositoryProvisionConfig(layout, "shop").Environment, adminPath)

	for _, repository := range []string{"shop/hooks/nested", "shop/web"} {
		t.Run(strings.ReplaceAll(repository, "/", "_"), func(t *testing.T) {
			config := repositoryProvisionConfig(layout, repository)
			_, err := ProvisionRepository(context.Background(), config)
			if !errors.Is(err, native.ErrRepositoryProvisionDestination) {
				t.Fatalf("error = %v; want ErrRepositoryProvisionDestination", err)
			}
			if _, statErr := os.Stat(filepath.Join(layout.tenantDirectory(), filepath.FromSlash(repository))); !os.IsNotExist(statErr) {
				t.Fatalf("nested repository %q exists after refusal: %v", repository, statErr)
			}
			if observed := repositoryProvisionPolicy(t, config.Environment, adminPath); observed != policy {
				t.Fatalf("trusted policy changed to %q; want %q", observed, policy)
			}
			if observed := strings.TrimSpace(requireProvisionGit(t, config.Environment,
				"--git-dir="+adminPath, "rev-parse", native.CommittedPolicyRef)); observed != head {
				t.Fatalf("trusted ref = %q; want %q", observed, head)
			}
		})
	}
}

// TestProvisionRepositoryRefusesNonRepositoryDestination pins that an existing
// non-empty directory that is not the provisioned bare repository is refused
// and left untouched; the trusted policy is never changed.
func TestProvisionRepositoryRefusesNonRepositoryDestination(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	repositoryPath := repositoryProvisionPath(layout, repositoryProvisionName)
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatalf("create foreign destination: %v", err)
	}
	marker := filepath.Join(repositoryPath, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	_, err := ProvisionRepository(context.Background(), config)
	if !errors.Is(err, native.ErrRepositoryProvisionDestination) {
		t.Fatalf("error = %v; want ErrRepositoryProvisionDestination", err)
	}
	data, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("read marker: %v", readErr)
	}
	if string(data) != "keep\n" {
		t.Fatalf("foreign destination was modified: %q", data)
	}
	if observed := repositoryProvisionPolicy(t, config.Environment, layout.repositoryPath()); observed != adminInitialPolicy {
		t.Fatalf("trusted policy changed to %q; want the initial policy", observed)
	}
}

// TestProvisionRepositoryRefusesInvalidNamesAndTargets pins name validation and
// containment: traversal, absolute names, uppercase, the tenant admin
// repository and symlinked destinations are refused before any write.
func TestProvisionRepositoryRefusesInvalidNamesAndTargets(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	adminPath := layout.repositoryPath()

	for _, repository := range []string{
		"../escape",
		"shop/../../escape",
		"/absolute",
		"Shop/Web",
		"shop//web",
		"shop/web/",
		".",
		"admin",
	} {
		t.Run(strings.ReplaceAll(repository, "/", "_"), func(t *testing.T) {
			config := repositoryProvisionConfig(layout, repository)
			_, err := ProvisionRepository(context.Background(), config)
			if err == nil {
				t.Fatalf("repository %q was accepted", repository)
			}
			if repository == "admin" {
				if !errors.Is(err, native.ErrRepositoryProvisionDestination) {
					t.Fatalf("error = %v; want ErrRepositoryProvisionDestination", err)
				}
			} else if !errors.Is(err, git.ErrInvalidRepository) {
				t.Fatalf("error = %v; want ErrInvalidRepository", err)
			}
			if observed := repositoryProvisionPolicy(t, config.Environment, adminPath); observed != adminInitialPolicy {
				t.Fatalf("trusted policy changed to %q; want the initial policy", observed)
			}
		})
	}

	outside := filepath.Join(layout.root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create outside target: %v", err)
	}
	parent := filepath.Join(layout.tenantDirectory(), "symlink")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("create symlink parent: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "web")); err != nil {
		t.Fatalf("symlink destination: %v", err)
	}
	config := repositoryProvisionConfig(layout, "symlink/web")
	_, err := ProvisionRepository(context.Background(), config)
	if !errors.Is(err, git.ErrInvalidRepository) {
		t.Fatalf("symlinked destination error = %v; want ErrInvalidRepository", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside target: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("outside target was written with %d entries", len(entries))
	}
}

// TestProvisionRepositoryRequiresProvisionedAdmin pins that a tenant without
// its trusted admin repository is refused before any repository is created.
func TestProvisionRepositoryRequiresProvisionedAdmin(t *testing.T) {
	layout := newProvisionLayout(t)
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	_, err := ProvisionRepository(context.Background(), config)
	if err == nil {
		t.Fatal("repository provisioning succeeded without a provisioned admin repository")
	}
	if _, statErr := os.Stat(layout.repositoryPath()); !os.IsNotExist(statErr) {
		t.Fatalf("admin repository exists after a refused provisioning: %v", statErr)
	}
	if _, statErr := os.Stat(repositoryProvisionPath(layout, repositoryProvisionName)); !os.IsNotExist(statErr) {
		t.Fatalf("repository exists after a refused provisioning: %v", statErr)
	}
}

// TestProvisionRepositoryRejectsUnknownTenant pins tenant validation.
func TestProvisionRepositoryRejectsUnknownTenant(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, repositoryProvisionName)
	config.TenantID = id.TenantID("../escape")
	_, err := ProvisionRepository(context.Background(), config)
	if !errors.Is(err, id.ErrInvalidID) {
		t.Fatalf("error = %v; want ErrInvalidID", err)
	}
}
