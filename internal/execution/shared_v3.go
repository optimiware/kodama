package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// This file is the aggregate side of runtime context version 3: a run_shared run
// that pins an origin and a source set of internally hosted repositories
// (ADR 0011, docs/contracts/runtime-v3.md). Everything the run_shared model
// does (resources, holder, order, deadline, retry, begin fence) is unchanged;
// what differs is the source: one commit per repository instead of one input
// commit, a receipt per source, a begin observing every source and a result that
// reports the refs the attempt updated.

// SourceContextVersion is the runtime context version a source-set run pins.
const SourceContextVersion = int64(3)

// Credential files of the Git delivery (all directly under runtimev3.GitDirectory
// and distinct, as the context requires).
const (
	gitCredentialFile            = runtimev3.GitDirectory + "/credential"
	gitSeedCredentialFile        = runtimev3.GitDirectory + "/seed-credential"
	gitIntegrationCredentialFile = runtimev3.GitDirectory + "/integration-credential"
)

// SourceRun reports a run that executes under runtime context v3.
func (r *Run) SourceRun() bool { return r.Sources != nil }

// GitGrantRef is the stable reference of an attempt's Git grant carried in its
// pinned context (git.grantId). The context is pinned when the attempt is
// claimed, before the grant can exist (a grant is issued only to a live,
// committed attempt), so the reference is derived from the attempt, never read
// from the grant service: it correlates the context with the attempt's grants and
// is not a credential. The grant itself is issued idempotently per attempt.
func GitGrantRef(scope domain.Scope, attempt domain.AttemptID) string {
	sum := sha256.Sum256([]byte("kodama/git-grant-ref/" + string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(attempt)))
	return "gg_" + hex.EncodeToString(sum[:16])
}

// ---- preparation receipt ----

// RecordPreparationReceiptV3 records the trusted checkout receipt of the one
// preparation of a source-set run: one entry per source, in order, at its
// pinned commit. Everything the v2 receipt enforces holds (bound resources, not
// stopping, before the deadline, an identical repeat returns the original and a
// different receipt is a conflict); the digest covers the whole request.
func (a *Aggregate) RecordPreparationReceiptV3(req runtimev3.PreparationReceiptRequestV3, now time.Time) (digest string, replay bool, err error) {
	prep, ws := a.Resource(KeyPreparation), a.Resource(KeyWorkspace)
	if a.Run.Shared == nil || !a.Run.SourceRun() || a.Run.Sources.Empty() || prep == nil || ws == nil {
		return "", false, ErrNotFound
	}
	if err := req.Validate(); err != nil {
		return "", false, invalidf("%v", err)
	}
	entries := a.Run.Sources.Entries
	if len(req.Sources) != len(entries) {
		return "", false, invalidf("the receipt must cover exactly the sources of the run")
	}
	for i, s := range req.Sources {
		if s.Path != entries[i].Path || string(s.Commit) != entries[i].Commit {
			return "", false, invalidf("a receipt source is not the run's pinned source")
		}
	}
	if req.Attempts > a.Run.Shared.PolicyBody.Limits.PreparationAttempts {
		return "", false, invalidf("the receipt reports more tries than the policy allows")
	}
	obj := PreparationReceiptObjectV3{Request: req, RunID: a.Run.ID, ResourceKey: KeyPreparation, WorkspaceUID: ws.UID}
	if prep.HasReceipt() {
		if prep.ReceiptV3 == nil || prep.ReceiptDigest == "" || ws.UID == "" {
			return "", false, ErrReceiptConflict
		}
		d, derr := obj.Digest()
		if derr != nil || d != prep.ReceiptDigest {
			return "", false, ErrReceiptConflict
		}
		return d, true, nil
	}
	if !prep.Bound() || !ws.Bound() {
		return "", false, ErrResourceUnbound
	}
	if prep.Stopping() || prep.Stopped() || a.Run.Draining() || a.Run.Status.Terminal() ||
		(a.Run.DeadlineAt != nil && !now.Before(*a.Run.DeadlineAt)) {
		return "", false, ErrResourceRevoked
	}
	d, derr := obj.Digest()
	if derr != nil {
		return "", false, derr
	}
	from := prep.state()
	r := req
	r.Sources = append([]runtimev3.PreparedSource(nil), req.Sources...)
	t := now.UTC()
	prep.ReceiptV3, prep.ReceiptDigest, prep.ReceiptAt = &r, d, &t
	prep.touch(now)
	a.recordResource(prep, from, now)
	return d, false, nil
}

