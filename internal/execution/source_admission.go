package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// SourceRunCommand is the internal request to start a run of any origin on the
// internal Git service: a work item's, a push's, a person's or a provisioning
// run (PX-23-PX-25). It is not the public POST /runs, which keeps admitting work
// runs on externally hosted source exactly as before.
//
// The command never carries a commit for a push origin (the applied push record
// decides) and never a work item for any other origin: no work item is
// fabricated for a run that did not start from one, and such a run consumes no
// business WIP.
type SourceRunCommand struct {
	Scope        domain.Scope
	Origin       Origin
	DefinitionID domain.DefinitionID
	// ContentRevision pins a work_item origin's specification; it is zero for
	// every other origin.
	ContentRevision domain.ContentRevision
	Inputs          map[string]runtimev1.Value
	// RunID, when set, is the run's stable identity: a caller that deduplicates
	// (the Git trigger by trigger, push record and ref) derives it, so a redelivery
	// is a replay that returns the stored run and a different declaration under
	// the same ID is a conflict. Empty selects a fresh identity.
	RunID domain.RunID
	// Repository names the repository of the implicit source of a document that
	// declares no sources (its one default repository at "."): by name for a
	// manual, provisioning or work item run; a push origin's implicit source is
	// the pushed repository. Empty with no declared sources means a source-free run.
	Repository string
	// Pins selects, by repository name, what a source pins when it is neither the
	// push origin's source nor a work item's writable source: the tip of a ref or
	// an exact commit. Absent means the default branch's tip, read once.
	Pins map[string]SourcePin
}

func (c SourceRunCommand) validate() error {
	if err := c.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.DefinitionID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.Origin.Validate(); err != nil {
		return err
	}
	if c.RunID != "" {
		if err := c.RunID.Validate(); err != nil {
			return invalidf("%v", err)
		}
	}
	if c.Origin.Kind == OriginWorkItem {
		if err := c.ContentRevision.Validate(); err != nil {
			return invalidf("%v", err)
		}
	} else if c.ContentRevision != 0 {
		return invalidf("only a work item's run pins a content revision")
	}
	for name, p := range c.Pins {
		if err := p.Validate(); err != nil {
			return invalidf("pin of %s: %v", name, err)
		}
	}
	return nil
}

// declareSources derives the sources a run declares from its document and
// command. A document with sources declares them as written; a document without
// has the one implicit source at "." (write exactly when a step writes the
// workspace) in the command's repository, the push's repository, or no source at
// all.
func declareSources(doc pipeline.Document, cmd SourceRunCommand) []DeclaredSource {
	srcs, implicit := pipeline.EffectiveSources(doc)
	if !implicit {
		out := make([]DeclaredSource, len(srcs))
		for i, s := range srcs {
			out[i] = DeclaredSource{Repository: s.Repository, Path: s.Path, Access: gitv1.SourceAccess(s.Access)}
		}
		return out
	}
	d := DeclaredSource{Path: srcs[0].Path, Access: gitv1.SourceAccess(srcs[0].Access)}
	switch {
	case cmd.Repository != "":
		d.Repository = cmd.Repository
	case cmd.Origin.Kind == OriginPush:
		d.RepositoryID = cmd.Origin.RepositoryID
	default:
		return nil
	}
	return []DeclaredSource{d}
}

