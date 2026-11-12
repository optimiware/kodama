package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

const (
	provisionTenantID          = id.TenantID("tenant-provision")
	provisionHelperEnvironment = "KODAMA_PROVISION_HELPER"
	adminInitialPolicy         = "repo admin\n    RW = @admin\n"
)

// provisionLayout is one isolated provisioning target.
type provisionLayout struct {
	root             string
	storageRoot      string
	journalDirectory string
	home             string
}

func newProvisionLayout(t *testing.T) provisionLayout {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("create provision home: %v", err)
	}
	return provisionLayout{
		root:             root,
		storageRoot:      filepath.Join(root, "repos"),
		journalDirectory: filepath.Join(root, "journal"),
		home:             home,
	}
}

func (layout provisionLayout) config() ProvisionConfig {
	return ProvisionConfig{
		TenantID:         provisionTenantID,
		StorageRoot:      layout.storageRoot,
		JournalDirectory: layout.journalDirectory,
		Environment:      provisionChildEnvironment(layout.home),
	}
}

func (layout provisionLayout) tenantDirectory() string {
	return filepath.Join(layout.storageRoot, string(provisionTenantID))
}

func (layout provisionLayout) repositoryPath() string {
	return filepath.Join(layout.tenantDirectory(), native.AdminRepository)
}

// provisionChildEnvironment is the explicit, isolated child environment the
// tests hand to provisioning. The implementation must add its own isolation so
// a hostile inherited environment cannot change the result.
func provisionChildEnvironment(home string) []string {
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
}

// versionProbeExecutor scripts the version probe and records every command.
type versionProbeExecutor struct {
	mutex    sync.Mutex
	commands []git.Command
	version  string
}

func (executor *versionProbeExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	executor.mutex.Lock()
	executor.commands = append(executor.commands, command)
	executor.mutex.Unlock()
	if len(command.Args) == 1 && command.Args[0] == "--version" {
		return git.CommandResult{Stdout: []byte(executor.version)}, nil
	}
	return git.CommandResult{}, fmt.Errorf("unexpected command %v", command.Args)
}

func (executor *versionProbeExecutor) recorded() []git.Command {
	executor.mutex.Lock()
	defer executor.mutex.Unlock()
	return append([]git.Command(nil), executor.commands...)
}

// provisionGitRaw runs one host git command outside any repository.
func provisionGitRaw(t *testing.T, environment []string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command("git", args...)
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
	t.Fatalf("git %v: %v", args, err)
	return "", "", -1
}

func requireProvisionGit(t *testing.T, environment []string, args ...string) string {
	t.Helper()
	stdout, stderr, code := provisionGitRaw(t, environment, args...)
	if code != 0 {
		t.Fatalf("git %v exited %d: %s", args, code, stderr)
	}
	return stdout
}

func mustProvisionTenant(t *testing.T, config ProvisionConfig) ProvisionResult {
	t.Helper()
	result, err := ProvisionTenant(context.Background(), config)
	if err != nil {
		t.Fatalf("ProvisionTenant: %v", err)
	}
	return result
}

func requireDirectoryMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat directory %q: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%q is not a directory", path)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("directory %q mode = %04o; want %04o", path, got, want)
	}
}

func readHookFiles(t *testing.T, repositoryPath string) map[string]string {
	t.Helper()
	hooks := map[string]string{}
	for _, name := range []string{"pre-receive", "post-receive", "update"} {
		data, err := os.ReadFile(filepath.Join(repositoryPath, "hooks", name))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read hook %q: %v", name, err)
		}
		hooks[name] = string(data)
	}
	return hooks
}

// writeAdminPolicyCommit writes one parentless-agnostic policy commit through
// bounded plumbing and points refs/heads/main at it. It models an accepted
// Admin policy update performed directly on the repository.
func writeAdminPolicyCommit(t *testing.T, environment []string, repositoryPath, policy string) string {
	t.Helper()
	directory := t.TempDir()
	file := filepath.Join(directory, "git.conf")
	if err := os.WriteFile(file, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}
	blob := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "hash-object", "-w", file))
	index := filepath.Join(directory, "index")
	indexEnvironment := append(append([]string(nil), environment...), "GIT_INDEX_FILE="+index)
	requireProvisionGit(t, indexEnvironment, "--git-dir="+repositoryPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+",conf/git.conf")
	tree := strings.TrimSpace(requireProvisionGit(t, indexEnvironment,
		"--git-dir="+repositoryPath, "write-tree"))
	commit := strings.TrimSpace(requireProvisionGit(t, environment,
		"-c", "user.name=Kodama", "-c", "user.email=kodama@example.invalid",
		"--git-dir="+repositoryPath, "commit-tree", tree, "-m", "policy update"))
	requireProvisionGit(t, environment, "--git-dir="+repositoryPath,
		"update-ref", "refs/heads/main", commit)
	return commit
}

// TestProvisionTenantCreatesSingleParentlessPolicyCommit pins the exact
// initial history: one parentless commit, one tree entry, the documented
// policy bytes, and an explicit author identity.
func TestProvisionTenantCreatesSingleParentlessPolicyCommit(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	result := mustProvisionTenant(t, config)
	repositoryPath := layout.repositoryPath()
	if !result.Created {
		t.Fatalf("Created = false; want true on first provisioning")
	}
	if result.RepositoryPath != repositoryPath {
		t.Fatalf("RepositoryPath = %q; want %q", result.RepositoryPath, repositoryPath)
	}

	environment := config.Environment
	count := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "rev-list", "--count", "HEAD"))
	if count != "1" {
		t.Fatalf("initial history length = %s; want exactly 1 commit", count)
	}
	body := requireProvisionGit(t, environment, "--git-dir="+repositoryPath, "cat-file", "-p", "HEAD")
	author := ""
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "parent ") {
			t.Fatalf("initial commit is not parentless: %q", line)
		}
		if after, ok := strings.CutPrefix(line, "author "); ok {
			author = after
		}
	}
	if author == "" {
		t.Fatal("initial commit has no author line")
	}
	if !strings.Contains(author, "<") || strings.Contains(author, "<>") || !strings.Contains(author, "@") {
		t.Fatalf("initial commit author %q has no explicit non-empty identity", author)
	}

	tree := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "ls-tree", "-r", "HEAD"))
	lines := strings.Split(tree, "\n")
	if len(lines) != 1 {
		t.Fatalf("initial tree has %d entries; want exactly conf/git.conf: %q", len(lines), tree)
	}
	metadata, name, ok := strings.Cut(lines[0], "\t")
	if !ok || name != "conf/git.conf" {
		t.Fatalf("initial tree entry = %q; want conf/git.conf", lines[0])
	}
	fields := strings.Fields(metadata)
	if len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" {
		t.Fatalf("initial tree entry metadata = %q; want 100644 blob", metadata)
	}
	blob := requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "cat-file", "blob", "HEAD:conf/git.conf")
	if blob != adminInitialPolicy {
		t.Fatalf("committed conf/git.conf = %q; want %q", blob, adminInitialPolicy)
	}
}