// ---- command begin ----

// BeginInputV3 is the verified begin request of an attempt of a source-set run.
type BeginInputV3 struct {
	Attempt *Attempt
	Request runtimev3.BeginRequestV3
	Context runtimev3.ContextV3
}

// RecordBeginV3 is RecordBegin for a context v3 attempt: the observation covers
// every source in order and the derived state is the server's.
func (a *Aggregate) RecordBeginV3(in BeginInputV3, now time.Time) (*BeginRecord, bool, error) {
	at := in.Attempt
	if a.Run.Shared == nil || !a.Run.SourceRun() || at == nil || a.Attempt(at.ID) == nil {
		return nil, false, ErrNotFound
	}
	state, ignored, err := runtimev3.DeriveBegin(in.Context, in.Request)
	if err != nil {
		return nil, false, invalidf("%v", err)
	}
	return a.recordBegin(at, in.Request, state, ignored, now)
}

// ---- heads and lineage ----

func (a *Aggregate) sortedBegins() []*BeginRecord {
	begins := append([]*BeginRecord(nil), a.Begins...)
	sort.Slice(begins, func(i, j int) bool {
		if !begins[i].BeganAt.Equal(begins[j].BeganAt) {
			return begins[i].BeganAt.Before(begins[j].BeganAt)
		}
		return begins[i].AttemptID < begins[j].AttemptID
	})
	return begins
}

// SourceHeads returns, per source in path order, the latest commit a successful
// attempt of this run published to that source's own branch, or the pinned commit
// before any. A read source's head never moves, and an agent-branch push never
// moves a head.
func (a *Aggregate) SourceHeads() []string {
	if a.Run.Sources == nil {
		return nil
	}
	entries := a.Run.Sources.Entries
	heads := make([]string, len(entries))
	for i, e := range entries {
		heads[i] = e.Commit
	}
	for _, b := range a.sortedBegins() {
		prior := a.Attempt(b.AttemptID)
		if prior == nil || prior.Status != AttemptSucceeded {
			continue
		}
		for _, out := range prior.Commits {
			for i, e := range entries {
				if e.Branch != nil && e.RepositoryID == out.RepositoryID && *e.Branch == out.Ref {
					heads[i] = out.Commit
				}
			}
		}
	}
	return heads
}

// ---- runtime v3 context ----

