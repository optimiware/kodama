package api

import (
	"context"
	"errors"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gitauth "github.com/optimiware/kodama/internal/shell/git"
	"github.com/optimiware/kodama/internal/shell/store"
)

// GitClaims is the record of a Git credential as stored in
// git_credentials.claims (docs/contracts/git-service.md, "Git credentials and
// grants"). It is what was issued, a ceiling: current roles, run state and the
// repository's policy are re-read at every use and only ever restrict it.
type GitClaims struct {
	GrantID      string              `json:"grantId"`
	Audience     string              `json:"audience"`
	TenantID     string              `json:"tenantId"`
	ProjectID    string              `json:"projectId,omitempty"`
	Actor        GitActor            `json:"actor"`
	Kind         string              `json:"kind"`
	Repositories []GitGrantRepoClaim `json:"repositories"`
	IssuedAt     string              `json:"issuedAt"`
	ExpiresAt    string              `json:"expiresAt"`
}

// GitActor is the principal a credential was issued to. RunID is set for a
// workload only.
type GitActor struct {
	Type  string  `json:"type"`
	ID    string  `json:"id"`
	RunID *string `json:"runId,omitempty"`
}

// GitGrantRepoClaim names one repository of a credential.
type GitGrantRepoClaim struct {
	ID          string          `json:"id"`
	Read        bool            `json:"read"`
	Update      []string        `json:"update"`
	Seed        bool            `json:"seed"`
	Integration *GitIntegration `json:"integration"`
}

// GitIntegration is the one update an integration credential permits, and the
// integration step of the run it was issued to. It is a ceiling: the shell's
// integration authority (internal/shell/git) decides again at every receive
// from the run's durable record, and a credential whose step has settled, whose
// waits do not approve the pushed commit or whose run is cancelled authorizes
// nothing.
type GitIntegration struct {
	Step string `json:"step"`
	Ref  string `json:"ref"`
	From string `json:"from"`
	To   string `json:"to"`
}

// humanCeiling is the recorded update set of a human credential that may push
// (the roles at use time decide whether it does).
func humanCeiling() []string { return []string{gitv1.HeadsPrefix + "*", gitv1.TagsPrefix + "*"} }

func (c GitClaims) repo(id string) (GitGrantRepoClaim, bool) {
	for _, r := range c.Repositories {
		if r.ID == id {
			return r, true
		}
	}
	return GitGrantRepoClaim{}, false
}

// mayUpdateARef reports whether the credential's own record can authorize any
// ref update (the forwarding assertion's receivePack is limited to that).
func (c GitClaims) mayUpdateARef(repositoryID string) bool {
	r, ok := c.repo(repositoryID)
	return ok && (len(r.Update) > 0 || r.Seed || r.Integration != nil)
}

// matchRefPattern matches a full ref name or a pattern with a trailing /*,
// whose wildcard stands for one or more further characters.
func matchRefPattern(pattern, ref string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasSuffix(prefix, "/") && strings.HasPrefix(ref, prefix) && len(ref) > len(prefix)
	}
	return pattern == ref
}

// refClass is the policy class of a ref (docs/specs/git-repositories.md, "Access
// and ref policy"). The kernel's run and agent branches are
// refs/heads/kodama/runs/<runId>/… and refs/heads/kodama/agents/<runId>/….
type refClass int

const (
	classBranch refClass = iota // any other branch
	classDefault
	classTask
	classRun // a run's or an agent's branch
	classTag
	classServer
	classOther // notes, changes and the like
)

func (c refClass) String() string {
	return [...]string{"branch", "default", "task", "run", "tag", "server", "other"}[c]
}

const (
	taskBranchPrefix  = gitv1.HeadsPrefix + "kodama/work/"
	runBranchPrefix   = gitv1.HeadsPrefix + "kodama/runs/"
	agentBranchPrefix = gitv1.HeadsPrefix + "kodama/agents/"
)

