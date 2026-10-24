package workspacetest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// File is a path and its content for a commit.
type File = gitfx.File

// DefaultMaxBundleBytes bounds the bundle a LocalPreparer accepts.
const DefaultMaxBundleBytes int64 = 64 << 20

const (
	maxHeaderBytes   = 64 << 10
	maxPrerequisites = 16
	// minSecretScanLen is the shortest secret value scanned for; shorter values
	// would match innocent content.
	minSecretScanLen = 8
	// maxObjectList bounds the listing of the objects a bundle introduces.
	maxObjectList = 16 << 20
	// maxGitOutput bounds what any other Git command may print.
	maxGitOutput = 16 << 20
	// scratchPoll is how often a running command's scratch size is checked.
	scratchPoll = 10 * time.Millisecond
)

// DefaultMaxExpandedBytes bounds the expanded size of the objects one bundle
// introduces (commits, trees and blobs, uncompressed): a highly compressible
// bundle is refused from the object sizes, before any content is read.
const DefaultMaxExpandedBytes int64 = 256 << 20

// DefaultMaxScratchBytes bounds the on-disk size of one preparation's private
// scratch, including the transferred bundle, while Git writes into it.
const DefaultMaxScratchBytes int64 = 256 << 20

// LocalGit is the `development` Git: bare repositories in a directory served by
// the Git executable through the file transport. Its three ports are separate
// types so write authority is held only where it is exercised:
//
//   - Reader: workspace.RefReader (no write operation);
//   - Preparer: workspace.SourcePreparer, which verifies and stages an uploaded
//     bundle in private scratch exactly like the HTTPS reader (size, digest,
//     header, prerequisites, object integrity, descent from the pinned input,
//     injected secrets);
//   - Publisher: workspace.RefPublisher, whose Publish only accepts a source its
//     Preparer staged for the same repository, and pushes it as an
//     expected-old-ref update that never forces.
//
// There are no credentials: the repository is its own authority. Construct it
// with NewLocalGit (a private temporary root) or NewLocalGitAt and release it
// with Close; it needs no testing.TB. It is development-only: every port marks
// itself so production refuses it (SD-12). See MEMORY.md for what it does not
// model.
type LocalGit struct {
	root    string
	scratch string
	owned   bool
	maxBody int64
	// maxExpanded and maxScratch are the bounds of one preparation.
	maxExpanded int64
	maxScratch  int64
	seq         atomic.Int64

	mu     sync.Mutex
	closed bool
}

// NewLocalGit creates a private temporary root, removed by Close. It fails when
// the Git executable is missing.
func NewLocalGit() (*LocalGit, error) {
	root, err := os.MkdirTemp("", "kodama-localgit-")
	if err != nil {
		return nil, fmt.Errorf("%w: temporary root: %v", workspace.ErrUnavailable, err)
	}
	g, err := newLocalGit(root, true)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	return g, nil
}

// NewLocalGitAt serves repositories under root, which must already exist. Close
// removes only the adapter's scratch, never root.
func NewLocalGitAt(root string) (*LocalGit, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: root: %v", workspace.ErrInvalidConfiguration, err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: root %q is not a directory", workspace.ErrInvalidConfiguration, root)
	}
	return newLocalGit(abs, false)
}

func newLocalGit(root string, owned bool) (*LocalGit, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("%w: local Git needs the git executable: %v", workspace.ErrInvalidConfiguration, err)
	}
	scratch := filepath.Join(root, ".scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("%w: scratch: %v", workspace.ErrUnavailable, err)
	}
	return &LocalGit{root: filepath.Clean(root), scratch: scratch, owned: owned, maxBody: DefaultMaxBundleBytes,
		maxExpanded: DefaultMaxExpandedBytes, maxScratch: DefaultMaxScratchBytes}, nil
}

// LocalGitFor is the test fixture: NewLocalGit, closed when the test ends.
func LocalGitFor(t testing.TB) *LocalGit {
	t.Helper()
	g, err := NewLocalGit()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// Close removes the adapter's scratch and, for a temporary root, the root with
// every repository in it. It is idempotent.
func (g *LocalGit) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if g.owned {
		return os.RemoveAll(g.root)
	}
	return os.RemoveAll(g.scratch)
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*LocalGit) DevelopmentOnly() bool { return true }

// Root is the directory that holds every repository the adapter will address.
func (g *LocalGit) Root() string { return g.root }

// SetMaxBundleBytes changes the transfer limit of the Preparer.
func (g *LocalGit) SetMaxBundleBytes(n int64) { g.maxBody = n }

// SetMaxExpandedBytes changes the bound on the expanded size of the objects a
// bundle introduces.
func (g *LocalGit) SetMaxExpandedBytes(n int64) { g.maxExpanded = n }