// pinRequests chooses what each source pins: the push origin's source pins the
// applied push entry's new commit (never a later tip), a work item's writable
// source the tip of its task branch (the workspace's accepted commit), every
// other source its command pin or the default branch's tip.
func pinRequests(cmd SourceRunCommand, runID domain.RunID, declared []DeclaredSource, repos []SourceRepository) ([]PinRequest, error) {
	reqs := make([]PinRequest, len(declared))
	pushed := false
	for i, d := range declared {
		repo := repos[i]
		r := PinRequest{Repository: repo}
		switch {
		case cmd.Origin.Kind == OriginPush && repo.ID == cmd.Origin.RepositoryID:
			r.Push = &PushPin{PushID: cmd.Origin.PushID, Ref: cmd.Origin.Ref}
			pushed = true
		case cmd.Origin.Kind == OriginWorkItem && d.Access == gitv1.AccessWrite:
			branch, err := cmd.Origin.Branch(runID)
			if err != nil {
				return nil, invalidf("%v", err)
			}
			r.Ref = &branch
		default:
			if p, ok := cmd.Pins[repo.Name]; ok {
				r.Ref, r.Commit = p.Ref, p.Commit
			}
		}
		reqs[i] = r
	}
	if cmd.Origin.Kind == OriginPush && !pushed {
		return nil, Refusedf("source_set_push_repository: the pushed repository is not a source of the run")
	}
	return reqs, nil
}

// mapSourceError classifies a SourceSets or InternalWorkspaces failure: an
// unknown or foreign repository is a stable admission refusal (indistinguishable
// from an unknown one), refusals pass through and anything else is an outage.
func mapSourceError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrHidden):
		return Refusedf("source_repository_unknown: a %s does not exist in the project", what)
	case errors.Is(err, ErrAdmissionRefused), errors.Is(err, ErrConflict), errors.Is(err, ErrInvalid), errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrNotConfigured), errors.Is(err, ErrWorkCancelled), errors.Is(err, ErrNotActionable), errors.Is(err, ErrStale), errors.Is(err, ErrBusy):
		return err
	}
	return Unavailablef(err, "the %s lookup failed", what)
}

func (s *Service) loadSourceDefinition(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, pipeline.Document, error) {
	def, err := s.d.Definitions.Get(ctx, scope, id)
	if err != nil {
		return pipeline.Definition{}, pipeline.Document{}, mapLookup(err, "definition")
	}
	doc, err := def.Content.Document()
	if err != nil {
		return pipeline.Definition{}, pipeline.Document{}, Unavailablef(err, "the stored definition is unreadable")
	}
	if err := s.checkSourceSetVersion(doc.APIVersion); err != nil {
		return pipeline.Definition{}, pipeline.Document{}, err
	}
	return def, doc, nil
}