// TestProvisionTenantInitializesFilesBackendAndMainHead pins the explicit ref
// backend and default branch.
func TestProvisionTenantInitializesFilesBackendAndMainHead(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	mustProvisionTenant(t, config)
	repositoryPath := layout.repositoryPath()

	format := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-parse", "--show-ref-format"))
	if format != "files" {
		t.Fatalf("ref storage format = %q; want files", format)
	}
	head := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "symbolic-ref", "HEAD"))
	if head != "refs/heads/main" {
		t.Fatalf("HEAD = %q; want refs/heads/main", head)
	}
	if _, err := os.Stat(filepath.Join(repositoryPath, "reftable")); !os.IsNotExist(err) {
		t.Fatalf("admin repository has a reftable store: %v", err)
	}
}

// TestProvisionTenantInstallsManagedHooksOnly pins that the two managed hooks
// are installed before publication and no update hook exists.
func TestProvisionTenantInstallsManagedHooksOnly(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	repositoryPath := layout.repositoryPath()

	hooks := readHookFiles(t, repositoryPath)
	for _, name := range []string{"pre-receive", "post-receive"} {
		content, ok := hooks[name]
		if !ok {
			t.Fatalf("hook %q was not installed", name)
		}
		if !strings.Contains(content, "managed by kodama") {
			t.Fatalf("hook %q is not the managed hook: %q", name, content)
		}
	}
	if _, ok := hooks["update"]; ok {
		t.Fatal("an update hook was installed; the batch design installs only pre-receive and post-receive")
	}
}

// TestProvisionTenantSetsPrivateModes pins 0700 directories, at most 0600
// ordinary files and 0700 hooks without a process-global umask change.
func TestProvisionTenantSetsPrivateModes(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	repositoryPath := layout.repositoryPath()

	for _, directory := range []string{
		layout.storageRoot,
		layout.tenantDirectory(),
		repositoryPath,
		layout.journalDirectory,
	} {
		requireDirectoryMode(t, directory, 0o700)
	}

	hooksDirectory := filepath.Join(repositoryPath, "hooks") + string(os.PathSeparator)
	err := filepath.WalkDir(layout.tenantDirectory(), func(path string, entry os.DirEntry, walkErr error) error {
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
		if strings.HasPrefix(path, hooksDirectory) {
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
}

// TestProvisionTenantPreparesEmptyJournalWithoutSyntheticEntry pins that
// provisioning creates the receive-journal directory and records nothing: the
// local seed is not a remote push.
func TestProvisionTenantPreparesEmptyJournalWithoutSyntheticEntry(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())

	requireDirectoryMode(t, layout.journalDirectory, 0o700)
	entries, err := os.ReadDir(layout.journalDirectory)
	if err != nil {
		t.Fatalf("read journal directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("journal directory has %d entries after local provisioning; want none", len(entries))
	}

	directory, err := os.ReadDir(layout.repositoryPath())
	if err != nil {
		t.Fatalf("read admin repository: %v", err)
	}
	var names []string
	for _, entry := range directory {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	want := []string{"HEAD", "config", "hooks", "objects", "refs"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("admin repository top-level entries = %v; want %v (no marker or store)", names, want)
	}
}

// TestProvisionTenantProbesGitVersionBeforeFilesystemEffect pins the version
// probe on the actual injected executor before any directory exists.
func TestProvisionTenantProbesGitVersionBeforeFilesystemEffect(t *testing.T) {
	root := t.TempDir()
	executor := &versionProbeExecutor{version: "git version 2.43.0\n"}
	config := ProvisionConfig{
		TenantID:         provisionTenantID,
		StorageRoot:      filepath.Join(root, "repos"),
		JournalDirectory: filepath.Join(root, "journal"),
		Environment:      provisionChildEnvironment(filepath.Join(root, "home")),
		Executor:         executor,
	}
	_, err := ProvisionTenant(context.Background(), config)
	if err == nil {
		t.Fatal("ProvisionTenant accepted Git 2.43.0")
	}
	if !strings.Contains(err.Error(), native.MinimumGitVersion) {
		t.Fatalf("error = %v; want the minimum Git version %s", err, native.MinimumGitVersion)
	}
	commands := executor.recorded()
	if len(commands) != 1 || strings.Join(commands[0].Args, " ") != "--version" {
		t.Fatalf("executor commands = %v; want exactly [--version] before any effect", commands)
	}
	if _, statErr := os.Stat(config.StorageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root exists after a refused version probe: %v", statErr)
	}
	if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("journal directory exists after a refused version probe: %v", statErr)
	}
}

// provisionHostileHost is the untrusted host Git state the isolation tests
// install: a global config at HOME/.gitconfig, another global source at
// XDG_CONFIG_HOME/git/config, a system config selected by GIT_CONFIG_SYSTEM,
// and template/hooks directories whose hostile hook leaves a marker. Every
// setting leaves a visible trace so a leaked configuration source is observed,
// never inferred: core.logAllRefUpdates writes logs/HEAD signed with
// user.name/user.email, init.templateDir copies a hostile hooks/pre-receive,
// core.hooksPath selects a hostile hook directory, and init.defaultBranch,
// init.defaultRefFormat, receive.denyDeleteCurrent and safe.directory would
// rewrite the repository if they ever reached git.
type provisionHostileHost struct {
	home          string
	xdgConfigHome string
	globalConfig  string
	xdgConfig     string
	systemConfig  string
	templateDir   string
	hooksPath     string
	hookMarker    string
	markers       []string
}

func newProvisionHostileHost(t *testing.T, root string) provisionHostileHost {
	t.Helper()
	hostile := filepath.Join(root, "hostile")
	host := provisionHostileHost{
		home:          filepath.Join(hostile, "home"),
		xdgConfigHome: filepath.Join(hostile, "xdg"),
		systemConfig:  filepath.Join(hostile, "system-gitconfig"),
		templateDir:   filepath.Join(hostile, "template"),
		hooksPath:     filepath.Join(hostile, "hooks"),
		hookMarker:    filepath.Join(hostile, "hostile-hook-ran"),
	}
	host.globalConfig = filepath.Join(host.home, ".gitconfig")
	host.xdgConfig = filepath.Join(host.xdgConfigHome, "git", "config")
	for _, directory := range []string{
		host.home,
		filepath.Dir(host.xdgConfig),
		filepath.Join(host.templateDir, "hooks"),
		host.hooksPath,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create hostile directory %q: %v", directory, err)
		}
	}
	hostileHook := "#!/bin/sh\ntouch \"" + host.hookMarker + "\"\nexit 1\n"
	for _, directory := range []string{filepath.Join(host.templateDir, "hooks"), host.hooksPath} {
		if err := os.WriteFile(filepath.Join(directory, "pre-receive"), []byte(hostileHook), 0o755); err != nil {
			t.Fatalf("write hostile hook: %v", err)
		}
	}
	config := "[core]\n" +
		"\tlogAllRefUpdates = true\n" +
		"\thooksPath = " + host.hooksPath + "\n" +
		"[user]\n" +
		"\tname = Hostile Person\n" +
		"\temail = hostile@evil.example\n" +
		"[init]\n" +
		"\ttemplateDir = " + host.templateDir + "\n" +
		"\tdefaultBranch = hostile\n" +
		"\tdefaultRefFormat = reftable\n" +
		"[receive]\n" +
		"\tdenyDeleteCurrent = refuse\n" +
		"[safe]\n" +
		"\tdirectory = *\n"
	for _, path := range []string{host.globalConfig, host.xdgConfig, host.systemConfig} {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatalf("write hostile config %q: %v", path, err)
		}
	}
	host.markers = []string{"Hostile Person", "hostile@evil.example", host.hookMarker}
	return host
}

// environment is the hostile child environment: every hostile setting is also
// offered as an inherited GIT_* variable, which the isolation must drop.
func (host provisionHostileHost) environment(path string) []string {
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + host.home,
		"XDG_CONFIG_HOME=" + host.xdgConfigHome,
		"GIT_CONFIG_GLOBAL=" + host.globalConfig,
		"GIT_CONFIG_SYSTEM=" + host.systemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_CONFIG_COUNT=1",
		// An inherited identity outranks `-c user.name`/`-c user.email`, so an
		// implementation that forwards ambient GIT_* variables commits with
		// this identity instead of its own explicit one.
		"GIT_AUTHOR_NAME=Hostile Person",
		"GIT_AUTHOR_EMAIL=hostile@evil.example",
		"GIT_COMMITTER_NAME=Hostile Person",
		"GIT_COMMITTER_EMAIL=hostile@evil.example",
		"PATH=" + path,
	}
}