// classifyRef returns the class of ref and, for a task or run class, the task or
// run ID the ref names.
func classifyRef(defaultBranch, ref string) (refClass, string) {
	switch {
	case ref == gitv1.HeadsPrefix+defaultBranch:
		return classDefault, ""
	case strings.HasPrefix(ref, gitv1.ServerRefPrefix):
		return classServer, ""
	case strings.HasPrefix(ref, gitv1.TagsPrefix):
		return classTag, ""
	case !strings.HasPrefix(ref, gitv1.HeadsPrefix):
		return classOther, ""
	}
	if rest, ok := strings.CutPrefix(ref, taskBranchPrefix); ok && domain.TaskID(rest).Validate() == nil {
		return classTask, rest
	}
	for _, prefix := range []string{runBranchPrefix, agentBranchPrefix} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			if id, label, ok := strings.Cut(rest, "/"); ok && label != "" && domain.RunID(id).Validate() == nil {
				return classRun, id
			}
		}
	}
	return classBranch, ""
}

// pushFacts is everything the ref policy reads, all of it current: the
// repository row, the credential, the person's roles now and the run's state now.
type pushFacts struct {
	Repo     store.Repository
	Cred     store.Credential
	Claims   GitClaims
	Engineer bool              // a human or seed credential's user holds engineer now
	Run      *store.RunGitInfo // the run of a run or integration credential, if it exists
	// Authority rechecks integration authority at use (integration only). Nil
	// refuses every integration update: authority is never assumed.
	Authority *authorityCheck
}

// authorityCheck is the use-time recheck of one integration authority. A
// refusal denies the update with its reason; a failure to read the evidence is
// kept in err and fails the whole receive, never an approval.
type authorityCheck struct {
	recheck func(auth gitauth.Authority, u gitv1.RefUpdate, fact gitv1.GitFact) error
	err     error
}

// reason returns "" when the authority holds for u and the refusal reason
// otherwise.
func (a *authorityCheck) reason(auth gitauth.Authority, u gitv1.RefUpdate, fact gitv1.GitFact) string {
	if a == nil || a.recheck == nil {
		return gitv1.ReasonIntegrationMismatch
	}
	err := a.recheck(auth, u, fact)
	var refusal *gitauth.Refusal
	switch {
	case err == nil:
		return ""
	case errors.As(err, &refusal):
		return refusal.PushReason()
	}
	if a.err == nil {
		a.err = err
	}
	return gitv1.ReasonIntegrationMismatch
}

// decidePush returns one verdict per update, in order. Any deny fails the whole
// receive at the worker. The order of rules is the specification's: archived
// repository, inactive run, seed rules, server refs, then the ref class rules.
func decidePush(f pushFacts, req gitv1.PushAuthorizeRequest) []gitv1.UpdateVerdict {
	out := make([]gitv1.UpdateVerdict, len(req.Updates))
	denyAll := func(reason string) []gitv1.UpdateVerdict {
		for i, u := range req.Updates {
			r := reason
			out[i] = gitv1.UpdateVerdict{Ref: u.Ref, Verdict: gitv1.VerdictDeny, Reason: &r}
		}
		return out
	}
	switch {
	case f.Repo.Status == store.RepoArchived:
		return denyAll(gitv1.ReasonRepositoryArchived)
	case f.Repo.Status != store.RepoReady:
		return denyAll(gitv1.ReasonGrantScope)
	}
	run := f.Cred.Kind == store.CredRun || f.Cred.Kind == store.CredIntegration
	if run && (f.Run == nil || !f.Run.Active()) {
		return denyAll(gitv1.ReasonRunInactive)
	}
	if f.Cred.Kind == store.CredSeed {
		// A seed authorizes one receive: any other receive with the grant is
		// consumed, whatever it carries.
		if f.Cred.ConsumedAt != nil && f.Cred.ConsumedReceive != req.ReceiveID {
			return denyAll(gitv1.ReasonSeedConsumed)
		}
		if reason := decideSeed(f, req); reason != "" {
			return denyAll(reason)
		}
		for i, u := range req.Updates {
			out[i] = gitv1.UpdateVerdict{Ref: u.Ref, Verdict: gitv1.VerdictAllow}
		}
		return out
	}
	for i, u := range req.Updates {
		var fact gitv1.GitFact
		if i < len(req.GitFacts) {
			fact = req.GitFacts[i]
		}
		if reason := decideUpdate(f, u, fact); reason != "" {
			r := reason
			out[i] = gitv1.UpdateVerdict{Ref: u.Ref, Verdict: gitv1.VerdictDeny, Reason: &r}
			continue
		}
		out[i] = gitv1.UpdateVerdict{Ref: u.Ref, Verdict: gitv1.VerdictAllow}
	}
	return out
}

