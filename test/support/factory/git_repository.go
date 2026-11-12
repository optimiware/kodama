package factory

// Real tenant provisioning and bounded host Git commands for the host Git
// composition.
//
// Provisioning uses the product `gitapp.ProvisionTenant` boundary: the parentless
// `repo admin / RW = @admin` commit and the managed hooks are created by the
// product, never by fixture plumbing. The ordinary repository is created
// through `native.InitBareRepository` and seeded with one local commit before
// the service starts; that seed is fixture setup, not enforcement evidence.
// The fixture policy is then pushed through the served admin repository, so it
// reaches the trusted ref only through the real pre-receive/receive-pack path.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

const (
	// gitAuthEnvironment is the environment entry carrying the transient Basic
	// authorization value. Git reads it through --config-env, so the token never
	// appears in argv, a URL or repository configuration.
	gitAuthEnvironment = "KODAMA_GIT_AUTHORIZATION"
	// gitCommandTimeout bounds one fixture Git command that has no caller
	// deadline of its own.
	gitCommandTimeout = 2 * time.Minute
)

// hostGitClient runs the host Git binary for fixture setup and assertions. It
// isolates inherited Git configuration exactly like the product executor and
// trusts only the fixture CA.
type hostGitClient struct {
	baseURL string
	caFile  string
	home    string
}

// newHostGitClient creates the isolated client home and returns the client.
func newHostGitClient(root, baseURL, caFile string) (hostGitClient, error) {
	if strings.TrimSpace(baseURL) == "" {
		return hostGitClient{}, errors.New("host Git client base URL is empty")
	}
	if strings.TrimSpace(caFile) == "" {
		return hostGitClient{}, errors.New("host Git client CA file is empty")
	}
	home := filepath.Join(root, "client-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return hostGitClient{}, fmt.Errorf("create host Git client home: %w", err)
	}
	return hostGitClient{baseURL: baseURL, caFile: caFile, home: home}, nil
}

// repositoryURL returns the smart-HTTP URL of one tenant repository.
func (client hostGitClient) repositoryURL(repository string) string {
	return client.baseURL + "/" + repository + ".git"
}

// run executes one host Git command. It returns the exit code for an ordinary
// failure and a non-nil error only when the command could not be started.
func (client hostGitClient) run(ctx context.Context, token, directory string, args ...string) (string, string, int, error) {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + client.home,
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TRACE=0",
		"GIT_SSL_CAINFO=" + client.caFile,
	}
	global := []string{"-c", "http.followRedirects=false"}
	if strings.TrimSpace(token) != "" {
		global = append(global, "--config-env=http.extraHeader="+gitAuthEnvironment)
		environment = append(environment, gitAuthEnvironment+"="+basicAuthorization(token))
	}
	command := exec.CommandContext(ctx, "git", append(global, args...)...)
	command.Dir = directory
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0, nil
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode(), nil
	}
	return stdout.String(), stderr.String(), -1, err
}

// basicAuthorization renders the Basic value Git reads from the environment.
func basicAuthorization(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"+token))
}

// gitRepositorySpec is the complete fixture provisioning input.
type gitRepositorySpec struct {
	TenantID         id.TenantID
	StorageRoot      string
	JournalDirectory string
	HookBinary       string
	ScratchDirectory string
}

// gitRepositoryState is the provisioned fixture state.
type gitRepositoryState struct {
	AdminPath   string
	ProjectPath string
	DeployPath  string
	SeedCommit  string
	DeploySeed  string
}