// BuildContextV3 assembles the immutable runtime v3 context of an attempt of a
// source-set run from the run's pins, durable resource state, recorded begins
// and writer evidence. Nothing in it comes from a workload or a client claim:
// the source set is the admission's pin, the preparation pin is the recorded
// server receipt bound to the workspace UID, a head is derived from the commits
// prior attempts reported (and the writer's accepted tip from src.Tips), and the
// Git delivery describes the grant the attempt will be issued. pins (may be nil)
// pins an agent step's profile and design baseline from its frozen payload; see
// AgentPins.
func BuildContextV3(a *Aggregate, step *StepRun, attempt domain.AttemptID, rs ResolvedStep, src SourceInfo, lim Limit, pins AgentPins) (runtimev3.ContextV3, error) {
	run := a.Run
	if run.Sources == nil {
		return runtimev3.ContextV3{}, invalidf("the run has no source set")
	}
	core, access, err := sharedContextCore(a, step, attempt, rs, lim, pins)
	if err != nil {
		return runtimev3.ContextV3{}, err
	}
	origin := OriginOf(run)
	c := runtimev3.ContextV3{
		ContextVersion: runtimev3.ContextVersion, RunModel: runtimev3.RunModel,
		TenantID: core.TenantID, ProjectID: core.ProjectID, RunID: core.RunID, StepID: core.StepID, AttemptID: core.AttemptID, ActionKey: core.ActionKey,
		Definition: core.Definition, Origin: origin.V3(), WorkItem: core.WorkItem, Command: core.Command,
		Workspace: runtimev3.WorkspaceV3{
			Access: runtimev1.WorkspaceAccess(access), MountPath: runtimev3.MountPath,
			Sources: []runtimev3.SourceV3{}, Lineage: []runtimev3.LineageEntryV3{},
		},
		Endpoints: core.Endpoints, Egress: core.Egress, Run: core.Run, Policy: core.Policy,
		Inputs: core.Inputs, Outputs: core.Outputs, Secrets: core.Secrets, Environment: core.Environment, Limits: core.Limits,
		Profile: core.Profile, DesignBaseline: core.DesignBaseline,
	}
	entries := run.Sources.Entries
	if len(entries) > 0 {
		prep := a.Resource(KeyPreparation)
		if prep == nil || prep.ReceiptV3 == nil || !a.ReceiptBound() {
			return runtimev3.ContextV3{}, Refusedf("the preparation receipt is not recorded")
		}
		heads := a.SourceHeads()
		for i, e := range entries {
			sv := runtimev3.SourceV3{Source: e.Source(), RepositoryURL: e.URL, HeadCommit: runtimev1.GitCommit(heads[i])}
			if access == AccessWrite && e.Access == gitv1.AccessWrite {
				tip := src.Tips[e.RepositoryID]
				if tip == "" {
					tip = heads[i]
				}
				t := runtimev1.GitCommit(tip)
				sv.ExpectedTip = &t
			}
			c.Workspace.Sources = append(c.Workspace.Sources, sv)
		}
		digest := runtimev1.Digest(run.Sources.Digest)
		c.Workspace.SourceSetDigest = &digest
		pv := &runtimev3.PreparationV3{ReceiptDigest: runtimev1.Digest(prep.ReceiptDigest)}
		pv.Sources = append(pv.Sources, prep.ReceiptV3.Sources...)
		c.Workspace.Preparation = pv
	}
	for _, b := range a.sortedBegins() {
		e := runtimev3.LineageEntryV3{
			StepID: b.StepID, AttemptID: b.AttemptID, Access: runtimev1.WorkspaceAccess(b.Access), BeganAt: stamp(b.BeganAt),
			WorkspaceState: b.WorkspaceState, IgnoredPresent: b.IgnoredPresent, Published: []runtimev3.CommitOutput{},
		}
		if prior := a.Attempt(b.AttemptID); prior != nil && prior.Status == AttemptSucceeded {
			for _, out := range prior.Commits {
				e.Published = append(e.Published, out.V3())
			}
		}
		c.Workspace.Lineage = append(c.Workspace.Lineage, e)
	}
	if len(entries) > 0 || origin.Kind == OriginProvision {
		g := &runtimev3.GitDelivery{BaseURL: run.Sources.BaseURL, Repositories: []runtimev3.GitRepositoryAccess{}}
		if run.Sources.CAFile != nil {
			ca := *run.Sources.CAFile
			g.CAFile = &ca
		}
		if len(entries) > 0 {
			ref, file := GitGrantRef(run.Scope, attempt), gitCredentialFile
			g.GrantID, g.CredentialFile = &ref, &file
		}
		if origin.Kind == OriginProvision {
			g.Runtime.Seed = &runtimev3.GitRuntimeGrant{CredentialFile: gitSeedCredentialFile}
		}
		if run.Sources.WritesSources() {
			g.Runtime.Integration = &runtimev3.GitRuntimeGrant{CredentialFile: gitIntegrationCredentialFile}
		}
		c.Git = g
		repos, err := c.GitAccess()
		if err != nil {
			return runtimev3.ContextV3{}, invalidf("%v", err)
		}
		g.Repositories = repos
	}
	return c, nil
}

