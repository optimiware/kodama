package entrypoint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Git's own object naming, not a security hash of ours
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// sealedGit runs Git over the shared run_shared /workspace without ever trusting
// what is stored in it.
//
// The scratch is written by earlier steps, which run arbitrary tenant code under
// their own images: anything in /workspace/.git can have been changed by them. A
// plain `git add` there would read `.git/config` and execute whatever it names (a
// clean filter, `core.fsmonitor`, `core.hooksPath`, a credential helper, a diff
// driver, `include.path`, a GPG program), follow a `.git` that is a symlink or a
// `gitdir:` file, and believe repository metadata owned by someone else. The
// control plane re-verifies every proposal in its own scratch, but the entrypoint
// must not run the tenant's configuration with its own authority first (it holds
// the attempt's token and secrets) and must report an honest workspace state.
//
// So the entrypoint never lets Git discover the workspace repository. It reads
// exactly three things from /workspace/.git as data, each through a checked,
// non-following open: the object directory, HEAD and the one ref HEAD names. Git
// itself runs against a private repository in the attempt's scratch (its config,
// hooks and info directories are the entrypoint's own and empty), pointed at the
// workspace's objects (GIT_OBJECT_DIRECTORY) and work tree (GIT_WORK_TREE) with a
// private index. Objects are content-addressed; new ones are written into the
// workspace's store so the next holder finds them. Nothing in the workspace's
// config, hooks, attributes drivers or info files is ever read.
type sealedGit struct {
	*git
	work    string // the workspace work tree (/workspace)
	gitDir  string // work/.git, read as data only
	priv    string // the private repository root; its .git is the GIT_DIR
	trusted func(uid uint32) bool
}

// trustedOwner is the default owner check: files the entrypoint's own identity
// owns. The step role runs every workload of a run as the installation's workload
// UID, so a repository owned by anyone else was not written by a step of this run.
func trustedOwner(uid uint32) bool { return uid == uint32(os.Geteuid()) }

