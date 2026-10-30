package runtimev3

import (
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

func invalidRequest(msg string) error {
	return &ContractError{Code: domain.CodeInvalidRequest, Message: msg}
}

// DeriveBegin applies the server's derivation of an attempt's begin record to
// the request, in the context of the attempt:
//
//   - source-free (no sources): the request observes nothing; the state is none
//     and ignoredPresent false;
//   - source-backed: the request observes exactly the context's sources, in
//     order, by path; the state is clean exactly when every source's head equals
//     its headCommit, its tracked files are clean and it has no untracked file,
//     otherwise modified; ignoredPresent is true when any source discloses
//     ignored files and never makes the state modified by itself.
//
// An observation of a different set of paths, or an observation for a
// source-free attempt, is invalid_request. The values are derived, never
// client-asserted in an artifact request.
func DeriveBegin(ctx ContextV3, req BeginRequestV3) (WorkspaceState, bool, error) {
	if err := req.Validate(); err != nil {
		return "", false, invalidRequest(err.Error())
	}
	sources := ctx.Workspace.Sources
	if len(req.Sources) != len(sources) {
		return "", false, invalidRequest("the request must observe exactly the sources of the context")
	}
	if len(sources) == 0 {
		return StateNone, false, nil
	}
	clean, ignored := true, false
	for i, o := range req.Sources {
		if o.Path != sources[i].Path {
			return "", false, invalidRequest("the request must observe the sources of the context in order")
		}
		if o.Head != sources[i].HeadCommit || !o.TrackedClean || o.UntrackedPresent {
			clean = false
		}
		ignored = ignored || o.IgnoredPresent
	}
	if clean {
		return StateClean, ignored, nil
	}
	return StateModified, ignored, nil
}

// CheckBeginResponse verifies that a begin response is the derivation of the
// request for the attempt's context.
func CheckBeginResponse(ctx ContextV3, req BeginRequestV3, resp BeginResponseV3) error {
	state, ignored, err := DeriveBegin(ctx, req)
	if err != nil {
		return err
	}
	if resp.AttemptID != ctx.AttemptID {
		return &ContractError{Code: domain.CodeForbidden, Message: "the response is for another attempt"}
	}
	if resp.WorkspaceState != state || resp.IgnoredPresent != ignored {
		return invalidRequest("the response is not the derivation of the request")
	}
	return nil
}

// CheckResult applies the rules a schema cannot: the result against the
// attempt's context and trusted artifact metadata. The rules shared with v1
// (structure, exact declared outputs and their types, artifacts in this
// attempt's scope and usable) are the v1 CheckResult applied to a projection
// with no source rule; the commit outputs are then checked against the grants
// the attempt may hold.
//
// An output is authorized by the attempt's own grant as the context records it:
// a repository of the grant and a ref one of its update patterns selects (a
// writer's branch is in the grant only for the attempt holding the writer
// reservation, so a reader reporting it is refused). Otherwise it must be covered
// by one of the runtime-issued grants the control plane recorded for this
// attempt (issued, loaded by the consuming module from its own records and never
// supplied by the caller): a seed grant covers the one ref it creates in its
// repository, the default branch, with any commit (the seed is the repository's
// content); an integration grant covers exactly its constrained update, so the
// ref must be the integration ref and the commit must be the approved candidate
// (`to`). Each grant covers at most one output, the grant must belong to this
// attempt and project, and the context must advertise the capability. A refusal
// is 422 output_invalid.
//
// Whether the push record exists, was applied, moved the ref to the commit and
// was authorized by that grant is verified by the consuming module against the
// push record; this package never reaches Git or storage.
func CheckResult(r ResultV3, ctx ContextV3, artifacts map[domain.ArtifactID]runtimev1.ArtifactRef, issued ...gitv1.GrantClaims) error {
	if err := r.Validate(); err != nil {
		return &ContractError{Code: domain.CodeInvalidRequest, Message: err.Error()}
	}
	shared := runtimev1.Context{
		TenantID: ctx.TenantID, ProjectID: ctx.ProjectID, AttemptID: ctx.AttemptID, Outputs: ctx.Outputs,
		Workspace: runtimev1.Workspace{Access: AccessNone},
	}
	if err := runtimev1.CheckResult(r.v1(), shared, artifacts); err != nil {
		return err
	}
	used := map[string]bool{}
	for _, out := range r.Commits {
		if ctx.permitsUpdate(out) {
			continue
		}
		if id, ok := ctx.runtimeGrantCovers(out, issued, used); ok {
			used[id] = true
			continue
		}
		return &ContractError{Code: runtimev1.CodeOutputInvalid, Message: "a commit output names a repository or ref outside the attempt's Git grants"}
	}
	return nil
}

// permitsUpdate reports whether the attempt's own Git grant, as the context
// records it, selects the output's ref in the output's repository.
func (c ContextV3) permitsUpdate(out CommitOutput) bool {
	if c.Git == nil {
		return false
	}
	for _, repo := range c.Git.Repositories {
		if repo.RepositoryID != out.RepositoryID {
			continue
		}
		for _, p := range repo.Update {
			if gitv1.MatchRefPattern(p, out.Ref) {
				return true
			}
		}
	}
	return false
}

// belongsToAttempt reports whether a grant's claims were issued to this
// attempt's workload in this run and project.
func (c ContextV3) belongsToAttempt(g gitv1.GrantClaims) bool {
	a := g.Actor
	return g.TenantID == string(c.TenantID) && g.ProjectID == string(c.ProjectID) && a.Type == gitv1.ActorWorkload &&
		a.AttemptID != nil && *a.AttemptID == string(c.AttemptID) && a.RunID != nil && *a.RunID == string(c.RunID)
}

// runtimeGrantCovers finds an unused issued grant that covers the output and
// returns its ID.
func (c ContextV3) runtimeGrantCovers(out CommitOutput, issued []gitv1.GrantClaims, used map[string]bool) (string, bool) {
	if c.Git == nil {
		return "", false
	}
	for _, g := range issued {
		if used[g.GrantID] || g.Validate() != nil || !c.belongsToAttempt(g) || len(g.Repositories) != 1 || g.Repositories[0].ID != out.RepositoryID {
			continue
		}
		repo := g.Repositories[0]
		switch g.Kind {
		case gitv1.GrantSeed:
			if c.Git.Runtime.Seed != nil && repo.Seed && len(repo.Update) == 1 && repo.Update[0] == out.Ref {
				return g.GrantID, true
			}
		case gitv1.GrantIntegration:
			in := repo.Integration
			if c.Git.Runtime.Integration != nil && in != nil && in.Ref == out.Ref && in.To == string(out.Commit) && c.hasWriteSource(out.RepositoryID) {
				return g.GrantID, true
			}
		}
	}
	return "", false
}

func (c ContextV3) hasWriteSource(repositoryID string) bool {
	for _, s := range c.Workspace.Sources {
		if s.RepositoryID == repositoryID && s.Access == gitv1.AccessWrite {
			return true
		}
	}
	return false
}

// CheckSeedGrant is the pure admission rule of POST .../git/seed for the
// provisioning attempt of ctx. The caller (the control plane) supplies what only
// it knows: created, the repositories this run created in its admitted project;
// repo, the repository the request names as the control plane reads it; and
// unsettled, whether it has an unsettled push record. In order:
//
//   - the context advertises the seed capability, otherwise 403 forbidden (only a
//     provisioning run is seeded, PX-25);
//   - the repository is one this run created, otherwise 404 not_found, so a
//     foreign or unknown repository is indistinguishable;
//   - the Git contract's seed eligibility (gitv1.CheckSeedEligible: internal,
//     ready, empty, no unsettled push, with its 409/422 codes);
//   - the response is a valid seed grant for exactly that repository
//     (gitv1.GrantClaims.CheckSeed against the repository's default branch: one
//     repository, seed set, update exactly refs/heads/<defaultBranch>), issued to
//     this attempt's workload in this run and project, and delivered to the file
//     the context advertised.
//
// The credential never enters the check; the grant's lifetime bound (15 minutes)
// is the Git contract's.
func CheckSeedGrant(ctx ContextV3, req GitSeedRequest, resp GitGrantResponse, created []string, repo gitv1.Repository, unsettled bool) error {
	if err := req.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	if ctx.Git == nil || ctx.Git.Runtime.Seed == nil {
		return &ContractError{Code: domain.CodeForbidden, Message: "the attempt may not request a seed grant"}
	}
	owned := false
	for _, id := range created {
		owned = owned || id == req.RepositoryID
	}
	if !owned || repo.ID != req.RepositoryID {
		return &ContractError{Code: domain.CodeNotFound, Message: "repository not found"}
	}
	if err := gitv1.CheckSeedEligible(repo, unsettled); err != nil {
		return err
	}
	if err := resp.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	g := resp.Grant
	if err := g.CheckSeed(repo.DefaultBranch); err != nil {
		return invalidRequest(err.Error())
	}
	if !ctx.belongsToAttempt(g) || g.Repositories[0].ID != req.RepositoryID {
		return &ContractError{Code: domain.CodeForbidden, Message: "the grant does not belong to this attempt and repository"}
	}
	if resp.CredentialFile != ctx.Git.Runtime.Seed.CredentialFile {
		return invalidRequest("the credential is delivered to the file the context advertised")
	}
	return nil
}

// CheckIntegrationGrant is the pure admission rule of POST .../git/integration.
// approval is what the control plane derived from the gate named by the request
// and the repository; nil means the gate has not approved a candidate for this
// run. In order:
//
//   - the context advertises the integration capability and the repository has a
//     write source, otherwise 403 forbidden;
//   - an unapproved gate (nil approval, or one for another gate or repository) is
//     403 forbidden: no grant exists for an unapproved candidate;
//   - a moved base (the default branch's tip is no longer the approved base) is
//     409 publication_conflict: the approval no longer applies and the pipeline
//     recomputes the candidate;
//   - the response is a valid integration grant for exactly that update: one
//     repository, update exactly the integration ref, constraint {ref, from =
//     approved base, to = approved candidate, gateId}, issued to this attempt's
//     workload in this run and project, delivered to the advertised file.
//
// Code on the task branch cannot widen the grant: nothing about the candidate
// but its commit ID reaches it.
func CheckIntegrationGrant(ctx ContextV3, req GitIntegrationRequest, resp GitGrantResponse, approval *IntegrationApproval) error {
	if err := req.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	if ctx.Git == nil || ctx.Git.Runtime.Integration == nil || !ctx.hasWriteSource(req.RepositoryID) {
		return &ContractError{Code: domain.CodeForbidden, Message: "the attempt may not request an integration grant for this repository"}
	}
	if approval == nil || approval.GateID != req.GateID || approval.RepositoryID != req.RepositoryID {
		return &ContractError{Code: domain.CodeForbidden, Message: "no approved candidate exists for this gate and repository"}
	}
	if approval.CurrentTip != approval.Base {
		return &ContractError{Code: runtimev1.CodePublicationConflict, Message: "the target ref moved since the approval"}
	}
	if err := resp.Validate(); err != nil {
		return invalidRequest(err.Error())
	}
	g := resp.Grant
	if g.Kind != gitv1.GrantIntegration || len(g.Repositories) != 1 {
		return invalidRequest("not an integration grant")
	}
	repo := g.Repositories[0]
	in := repo.Integration
	if repo.ID != req.RepositoryID || in == nil || in.Ref != approval.Ref || in.From != approval.Base || in.To != approval.Candidate || in.GateID != approval.GateID {
		return invalidRequest("the grant is not exactly the approved update")
	}
	if !ctx.belongsToAttempt(g) {
		return &ContractError{Code: domain.CodeForbidden, Message: "the grant does not belong to this attempt"}
	}
	if resp.CredentialFile != ctx.Git.Runtime.Integration.CredentialFile {
		return invalidRequest("the credential is delivered to the file the context advertised")
	}
	return nil
}