// PrepareSourceRun authorizes the caller and resolves everything the admission
// of a run of any origin pins, outside any transaction, into an AdmissionPlan:
// the definition (kodama/v1alpha3 only), the work item's specification and
// writable workspace (a work item's run only), the repositories of the project,
// one exact commit per source and the validated source set. A refused set pins
// nothing and creates nothing: a foreign repository, a repository that is not
// ready, a duplicate path or an unreachable commit fails here (PX-A19).
//
// A replay of the same RunID with the same declaration returns the stored run
// with the commits it pinned, however the branches moved since; another
// declaration under that ID is a conflict.
func (s *Service) PrepareSourceRun(ctx context.Context, caller Caller, cmd SourceRunCommand) (*AdmissionPlan, error) {
	if err := cmd.validate(); err != nil {
		return nil, err
	}
	if s.d.Shared == nil || s.d.Runtime == nil || s.d.SourceSets == nil {
		return nil, ErrNotConfigured
	}
	actor, err := s.authorize(ctx, caller, cmd.Scope, PermRunManage)
	if err != nil {
		return nil, err
	}
	def, doc, err := s.loadSourceDefinition(ctx, cmd.Scope, cmd.DefinitionID)
	if err != nil {
		return nil, err
	}
	if err := s.checkKinds(ctx, doc); err != nil {
		return nil, err
	}
	declared := declareSources(doc, cmd)
	declaration, err := declarationDigest(cmd.Origin, string(def.ID), string(def.Digest), int64(cmd.ContentRevision), declared, pinsOf(cmd.Pins), cmd.Inputs)
	if err != nil {
		return nil, err
	}
	runID := cmd.RunID
	if runID == "" {
		runID = domain.RunID(s.d.NewID("run"))
	} else if plan, err := s.replaySourceRun(ctx, cmd.Scope, runID, declaration, actor, caller); plan != nil || err != nil {
		return plan, err
	}
	var work WorkSnapshot
	if cmd.Origin.Kind == OriginWorkItem {
		if work, err = s.d.Work.Snapshot(ctx, cmd.Scope, cmd.Origin.WorkItemID); err != nil {
			return nil, mapLookup(err, "work item")
		}
		switch {
		case work.Cancelled:
			return nil, ErrWorkCancelled
		case !work.Actionable:
			return nil, ErrNotActionable
		case work.ContentRevision != cmd.ContentRevision:
			return nil, fmt.Errorf("%w: the work item's specification is at another content revision", ErrConflict)
		}
	}
	refs := make([]SourceRef, len(declared))
	for i, d := range declared {
		refs[i] = d.ref()
	}
	res, err := s.d.SourceSets.Resolve(ctx, cmd.Scope, refs)
	if err != nil {
		return nil, mapSourceError(err, "source repository")
	}
	if len(res.Repositories) != len(declared) {
		return nil, Refusedf("source_repository_unknown: a declared repository does not exist in the project")
	}
	workspaceID, err := s.provisionWorkspace(ctx, cmd, declared, res)
	if err != nil {
		return nil, err
	}
	reqs, err := pinRequests(cmd, runID, declared, res.Repositories)
	if err != nil {
		return nil, err
	}
	var commits []string
	if len(reqs) > 0 {
		if commits, err = s.d.SourceSets.Pin(ctx, cmd.Scope, reqs); err != nil {
			return nil, mapSourceError(err, "source commit")
		}
	}
	sources, err := NewRunSources(cmd.Origin, runID, res, declared, commits, declaration)
	if err != nil {
		return nil, err
	}
	snap, err := s.resolveConfiguration(ctx, cmd.Scope, doc, cmd.Inputs)
	if err != nil {
		return nil, err
	}
	snap, bindings, err := s.resolveEnvironmentSet(ctx, cmd.Scope, actor, doc, snap)
	if err != nil {
		return nil, err
	}
	pins, binding, err := s.planShared(ctx, cmd.Scope, runID, doc, !sources.Empty(), SourceInfo{WorkspaceID: workspaceID}, snap, sources)
	if err != nil {
		return nil, err
	}
	snap.Runtime = binding
	retention := RetentionNotRequired
	if !sources.Empty() {
		retention = RetentionPending
	}
	run := &Run{
		Scope: cmd.Scope, ID: runID, Purpose: PurposeWork, Origin: cmd.Origin, Sources: sources, Shared: pins,
		WorkItemID: cmd.Origin.WorkItemID, WorkspaceID: workspaceID,
		DefinitionID: def.ID, DefinitionDigest: string(def.Digest),
		Snapshot: snap, SourceRetention: retention,
		RequestedBy: actor, Status: RunQueued, Reason: ReasonCreated, Version: 1,
	}
	if cmd.Origin.Kind == OriginWorkItem {
		run.ContentRevision = work.ContentRevision
		run.Specification = &Specification{Spec: work.Spec, AcceptanceCriteria: work.Criteria}
	}
	plan, err := s.plan(run, doc, SourceInfo{WorkspaceID: workspaceID}, actor, caller.RequestID, cmd.ContentRevision)
	if err != nil {
		return nil, err
	}
	plan.Environments = bindings
	return plan, nil
}

// replaySourceRun answers an admission whose RunID already exists: the stored run
// when its declaration is the same (the plan carries it, so the store returns it
// unchanged), a conflict otherwise. A run of another kind under that ID is a
// conflict too. It reads nothing outside the store.
func (s *Service) replaySourceRun(ctx context.Context, scope domain.Scope, id domain.RunID, declaration string, actor Actor, caller Caller) (*AdmissionPlan, error) {
	view, err := s.d.Store.Get(ctx, scope, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case view.Run.Sources == nil || view.Run.Sources.Declaration != declaration:
		return nil, fmt.Errorf("%w: idempotency_conflict: the run exists with another declaration", ErrConflict)
	}
	return &AdmissionPlan{Aggregate: &Aggregate{Run: view.Run}, Actor: actor, RequestID: caller.RequestID, WorkRevision: view.Run.ContentRevision}, nil
}

