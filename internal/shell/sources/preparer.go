// Package sources is the shell's source preparation for a run: it turns the
// run's pinned Git sources and its knowledge-base repositories into the
// [runtime.PreparationInputs] a [runtime.ResourceManager] consumes, and issues the
// run-scoped Git credentials those inputs refer to through the secret custody
// adapter. The kernel learns nothing about Git from it: a locator is an opaque
// string, a revision an exact commit, a credential a secret reference.
//
// A run's sources are pinned twice over. The commits a run declared are used as
// declared. A context repository has no commit of its own: its branch tip is
// resolved once, recorded durably, and every later call for the same run and plan
// digest returns the recorded commit, so a restarted engine computes the same
// digest and adopts its workspace instead of preparing it again.
package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/shell/store"
)

// ErrSourceUnavailable wraps every failure to determine a run's pinned sources: a
// repository or run that cannot be read, a commit that is not pinned, a
// knowledge base that is not usable or a branch tip that cannot be resolved.
var ErrSourceUnavailable = errors.New("sources: source unavailable")

// Repositories is what preparation needs of the repository records.
type Repositories interface {
	GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error)
	RepositoryByPath(ctx context.Context, tenant domain.TenantID, projectKey, name string) (store.Repository, error)
}

// Keys names a scope in the Git service's URLs.
type Keys interface {
	// ScopeKeys returns the tenant's and the project's URL keys.
	ScopeKeys(ctx context.Context, scope domain.Scope) (tenantKey, projectKey string, err error)
}

// Tips resolves the commit a branch of a repository points at now.
type Tips interface {
	Tip(ctx context.Context, scope domain.Scope, repo store.Repository, ref string) (commit string, err error)
}

// Pins durably records the commit a context repository was pinned to.
type Pins interface {
	// Get returns the recorded commit, if any.
	Get(ctx context.Context, key string) (commit string, ok bool, err error)
	// PutIfAbsent records commit under key unless key is already recorded, and
	// returns what is recorded: the first writer wins, so racing engines agree.
	PutIfAbsent(ctx context.Context, key, commit string) (string, error)
}

// Paths of the knowledge-base repositories in the workspace.
const (
	contextTenantPath  = ".context/tenant"
	contextProjectPath = ".context/project"
	contextRepository  = "context"
)

// PreparerConfig builds a Preparer.
type PreparerConfig struct {
	Runs         RunInfoReader
	Repositories Repositories
	Keys         Keys
	Tips         Tips
	Pins         Pins
	// BaseURL is the Git service's base URL; repositories live at
	// <BaseURL>/git/<tenant>/<project>/<name>.git. It never carries a credential.
	BaseURL string
}

// Preparer is the shell's [runtime.WorkspacePreparer].
type Preparer struct{ cfg PreparerConfig }

var _ runtime.WorkspacePreparer = (*Preparer)(nil)