// setProcessEnvironment installs the hostile host state in this process so the
// nil-Environment path inherits it, as `kodama git provision-tenant` does when
// an operator runs it from a hostile shell.
func (host provisionHostileHost) setProcessEnvironment(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"HOME":                host.home,
		"XDG_CONFIG_HOME":     host.xdgConfigHome,
		"GIT_CONFIG_GLOBAL":   host.globalConfig,
		"GIT_CONFIG_SYSTEM":   host.systemConfig,
		"GIT_CONFIG_NOSYSTEM": "0",
		"GIT_AUTHOR_NAME":     "Hostile Person",
		"GIT_AUTHOR_EMAIL":    "hostile@evil.example",
		"GIT_COMMITTER_NAME":  "Hostile Person",
		"GIT_COMMITTER_EMAIL": "hostile@evil.example",
	} {
		t.Setenv(name, value)
	}
}

// provisionAssertionEnvironment is the explicit environment the isolation
// assertions use. It pins both global and system configuration to /dev/null so
// the assertions themselves never read a hostile inherited configuration.
func provisionAssertionEnvironment(t *testing.T) []string {
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

// requireIsolatedAdminRepository pins the complete absence of the hostile host
// state: no reflog anywhere, no hostile text in any file, exactly the two
// managed hooks, `main` HEAD, the files backend and the explicit identity.
func requireIsolatedAdminRepository(t *testing.T, environment []string,
	repositoryPath string, host provisionHostileHost) {
	t.Helper()
	if _, err := os.Stat(host.hookMarker); !os.IsNotExist(err) {
		t.Fatalf("a hostile hook ran during provisioning: %v", err)
	}
	err := filepath.WalkDir(repositoryPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == "logs" {
				t.Errorf("hostile core.logAllRefUpdates created a reflog directory %q", path)
			}
			if name == "reftable" {
				t.Errorf("hostile init.defaultRefFormat created a reftable store %q", path)
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range host.markers {
			if bytes.Contains(data, []byte(marker)) {
				t.Errorf("hostile value %q appears in %q", marker, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk provisioned repository: %v", err)
	}

	if format := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "rev-parse", "--show-ref-format")); format != "files" {
		t.Fatalf("hostile configuration selected ref backend %q; want files", format)
	}
	if head := strings.TrimSpace(requireProvisionGit(t, environment,
		"--git-dir="+repositoryPath, "symbolic-ref", "HEAD")); head != "refs/heads/main" {
		t.Fatalf("hostile configuration selected branch %q; want refs/heads/main", head)
	}
	if _, err := os.Lstat(filepath.Join(repositoryPath, "logs")); !os.IsNotExist(err) {
		t.Fatalf("provisioned repository has a logs directory: %v", err)
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

	body := requireProvisionGit(t, environment, "--git-dir="+repositoryPath, "cat-file", "-p", "HEAD")
	for _, identity := range []string{
		"author Kodama Provisioning <kodama@example.invalid>",
		"committer Kodama Provisioning <kodama@example.invalid>",
	} {
		if !strings.Contains(body, identity) {
			t.Fatalf("initial commit does not carry the explicit identity %q: %q", identity, body)
		}
	}
}

// TestProvisionTenantIsolatesHostileGitConfiguration pins that hostile host Git
// configuration cannot choose the template, hooks path, ref backend, default
// branch, reflog or author identity. It covers the in-process boundary with a
// hostile child environment and with the documented nil environment inherited
// from a hostile process; the real `kodama git provision-tenant` binary is
// covered at the integration boundary.
func TestProvisionTenantIsolatesHostileGitConfiguration(t *testing.T) {
	t.Run("hostile-child-environment", func(t *testing.T) {
		layout := newProvisionLayout(t)
		host := newProvisionHostileHost(t, layout.root)
		config := layout.config()
		config.Environment = host.environment(os.Getenv("PATH"))
		result := mustProvisionTenant(t, config)
		if !result.Created {
			t.Fatal("Created = false; want true")
		}
		requireIsolatedAdminRepository(t, provisionAssertionEnvironment(t), layout.repositoryPath(), host)
	})

	t.Run("nil-environment-inherits-hostile-process", func(t *testing.T) {
		layout := newProvisionLayout(t)
		host := newProvisionHostileHost(t, layout.root)
		host.setProcessEnvironment(t)
		config := layout.config()
		config.Environment = nil
		result := mustProvisionTenant(t, config)
		if !result.Created {
			t.Fatal("Created = false; want true")
		}
		requireIsolatedAdminRepository(t, provisionAssertionEnvironment(t), layout.repositoryPath(), host)
	})

	// The next two subtests isolate each inherited global source: HOME/.gitconfig
	// alone and XDG_CONFIG_HOME/git/config alone. GIT_CONFIG_GLOBAL replaces both,
	// so the same pin neutralizes each of them; isolating them proves neither
	// source is read.
	t.Run("home-global-only", func(t *testing.T) {
		layout := newProvisionLayout(t)
		host := newProvisionHostileHost(t, layout.root)
		for _, path := range []string{host.xdgConfig, host.systemConfig} {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove %q: %v", path, err)
			}
		}
		config := layout.config()
		config.Environment = []string{
			"LC_ALL=C",
			"GIT_TERMINAL_PROMPT=0",
			"HOME=" + host.home,
			"PATH=" + os.Getenv("PATH"),
		}
		result := mustProvisionTenant(t, config)
		if !result.Created {
			t.Fatal("Created = false; want true")
		}
		requireIsolatedAdminRepository(t, provisionAssertionEnvironment(t), layout.repositoryPath(), host)
	})

	t.Run("xdg-global-only", func(t *testing.T) {
		layout := newProvisionLayout(t)
		host := newProvisionHostileHost(t, layout.root)
		for _, path := range []string{host.globalConfig, host.systemConfig} {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove %q: %v", path, err)
			}
		}
		config := layout.config()
		config.Environment = []string{
			"LC_ALL=C",
			"GIT_TERMINAL_PROMPT=0",
			"HOME=" + host.home,
			"XDG_CONFIG_HOME=" + host.xdgConfigHome,
			"PATH=" + os.Getenv("PATH"),
		}
		result := mustProvisionTenant(t, config)
		if !result.Created {
			t.Fatal("Created = false; want true")
		}
		requireIsolatedAdminRepository(t, provisionAssertionEnvironment(t), layout.repositoryPath(), host)
	})
}

// TestProvisionGitEnvironmentPinsIsolation pins the exact child environment:
// every inherited GIT_* variable is dropped and the global and system
// configuration sources are replaced by /dev/null. GIT_CONFIG_GLOBAL also
// replaces XDG_CONFIG_HOME/git/config, so the hostile XDG source is neutralized
// by the same pin.
func TestProvisionGitEnvironmentPinsIsolation(t *testing.T) {
	environment := provisionGitEnvironment([]string{
		"HOME=/hostile/home",
		"PATH=/usr/bin",
		"LC_ALL=en_US.UTF-8",
		"GIT_CONFIG_GLOBAL=/hostile/gitconfig",
		"GIT_CONFIG_SYSTEM=/hostile/system-gitconfig",
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_DIR=/hostile/repository",
		"GIT_AUTHOR_NAME=Hostile Person",
		"GIT_AUTHOR_EMAIL=hostile@evil.example",
		"XDG_CONFIG_HOME=/hostile/xdg",
	})
	values := map[string][]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("environment entry %q has no value", entry)
		}
		values[name] = append(values[name], value)
	}
	want := map[string]string{
		"LC_ALL":              "C",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_SYSTEM":   os.DevNull,
		"GIT_ATTR_NOSYSTEM":   "1",
		// HOME is passed through for git's own path resolution only; the
		// configuration sources under it are pinned above.
		"HOME": "/hostile/home",
	}
	for name, expected := range want {
		observed, ok := values[name]
		if !ok {
			t.Errorf("environment is missing %s", name)
			continue
		}
		if len(observed) != 1 || observed[0] != expected {
			t.Errorf("%s = %v; want exactly [%q]", name, observed, expected)
		}
	}
	for _, name := range []string{
		"GIT_DIR",
		"GIT_CONFIG_COUNT",
		"GIT_AUTHOR_NAME",
		"GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME",
		"GIT_COMMITTER_EMAIL",
	} {
		if observed, ok := values[name]; ok {
			t.Errorf("inherited %s = %v; every GIT_* variable must be dropped", name, observed)
		}
	}
}