// SetMaxScratchBytes changes the bound on one preparation's scratch space.
func (g *LocalGit) SetMaxScratchBytes(n int64) { g.maxScratch = n }

// ScratchEntries counts the preparations whose scratch still exists: zero once
// every one was released or refused.
func (g *LocalGit) ScratchEntries() int {
	entries, err := os.ReadDir(g.scratch)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "prepare-") {
			n++
		}
	}
	return n
}

// LocalRepo is one bare repository.
type LocalRepo struct {
	g       *LocalGit
	Name    string
	Path    string
	BaseRef string
	Initial workspace.Commit
}

// git runs the Git executable on dir with a clean, deterministic environment and
// no ambient configuration, hooks or prompts.
func git(ctx context.Context, dir string, stdin []byte, env []string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, stdin, env, args...)
	return strings.TrimSpace(string(out)), err
}

// gitConfig is applied to every invocation: no hooks or maintenance, objects
// are checked for integrity on every transfer and kept packed rather than
// exploded into loose files, as the HTTPS adapter's engine does.
var gitConfig = []string{
	"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "advice.detachedHead=false",
	"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "submodule.recurse=false",
	"-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true", "-c", "fetch.unpackLimit=1", "-c", "transfer.unpackLimit=1",
}

func gitCommand(ctx context.Context, dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, gitConfig...), args...)...)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_AUTHOR_NAME=Kodama", "GIT_AUTHOR_EMAIL=kodama@example.invalid", "GIT_COMMITTER_NAME=Kodama", "GIT_COMMITTER_EMAIL=kodama@example.invalid",
	}, env...)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func gitRaw(ctx context.Context, dir string, stdin []byte, env []string, args ...string) ([]byte, error) {
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	return gitRun(ctx, runSpec{dir: dir, stdin: in, env: env, maxOut: maxGitOutput}, args...)
}

// runSpec bounds one Git invocation: what it may print and, when watch is set,
// how large that directory may grow while it runs.
type runSpec struct {
	dir      string
	stdin    io.Reader
	env      []string
	maxOut   int
	watch    string
	maxWatch int64
}

// boundedBuffer keeps at most max bytes and fails the writer beyond it, which
// ends the command with a broken pipe instead of buffering its output.
type boundedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

// dirSize sums the sizes of the files under root.
func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// gitRun executes Git under the spec's bounds: the output is capped while the
// command runs and the watched directory is polled so a command that expands
// objects is stopped as soon as it passes its scratch bound.
func gitRun(ctx context.Context, sp runSpec, args ...string) ([]byte, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(ctx, sp.dir, sp.env, args...)
	if sp.stdin != nil {
		cmd.Stdin = sp.stdin
	}
	stdout := &boundedBuffer{max: sp.maxOut}
	stderr := &boundedBuffer{max: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, &truncWriter{b: stderr}

	var overflow atomic.Bool
	stop, done := make(chan struct{}), make(chan struct{})
	if sp.watch != "" {
		go func() {
			defer close(done)
			t := time.NewTicker(scratchPoll)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if dirSize(sp.watch) > sp.maxWatch {
						overflow.Store(true)
						cancel()
						return
					}
				}
			}
		}()
	} else {
		close(done)
	}
	err := cmd.Run()
	close(stop)
	<-done
	if sp.watch != "" && dirSize(sp.watch) > sp.maxWatch {
		overflow.Store(true)
	}
	switch {
	case overflow.Load():
		return nil, fmt.Errorf("%w: scratch space exceeded %d bytes", workspace.ErrLimitExceeded, sp.maxWatch)
	case parent.Err() != nil:
		return nil, fmt.Errorf("git %s: %w", args[0], parent.Err())
	case stdout.overflow:
		return nil, fmt.Errorf("%w: git output exceeded %d bytes", workspace.ErrLimitExceeded, sp.maxOut)
	case err != nil:
		return nil, &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.buf.String())}
	}
	return stdout.buf.Bytes(), nil
}

// truncWriter keeps the head of what a command prints on stderr and discards the
// rest without failing the command.
type truncWriter struct{ b *boundedBuffer }

