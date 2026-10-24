package git

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is one bare repository served by a Server.
type Repo struct {
	Name string
	// Path is the bare repository directory.
	Path string
	// URL is the HTTPS clone URL (no credential).
	URL string
	// BaseRef is the default branch the fixture created.
	BaseRef string
	// Initial is the initial commit on BaseRef.
	Initial string
	srv     *Server
}

// run executes Git for fixtures with a clean, deterministic environment.
func run(t testing.TB, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Human", "GIT_AUTHOR_EMAIL=human@example.invalid",
		"GIT_COMMITTER_NAME=Human", "GIT_COMMITTER_EMAIL=human@example.invalid")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// NewRepo creates a bare repository named name with an initial commit on
// refs/heads/main and the server settings the workspace tests need: smart HTTP
// fetch and push, and fetching any reachable commit by name.
func (s *Server) NewRepo(t testing.TB, name string) *Repo {
	t.Helper()
	path := filepath.Join(s.Root, name+".git")
	work := t.TempDir()
	run(t, work, nil, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, nil, "add", ".")
	run(t, work, nil, "commit", "-q", "-m", "initial")
	initial := run(t, work, nil, "rev-parse", "HEAD")
	run(t, ".", nil, "clone", "-q", "--bare", work, path)
	for k, v := range map[string]string{
		"http.receivepack":              "true",
		"http.uploadpack":               "true",
		"uploadpack.allowAnySHA1InWant": "true",
		"receive.denyDeleteCurrent":     "false",
		"gc.auto":                       "0",
	} {
		run(t, path, nil, "config", k, v)
	}
	return &Repo{Name: name, Path: path, URL: s.URL + "/" + name + ".git", BaseRef: "refs/heads/main", Initial: initial, srv: s}
}

// Git runs a Git command inside the bare repository.
func (r *Repo) Git(t testing.TB, args ...string) string {
	t.Helper()
	return run(t, r.Path, nil, args...)
}

// Tip returns the commit of ref, or "" when it does not exist.
func (r *Repo) Tip(t testing.TB, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = r.Path
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// HasCommit reports whether the object exists in the repository.
func (r *Repo) HasCommit(t testing.TB, commit string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", commit+"^{commit}")
	cmd.Dir = r.Path
	return cmd.Run() == nil
}

// SetRef points ref at commit directly (fixture setup, not product behavior).
func (r *Repo) SetRef(t testing.TB, ref, commit string) { r.Git(t, "update-ref", ref, commit) }

// DeleteRef removes ref directly.
func (r *Repo) DeleteRef(t testing.TB, ref string) { r.Git(t, "update-ref", "-d", ref) }

// GC runs an aggressive prune, so unreachable objects really disappear.
func (r *Repo) GC(t testing.TB) {
	t.Helper()
	r.Git(t, "reflog", "expire", "--expire=now", "--all")
	r.Git(t, "gc", "--quiet", "--prune=now")
}

// File is a path and its content for a commit.
type File struct{ Path, Content string }

// Commit clones the repository at base, adds files on top and returns the new
// commit together with the working directory. The commit is not pushed.
func (r *Repo) Commit(t testing.TB, base string, msg string, files ...File) (commit, work string) {
	t.Helper()
	work = t.TempDir()
	run(t, work, nil, "init", "-q", "-b", "scratch")
	run(t, work, nil, "fetch", "-q", r.Path, base)
	run(t, work, nil, "checkout", "-q", "--detach", base)
	for _, f := range files {
		p := filepath.Join(work, f.Path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(t, work, nil, "add", "-A")
	run(t, work, nil, "commit", "-q", "-m", msg)
	return run(t, work, nil, "rev-parse", "HEAD"), work
}

// HumanPush plays an engineer: commit on top of the branch tip and push a
// normal fast-forward update straight to the repository (not through the
// server, so server fault injection does not affect it). It returns the commit.
func (r *Repo) HumanPush(t testing.TB, branch, msg string, files ...File) string {
	t.Helper()
	tip := r.Tip(t, branch)
	if tip == "" {
		t.Fatalf("branch %s does not exist", branch)
	}
	commit, work := r.Commit(t, tip, msg, files...)
	run(t, work, nil, "push", "-q", r.Path, commit+":"+branch)
	return commit
}

// Bundle creates what the step entrypoint uploads: an incremental bundle of
// base..commit holding one ref, created from a working clone. It returns the
// commit, the bundle bytes and the clone directory.
func (r *Repo) Bundle(t testing.TB, base, msg string, files ...File) (commit string, bundle []byte, work string) {
	t.Helper()
	commit, work = r.Commit(t, base, msg, files...)
	run(t, work, nil, "update-ref", "refs/kodama/candidate", commit)
	out := filepath.Join(t.TempDir(), "source.bundle")
	run(t, work, nil, "bundle", "create", "--quiet", out, base+"..refs/kodama/candidate")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return commit, data, work
}

// OrphanCommit creates a root commit unrelated to the repository's history
// (an independently created branch tip) and returns it. The commit object is
// stored in the bare repository but no ref points at it.
func (r *Repo) OrphanCommit(t testing.TB, msg string) string {
	t.Helper()
	work := t.TempDir()
	run(t, work, nil, "init", "-q", "-b", "orphan")
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, nil, "add", ".")
	run(t, work, nil, "commit", "-q", "-m", msg)
	commit := run(t, work, nil, "rev-parse", "HEAD")
	run(t, work, nil, "push", "-q", r.Path, commit+":refs/heads/orphan-"+commit[:8])
	r.DeleteRef(t, "refs/heads/orphan-"+commit[:8])
	return commit
}