// TestProvisionTenantRefusesExistingEmptyDestination pins that an empty admin
// destination is refused and never seeded.
func TestProvisionTenantRefusesExistingEmptyDestination(t *testing.T) {
	layout := newProvisionLayout(t)
	repositoryPath := layout.repositoryPath()
	if err := os.MkdirAll(repositoryPath, 0o700); err != nil {
		t.Fatalf("create empty destination: %v", err)
	}
	_, err := ProvisionTenant(context.Background(), layout.config())
	if !errors.Is(err, native.ErrProvisionDestination) {
		t.Fatalf("error = %v; want ErrProvisionDestination", err)
	}
	entries, readErr := os.ReadDir(repositoryPath)
	if readErr != nil {
		t.Fatalf("read empty destination: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("empty destination was seeded with %d entries", len(entries))
	}
}

// TestProvisionTenantRefusesDamagedDestination pins that a damaged admin
// repository is refused without repair or reseeding.
func TestProvisionTenantRefusesDamagedDestination(t *testing.T) {
	layout := newProvisionLayout(t)
	repositoryPath := layout.repositoryPath()
	if err := os.MkdirAll(filepath.Join(repositoryPath, "objects"), 0o700); err != nil {
		t.Fatalf("create damaged destination: %v", err)
	}
	damagedHead := []byte("garbage\n")
	if err := os.WriteFile(filepath.Join(repositoryPath, "HEAD"), damagedHead, 0o600); err != nil {
		t.Fatalf("write damaged HEAD: %v", err)
	}
	_, err := ProvisionTenant(context.Background(), layout.config())
	if !errors.Is(err, native.ErrProvisionDestination) {
		t.Fatalf("error = %v; want ErrProvisionDestination", err)
	}
	head, readErr := os.ReadFile(filepath.Join(repositoryPath, "HEAD"))
	if readErr != nil {
		t.Fatalf("read damaged HEAD: %v", readErr)
	}
	if string(head) != string(damagedHead) {
		t.Fatalf("damaged HEAD was repaired to %q; want %q", head, damagedHead)
	}
}

// TestProvisionTenantRefusesForeignHookDestination pins that a repository with
// a foreign hook is refused, never overwritten and never reseeded.
func TestProvisionTenantRefusesForeignHookDestination(t *testing.T) {
	layout := newProvisionLayout(t)
	repositoryPath := layout.repositoryPath()
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{Path: repositoryPath}); err != nil {
		t.Fatalf("initialize existing repository: %v", err)
	}
	foreign := "#!/bin/sh\nexit 0\n"
	hookPath := filepath.Join(repositoryPath, "hooks", "pre-receive")
	if err := os.WriteFile(hookPath, []byte(foreign), 0o755); err != nil {
		t.Fatalf("write foreign hook: %v", err)
	}
	_, err := ProvisionTenant(context.Background(), layout.config())
	if !errors.Is(err, native.ErrProvisionDestination) {
		t.Fatalf("error = %v; want ErrProvisionDestination", err)
	}
	data, readErr := os.ReadFile(hookPath)
	if readErr != nil {
		t.Fatalf("read foreign hook: %v", readErr)
	}
	if string(data) != foreign {
		t.Fatalf("foreign hook was overwritten with %q", data)
	}
	if _, _, code := provisionGitRaw(t, layout.config().Environment,
		"--git-dir="+repositoryPath, "rev-parse", "--verify", "--quiet", "refs/heads/main"); code == 0 {
		t.Fatal("foreign destination was reseeded with refs/heads/main")
	}
}

// TestProvisionTenantRefusesForeignHookOnProvisionedRepository pins that an
// otherwise provisioned repository whose managed hook was replaced is treated
// as a foreign destination: it is refused and its hook and history are never
// repaired or reseeded.
func TestProvisionTenantRefusesForeignHookOnProvisionedRepository(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	first := mustProvisionTenant(t, config)
	if !first.Created {
		t.Fatal("Created = false on first provisioning")
	}
	repositoryPath := layout.repositoryPath()
	hookPath := filepath.Join(repositoryPath, "hooks", "pre-receive")
	foreign := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(hookPath, []byte(foreign), 0o700); err != nil {
		t.Fatalf("replace managed hook: %v", err)
	}
	head := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-parse", "refs/heads/main"))

	_, err := ProvisionTenant(context.Background(), config)
	if !errors.Is(err, native.ErrProvisionDestination) {
		t.Fatalf("error = %v; want ErrProvisionDestination", err)
	}
	data, readErr := os.ReadFile(hookPath)
	if readErr != nil {
		t.Fatalf("read foreign hook: %v", readErr)
	}
	if string(data) != foreign {
		t.Fatalf("foreign hook was overwritten with %q", data)
	}
	observed := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-parse", "refs/heads/main"))
	if observed != head {
		t.Fatalf("refused attempt changed the trusted ref from %q to %q", head, observed)
	}
}