// V3 is the runtime v3 wire form of a commit output.
func (o CommitOutput) V3() runtimev3.CommitOutput {
	return runtimev3.CommitOutput{RepositoryID: o.RepositoryID, Ref: o.Ref, Commit: runtimev1.GitCommit(o.Commit), PushID: o.PushID}
}

func commitOutputsOf(outs []runtimev3.CommitOutput) []CommitOutput {
	res := make([]CommitOutput, len(outs))
	for i, o := range outs {
		res[i] = CommitOutput{RepositoryID: o.RepositoryID, Ref: o.Ref, Commit: string(o.Commit), PushID: o.PushID}
	}
	return res
}

// planSharedAttemptV3 is the workload plan of a source-set attempt: its
// deterministic workload name, a fingerprint binding the context digest (which
// covers the source set and its commits), image, action key and policy pin, and
// the canonical context.
func planSharedAttemptV3(a *Aggregate, c runtimev3.ContextV3) (WorkloadPlan, error) {
	if err := c.Validate(); err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return WorkloadPlan{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	sum := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	name := WorkloadName(a.Run.Scope.TenantID, c.AttemptID)
	fp := sha256.Sum256([]byte("kodama/job-spec/v3|" + name + "|" + digest + "|" + c.Command.Image + "|" + c.ActionKey + "|" + a.Run.Shared.Policy.Digest))
	return WorkloadPlan{WorkloadName: name, SpecFingerprint: "sha256:" + hex.EncodeToString(fp[:]), ContextDigest: digest, Context: canonical}, nil
}

// DecodeContextV3 decodes the pinned runtime v3 context of an attempt spec.
func (s AttemptSpec) DecodeContextV3() (runtimev3.ContextV3, error) {
	if err := s.Validate(); err != nil {
		return runtimev3.ContextV3{}, err
	}
	c, err := runtimev3.DecodeRequest[runtimev3.ContextV3](s.Context)
	if err != nil {
		return runtimev3.ContextV3{}, invalidf("the attempt context is unreadable")
	}
	if c.Scope() != s.Scope || c.RunID != s.RunID || c.StepID != s.StepID || c.AttemptID != s.AttemptID {
		return runtimev3.ContextV3{}, invalidf("the attempt context belongs to another attempt")
	}
	return c, nil
}

// ---- result ----

// resultV1Of is the projection of a v3 result onto the v1 result the attempt
// records: everything but the commit outputs, which an attempt keeps apart
// (Attempt.Commits) and which are never a source commit of the run.
func resultV1Of(r runtimev3.ResultV3) runtimev1.Result {
	return runtimev1.Result{
		Outcome: r.Outcome, ExitCode: r.ExitCode, Reason: r.Reason, Question: r.Question,
		Outputs: r.Outputs, DiagnosticArtifactIDs: r.DiagnosticArtifactIDs,
	}
}

// CompleteAttemptV3 applies an accepted v3 result to a running attempt: as
// CompleteAttempt, and a completed result records the refs the attempt updated.
// The writer is settled without a source commit; each commit was accepted as a
// checkpoint, from its push record, before this transaction.
func (a *Aggregate) CompleteAttemptV3(at *Attempt, hash string, res runtimev3.ResultV3, now time.Time) error {
	if err := a.CompleteAttempt(at, hash, resultV1Of(res), now); err != nil {
		return err
	}
	if res.Outcome == runtimev1.OutcomeCompleted {
		at.Commits = commitOutputsOf(res.Commits)
		at.touch(now)
	}
	return nil
}
