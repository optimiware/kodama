package adapters

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/repository"
)

// SourceRepositories is the repository module's read side the source-set path
// consumes (repository.Store satisfies it).
type SourceRepositories interface {
	Get(ctx context.Context, scope domain.Scope, id repository.ID) (*repository.Repository, error)
	GetByName(ctx context.Context, scope domain.Scope, name string) (*repository.Repository, error)
}

// SourceHosts is the part of the tenant Git worker's port (repository.Hosts)
// the source-set path consumes.
type SourceHosts interface {
	ReadRefValues(ctx context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error)
	SetRetainedRef(ctx context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error)
	CreateBranch(ctx context.Context, tenant domain.TenantID, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error)
}

// SourcePushes reloads a push record under the run's scope (gitpush.Service).
type SourcePushes interface {
	Reload(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error)
}

// SourceEndpoint is the tenant's Git endpoint as configuration knows it.
type SourceEndpoint struct {
	// BaseURL is https://<git-host>/git without credentials or a trailing slash.
	BaseURL string
	// CAFile names the file under /kodama/git/ holding the CA bundle, or "".
	CAFile string
}

// SourceSets implements execution.SourceSets over the repository, gitpush and
// repository host modules. Every Git read and write happens outside SQL, through
// the tenant's worker, idempotently on the repository and the run: repeating
// Retain after a crash creates nothing more.
type SourceSets struct {
	repos    SourceRepositories
	urls     repository.CloneURLs
	hosts    SourceHosts
	pushes   SourcePushes
	endpoint SourceEndpoint
}

var _ execution.SourceSets = (*SourceSets)(nil)

// NewSourceSets requires every collaborator and an https endpoint.
func NewSourceSets(repos SourceRepositories, urls repository.CloneURLs, hosts SourceHosts, pushes SourcePushes, endpoint SourceEndpoint) (*SourceSets, error) {
	if repos == nil || urls == nil || hosts == nil || pushes == nil {
		return nil, errors.New("execution/adapters: repositories, clone URLs, hosts and push records are required")
	}
	if !strings.HasPrefix(endpoint.BaseURL, "https://") || strings.HasSuffix(endpoint.BaseURL, "/") {
		return nil, errors.New("execution/adapters: the Git endpoint is an https URL without a trailing slash")
	}
	return &SourceSets{repos: repos, urls: urls, hosts: hosts, pushes: pushes, endpoint: endpoint}, nil
}

// hostError classifies a failed worker call: a refusal is a conflict or a
// contract refusal of the request; anything else (undelivered, unknown) is an
// outage the caller repeats, never an assumed outcome.
func hostError(err error, what string) error {
	var ce *repository.CallError
	if errors.As(err, &ce) && ce.Outcome == repository.Refused && ce.Err != nil {
		switch ce.Err.Code {
		case domain.CodeNotFound:
			return execution.ErrNotFound
		case gitv1.CodeRetainedRefExists, gitv1.CodeBranchExists, domain.CodeIdempotencyConflict:
			return fmt.Errorf("%w: %s refused: %s", execution.ErrConflict, what, ce.Err.Code)
		}
		return execution.Refusedf("%s refused: %s", what, ce.Err.Code)
	}
	return execution.Unavailablef(err, "the Git worker could not complete %s", what)
}

// Resolve implements execution.SourceSets.
func (s *SourceSets) Resolve(ctx context.Context, scope domain.Scope, refs []execution.SourceRef) (execution.ResolvedSources, error) {
	out := execution.ResolvedSources{BaseURL: s.endpoint.BaseURL}
	if s.endpoint.CAFile != "" {
		f := s.endpoint.CAFile
		out.CAFile = &f
	}
	for _, ref := range refs {
		var r *repository.Repository
		var err error
		if ref.ID != "" {
			r, err = s.repos.Get(ctx, scope, repository.ID(ref.ID))
		} else {
			r, err = s.repos.GetByName(ctx, scope, ref.Name)
		}
		switch {
		case errors.Is(err, repository.ErrNotFound):
			return execution.ResolvedSources{}, execution.ErrNotFound
		case err != nil:
			return execution.ResolvedSources{}, execution.Unavailablef(err, "the repository could not be read")
		case r.Hosting != gitv1.HostingInternal:
			return execution.ResolvedSources{}, execution.Refusedf("hosting_mismatch: a source is an internally hosted repository")
		case r.Status == gitv1.StatusArchived:
			return execution.ResolvedSources{}, execution.Refusedf("repository_archived")
		case r.Status != gitv1.StatusReady:
			return execution.ResolvedSources{}, execution.Refusedf("repository_not_ready")
		}
		url, err := s.urls.CloneURL(ctx, scope, r.Name)
		if err != nil {
			return execution.ResolvedSources{}, execution.Unavailablef(err, "the clone URL could not be built")
		}
		if !strings.HasPrefix(url, s.endpoint.BaseURL+"/") {
			return execution.ResolvedSources{}, execution.Unavailablef(nil, "a repository URL lies outside the Git endpoint")
		}
		out.Repositories = append(out.Repositories, execution.SourceRepository{ID: string(r.ID), Name: r.Name, URL: url, DefaultBranch: r.DefaultBranch})
	}
	return out, nil
}

