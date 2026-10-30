package entrypoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
)

// checkoutCredential is the read-only HTTPS credential for the clone. It is
// handed to Git through the environment of one invocation only.
type checkoutCredential struct {
	username string
	password string
}

// parseCheckoutCredential reads "username:password" (split at the first colon).
func parseCheckoutCredential(data []byte) (*checkoutCredential, error) {
	line := strings.TrimSpace(string(data))
	user, pass, ok := strings.Cut(line, ":")
	if !ok || user == "" || pass == "" || strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("checkout credential must be a single line username:password")
	}
	return &checkoutCredential{username: user, password: pass}, nil
}

func (c *checkoutCredential) header() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
}

// git runs the Git executable with a hardened, explicit configuration: no
// hooks, no system/global config, no credential helpers or prompts, HTTPS
// only, no submodule recursion, no redirects to other hosts.
type git struct {
	bin       string
	caFile    string
	home      string
	pathEnv   string
	redactors []string // exact strings removed from error output

	// seal and cfg are set only by sealedGit (the run_shared workspace): extra
	// environment that pins GIT_DIR/GIT_WORK_TREE/GIT_OBJECT_DIRECTORY/GIT_INDEX_FILE
	// to a private repository, and extra -c settings. The v1 checkout never sets
	// them and its invocations are byte-for-byte what they were.
	seal []string
	cfg  []string
	// group runs every invocation in its own process group and, when its context
	// ends, kills the whole group and stops waiting for output after a short
	// delay. Git delegates to helpers (git-remote-https, filters) that inherit its
	// pipes: killing Git alone leaves a stalled helper holding stderr open and the
	// call waits for it past every deadline. The preparation and the sealed Git set
	// it; the v1 checkout is unchanged.
	group bool
}

// groupWaitDelay bounds how long a killed invocation's output pipes are awaited.
const groupWaitDelay = 2 * time.Second

func (g *git) base() []string {
	return append(g.baseConfig(), g.cfg...)
}

func (g *git) baseConfig() []string {
	return []string{
		// The workspace volume is owned by the kubelet, not the non-root step, and
		// restricted Pod Security forbids a chown. Hooks and global config are
		// already disabled, so the ownership check would only refuse the step's
		// own checkout.
		"-c", "safe.directory=*",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.version=2",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "commit.gpgsign=false",
		"-c", "gc.auto=0",
	}
}

func (g *git) env(extra ...string) []string {
	env := []string{
		"HOME=" + g.home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_AUTHOR_NAME=Kodama",
		"GIT_AUTHOR_EMAIL=step@kodama.invalid",
		"GIT_COMMITTER_NAME=Kodama",
		"GIT_COMMITTER_EMAIL=step@kodama.invalid",
	}
	if g.pathEnv != "" {
		env = append(env, "PATH="+g.pathEnv)
	}
	if g.caFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+g.caFile)
	}
	env = append(env, g.seal...)
	return append(env, extra...)
}

func (g *git) redact(s string) string {
	for _, r := range g.redactors {
		if r != "" {
			s = strings.ReplaceAll(s, r, "***")
		}
	}
	return s
}

// run executes git in dir and returns stdout. Errors carry redacted stderr.
func (g *git) run(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.bin, append(g.base(), args...)...)
	cmd.Dir = dir
	cmd.Env = g.env(extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &limitedBuffer{buf: &stderr, max: 8 << 10}
	g.confine(cmd)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(g.redact(stderr.String()))
		return nil, fmt.Errorf("git %s: %v: %s", firstArg(args), err, msg)
	}
	return stdout.Bytes(), nil
}

// confine puts the command in its own process group when g.group is set, so that
// cancellation reaches its helpers and the wait for its pipes is bounded.
func (g *git) confine(cmd *exec.Cmd) {
	if !g.group {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = groupWaitDelay
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// limitedBuffer keeps at most max bytes and discards the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// checkout fetches exactly commit from repoURL into dir and detaches HEAD on
// it. The remote URL stored in .git/config never contains the credential; the
// credential travels as an http.<origin>.extraHeader in the environment of the
// fetch only.
func (g *git) checkout(ctx context.Context, dir, repoURL string, commit runtimev1.GitCommit, cred *checkoutCredential) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("workspace directory is not empty")
	}
	initArgs := []string{"init", "--quiet", "--template="}
	if len(commit) == 64 {
		initArgs = append(initArgs, "--object-format=sha256")
	}
	if _, err := g.run(ctx, dir, nil, initArgs...); err != nil {
		return err
	}
	if _, err := g.run(ctx, dir, nil, "remote", "add", "origin", repoURL); err != nil {
		return err
	}
	var extra []string
	if cred != nil {
		u, err := url.Parse(repoURL)
		if err != nil {
			return err
		}
		origin := u.Scheme + "://" + u.Host
		extra = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + origin + ".extraHeader", "GIT_CONFIG_VALUE_0=" + cred.header()}
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
				return err
			}
		}
		if _, last = g.run(ctx, dir, extra, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "origin", string(commit)); last == nil {
			break
		}
	}
	if last != nil {
		return last
	}
	if _, err := g.run(ctx, dir, nil, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", string(commit)); err != nil {
		return err
	}
	head, err := g.head(ctx, dir)
	if err != nil {
		return err
	}
	if head != string(commit) {
		return fmt.Errorf("checked out %s, expected %s", head, commit)
	}
	return nil
}

