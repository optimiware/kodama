package runtimev3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
)

// Patterns are the string syntaxes new in v3; the shared ones are
// runtimev1.Patterns, runtimev2.Patterns and gitv1.Patterns. A test requires
// schema.json's definitions to equal them.
var Patterns = map[string]string{
	// CredentialFile is one file directly under GitDirectory.
	"GitFile": `^/kodama/git/[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`,
}

var (
	gitFileRE       = regexp.MustCompile(Patterns["GitFile"])
	repositoryURLRE = regexp.MustCompile(gitv1.Patterns["RepositoryURL"])
	idRE            = regexp.MustCompile(domain.IDPattern)
	credentialRE    = regexp.MustCompile(gitv1.Patterns["Credential"])
	stepNameRE      = regexp.MustCompile(runtimev1.Patterns["StepName"])
)

// Bounds fixed by the Git contract or chosen here as engineering bounds (marked).
const (
	maxLineage       = 8192 // engineering bound, as in v2
	maxCommitOutputs = 1024 // engineering bound: one entry per repository and ref of a result, at most the Git contract's ceiling on one push
	maxBaseURLBytes  = 512  // engineering bound
)

func fail(path, format string, args ...any) error {
	return &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
}

func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	return fail(path, "%v", err)
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func checkID(path, v string) error {
	if !idRE.MatchString(v) {
		return fail(path, "must match the opaque ID syntax")
	}
	return nil
}

func checkLiveCommit(path string, c GitCommit) error {
	if err := c.Validate(); err != nil {
		return fail(path, "%v", err)
	}
	if gitv1.IsZeroObjectID(string(c)) {
		return fail(path, "must not be the all-zero ID")
	}
	return nil
}

func validAccess(a WorkspaceAccess) bool {
	return a == AccessNone || a == AccessRead || a == AccessWrite
}

func validState(s WorkspaceState) bool {
	return s == StateNone || s == StateClean || s == StateModified
}

// ---- origin ----

func (k OriginKind) valid() bool {
	return k == OriginWorkItem || k == OriginPush || k == OriginManual || k == OriginProvision
}

// Validate checks the member rules of the origin's kind. The relation to the
// context's work item and source set is ContextV3.Validate's.
func (o Origin) Validate() error {
	if !o.Kind.valid() {
		return fail("origin.kind", "unknown origin kind %q", o.Kind)
	}
	nonNull := func(path string, present bool, want bool) error {
		if present != want {
			if want {
				return fail(path, "is required for origin %s", o.Kind)
			}
			return fail(path, "must be null for origin %s", o.Kind)
		}
		return nil
	}
	isWI, isPush := o.Kind == OriginWorkItem, o.Kind == OriginPush
	if err := firstError(
		nonNull("origin.workItemId", o.WorkItemID != nil, isWI),
		nonNull("origin.pushId", o.PushID != nil, isPush),
		nonNull("origin.repositoryId", o.RepositoryID != nil, isPush),
		nonNull("origin.ref", o.Ref != nil, isPush),
	); err != nil {
		return err
	}
	if isWI {
		return wrap("origin.workItemId", o.WorkItemID.Validate())
	}
	if isPush {
		if err := firstError(checkID("origin.pushId", *o.PushID), checkID("origin.repositoryId", *o.RepositoryID), wrap("origin.ref", gitv1.ValidateRefName(*o.Ref))); err != nil {
			return err
		}
		if strings.HasPrefix(*o.Ref, gitv1.ServerRefPrefix) {
			return fail("origin.ref", "a server ref is never the ref of a push")
		}
	}
	return nil
}

// ---- context ----

// AgentPattern returns the ref pattern of the run's agent-branch namespace in
// every repository: refs/heads/kodama/agents/<workItemId>/<runId>/* for a
// work-item run and refs/heads/kodama/runs/<runId>/* for every other origin. It
// is the namespace of the grant of every executable attempt of the run.
func (c ContextV3) AgentPattern() (string, error) {
	if c.Origin.Kind == OriginWorkItem && c.Origin.WorkItemID != nil {
		return gitv1.AgentBranchPattern(string(*c.Origin.WorkItemID), string(c.RunID))
	}
	return gitv1.RunBranchPattern(string(c.RunID))
}