// TestProvisionTenantRefusesSymlinkedDestinations pins that a symlinked admin
// path is refused and its target is untouched.
func TestProvisionTenantRefusesSymlinkedDestinations(t *testing.T) {
	t.Run("outside-tenant", func(t *testing.T) {
		layout := newProvisionLayout(t)
		outside := filepath.Join(layout.root, "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatalf("create outside target: %v", err)
		}
		if err := os.MkdirAll(layout.tenantDirectory(), 0o700); err != nil {
			t.Fatalf("create tenant directory: %v", err)
		}
		if err := os.Symlink(outside, layout.repositoryPath()); err != nil {
			t.Fatalf("symlink admin destination: %v", err)
		}
		_, err := ProvisionTenant(context.Background(), layout.config())
		if !errors.Is(err, native.ErrProvisionContainment) && !errors.Is(err, native.ErrProvisionDestination) {
			t.Fatalf("error = %v; want a containment or destination refusal", err)
		}
		entries, readErr := os.ReadDir(outside)
		if readErr != nil {
			t.Fatalf("read outside target: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("outside target was written with %d entries", len(entries))
		}
	})
	t.Run("inside-tenant", func(t *testing.T) {
		layout := newProvisionLayout(t)
		target := filepath.Join(layout.tenantDirectory(), "real-admin")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatalf("create in-tenant target: %v", err)
		}
		if err := os.Symlink(target, layout.repositoryPath()); err != nil {
			t.Fatalf("symlink admin destination: %v", err)
		}
		_, err := ProvisionTenant(context.Background(), layout.config())
		if !errors.Is(err, native.ErrProvisionContainment) && !errors.Is(err, native.ErrProvisionDestination) {
			t.Fatalf("error = %v; want a containment or destination refusal", err)
		}
		entries, readErr := os.ReadDir(target)
		if readErr != nil {
			t.Fatalf("read in-tenant target: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("in-tenant symlink target was written with %d entries", len(entries))
		}
	})
}

// TestProvisionTenantPreservesExistingRepositoryAfterAdminPolicyUpdate pins
// that a second attempt validates and preserves every byte, including an
// accepted Admin policy update.
func TestProvisionTenantPreservesExistingRepositoryAfterAdminPolicyUpdate(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	first := mustProvisionTenant(t, config)
	if !first.Created {
		t.Fatal("Created = false on first provisioning")
	}
	repositoryPath := layout.repositoryPath()
	updatedPolicy := "repo admin\n    RW = @admin\n\nrepo shop/web\n    R = @all\n"
	updatedCommit := writeAdminPolicyCommit(t, config.Environment, repositoryPath, updatedPolicy)

	configBytes, err := os.ReadFile(filepath.Join(repositoryPath, "config"))
	if err != nil {
		t.Fatalf("read repository config: %v", err)
	}
	hooks := readHookFiles(t, repositoryPath)

	second := mustProvisionTenant(t, config)
	if second.Created {
		t.Fatal("Created = true on an existing valid repository; want false")
	}
	if second.RepositoryPath != repositoryPath {
		t.Fatalf("RepositoryPath = %q; want %q", second.RepositoryPath, repositoryPath)
	}
	head := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-parse", "refs/heads/main"))
	if head != updatedCommit {
		t.Fatalf("admin policy ref = %q; want the preserved update %q", head, updatedCommit)
	}
	blob := requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "cat-file", "blob", "HEAD:conf/git.conf")
	if blob != updatedPolicy {
		t.Fatalf("preserved policy = %q; want %q", blob, updatedPolicy)
	}
	preservedConfig, err := os.ReadFile(filepath.Join(repositoryPath, "config"))
	if err != nil {
		t.Fatalf("read preserved repository config: %v", err)
	}
	if string(preservedConfig) != string(configBytes) {
		t.Fatal("existing repository config was rewritten")
	}
	preservedHooks := readHookFiles(t, repositoryPath)
	if len(preservedHooks) != len(hooks) {
		t.Fatalf("hook count changed from %d to %d", len(hooks), len(preservedHooks))
	}
	for name, content := range hooks {
		if preservedHooks[name] != content {
			t.Fatalf("hook %q was rewritten on a preserving attempt", name)
		}
	}
}

// TestProvisionTenantRejectsContainmentEscape pins that invalid tenants and
// tenant-directory symlinks are refused before any write.
func TestProvisionTenantRejectsContainmentEscape(t *testing.T) {
	t.Run("parent-tenant", func(t *testing.T) {
		layout := newProvisionLayout(t)
		config := layout.config()
		config.TenantID = id.TenantID("../escape")
		_, err := ProvisionTenant(context.Background(), config)
		if !errors.Is(err, id.ErrInvalidID) {
			t.Fatalf("error = %v; want ErrInvalidID", err)
		}
		if _, statErr := os.Stat(layout.storageRoot); !os.IsNotExist(statErr) {
			t.Fatalf("storage root was created for a refusing tenant: %v", statErr)
		}
	})
	t.Run("absolute-tenant", func(t *testing.T) {
		layout := newProvisionLayout(t)
		config := layout.config()
		config.TenantID = id.TenantID("/tmp/escape")
		_, err := ProvisionTenant(context.Background(), config)
		if !errors.Is(err, id.ErrInvalidID) {
			t.Fatalf("error = %v; want ErrInvalidID", err)
		}
		if _, statErr := os.Stat(layout.storageRoot); !os.IsNotExist(statErr) {
			t.Fatalf("storage root was created for a refusing tenant: %v", statErr)
		}
	})
	t.Run("symlinked-tenant", func(t *testing.T) {
		layout := newProvisionLayout(t)
		outside := filepath.Join(layout.root, "outside")
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatalf("create outside target: %v", err)
		}
		if err := os.MkdirAll(layout.storageRoot, 0o700); err != nil {
			t.Fatalf("create storage root: %v", err)
		}
		if err := os.Symlink(outside, layout.tenantDirectory()); err != nil {
			t.Fatalf("symlink tenant directory: %v", err)
		}
		_, err := ProvisionTenant(context.Background(), layout.config())
		if !errors.Is(err, native.ErrProvisionContainment) {
			t.Fatalf("error = %v; want ErrProvisionContainment", err)
		}
		entries, readErr := os.ReadDir(outside)
		if readErr != nil {
			t.Fatalf("read outside target: %v", readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("outside tenant target was written with %d entries", len(entries))
		}
	})
}

