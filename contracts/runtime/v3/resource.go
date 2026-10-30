package runtimev3

import (
	"fmt"
	"regexp"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Bounds of the preparation, the same as v2's (engineering bounds where the
// contract is silent).
const (
	maxPrepareTimeout = 604800
	maxPrepareTries   = 100
	maxByteCount      = int64(1) << 40
)

// ResourceBasePath is the route family of the resource subjects of a v3 run.
// The audience is the unchanged kodama-runtime-resource of v2.
const (
	ResourceBasePath = "/runtime/v3/runs/{runId}/resources/{resourceKey}"
	ResourceAudience = runtimev2.ResourceAudience
)

var safeNameRE = regexp.MustCompile(runtimev1.Patterns["SafeName"])

// SourcePreparation projects the attempt context's sources to the assignment of
// the one preparation workload of the run, in the same order.
func (w WorkspaceV3) SourcePreparation() []PreparationSource {
	out := make([]PreparationSource, len(w.Sources))
	for i, s := range w.Sources {
		out[i] = PreparationSource{RepositoryID: s.RepositoryID, RepositoryURL: s.RepositoryURL, Commit: GitCommit(s.Commit), Path: s.Path}
	}
	return out
}

// GrantRepositories converts the delivery into the repository claims of the
// preparation grant: read access, no update pattern, no seed, no integration.
func (g PreparationGit) GrantRepositories() []gitv1.GrantRepository {
	out := make([]gitv1.GrantRepository, len(g.Repositories))
	for i, id := range g.Repositories {
		out[i] = gitv1.GrantRepository{ID: id, Read: true, Update: []string{}}
	}
	return out
}

// Validate checks the context structure and its cross-field rules: the key
// matches the kind, exactly one of preparation and service is set, a service
// is valid by the v2 rules, and the preparation names one checkout per source of
// a valid source set and a read-only Git delivery that covers exactly them.
func (c ResourceContextV3) Validate() error {
	if c.ContextVersion != ContextVersion {
		return fail("contextVersion", "must be %d", ContextVersion)
	}
	if err := firstError(
		wrap("tenantId", c.TenantID.Validate()), wrap("projectId", c.ProjectID.Validate()), wrap("runId", c.RunID.Validate()),
		func() error {
			if !safeNameRE.MatchString(c.Policy.ID) {
				return fail("policy.id", "must match SafeName syntax")
			}
			return wrap("policy.digest", c.Policy.Digest.Validate())
		}(),
	); err != nil {
		return err
	}
	switch c.ResourceKind {
	case ResourcePreparation:
		if c.ResourceKey != string(ResourcePreparation) {
			return fail("resourceKey", "a preparation resource has the key preparation")
		}
		if c.Preparation == nil || c.Service != nil {
			return fail("preparation", "exactly the preparation member is set for a preparation resource")
		}
		return c.Preparation.validate()
	case ResourceService:
		if c.Service == nil || c.Preparation != nil {
			return fail("service", "exactly the service member is set for a service resource")
		}
		// The service message is the v2 message; its validator (name, role,
		// command, secret bindings, deadline, key) is applied unchanged through
		// the v2 context it is a member of.
		v2 := runtimev2.ResourceContext{
			ContextVersion: runtimev2.ContextVersion, ResourceKind: runtimev2.ResourceService, TenantID: c.TenantID, ProjectID: c.ProjectID,
			RunID: c.RunID, ResourceKey: c.ResourceKey, Service: c.Service, Policy: c.Policy,
		}
		return v2.Validate()
	}
	return fail("resourceKind", "unknown kind %q", c.ResourceKind)
}

func (p *PreparationContextV3) validate() error {
	if p.MountPath != MountPath {
		return fail("preparation.mountPath", "must be %s", MountPath)
	}
	if p.Sources == nil || len(p.Sources) == 0 {
		return fail("preparation.sources", "a preparation checks out at least one source")
	}
	set := gitv1.SourceSet{Sources: make([]gitv1.Source, len(p.Sources))}
	for i, s := range p.Sources {
		if err := firstError(checkID(fmt.Sprintf("preparation.sources[%d].repositoryId", i), s.RepositoryID),
			func() error {
				if len(s.RepositoryURL) > maxBaseURLBytes || !repositoryURLRE.MatchString(s.RepositoryURL) {
					return fail(fmt.Sprintf("preparation.sources[%d].repositoryUrl", i), "must be an https URL without credentials")
				}
				return nil
			}()); err != nil {
			return err
		}
		set.Sources[i] = gitv1.Source{RepositoryID: s.RepositoryID, Commit: string(s.Commit), Path: s.Path, Access: gitv1.AccessRead}
	}
	if err := set.Validate(); err != nil {
		return wrap("preparation.sources", err)
	}
	if err := firstError(
		inRangeV3("preparation.timeoutSeconds", p.TimeoutSeconds, 1, maxPrepareTimeout),
		inRangeV3("preparation.maxBytes", p.MaxBytes, 1, maxByteCount),
		inRangeV3("preparation.maxAttempts", p.MaxAttempts, 1, maxPrepareTries),
		wrap("preparation.deadlineAt", p.DeadlineAt.Validate()),
	); err != nil {
		return err
	}
	return p.Git.validate(p.Sources)
}

func inRangeV3(path string, v, lo, hi int64) error {
	if v < lo || v > hi {
		return fail(path, "must be between %d and %d", lo, hi)
	}
	return nil
}

func (g PreparationGit) validate(sources []PreparationSource) error {
	if err := firstError(
		checkID("preparation.git.grantId", g.GrantID),
		func() error {
			if len(g.BaseURL) > maxBaseURLBytes || !repositoryURLRE.MatchString(g.BaseURL) || len(g.BaseURL) > 0 && g.BaseURL[len(g.BaseURL)-1] == '/' {
				return fail("preparation.git.baseUrl", "must be an https URL without credentials, query, fragment or trailing slash")
			}
			return nil
		}(),
		func() error {
			if !gitFileRE.MatchString(g.CredentialFile) {
				return fail("preparation.git.credentialFile", "must be a file directly under %s", GitDirectory)
			}
			return nil
		}(),
	); err != nil {
		return err
	}
	if g.CAFile != nil {
		if !gitFileRE.MatchString(*g.CAFile) {
			return fail("preparation.git.caFile", "must be null or a file directly under %s", GitDirectory)
		}
		if *g.CAFile == g.CredentialFile {
			return fail("preparation.git.caFile", "must not be the credential file")
		}
	}
	var want []string
	seen := map[string]bool{}
	for i, s := range sources {
		if len(s.RepositoryURL) <= len(g.BaseURL) || s.RepositoryURL[:len(g.BaseURL)+1] != g.BaseURL+"/" {
			return fail(fmt.Sprintf("preparation.sources[%d].repositoryUrl", i), "must lie beneath git.baseUrl, so the credential is never offered to another host")
		}
		if !seen[s.RepositoryID] {
			seen[s.RepositoryID] = true
			want = append(want, s.RepositoryID)
		}
	}
	if g.Repositories == nil || len(g.Repositories) != len(want) {
		return fail("preparation.git.repositories", "must list exactly the repositories of the sources, once each, in order of first appearance")
	}
	for i, id := range g.Repositories {
		if id != want[i] {
			return fail(fmt.Sprintf("preparation.git.repositories[%d]", i), "must be %s", want[i])
		}
	}
	return nil
}

// Validate checks the token and the context structurally, then the envelope
// consistency: the token's maximum lifetime never exceeds the resource's own
// deadlineAt.
func (r ResourceAdmitResponseV3) Validate() error {
	if err := firstError(r.Token.Validate(), r.Context.Validate()); err != nil {
		return err
	}
	var deadline Timestamp
	switch {
	case r.Context.ResourceKind == ResourcePreparation && r.Context.Preparation != nil:
		deadline = r.Context.Preparation.DeadlineAt
	case r.Context.ResourceKind == ResourceService && r.Context.Service != nil:
		deadline = r.Context.Service.DeadlineAt
	default:
		return fail("context", "the resource kind has no deadline member")
	}
	maximum, _ := r.Token.MaxExpiresAt.Time()
	limit, err := deadline.Time()
	if err != nil {
		return fail("context.deadlineAt", "not a valid time")
	}
	if maximum.After(limit) {
		return fail("token.maxExpiresAt", "must not be later than the resource's deadlineAt")
	}
	return nil
}

// Validate checks the receipt request: one receipt per source with valid
// objects, and the three safety fields with their literal values. Whether the
// sources are the context's and the try count is within maxAttempts depends on
// the context (CheckReceipt).
func (r PreparationReceiptRequestV3) Validate() error {
	if r.Sources == nil || len(r.Sources) < 1 || len(r.Sources) > gitv1.MaxSources {
		return fail("sources", "must be an array of 1 to %d receipts", gitv1.MaxSources)
	}
	for i, s := range r.Sources {
		p := fmt.Sprintf("sources[%d]", i)
		if err := firstError(wrap(p+".path", gitv1.ValidateSourcePath(s.Path)), wrap(p+".commit", s.Commit.Validate()), wrap(p+".treeId", s.TreeID.Validate())); err != nil {
			return err
		}
	}
	if !r.HooksDisabled {
		return fail("hooksDisabled", "must be true")
	}
	if r.Submodules != SubmodulesSkipped {
		return fail("submodules", "must be %s", SubmodulesSkipped)
	}
	if !r.CredentialsRemoved {
		return fail("credentialsRemoved", "must be true")
	}
	return inRangeV3("attempts", r.Attempts, 1, maxPrepareTries)
}

// CheckReceipt verifies a receipt request against the resource's context: it must
// be a preparation resource, observe exactly the context's sources in order at
// their pinned commits, and the internal try count at most maxAttempts.
func CheckReceipt(ctx ResourceContextV3, req PreparationReceiptRequestV3) error {
	if err := req.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	if ctx.ResourceKind != ResourcePreparation || ctx.Preparation == nil {
		return &ContractError{Code: domain.CodeForbidden, Message: "only a preparation resource records a receipt"}
	}
	p := ctx.Preparation
	if len(req.Sources) != len(p.Sources) {
		return invalidRequest("the receipt must cover exactly the sources of the context")
	}
	for i, s := range req.Sources {
		if s.Path != p.Sources[i].Path {
			return invalidRequest("the receipt must cover the sources of the context in order")
		}
		if s.Commit != p.Sources[i].Commit {
			return invalidRequest("a receipt commit is not the run's pinned input commit")
		}
	}
	if req.Attempts > p.MaxAttempts {
		return invalidRequest("the receipt reports more tries than the policy allows")
	}
	return nil
}

// CheckPreparationMatches verifies that a preparation resource context and an
// attempt context of the same run describe the same checkout: the same tenant,
// project and run, every source at the same repository, URL, commit and path, the
// same Git service, and the same repositories. The pinned set is one thing; the
// preparation and the attempts are two views of it.
func CheckPreparationMatches(prep ResourceContextV3, attempt ContextV3) error {
	if prep.ResourceKind != ResourcePreparation || prep.Preparation == nil {
		return invalidRequest("not a preparation resource")
	}
	if prep.TenantID != attempt.TenantID || prep.ProjectID != attempt.ProjectID || prep.RunID != attempt.RunID {
		return &ContractError{Code: domain.CodeForbidden, Message: "the contexts belong to different runs"}
	}
	want := attempt.Workspace.SourcePreparation()
	got := prep.Preparation.Sources
	if len(got) != len(want) {
		return invalidRequest("the preparation must check out exactly the sources of the run")
	}
	for i := range want {
		if got[i] != want[i] {
			return invalidRequest("a preparation source differs from the run's source set")
		}
	}
	if attempt.Git == nil || prep.Preparation.Git.BaseURL != attempt.Git.BaseURL {
		return invalidRequest("the preparation and the attempts use different Git services")
	}
	return nil
}
