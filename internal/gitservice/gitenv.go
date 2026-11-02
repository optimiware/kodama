package gitservice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Server-owned Git configuration (GR-08). It is applied three ways so no
// repository-local setting can replace it:
//
//   - as command-scope configuration (GIT_CONFIG_COUNT), the highest
//     precedence Git has: it overrides a repository's own config file, which
//     is the only place a repository-local `core.hooksPath` or
//     `uploadpack.allowAnySHA1InWant` could come from;
//   - in a worker-owned global file (GIT_CONFIG_GLOBAL) with the system file
//     disabled (GIT_CONFIG_NOSYSTEM), so a Git installation's own defaults
//     (credential helpers, filters, hooks) are never read;
//   - written into each repository at initialization (provisioning step 2),
//     so the repository on the volume also carries the settings.
//
// The environment of every Git process starts empty: no inherited GIT_*
// variable, HOME or credential reaches it.

// configPair is one Git configuration entry.
type configPair struct{ key, value string }

// serverConfig lists the enforced settings. hooksPath is where the shared
// server-owned hooks live; maxPush is receive.maxInputSize.
func serverConfig(hooksPath string, maxPush int64) []configPair {
	return []configPair{
		{"core.hooksPath", hooksPath},
		{"core.logAllRefUpdates", "always"},
		{"core.attributesFile", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"safe.directory", "*"},
		{"receive.fsckObjects", "true"},
		{"receive.maxInputSize", strconv.FormatInt(maxPush, 10)},
		{"receive.advertisePushOptions", "true"},
		{"receive.autogc", "false"},
		{"receive.hideRefs", "refs/kodama"},
		{"uploadpack.hideRefs", "refs/kodama"},
		{"uploadpack.allowAnySHA1InWant", "false"},
		{"uploadpack.allowReachableSHA1InWant", "false"},
		{"uploadpack.allowTipSHA1InWant", "false"},
		{"uploadpack.allowFilter", "false"},
		{"http.getanyfile", "false"},
		{"gc.auto", "0"},
		{"gc.pruneExpire", "2.weeks.ago"},
		{"maintenance.auto", "false"},
	}
}

// repoInitConfig are the settings written into a repository's own config at
// initialization: the provisioning contract (core.logAllRefUpdates,
// receive.fsckObjects, size limit, hidden refs and the hooks path).
func repoInitConfig(hooksPath string, maxPush int64) []configPair {
	return []configPair{
		{"core.logAllRefUpdates", "always"},
		{"core.hooksPath", hooksPath},
		{"receive.fsckObjects", "true"},
		{"receive.maxInputSize", strconv.FormatInt(maxPush, 10)},
		{"receive.hideRefs", "refs/kodama"},
		{"uploadpack.hideRefs", "refs/kodama"},
		{"uploadpack.allowAnySHA1InWant", "false"},
		{"uploadpack.allowReachableSHA1InWant", "false"},
		{"uploadpack.allowTipSHA1InWant", "false"},
	}
}

// globalConfigFile renders the worker-owned global configuration.
func globalConfigFile(pairs []configPair) []byte {
	sections := map[string][]configPair{}
	var order []string
	for _, p := range pairs {
		section, name, _ := strings.Cut(p.key, ".")
		if _, ok := sections[section]; !ok {
			order = append(order, section)
		}
		sections[section] = append(sections[section], configPair{name, p.value})
	}
	var b bytes.Buffer
	b.WriteString("# Written by the Kodama Git worker. Do not edit: it is rewritten at start.\n")
	for _, s := range order {
		fmt.Fprintf(&b, "[%s]\n", s)
		for _, p := range sections[s] {
			fmt.Fprintf(&b, "\t%s = %s\n", p.key, strconv.Quote(p.value))
		}
	}
	return b.Bytes()
}

// layout names the worker's paths under the data root.
type layout struct {
	root         string
	repositories string
	hooks        string
	spool        string
	tmp          string
	fences       string
	home         string
	globalConfig string
}

func newLayout(root string) layout {
	return layout{
		root:         root,
		repositories: filepath.Join(root, "repositories"),
		hooks:        filepath.Join(root, "hooks"),
		spool:        filepath.Join(root, "spool"),
		tmp:          filepath.Join(root, "tmp"),
		fences:       filepath.Join(root, "restore-fences"),
		home:         filepath.Join(root, "home"),
		globalConfig: filepath.Join(root, "config"),
	}
}