func (w *truncWriter) Write(p []byte) (int, error) {
	if room := w.b.max - w.b.buf.Len(); room > 0 {
		w.b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args[:min(len(e.args), 2)], " "), e.err, e.stderr)
}
func (e *gitError) Unwrap() error { return e.err }

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// CreateRepo creates a bare repository with an initial commit on refs/heads/main.
func (g *LocalGit) CreateRepo(ctx context.Context, name string) (*LocalRepo, error) {
	if name == "" || strings.ContainsAny(name, `/\.`) || strings.HasPrefix(name, "-") {
		return nil, fmt.Errorf("%w: repository name %q", workspace.ErrInvalidValue, name)
	}
	path := filepath.Join(g.root, name+".git")
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%w: repository %q already exists", workspace.ErrBranchExists, name)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	r := &LocalRepo{g: g, Name: name, Path: path, BaseRef: "refs/heads/main"}
	steps := func() (string, error) {
		if _, err := git(ctx, path, nil, nil, "init", "-q", "--bare", "-b", "main"); err != nil {
			return "", err
		}
		blob, err := git(ctx, path, []byte("# repository "+name+"\n"), nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		tree, err := git(ctx, path, []byte("100644 blob "+blob+"\tREADME.md\n"), nil, "mktree")
		if err != nil {
			return "", err
		}
		commit, err := git(ctx, path, nil, nil, "commit-tree", tree, "-m", "initial")
		if err != nil {
			return "", err
		}
		_, err = git(ctx, path, nil, nil, "update-ref", r.BaseRef, commit)
		return commit, err
	}
	commit, err := steps()
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	r.Initial = workspace.Commit(commit)
	return r, nil
}

// NewRepo is CreateRepo for tests.
func (g *LocalGit) NewRepo(t testing.TB, name string) *LocalRepo {
	t.Helper()
	r, err := g.CreateRepo(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Ref is the workspace.RepoRef addressing the repository. The secret references
// are carried but never resolved: the local transport has no credentials.
func (r *LocalRepo) Ref(scope domain.Scope) workspace.RepoRef {
	return workspace.RepoRef{Scope: scope, URL: r.Path, CheckoutSecretRef: "secret://local/checkout", PublisherSecretRef: "secret://local/publisher"}
}

// CommitFiles creates a commit on top of parent with the files added, stores it
// in the repository without moving any ref, and returns it.
func (r *LocalRepo) CommitFiles(ctx context.Context, parent workspace.Commit, msg string, files ...File) (workspace.Commit, error) {
	dir, err := os.MkdirTemp(r.g.scratch, "idx-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := git(ctx, r.Path, nil, env, "read-tree", string(parent)); err != nil {
		return "", err
	}
	for _, f := range files {
		blob, err := git(ctx, r.Path, []byte(f.Content), nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", err
		}
		if _, err := git(ctx, r.Path, nil, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+f.Path); err != nil {
			return "", err
		}
	}
	tree, err := git(ctx, r.Path, nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	c, err := git(ctx, r.Path, nil, nil, "commit-tree", tree, "-p", string(parent), "-m", msg)
	return workspace.Commit(c), err
}

// Commit is CommitFiles for tests.
func (r *LocalRepo) Commit(t testing.TB, parent workspace.Commit, msg string, files ...File) workspace.Commit {
	t.Helper()
	c, err := r.CommitFiles(context.Background(), parent, msg, files...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Bundle creates what the step entrypoint uploads: an incremental bundle of
// base..commit holding one ref, with the files added on top of base. The
// repository's refs are not changed.
func (r *LocalRepo) Bundle(ctx context.Context, base workspace.Commit, msg string, files ...File) (workspace.Commit, []byte, error) {
	work, err := os.MkdirTemp(r.g.scratch, "bundle-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(work)
	if _, err := git(ctx, work, nil, nil, "init", "-q", "-b", "scratch"); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "fetch", "-q", "--no-tags", r.Path, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "checkout", "-q", "--detach", string(base)); err != nil {
		return "", nil, err
	}
	for _, f := range files {
		p := filepath.Join(work, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			return "", nil, err
		}
	}
	if _, err := git(ctx, work, nil, nil, "add", "-A"); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "commit", "-q", "-m", msg); err != nil {
		return "", nil, err
	}
	commit, err := git(ctx, work, nil, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "update-ref", "refs/kodama/candidate", commit); err != nil {
		return "", nil, err
	}
	out := filepath.Join(work, "source.bundle")
	if _, err := git(ctx, work, nil, nil, "bundle", "create", "--quiet", out, string(base)+"..refs/kodama/candidate"); err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(out)
	return workspace.Commit(commit), data, err
}

// CorruptBundle returns an upload whose commit is correctly hashed but not a
// valid Git object (it names no author or committer), so only the transfer's
// object checks can refuse it. The repository's refs are not changed.
func (r *LocalRepo) CorruptBundle(ctx context.Context, base workspace.Commit) (workspace.Commit, []byte, error) {
	work, err := os.MkdirTemp(r.g.scratch, "bundle-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(work)
	if _, err := git(ctx, work, nil, nil, "init", "-q", "-b", "scratch"); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "fetch", "-q", "--no-tags", r.Path, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", nil, err
	}
	return CorruptBundleIn(ctx, work, base)
}

// CorruptBundleIn builds the upload of a correctly hashed commit that is not a
// valid Git object (no author, no committer) on top of base, in work: a
// repository that holds base. The digest, the header and the prerequisites of
// the bundle are all fine; only a transfer's object checks can refuse it. Every
// SourcePreparer is held to the same refusal, so the fixture of any adapter may
// use it.
func CorruptBundleIn(ctx context.Context, work string, base workspace.Commit) (workspace.Commit, []byte, error) {
	tree, err := git(ctx, work, nil, nil, "rev-parse", string(base)+"^{tree}")
	if err != nil {
		return "", nil, err
	}
	raw := fmt.Sprintf("tree %s\nparent %s\n\nA commit with no author or committer.\n", tree, base)
	commit, err := git(ctx, work, []byte(raw), nil, "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
	if err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, work, nil, nil, "update-ref", "refs/kodama/candidate", commit); err != nil {
		return "", nil, err
	}
	out := filepath.Join(work, "malformed.bundle")
	if _, err := git(ctx, work, nil, nil, "bundle", "create", "--quiet", out, string(base)+"..refs/kodama/candidate"); err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(out)
	return workspace.Commit(commit), data, err
}

// CompressibleBundle returns an upload of one file of size zero bytes: it
// compresses to a few kilobytes and expands to size.
func (r *LocalRepo) CompressibleBundle(ctx context.Context, base workspace.Commit, size int) (workspace.Commit, []byte, error) {
	return r.Bundle(ctx, base, "compressible", File{Path: "zeros.bin", Content: string(make([]byte, size))})
}

// BundleT is Bundle for tests.
func (r *LocalRepo) BundleT(t testing.TB, base workspace.Commit, msg string, files ...File) (workspace.Commit, []byte) {
	t.Helper()
	c, b, err := r.Bundle(context.Background(), base, msg, files...)
	if err != nil {
		t.Fatal(err)
	}
	return c, b
}

// Tip returns the commit ref points at, or "" when it does not exist.
func (r *LocalRepo) Tip(t testing.TB, ref string) workspace.Commit {
	t.Helper()
	c, ok, err := resolve(context.Background(), r.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return ""
	}
	return c
}

// HumanPush plays an engineer: a commit on top of the branch tip, fast-forwarded
// onto the branch directly.
func (r *LocalRepo) HumanPush(t testing.TB, branch, msg string, files ...File) workspace.Commit {
	t.Helper()
	tip := r.Tip(t, branch)
	if tip == "" {
		t.Fatalf("branch %s does not exist", branch)
	}
	c := r.Commit(t, tip, msg, files...)
	if _, err := git(context.Background(), r.Path, nil, nil, "update-ref", branch, string(c), string(tip)); err != nil {
		t.Fatal(err)
	}
	return c
}

// Foreign returns a commit the repository does not hold: it exists only in
// another repository of the same root.
func (r *LocalRepo) Foreign(t testing.TB) workspace.Commit {
	t.Helper()
	return r.g.NewRepo(t, fmt.Sprintf("%s-foreign-%d", r.Name, r.g.seq.Add(1))).Initial
}

func resolve(ctx context.Context, dir, ref string) (workspace.Commit, bool, error) {
	out, err := git(ctx, dir, nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	c := workspace.Commit(out)
	return c, true, c.Validate()
}

func hasCommit(ctx context.Context, dir string, c workspace.Commit) (bool, error) {
	_, err := git(ctx, dir, nil, nil, "cat-file", "-e", string(c)+"^{commit}")
	if err == nil {
		return true, nil
	}
	if exitCode(err) > 0 {
		return false, nil
	}
	return false, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
}

func isAncestor(ctx context.Context, dir string, ancestor, descendant workspace.Commit) (bool, error) {
	_, err := git(ctx, dir, nil, nil, "merge-base", "--is-ancestor", string(ancestor), string(descendant))
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
}

// repoDir confines an address to the adapter's root: the URL names a directory
// directly under it.
func (g *LocalGit) repoDir(repo workspace.RepoRef) (string, error) {
	clean := filepath.Clean(repo.URL)
	if repo.URL == "" || !filepath.IsAbs(clean) || filepath.Dir(clean) != g.root || !strings.HasSuffix(clean, ".git") {
		return "", fmt.Errorf("%w: repository %q is not served by this adapter", workspace.ErrInvalidValue, repo.URL)
	}
	if err := repo.Scope.Validate(); err != nil {
		return "", err
	}
	if _, err := os.Stat(clean); err != nil {
		return "", fmt.Errorf("%w: repository is not available", workspace.ErrUnavailable)
	}
	return clean, nil
}

// ---- reader --------------------------------------------------------------------

// LocalReader is the read side: it holds no write operation.
type LocalReader struct{ g *LocalGit }

var _ workspace.RefReader = LocalReader{}

// Reader returns the read-only side.
func (g *LocalGit) Reader() LocalReader { return LocalReader{g} }

// DevelopmentOnly marks the port for the composition root (SD-12).
func (LocalReader) DevelopmentOnly() bool { return true }

// ResolveRef implements workspace.RefReader.
func (r LocalReader) ResolveRef(ctx context.Context, repo workspace.RepoRef, ref string) (workspace.Commit, bool, error) {
	dir, err := r.g.repoDir(repo)
	if err != nil {
		return "", false, err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return "", false, err
	}
	return resolve(ctx, dir, ref)
}

// BranchContains implements workspace.RefReader.
func (r LocalReader) BranchContains(ctx context.Context, repo workspace.RepoRef, branch string, commit workspace.Commit) (workspace.BranchInfo, error) {
	var info workspace.BranchInfo
	dir, err := r.g.repoDir(repo)
	if err != nil {
		return info, err
	}
	if err := workspace.ValidateBranch(branch); err != nil {
		return info, err
	}
	if commit != "" {
		if err := commit.Validate(); err != nil {
			return info, err
		}
	}
	tip, ok, err := resolve(ctx, dir, branch)
	if err != nil || !ok {
		return info, err
	}
	info.Tip, info.Exists = tip, true
	if commit == "" {
		return info, nil
	}
	if commit == tip {
		info.Contains = true
		return info, nil
	}
	has, err := hasCommit(ctx, dir, commit)
	if err != nil || !has {
		return info, err
	}
	info.Contains, err = isAncestor(ctx, dir, commit, tip)
	return info, err
}

// ---- source preparer -------------------------------------------------------------

// LocalPreparer verifies an uploaded bundle against the pinned source and stages
// it for publication.
type LocalPreparer struct{ g *LocalGit }

var _ workspace.SourcePreparer = LocalPreparer{}

// Preparer returns the bundle verification side.
func (g *LocalGit) Preparer() LocalPreparer { return LocalPreparer{g} }

// DevelopmentOnly marks the port for the composition root (SD-12).
func (LocalPreparer) DevelopmentOnly() bool { return true }

type bundleHeader struct {
	prerequisites []string
	refs          map[string]string
	hashLen       int
}

func bundleErr(reason string) error { return &workspace.BundleError{Reason: reason} }

// parseBundleHeader reads the header only, so a malformed or hostile bundle is
// rejected before Git expands any object.
func parseBundleHeader(path string) (*bundleHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, maxHeaderBytes), 4096)
	sig, err := rd.ReadString('\n')
	if err != nil {
		return nil, bundleErr(workspace.ReasonTruncated)
	}
	sig = strings.TrimSuffix(sig, "\n")
	h := &bundleHeader{refs: map[string]string{}, hashLen: 40}
	if sig != "# v2 git bundle" && sig != "# v3 git bundle" {
		return nil, bundleErr(workspace.ReasonUnsupportedBound)
	}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, bundleErr(workspace.ReasonTruncated)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return h, nil
		case strings.HasPrefix(line, "@"):
			if sig != "# v3 git bundle" {
				return nil, bundleErr(workspace.ReasonUnsupportedBound)
			}
			k, v, _ := strings.Cut(line[1:], "=")
			switch {
			case k == "object-format" && v == "sha256":
				h.hashLen = 64
			case k == "object-format" && v == "sha1":
			default:
				return nil, bundleErr(workspace.ReasonUnsupportedBound)
			}
		case strings.HasPrefix(line, "-"):
			oid, _, _ := strings.Cut(line[1:], " ")
			if len(h.prerequisites) >= maxPrerequisites {
				return nil, bundleErr(workspace.ReasonBadPrerequisite)
			}
			h.prerequisites = append(h.prerequisites, oid)
		default:
			oid, name, ok := strings.Cut(line, " ")
			if !ok || name == "" || len(h.refs) >= 1 {
				return nil, bundleErr(workspace.ReasonCommitMismatch)
			}
			h.refs[name] = oid
		}
	}
}

func copyBounded(path string, body io.Reader, max int64) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, max+1))
	if err != nil {
		return n, "", err
	}
	if n > max {
		return n, "", fmt.Errorf("%w: bundle exceeds the %d byte transfer limit", workspace.ErrLimitExceeded, max)
	}
	return n, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func classifyBundleFailure(err error) error {
	if errors.Is(err, workspace.ErrLimitExceeded) || errors.Is(err, workspace.ErrUnavailable) || errors.Is(err, context.Canceled) {
		return err
	}
	var ge *gitError
	msg := ""
	if errors.As(err, &ge) {
		msg = ge.stderr
	}
	switch {
	case strings.Contains(msg, "prerequisite"):
		return &workspace.BundleError{Reason: workspace.ReasonBadPrerequisite, Cause: err}
	case strings.Contains(msg, "early EOF") || strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "truncated"):
		return &workspace.BundleError{Reason: workspace.ReasonTruncated, Cause: err}
	}
	return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects, Cause: err}
}

