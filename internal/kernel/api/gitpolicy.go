package api

import (
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/store"
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

// GitIntegration is the one update an integration credential permits.
type GitIntegration struct {
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
	// TaskApproved is whether the run's task is approved now (integration only).
	TaskApproved bool
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

// integrationUpdate is a run's push to the default branch: its pipeline must be
// the repository's configured integration pipeline and the update exactly the
// credential's from to to, as a fast-forward, and its task must be approved at
// this receive (a promotion request resolved approved; see Store.TaskApproved).
// There is no gate subsystem to consult (O-36).
func integrationUpdate(f pushFacts, repo GitGrantRepoClaim, u gitv1.RefUpdate, fact gitv1.GitFact) string {
	pipeline := f.Repo.Policy.IntegrationPipeline
	if pipeline == "" || f.Run == nil || f.Run.PipelineName != pipeline || repo.Integration == nil || !f.TaskApproved {
		return gitv1.ReasonIntegrationMismatch
	}
	c := repo.Integration
	if u.Ref != c.Ref || u.Ref != gitv1.HeadsPrefix+f.Repo.DefaultBranch || u.Old != c.From || u.New != c.To {
		return gitv1.ReasonIntegrationMismatch
	}
	if fact.FastForward == nil || !*fact.FastForward {
		return gitv1.ReasonNotFastForward
	}
	return ""
}