// provisionGitRepositories creates the trusted admin repository through the
// product boundary and the ordinary repository through the native initializer,
// then seeds the ordinary history before the service serves it.
func provisionGitRepositories(ctx context.Context, spec gitRepositorySpec) (*gitRepositoryState, error) {
	result, err := gitapp.ProvisionTenant(ctx, gitapp.ProvisionConfig{
		TenantID:         spec.TenantID,
		StorageRoot:      spec.StorageRoot,
		JournalDirectory: spec.JournalDirectory,
		HookBinary:       spec.HookBinary,
	})
	if err != nil {
		return nil, fmt.Errorf("provision tenant Git storage: %w", err)
	}
	if !result.Created {
		return nil, fmt.Errorf("tenant admin repository %q already existed; the fixture requires a fresh provisioned repository", result.RepositoryPath)
	}
	projectPath := filepath.Join(spec.StorageRoot, string(spec.TenantID), filepath.FromSlash(GitProjectRepository))
	if err := native.InitBareRepository(ctx, native.BareRepositoryConfig{Path: projectPath}); err != nil {
		return nil, fmt.Errorf("initialize project repository: %w", err)
	}
	deployPath := filepath.Join(spec.StorageRoot, string(spec.TenantID), filepath.FromSlash(GitDeployRepository))
	if err := native.InitBareRepository(ctx, native.BareRepositoryConfig{Path: deployPath}); err != nil {
		return nil, fmt.Errorf("initialize deploy repository: %w", err)
	}
	client, err := newHostGitClient(spec.ScratchDirectory, "https://127.0.0.1:1", filepath.Join(spec.ScratchDirectory, "fixture-ca.pem"))
	if err != nil {
		return nil, err
	}
	seed, err := seedRepositoryHistory(ctx, client, spec.ScratchDirectory, projectPath, "main", "seed.txt", "seed project-i04/web")
	if err != nil {
		return nil, err
	}
	deploySeed, err := seedRepositoryHistory(ctx, client, spec.ScratchDirectory, deployPath, "deploy", "deploy.txt", "seed deploy-i04/release")
	if err != nil {
		return nil, err
	}
	if err := runGitOK(ctx, client, "", "-C", deployPath, "symbolic-ref", "HEAD", "refs/heads/deploy"); err != nil {
		return nil, fmt.Errorf("point deploy repository HEAD at deploy: %w", err)
	}
	return &gitRepositoryState{
		AdminPath:   result.RepositoryPath,
		ProjectPath: projectPath,
		DeployPath:  deployPath,
		SeedCommit:  seed,
		DeploySeed:  deploySeed,
	}, nil
}

// seedRepositoryHistory creates one commit on branch and pushes it directly to
// the bare repository path. The hooks are not installed yet, so this is
// explicitly fixture setup rather than an enforcement observation.
func seedRepositoryHistory(ctx context.Context, client hostGitClient, scratch, repositoryPath, branch, name, content string) (string, error) {
	worktree, err := os.MkdirTemp(scratch, "seed-")
	if err != nil {
		return "", fmt.Errorf("create seed worktree: %w", err)
	}
	defer os.RemoveAll(worktree)
	if err := runGitOK(ctx, client, worktree, "init", "--quiet", "-b", branch); err != nil {
		return "", err
	}
	file := filepath.Join(worktree, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", fmt.Errorf("create seed directory: %w", err)
	}
	if err := os.WriteFile(file, []byte(content+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write seed file: %w", err)
	}
	if err := runGitOK(ctx, client, worktree, "add", "--all"); err != nil {
		return "", err
	}
	if err := runGitOK(ctx, client, worktree,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", "seed "+branch); err != nil {
		return "", err
	}
	if err := runGitOK(ctx, client, worktree, "push", "--quiet", repositoryPath, "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	return gitOutputOK(ctx, client, worktree, "rev-parse", "HEAD")
}

// pushAdminPolicy clones the trusted admin repository, commits the fixture
// policy and pushes it through the served receive path. Receive-pack alone
// moves the trusted ref, so the accepted policy is the commit that passed
// pre-receive validation.
func pushAdminPolicy(ctx context.Context, client hostGitClient, scratch, token, policy string) (string, error) {
	worktree := filepath.Join(scratch, "admin-policy-worktree")
	if err := os.RemoveAll(worktree); err != nil {
		return "", fmt.Errorf("reset admin policy worktree: %w", err)
	}
	if _, stderr, code, err := client.run(ctx, token, "", "clone", "--quiet", client.repositoryURL(GitAdminRepository), worktree); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("clone admin repository exited %d: %s", code, strings.TrimSpace(stderr))
	}
	conf := filepath.Join(worktree, filepath.FromSlash(native.CommittedPolicyFile))
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		return "", fmt.Errorf("create policy directory: %w", err)
	}
	if err := os.WriteFile(conf, []byte(policy), 0o644); err != nil {
		return "", fmt.Errorf("write served policy file: %w", err)
	}
	if err := runGitOK(ctx, client, worktree, "add", "--all"); err != nil {
		return "", err
	}
	if err := runGitOK(ctx, client, worktree,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", "accept fixture repository policy"); err != nil {
		return "", err
	}
	commit, err := gitOutputOK(ctx, client, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if _, stderr, code, err := client.run(ctx, token, worktree, "push", "--quiet", "origin", "HEAD:"+native.CommittedPolicyRef); err != nil {
		return "", err
	} else if code != 0 {
		return "", fmt.Errorf("push admin policy exited %d: %s", code, strings.TrimSpace(stderr))
	}
	return commit, nil
}

// runGitOK runs one successful fixture command.
func runGitOK(ctx context.Context, client hostGitClient, directory string, args ...string) error {
	_, stderr, code, err := client.run(ctx, "", directory, args...)
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr))
	}
	return nil
}