// Pin implements execution.SourceSets. A push origin pins the new value of its
// applied entry (gitv1.PinnedCommit over the record reloaded under the run's
// scope; a record of another repository is refused), an exact commit itself,
// a ref's or the default branch's tip the value the worker reads now. Nothing
// is created or retained here.
//
// An exact commit is not verified to exist at this seam (the worker's port has
// no object read): it is refused when Retain asks the worker to pin it.
func (s *SourceSets) Pin(ctx context.Context, scope domain.Scope, reqs []execution.PinRequest) ([]string, error) {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		switch {
		case r.Push != nil:
			rec, err := s.pushes.Reload(ctx, scope, r.Repository.ID, r.Push.PushID)
			var ce *gitv1.ContractError
			switch {
			case errors.As(err, &ce) && ce.Code == domain.CodeNotFound:
				return nil, execution.Refusedf("push_record_unavailable")
			case err != nil:
				return nil, execution.Unavailablef(err, "the push record could not be read")
			case rec.RepositoryID != r.Repository.ID:
				return nil, execution.Refusedf("push_record_unavailable")
			}
			commit, err := gitv1.PinnedCommit(rec, r.Push.Ref)
			if err != nil {
				return nil, execution.Refusedf("push_not_applied")
			}
			out[i] = commit
		case r.Commit != nil:
			out[i] = *r.Commit
		default:
			ref := gitv1.HeadsPrefix + r.Repository.DefaultBranch
			if r.Ref != nil {
				ref = *r.Ref
			}
			resp, err := s.hosts.ReadRefValues(ctx, scope.TenantID, gitv1.ReadRefValuesRequest{RepositoryID: r.Repository.ID, Refs: []string{ref}})
			if err != nil {
				return nil, hostError(err, "reading a ref")
			}
			if len(resp.Values) != 1 || resp.Values[0].Ref != ref || resp.Values[0].Commit == nil || gitv1.IsZeroObjectID(*resp.Values[0].Commit) {
				return nil, execution.Refusedf("source_ref_unknown: a pinned ref does not exist")
			}
			out[i] = *resp.Values[0].Commit
		}
	}
	return out, nil
}

// maxRetainTries bounds how many retained-ref numbers one commit tries when the
// worker reports them taken by other commits of the run (a checkpoint of the
// workspace module numbers the run's refs too).
const maxRetainTries = 8

// Retain implements execution.SourceSets: each pinned commit gets a retained
// ref refs/kodama/runs/<run>/<n> of its repository, and the run branch of each
// write source of a run without a work item is created at its pinned commit.
// The operation IDs are stable per run, repository and number, so a repeat after
// a crash converges: a number found holding this commit counts as retained, one
// holding another commit moves to the next number.
func (s *SourceSets) Retain(ctx context.Context, scope domain.Scope, run domain.RunID, set gitv1.SourceSet) error {
	for _, src := range set.Sources {
		if err := s.retainCommit(ctx, scope, run, src); err != nil {
			return err
		}
		if src.Branch == nil || strings.HasPrefix(*src.Branch, gitv1.TaskBranchPrefix) {
			continue // a read source, or a task branch the workspace provisioned
		}
		if _, err := s.hosts.CreateBranch(ctx, scope.TenantID, gitv1.CreateBranchRequest{
			RepositoryID: src.RepositoryID, OperationID: "branch-" + string(run) + "-" + src.RepositoryID, Ref: *src.Branch, Commit: src.Commit}); err != nil {
			return hostError(err, "creating the run branch")
		}
	}
	return nil
}

func (s *SourceSets) retainCommit(ctx context.Context, scope domain.Scope, run domain.RunID, src gitv1.Source) error {
	for n := int64(1); n <= maxRetainTries; n++ {
		ref, err := gitv1.RetainedRef(string(run), n)
		if err != nil {
			return execution.Invalidf("%v", err)
		}
		_, err = s.hosts.SetRetainedRef(ctx, scope.TenantID, gitv1.SetRetainedRefRequest{
			RepositoryID: src.RepositoryID, OperationID: fmt.Sprintf("retain-%s-%s-%d", run, src.RepositoryID, n), Ref: ref, Commit: src.Commit})
		var ce *repository.CallError
		if err == nil {
			return nil
		}
		if !errors.As(err, &ce) || ce.Outcome != repository.Refused || ce.Err == nil || ce.Err.Code != gitv1.CodeRetainedRefExists {
			return hostError(err, "retaining a commit")
		}
		// The number is taken: by this commit (an earlier try finished) or by another.
		resp, rerr := s.hosts.ReadRefValues(ctx, scope.TenantID, gitv1.ReadRefValuesRequest{RepositoryID: src.RepositoryID, Refs: []string{ref}})
		if rerr != nil {
			return hostError(rerr, "reading a retained ref")
		}
		if len(resp.Values) == 1 && resp.Values[0].Commit != nil && *resp.Values[0].Commit == src.Commit {
			return nil
		}
	}
	return fmt.Errorf("%w: the run's retained ref numbers are taken", execution.ErrConflict)
}
