package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/store"
)

// Stable failure reasons of a run's Git preparation. A run that cannot have its
// sources fails with one of them before any step starts; a step never runs
// against an empty or partial workspace.
const (
	// ReasonSourceUnavailable: a pinned commit is missing, the Git service or
	// its worker cannot be reached, a credential was refused or the checkout
	// failed.
	ReasonSourceUnavailable = "source_unavailable"
	// ReasonSourceUnconfigured: the backend has no way to check sources out.
	ReasonSourceUnconfigured = "source_checkout_unconfigured"
)

var (
	// ErrSourceUnavailable wraps every checkout and credential failure of a
	// pinned source.
	ErrSourceUnavailable = errors.New("runner: source unavailable")
	// ErrCheckoutUnconfigured is the refusal of a backend that cannot check a
	// workspace out (for Podman: no KODAMA_GIT_CHECKOUT_IMAGE).
	ErrCheckoutUnconfigured = errors.New("runner: source checkout is not configured")
)

// Directory names inside /workspace for the knowledge base repositories.
const (
	ContextTenantPath  = ".context/tenant"
	ContextProjectPath = ".context/project"
	// contextRepository is the name of a tenant's or a project's knowledge base.
	contextRepository = "context"
)

// CheckoutSource is one repository to place in the workspace. A source is checked
// out detached at exactly Commit. A context source (Context true) is checked out
// at the tip of Ref and never over a path that already exists.
type CheckoutSource struct {
	URL     string // the Git endpoint of the repository; it never carries a credential
	Path    string // relative to the workspace; "." is the workspace itself
	Commit  string // the pinned commit (a source)
	Ref     string // the ref whose tip is checked out (a context)
	Context bool
}

// GitCredentials is where a run's Git authority lives: a directory outside the
// workspace holding the generated Git configuration and one credential file per
// repository. Dir is a host path.
type GitCredentials struct{ Dir string }

// Request is one workspace checkout.
type Request struct {
	// Workspace is the backend's workspace: a directory for Process, the run's
	// volume name for Podman.
	Workspace   string
	Sources     []CheckoutSource
	Credentials GitCredentials
}

// Checkout places repositories in a prepared workspace. An implementation must
// leave no credential in the workspace or in a repository's configuration, and
// must fail rather than leave a partial checkout without an error.
type Checkout interface {
	Checkout(ctx context.Context, req Request) error
}

// Repositories is what the preparation needs of the repository records.
type Repositories interface {
	GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error)
	RepositoryByPath(ctx context.Context, tenant domain.TenantID, projectKey, name string) (store.Repository, error)
}

// IssuedCredential is a run's credential for one repository.
type IssuedCredential struct {
	GrantID   string
	Secret    string
	ExpiresAt time.Time
}

// CredentialIssuer issues a run's Git credential for one repository while the run
// is live. A run that ended, or was cancelled, is refused with store.ErrConflict;
// a repository the run has no authority over is store.ErrNotFound.
type CredentialIssuer interface {
	IssueRunCredential(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID, repositoryID string) (IssuedCredential, error)
}

// RunLiveness reports whether a run still holds Git authority (queued or
// running, not cancelled). A run that is unknown is not live. An error means the
// answer is unknown, never that the run ended.
type RunLiveness interface {
	RunActive(ctx context.Context, tenant domain.TenantID, project domain.ProjectID, runID string) (bool, error)
}

