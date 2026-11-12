package gitkeep

// Keeper tests. They prove repository discovery from the storage layout,
// per-repository failure aggregation, a missing repository as a no-op, and the
// isolated native environment the keeper writes refs with.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/userland/git"
)

const testTenant = "tenant-a"

// fakeExecutor is a scripted native Git boundary keyed by repository directory.
type fakeExecutor struct {
	mu       sync.Mutex
	objects  map[string]bool
	refs     map[string]map[string]string
	failures map[string]error
	commands []string
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		objects:  make(map[string]bool),
		refs:     make(map[string]map[string]string),
		failures: make(map[string]error),
	}
}

func (executor *fakeExecutor) setObject(repository, commit string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.objects[repository+"|"+commit] = true
}

func (executor *fakeExecutor) ref(repository, ref string) string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.refs[repository][ref]
}

func (executor *fakeExecutor) fail(repository string, err error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.failures[repository] = err
}

func (executor *fakeExecutor) commandCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return len(executor.commands)
}

func (executor *fakeExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	repository := repositoryDirectory(command.Args)
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, repository+" "+strings.Join(command.Args, " "))
	if err := executor.failures[repository]; err != nil {
		return git.CommandResult{}, err
	}
	subcommand := 0
	if len(command.Args) >= 2 && command.Args[0] == "-C" {
		subcommand = 2
	}
	switch {
	case len(command.Args) > subcommand && command.Args[subcommand] == "rev-parse":
		commit := strings.TrimSuffix(command.Args[len(command.Args)-1], "^{commit}")
		if executor.objects[repository+"|"+commit] {
			return git.CommandResult{ExitCode: 0}, nil
		}
		return git.CommandResult{ExitCode: 1}, nil
	case len(command.Args) > subcommand && command.Args[subcommand] == "for-each-ref":
		var output strings.Builder
		for ref, commit := range executor.refs[repository] {
			if strings.HasPrefix(ref, command.Args[len(command.Args)-1]) {
				output.WriteString(ref + " " + commit + "\n")
			}
		}
		return git.CommandResult{ExitCode: 0, Stdout: []byte(output.String())}, nil
	case len(command.Args) > subcommand && command.Args[subcommand] == "update-ref":
		if executor.refs[repository] == nil {
			executor.refs[repository] = make(map[string]string)
		}
		if len(command.Args) > subcommand+1 && command.Args[subcommand+1] == "-d" {
			delete(executor.refs[repository], command.Args[subcommand+2])
			return git.CommandResult{ExitCode: 0}, nil
		}
		executor.refs[repository][command.Args[subcommand+1]] = command.Args[subcommand+2]
		return git.CommandResult{ExitCode: 0}, nil
	default:
		return git.CommandResult{}, errors.New("unexpected command")
	}
}

// repositoryDirectory extracts the `-C <path>` value and makes it relative to
// the test storage root's tenant directory so the fake can key by repository.
func repositoryDirectory(args []string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-C" {
			continue
		}
		path := filepath.ToSlash(args[index+1])
		if marker := "/" + testTenant + "/"; strings.Contains(path, marker) {
			return path[strings.LastIndex(path, marker)+len(marker):]
		}
		return path
	}
	return ""
}

// seedBare creates the minimum bare layout the enumeration recognizes.
func seedBare(t *testing.T, root, repository string) string {
	t.Helper()
	path := filepath.Join(root, testTenant, filepath.FromSlash(repository))
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(path, name), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	return path
}

func newKeeper(t *testing.T, root string, executor *fakeExecutor, repositories ...string) *Keeper {
	t.Helper()
	keeper, err := New(Config{StorageRoot: root, Repositories: repositories, Executor: executor})
	if err != nil {
		t.Fatalf("New keeper: %v", err)
	}
	return keeper
}

// TestKeeperEnumeratesEveryServedRepository proves the sweep discovers
// repositories from the tenant storage directory on every pass, including a
// repository provisioned after the keeper was built.
func TestKeeperEnumeratesEveryServedRepository(t *testing.T) {
	root := t.TempDir()
	seedBare(t, root, "shop/web")
	executor := newFakeExecutor()
	retained := strings.Repeat("a", 40)
	executor.setObject("shop/web", retained)

	keeper := newKeeper(t, root, executor)
	if err := keeper.KeepRefs(context.Background(), testTenant, []string{retained}); err != nil {
		t.Fatalf("KeepRefs: %v", err)
	}
	if got := executor.ref("shop/web", git.KeepRefPrefix+retained); got != retained {
		t.Fatalf("keep ref = %q; want %q", got, retained)
	}

	// A repository created after construction is swept on the next pass.
	seedBare(t, root, "shop/api")
	executor.setObject("shop/api", retained)
	if err := keeper.KeepRefs(context.Background(), testTenant, []string{retained}); err != nil {
		t.Fatalf("second KeepRefs: %v", err)
	}
	if got := executor.ref("shop/api", git.KeepRefPrefix+retained); got != retained {
		t.Fatalf("new repository keep ref = %q; want %q", got, retained)
	}
}

