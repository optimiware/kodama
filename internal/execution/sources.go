package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Source sets in execution (ADR 0011, PX-23-PX-25, seam S6). A run of the
// internal Git service pins its origin and a source set at admission: one exact
// commit per repository of its own project and a unique checkout path each. The
// pins are immutable (a database trigger refuses to change them); a replay of the
// same admission returns the stored commits whatever the branches did since.
//
// The ports below are owned by this package. Adapters over the repository,
// gitpush, gitops and workspace modules implement them (adapters/); the use
// cases never see a Git client, a repository record or a push record.

// SourceRepository is a repository of the run's project resolved for a source.
type SourceRepository struct {
	ID            string
	Name          string
	URL           string // the credential-free https clone URL
	DefaultBranch string
}

// ResolvedSources is the answer of SourceSets.Resolve: the tenant's Git endpoint
// and the named repositories.
type ResolvedSources struct {
	// BaseURL is the https URL of the tenant's Git endpoint without credentials,
	// query, fragment or trailing slash; every repository URL lies beneath it.
	BaseURL string
	// CAFile names the file under /kodama/git/ holding the CA bundle workloads
	// trust, or nil for the system roots.
	CAFile *string
	// Repositories are in the order of the requested names.
	Repositories []SourceRepository
}

// DeclaredSource is one source a run declares: a repository of its project, its
// checkout path and its access ceiling. A document names the repository by its
// logical name and never carries a commit, ref, URL or repository ID; only a
// document without sources has an implicit source, whose repository admission
// supplies (by name from the caller, or by ID from the push origin).
type DeclaredSource struct {
	// Repository is the repository's logical name within the project; empty
	// when RepositoryID selects it.
	Repository   string
	RepositoryID string
	Path         string
	Access       gitv1.SourceAccess
}

// ref is the repository reference SourceSets.Resolve takes.
func (d DeclaredSource) ref() SourceRef { return SourceRef{Name: d.Repository, ID: d.RepositoryID} }

// SourceRef names a repository of the project by logical name or by ID.
type SourceRef struct {
	Name string
	ID   string
}

// SourcePin selects what a source pins when it is not the push origin's source
// or a work item's task branch: the tip of Ref, or exactly Commit. Neither
// means the default branch's tip, read once at admission.
type SourcePin struct {
	Ref    *string
	Commit *string
}

// Validate checks that at most one alternative is chosen and well formed.
func (p SourcePin) Validate() error {
	if p.Ref != nil && p.Commit != nil {
		return invalidf("a source pins a ref or a commit, not both")
	}
	if p.Ref != nil {
		if err := gitv1.ValidateRefName(*p.Ref); err != nil {
			return invalidf("source ref: %v", err)
		}
	}
	if p.Commit != nil && !commitPattern.MatchString(*p.Commit) {
		return invalidf("a pinned commit is a full lowercase object name")
	}
	return nil
}

// PinRequest asks SourceSets.Pin for the exact commit of one source. Push, Ref
// and Commit are alternatives (at most one); none selects the default branch.
type PinRequest struct {
	Repository SourceRepository
	Ref        *string
	Commit     *string
	// Push is a push-originated run's applied push entry: the commit is that
	// entry's new value (gitv1.PinnedCommit), never the branch's tip.
	Push *PushPin
}

// PushPin names the applied push record entry a run started from (PX-24).
type PushPin struct {
	PushID string
	Ref    string
}

// SourceSets resolves, pins and retains a run's sources (S6). All methods run
// outside SQL transactions and every call is scoped to the project.
type SourceSets interface {
	// Resolve returns the referenced repositories of the scope's project, one per
	// reference in order, and the tenant's Git endpoint. A foreign or unknown
	// repository is ErrNotFound, indistinguishable; a repository that is not
	// internally hosted, ready and not archived is ErrAdmissionRefused
	// (hosting_mismatch, repository_not_ready, repository_archived). With no
	// references it returns only the endpoint, so a run without sources (a
	// provisioning run before its repository exists) pins the endpoint too.
	Resolve(ctx context.Context, scope domain.Scope, refs []SourceRef) (ResolvedSources, error)
	// Pin fixes one exact, reachable commit per request, in order, without side
	// effects: the push origin's entry (the record is loaded under the scope; an
	// unapplied record, a refused or deleted entry or a record of another
	// repository or project is ErrAdmissionRefused), an exact commit, the tip of
	// a ref or the default branch's tip. A commit that does not exist or is not
	// reachable is ErrAdmissionRefused.
	Pin(ctx context.Context, scope domain.Scope, reqs []PinRequest) ([]string, error)
	// Retain keeps every pinned commit reachable for the run's retention
	// (retained refs refs/kodama/runs/<runId>/<n>) and, for a run without a work
	// item, creates the run branch of each write source at its pinned commit. It
	// is idempotent on the run: repeating it after a crash creates nothing more.
	Retain(ctx context.Context, scope domain.Scope, run domain.RunID, set gitv1.SourceSet) error
}