// GitSources prepares a run's Git sources: the checkout of its pinned commits,
// the knowledge base repositories and the run's credential files.
type GitSources struct {
	Checkout     Checkout
	Repositories Repositories
	Credentials  CredentialIssuer
	// Liveness is read on every refresh, independently of when a credential is due
	// for rotation, so a cancelled run's credential files go at once.
	Liveness RunLiveness
	// BaseURL is the Git service's base URL (the server's public URL) without a
	// trailing slash; repositories live at <BaseURL>/git/<tenant>/<project>/<name>.git.
	BaseURL string
	// Dir is the host directory holding the runs' credential directories. It must
	// never be inside a workspace.
	Dir    string
	Logger *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

func (g *GitSources) validate() error {
	switch {
	case g.Checkout == nil:
		return errors.New("runner: git sources need a checkout")
	case g.Repositories == nil:
		return errors.New("runner: git sources need the repositories")
	case g.Credentials == nil:
		return errors.New("runner: git sources need a credential issuer")
	case g.Liveness == nil:
		return errors.New("runner: git sources need a run liveness check")
	case g.BaseURL == "":
		return errors.New("runner: git sources need the Git service's base URL")
	case g.Dir == "":
		return errors.New("runner: git sources need a credential directory")
	case !filepath.IsAbs(g.Dir):
		return errors.New("runner: the credential directory must be absolute")
	}
	return nil
}

func (g *GitSources) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *GitSources) log() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// repoURL renders a repository's endpoint, never with a credential.
func (g *GitSources) repoURL(tenantKey, projectKey string, r store.Repository) string {
	segs := []string{tenantKey}
	if r.ProjectID != "" {
		segs = append(segs, projectKey)
	}
	segs = append(segs, r.Name)
	return strings.TrimSuffix(g.BaseURL, "/") + "/git/" + strings.Join(segs, "/") + ".git"
}

// RunGit is a run's prepared Git authority: the credential directory (outside the
// workspace) and the means to keep it fresh and to end it with the run.
type RunGit struct {
	g       *GitSources
	tenant  domain.TenantID
	project domain.ProjectID
	runID   string
	dir     string
	mu      sync.Mutex
	repos   []*issued
	closed  bool
}

type issued struct {
	id       string
	issuedAt time.Time
	expires  time.Time
	refused  bool // the credential was refused and its file removed
}

// Dir is the host directory the steps' Git configuration and credential files
// live in (KODAMA_GIT_DIR).
func (rg *RunGit) Dir() string {
	if rg == nil {
		return ""
	}
	return rg.dir
}