// TestProvisionTenantFailureBeforePublicationLeavesNoRepository pins every
// pre-publication boundary: the injected failure is returned, no canonical
// repository appears, only this invocation's staging is removed, and existing
// journal entries survive.
func TestProvisionTenantFailureBeforePublicationLeavesNoRepository(t *testing.T) {
	steps := []native.ProvisionStep{
		native.StepStage,
		native.StepHooks,
		native.StepCommit,
		native.StepSync,
		native.StepJournal,
		native.StepRename,
	}
	for _, step := range steps {
		t.Run(string(step), func(t *testing.T) {
			layout := newProvisionLayout(t)
			if err := os.MkdirAll(layout.journalDirectory, 0o700); err != nil {
				t.Fatalf("create journal directory: %v", err)
			}
			existingEntry := filepath.Join(layout.journalDirectory, "existing.json")
			existingBytes := []byte(`{"keep":true}`)
			if err := os.WriteFile(existingEntry, existingBytes, 0o600); err != nil {
				t.Fatalf("write existing journal entry: %v", err)
			}
			config := layout.config()
			injected := fmt.Errorf("injected %s failure", step)
			config.Step = func(_ context.Context, observed native.ProvisionStep) error {
				if observed == step {
					return injected
				}
				return nil
			}
			_, err := ProvisionTenant(context.Background(), config)
			if !errors.Is(err, injected) {
				t.Fatalf("error = %v; want the injected %s failure", err, step)
			}
			if _, statErr := os.Stat(layout.repositoryPath()); !os.IsNotExist(statErr) {
				t.Fatalf("canonical admin repository exists after a %s failure", step)
			}
			if entries, readErr := os.ReadDir(layout.tenantDirectory()); readErr == nil {
				for _, entry := range entries {
					// Only a regular lock file may remain; a staging
					// directory is this invocation's private state and must be
					// removed before returning the failure.
					if entry.IsDir() {
						t.Fatalf("staging directory %q survived a %s failure", entry.Name(), step)
					}
				}
			}
			preserved, readErr := os.ReadFile(existingEntry)
			if readErr != nil {
				t.Fatalf("existing journal entry was removed after a %s failure: %v", step, readErr)
			}
			if string(preserved) != string(existingBytes) {
				t.Fatalf("existing journal entry changed after a %s failure", step)
			}
		})
	}
}

// TestProvisionTenantFailureAfterPublicationPreservesRepository pins that a
// post-rename failure reports uncertainty, never deletes the repository, and a
// retry validates and preserves it.
func TestProvisionTenantFailureAfterPublicationPreservesRepository(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	injected := errors.New("injected post-publication failure")
	config.Step = func(_ context.Context, step native.ProvisionStep) error {
		if step == native.StepPublished {
			return injected
		}
		return nil
	}
	result, err := ProvisionTenant(context.Background(), config)
	if !errors.Is(err, injected) {
		t.Fatalf("error = %v; want the injected post-publication failure", err)
	}
	repositoryPath := layout.repositoryPath()
	if _, statErr := os.Stat(repositoryPath); statErr != nil {
		t.Fatalf("published repository was deleted after a post-publication failure: %v", statErr)
	}
	// Decision pinned by this test: a failure observed after the publication
	// rename still reports Created=true because this invocation published the
	// repository. Reporting Created=false would invite a caller to treat the
	// existing repository as somebody else's state or to attempt a repair.
	if !result.Created {
		t.Fatalf("Created = false after a post-publication failure; want true for the repository this invocation published")
	}
	if result.RepositoryPath != repositoryPath {
		t.Fatalf("RepositoryPath = %q; want %q", result.RepositoryPath, repositoryPath)
	}
	count := strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-list", "--count", "HEAD"))
	if count != "1" {
		t.Fatalf("published history length = %s; want 1", count)
	}

	config.Step = nil
	retry, err := ProvisionTenant(context.Background(), config)
	if err != nil {
		t.Fatalf("retry after publication: %v", err)
	}
	if retry.Created {
		t.Fatal("retry Created = true; want false for an existing valid repository")
	}
	count = strings.TrimSpace(requireProvisionGit(t, config.Environment,
		"--git-dir="+repositoryPath, "rev-list", "--count", "HEAD"))
	if count != "1" {
		t.Fatalf("retry changed the history length to %s; want 1", count)
	}
}

// TestProvisionTenantIgnoresPrivateStagingLeftover pins that a crash leftover
// is never interpreted as provisioning state and stays private.
func TestProvisionTenantIgnoresPrivateStagingLeftover(t *testing.T) {
	layout := newProvisionLayout(t)
	tenantDirectory := layout.tenantDirectory()
	leftover := filepath.Join(tenantDirectory, ".provision-admin-crash")
	if err := os.MkdirAll(filepath.Join(leftover, "objects"), 0o700); err != nil {
		t.Fatalf("create crash leftover: %v", err)
	}
	leftoverHead := []byte("garbage\n")
	if err := os.WriteFile(filepath.Join(leftover, "HEAD"), leftoverHead, 0o600); err != nil {
		t.Fatalf("write crash leftover HEAD: %v", err)
	}
	result := mustProvisionTenant(t, layout.config())
	if !result.Created {
		t.Fatal("Created = false; want true with only a private leftover present")
	}
	head, readErr := os.ReadFile(filepath.Join(leftover, "HEAD"))
	if readErr != nil {
		t.Fatalf("private crash leftover was removed or reinterpreted: %v", readErr)
	}
	if string(head) != string(leftoverHead) {
		t.Fatalf("private crash leftover was modified to %q", head)
	}
}

// TestProvisionTenantCancellationLeavesNoRepository pins pre-effect
// cancellation.
func TestProvisionTenantCancellationLeavesNoRepository(t *testing.T) {
	layout := newProvisionLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ProvisionTenant(ctx, layout.config())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if _, statErr := os.Stat(layout.storageRoot); !os.IsNotExist(statErr) {
		t.Fatalf("storage root exists after cancellation: %v", statErr)
	}
}

// TestProvisionTenantAcceptedPolicyAllowsAdminPolicyRef pins that the
// parser and rule evaluator accept the committed initial policy: the admin
// role may fast-forward the trusted policy ref and no approval guard covers it.
func TestProvisionTenantAcceptedPolicyAllowsAdminPolicyRef(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	mustProvisionTenant(t, config)
	blob := requireProvisionGit(t, config.Environment,
		"--git-dir="+layout.repositoryPath(), "cat-file", "blob", "HEAD:conf/git.conf")
	parsed, err := git.ParseConfig([]byte(blob))
	if err != nil {
		t.Fatalf("committed policy does not parse: %v", err)
	}
	policy, ok := parsed.Policy(native.AdminRepository)
	if !ok {
		t.Fatalf("committed policy has no %q repository", native.AdminRepository)
	}
	decision, err := policy.Authorize(git.AccessRequest{
		Identity: git.Identity{Roles: []string{"admin"}},
		Ref:      native.CommittedPolicyRef,
		Update:   git.UpdateFastForward,
	})
	if err != nil || decision != git.DecisionAllow {
		t.Fatalf("admin fast-forward decision = %q, %v; want allow", decision, err)
	}
	if _, guarded := policy.ApprovalFor(native.CommittedPolicyRef); guarded {
		t.Fatalf("committed policy approval-guards %s", native.CommittedPolicyRef)
	}
	denied, err := policy.Authorize(git.AccessRequest{
		Identity: git.Identity{Roles: []string{"engineer"}},
		Ref:      native.CommittedPolicyRef,
		Update:   git.UpdateFastForward,
	})
	if err != nil || denied != git.DecisionDeny {
		t.Fatalf("engineer fast-forward decision = %q, %v; want deny", denied, err)
	}
}