func pinsOf(p map[string]SourcePin) []any {
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, []any{n, p[n].Ref, p[n].Commit})
	}
	return out
}

// provisionWorkspace ensures the work item's internally hosted workspace, with a
// task branch in each repository a source writes, before the set is built: the
// task branch must exist for its tip to be pinned. Only a work item's run that
// writes a source has a workspace; every other run has none.
func (s *Service) provisionWorkspace(ctx context.Context, cmd SourceRunCommand, declared []DeclaredSource, res ResolvedSources) (string, error) {
	if cmd.Origin.Kind != OriginWorkItem {
		return "", nil
	}
	var writes []string
	for i, d := range declared {
		if d.Access == gitv1.AccessWrite {
			writes = append(writes, res.Repositories[i].ID)
		}
	}
	if len(writes) == 0 {
		return "", nil
	}
	if s.d.InternalWorkspaces == nil {
		return "", fmt.Errorf("%w: a work item's writable source needs its internal workspace", ErrNotConfigured)
	}
	ws, err := s.d.InternalWorkspaces.Provision(ctx, cmd.Scope, cmd.Origin.WorkItemID, writes)
	if err != nil {
		return "", mapSourceError(err, "workspace")
	}
	if ws.ID == "" {
		return "", Unavailablef(nil, "the workspace could not be resolved")
	}
	return ws.ID, nil
}

// AdmitSourceRun prepares and commits a run of any origin in its own
// transaction, then retains its source set. Transports that deduplicate call
// PrepareSourceRun, join the plan to their keyed transaction and then
// ConfirmSource.
func (s *Service) AdmitSourceRun(ctx context.Context, caller Caller, cmd SourceRunCommand) (*Run, error) {
	plan, err := s.PrepareSourceRun(ctx, caller, cmd)
	if err != nil {
		return nil, err
	}
	run, _, err := s.d.Store.Admit(ctx, plan)
	if err != nil {
		return nil, err
	}
	return s.ConfirmSource(ctx, run.Scope, run.ID)
}

