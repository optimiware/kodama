//go:build integration

package native_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// provisionedEngineerPolicy is a valid accepted policy that keeps the admin
// write rule while granting the engineer role writes elsewhere. It is used to
// model an Admin policy update after provisioning.
const provisionedEngineerPolicy = `
repo admin
    RW = @admin

repo shop/web
    R = @all
    RW refs/heads/ = @engineer
`

// provisionedPolicyWithoutAdminWrite parses but leaves the trusted policy ref
// unwritable by @admin, so the pre-receive validator must refuse it.
const provisionedPolicyWithoutAdminWrite = `
repo admin
    R = @admin

repo shop/web
    R = @all
    RW refs/heads/ = @engineer
`

// provisionAdmin creates the tenant's admin repository through the operator
// command's app boundary, with the real host git and the fixture's paths.
func provisionAdmin(t *testing.T, harness *nativefix.Harness) gitapp.ProvisionResult {
	t.Helper()
	result, err := gitapp.ProvisionTenant(context.Background(), gitapp.ProvisionConfig{
		TenantID:         harness.Tenant,
		StorageRoot:      harness.StorageRoot,
		JournalDirectory: harness.JournalDir,
		HookBinary:       harness.BinaryPath,
		Environment:      harness.Environment(),
	})
	if err != nil {
		t.Fatalf("ProvisionTenant(%s): %v", harness.Tenant, err)
	}
	return result
}

// harnessGitOutput runs one fixture git command in a repository and returns
// stdout or fails.
func harnessGitOutput(t *testing.T, harness *nativefix.Harness, repositoryPath string, args ...string) string {
	t.Helper()
	stdout, stderr, code := harness.Git(t, append([]string{"-C", repositoryPath}, args...)...)
	if code != 0 {
		t.Fatalf("git %v exited %d: %s", args, code, stderr)
	}
	return stdout
}

// provisionCLI runs the real `kodama` binary with the exact environment and
// returns stdout, stderr and the exit code.
func provisionCLI(t *testing.T, environment []string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(kodamaBinary, args...)
	command.Env = environment
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	t.Fatalf("run kodama %v: %v", args, err)
	return "", "", -1
}

// provisionBoundaryGit runs one host git command with the exact environment and
// requires a zero exit code.
func provisionBoundaryGit(t *testing.T, environment []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Env = environment
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return stdout.String()
}

// provisionBoundaryAssertionEnvironment pins the assertion commands to an
// innocuous home and /dev/null configuration, so the assertions themselves
// never read a hostile inherited configuration.
func provisionBoundaryAssertionEnvironment(t *testing.T) []string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "assert-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create assertion home: %v", err)
	}
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
}