// gitOutputOK runs one successful fixture command and returns trimmed stdout.
func gitOutputOK(ctx context.Context, client hostGitClient, directory string, args ...string) (string, error) {
	stdout, stderr, code, err := client.run(ctx, "", directory, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if code != 0 {
		return "", fmt.Errorf("git %s exited %d: %s", strings.Join(args, " "), code, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout), nil
}

// hostGitVersion records the host binary the product probe runs.
func hostGitVersion(ctx context.Context) (string, error) {
	output, err := exec.CommandContext(ctx, "git", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("probe host git version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", errors.New("host git version probe returned no output")
	}
	return version, nil
}

// Clone clones one repository with the supplied actor credential.
func (fixture *GitFixture) Clone(t testing.TB, token, repository, directory string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), gitCommandTimeout)
	defer cancel()
	_, stderr, code, err := fixture.client.run(ctx, token, "", "clone", "--quiet", fixture.client.repositoryURL(repository), directory)
	if err != nil {
		t.Fatalf("clone %s: %v", repository, err)
	}
	return stderr, code
}

// Push pushes one or more refspecs with the supplied actor credential and
// returns the stderr and exit code so a refusal can be asserted.
func (fixture *GitFixture) Push(ctx context.Context, directory, token string, refspecs ...string) (string, int) {
	args := append([]string{"push", "--quiet", "origin"}, refspecs...)
	_, stderr, code, err := fixture.client.run(ctx, token, directory, args...)
	if err != nil {
		return err.Error(), -1
	}
	return stderr, code
}

// PushAtomic pushes one batch with Git's negotiated --atomic capability. The
// option must precede the remote, so this is a separate helper rather than a
// refspec argument: a batch is either applied whole by receive-pack or not at
// all, and the managed pre-receive hook refuses the whole batch when any ref is
// denied.
func (fixture *GitFixture) PushAtomic(ctx context.Context, directory, token string, refspecs ...string) (string, int) {
	args := append([]string{"push", "--quiet", "--atomic", "origin"}, refspecs...)
	_, stderr, code, err := fixture.client.run(ctx, token, directory, args...)
	if err != nil {
		return err.Error(), -1
	}
	return stderr, code
}

// Commit writes one deterministic commit on the checked-out branch and returns
// its object identity.
func (fixture *GitFixture) Commit(t testing.TB, directory, message string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), gitCommandTimeout)
	defer cancel()
	file, err := os.CreateTemp(directory, "g6a-commit-")
	if err != nil {
		t.Fatalf("create commit file: %v", err)
	}
	if _, err := fmt.Fprintf(file, "%s\n%s\n", message, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		file.Close()
		t.Fatalf("write commit file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close commit file: %v", err)
	}
	if err := runGitOK(ctx, fixture.client, directory, "add", "--all"); err != nil {
		t.Fatalf("%v", err)
	}
	if err := runGitOK(ctx, fixture.client, directory,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", message); err != nil {
		t.Fatalf("%v", err)
	}
	commit, err := gitOutputOK(ctx, fixture.client, directory, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("%v", err)
	}
	return commit
}