// Prepare issues the run's credentials, writes them outside the workspace and
// checks the pinned sources and the context repositories out. Every failure
// removes what was written and is an error wrapping ErrSourceUnavailable or
// ErrCheckoutUnconfigured.
func (g *GitSources) Prepare(ctx context.Context, run store.Run, workspace string) (*RunGit, error) {
	if err := g.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCheckoutUnconfigured, err)
	}
	sources := make([]CheckoutSource, 0, len(run.Sources)+2)
	ids := []string{}
	seen := map[string]bool{}
	for _, s := range run.Sources {
		repo, err := g.Repositories.GetRepository(ctx, run.TenantID, s.RepositoryID)
		if err != nil {
			return nil, fmt.Errorf("%w: repository %s: %v", ErrSourceUnavailable, s.RepositoryID, err)
		}
		if repo.ProjectID != run.ProjectID {
			return nil, fmt.Errorf("%w: repository %s is not in the run's project", ErrSourceUnavailable, s.RepositoryID)
		}
		sources = append(sources, CheckoutSource{URL: g.repoURL(run.TenantKey, run.ProjectKey, repo), Path: s.Path, Commit: s.Commit})
		if !seen[s.RepositoryID] {
			seen[s.RepositoryID] = true
			ids = append(ids, s.RepositoryID)
		}
	}
	// The knowledge base: only the repositories that exist and have content; an
	// absent one is not an error.
	type ctxRepo struct {
		projectKey string
		path       string
	}
	var contexts []struct {
		repo store.Repository
		path string
	}
	for _, c := range []ctxRepo{{"", ContextTenantPath}, {run.ProjectKey, ContextProjectPath}} {
		repo, err := g.Repositories.RepositoryByPath(ctx, run.TenantID, c.projectKey, contextRepository)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // the repository does not exist: not an error
		case err != nil:
			return nil, fmt.Errorf("%w: context repository: %w", ErrSourceUnavailable, err)
		case repo.Status != store.RepoReady && repo.Status != store.RepoArchived:
			// It exists but is not usable (still provisioning): the run does not
			// start without a knowledge base it was meant to have.
			return nil, fmt.Errorf("%w: context repository %s is %s", ErrSourceUnavailable, repo.ID, repo.Status)
		case repo.Empty:
			continue // it exists and has nothing to read
		}
		// An archived repository refuses pushes, not history: it is read like any other.
		contexts = append(contexts, struct {
			repo store.Repository
			path string
		}{repo, c.path})
	}

	rg := &RunGit{g: g, tenant: run.TenantID, project: run.ProjectID, runID: run.ID,
		dir: filepath.Join(g.Dir, sanitize(string(run.TenantID))+"_"+sanitize(string(run.ProjectID))+"_"+sanitize(run.ID))}
	if len(sources) == 0 && len(contexts) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(rg.dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: credential directory: %v", ErrSourceUnavailable, err)
	}
	fail := func(err error) (*RunGit, error) {
		_ = rg.Close()
		return nil, err
	}
	urls := map[string]string{} // repositoryId -> URL, for the helper configuration
	for i, s := range run.Sources {
		urls[s.RepositoryID] = sources[i].URL
	}
	for _, id := range ids {
		if err := rg.issue(ctx, id); err != nil {
			return fail(fmt.Errorf("%w: credential for %s: %w", ErrSourceUnavailable, id, err))
		}
	}
	for _, c := range contexts {
		// A knowledge base that exists is part of the run's workspace: when the run
		// cannot read it, the run fails rather than start without it.
		if err := rg.issue(ctx, c.repo.ID); err != nil {
			return fail(fmt.Errorf("%w: credential for context repository %s: %w", ErrSourceUnavailable, c.repo.ID, err))
		}
		urls[c.repo.ID] = g.repoURL(run.TenantKey, run.ProjectKey, c.repo)
		sources = append(sources, CheckoutSource{URL: urls[c.repo.ID], Path: c.path, Ref: gitv1.HeadsPrefix + c.repo.DefaultBranch, Context: true})
	}
	if err := writeGitConfig(rg.dir, urls); err != nil {
		return fail(fmt.Errorf("%w: git configuration: %v", ErrSourceUnavailable, err))
	}
	if len(sources) == 0 {
		_ = rg.Close()
		return nil, nil
	}
	if err := g.Checkout.Checkout(ctx, Request{Workspace: workspace, Sources: sources, Credentials: GitCredentials{Dir: rg.dir}}); err != nil {
		_ = rg.Close()
		if errors.Is(err, ErrCheckoutUnconfigured) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrSourceUnavailable, err)
	}
	return rg, nil
}

func (rg *RunGit) credentialPath(id string) string { return filepath.Join(rg.dir, id+".credential") }