// InternalWorkspace is a work item's internally hosted workspace.
type InternalWorkspace struct {
	ID string
}

// WriterAdmission is the evidence a writer claim needs: the workspace version the
// gate was checked at and, per repository, the accepted commit the task branch
// must still have.
type WriterAdmission struct {
	Version  domain.Version
	Accepted map[string]string
}

// CommitOutput is one ref an attempt updated, as its result reports it.
type CommitOutput struct {
	RepositoryID string
	Ref          string
	Commit       string
	PushID       string
}

// InternalWorkspaces is the workspace module's internal-hosting seam (T-147).
// All methods run outside SQL transactions; none is called for a run without a
// work item.
type InternalWorkspaces interface {
	// Provision returns the work item's internally hosted workspace with a task
	// branch in each named repository (by ID), creating whatever is missing. It is
	// idempotent and runs before the source set is built, so the task branch exists
	// when its tip is read.
	Provision(ctx context.Context, scope domain.Scope, item domain.WorkItemID, repositories []string) (InternalWorkspace, error)
	// AdmitWriter synchronizes the workspace's task branches and checks the writer
	// gate. ErrStale: observation raced another change, retry later;
	// ErrBusy: another attempt holds the slot or a push is unsettled or unknown
	// (the writer-blocked family); the run waits.
	AdmitWriter(ctx context.Context, scope domain.Scope, workspaceID string) (WriterAdmission, error)
	// AcceptCheckpoint accepts one commit output of the writer attempt from its
	// push record BEFORE the attempt's writer slot is settled. ErrBusy: the push
	// is not settled yet (retry); ErrAdmissionRefused: the output is refused (no
	// applied record, a secret in the commit, another writer, not a task-branch
	// push); ErrUnavailable: retry.
	AcceptCheckpoint(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, out CommitOutput) error
}

// RunSource is one pinned source of a run with the facts attempts need beside the
// Git contract's entry.
type RunSource struct {
	RepositoryID string             `json:"repositoryId"`
	Repository   string             `json:"repository"`
	URL          string             `json:"url"`
	Commit       string             `json:"commit"`
	Path         string             `json:"path"`
	Access       gitv1.SourceAccess `json:"access"`
	Branch       *string            `json:"branch"`
}

// Source is the Git contract's entry.
func (s RunSource) Source() gitv1.Source {
	return gitv1.Source{RepositoryID: s.RepositoryID, Commit: s.Commit, Path: s.Path, Access: s.Access, Branch: s.Branch}
}

// RunSources is the immutable source-set pin of a run, recorded at admission.
// Its presence marks a run that executes under runtime context v3; a source-free
// manual or provisioning run has no entries.
type RunSources struct {
	// BaseURL and CAFile are the Git endpoint and CA the run's workloads use.
	BaseURL string  `json:"baseUrl"`
	CAFile  *string `json:"caFile"`
	// Declaration is the digest of the declaration the admission pinned; an
	// admission replayed under the same run ID with another declaration is a
	// conflict.
	Declaration string `json:"declaration"`
	// Digest is the canonical digest of the source set (gitv1.SourceSet.Digest);
	// empty for a source-free run.
	Digest  string      `json:"digest"`
	Entries []RunSource `json:"entries"`
}

// Set is the Git contract's source set, ordered and validated.
func (r RunSources) Set() (gitv1.SourceSet, error) {
	srcs := make([]gitv1.Source, len(r.Entries))
	for i, e := range r.Entries {
		srcs[i] = e.Source()
	}
	set := gitv1.SourceSet{Sources: srcs}
	if err := set.Validate(); err != nil {
		return gitv1.SourceSet{}, err
	}
	return set, nil
}

