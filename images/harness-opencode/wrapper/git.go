package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
)

// The wrapper's Git is local and credential-free: it commits what the agent
// changed and leaves the commits for the step entrypoint to publish. It never
// pushes, never fetches, never configures a remote or a credential helper, and
// the environment it runs Git in holds no Git credential (a push is made by the
// entrypoint with the attempt's own grant, which the wrapper never reads).

// gitFunc runs one Git command in a checkout and returns its trimmed standard
// output. The error names the subcommand only: Git's own text could carry paths
// or, for a hostile repository configuration, anything.
type gitFunc func(args ...string) (string, error)

// gitIn returns a runner for dir. Hooks, signing and the file-system monitor are
// disabled, global and system configuration are ignored and the environment is
// explicit, so nothing the checkout's own configuration names is run for a
// hook. safe lists the directories Git must trust (the image's `git` shim trusts
// only /workspace itself).
func gitIn(ctx context.Context, dir string, safe ...string) gitFunc {
	return func(args ...string) (string, error) {
		full := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.fsmonitor=false"}
		for _, s := range safe {
			full = append(full, "-c", "safe.directory="+s)
		}
		full = append(full, args...)
		cmd := exec.CommandContext(ctx, "git", full...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=Kodama", "GIT_AUTHOR_EMAIL=step@kodama.invalid", "GIT_COMMITTER_NAME=Kodama", "GIT_COMMITTER_EMAIL=step@kodama.invalid"}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", errors.New("git " + args[0] + " failed")
		}
		return strings.TrimSpace(out.String()), nil
	}
}

// sourceError is a checkout the wrapper refuses to run Git in: the verdict is
// source_invalid, whether it is found before the harness runs or after.
type sourceError struct{ msg string }

func (e *sourceError) Error() string { return e.msg }

// gitCallTimeout bounds one Git invocation in a source checkout, so a command that
// would not end cannot outlive the whole commit step's context.
const gitCallTimeout = 30 * time.Second

// hardenedGit is the runner for a source checkout. Checkouts are directories the
// agent controlled until a moment ago, so before every invocation the wrapper
// re-validates what Git is about to read (validateCheckout), points Git at the
// repository explicitly (GIT_DIR and GIT_WORK_TREE, so no discovery or
// core.worktree can send it elsewhere, and GIT_CEILING_DIRECTORIES stops any
// search above it), disables everything the command line can disable, and kills
// the invocation's process group when its own timeout ends. Repository
// configuration that names a program (a filter, a hook path, a work tree, an
// include, a helper) is not neutralized, it is refused: see validateCheckout.
func hardenedGit(ctx context.Context, workspace string, co agents.Checkout) gitFunc {
	dir := checkoutDir(workspace, co)
	return func(args ...string) (string, error) {
		if err := validateCheckout(workspace, co); err != nil {
			return "", err
		}
		full := []string{"-c", "safe.directory=" + dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null",
			"-c", "core.excludesFile=/dev/null", "-c", "commit.gpgsign=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.allow=never",
			"-c", "credential.helper=", "-c", "core.pager=cat", "-c", "core.editor=true", "-c", "core.autocrlf=false",
			"-c", "diff.ignoreSubmodules=all", "-c", "submodule.recurse=false", "-c", "status.submoduleSummary=false"}
		full = append(full, args...)
		gitDir := filepath.Join(dir, ".git")
		// Every location Git reads from is named: GIT_COMMON_DIR and
		// GIT_OBJECT_DIRECTORY take precedence over a `commondir` file or an
		// `objects/info/alternates` the checkout might carry (which validateCheckout
		// refuses anyway), and GIT_ALTERNATE_OBJECT_DIRECTORIES is never set (the
		// environment is built here, not inherited).
		return runGit(ctx, dir, full, []string{"GIT_DIR=" + gitDir, "GIT_COMMON_DIR=" + gitDir, "GIT_OBJECT_DIRECTORY=" + filepath.Join(gitDir, "objects"),
			"GIT_WORK_TREE=" + dir, "GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir), "GIT_OPTIONAL_LOCKS=0"})
	}
}

// openCheckout validates a source checkout and returns its hardened runner. It
// additionally requires Git itself, discovering from the directory with a ceiling
// at its parent, to report the directory as the top level of its work tree.
func openCheckout(ctx context.Context, workspace string, co agents.Checkout) (gitFunc, error) {
	if err := validateCheckout(workspace, co); err != nil {
		return nil, err
	}
	dir := checkoutDir(workspace, co)
	if err := noNestedRepository(ctx, dir, co); err != nil {
		return nil, err
	}
	top, err := runGit(ctx, dir, []string{"-c", "safe.directory=" + dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel"},
		[]string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)})
	if err != nil {
		return nil, &sourceError{"the checkout of source " + co.Path + " is not a repository"}
	}
	want, werr := filepath.EvalSymlinks(dir)
	got, gerr := filepath.EvalSymlinks(top)
	if werr != nil || gerr != nil || want != got {
		return nil, &sourceError{"the repository of source " + co.Path + " is not rooted at its checkout"}
	}
	return hardenedGit(ctx, workspace, co), nil
}

// asSource keeps a classified source error and replaces anything else by msg.
func asSource(err error, msg string) error {
	var se *sourceError
	if errors.As(err, &se) {
		return err
	}
	return errors.New(msg)
}

// runGit runs one Git command with the wrapper's explicit environment, its own
// timeout and, on timeout, a kill of its whole process group.
func runGit(ctx context.Context, dir string, args, extraEnv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Kodama", "GIT_AUTHOR_EMAIL=step@kodama.invalid", "GIT_COMMITTER_NAME=Kodama", "GIT_COMMITTER_EMAIL=step@kodama.invalid"}, extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		// Git's own text is never returned: it could carry paths or hostile configuration.
		return "", errors.New("git " + firstArg(args) + " failed")
	}
	return strings.TrimSpace(out.String()), nil
}

