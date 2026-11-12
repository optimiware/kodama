package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrForeignHook reports an existing hook script this installer did not write.
// A foreign hook is never overwritten silently.
var ErrForeignHook = errors.New("repository hook was not installed by kodama")

// ErrHookSymlink reports a hooks directory or hook script path that is a
// symbolic link. Following it would write the managed hooks outside the
// repository, so the install is refused before any write.
var ErrHookSymlink = errors.New("repository hook path is a symbolic link")

// managedMarker identifies a hook script this installer owns.
const managedMarker = "# managed by kodama: native git hook callback"

// HookReport records exactly what InstallHooks did. It never contains a
// credential or a ref name.
type HookReport struct {
	// Installed lists hooks that did not exist and were written.
	Installed []string
	// Updated lists managed hooks whose content changed.
	Updated []string
	// Unchanged lists managed hooks whose content already matched.
	Unchanged []string
}

// HooksConfig configures native hook installation for one bare repository.
type HooksConfig struct {
	// RepositoryPath is the bare repository directory.
	RepositoryPath string
	// BinaryPath is the absolute kodama binary the hook scripts exec.
	BinaryPath string
}

// InstallHooks writes the two safe native hooks into a bare repository. It
// never follows a symbolic link: a `hooks` directory or managed hook name that
// is a symlink is refused with ErrHookSymlink before any write, so a repository
// can never redirect its managed hooks outside its own tree.
//
// The `update` hook is deliberately NOT installed. The single-ref
// `git.Hook.RunUpdate` path mutates the ref through Service.Update before
// receive-pack applies it, so installing it would move a ref before the native
// receive transaction and could leave a ref changed by a rejected push. The
// batch design uses `pre-receive` to verify and journal the whole push without
// any effect, native receive-pack as the atomic apply, and `post-receive` to
// reconcile by observation. `kodama git hook update` still exists for explicit
// invocation but no installer writes it.
func InstallHooks(config HooksConfig) (HookReport, error) {
	if strings.TrimSpace(config.RepositoryPath) == "" {
		return HookReport{}, fmt.Errorf("hook installation repository path is empty")
	}
	repositoryPath, err := filepath.Abs(config.RepositoryPath)
	if err != nil {
		return HookReport{}, fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Stat(repositoryPath)
	if err != nil {
		return HookReport{}, fmt.Errorf("stat repository path: %w", err)
	}
	if !info.IsDir() {
		return HookReport{}, fmt.Errorf("repository path %q is not a directory", repositoryPath)
	}
	binaryPath, err := filepath.Abs(config.BinaryPath)
	if err != nil || strings.TrimSpace(config.BinaryPath) == "" {
		return HookReport{}, fmt.Errorf("kodama binary path is empty or unresolvable")
	}
	if info, err := os.Stat(binaryPath); err != nil || info.IsDir() {
		return HookReport{}, fmt.Errorf("kodama binary %q is not an executable file", binaryPath)
	}

	hooksDirectory := filepath.Join(repositoryPath, "hooks")
	if err := requireRealHooksDirectory(hooksDirectory); err != nil {
		return HookReport{}, err
	}

	var report HookReport
	for _, phase := range []Phase{PhasePreReceive, PhasePostReceive} {
		name := string(phase)
		path := filepath.Join(hooksDirectory, name)
		content := hookScript(binaryPath, phase)
		if err := requireRealHookPath(path); err != nil {
			return report, err
		}
		existing, err := os.ReadFile(path)
		switch {
		case err == nil:
			if !strings.Contains(string(existing), managedMarker) {
				return report, fmt.Errorf("%w: %s", ErrForeignHook, path)
			}
			if string(existing) == content {
				report.Unchanged = append(report.Unchanged, name)
				continue
			}
			report.Updated = append(report.Updated, name)
		case os.IsNotExist(err):
			report.Installed = append(report.Installed, name)
		default:
			return report, fmt.Errorf("read hook %q: %w", path, err)
		}
		if err := writeHook(path, content); err != nil {
			return report, err
		}
	}
	return report, nil
}

// requireRealHooksDirectory creates the repository's hooks directory when it is
// absent and refuses anything that would make the write land elsewhere. A
// symbolic link named `hooks` is refused before it is followed: writing through
// it would install managed hooks outside the repository, and the adopt path's
// later tree normalization would reject the repository only after the write. A
// non-directory path is refused for the same reason.
func requireRealHooksDirectory(path string) error {
	// One level only: the bare repository directory itself was already checked,
	// so a plain Mkdir cannot create an ancestor.
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create hooks directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect hooks directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrHookSymlink, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository hooks path %q is not a directory", path)
	}
	return nil
}