var (
	hexCommit   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	refNamePath = regexp.MustCompile(`^refs/[A-Za-z0-9_-][A-Za-z0-9._-]*(?:/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)
)

// errUnsafeWorkspace is a workspace repository the entrypoint will not read.
var errUnsafeWorkspace = errors.New("the workspace repository is not trustworthy")

func unsafef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUnsafeWorkspace}, args...)...)
}

// newSealedGit prepares a private repository under scratch. base supplies the
// executable, the CA bundle and the sanitized environment. trusted may be nil
// (the entrypoint's own UID). The caller closes it.
func newSealedGit(ctx context.Context, base *git, work, scratch string, trusted func(uint32) bool) (*sealedGit, error) {
	if trusted == nil {
		trusted = trustedOwner
	}
	abs, err := filepath.Abs(work)
	if err != nil {
		return nil, err
	}
	s := &sealedGit{work: abs, gitDir: filepath.Join(abs, ".git"), trusted: trusted}
	if err := s.checkLayout(); err != nil {
		return nil, err
	}
	head, err := s.workspaceHead()
	if err != nil {
		return nil, err
	}
	s.priv, err = os.MkdirTemp(scratch, "kodama-seal-")
	if err != nil {
		return nil, err
	}
	init := []string{"init", "--quiet", "--template="}
	if len(head) == 64 {
		init = append(init, "--object-format=sha256")
	}
	if _, err := base.run(ctx, s.priv, nil, append(init, s.priv)...); err != nil {
		_ = os.RemoveAll(s.priv)
		return nil, err
	}
	pg := *base
	pg.group = true
	pg.seal = []string{
		"GIT_DIR=" + filepath.Join(s.priv, ".git"),
		"GIT_WORK_TREE=" + s.work,
		"GIT_OBJECT_DIRECTORY=" + filepath.Join(s.gitDir, "objects"),
		"GIT_INDEX_FILE=" + filepath.Join(s.priv, ".git", "index"),
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
	}
	// The private repository decides how files are read, not the volume: fixed,
	// not probed from whatever filesystem the scratch happens to be on.
	pg.cfg = []string{"-c", "core.filemode=true", "-c", "core.symlinks=true", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false",
		"-c", "core.quotepath=off", "-c", "core.untrackedCache=false", "-c", "status.showUntrackedFiles=all", "-c", "advice.addEmbeddedRepo=false"}
	s.git = &pg
	return s, nil
}

// Close removes the private repository.
func (s *sealedGit) Close() {
	if s.priv != "" {
		_ = os.RemoveAll(s.priv)
	}
}

func (s *sealedGit) owned(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && s.trusted(st.Uid)
}

// checkDir requires path to be a real directory (not a symlink) of a trusted owner.
func (s *sealedGit) checkDir(path, what string) error {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return unsafef("%s: %v", what, scrub(err))
	case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
		return unsafef("%s is not a plain directory", what)
	case !s.owned(fi):
		return unsafef("%s is owned by another identity", what)
	}
	return nil
}

// checkLayout verifies the two directories Git data is read from and the whole
// object store beneath the second one.
func (s *sealedGit) checkLayout() error {
	if err := s.checkDir(s.gitDir, ".git"); err != nil {
		return err
	}
	objects := filepath.Join(s.gitDir, "objects")
	if err := s.checkDir(objects, ".git/objects"); err != nil {
		return err
	}
	return s.checkObjectStore(objects)
}

// maxObjectEntries bounds the walk of one object store.
const maxObjectEntries = 4 << 20

// checkObjectStore requires the workspace's object store to be nothing but plain
// directories and regular files of a trusted owner, with no alternates. Git
// follows objects/info/alternates (and http-alternates) to other object
// directories, follows a symlinked fan-out directory or pack file wherever it
// points, and writes new objects through whatever a child names; any of those
// would read or write outside the volume's own store, with the attempt's
// authority. Nothing is followed: the first symlink, special file, foreign owner
// or alternates file refuses the whole store.
func (s *sealedGit) checkObjectStore(objects string) error {
	for _, alt := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(objects, "info", alt)); err == nil {
			return unsafef(".git/objects/info/%s redirects the object store", alt)
		} else if !errors.Is(err, os.ErrNotExist) {
			return unsafef(".git/objects/info/%s: %v", alt, scrub(err))
		}
	}
	entries := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		list, err := os.ReadDir(dir)
		if err != nil {
			return unsafef("the object store cannot be read: %v", scrub(err))
		}
		for _, e := range list {
			if entries++; entries > maxObjectEntries {
				return unsafef("the object store has too many entries")
			}
			fi, err := os.Lstat(filepath.Join(dir, e.Name()))
			if err != nil {
				return unsafef("the object store changed while it was read")
			}
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				return unsafef("%s in the object store is a symlink", e.Name())
			case !s.owned(fi):
				return unsafef("%s in the object store is owned by another identity", e.Name())
			case fi.IsDir():
				if depth >= 2 {
					return unsafef("the object store is nested deeper than Git writes it")
				}
				if err := walk(filepath.Join(dir, e.Name()), depth+1); err != nil {
					return err
				}
			case !fi.Mode().IsRegular():
				return unsafef("%s in the object store is not a regular file", e.Name())
			}
		}
		return nil
	}
	return walk(objects, 0)
}

// verifyClosure recomputes the object hash of everything status trusts: the HEAD
// commit, every tree reachable from it and every blob those trees name. Loose and
// packed objects are addressed by content, but nothing makes the bytes stored
// under a name match it: an earlier step can replace a tree under its own file
// name and keep HEAD. Git itself does not check on read, so the hashes are
// recomputed here from the bytes Git returns (through the private repository,
// which reads only this object store). A missing, unreadable or mismatching
// object refuses the workspace.
func (s *sealedGit) verifyClosure(ctx context.Context, head string) error {
	out, err := s.run(ctx, s.work, nil, "ls-tree", "-r", "-t", "-z", "--full-tree", head)
	if err != nil {
		return unsafef("the tree of HEAD cannot be listed: %v", err)
	}
	tree, err := s.run(ctx, s.work, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return unsafef("HEAD names no tree")
	}
	want := []string{head, strings.TrimSpace(string(tree))}
	seen := map[string]bool{head: true, want[1]: true}
	for _, rec := range bytes.Split(out, []byte{0}) {
		meta, _, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta))
		if len(f) != 3 || f[1] == "commit" { // a gitlink names a submodule commit that is not here
			continue
		}
		if !hexCommit.MatchString(f[2]) {
			return unsafef("the tree of HEAD lists a malformed object name")
		}
		if !seen[f[2]] {
			seen[f[2]] = true
			want = append(want, f[2])
		}
	}
	cmd := exec.CommandContext(ctx, s.bin, append(s.base(), "cat-file", "--batch")...)
	cmd.Dir = s.work
	cmd.Env = s.env()
	cmd.Stdin = strings.NewReader(strings.Join(want, "\n") + "\n")
	s.confine(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	finish := func() { _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait() }
	rd := bufio.NewReaderSize(stdout, 64<<10)
	for _, oid := range want {
		header, err := rd.ReadString('\n')
		if err != nil {
			finish()
			return unsafef("the object store ended before %s", oid)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != oid {
			finish()
			return unsafef("object %s is missing or unreadable", oid)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			finish()
			return unsafef("object %s has no size", oid)
		}
		var h hash.Hash = sha1.New()
		if len(oid) == 64 {
			h = sha256.New()
		}
		fmt.Fprintf(h, "%s %d\x00", f[1], size)
		if _, err := io.CopyN(h, rd, size); err != nil {
			finish()
			return unsafef("object %s is truncated", oid)
		}
		if _, err := rd.Discard(1); err != nil {
			finish()
			return unsafef("object %s is truncated", oid)
		}
		if hex.EncodeToString(h.Sum(nil)) != oid {
			finish()
			return unsafef("object %s does not hash to its name", oid)
		}
	}
	finish()
	return nil
}

func scrub(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// readData reads one small file of the workspace repository without following a
// symlink, requiring a regular file of a trusted owner and bounding its size.
func (s *sealedGit) readData(path, what string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, unsafef("%s: %v", what, scrub(err))
	}
	if !fi.Mode().IsRegular() {
		return nil, unsafef("%s is not a regular file", what)
	}
	if !s.owned(fi) {
		return nil, unsafef("%s is owned by another identity", what)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, unsafef("%s: %v", what, scrub(err))
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil || !os.SameFile(fi, fi2) {
		return nil, unsafef("%s changed while it was opened", what)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, unsafef("%s: %v", what, scrub(err))
	}
	if int64(len(data)) > max {
		return nil, unsafef("%s is larger than %d bytes", what, max)
	}
	return data, nil
}

// refPath resolves a ref name to its loose file, requiring every directory on the
// way to be a plain directory of a trusted owner.
func (s *sealedGit) refPath(ref string) (string, error) {
	if !refNamePath.MatchString(ref) || strings.Contains(ref, "..") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, ".") {
		return "", unsafef("HEAD names a ref that is not a plain ref name")
	}
	dir := s.gitDir
	parts := strings.Split(ref, "/")
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
			break // no loose ref below a directory that does not exist
		}
		if err := s.checkDir(dir, "a ref directory"); err != nil {
			return "", err
		}
	}
	return filepath.Join(s.gitDir, filepath.FromSlash(ref)), nil
}

// workspaceHead returns the commit the workspace's HEAD designates, read as data:
// a detached commit, or the loose or packed value of the one ref HEAD names.
func (s *sealedGit) workspaceHead() (string, error) {
	raw, err := s.readData(filepath.Join(s.gitDir, "HEAD"), ".git/HEAD", 4<<10)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(raw))
	if hexCommit.MatchString(line) {
		return line, nil
	}
	ref, ok := strings.CutPrefix(line, "ref: ")
	if !ok {
		return "", unsafef(".git/HEAD is neither a commit nor a ref")
	}
	path, err := s.refPath(ref)
	if err != nil {
		return "", err
	}
	if data, err := s.readData(path, "the ref HEAD names", 1<<10); err == nil {
		v := strings.TrimSpace(string(data))
		if !hexCommit.MatchString(v) {
			return "", unsafef("the ref HEAD names does not hold a commit")
		}
		return v, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	packed, err := s.readData(filepath.Join(s.gitDir, "packed-refs"), ".git/packed-refs", 16<<20)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", unsafef("HEAD names a ref that does not exist")
		}
		return "", err
	}
	for _, l := range bytes.Split(packed, []byte{'\n'}) {
		oid, name, ok := strings.Cut(string(l), " ")
		if ok && name == ref && hexCommit.MatchString(oid) {
			return oid, nil
		}
	}
	return "", unsafef("HEAD names a ref that does not exist")
}

// sync points the private repository at the workspace's current HEAD and loads
// that commit's tree into the private index, so every later query compares the
// work tree with exactly what HEAD says. It returns the head.
func (s *sealedGit) sync(ctx context.Context) (string, error) {
	if err := s.checkLayout(); err != nil {
		return "", err
	}
	head, err := s.workspaceHead()
	if err != nil {
		return "", err
	}
	if _, err := s.run(ctx, s.work, nil, "cat-file", "-e", head+"^{commit}"); err != nil {
		return "", unsafef("HEAD %s is not a commit of the workspace's object store", head)
	}
	if err := s.verifyClosure(ctx, head); err != nil {
		return "", err
	}
	if _, err := s.run(ctx, s.work, nil, "update-ref", "HEAD", head); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Join(s.priv, ".git", "index"))
	if _, err := s.run(ctx, s.work, nil, "read-tree", head); err != nil {
		return "", err
	}
	return head, nil
}

// hasCommit reports whether the commit is in the workspace's object store.
func (s *sealedGit) hasCommit(ctx context.Context, commit string) bool {
	_, err := s.run(ctx, s.work, nil, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// workspaceState is what the begin fence reports about /workspace.
type workspaceState struct {
	Head             string
	TrackedClean     bool
	UntrackedPresent bool
	IgnoredPresent   bool
}

// state reports the head and whether tracked files are unchanged, untracked files
// exist and ignored files exist. It changes nothing in the workspace.
func (s *sealedGit) state(ctx context.Context) (workspaceState, error) {
	head, err := s.sync(ctx)
	if err != nil {
		return workspaceState{}, err
	}
	out, err := s.run(ctx, s.work, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=traditional", "--ignore-submodules=all")
	if err != nil {
		return workspaceState{}, err
	}
	st := workspaceState{Head: head, TrackedClean: true}
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) < 3 {
			continue
		}
		switch string(entry[:2]) {
		case "??":
			st.UntrackedPresent = true
		case "!!":
			st.IgnoredPresent = true
		default:
			st.TrackedClean = false
		}
	}
	return st, nil
}

// advance moves the workspace's own HEAD (the ref it names, or a detached HEAD) to
// a commit the entrypoint just created, and its index with it, so the next step
// finds a repository whose HEAD is the published head and a clean tree. It writes
// plain files with checked, atomic renames; no workspace configuration is read.
func (s *sealedGit) advance(ctx context.Context, commit string) error {
	if !hexCommit.MatchString(commit) {
		return unsafef("not a commit")
	}
	if err := s.checkLayout(); err != nil {
		return err
	}
	headFile := filepath.Join(s.gitDir, "HEAD")
	raw, err := s.readData(headFile, ".git/HEAD", 4<<10)
	if err != nil {
		return err
	}
	target := headFile
	content := commit + "\n"
	if ref, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "ref: "); ok {
		if target, err = s.refPath(ref); err != nil {
			return err
		}
	}
	if err := atomicWrite(target, []byte(content)); err != nil {
		return err
	}
	// The workspace's own index follows HEAD (written by our Git from the commit's
	// tree; the old one is never read).
	_, err = s.run(ctx, s.work, []string{"GIT_INDEX_FILE=" + filepath.Join(s.gitDir, "index")}, "read-tree", commit)
	return err
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kodama-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