// firstArg is the subcommand of an argument list that starts with -c pairs.
func firstArg(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++
			continue
		}
		return args[i]
	}
	return "?"
}

// validateCheckout is the check run before every Git invocation in a source
// checkout and once before the harness starts. It requires:
//
//   - every directory from the workspace to the source to be a real directory (a
//     symlink the agent planted would send Git elsewhere; the same walk as the
//     entrypoint's checkSourceDir);
//   - `.git` to be a real directory (not a `gitdir:` file, which would name
//     another repository, and not a symlink), with a regular `config` and no
//     `config.worktree`;
//   - the repository configuration to name no program or redirection: no filter,
//     include, hook path, work tree, file-system monitor, helper, pager, editor,
//     ssh/proxy command, attributes file, alias, diff/merge driver or worktree
//     config extension. Git runs those from `git add`/`status`; the wrapper's
//     command-line overrides cover some, but `filter.*.clean` cannot be disabled
//     generically, so a checkout that carries any of them is refused.
//
// A preparation checkout carries none of these (its hardening is the command
// line's, and no configuration it writes names a program).
func validateCheckout(workspace string, co agents.Checkout) error {
	dir := workspace
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return &sourceError{"the workspace is not a directory"}
	}
	if co.Path != "." {
		for _, part := range strings.Split(co.Path, "/") {
			if part == "" {
				continue
			}
			dir = filepath.Join(dir, part)
			fi, err := os.Lstat(dir)
			switch {
			case err != nil:
				return &sourceError{"the checkout of source " + co.Path + " is not present"}
			case fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir():
				return &sourceError{"the checkout of source " + co.Path + " is not a plain directory"}
			}
		}
	}
	gitDir := filepath.Join(dir, ".git")
	if fi, err := os.Lstat(gitDir); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return &sourceError{"the repository of source " + co.Path + " is not a plain .git directory"}
	}
	// A preparation checkout is `git init` plus one fetch and a detached checkout:
	// it has none of these. Each one redirects what Git reads (configuration,
	// hooks and refs from another directory; objects from another store; history
	// grafts; a submodule's own repository), so its presence is a refusal.
	for _, rel := range []string{"commondir", "config.worktree", "modules", "info/grafts", "info/alternates", "objects/info/alternates", "objects/info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(gitDir, filepath.FromSlash(rel))); err == nil {
			return &sourceError{"the repository of source " + co.Path + " carries .git/" + rel + ", which the wrapper does not run Git under"}
		}
	}
	if fi, err := os.Lstat(filepath.Join(gitDir, "objects")); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return &sourceError{"the object store of source " + co.Path + " is not a plain directory"}
	}
	fi, err := os.Lstat(filepath.Join(gitDir, "config"))
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return &sourceError{"the repository configuration of source " + co.Path + " is not a regular file of a sane size"}
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return &sourceError{"the repository configuration of source " + co.Path + " is unreadable"}
	}
	if key := forbiddenConfigKey(string(raw)); key != "" {
		return &sourceError{"the repository configuration of source " + co.Path + " names " + key + ", which the wrapper does not run Git under"}
	}
	return nil
}

// maxScanEntries bounds the walk of a checkout for nested repositories.
const maxScanEntries = 2_000_000