func (g *git) head(ctx context.Context, dir string) (string, error) {
	out, err := g.run(ctx, dir, nil, "rev-parse", "--verify", "HEAD^{commit}")
	return strings.TrimSpace(string(out)), err
}

// dirty reports uncommitted, non-ignored changes including untracked files.
func (g *git) dirty(ctx context.Context, dir string) (bool, error) {
	out, err := g.run(ctx, dir, nil, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=all")
	return len(out) > 0, err
}

// commitAll stages every non-ignored change and commits it on top of HEAD
// with the fixed identity, bypassing hooks.
func (g *git) commitAll(ctx context.Context, dir, message string) (string, error) {
	if _, err := g.run(ctx, dir, nil, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, dir, nil, "commit", "--quiet", "--no-verify", "--no-gpg-sign", "--allow-empty-message", "-m", message); err != nil {
		return "", err
	}
	return g.head(ctx, dir)
}

func (g *git) isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	_, err := g.run(ctx, dir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// bundle writes a bundle holding base..commit under a temporary ref. The
// bundle carries objects only; it never includes repository configuration.
func (g *git) bundle(ctx context.Context, dir, base, commit, out string) error {
	const ref = "refs/kodama/candidate"
	if _, err := g.run(ctx, dir, nil, "update-ref", ref, commit); err != nil {
		return err
	}
	_, err := g.run(ctx, dir, nil, "bundle", "create", "--quiet", out, base+".."+ref)
	return err
}

// minSecretScanLen is the shortest secret value scanned for in source; shorter
// values would match innocent content.
const minSecretScanLen = 8

// scanForSecrets reports the first object that would enter the bundle (every
// commit, tree and blob reachable from commit but not from base, so
// intermediate commits and commit messages are covered, not only the final
// tree) whose raw content contains any needle. It returns a description of
// the object (path when known). Injected credentials must never enter a bundle.
func (g *git) scanForSecrets(ctx context.Context, dir, base, commit string, needles [][]byte) (string, error) {
	var usable [][]byte
	for _, n := range needles {
		if len(n) >= minSecretScanLen {
			usable = append(usable, n)
		}
	}
	if len(usable) == 0 {
		return "", nil
	}
	listing, err := g.run(ctx, dir, nil, "rev-list", "--objects", commit, "^"+base)
	if err != nil {
		return "", err
	}
	var oids []string
	names := map[string]string{}
	for _, line := range strings.Split(string(listing), "\n") {
		oid, name, _ := strings.Cut(line, " ")
		if oid == "" {
			continue
		}
		oids = append(oids, oid)
		if name != "" {
			names[oid] = name
		}
	}
	if len(oids) == 0 {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, g.bin, append(g.base(), "cat-file", "--batch")...)
	cmd.Dir = dir
	cmd.Env = g.env()
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	g.confine(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	finish := func() { _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait() }
	rd := bufio.NewReaderSize(stdout, 64<<10)
	for range oids {
		header, err := rd.ReadString('\n')
		if err != nil {
			finish()
			return "", fmt.Errorf("git cat-file: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			finish()
			return "", fmt.Errorf("git cat-file: unexpected header %q", strings.TrimSpace(header))
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			finish()
			return "", fmt.Errorf("git cat-file: bad size in %q", strings.TrimSpace(header))
		}
		body := io.LimitReader(rd, size)
		found := containsAny(body, usable)
		if _, err := io.Copy(io.Discard, body); err != nil {
			finish()
			return "", err
		}
		if _, err := rd.Discard(1); err != nil { // trailing newline
			finish()
			return "", err
		}
		if found {
			finish()
			where := names[fields[0]]
			if where == "" {
				where = fields[1] + " " + fields[0]
			}
			return where, nil
		}
	}
	finish()
	return "", nil
}

// containsAny streams r looking for any needle, keeping an overlap window.
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