// TestProvisionTenantConcurrentProcessesConvergeOnOneHistory pins cross-process
// serialization: independent processes provisioning the same tenant converge on
// exactly one initial history and only one Created=true result.
func TestProvisionTenantConcurrentProcessesConvergeOnOneHistory(t *testing.T) {
	layout := newProvisionLayout(t)
	const workers = 4
	type outcome struct {
		created bool
		output  string
		err     error
	}
	outcomes := make([]outcome, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Go(func() {
			resultPath := filepath.Join(layout.root, fmt.Sprintf("result-%d", index))
			command := exec.Command(os.Args[0], "-test.run=^TestProvisionTenantHelperProcess$", "-test.count=1")
			command.Env = append(os.Environ(),
				provisionHelperEnvironment+"=1",
				"KODAMA_PROVISION_TENANT="+string(provisionTenantID),
				"KODAMA_PROVISION_STORAGE="+layout.storageRoot,
				"KODAMA_PROVISION_JOURNAL="+layout.journalDirectory,
				"KODAMA_PROVISION_HOME="+layout.home,
				"KODAMA_PROVISION_RESULT="+resultPath,
			)
			output, err := command.CombinedOutput()
			if err != nil {
				outcomes[index] = outcome{output: string(output), err: err}
				return
			}
			data, readErr := os.ReadFile(resultPath)
			if readErr != nil {
				outcomes[index] = outcome{output: string(output), err: readErr}
				return
			}
			outcomes[index] = outcome{
				created: strings.TrimSpace(string(data)) == "created",
				output:  string(output),
			}
		})
	}
	wait.Wait()
	created := 0
	for index, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("worker %d: %v: %s", index, outcome.err, outcome.output)
		}
		if outcome.created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("Created=true count = %d; want exactly 1", created)
	}
	count := strings.TrimSpace(requireProvisionGit(t, layout.config().Environment,
		"--git-dir="+layout.repositoryPath(), "rev-list", "--count", "HEAD"))
	if count != "1" {
		t.Fatalf("converged history length = %s; want 1", count)
	}
}

// hashProvisionedTree hashes every path, mode and byte under root so any
// rewrite, repair or reseed in a refused attempt is observable.
func hashProvisionedTree(t *testing.T, root string) string {
	t.Helper()
	digest := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(digest, "%s %s %04o\n", relative, info.Mode().Type(), info.Mode().Perm())
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest.Write(data)
		return nil
	})
	if err != nil {
		t.Fatalf("hash provisioned tree %q: %v", root, err)
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

// TestProvisionTenantRefusesInvalidExistingDestination pins every refused
// existing-destination shape: a missing managed hook, an update hook, a wrong
// HEAD, a committed policy that would lock @admin out of the policy ref, a
// non-bare repository, a trusted ref that is not a commit and a reftable store.
// Each refusal is ErrProvisionDestination, never repaired, and the repository
// is byte-identical afterwards.
func TestProvisionTenantRefusesInvalidExistingDestination(t *testing.T) {
	policyWithoutAdminWrite := "repo admin\n    R = @admin\n\nrepo shop/web\n    R = @all\n"
	cases := []struct {
		name    string
		mutate  func(t *testing.T, layout provisionLayout, config ProvisionConfig, repositoryPath string)
		observe func(t *testing.T, layout provisionLayout, config ProvisionConfig, repositoryPath string)
	}{
		{
			name: "missing-managed-hook",
			mutate: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if err := os.Remove(filepath.Join(repositoryPath, "hooks", "post-receive")); err != nil {
					t.Fatalf("remove managed hook: %v", err)
				}
			},
			observe: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if _, err := os.Lstat(filepath.Join(repositoryPath, "hooks", "post-receive")); !os.IsNotExist(err) {
					t.Fatalf("refused attempt installed the missing managed hook: %v", err)
				}
			},
		},
		{
			name: "update-hook",
			mutate: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repositoryPath, "hooks", "update"),
					[]byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatalf("write update hook: %v", err)
				}
			},
			observe: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(repositoryPath, "hooks", "update"))
				if err != nil {
					t.Fatalf("refused attempt removed the update hook: %v", err)
				}
				if string(data) != "#!/bin/sh\nexit 0\n" {
					t.Fatalf("refused attempt rewrote the update hook to %q", data)
				}
			},
		},
		{
			name: "wrong-head",
			mutate: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repositoryPath, "HEAD"),
					[]byte("ref: refs/heads/hostile\n"), 0o600); err != nil {
					t.Fatalf("write wrong HEAD: %v", err)
				}
			},
			observe: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(repositoryPath, "HEAD"))
				if err != nil {
					t.Fatalf("read HEAD: %v", err)
				}
				if string(data) != "ref: refs/heads/hostile\n" {
					t.Fatalf("refused attempt repaired HEAD to %q", data)
				}
			},
		},
		{
			name: "non-admin-write-policy",
			mutate: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				writeAdminPolicyCommit(t, config.Environment, repositoryPath, policyWithoutAdminWrite)
			},
			observe: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				blob := requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "cat-file", "blob", "HEAD:conf/git.conf")
				if blob != policyWithoutAdminWrite {
					t.Fatalf("refused attempt rewrote the committed policy to %q", blob)
				}
			},
		},
		{
			name: "non-bare-repository",
			mutate: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "config", "core.bare", "false")
			},
			observe: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				bare := strings.TrimSpace(requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "rev-parse", "--is-bare-repository"))
				if bare != "false" {
					t.Fatalf("refused attempt repaired core.bare to %q", bare)
				}
			},
		},
		{
			name: "trusted-ref-not-a-commit",
			mutate: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				directory := t.TempDir()
				file := filepath.Join(directory, "not-a-commit")
				if err := os.WriteFile(file, []byte("this is not a commit\n"), 0o600); err != nil {
					t.Fatalf("write blob source: %v", err)
				}
				blob := strings.TrimSpace(requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "hash-object", "-w", file))
				if err := os.WriteFile(filepath.Join(repositoryPath, "refs", "heads", "main"),
					[]byte(blob+"\n"), 0o600); err != nil {
					t.Fatalf("point the trusted ref at a blob: %v", err)
				}
			},
			observe: func(t *testing.T, _ provisionLayout, config ProvisionConfig, repositoryPath string) {
				t.Helper()
				commit := strings.TrimSpace(requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "rev-parse", "refs/heads/main"))
				kind := strings.TrimSpace(requireProvisionGit(t, config.Environment,
					"--git-dir="+repositoryPath, "cat-file", "-t", commit))
				if kind != "blob" {
					t.Fatalf("refused attempt repaired the trusted ref to a %q", kind)
				}
			},
		},
		{
			name: "reftable-store",
			mutate: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(repositoryPath, "reftable"), 0o700); err != nil {
					t.Fatalf("create reftable store: %v", err)
				}
			},
			observe: func(t *testing.T, _ provisionLayout, _ ProvisionConfig, repositoryPath string) {
				t.Helper()
				if _, err := os.Lstat(filepath.Join(repositoryPath, "reftable")); err != nil {
					t.Fatalf("refused attempt removed the reftable store: %v", err)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			layout := newProvisionLayout(t)
			config := layout.config()
			mustProvisionTenant(t, config)
			repositoryPath := layout.repositoryPath()
			testCase.mutate(t, layout, config, repositoryPath)
			before := hashProvisionedTree(t, repositoryPath)

			_, err := ProvisionTenant(context.Background(), config)
			if !errors.Is(err, native.ErrProvisionDestination) {
				t.Fatalf("error = %v; want ErrProvisionDestination", err)
			}
			if after := hashProvisionedTree(t, repositoryPath); after != before {
				t.Fatalf("refused attempt changed the repository tree: %s -> %s", before, after)
			}
			testCase.observe(t, layout, config, repositoryPath)
		})
	}
}