// localGit runs one read-only host Git command outside a worktree.
func (fixture *GitFixture) localGit(t testing.TB, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), gitCommandTimeout)
	defer cancel()
	stdout, stderr, code, err := fixture.client.run(ctx, "", "", args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return stdout, stderr, code
}

// repositoryGit runs one read-only host Git command against one repository.
func (fixture *GitFixture) repositoryGit(t testing.TB, repository string, args ...string) (string, string, int) {
	t.Helper()
	return fixture.localGit(t, append([]string{"-C", fixture.RepositoryPath(repository)}, args...)...)
}

// RemoteRef returns the commit stored in one repository ref, or "" when the ref
// is absent.
func (fixture *GitFixture) RemoteRef(t testing.TB, repository, ref string) string {
	t.Helper()
	stdout, _, code := fixture.repositoryGit(t, repository, "rev-parse", "--verify", "--quiet", ref)
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(stdout)
}

// RefCommit returns the commit stored in one repository ref. It is the same
// local read as RemoteRef under the name the provisioning assertions use.
func (fixture *GitFixture) RefCommit(t testing.TB, repository, ref string) string {
	t.Helper()
	return fixture.RemoteRef(t, repository, ref)
}

// History returns the commits reachable from the repository's main ref, newest
// first.
func (fixture *GitFixture) History(t testing.TB, repository string) []string {
	t.Helper()
	return fixture.HistoryAt(t, repository, "refs/heads/main")
}

// HistoryAt returns the commits reachable from one ref, newest first.
func (fixture *GitFixture) HistoryAt(t testing.TB, repository, ref string) []string {
	t.Helper()
	stdout, stderr, code := fixture.repositoryGit(t, repository, "rev-list", ref)
	if code != 0 {
		t.Fatalf("git rev-list %s: exit %d: %s", ref, code, strings.TrimSpace(stderr))
	}
	var commits []string
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	return commits
}

// CommitParents returns the parents of one commit.
func (fixture *GitFixture) CommitParents(t testing.TB, repository, commit string) []string {
	t.Helper()
	stdout, stderr, code := fixture.repositoryGit(t, repository, "rev-list", "--parents", "-n", "1", commit)
	if code != 0 {
		t.Fatalf("git rev-list --parents %s: exit %d: %s", commit, code, strings.TrimSpace(stderr))
	}
	fields := strings.Fields(strings.TrimSpace(stdout))
	if len(fields) == 0 {
		t.Fatalf("git rev-list --parents returned no fields for %s", commit)
	}
	return append([]string(nil), fields[1:]...)
}

// FileAt returns the blob stored at path in one commit of a repository. It is
// the read-only proof that a committed candidate carries exact content.
func (fixture *GitFixture) FileAt(t testing.TB, repository, commit, path string) string {
	t.Helper()
	stdout, stderr, code := fixture.repositoryGit(t, repository, "cat-file", "blob", commit+":"+path)
	if code != 0 {
		t.Fatalf("read %s at %s in %s: exit %d: %s", path, commit, repository, code, strings.TrimSpace(stderr))
	}
	return stdout
}

// PolicyAt returns the committed policy blob of one commit.
func (fixture *GitFixture) PolicyAt(t testing.TB, repository, commit string) string {
	t.Helper()
	stdout, stderr, code := fixture.repositoryGit(t, repository, "cat-file", "blob", commit+":"+native.CommittedPolicyFile)
	if code != 0 {
		t.Fatalf("read policy at %s: exit %d: %s", commit, code, strings.TrimSpace(stderr))
	}
	return stdout
}

// AdminPolicy returns the accepted policy blob of the trusted admin ref.
func (fixture *GitFixture) AdminPolicy(t testing.TB) string {
	t.Helper()
	stdout, stderr, code := fixture.repositoryGit(t, GitAdminRepository, "cat-file", "blob", native.CommittedPolicyRef+":"+native.CommittedPolicyFile)
	if code != 0 {
		t.Fatalf("read accepted admin policy: exit %d: %s", code, strings.TrimSpace(stderr))
	}
	return stdout
}