// SourceSet projects the workspace's sources to the Git contract's source set,
// ordered and validated, or reports why they are not one. A source-free
// workspace has none.
func (w WorkspaceV3) SourceSet() (gitv1.SourceSet, error) {
	sources := make([]gitv1.Source, len(w.Sources))
	for i, s := range w.Sources {
		sources[i] = s.Source
	}
	set := gitv1.SourceSet{Sources: sources}
	if err := set.Validate(); err != nil {
		return gitv1.SourceSet{}, err
	}
	return set, nil
}

// GitAccess returns what the attempt's Git grant must allow in each repository
// of the source set, in order of first appearance: the run's agent-branch
// namespace and, only when the attempt holds the writer reservation (workspace
// access write), the exact sorted unique branches of the repository's write
// sources. It is what run admission records in the grant and puts in
// ContextV3.Git.Repositories; Validate requires the two to be equal.
func (c ContextV3) GitAccess() ([]GitRepositoryAccess, error) {
	agent, err := c.AgentPattern()
	if err != nil {
		return nil, err
	}
	out := []GitRepositoryAccess{}
	index := map[string]int{}
	for _, s := range c.Workspace.Sources {
		i, ok := index[s.RepositoryID]
		if !ok {
			i = len(out)
			index[s.RepositoryID] = i
			out = append(out, GitRepositoryAccess{RepositoryID: s.RepositoryID, Update: []string{agent}})
		}
		if c.Workspace.Access == AccessWrite && s.Access == gitv1.AccessWrite && s.Branch != nil {
			out[i].Update = append(out[i].Update, *s.Branch)
		}
	}
	for i := range out {
		tail := out[i].Update[1:]
		sort.Strings(tail)
		out[i].Update = append(out[i].Update[:1], dedupe(tail)...)
	}
	return out, nil
}