// TestProvisionTenantTransportConfigMatchesInitBareRepository pins the two
// smart-HTTP receive settings the provisioned repository carries and proves
// they are exactly the values InitBareRepository writes.
func TestProvisionTenantTransportConfigMatchesInitBareRepository(t *testing.T) {
	layout := newProvisionLayout(t)
	config := layout.config()
	mustProvisionTenant(t, config)
	reference := filepath.Join(layout.root, "reference", native.AdminRepository)
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{
		Path: reference,
		Env:  config.Environment,
	}); err != nil {
		t.Fatalf("InitBareRepository reference: %v", err)
	}
	for _, key := range []string{"receive.advertiseAtomic", "http.receivepack"} {
		provisioned := requireProvisionGit(t, config.Environment,
			"--git-dir="+layout.repositoryPath(), "config", "--local", "--get-all", key)
		initializer := requireProvisionGit(t, config.Environment,
			"-C", reference, "config", "--local", "--get-all", key)
		if strings.TrimSpace(provisioned) != "true" || len(strings.Fields(provisioned)) != 1 {
			t.Fatalf("provisioned %s = %q; want exactly true", key, provisioned)
		}
		if strings.TrimSpace(provisioned) != strings.TrimSpace(initializer) {
			t.Fatalf("%s: provisioned %q != InitBareRepository %q",
				key, strings.TrimSpace(provisioned), strings.TrimSpace(initializer))
		}
	}
}

// TestProvisionTenantPreservesJournalEntriesAndPinsPrivateLock pins the
// receive-journal directory mode, the preservation of existing journal entries
// and the private 0600 provisioning lock inside the 0700 tenant directory.
func TestProvisionTenantPreservesJournalEntriesAndPinsPrivateLock(t *testing.T) {
	t.Run("fresh-journal", func(t *testing.T) {
		layout := newProvisionLayout(t)
		mustProvisionTenant(t, layout.config())
		requireDirectoryMode(t, layout.journalDirectory, 0o700)
		entries, err := os.ReadDir(layout.journalDirectory)
		if err != nil {
			t.Fatalf("read journal directory: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("fresh journal directory has %d entries; want none", len(entries))
		}
	})
	t.Run("existing-entries-preserved", func(t *testing.T) {
		layout := newProvisionLayout(t)
		if err := os.MkdirAll(layout.journalDirectory, 0o700); err != nil {
			t.Fatalf("create journal directory: %v", err)
		}
		existing := filepath.Join(layout.journalDirectory, "existing.json")
		contents := []byte(`{"keep":true}`)
		if err := os.WriteFile(existing, contents, 0o640); err != nil {
			t.Fatalf("write existing journal entry: %v", err)
		}
		mustProvisionTenant(t, layout.config())

		requireDirectoryMode(t, layout.journalDirectory, 0o700)
		info, err := os.Stat(existing)
		if err != nil {
			t.Fatalf("stat preserved journal entry: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("preserved journal entry mode = %04o; want the existing 0640", got)
		}
		preserved, err := os.ReadFile(existing)
		if err != nil {
			t.Fatalf("read preserved journal entry: %v", err)
		}
		if !bytes.Equal(preserved, contents) {
			t.Fatalf("preserved journal entry = %q; want %q", preserved, contents)
		}
	})
	t.Run("private-lock", func(t *testing.T) {
		layout := newProvisionLayout(t)
		mustProvisionTenant(t, layout.config())
		requireDirectoryMode(t, layout.tenantDirectory(), 0o700)
		lockPath := filepath.Join(layout.tenantDirectory(), ".provision-tenant.lock")
		info, err := os.Stat(lockPath)
		if err != nil {
			t.Fatalf("stat provisioning lock: %v", err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("provisioning lock mode = %s; want a regular file", info.Mode())
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("provisioning lock mode = %04o; want 0600", got)
		}
	})
}

// TestProvisionTenantHelperProcess is the re-executed child of the concurrency
// test. It is skipped in an ordinary test run.
func TestProvisionTenantHelperProcess(t *testing.T) {
	if os.Getenv(provisionHelperEnvironment) != "1" {
		t.Skip("provisioning helper process only")
	}
	config := ProvisionConfig{
		TenantID:         id.TenantID(os.Getenv("KODAMA_PROVISION_TENANT")),
		StorageRoot:      os.Getenv("KODAMA_PROVISION_STORAGE"),
		JournalDirectory: os.Getenv("KODAMA_PROVISION_JOURNAL"),
		Environment:      provisionChildEnvironment(os.Getenv("KODAMA_PROVISION_HOME")),
	}
	result, err := ProvisionTenant(context.Background(), config)
	if err != nil {
		t.Fatalf("helper ProvisionTenant: %v", err)
	}
	state := "preserved"
	if result.Created {
		state = "created"
	}
	if err := os.WriteFile(os.Getenv("KODAMA_PROVISION_RESULT"), []byte(state), 0o600); err != nil {
		t.Fatalf("write helper result: %v", err)
	}
}

// TestProvisionTenantRefusesJournalInsideAdminRepository pins the provisioning
// half of the journal placement rule: a journal directory at or below the
// admin repository is refused before any command, storage directory or journal
// directory exists. Preparing it would create admin/ before the publish rename
// and leave a non-repository destination every retry refuses.
func TestProvisionTenantRefusesJournalInsideAdminRepository(t *testing.T) {
	cases := []struct {
		name    string
		journal func(provisionLayout) string
	}{
		{name: "admin repository itself", journal: func(layout provisionLayout) string { return layout.repositoryPath() }},
		{name: "inside the admin repository", journal: func(layout provisionLayout) string {
			return filepath.Join(layout.repositoryPath(), "journal")
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			layout := newProvisionLayout(t)
			config := layout.config()
			config.JournalDirectory = test.journal(layout)
			executor := &versionProbeExecutor{version: "git version 2.44.0\n"}
			config.Executor = executor

			_, err := ProvisionTenant(context.Background(), config)
			if !errors.Is(err, ErrJournalInRepository) {
				t.Fatalf("ProvisionTenant error = %v; want ErrJournalInRepository", err)
			}
			if commands := executor.recorded(); len(commands) != 0 {
				t.Fatalf("provisioning ran %d commands before the journal refusal; want none", len(commands))
			}
			for _, path := range []string{layout.storageRoot, config.JournalDirectory} {
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("%q state = %v; want no provisioning effect", path, statErr)
				}
			}
		})
	}
}