// TestTenantProvisionCLIIsolatesHostileHostConfiguration pins the operator
// command end to end: the real binary run from a hostile shell (global config at
// HOME/.gitconfig and XDG_CONFIG_HOME/git/config, a system config selected by
// GIT_CONFIG_SYSTEM, a hostile init template and hooks path, and inherited GIT_*
// variables) still creates a repository with none of the hostile trace.
func TestTenantProvisionCLIIsolatesHostileHostConfiguration(t *testing.T) {
	root := t.TempDir()
	hostile := filepath.Join(root, "hostile")
	home := filepath.Join(hostile, "home")
	xdgConfigHome := filepath.Join(hostile, "xdg")
	templateDirectory := filepath.Join(hostile, "template")
	templateHooks := filepath.Join(templateDirectory, "hooks")
	hooksPath := filepath.Join(hostile, "hooks")
	marker := filepath.Join(hostile, "hostile-hook-ran")
	for _, directory := range []string{
		home,
		filepath.Join(xdgConfigHome, "git"),
		templateHooks,
		hooksPath,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create hostile directory %q: %v", directory, err)
		}
	}
	hostileHook := "#!/bin/sh\ntouch \"" + marker + "\"\nexit 1\n"
	for _, directory := range []string{templateHooks, hooksPath} {
		if err := os.WriteFile(filepath.Join(directory, "pre-receive"), []byte(hostileHook), 0o755); err != nil {
			t.Fatalf("write hostile hook: %v", err)
		}
	}
	hostileConfig := "[core]\n" +
		"\tlogAllRefUpdates = true\n" +
		"\thooksPath = " + hooksPath + "\n" +
		"[user]\n" +
		"\tname = Hostile Person\n" +
		"\temail = hostile@evil.example\n" +
		"[init]\n" +
		"\ttemplateDir = " + templateDirectory + "\n" +
		"\tdefaultBranch = hostile\n" +
		"\tdefaultRefFormat = reftable\n" +
		"[receive]\n" +
		"\tdenyDeleteCurrent = refuse\n" +
		"[safe]\n" +
		"\tdirectory = *\n"
	globalConfig := filepath.Join(home, ".gitconfig")
	xdgConfig := filepath.Join(xdgConfigHome, "git", "config")
	systemConfig := filepath.Join(hostile, "system-gitconfig")
	for _, path := range []string{globalConfig, xdgConfig, systemConfig} {
		if err := os.WriteFile(path, []byte(hostileConfig), 0o600); err != nil {
			t.Fatalf("write hostile config %q: %v", path, err)
		}
	}
	environment := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdgConfigHome,
		"GIT_CONFIG_GLOBAL=" + globalConfig,
		"GIT_CONFIG_SYSTEM=" + systemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_AUTHOR_NAME=Hostile Person",
		"GIT_AUTHOR_EMAIL=hostile@evil.example",
		"GIT_COMMITTER_NAME=Hostile Person",
		"GIT_COMMITTER_EMAIL=hostile@evil.example",
		"PATH=" + os.Getenv("PATH"),
	}

	tenant := "tenant-hostile-cli"
	storageRoot := filepath.Join(root, "repos")
	journalDirectory := filepath.Join(root, "journal")
	stdout, stderr, code := provisionCLI(t, environment,
		"git", "provision-tenant",
		"--tenant", tenant,
		"--storage-root", storageRoot,
		"--journal-dir", journalDirectory,
	)
	if code != 0 {
		t.Fatalf("kodama git provision-tenant exited %d: %s", code, stderr)
	}
	repositoryPath := filepath.Join(storageRoot, tenant, "admin")
	if strings.TrimSpace(stdout) != repositoryPath {
		t.Fatalf("stdout = %q; want the admin repository path %q", strings.TrimSpace(stdout), repositoryPath)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("a hostile hook ran during CLI provisioning: %v", err)
	}

	markers := []string{"Hostile Person", "hostile@evil.example", marker}
	err := filepath.WalkDir(repositoryPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "logs" {
				t.Errorf("hostile core.logAllRefUpdates created a reflog directory %q", path)
			}
			if entry.Name() == "reftable" {
				t.Errorf("hostile init.defaultRefFormat created a reftable store %q", path)
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, hostileValue := range markers {
			if bytes.Contains(data, []byte(hostileValue)) {
				t.Errorf("hostile value %q appears in %q", hostileValue, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk provisioned repository: %v", err)
	}

	assertions := provisionBoundaryAssertionEnvironment(t)
	if format := strings.TrimSpace(provisionBoundaryGit(t, assertions,
		"--git-dir="+repositoryPath, "rev-parse", "--show-ref-format")); format != "files" {
		t.Fatalf("hostile configuration selected ref backend %q; want files", format)
	}
	if head := strings.TrimSpace(provisionBoundaryGit(t, assertions,
		"--git-dir="+repositoryPath, "symbolic-ref", "HEAD")); head != "refs/heads/main" {
		t.Fatalf("hostile configuration selected branch %q; want refs/heads/main", head)
	}
	hooks, err := os.ReadDir(filepath.Join(repositoryPath, "hooks"))
	if err != nil {
		t.Fatalf("read installed hooks: %v", err)
	}
	var names []string
	for _, hook := range hooks {
		names = append(names, hook.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "post-receive,pre-receive" {
		t.Fatalf("installed hooks = %v; want exactly post-receive,pre-receive", names)
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(repositoryPath, "hooks", name))
		if err != nil {
			t.Fatalf("read installed hook %q: %v", name, err)
		}
		if !strings.Contains(string(data), "managed by kodama") {
			t.Fatalf("hook %q is not the managed hook: %q", name, data)
		}
	}
	body := provisionBoundaryGit(t, assertions, "--git-dir="+repositoryPath, "cat-file", "-p", "HEAD")
	for _, identity := range []string{
		"author Kodama Provisioning <kodama@example.invalid>",
		"committer Kodama Provisioning <kodama@example.invalid>",
	} {
		if !strings.Contains(body, identity) {
			t.Fatalf("initial commit does not carry the explicit identity %q: %q", identity, body)
		}
	}
}

// TestTenantProvisionCLISecondRunReportsExistingRepository pins the operator
// command's idempotence: the second run over a valid repository exits 0 and
// prints the same admin path without reseeding it.
func TestTenantProvisionCLISecondRunReportsExistingRepository(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create home: %v", err)
	}
	environment := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
	tenant := "tenant-idempotent-cli"
	storageRoot := filepath.Join(root, "repos")
	args := []string{
		"git", "provision-tenant",
		"--tenant", tenant,
		"--storage-root", storageRoot,
		"--journal-dir", filepath.Join(root, "journal"),
	}
	firstStdout, firstStderr, firstCode := provisionCLI(t, environment, args...)
	if firstCode != 0 {
		t.Fatalf("first run exited %d: %s", firstCode, firstStderr)
	}
	secondStdout, secondStderr, secondCode := provisionCLI(t, environment, args...)
	if secondCode != 0 {
		t.Fatalf("second run (existing repository) exited %d: %s", secondCode, secondStderr)
	}
	if secondStdout != firstStdout {
		t.Fatalf("second stdout = %q; want the first admin path %q", secondStdout, firstStdout)
	}
	repositoryPath := filepath.Join(storageRoot, tenant, nativefix.AdminRepository)
	if strings.TrimSpace(secondStdout) != repositoryPath {
		t.Fatalf("stdout = %q; want %q", strings.TrimSpace(secondStdout), repositoryPath)
	}
	count := strings.TrimSpace(provisionBoundaryGit(t, environment,
		"--git-dir="+repositoryPath, "rev-list", "--count", "HEAD"))
	if count != "1" {
		t.Fatalf("history after the second run = %s; want the single provisioned commit", count)
	}
}

// TestTenantProvisionServesAdminPolicyPushes pins the first served policy flow:
// the committed initial policy decides the first remote policy push, an
// invalid proposal is refused, and an engineer cannot write.
func TestTenantProvisionServesAdminPolicyPushes(t *testing.T) {
	harness := newHarness(t, nativefix.Options{SkipProvision: true})
	result := provisionAdmin(t, harness)
	if !result.Created {
		t.Fatal("first provisioning Created = false; want true")
	}
	initial := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
	if initial == "" {
		t.Fatal("provisioned admin repository has no refs/heads/main")
	}
	if entries := harness.JournalEntries(t); len(entries) != 0 {
		t.Fatalf("provisioning wrote %d journal entries; want none for the local seed", len(entries))
	}

	admin := cloneAdmin(t, harness, "admin-provisioned-accepted")
	writeAdminPolicy(t, admin, provisionedEngineerPolicy)
	accepted := commitAdminPolicy(t, harness, admin, "accepted policy update")
	requireAdminPush(t, harness, admin)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("accepted admin fast-forward = %q; want %q", observed, accepted)
	}

	// A proposal without the admin write rule is refused by the committed
	// policy's pre-receive validator.
	writeAdminPolicy(t, admin, provisionedPolicyWithoutAdminWrite)
	commitAdminPolicy(t, harness, admin, "invalid policy update")
	stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("refused policy push moved main to %q; want %q", observed, accepted)
	}

	// The committed policy grants the engineer role nothing on admin.
	if _, stderr, code := harness.GitIn(t, admin, "reset", "--hard", "origin/main"); code != 0 {
		t.Fatalf("reset admin main: %s", stderr)
	}
	engineerCommit := harness.Commit(t, admin, "engineer attempt")
	stderr, code = harness.Push(t, admin, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("engineer push moved main to %q; want %q", observed, accepted)
	}
	if engineerCommit == accepted {
		t.Fatal("fixture commits are identical")
	}
}

// TestTenantProvisionRefusesRewindAndDelete pins that the provisioned admin
// policy grants only create and fast-forward: a rewind and a delete are refused
// and the ref is unchanged.
func TestTenantProvisionRefusesRewindAndDelete(t *testing.T) {
	harness := newHarness(t, nativefix.Options{SkipProvision: true})
	provisionAdmin(t, harness)
	admin := cloneAdmin(t, harness, "admin-provisioned-history")
	base := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")

	writeAdminPolicy(t, admin, provisionedEngineerPolicy)
	next := commitAdminPolicy(t, harness, admin, "second accepted policy")
	requireAdminPush(t, harness, admin)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != next {
		t.Fatalf("admin fast-forward = %q; want %q", observed, next)
	}

	if _, stderr, code := harness.GitIn(t, admin, "reset", "--hard", base); code != 0 {
		t.Fatalf("reset admin rewind: %s", stderr)
	}
	stderr, code := harness.Push(t, admin, harness.AdminToken, "--force", "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != next {
		t.Fatalf("refused rewind moved main to %q; want %q", observed, next)
	}

	stderr, code = harness.Push(t, admin, harness.AdminToken, "origin", ":refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != next {
		t.Fatalf("refused delete moved main to %q; want %q", observed, next)
	}
}

// TestTenantProvisionSecondCallPreservesRepository pins idempotency after an
// accepted Admin policy update: Created=false and refs, policy, config and
// hooks are byte-identical.
func TestTenantProvisionSecondCallPreservesRepository(t *testing.T) {
	harness := newHarness(t, nativefix.Options{SkipProvision: true})
	first := provisionAdmin(t, harness)
	if !first.Created {
		t.Fatal("first provisioning Created = false; want true")
	}
	admin := cloneAdmin(t, harness, "admin-provisioned-preserve")
	writeAdminPolicy(t, admin, provisionedEngineerPolicy)
	updated := commitAdminPolicy(t, harness, admin, "accepted policy update")
	requireAdminPush(t, harness, admin)

	repositoryPath := harness.RepositoryPath(nativefix.AdminRepository)
	refsBefore := harnessGitOutput(t, harness, repositoryPath, "for-each-ref", "--format=%(refname) %(objectname)")
	configBefore, err := os.ReadFile(filepath.Join(repositoryPath, "config"))
	if err != nil {
		t.Fatalf("read repository config: %v", err)
	}
	hookBefore, err := os.ReadFile(filepath.Join(repositoryPath, "hooks", "pre-receive"))
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}

	second := provisionAdmin(t, harness)
	if second.Created {
		t.Fatal("second provisioning Created = true; want false")
	}
	if refsAfter := harnessGitOutput(t, harness, repositoryPath, "for-each-ref", "--format=%(refname) %(objectname)"); refsAfter != refsBefore {
		t.Fatalf("second provisioning changed refs:\nbefore %s\nafter  %s", refsBefore, refsAfter)
	}
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != updated {
		t.Fatalf("admin policy ref = %q; want the preserved update %q", observed, updated)
	}
	blob := harnessGitOutput(t, harness, repositoryPath, "cat-file", "blob", "HEAD:conf/git.conf")
	if strings.TrimSpace(blob) != strings.TrimSpace(provisionedEngineerPolicy) {
		t.Fatalf("preserved policy = %q; want %q", blob, provisionedEngineerPolicy)
	}
	configAfter, err := os.ReadFile(filepath.Join(repositoryPath, "config"))
	if err != nil {
		t.Fatalf("read preserved repository config: %v", err)
	}
	if string(configAfter) != string(configBefore) {
		t.Fatal("second provisioning rewrote the repository config")
	}
	hookAfter, err := os.ReadFile(filepath.Join(repositoryPath, "hooks", "pre-receive"))
	if err != nil {
		t.Fatalf("read preserved hook: %v", err)
	}
	if string(hookAfter) != string(hookBefore) {
		t.Fatal("second provisioning rewrote the installed hook")
	}
}