// noNestedRepository refuses a checkout that contains another repository (any
// `.git` entry below its own). Measured with Git 2.47: `git status` and
// `git add -A` descend into a path the index records as a gitlink and run that
// nested repository's own configuration (a clean filter, a file-system monitor)
// with its own settings, with or without `.gitmodules`; only `status` is stopped
// by --ignore-submodules=all, `add` is not. The preparation skips submodules, so a
// legitimate checkout has no `.git` below its own (a gitlink is an empty
// directory) and every other nested repository is the agent's. The walk does not
// follow symlinks and does not honor ignore files.
func noNestedRepository(ctx context.Context, dir string, co agents.Checkout) error {
	top := filepath.Join(dir, ".git")
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > maxScanEntries || n%4096 == 0 && ctx.Err() != nil {
			return errors.New("too large")
		}
		if path == top {
			return filepath.SkipDir
		}
		if d.Name() == ".git" {
			return &sourceError{"source " + co.Path + " contains a nested repository"}
		}
		return nil
	})
	var se *sourceError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return se
	}
	return &sourceError{"source " + co.Path + " cannot be inspected for nested repositories"}
}

// forbiddenSections are configuration sections of which any key is refused; the
// pairs below are single refused keys of otherwise ordinary sections.
var forbiddenSections = map[string]bool{
	"filter": true, "include": true, "includeif": true, "credential": true, "url": true, "alias": true,
	"diff": true, "merge": true, "gpg": true, "pager": true, "remote": true, "submodule": true, "trace2": true,
	"interactive": true, "maintenance": true, "http": true, "protocol": true, "uploadpack": true, "receive": true, "transfer": true,
	"difftool": true, "mergetool": true, "web": true, "man": true, "help": true,
}

var forbiddenKeys = map[string]bool{
	"core.worktree": true, "core.hookspath": true, "core.fsmonitor": true, "core.sshcommand": true, "core.gitproxy": true,
	"core.pager": true, "core.editor": true, "core.askpass": true, "core.attributesfile": true, "extensions.worktreeconfig": true,
	"core.alternaterefscommand": true, "core.alternaterefsprefixes": true, "core.excludesfile": true, "core.fsmonitorhookversion": true,
	"sequence.editor": true, "commit.template": true, "commit.gpgsign": true, "gc.auto": true, "gc.autodetach": true,
	"gc.pruneexpire": true, "rebase.instructionformat": true,
}