// requireRealHookPath refuses one managed hook name that is a symbolic link.
// Reading through it could adopt a file outside the repository, and replacing
// it would silently change what the link pointed at; either way the repository
// would not own its hooks, so the install stops before touching the path.
func requireRealHookPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect hook %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s", ErrHookSymlink, path)
	}
	return nil
}

// hookScript builds one fixed-argument wrapper. Ref names are never
// interpolated: git passes them on stdin or argv to the binary, and the script
// only execs a fixed absolute path and phase.
func hookScript(binaryPath string, phase Phase) string {
	return "#!/bin/sh\n" +
		managedMarker + "\n" +
		"exec " + shellQuote(binaryPath) + " git hook " + string(phase) + "\n"
}

// shellQuote single-quotes one fixed path for /bin/sh.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// writeHook atomically writes one executable hook.
func writeHook(path, content string) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".hook-*.tmp")
	if err != nil {
		return fmt.Errorf("create hook temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.WriteString(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write hook temporary file: %w", err)
	}
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return fmt.Errorf("set hook mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync hook temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close hook temporary file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace hook %q: %w", path, err)
	}
	if err := syncHookDirectory(directory); err != nil {
		return fmt.Errorf("sync hook directory: %w", err)
	}
	return nil
}

// syncHookDirectory fsyncs the hooks directory so a replaced hook script
// survives a crash.
func syncHookDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// BareRepositoryConfig configures bare repository creation.
type BareRepositoryConfig struct {
	// Path is the bare repository directory.
	Path string
	// GitPath is the native git binary. It defaults to "git".
	GitPath string
	// Env is the exact child environment. A nil slice selects the documented
	// minimal environment.
	Env []string
}

// InitBareRepository creates one bare repository with the minimal transport
// configuration the smart-HTTP backend needs. It does NOT set
// receive.denyDeletes or receive.denyNonFastForwards: ref policy belongs to
// Kodama's ordered rules and the hook, never to git config.
func InitBareRepository(ctx context.Context, config BareRepositoryConfig) error {
	if strings.TrimSpace(config.Path) == "" {
		return fmt.Errorf("bare repository path is empty")
	}
	path, err := filepath.Abs(config.Path)
	if err != nil {
		return fmt.Errorf("resolve bare repository path: %w", err)
	}
	gitPath := strings.TrimSpace(config.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	env := config.Env
	if env == nil {
		env = minimalEnvironment()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create repository parent: %w", err)
	}
	commands := [][]string{
		{"init", "--bare", "--quiet", path},
		{"-C", path, "symbolic-ref", "HEAD", "refs/heads/main"},
		{"-C", path, "config", "receive.advertiseAtomic", "true"},
		{"-C", path, "config", "http.receivepack", "true"},
	}
	for _, args := range commands {
		if err := runGit(ctx, gitPath, env, args...); err != nil {
			return err
		}
	}
	return nil
}

func runGit(ctx context.Context, gitPath string, env []string, args ...string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	command := exec.CommandContext(ctx, gitPath, args...)
	command.Env = env
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// minimalEnvironment mirrors the app executor's explicit child environment so
// a native command never inherits ambient policy or credentials.
func minimalEnvironment() []string {
	env := []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	if path, ok := os.LookupEnv("PATH"); ok && path != "" {
		env = append(env, "PATH="+path)
	}
	if home, ok := os.LookupEnv("HOME"); ok && home != "" {
		env = append(env, "HOME="+home)
	}
	return env
}