// gitEnv builds the sanitized environment of a Git process. extra entries are
// appended last and win. hooks selects the server hooks (true) or none (false,
// for initialization and maintenance, which must never run a hook).
func (s *Service) gitEnv(hooks bool, extra ...string) []string {
	hooksPath := s.layout.hooks
	if !hooks {
		hooksPath = os.DevNull
	}
	pairs := serverConfig(hooksPath, s.cfg.Limits.MaxPushBytes)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.layout.home,
		"TMPDIR=" + s.layout.tmp,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + s.layout.globalConfig,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(pairs)),
	}
	for i, p := range pairs {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p.key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p.value))
	}
	return append(env, extra...)
}

// gitResult is the outcome of one Git invocation.
type gitResult struct {
	Stdout   []byte
	Stderr   string
	ExitCode int
}

var errGitFailed = errors.New("git failed")

// runGit runs Git against one repository with the sanitized environment. A
// non-zero exit is reported in the result, not as an error; only a failure to
// run Git at all is an error.
func (s *Service) runGit(ctx context.Context, repoDir string, hooks bool, extraEnv []string, args ...string) (gitResult, error) {
	full := append([]string{"--git-dir=" + repoDir}, args...)
	return s.execGit(ctx, repoDir, hooks, extraEnv, full...)
}

// execGit runs a read-only Git with args exactly as given. Every Git process
// receives the generation lock's descriptor (cmd.ExtraFiles, which is not
// close-on-exec in the child) and so do all its descendants.
func (s *Service) execGit(ctx context.Context, dir string, hooks bool, extraEnv []string, args ...string) (gitResult, error) {
	return s.execGitTracked(ctx, dir, "", hooks, extraEnv, args...)
}

// runGitTracked is runGit for a command that changes the repository: its
// process group is verified gone afterwards (fence.go), whether or not the hooks
// are enabled.
func (s *Service) runGitTracked(ctx context.Context, repoDir string, hooks bool, extraEnv []string, args ...string) (gitResult, error) {
	id, _ := repositoryOf(s, repoDir)
	full := append([]string{"--git-dir=" + repoDir}, args...)
	return s.execGitTracked(ctx, repoDir, id, hooks, extraEnv, full...)
}

// execGitTracked is execGit for a command that changes a repository (a ref, its
// configuration, HEAD, its objects): with a repositoryID its process group is
// killed and verified gone after the command ends, and the repository is fenced
// until it is. Every mutating command is tracked, with hooks or without.
func (s *Service) execGitTracked(ctx context.Context, dir, repositoryID string, hooks bool, extraEnv []string, args ...string) (gitResult, error) {
	return s.execGitIO(ctx, dir, repositoryID, hooks, extraEnv, nil, nil, args...)
}

// execGitIO is execGitTracked with an optional stdin and an optional stdout
// sink (the answer is then not collected in the result).
func (s *Service) execGitIO(ctx context.Context, dir, repositoryID string, hooks bool, extraEnv []string, stdin io.Reader, stdoutSink io.Writer, args ...string) (gitResult, error) {
	cmd := exec.CommandContext(ctx, s.cfg.GitBinary, args...)
	cmd.Stdin = stdin
	cmd.Dir = dir
	cmd.Env = s.gitEnv(hooks, extraEnv...)
	setProcessGroup(cmd)
	cmd.ExtraFiles = []*os.File{s.lock}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if stdoutSink != nil {
		cmd.Stdout = stdoutSink
	}
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: 64 << 10}
	res := gitResult{}
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
		if repositoryID != "" {
			s.groupExited(repositoryID, s.repoPath(repositoryID), cmd, "", ctx.Err() != nil)
		}
	}
	res.Stdout, res.Stderr = stdout.Bytes(), strings.TrimSpace(stderr.String())
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
	default:
		return res, err
	}
	return res, nil
}

// must turns a non-zero exit into an error carrying Git's message.
func (r gitResult) must(op string) error {
	if r.ExitCode != 0 {
		return fmt.Errorf("%w: %s: exit %d: %s", errGitFailed, op, r.ExitCode, truncateText(r.Stderr, 512))
	}
	return nil
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// limitedWriter keeps the first remaining bytes written and discards the rest.
type limitedWriter struct {
	w         *bytes.Buffer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.remaining > 0 {
		if len(p) > l.remaining {
			p = p[:l.remaining]
		}
		l.w.Write(p)
		l.remaining -= len(p)
	}
	return n, nil
}