// forbiddenConfigKey returns the first refused key of a Git configuration file, as
// "section.key", or "" when there is none. The parser is deliberately
// conservative: a section header is parsed as Git parses it (a quoted subsection
// may contain `]`, as in `[core "a]"] worktree = x`, which is the section `core`
// with the key `worktree`), anything it cannot parse is refused, and a line Git
// would take as the continuation of a value is still read as a header or key, so
// it may refuse a harmless file and never the reverse.
func forbiddenConfigKey(cfg string) string {
	cfg = strings.TrimPrefix(cfg, "\ufeff")
	section := ""
	for _, line := range strings.Split(cfg, "\n") {
		line = strings.TrimSpace(line)
		for line != "" && line[0] == '[' {
			name, rest, ok := parseSectionHeader(line)
			if !ok {
				return "an unparsable section header"
			}
			section = name
			if forbiddenSections[section] {
				return section + ".*"
			}
			line = strings.TrimSpace(rest)
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key := line
		if i := strings.IndexAny(key, "= \t"); i >= 0 {
			key = key[:i]
		}
		if k := section + "." + strings.ToLower(key); forbiddenKeys[k] {
			return k
		}
	}
	return ""
}

// parseSectionHeader reads `[name]`, `[name.sub]` or `[name "sub"]` (the quoted
// subsection ends at its first unescaped quote) from the start of line and
// returns the lower-case section name and what follows the closing bracket.
func parseSectionHeader(line string) (name, rest string, ok bool) {
	i := 1
	for i < len(line) && line[i] != ']' && line[i] != '"' && line[i] != '.' && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	name = strings.ToLower(strings.TrimSpace(line[1:i]))
	if name == "" {
		return "", "", false
	}
	for i < len(line) && line[i] != ']' && line[i] != '"' {
		i++ // a dotted subsection or the space before a quoted one: not part of the name
	}
	if i < len(line) && line[i] == '"' {
		for i++; i < len(line) && line[i] != '"'; i++ {
			if line[i] == '\\' {
				i++
			}
		}
		if i >= len(line) {
			return "", "", false
		}
		i++
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
	}
	if i >= len(line) || line[i] != ']' {
		return "", "", false
	}
	return name, line[i+1:], true
}

// commitDirty commits whatever is changed in the checkout on its current HEAD.
func commitDirty(git gitFunc, summary string, redact func(string) string) error {
	// Under the hardened runner diff.ignoreSubmodules=all is set, so status never
	// asks a nested repository; the v1/v2 runner is left exactly as it was.
	status, err := git("status", "--porcelain")
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	if _, err := git("add", "-A"); err != nil {
		return err
	}
	msg := firstLine(redact(summary))
	if msg == "" {
		msg = "agent change"
	}
	_, err = git("commit", "--no-verify", "--no-gpg-sign", "-m", msg)
	return err
}

func headOf(git gitFunc) (string, error) {
	head, err := git("rev-parse", "HEAD")
	if err != nil || len(head) != 40 && len(head) != 64 {
		return "", errors.New("no candidate commit")
	}
	return head, nil
}

// commitWorkspace commits whatever the agent changed on top of the pinned
// checkout and returns the candidate commit (HEAD). The conformance report
// must name the candidate, and the candidate exists only once it is committed;
// the entrypoint later proposes it through the workspace protocol and checks
// that it descends from the pinned commit. The wrapper never pushes and holds
// no Git credential. This is the single-checkout behavior of runtime contexts
// v1 and v2, unchanged.
func commitWorkspace(ctx context.Context, workspace, summary string, redact func(string) string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	git := gitIn(ctx, workspace)
	if err := commitDirty(git, summary, redact); err != nil {
		return "", err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil || len(head) != 40 {
		return "", errors.New("no candidate commit")
	}
	return head, nil
}

// checkoutDir is where a planned checkout lives under the workspace mount.
func checkoutDir(workspace string, co agents.Checkout) string {
	if co.Path == "." {
		return workspace
	}
	return filepath.Join(workspace, filepath.FromSlash(co.Path))
}

// startCommits reads the commit every planned checkout is on before the harness
// runs, from the checkouts themselves. "Changed" is later measured against it:
// the harness's account of what it did is never believed.
func startCommits(ctx context.Context, workspace string, plan []agents.Checkout) (map[string]runtimev1.GitCommit, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out := map[string]runtimev1.GitCommit{}
	for _, co := range plan {
		git, err := openCheckout(ctx, workspace, co)
		if err != nil {
			return nil, err
		}
		head, err := headOf(git)
		if err != nil {
			return nil, &sourceError{"the checkout of source " + co.Path + " is not a repository at a commit"}
		}
		out[co.Path] = runtimev1.GitCommit(head)
	}
	return out, nil
}

// commitSources commits every planned checkout of a source-set attempt and
// returns the candidate: the commit each source ends on and, for a changed
// source, the branch it is on.
//
//   - A source planned ModeHead (the writer's own write source) is committed on its
//     HEAD; the entrypoint publishes it to the source's task branch with a lease on
//     the expected tip.
//   - A source planned ModeAgentBranch is, when the tree is dirty or HEAD moved,
//     put on the run-scoped agent branch the plan names (`checkout -B`, carrying
//     any commit the agent made itself) and committed there, so the entrypoint's
//     "push the branch the command left checked out when the grant selects it"
//     publishes it. An unchanged source is left exactly as it was.
//
// The wrapper creates local branches and commits only. Nothing is pushed here and
// no credential is read: publication, and the authority to publish, are the
// entrypoint's.
func commitSources(ctx context.Context, workspace string, plan []agents.Checkout, starts map[string]runtimev1.GitCommit, summary string, redact func(string) string) (agents.CandidateSet, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ends := map[string]runtimev1.GitCommit{}
	for _, co := range plan {
		git, err := openCheckout(ctx, workspace, co)
		if err != nil {
			return agents.CandidateSet{}, err
		}
		switch co.Mode {
		case agents.ModeHead:
			if err := commitDirty(git, summary, redact); err != nil {
				return agents.CandidateSet{}, asSource(err, "cannot commit source "+co.Path)
			}
		case agents.ModeAgentBranch:
			status, err := git("status", "--porcelain", "--ignore-submodules=all")
			if err != nil {
				return agents.CandidateSet{}, asSource(err, "cannot inspect source "+co.Path)
			}
			head, err := headOf(git)
			if err != nil {
				return agents.CandidateSet{}, asSource(err, "cannot read source "+co.Path)
			}
			if status != "" || runtimev1.GitCommit(head) != starts[co.Path] {
				branch, ok := strings.CutPrefix(co.Ref, "refs/heads/")
				if !ok {
					return agents.CandidateSet{}, errors.New("source " + co.Path + " is planned onto a ref that is not a branch")
				}
				if _, err := git("checkout", "--quiet", "-B", branch); err != nil {
					return agents.CandidateSet{}, asSource(err, "cannot create the agent branch of source "+co.Path)
				}
				if err := commitDirty(git, summary, redact); err != nil {
					return agents.CandidateSet{}, asSource(err, "cannot commit source "+co.Path)
				}
			}
		default:
			return agents.CandidateSet{}, errors.New("unknown checkout mode for source " + co.Path)
		}
		head, err := headOf(git)
		if err != nil {
			return agents.CandidateSet{}, asSource(err, "no candidate commit for source "+co.Path)
		}
		ends[co.Path] = runtimev1.GitCommit(head)
	}
	set, err := agents.NewCandidateSet(plan, starts, ends)
	if err != nil {
		return agents.CandidateSet{}, errors.New("the candidate is not valid")
	}
	return set, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
