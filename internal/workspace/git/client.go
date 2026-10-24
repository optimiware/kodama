// Package git is the Git adapter of the workspace module. It runs the existing
// Git executable with an explicit, hardened configuration: HTTPS only with
// verified TLS, a sanitized environment, hooks, helpers, prompts, submodule
// recursion and redirects disabled, and no ambient Git configuration.
//
// Credentials are resolved per operation, travel only in the environment of the
// one invocation that needs them, and never enter repository directories,
// stored configuration, command arguments, bundles or diagnostics. The reader
// (checkout credential, read-only) and the publisher (publisher credential) are
// separate types so write authority is held only where it is exercised.
//
// Every operation is bounded: a per-command deadline, a bundle transfer limit,
// and a scratch limit enforced while Git expands objects, not afterwards.
package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// Credential is an HTTPS Basic credential for one repository.
type Credential struct {
	Username string
	Password string
}

func (c Credential) header() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password))
}

func (c Credential) valid() bool {
	return c.Username != "" && c.Password != "" && !strings.ContainsAny(c.Username+c.Password, "\r\n\x00")
}

// Credentials resolves a secret reference inside a scope. The reader is wired
// with one resolving checkout references, the publisher with another resolving
// publisher references; neither is given the other's.
type Credentials interface {
	Resolve(ctx context.Context, scope domain.Scope, ref string) (Credential, error)
}

// Options bound and configure the adapter. Every limit is required: an
// unbounded Git invocation is refused rather than silently allowed.
type Options struct {
	// Binary is the Git executable (default "git" resolved through PATH).
	Binary string
	// CAFile adds trusted roots for the server certificate (PEM).
	CAFile string
	// CommandTimeout bounds each Git invocation.
	CommandTimeout time.Duration
	// MaxBundleBytes bounds the bundle bytes accepted from the artifact store.
	MaxBundleBytes int64
	// MaxScratchBytes bounds the on-disk size of one operation's private
	// scratch, enforced while Git runs.
	MaxScratchBytes int64
	// ScratchRoot is the private directory under which scratch is created.
	ScratchRoot string
}

func (o Options) validate() (Options, error) {
	if o.Binary == "" {
		o.Binary = "git"
	}
	switch {
	case o.CommandTimeout <= 0, o.MaxBundleBytes <= 0, o.MaxScratchBytes <= 0:
		return o, fmt.Errorf("%w: command timeout and transfer/scratch limits must be positive", workspace.ErrInvalidConfiguration)
	case o.ScratchRoot == "":
		return o, fmt.Errorf("%w: scratch root", workspace.ErrInvalidConfiguration)
	case o.MaxScratchBytes < o.MaxBundleBytes:
		return o, fmt.Errorf("%w: scratch limit must cover the bundle limit", workspace.ErrInvalidConfiguration)
	}
	path, err := exec.LookPath(o.Binary)
	if err != nil {
		return o, fmt.Errorf("%w: git executable: %v", workspace.ErrInvalidConfiguration, err)
	}
	o.Binary = path
	if err := os.MkdirAll(o.ScratchRoot, 0o700); err != nil {
		return o, fmt.Errorf("%w: scratch root: %v", workspace.ErrInvalidConfiguration, err)
	}
	return o, nil
}

// engine runs Git commands.
type engine struct {
	o     Options
	home  string
	creds Credentials
}

func newEngine(o Options, creds Credentials) (*engine, error) {
	if creds == nil {
		return nil, fmt.Errorf("%w: credentials", workspace.ErrNotConfigured)
	}
	o, err := o.validate()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp(o.ScratchRoot, "home-")
	if err != nil {
		return nil, err
	}
	return &engine{o: o, home: home, creds: creds}, nil
}

// Close removes the private home directory.
func (e *engine) close() { _ = os.RemoveAll(e.home) }

var baseConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "protocol.version=2",
	"-c", "submodule.recurse=false",
	"-c", "fetch.recurseSubmodules=false",
	"-c", "credential.helper=",
	"-c", "http.followRedirects=false",
	"-c", "http.sslVerify=true",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "fetch.fsckObjects=true",
	"-c", "transfer.fsckObjects=true",
	"-c", "fetch.unpackLimit=1",
	"-c", "advice.detachedHead=false",
}

type cmdSpec struct {
	dir   string
	args  []string
	proto string // GIT_ALLOW_PROTOCOL: "https" or "file"
	cred  *Credential
	url   string
	// watch is a directory whose size is bounded while the command runs.
	watch string
	// maxOut bounds captured stdout (default 1 MiB).
	maxOut int64
	stdin  *bytes.Reader
}