// decideSeed returns "" when the receive is a valid seed: exactly one update,
// creating the default branch of a repository with no refs, by a user who is an
// engineer now. Anything else refuses the whole receive.
func decideSeed(f pushFacts, req gitv1.PushAuthorizeRequest) string {
	if !f.Engineer {
		return gitv1.ReasonGrantScope
	}
	want := gitv1.HeadsPrefix + f.Repo.DefaultBranch
	r, ok := f.Claims.repo(f.Repo.ID)
	if !ok || !r.Seed || len(r.Update) != 1 || r.Update[0] != want {
		return gitv1.ReasonSeedOnly
	}
	if len(req.Updates) != 1 || len(req.GitFacts) != 1 {
		return gitv1.ReasonSeedOnly
	}
	u := req.Updates[0]
	if u.Ref != want || !gitv1.IsZeroObjectID(u.Old) || gitv1.IsZeroObjectID(u.New) || !req.GitFacts[0].RepositoryEmpty {
		return gitv1.ReasonSeedOnly
	}
	return ""
}

// decideUpdate returns "" to allow one update of a non-seed credential, or the
// refusal reason.
func decideUpdate(f pushFacts, u gitv1.RefUpdate, fact gitv1.GitFact) string {
	class, id := classifyRef(f.Repo.DefaultBranch, u.Ref)
	switch class {
	case classServer, classOther:
		return gitv1.ReasonRefProtected // refs/kodama/* is the server's, hidden from fetch
	}
	if f.Repo.Name == ContextRepository && (f.Cred.Kind == store.CredRun || f.Cred.Kind == store.CredIntegration) {
		return gitv1.ReasonRefProtected // a run reads its knowledge base and never updates it
	}
	if class == classDefault && f.Cred.Kind == store.CredRun {
		return gitv1.ReasonRefProtected // a run reaches the default branch only through integration
	}
	repo, ok := f.Claims.repo(f.Repo.ID)
	if !ok {
		return gitv1.ReasonGrantScope
	}
	inScope := false
	for _, p := range repo.Update {
		if matchRefPattern(p, u.Ref) {
			inScope = true
			break
		}
	}
	if !inScope {
		return gitv1.ReasonGrantScope
	}
	switch f.Cred.Kind {
	case store.CredHuman:
		if !f.Engineer {
			return gitv1.ReasonGrantScope
		}
		if class == classDefault {
			return directDefault(f, u, fact)
		}
		return ""
	case store.CredRun:
		switch class {
		case classDefault:
			return gitv1.ReasonRefProtected
		case classTag:
			return gitv1.ReasonGrantScope
		case classTask:
			if f.Run.TaskID != id {
				return gitv1.ReasonGrantScope
			}
		case classRun:
			if f.Run.RunID != id {
				return gitv1.ReasonGrantScope
			}
		}
		return "" // another branch: the run's source granted the pattern
	case store.CredIntegration:
		if class != classDefault {
			return gitv1.ReasonGrantScope
		}
		return integrationUpdate(f, repo, u, fact)
	}
	return gitv1.ReasonGrantScope
}

// directDefault is an engineer's push to the default branch: only where the
// repository opted in, only as a fast-forward, never a create (the seed's) or a
// delete.
func directDefault(f pushFacts, u gitv1.RefUpdate, fact gitv1.GitFact) string {
	if !f.Repo.Policy.DirectPushToDefault || fact.RepositoryEmpty || gitv1.IsZeroObjectID(u.Old) || gitv1.IsZeroObjectID(u.New) {
		return gitv1.ReasonRefProtected
	}
	if fact.FastForward == nil || !*fact.FastForward {
		return gitv1.ReasonNotFastForward
	}
	return ""
}