// Empty reports a source-free run.
func (r RunSources) Empty() bool { return len(r.Entries) == 0 }

// WritesSources reports whether any source may be written.
func (r RunSources) WritesSources() bool {
	for _, e := range r.Entries {
		if e.Access == gitv1.AccessWrite {
			return true
		}
	}
	return false
}

// RepositoryIDs lists the distinct repositories of the set in path order.
func (r RunSources) RepositoryIDs() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range r.Entries {
		if !seen[e.RepositoryID] {
			seen[e.RepositoryID] = true
			out = append(out, e.RepositoryID)
		}
	}
	return out
}

// Validate checks the pin: a valid source set whose digest is recorded, every
// URL beneath the endpoint and every write source on a branch of the run's own
// namespace.
func (r RunSources) Validate(origin Origin, run domain.RunID) error {
	if !strings.HasPrefix(r.BaseURL, "https://") || strings.HasSuffix(r.BaseURL, "/") {
		return invalidf("the Git endpoint is a credential-free https URL without a trailing slash")
	}
	if r.Empty() {
		if r.Digest != "" {
			return invalidf("a source-free run has no source set digest")
		}
		return nil
	}
	set, err := r.Set()
	if err != nil {
		return invalidf("source set: %v", err)
	}
	want, err := set.Digest()
	if err != nil {
		return invalidf("source set: %v", err)
	}
	if r.Digest != want {
		return invalidf("the source set digest does not match its sources")
	}
	for _, e := range r.Entries {
		if !strings.HasPrefix(e.URL, r.BaseURL+"/") {
			return invalidf("a repository URL lies outside the Git endpoint")
		}
		if e.Access != gitv1.AccessWrite {
			continue
		}
		branch, err := origin.Branch(run)
		if err != nil {
			return invalidf("%v", err)
		}
		if e.Branch == nil || *e.Branch != branch {
			return invalidf("a write source updates the work item's task branch or the run's own branch only")
		}
	}
	return nil
}

// NewRunSources builds the pin from the resolved repositories and the pinned
// commits (one per declared source, in order). Sources
// are ordered by path; a write source's branch is the origin's. The result is
// validated.
func NewRunSources(origin Origin, run domain.RunID, res ResolvedSources, declared []DeclaredSource, commits []string, declaration string) (*RunSources, error) {
	if len(commits) != len(declared) {
		return nil, invalidf("one pinned commit per source is required")
	}
	if len(res.Repositories) != len(declared) {
		return nil, Refusedf("source_repository_unknown: a declared repository does not exist in the project")
	}
	branch := ""
	for _, d := range declared {
		if d.Access == gitv1.AccessWrite {
			b, err := origin.Branch(run)
			if err != nil {
				return nil, invalidf("%v", err)
			}
			branch = b
			break
		}
	}
	entries := make([]RunSource, len(declared))
	for i, d := range declared {
		repo := res.Repositories[i]
		if (d.Repository != "" && repo.Name != d.Repository) || (d.RepositoryID != "" && repo.ID != d.RepositoryID) {
			return nil, Refusedf("source_repository_unknown: a declared repository does not exist in the project")
		}
		e := RunSource{RepositoryID: repo.ID, Repository: repo.Name, URL: repo.URL, Commit: commits[i], Path: d.Path, Access: d.Access}
		if d.Access == gitv1.AccessWrite {
			b := branch
			e.Branch = &b
		}
		entries[i] = e
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	rs := &RunSources{BaseURL: res.BaseURL, CAFile: res.CAFile, Declaration: declaration, Entries: entries}
	if len(entries) > 0 {
		set, err := rs.Set()
		if err != nil {
			return nil, Refusedf("source set: %v", err)
		}
		if rs.Digest, err = set.Digest(); err != nil {
			return nil, Refusedf("source set: %v", err)
		}
	}
	if err := rs.Validate(origin, run); err != nil {
		return nil, Refusedf("%v", err)
	}
	return rs, nil
}

// declarationDigest identifies what an admission asked for, so a replay under
// the same run ID with another request is a conflict.
func declarationDigest(parts ...any) (string, error) {
	raw, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	c, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return "", invalidf("the declaration is not canonicalizable")
	}
	sum := sha256.Sum256(append([]byte("kodama/source-declaration/v1|"), c...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (r RunSources) String() string { return fmt.Sprintf("%d sources", len(r.Entries)) }