func dedupe(sorted []string) []string {
	out := sorted[:0:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// GrantRepositories converts the delivery's access into the repository claims
// of the attempt's Git grant: read access everywhere, the recorded update
// patterns, no seed and no integration constraint.
func (g GitDelivery) GrantRepositories() []gitv1.GrantRepository {
	out := make([]gitv1.GrantRepository, len(g.Repositories))
	for i, r := range g.Repositories {
		out[i] = gitv1.GrantRepository{ID: r.RepositoryID, Read: true, Update: append([]string{}, r.Update...)}
	}
	return out
}

// Validate checks the structure and the cross-field rules of the context.
func (c ContextV3) Validate() error {
	if c.ContextVersion != ContextVersion {
		return fail("contextVersion", "must be %d", ContextVersion)
	}
	if c.RunModel != RunModel {
		return fail("runModel", "must be %s", RunModel)
	}
	// Every field v3 shares with v2 (and through it with v1) has its meaning and
	// validator. They are applied through a projection whose workspace is the
	// trivially valid source-free one; the v3 workspace is validated below.
	shared := runtimev2.ContextV2{
		ContextVersion: runtimev2.ContextVersion, RunModel: runtimev2.RunModel,
		TenantID: c.TenantID, ProjectID: c.ProjectID, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Definition: c.Definition, WorkItem: c.WorkItem, DesignBaseline: c.DesignBaseline, Profile: c.Profile, Command: c.Command,
		Workspace: runtimev2.WorkspaceV2{Access: runtimev2.AccessNone, MountPath: runtimev2.MountPath, Lineage: []runtimev2.LineageEntry{}},
		Endpoints: c.Endpoints, Egress: c.Egress, Run: c.Run, Policy: c.Policy,
		Inputs: c.Inputs, Outputs: c.Outputs, Secrets: c.Secrets, Environment: c.Environment, Limits: c.Limits,
	}
	// The checks run in order and each assumes the earlier ones passed.
	for _, check := range []func() error{
		shared.Validate, c.Origin.Validate, c.validateOriginRelations, c.validateWorkspace, func() error { return c.Git.validate(c) },
	} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (c ContextV3) validateOriginRelations() error {
	switch c.Origin.Kind {
	case OriginWorkItem:
		if c.WorkItem == nil {
			return fail("workItem", "a work_item origin pins its work item")
		}
		if c.WorkItem.ID != *c.Origin.WorkItemID {
			return fail("workItem.id", "must be the origin's work item")
		}
	default:
		if c.WorkItem != nil {
			return fail("workItem", "only a work_item origin has a work item")
		}
	}
	if c.Origin.Kind == OriginPush {
		for _, s := range c.Workspace.Sources {
			if s.RepositoryID == *c.Origin.RepositoryID {
				return nil
			}
		}
		return fail("origin.repositoryId", "a push-originated run pins a source of the pushed repository")
	}
	return nil
}

func (c ContextV3) validateWorkspace() error {
	w := c.Workspace
	if w.MountPath != MountPath {
		return fail("workspace.mountPath", "must be %s", MountPath)
	}
	if !validAccess(w.Access) {
		return fail("workspace.access", "unknown access %q", w.Access)
	}
	if w.Sources == nil || w.Lineage == nil || len(w.Lineage) > maxLineage {
		return fail("workspace", "sources must be an array and lineage an array of at most %d entries", maxLineage)
	}
	sourceFree := len(w.Sources) == 0
	if sourceFree {
		if w.SourceSetDigest != nil || w.Preparation != nil {
			return fail("workspace", "sourceSetDigest and preparation must be null for a source-free run")
		}
		if w.Access != AccessNone {
			return fail("workspace.access", "a source-free run has access none")
		}
		return w.validateLineage(c, nil)
	}
	if w.Access == AccessNone {
		return fail("workspace.access", "a source-backed run has effective access read or write")
	}
	set, err := w.SourceSet()
	if err != nil {
		return wrap("workspace.sources", err)
	}
	if w.SourceSetDigest == nil {
		return fail("workspace.sourceSetDigest", "required for a source-backed run")
	}
	want, err := set.Digest()
	if err != nil {
		return wrap("workspace.sources", err)
	}
	if *w.SourceSetDigest != Digest(want) {
		return fail("workspace.sourceSetDigest", "must be the digest of the source set")
	}
	if err := c.validateSources(); err != nil {
		return err
	}
	if w.Preparation == nil {
		return fail("workspace.preparation", "required for a source-backed run")
	}
	if err := w.Preparation.validate(w.Sources); err != nil {
		return err
	}
	return w.validateLineage(c, w.Sources)
}

func (c ContextV3) validateSources() error {
	w := c.Workspace
	agent, err := c.AgentPattern()
	if err != nil {
		return wrap("origin", err)
	}
	var task string
	if c.Origin.Kind == OriginWorkItem {
		if task, err = gitv1.TaskBranch(string(*c.Origin.WorkItemID)); err != nil {
			return wrap("origin.workItemId", err)
		}
	}
	writable := false
	for i, s := range w.Sources {
		p := fmt.Sprintf("workspace.sources[%d]", i)
		if err := firstError(
			func() error {
				if len(s.RepositoryURL) > maxBaseURLBytes || !repositoryURLRE.MatchString(s.RepositoryURL) {
					return fail(p+".repositoryUrl", "must be an https URL without credentials")
				}
				return nil
			}(),
			wrap(p+".headCommit", s.HeadCommit.Validate()),
		); err != nil {
			return err
		}
		if s.Access == gitv1.AccessWrite {
			if task != "" && *s.Branch != task && !gitv1.MatchRefPattern(agent, *s.Branch) {
				return fail(p+".branch", "a write source updates the work item's task branch or a branch of the run's own namespace")
			}
			if task == "" && !gitv1.MatchRefPattern(agent, *s.Branch) {
				return fail(p+".branch", "a write source updates a branch of the run's own namespace")
			}
		}
		switch w.Access {
		case AccessRead:
			if s.ExpectedTip != nil {
				return fail(p+".expectedTip", "must be null for read access")
			}
		case AccessWrite:
			if s.Access == gitv1.AccessWrite {
				writable = true
				if s.ExpectedTip == nil {
					return fail(p+".expectedTip", "required for a write source of the writer")
				}
				if err := wrap(p+".expectedTip", s.ExpectedTip.Validate()); err != nil {
					return err
				}
			} else if s.ExpectedTip != nil {
				return fail(p+".expectedTip", "must be null for a read source")
			}
		}
	}
	if w.Access == AccessWrite && !writable {
		return fail("workspace.access", "a writer needs a write source")
	}
	return nil
}

func (p *PreparationV3) validate(sources []SourceV3) error {
	if err := wrap("workspace.preparation.receiptDigest", p.ReceiptDigest.Validate()); err != nil {
		return err
	}
	if len(p.Sources) != len(sources) {
		return fail("workspace.preparation.sources", "must have one receipt per source")
	}
	for i, r := range p.Sources {
		pp := fmt.Sprintf("workspace.preparation.sources[%d]", i)
		if r.Path != sources[i].Path {
			return fail(pp+".path", "must be the path of the source at the same position")
		}
		if err := firstError(wrap(pp+".commit", r.Commit.Validate()), wrap(pp+".treeId", r.TreeID.Validate())); err != nil {
			return err
		}
		if r.Commit != GitCommit(sources[i].Commit) {
			return fail(pp+".commit", "the receipt must be for the run's pinned input commit")
		}
	}
	return nil
}

// validateLineage checks the lineage entries and, for a source-backed run, that
// every source's head is the commit the latest writer published to its own
// branch, or the pinned input commit before any.
func (w WorkspaceV3) validateLineage(c ContextV3, sources []SourceV3) error {
	sourceFree := len(sources) == 0
	agent, _ := c.AgentPattern()
	heads := make([]GitCommit, len(sources))
	for i, s := range sources {
		heads[i] = GitCommit(s.Commit)
	}
	var lastTime string
	seen := map[domain.AttemptID]bool{}
	for i, e := range w.Lineage {
		p := fmt.Sprintf("workspace.lineage[%d]", i)
		if err := e.validate(p, sourceFree, sources, agent, c.Git); err != nil {
			return err
		}
		if e.AttemptID == c.AttemptID {
			return fail(p+".attemptId", "an attempt is not part of its own lineage")
		}
		if seen[e.AttemptID] {
			return fail(p+".attemptId", "duplicate attempt in the lineage")
		}
		seen[e.AttemptID] = true
		at, _ := e.BeganAt.Time()
		if lastTime != "" {
			if prev, _ := Timestamp(lastTime).Time(); at.Before(prev) {
				return fail(p+".beganAt", "lineage must be in beganAt order")
			}
		}
		lastTime = string(e.BeganAt)
		for _, out := range e.Published {
			for j, s := range sources {
				if s.Branch != nil && s.RepositoryID == out.RepositoryID && *s.Branch == out.Ref {
					heads[j] = out.Commit
				}
			}
		}
	}
	for i, s := range sources {
		if s.HeadCommit != heads[i] {
			return fail(fmt.Sprintf("workspace.sources[%d].headCommit", i), "must be the latest commit published to the source's branch in this run, or the input commit before any")
		}
	}
	return nil
}

func (e LineageEntryV3) validate(path string, sourceFree bool, sources []SourceV3, agent string, git *GitDelivery) error {
	if err := firstError(
		func() error {
			if !stepNameRE.MatchString(e.StepID) {
				return fail(path+".stepId", "must match StepName syntax")
			}
			return nil
		}(),
		wrap(path+".attemptId", e.AttemptID.Validate()),
		wrap(path+".beganAt", e.BeganAt.Validate()),
	); err != nil {
		return err
	}
	if !validAccess(e.Access) {
		return fail(path+".access", "unknown access %q", e.Access)
	}
	if !validState(e.WorkspaceState) {
		return fail(path+".workspaceState", "unknown state %q", e.WorkspaceState)
	}
	if e.Published == nil {
		return fail(path+".published", "must be an array")
	}
	if err := validateCommitOutputs(path+".published", e.Published); err != nil {
		return err
	}
	for i, out := range e.Published {
		if !publishable(out, e.Access, sources, agent, git) {
			return fail(fmt.Sprintf("%s.published[%d]", path, i), "must name a repository and a ref the attempt's grants allowed: the run's agent namespace, a write source's branch for a writer, or the repository's default branch for a runtime-issued seed or integration grant")
		}
	}
	if sourceFree {
		if e.Access != AccessNone || e.WorkspaceState != StateNone || e.IgnoredPresent {
			return fail(path, "a source-free entry has access none, state none and ignoredPresent false")
		}
		return nil
	}
	if e.Access == AccessNone {
		return fail(path+".access", "a source-backed attempt has effective access read or write")
	}
	if e.WorkspaceState == StateNone {
		return fail(path+".workspaceState", "state none is only for source-free attempts")
	}
	return nil
}

// publishable reports whether an output names a ref the attempt could have
// updated under the grants its context describes: a ref of the run's agent
// namespace in a source repository; the branch of a write source of that
// repository, for the attempt holding the writer reservation; or an ordinary
// branch (never under refs/heads/kodama/, so a default branch) under a
// runtime-issued grant: the seed grant of a provisioning run (any repository it
// created) or the integration grant for a repository with a write source.
func publishable(o CommitOutput, access WorkspaceAccess, sources []SourceV3, agent string, git *GitDelivery) bool {
	inSet, writes, writeBranch := false, false, false
	for _, s := range sources {
		if s.RepositoryID != o.RepositoryID {
			continue
		}
		inSet = true
		if s.Access == gitv1.AccessWrite {
			writes = true
			writeBranch = writeBranch || (s.Branch != nil && *s.Branch == o.Ref)
		}
	}
	switch {
	case inSet && gitv1.MatchRefPattern(agent, o.Ref):
		return true
	case writeBranch && access == AccessWrite:
		return true
	case strings.HasPrefix(o.Ref, gitv1.HeadsPrefix+"kodama/") || git == nil:
		return false
	}
	return git.Runtime.Seed != nil || (git.Runtime.Integration != nil && writes)
}

func (g *GitDelivery) validate(c ContextV3) error {
	needed := len(c.Workspace.Sources) > 0 || c.Origin.Kind == OriginProvision
	if (g != nil) != needed {
		if needed {
			return fail("git", "required for a source-backed or provisioning run")
		}
		return fail("git", "must be null for a source-free run that is not provisioning")
	}
	if g == nil {
		return nil
	}
	if len(g.BaseURL) > maxBaseURLBytes || !repositoryURLRE.MatchString(g.BaseURL) || strings.HasSuffix(g.BaseURL, "/") {
		return fail("git.baseUrl", "must be an https URL without credentials, query, fragment or trailing slash")
	}
	// Every file the delivery names is a distinct file directly under GitDirectory.
	files := map[string]string{}
	file := func(path string, f string) error {
		if !gitFileRE.MatchString(f) {
			return fail(path, "must be a file directly under %s", GitDirectory)
		}
		if other, dup := files[f]; dup {
			return fail(path, "must not be the same file as %s", other)
		}
		files[f] = path
		return nil
	}
	static := len(c.Workspace.Sources) > 0
	if (g.GrantID != nil) != static || (g.CredentialFile != nil) != static {
		if static {
			return fail("git.grantId", "grantId and credentialFile are required while the attempt holds a grant over a source set")
		}
		return fail("git.grantId", "grantId and credentialFile are null for a provisioning run without sources: no grant can name a repository that does not exist yet")
	}
	if static {
		if err := firstError(checkID("git.grantId", *g.GrantID), file("git.credentialFile", *g.CredentialFile)); err != nil {
			return err
		}
	}
	if g.CAFile != nil {
		if err := file("git.caFile", *g.CAFile); err != nil {
			return err
		}
	}
	if (g.Runtime.Seed != nil) != (c.Origin.Kind == OriginProvision) {
		return fail("git.runtimeGrants.seed", "is advertised exactly for a provisioning run, the only run that creates repositories it may seed")
	}
	writeSource := false
	for _, s := range c.Workspace.Sources {
		writeSource = writeSource || s.Access == gitv1.AccessWrite
	}
	if (g.Runtime.Integration != nil) != writeSource {
		return fail("git.runtimeGrants.integration", "is advertised exactly when the source set has a write source, the repository an approved candidate is integrated into")
	}
	if g.Runtime.Seed != nil {
		if err := file("git.runtimeGrants.seed.credentialFile", g.Runtime.Seed.CredentialFile); err != nil {
			return err
		}
	}
	if g.Runtime.Integration != nil {
		if err := file("git.runtimeGrants.integration.credentialFile", g.Runtime.Integration.CredentialFile); err != nil {
			return err
		}
	}
	for i, s := range c.Workspace.Sources {
		if !strings.HasPrefix(s.RepositoryURL, g.BaseURL+"/") {
			return fail(fmt.Sprintf("workspace.sources[%d].repositoryUrl", i), "must lie beneath git.baseUrl, so the credential is never offered to another host")
		}
	}
	if g.Repositories == nil {
		return fail("git.repositories", "must be an array")
	}
	want, err := c.GitAccess()
	if err != nil {
		return wrap("git.repositories", err)
	}
	if len(g.Repositories) != len(want) {
		return fail("git.repositories", "must list exactly the repositories of the source set, in order of first appearance")
	}
	for i, r := range g.Repositories {
		p := fmt.Sprintf("git.repositories[%d]", i)
		if r.RepositoryID != want[i].RepositoryID {
			return fail(p+".repositoryId", "must be the repository of the source set at this position")
		}
		if r.Update == nil || len(r.Update) != len(want[i].Update) {
			return fail(p+".update", "must be the agent namespace and, only for the writer, the write branches")
		}
		for j := range r.Update {
			if r.Update[j] != want[i].Update[j] {
				return fail(fmt.Sprintf("%s.update[%d]", p, j), "must be %s", want[i].Update[j])
			}
		}
	}
	return nil
}

// Validate checks the response as a client receives it.
func (r AdmitResponseV3) Validate() error {
	if err := r.Token.Validate(); err != nil {
		return err
	}
	return r.Context.Validate()
}

// ---- begin ----

// Validate checks the structure of one observation.
func (s BeginSource) Validate() error {
	return firstError(wrap("path", gitv1.ValidateSourcePath(s.Path)), wrap("head", s.Head.Validate()))
}

// Validate checks the request structure; whether the observed sources are right
// for the attempt depends on the context (DeriveBegin).
func (r BeginRequestV3) Validate() error {
	if r.Sources == nil || len(r.Sources) > gitv1.MaxSources {
		return fail("sources", "must be an array of at most %d observations", gitv1.MaxSources)
	}
	for i, s := range r.Sources {
		if err := s.Validate(); err != nil {
			return fail(fmt.Sprintf("sources[%d]", i), "%v", err)
		}
	}
	return nil
}

// Validate checks the response structure.
func (r BeginResponseV3) Validate() error {
	if err := firstError(wrap("attemptId", r.AttemptID.Validate()), wrap("beganAt", r.BeganAt.Validate())); err != nil {
		return err
	}
	if !validState(r.WorkspaceState) {
		return fail("workspaceState", "unknown state %q", r.WorkspaceState)
	}
	if r.WorkspaceState == StateNone && r.IgnoredPresent {
		return fail("ignoredPresent", "must be false for a source-free attempt")
	}
	return nil
}

// ---- result ----

func (c CommitOutput) validate(path string) error {
	if err := firstError(
		checkID(path+".repositoryId", c.RepositoryID),
		wrap(path+".ref", gitv1.ValidateBranchRef(c.Ref)),
		checkLiveCommit(path+".commit", c.Commit),
		checkID(path+".pushId", c.PushID),
	); err != nil {
		return err
	}
	return nil
}

// validateCommitOutputs checks every output and the strict (repositoryId, ref)
// order that makes one result have one canonical form and one hash.
func validateCommitOutputs(path string, outs []CommitOutput) error {
	if outs == nil || len(outs) > maxCommitOutputs {
		return fail(path, "must be an array of at most %d commit outputs", maxCommitOutputs)
	}
	for i, o := range outs {
		if err := o.validate(fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
		if i > 0 {
			prev := outs[i-1]
			if prev.RepositoryID > o.RepositoryID || (prev.RepositoryID == o.RepositoryID && prev.Ref >= o.Ref) {
				return fail(fmt.Sprintf("%s[%d]", path, i), "commit outputs are strictly ordered by repository and ref, one per ref")
			}
		}
	}
	return nil
}

// v1 projects the result to the v1 Result with no source commit, so the
// shared rules (outcome shape, exit code, reason, question, outputs, diagnostic
// artifacts) have exactly their v1 meaning.
func (r ResultV3) v1() runtimev1.Result {
	return runtimev1.Result{
		Outcome: r.Outcome, ExitCode: r.ExitCode, Reason: r.Reason, Question: r.Question,
		Outputs: r.Outputs, DiagnosticArtifactIDs: r.DiagnosticArtifactIDs,
	}
}

// Validate checks the result: the v1 rules for everything it shares, and the
// commit outputs, which only a completed attempt reports.
func (r ResultV3) Validate() error {
	if err := r.v1().Validate(); err != nil {
		return err
	}
	if err := validateCommitOutputs("commits", r.Commits); err != nil {
		return err
	}
	if r.Outcome != OutcomeCompleted && len(r.Commits) != 0 {
		return fail("outcome", "%s requires no commit outputs", r.Outcome)
	}
	return nil
}

// CanonicalResult returns the canonical bytes covered by the result hash: the
// v1 canonicalization of the validated result object.
func CanonicalResult(r ResultV3) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return runtimev1.Canonicalize(raw)
}

// ResultHash is "sha256:" and the lowercase hex SHA-256 of CanonicalResult.
func ResultHash(r ResultV3) (Digest, error) {
	canonical, err := CanonicalResult(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return Digest("sha256:" + hex.EncodeToString(sum[:])), nil
}

// Validate checks the request structurally and that ResultHash is the hash of
// the canonical result.
func (r ResultRequestV3) Validate() error {
	if err := firstError(wrap("resultHash", r.ResultHash.Validate()), r.Result.Validate()); err != nil {
		return err
	}
	got, err := ResultHash(r.Result)
	if err != nil {
		return err
	}
	if got != r.ResultHash {
		return &ContractError{Code: runtimev1.CodeResultHashMismatch, Message: "resultHash does not match the canonical result"}
	}
	return nil
}

// ---- runtime-issued Git grants ----

func (r GitSeedRequest) Validate() error { return checkID("repositoryId", r.RepositoryID) }

func (r GitIntegrationRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("gateId", r.GateID))
}

// Validate checks the response: a valid Git grant of kind seed or integration
// (the Git contract's own claim validation, including the single repository, the
// seed flag and the one constrained update), a well-formed credential and the file
// it is delivered to. Whether it is the grant this attempt may hold is
// CheckSeedGrant and CheckIntegrationGrant.
func (r GitGrantResponse) Validate() error {
	if err := wrap("grant", r.Grant.Validate()); err != nil {
		return err
	}
	if r.Grant.Kind != gitv1.GrantSeed && r.Grant.Kind != gitv1.GrantIntegration {
		return fail("grant.kind", "a runtime route issues a seed or an integration grant")
	}
	if !credentialRE.MatchString(r.Credential) {
		return fail("credential", "must match Credential syntax")
	}
	if !gitFileRE.MatchString(r.CredentialFile) {
		return fail("credentialFile", "must be a file directly under %s", GitDirectory)
	}
	return nil
}

// Validate checks the envelope: a known v3 code and the v1 message bounds.
func (r ErrorResponse) Validate() error {
	e := r.Error
	if _, ok := Status(e.Code); !ok {
		return fail("error.code", "unknown code %q", e.Code)
	}
	if e.Message == "" || len(e.Message) > 2048 {
		return fail("error.message", "must be 1-2048 bytes")
	}
	if e.RequestID == "" || len(e.RequestID) > 128 {
		return fail("error.requestId", "must be 1-128 bytes")
	}
	if e.Details == nil {
		return fail("error.details", "must be an object")
	}
	return nil
}