// digestOfSources folds the source-set pins into the run's snapshot digest.
func digestOfSources(base string, o Origin, r *RunSources) string {
	sum := sha256.Sum256([]byte(base + "|" + o.String() + "|" + r.Declaration + "|" + r.Digest))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// describeSourceSet gathers, outside any transaction, the evidence a claim of a
// source-set run needs. Only a writer of a work item's internal workspace needs
// any: the gate (no unknown or unsettled push on a task branch, WS-11) and the
// accepted commit of each task branch, with the workspace version they were read
// at. A blocked gate or a racing change makes the claim wait (ErrBusy/ErrStale
// skip the run) and never reserves a writer against stale evidence.
func (s *Service) describeSourceSet(ctx context.Context, run *Run, access Access) (SourceInfo, error) {
	info := SourceInfo{WorkspaceID: run.WorkspaceID}
	if access != AccessWrite || run.WorkspaceID == "" {
		return info, nil
	}
	if s.d.InternalWorkspaces == nil {
		return SourceInfo{}, fmt.Errorf("%w: the internal workspace seam is required for a work item's writer", ErrNotConfigured)
	}
	adm, err := s.d.InternalWorkspaces.AdmitWriter(ctx, run.Scope, run.WorkspaceID)
	if err != nil {
		return SourceInfo{}, mapSourceError(err, "workspace")
	}
	info.Tips, info.WorkspaceVersion = adm.Accepted, adm.Version
	return info, nil
}

// sourceAttemptShape is the workspace shape of an attempt row of a source-set
// run: attempts of a run without a workspace bind none (their sources are in the
// pinned context), those of a work item's workspace record the primary source's
// head and, for the writer, the accepted tip it must still have.
func sourceAttemptShape(a *Aggregate, access Access, src SourceInfo) (input, tip, workspaceID string) {
	run := a.Run
	if access == AccessNone || run.WorkspaceID == "" {
		return "", "", ""
	}
	heads := a.SourceHeads()
	primary := 0
	if access == AccessWrite {
		for i, e := range run.Sources.Entries {
			if e.Access == gitv1.AccessWrite {
				primary = i
				break
			}
		}
	}
	input = heads[primary]
	if access == AccessWrite {
		if tip = src.Tips[run.Sources.Entries[primary].RepositoryID]; tip == "" {
			tip = heads[primary]
		}
	}
	return input, tip, run.WorkspaceID
}

// InternalWriterEnv is implemented by a transaction environment that can reserve
// the writer slot of a work item's internally hosted workspace: the same single
// slot as ReserveWriter, pinned at the version the gate was checked at, so the
// reservation never commits against a gate answer that predates another writer's
// unknown push.
type InternalWriterEnv interface {
	ReserveInternalWriter(ctx context.Context, at *Attempt, observed domain.Version) error
}

// writerReservation is the peer effect that reserves the writer of an attempt. An
// attempt of a run without a workspace (every origin but a work item's) has no
// slot to reserve: the run executes one attempt at a time and the attempt's push
// authority is its own run branch.
func writerReservation(run *Run, at *Attempt, src SourceInfo) func(context.Context, Env) error {
	switch {
	case run.Sources == nil:
		return func(ctx context.Context, env Env) error { return env.ReserveWriter(ctx, at) }
	case at.WorkspaceID == "":
		return func(context.Context, Env) error { return nil }
	}
	observed := src.WorkspaceVersion
	return func(ctx context.Context, env Env) error {
		iw, ok := env.(InternalWriterEnv)
		if !ok {
			return fmt.Errorf("%w: the store cannot reserve an internal writer", ErrNotConfigured)
		}
		return iw.ReserveInternalWriter(ctx, at, observed)
	}
}

// confirmSourceSet retains the pinned set of a pending source-set run (every
// commit reachable for the run's retention, the run branch of each write source
// created) and records the confirmation. Dispatch is refused until it succeeds; a
// crash anywhere before leaves the same run pending, and reconciliation repeats
// this idempotently.
func (s *Service) confirmSourceSet(ctx context.Context, scope domain.Scope, run *Run) (*Run, error) {
	if s.d.SourceSets == nil {
		return run, fmt.Errorf("%w: the source set seam is required", ErrNotConfigured)
	}
	set, err := run.Sources.Set()
	if err != nil {
		return run, invalidf("source set: %v", err)
	}
	if err := s.d.SourceSets.Retain(ctx, scope, run.ID, set); err != nil {
		return run, mapSourceError(err, "source retention")
	}
	agg, _, err := s.d.Store.Update(ctx, scope, run.ID, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		return Effects{}, agg.ConfirmSourceSetRetention(env.Now())
	})
	if err != nil {
		return run, err
	}
	return agg.Run, nil
}

// CheckReplay is the store's rule for an admission whose run ID exists: a repeat
// of the same admission returns the stored run, whatever else is in the plan; a
// run of another declaration (another sources, origin or request under the same
// ID: a concurrent admission that lost the race, or a caller reusing the ID) is
// an idempotency conflict. A run of an earlier model, which has no declaration,
// is replayed as before.
func CheckReplay(existing, planned *Run) error {
	if existing.Sources == nil && planned.Sources == nil {
		return nil
	}
	if existing.Sources == nil || planned.Sources == nil || existing.Sources.Declaration != planned.Sources.Declaration {
		return fmt.Errorf("%w: idempotency_conflict: the run exists with another declaration", ErrConflict)
	}
	return nil
}