// Prepare implements workspace.SourcePreparer. It verifies, in private scratch:
// the transferred size and digest, the header (one ref at the proposed commit;
// prerequisites limited to the pinned input and expected tip), the bundle's
// object integrity, that the commit descends from the pinned input, and that no
// injected secret appears in the objects the bundle introduces.
func (p LocalPreparer) Prepare(ctx context.Context, req workspace.PrepareRequest) (workspace.Prepared, error) {
	g := p.g
	repoPath, err := g.repoDir(req.Repo)
	if err != nil {
		return nil, err
	}
	if err := workspace.ValidateBranch(req.Branch); err != nil {
		return nil, err
	}
	for _, c := range []workspace.Commit{req.Input, req.ExpectedTip, req.Commit} {
		if err := c.Validate(); err != nil {
			return nil, err
		}
	}
	if req.Body == nil || req.Digest == "" {
		return nil, bundleErr(workspace.ReasonTruncated)
	}
	if req.Size > g.maxBody {
		return nil, fmt.Errorf("%w: bundle is %d bytes, over the %d byte transfer limit", workspace.ErrLimitExceeded, req.Size, g.maxBody)
	}
	scratch, err := os.MkdirTemp(g.scratch, "prepare-")
	if err != nil {
		return nil, fmt.Errorf("%w: scratch: %v", workspace.ErrUnavailable, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(scratch)
		}
	}()

	// 1. Bounded transfer into scratch, hashing as it arrives.
	bundlePath := filepath.Join(scratch, "source.bundle")
	n, sum, err := copyBounded(bundlePath, req.Body, g.maxBody)
	switch {
	case errors.Is(err, workspace.ErrLimitExceeded):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: reading the bundle: %w", workspace.ErrUnavailable, err)
	case req.Size > 0 && n < req.Size:
		return nil, bundleErr(workspace.ReasonTruncated)
	case (req.Size > 0 && n != req.Size) || sum != req.Digest:
		return nil, bundleErr(workspace.ReasonDigestMismatch)
	}

	// 2. Header checks before any object is expanded.
	h, err := parseBundleHeader(bundlePath)
	if err != nil {
		return nil, err
	}
	if h.hashLen != len(req.Commit) {
		return nil, bundleErr(workspace.ReasonUnsupportedBound)
	}
	var bundleRef string
	for name, oid := range h.refs {
		if oid != string(req.Commit) {
			return nil, bundleErr(workspace.ReasonCommitMismatch)
		}
		bundleRef = name
	}
	if bundleRef == "" {
		return nil, bundleErr(workspace.ReasonCommitMismatch)
	}
	for _, pre := range h.prerequisites {
		if pre != string(req.Input) && pre != string(req.ExpectedTip) {
			return nil, bundleErr(workspace.ReasonBadPrerequisite)
		}
	}

	// 3. Pinned source and the current tip, from the repository, into a private
	// bare repository.
	// Every command that writes objects runs under the scratch bound.
	bounded := func(dir string, args ...string) error {
		_, err := gitRun(ctx, runSpec{dir: dir, maxOut: maxGitOutput, watch: scratch, maxWatch: g.maxScratch}, args...)
		return err
	}
	dir := filepath.Join(scratch, "work")
	if _, err := git(ctx, scratch, nil, nil, "init", "-q", "--bare", "-b", "scratch", dir); err != nil {
		return nil, fmt.Errorf("%w: %v", workspace.ErrUnavailable, err)
	}
	tip, exists, err := resolve(ctx, repoPath, req.Branch)
	if err != nil {
		return nil, err
	}
	if err := bounded(dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", repoPath, "+refs/heads/*:refs/kodama/heads/*"); err != nil {
		if errors.Is(err, workspace.ErrLimitExceeded) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: fetching the pinned source: %v", workspace.ErrUnavailable, err)
	}
	if has, err := hasCommit(ctx, dir, req.Input); err != nil {
		return nil, err
	} else if !has {
		return nil, fmt.Errorf("%w: the pinned input commit is not available", workspace.ErrUnavailable)
	}

	// 4. Verify the bundle's prerequisites, then import it with the transfer
	// object checks enabled (fetch.fsckObjects): a correctly hashed but malformed
	// object is refused as corrupt, and the import cannot outgrow the scratch bound.
	if _, err := git(ctx, dir, nil, nil, "bundle", "verify", "--quiet", bundlePath); err != nil {
		return nil, classifyBundleFailure(err)
	}
	if err := bounded(dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundlePath, bundleRef+":refs/kodama/candidate"); err != nil {
		return nil, classifyBundleFailure(err)
	}
	got, ok2, err := resolve(ctx, dir, "refs/kodama/candidate")
	if err != nil || !ok2 || got != req.Commit {
		return nil, bundleErr(workspace.ReasonCommitMismatch)
	}
	desc, err := isAncestor(ctx, dir, req.Input, req.Commit)
	if err != nil {
		return nil, err
	}
	if !desc {
		return nil, bundleErr(workspace.ReasonNotDescendant)
	}

	// 5. Credentials never enter source: scan only the objects introduced.
	if err := g.scanNewObjects(ctx, scratch, dir, req.Commit, req.Input, req.Needles); err != nil {
		return nil, err
	}
	ok = true
	if !exists {
		tip = ""
	}
	return &LocalPrepared{repo: repoPath, dir: dir, scratch: scratch, observed: tip, commit: req.Commit}, nil
}

// scanNewObjects checks every commit, tree and blob reachable from commit but
// not from input (intermediate commits and messages included). The introduced
// objects' expanded sizes are summed from Git's size query first, so a
// compressed bundle that would expand past the bound is refused without
// reading any content; when needles are present the content is then streamed
// through a bounded window, never buffered whole, and the refused bundle's
// scan process is stopped rather than drained.
func (g *LocalGit) scanNewObjects(ctx context.Context, scratch, dir string, commit, input workspace.Commit, needles [][]byte) error {
	var usable [][]byte
	for _, n := range needles {
		if len(n) >= minSecretScanLen {
			usable = append(usable, n)
		}
	}
	listing, err := gitRun(ctx, runSpec{dir: dir, maxOut: maxObjectList, watch: scratch, maxWatch: g.maxScratch},
		"rev-list", "--objects", string(commit), "^"+string(input))
	if err != nil {
		return classifyBundleFailure(err)
	}
	var oids []string
	for _, line := range strings.Split(string(listing), "\n") {
		if oid, _, _ := strings.Cut(line, " "); oid != "" {
			oids = append(oids, oid)
		}
	}
	if len(oids) == 0 {
		return nil
	}
	stdin := []byte(strings.Join(oids, "\n") + "\n")
	sizes, err := gitRun(ctx, runSpec{dir: dir, stdin: bytes.NewReader(stdin), maxOut: maxObjectList}, "cat-file", "--batch-check")
	if err != nil {
		return classifyBundleFailure(err)
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(sizes)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		if total += size; total > g.maxExpanded {
			return fmt.Errorf("%w: the bundle's objects expand beyond %d bytes", workspace.ErrLimitExceeded, g.maxExpanded)
		}
	}
	if len(usable) == 0 {
		return nil
	}

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(sctx, dir, nil, "cat-file", "--batch")
	cmd.Stdin = bytes.NewReader(stdin)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
	}
	// stop ends the scan process at once; its remaining output is not read.
	stop := func() { cancel(); _ = cmd.Wait() }
	rd := bufio.NewReaderSize(stdout, 64<<10)
	var read int64
	for range oids {
		header, err := rd.ReadString('\n')
		if err != nil {
			stop()
			return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			stop()
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			stop()
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		if read += size; read > g.maxExpanded {
			stop()
			return fmt.Errorf("%w: the objects to scan exceed %d bytes", workspace.ErrLimitExceeded, g.maxExpanded)
		}
		body := io.LimitReader(rd, size)
		found := containsAny(body, usable)
		if _, err := io.Copy(io.Discard, body); err != nil {
			stop()
			return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
		}
		if _, err := rd.Discard(1); err != nil {
			stop()
			return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
		}
		if found {
			stop()
			return workspace.ErrSecretInSource
		}
	}
	stop()
	return nil
}

// containsAny streams r looking for any needle through an overlap window the
// size of the longest needle, so memory stays bounded whatever the object size.
func containsAny(r io.Reader, needles [][]byte) bool {
	maxLen := 0
	for _, n := range needles {
		maxLen = max(maxLen, len(n))
	}
	buf := make([]byte, 0, 64<<10+maxLen)
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for _, needle := range needles {
			if bytes.Contains(buf, needle) {
				return true
			}
		}
		if len(buf) > maxLen-1 {
			buf = append(buf[:0], buf[len(buf)-(maxLen-1):]...)
		}
		if err != nil {
			return false
		}
	}
}

// LocalPrepared is verified source staged in private scratch, ready to publish to
// the repository it was prepared against.
type LocalPrepared struct {
	repo     string
	dir      string
	scratch  string
	commit   workspace.Commit
	observed workspace.Commit
}

var _ workspace.Prepared = (*LocalPrepared)(nil)

// ObservedTip implements workspace.Prepared.
func (p *LocalPrepared) ObservedTip() workspace.Commit { return p.observed }

// DescendsFrom implements workspace.Prepared.
func (p *LocalPrepared) DescendsFrom(ctx context.Context, c workspace.Commit) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	has, err := hasCommit(ctx, p.dir, c)
	if err != nil || !has {
		return false, err
	}
	return isAncestor(ctx, p.dir, c, p.commit)
}

// Release removes the private scratch.
func (p *LocalPrepared) Release() { _ = os.RemoveAll(p.scratch) }

// ---- publisher -------------------------------------------------------------------

// LocalPublisher is the only side that writes.
type LocalPublisher struct{ g *LocalGit }

var _ workspace.RefPublisher = LocalPublisher{}

// Publisher returns the write side.
func (g *LocalGit) Publisher() LocalPublisher { return LocalPublisher{g} }

// DevelopmentOnly marks the port for the composition root (SD-12).
func (LocalPublisher) DevelopmentOnly() bool { return true }

// Publish implements workspace.RefPublisher: a fast-forward check against the
// prepared source, then an expected-old-ref push that fails when the branch
// moved. It never forces.
func (p LocalPublisher) Publish(ctx context.Context, repo workspace.RepoRef, req workspace.PublishRequest) error {
	dir, err := p.g.repoDir(repo)
	if err != nil {
		return err
	}
	if err := workspace.ValidateBranch(req.Branch); err != nil {
		return err
	}
	if err := req.Expected.Validate(); err != nil {
		return err
	}
	if err := req.Commit.Validate(); err != nil {
		return err
	}
	src, ok := req.Source.(*LocalPrepared)
	if !ok || src == nil || src.commit != req.Commit || src.repo != dir {
		return fmt.Errorf("%w: publication source was not prepared for this commit", workspace.ErrInvalidValue)
	}
	ff, err := src.DescendsFrom(ctx, req.Expected)
	if err != nil {
		return err
	}
	if !ff {
		return &workspace.ConflictError{Reason: "not_fast_forward"}
	}
	if _, err := git(ctx, src.dir, nil, nil, "push", "--quiet", "--no-verify", "--no-recurse-submodules",
		"--force-with-lease="+req.Branch+":"+string(req.Expected), dir, string(req.Commit)+":"+req.Branch); err != nil {
		return fmt.Errorf("%w: expected-old-ref update refused: %v", workspace.ErrUnavailable, err)
	}
	return nil
}

// CreateRef implements workspace.RefPublisher: create ref at commit only when it
// is absent, adopting an existing ref already at commit.
func (p LocalPublisher) CreateRef(ctx context.Context, repo workspace.RepoRef, ref string, commit workspace.Commit, from string) error {
	dir, err := p.g.repoDir(repo)
	if err != nil {
		return err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return err
	}
	if err := workspace.ValidateBranch(from); err != nil {
		return err
	}
	if err := commit.Validate(); err != nil {
		return err
	}
	existing, found, err := resolve(ctx, dir, ref)
	if err != nil {
		return err
	}
	if found {
		if existing == commit {
			return nil
		}
		return workspace.ErrBranchExists
	}
	tip, ok, err := resolve(ctx, dir, from)
	if err != nil {
		return err
	}
	reachable := false
	if ok {
		if has, err := hasCommit(ctx, dir, commit); err != nil {
			return err
		} else if has {
			if reachable, err = isAncestor(ctx, dir, commit, tip); err != nil {
				return err
			}
		}
	}
	if !reachable {
		return fmt.Errorf("%w: commit is not reachable from %s", workspace.ErrInvalidValue, from)
	}
	// The empty old value makes the update succeed only when ref is absent.
	if _, perr := git(ctx, dir, nil, nil, "update-ref", ref, string(commit), ""); perr != nil {
		if now, found, rerr := resolve(ctx, dir, ref); rerr == nil && found {
			if now == commit {
				return nil
			}
			return workspace.ErrBranchExists
		}
		return fmt.Errorf("%w: %v", workspace.ErrUnavailable, perr)
	}
	return nil
}

// DeleteRef implements workspace.RefPublisher: delete ref only while it still
// points at expected; an absent ref is already deleted.
func (p LocalPublisher) DeleteRef(ctx context.Context, repo workspace.RepoRef, ref string, expected workspace.Commit) error {
	dir, err := p.g.repoDir(repo)
	if err != nil {
		return err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	existing, found, err := resolve(ctx, dir, ref)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if existing != expected {
		return fmt.Errorf("%w: reference moved; refusing to delete", workspace.ErrStale)
	}
	if _, perr := git(ctx, dir, nil, nil, "update-ref", "-d", ref, string(expected)); perr != nil {
		if _, found, rerr := resolve(ctx, dir, ref); rerr == nil && !found {
			return nil
		}
		return fmt.Errorf("%w: %v", workspace.ErrUnavailable, perr)
	}
	return nil
}