// NewPreparer validates the wiring.
func NewPreparer(cfg PreparerConfig) (*Preparer, error) {
	switch {
	case cfg.Runs == nil || cfg.Repositories == nil || cfg.Keys == nil || cfg.Tips == nil || cfg.Pins == nil:
		return nil, errors.New("sources: the preparer needs runs, repositories, keys, tips and pins")
	case !strings.HasPrefix(cfg.BaseURL, "https://") && !strings.HasPrefix(cfg.BaseURL, "http://"):
		return nil, errors.New("sources: the Git base URL must be an http(s) endpoint")
	case strings.Contains(cfg.BaseURL, "@") || strings.ContainsAny(cfg.BaseURL, "\r\n\x00"):
		return nil, errors.New("sources: the Git base URL must not carry a credential")
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	return &Preparer{cfg: cfg}, nil
}

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	repoIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,58}$`)
)

// credentialPrefix starts the name of a Git credential reference; the rest is
// the hex of the repository id, so the name is a valid secret name and decodes
// back to exactly one repository.
const credentialPrefix = "KODAMA_GIT_"

// CredentialName is the reference naming the run's Git credential for a repository.
func CredentialName(repositoryID string) (string, error) {
	if !repoIDRE.MatchString(repositoryID) {
		return "", fmt.Errorf("%w: repository id %q is not usable", ErrSourceUnavailable, repositoryID)
	}
	return credentialPrefix + hex.EncodeToString([]byte(repositoryID)), nil
}

// repositoryOf is the inverse of CredentialName.
func repositoryOf(name string) (string, bool) {
	hexID, ok := strings.CutPrefix(name, credentialPrefix)
	if !ok {
		return "", false
	}
	raw, err := hex.DecodeString(hexID)
	if err != nil || !repoIDRE.MatchString(string(raw)) {
		return "", false
	}
	return string(raw), true
}

func (p *Preparer) url(tenantKey, projectKey string, r store.Repository) string {
	segs := []string{tenantKey}
	if r.ProjectID != "" {
		segs = append(segs, projectKey)
	}
	segs = append(segs, r.Name)
	return p.cfg.BaseURL + "/git/" + strings.Join(segs, "/") + ".git"
}

func unavailable(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrSourceUnavailable}, args...)...)
}

// Inputs returns the run's pinned preparation inputs, or an empty source set for
// a source-free run. It is deterministic for one (run, plan digest).
func (p *Preparer) Inputs(ctx context.Context, req runtime.PrepareRequest) (runtime.PreparationInputs, error) {
	if err := req.Scope.Validate(); err != nil {
		return runtime.PreparationInputs{}, err
	}
	if err := req.RunID.Validate(); err != nil {
		return runtime.PreparationInputs{}, err
	}
	if strings.TrimSpace(req.PlanDigest) == "" {
		return runtime.PreparationInputs{}, fmt.Errorf("%w: preparation names the plan digest", domain.ErrInvalidValue)
	}
	info, err := p.cfg.Runs.RunGitInfo(ctx, req.Scope.TenantID, req.Scope.ProjectID, string(req.RunID))
	if err != nil {
		return runtime.PreparationInputs{}, unavailable("run %s: %v", req.RunID, err)
	}
	var declared []gitv1.Source
	if len(info.Sources) > 0 && string(info.Sources) != "null" {
		if err := json.Unmarshal(info.Sources, &declared); err != nil {
			return runtime.PreparationInputs{}, unavailable("the run's source set is unreadable")
		}
	}
	tenantKey, projectKey, err := p.cfg.Keys.ScopeKeys(ctx, req.Scope)
	if err != nil {
		return runtime.PreparationInputs{}, unavailable("scope keys: %v", err)
	}

	out := runtime.PreparationInputs{Scope: req.Scope, RunID: req.RunID}
	ids := map[string]bool{}
	sort.SliceStable(declared, func(i, j int) bool { return declared[i].Path < declared[j].Path })
	for _, s := range declared {
		repo, err := p.cfg.Repositories.GetRepository(ctx, req.Scope.TenantID, s.RepositoryID)
		if err != nil {
			return runtime.PreparationInputs{}, unavailable("repository %s: %v", s.RepositoryID, err)
		}
		if repo.ProjectID != req.Scope.ProjectID {
			return runtime.PreparationInputs{}, unavailable("repository %s is not in the run's project", s.RepositoryID)
		}
		if !commitRE.MatchString(s.Commit) {
			return runtime.PreparationInputs{}, unavailable("source %s is not pinned to a full commit", s.RepositoryID)
		}
		out.Sources = append(out.Sources, runtime.SourceInput{Name: "source:" + s.Path, Locator: p.url(tenantKey, projectKey, repo), Revision: s.Commit, Path: s.Path})
		ids[s.RepositoryID] = true
	}

	// The knowledge base: only repositories that exist and have content; an absent one is not an error.
	for _, c := range []struct{ key, path, name string }{
		{"", contextTenantPath, "context:tenant"},
		{projectKey, contextProjectPath, "context:project"},
	} {
		repo, err := p.cfg.Repositories.RepositoryByPath(ctx, req.Scope.TenantID, c.key, contextRepository)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return runtime.PreparationInputs{}, unavailable("context repository: %v", err)
		case repo.Status != store.RepoReady && repo.Status != store.RepoArchived:
			// It exists and is not usable: the run does not start without a knowledge base it was meant to have.
			return runtime.PreparationInputs{}, unavailable("context repository %s is %s", repo.ID, repo.Status)
		case repo.Empty:
			continue
		}
		ref := gitv1.HeadsPrefix + repo.DefaultBranch
		pin := pinKey(req, repo.ID, ref)
		commit, err := p.pinned(ctx, req.Scope, repo, ref, pin)
		if err != nil {
			return runtime.PreparationInputs{}, err
		}
		out.Sources = append(out.Sources, runtime.SourceInput{Name: c.name, Locator: p.url(tenantKey, projectKey, repo), Revision: commit, Path: c.path})
		ids[repo.ID] = true
	}

	names := make([]string, 0, len(ids))
	for id := range ids {
		n, err := CredentialName(id)
		if err != nil {
			return runtime.PreparationInputs{}, err
		}
		names = append(names, n)
	}
	sort.Strings(names)
	owner := domain.Owner{TenantID: req.Scope.TenantID, ProjectID: req.Scope.ProjectID}
	for _, n := range names {
		out.Credentials = append(out.Credentials, runtime.SecretRef{Owner: owner, Name: n})
	}
	out.Digest = digest(req, out)
	if err := out.Validate(); err != nil {
		return runtime.PreparationInputs{}, fmt.Errorf("%w: %w", ErrSourceUnavailable, err)
	}
	return out, nil
}

// pinned returns the commit a context repository is pinned to for this run and
// plan: the recorded one, or the branch tip resolved now and recorded. Once a pin
// is recorded the branch is never read again for this run and plan.
func (p *Preparer) pinned(ctx context.Context, scope domain.Scope, repo store.Repository, ref, key string) (string, error) {
	commit, ok, err := p.cfg.Pins.Get(ctx, key)
	if err != nil {
		return "", unavailable("reading the pin of context repository %s: %v", repo.ID, err)
	}
	if !ok {
		tip, err := p.cfg.Tips.Tip(ctx, scope, repo, ref)
		if err != nil {
			return "", unavailable("the tip of context repository %s: %v", repo.ID, err)
		}
		if !commitRE.MatchString(tip) {
			return "", unavailable("the tip of context repository %s is not a full commit", repo.ID)
		}
		if commit, err = p.cfg.Pins.PutIfAbsent(ctx, key, tip); err != nil {
			return "", unavailable("recording the pin of context repository %s: %v", repo.ID, err)
		}
	}
	if !commitRE.MatchString(commit) {
		return "", unavailable("the recorded pin of context repository %s is not a full commit", repo.ID)
	}
	return commit, nil
}

func pinKey(req runtime.PrepareRequest, repositoryID, ref string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(req.Scope.TenantID), string(req.Scope.ProjectID), string(req.RunID), req.PlanDigest, repositoryID, ref}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// digest identifies the exact input set: the run, the plan and every pinned input.
func digest(req runtime.PrepareRequest, in runtime.PreparationInputs) string {
	doc, _ := json.Marshal(struct {
		Scope       domain.Scope
		RunID       domain.RunID
		PlanDigest  string
		Sources     []runtime.SourceInput
		Credentials []runtime.SecretRef
	}{in.Scope, in.RunID, req.PlanDigest, in.Sources, in.Credentials})
	sum := sha256.Sum256(doc)
	return "sha256:" + hex.EncodeToString(sum[:])
}