// issue obtains the run's credential for one repository and publishes it
// atomically, replacing a previous one.
func (rg *RunGit) issue(ctx context.Context, repositoryID string) error {
	if !safeID.MatchString(repositoryID) {
		return fmt.Errorf("repository id %q is not usable in a file name", repositoryID)
	}
	cred, err := rg.g.Credentials.IssueRunCredential(ctx, rg.tenant, rg.project, rg.runID, repositoryID)
	if err != nil {
		return err
	}
	if cred.Secret == "" || strings.ContainsAny(cred.Secret, "\r\n \t") {
		return errors.New("the issued credential is not an opaque secret")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if rg.closed {
		return errors.New("the run's Git authority has ended")
	}
	if err := writeAtomic(rg.credentialPath(repositoryID), []byte(cred.Secret+"\n")); err != nil {
		return err
	}
	now := rg.g.now()
	for _, r := range rg.repos {
		if r.id == repositoryID {
			r.issuedAt, r.expires, r.refused = now, cred.ExpiresAt, false
			return nil
		}
	}
	rg.repos = append(rg.repos, &issued{id: repositoryID, issuedAt: now, expires: cred.ExpiresAt})
	return nil
}

// Refresh first reads the run's liveness, whether or not any credential is due:
// a run that was cancelled or finished loses every credential file at once, and
// the refusal wraps store.ErrConflict. It then rotates every credential that has
// used half of its lifetime. The new secret replaces the file atomically, so a Git
// process reads either the old or the new credential, never a torn one. A
// credential the issuer refuses has its file removed. Only a refusal removes a
// file: a failure to answer (the database or the control plane is down) keeps the
// old credential, which stays valid until it expires, and is retried.
func (rg *RunGit) Refresh(ctx context.Context) error {
	if rg == nil {
		return nil
	}
	var errs []error
	switch active, err := rg.g.Liveness.RunActive(ctx, rg.tenant, rg.project, rg.runID); {
	case err != nil:
		errs = append(errs, fmt.Errorf("run liveness: %w", err))
	case !active:
		rg.revokeAll()
		return fmt.Errorf("the run no longer holds Git authority: %w", store.ErrConflict)
	}
	rg.mu.Lock()
	var due []string
	now := rg.g.now()
	for _, r := range rg.repos {
		if r.refused {
			continue
		}
		if !now.Before(r.issuedAt.Add(r.expires.Sub(r.issuedAt) / 2)) {
			due = append(due, r.id)
		}
	}
	rg.mu.Unlock()
	sort.Strings(due)
	for _, id := range due {
		err := rg.issue(ctx, id)
		switch {
		case err == nil:
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
			rg.revokeFile(id)
			errs = append(errs, fmt.Errorf("credential for %s is refused: %w", id, err))
		default:
			errs = append(errs, fmt.Errorf("credential for %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// revokeAll removes every credential file of the run.
func (rg *RunGit) revokeAll() {
	rg.mu.Lock()
	ids := make([]string, 0, len(rg.repos))
	for _, r := range rg.repos {
		ids = append(ids, r.id)
	}
	rg.mu.Unlock()
	for _, id := range ids {
		rg.revokeFile(id)
	}
}

// revokeFile removes a refused credential's file.
func (rg *RunGit) revokeFile(id string) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	_ = os.Remove(rg.credentialPath(id))
	for _, r := range rg.repos {
		if r.id == id {
			r.refused = true
		}
	}
}

// Keep refreshes the credentials every interval until the returned stop is
// called, for the duration of a long step.
func (rg *RunGit) Keep(ctx context.Context, interval time.Duration) (stop func()) {
	if rg == nil || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := rg.Refresh(ctx); err != nil {
					rg.g.log().Warn("rotating the run's Git credential failed", "run", rg.runID, "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-finished
	}
}

// Close ends the run's Git authority on this host: the credential directory,
// with every credential file in it, is removed. It is idempotent. The credential
// itself is revoked by the run's end in the store.
func (rg *RunGit) Close() error {
	if rg == nil {
		return nil
	}
	rg.mu.Lock()
	rg.closed = true
	rg.mu.Unlock()
	return os.RemoveAll(rg.dir)
}

// writeAtomic replaces name with data (mode 0600) so a reader never sees a
// partial file.
func writeAtomic(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// configQuote escapes a value inside a double-quoted Git configuration string.
func configQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// writeGitConfig writes the Git configuration the checkout and the steps share
// (GIT_CONFIG_GLOBAL): one credential helper per repository URL, answering only
// for that URL with the contents of the run's credential file read at the time
// Git asks. The configuration holds no secret, so a rotation needs no rewrite.
func writeGitConfig(dir string, urls map[string]string) error {
	ids := make([]string, 0, len(urls))
	for id := range urls {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("# Generated for one run. It contains no credential.\n[safe]\n\tdirectory = *\n")
	for _, id := range ids {
		if !safeID.MatchString(id) {
			return fmt.Errorf("repository id %q is not usable in a file name", id)
		}
		u := urls[id]
		if strings.ContainsAny(u, "\r\n\x00") || strings.Contains(u, "@") {
			return fmt.Errorf("repository URL for %s is not a plain endpoint", id)
		}
		helper := `!f() { test "$1" = get || exit 0; echo username=` + GitUsername +
			`; echo "password=$(cat "$KODAMA_GIT_DIR/` + id + `.credential")"; }; f`
		fmt.Fprintf(&b, "[credential \"%s\"]\n\thelper = \"%s\"\n", configQuote(u), configQuote(helper))
	}
	return writeAtomic(filepath.Join(dir, GitConfigFile), []byte(b.String()))
}

// Names of the files in a run's credential directory.
const (
	GitConfigFile = "gitconfig"
	// GitUsername is the conventional user name of every Git service credential.
	GitUsername = "kodama"
	// GitContainerDir is where a container sees the credential directory.
	GitContainerDir = "/kodama/git"
)

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// checkoutScript renders the POSIX shell script that performs a checkout in the
// current directory. Process runs it on the host and Podman in a helper
// container, so both perform the same steps: init, remote add, fetch the exact
// commit and detach.
func checkoutScript(sources []CheckoutSource) (string, error) {
	var b strings.Builder
	b.WriteString(`set -eu
G='git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c protocol.version=2 -c protocol.ext.allow=never -c protocol.file.allow=never -c submodule.recurse=false'
clear_path() { # no component of the path may be a symlink, dangling or not
  p=
  for c in $(printf '%s' "$1" | tr '/' ' '); do
    p="${p:+$p/}$c"
    if [ -L "$p" ]; then echo "$1: $p is a symbolic link; refusing to place a repository through it" >&2; exit 1; fi
  done
}
fresh() { # path url
  clear_path "$1"
  mkdir -p -- "$1"
  $G -C "$1" init -q --template=
  $G -C "$1" remote add origin "$2"
}
pin() { # path url commit
  fresh "$1" "$2"
  $G -C "$1" fetch -q --no-tags origin "$3"
  $G -C "$1" checkout -q --detach "$3"
  test "$($G -C "$1" rev-parse HEAD)" = "$3"
}
tip() { # path url ref
  clear_path "$1"
  if [ -e "$1" ]; then echo "context path $1 exists; not overwritten" >&2; return 0; fi
  fresh "$1" "$2"
  $G -C "$1" fetch -q --no-tags origin "$3"
  $G -C "$1" checkout -q --detach FETCH_HEAD
  if [ -d .git ]; then mkdir -p .git/info; grep -qxF '/.context/' .git/info/exclude 2>/dev/null || echo '/.context/' >> .git/info/exclude; fi
}
`)
	for _, s := range sources {
		if err := validateCheckoutSource(s); err != nil {
			return "", err
		}
		if s.Context {
			fmt.Fprintf(&b, "tip %s %s %s\n", shellQuote(s.Path), shellQuote(s.URL), shellQuote(s.Ref))
			continue
		}
		fmt.Fprintf(&b, "pin %s %s %s\n", shellQuote(s.Path), shellQuote(s.URL), shellQuote(s.Commit))
	}
	return b.String(), nil
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

func validateCheckoutSource(s CheckoutSource) error {
	if strings.ContainsAny(s.URL, "\r\n\x00@") || (!strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://")) {
		return fmt.Errorf("source URL %q is not a plain http(s) endpoint", s.URL)
	}
	if s.Context {
		if !strings.HasPrefix(s.Ref, gitv1.HeadsPrefix) || strings.ContainsAny(s.Ref, " \t\r\n\x00") {
			return fmt.Errorf("context ref %q is not a branch", s.Ref)
		}
		if s.Path != ContextTenantPath && s.Path != ContextProjectPath {
			return fmt.Errorf("context path %q is not under .context", s.Path)
		}
		return nil
	}
	if !commitRE.MatchString(s.Commit) {
		return fmt.Errorf("source commit %q is not a full commit ID", s.Commit)
	}
	if s.Path != gitv1.RootPath && (path.Clean(s.Path) != s.Path || gitv1.ValidateSourcePath(s.Path) != nil) {
		return fmt.Errorf("source path %q is not a clean workspace path", s.Path)
	}
	return nil
}