// TestKeeperAggregatesPerRepositoryFailures proves one damaged repository does
// not stop another from being reconciled, and that the failure is reported.
func TestKeeperAggregatesPerRepositoryFailures(t *testing.T) {
	root := t.TempDir()
	seedBare(t, root, "shop/broken")
	seedBare(t, root, "shop/healthy")
	executor := newFakeExecutor()
	retained := strings.Repeat("b", 40)
	executor.setObject("shop/healthy", retained)
	executor.fail("shop/broken", errors.New("native git unavailable"))

	err := newKeeper(t, root, executor).KeepRefs(context.Background(), testTenant, []string{retained})
	if err == nil {
		t.Fatal("KeepRefs hid a repository failure")
	}
	if !strings.Contains(err.Error(), "shop/broken") {
		t.Fatalf("error = %v; want the failing repository named", err)
	}
	if got := executor.ref("shop/healthy", git.KeepRefPrefix+retained); got != retained {
		t.Fatalf("healthy repository was not reconciled after another failed: keep ref = %q", got)
	}
}

// TestKeeperMissingRepositoryIsNoOp proves an explicitly configured or
// disappeared repository never turns the sweep into an error.
func TestKeeperMissingRepositoryIsNoOp(t *testing.T) {
	root := t.TempDir()
	// The tenant directory exists, but the configured repository does not.
	if err := os.MkdirAll(filepath.Join(root, testTenant), 0o750); err != nil {
		t.Fatal(err)
	}
	executor := newFakeExecutor()
	keeper := newKeeper(t, root, executor, "shop/missing")
	before := executor.commandCount()
	if err := keeper.KeepRefs(context.Background(), testTenant, []string{strings.Repeat("c", 40)}); err != nil {
		t.Fatalf("missing repository was an error: %v", err)
	}
	if after := executor.commandCount(); after != before {
		t.Fatalf("missing repository ran %d native commands", after-before)
	}

	// A tenant that has no directory at all is also a clean no-op.
	empty := newKeeper(t, t.TempDir(), executor)
	if err := empty.KeepRefs(context.Background(), testTenant, nil); err != nil {
		t.Fatalf("missing tenant directory was an error: %v", err)
	}
}

// TestKeeperRefusesAnUnmountedRoot proves the keeper never creates a missing
// root: an unmounted PVC must stop composition startup, not sweep an empty
// directory forever.
func TestKeeperRefusesAnUnmountedRoot(t *testing.T) {
	_, err := New(Config{StorageRoot: filepath.Join(t.TempDir(), "not-mounted"), Executor: newFakeExecutor()})
	if !errors.Is(err, ErrStorageRootUnavailable) {
		t.Fatalf("New error = %v; want ErrStorageRootUnavailable", err)
	}
}

// TestIsolatedEnvironmentDropsInheritedGitConfiguration proves the keeper's
// native boundary never inherits a hostile GIT_* setting from the process.
func TestIsolatedEnvironmentDropsInheritedGitConfiguration(t *testing.T) {
	t.Setenv("GIT_DIR", "/hostile/repository")
	t.Setenv("GIT_CONFIG_GLOBAL", "/hostile/global")
	t.Setenv("GIT_OBJECT_DIRECTORY", "/hostile/objects")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")
	environment := strings.Join(isolatedEnvironment(), "\n")
	for _, forbidden := range []string{"GIT_DIR=", "GIT_OBJECT_DIRECTORY=", "/hostile"} {
		if strings.Contains(environment, forbidden) {
			t.Fatalf("isolated environment carried %q:\n%s", forbidden, environment)
		}
	}
	for _, required := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/local/bin:/usr/bin"} {
		if !strings.Contains(environment, required) {
			t.Fatalf("isolated environment is missing %q:\n%s", required, environment)
		}
	}
}