// integrationUpdate is a run's push to the default branch. The credential must
// name exactly this update, the run's pipeline must be the repository's
// configured integration pipeline, and the shell's integration authority
// (internal/shell/git) must hold now: the credential's step is running, every
// wait it requires approved exactly this commit, the ref is the protected
// default branch, and the update is a fast-forward of the ref. No task field,
// resolution row or event is read. There is no gate subsystem to consult (O-36).
func integrationUpdate(f pushFacts, repo GitGrantRepoClaim, u gitv1.RefUpdate, fact gitv1.GitFact) string {
	pipeline := f.Repo.Policy.IntegrationPipeline
	if pipeline == "" || f.Run == nil || f.Run.PipelineName != pipeline || repo.Integration == nil {
		return gitv1.ReasonIntegrationMismatch
	}
	c := repo.Integration
	if c.Step == "" || u.Ref != c.Ref || u.Ref != gitv1.HeadsPrefix+f.Repo.DefaultBranch || u.Old != c.From || u.New != c.To {
		return gitv1.ReasonIntegrationMismatch
	}
	auth := gitauth.Authority{Scope: domain.Scope{TenantID: f.Cred.TenantID, ProjectID: f.Cred.ProjectID}, Repository: f.Repo.ID,
		Run: domain.RunID(f.Run.RunID), Step: domain.StepID(c.Step), Ref: c.Ref, Old: c.From, New: c.To}
	return f.Authority.reason(auth, u, fact)
}

// ---- integration authority adapters -----------------------------------------

// authorityPolicy reads a repository's integration policy from the store.
type authorityPolicy struct{ store store.GitBackend }

func (p authorityPolicy) IntegrationPolicy(ctx context.Context, scope domain.Scope, repositoryID string) (gitauth.Policy, error) {
	repo, err := p.store.GetRepository(ctx, scope.TenantID, repositoryID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && repo.ProjectID != scope.ProjectID) {
		return gitauth.Policy{}, &domain.Error{Code: domain.CodeNotFound, Message: "repository"}
	}
	if err != nil {
		return gitauth.Policy{}, err
	}
	return gitauth.Policy{DefaultBranch: repo.DefaultBranch, IntegrationPipeline: repo.Policy.IntegrationPipeline}, nil
}

// authorityPipelines names the pipeline of a run from the run's row.
type authorityPipelines struct{ store store.GitBackend }

func (p authorityPipelines) PipelineName(ctx context.Context, scope domain.Scope, run domain.RunID, _ domain.PipelineID) (string, error) {
	info, err := p.store.RunGitInfo(ctx, scope.TenantID, scope.ProjectID, string(run))
	if errors.Is(err, store.ErrNotFound) {
		return "", &domain.Error{Code: domain.CodeNotFound, Message: "run"}
	}
	return info.PipelineName, err
}

// authorityRefs asks the tenant's worker for the ref and the fast-forward fact.
type authorityRefs struct{ workers WorkerRPC }

func (r authorityRefs) Observe(ctx context.Context, scope domain.Scope, repositoryID, ref, candidate string) (gitauth.Observation, error) {
	var cursor *string
	current := ""
	for {
		page, err := r.workers.ReadRefs(ctx, scope.TenantID, gitv1.ReadRefsRequest{RepositoryID: repositoryID, Prefix: ref, Cursor: cursor, Limit: gitv1.MaxPage})
		if err != nil {
			return gitauth.Observation{}, err
		}
		for _, tip := range page.Refs {
			if tip.Ref == ref {
				current = tip.Commit
			}
		}
		if current != "" || page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if current == "" {
		return gitauth.Observation{}, nil
	}
	res, err := r.workers.IsAncestor(ctx, scope.TenantID, gitv1.IsAncestorRequest{RepositoryID: repositoryID, Ancestor: current, Descendant: candidate})
	if err != nil {
		var contract *gitv1.ContractError
		if errors.As(err, &contract) && contract.Code == gitv1.CodeCommitNotFound {
			return gitauth.Observation{Current: current}, nil // the candidate is unknown: not a fast-forward
		}
		return gitauth.Observation{}, err
	}
	return gitauth.Observation{Current: current, FastForward: res.IsAncestor}, nil
}