func (e *engine) env(s cmdSpec) []string {
	env := []string{
		"HOME=" + e.home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=" + s.proto,
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_AUTHOR_NAME=Kodama", "GIT_AUTHOR_EMAIL=workspace@kodama.invalid",
		"GIT_COMMITTER_NAME=Kodama", "GIT_COMMITTER_EMAIL=workspace@kodama.invalid",
		"PATH=" + os.Getenv("PATH"),
	}
	if e.o.CAFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+e.o.CAFile)
	}
	if s.cred != nil {
		u, err := url.Parse(s.url)
		if err == nil {
			origin := u.Scheme + "://" + u.Host
			env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+origin+"/.extraHeader", "GIT_CONFIG_VALUE_0="+s.cred.header())
		}
	}
	return env
}

type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room >= len(p) {
		l.buf.Write(p)
	} else {
		if room > 0 {
			l.buf.Write(p[:room])
		}
		l.overflow = true
	}
	return len(p), nil
}

// errCommand reports a failed Git invocation without secrets.
type errCommand struct {
	op     string
	exit   int
	stderr string
}

func (e *errCommand) Error() string {
	return fmt.Sprintf("git %s failed (exit %d): %s", e.op, e.exit, e.stderr)
}

// run executes one Git command under the deadline and scratch bounds.
func (e *engine) run(ctx context.Context, s cmdSpec) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, e.o.CommandTimeout)
	defer cancel()
	args := append(append([]string{}, baseConfig...), s.args...)
	cmd := exec.CommandContext(ctx, e.o.Binary, args...)
	cmd.Dir = s.dir
	cmd.Env = e.env(s)
	cmd.WaitDelay = 2 * time.Second
	isolate(cmd)
	maxOut := s.maxOut
	if maxOut == 0 {
		maxOut = 1 << 20
	}
	stdout := &limitedBuffer{max: int(maxOut)}
	stderr := &limitedBuffer{max: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if s.stdin != nil {
		cmd.Stdin = s.stdin
	}

	var overflow atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	if s.watch != "" {
		go func() {
			defer close(done)
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					if dirSize(s.watch) > e.o.MaxScratchBytes {
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
	if s.watch != "" && dirSize(s.watch) > e.o.MaxScratchBytes {
		overflow.Store(true)
	}

	switch {
	case overflow.Load():
		return nil, fmt.Errorf("%w: scratch space exceeded %d bytes", workspace.ErrLimitExceeded, e.o.MaxScratchBytes)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("%w: git %s exceeded its %s deadline: %w", workspace.ErrUnavailable, opName(s.args), e.o.CommandTimeout, context.DeadlineExceeded)
	case ctx.Err() != nil:
		return nil, fmt.Errorf("git %s: %w", opName(s.args), ctx.Err())
	case stdout.overflow:
		return nil, fmt.Errorf("%w: git output exceeded %d bytes", workspace.ErrLimitExceeded, maxOut)
	case err != nil:
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return nil, &errCommand{op: opName(s.args), exit: code, stderr: redact(strings.TrimSpace(stderr.buf.String()), s.cred)}
	}
	return stdout.buf.Bytes(), nil
}

func opName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func redact(msg string, c *Credential) string {
	if c != nil {
		for _, secret := range []string{c.Password, c.header(), base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))} {
			if secret != "" {
				msg = strings.ReplaceAll(msg, secret, "***")
			}
		}
	}
	return msg
}

// exitCode returns the process exit code of a failed command, or -1.
func exitCode(err error) int {
	var ce *errCommand
	if errors.As(err, &ce) {
		return ce.exit
	}
	return -1
}

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

// scratch is one operation's private directory; cleanup removes it.
type scratch struct {
	dir     string
	cleanup func()
}

func (e *engine) newScratch() (*scratch, error) {
	dir, err := os.MkdirTemp(e.o.ScratchRoot, "op-")
	if err != nil {
		return nil, fmt.Errorf("%w: scratch: %v", workspace.ErrUnavailable, err)
	}
	return &scratch{dir: dir, cleanup: func() { _ = os.RemoveAll(dir) }}, nil
}

// initBare creates an empty bare repository under the scratch directory,
// without templates (so no hooks) and matching the commit's object format.
func (e *engine) initBare(ctx context.Context, sc *scratch, sample workspace.Commit) (string, error) {
	repo := filepath.Join(sc.dir, "repo.git")
	args := []string{"init", "--bare", "--quiet", "--template="}
	if len(sample) == 64 {
		args = append(args, "--object-format=sha256")
	}
	args = append(args, repo)
	if _, err := e.run(ctx, cmdSpec{dir: sc.dir, args: args, proto: "file"}); err != nil {
		return "", err
	}
	return repo, nil
}

// validateRepo checks the repository URL: HTTPS only, no embedded credential,
// no query or fragment.
func validateRepo(r workspace.RepoRef) error {
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(r.URL, " \t\r\n\x00") || strings.HasPrefix(u.Path, "-") {
		return fmt.Errorf("%w: repository URL must be a plain https URL", workspace.ErrInvalidValue)
	}
	return r.Scope.Validate()
}
