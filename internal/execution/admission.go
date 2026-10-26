package execution

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// AdmitCommand is the public request to start a work run: the contract's
// creation fields. The server resolves and pins spec, configuration and
// credentials; the caller never submits candidate or checkpoint identifiers.
type AdmitCommand struct {
	Scope           domain.Scope
	WorkItemID      domain.WorkItemID
	DefinitionID    domain.DefinitionID
	ContentRevision domain.ContentRevision
	SourceCommit    string
	Inputs          map[string]runtimev1.Value
}

// CleanupCommand is the internal request to start a deployment cleanup run
// (M2). It is not reachable from the public API.
type CleanupCommand struct {
	Scope        domain.Scope
	DeploymentID string
	DefinitionID domain.DefinitionID
	Inputs       map[string]runtimev1.Value
}

var commitPattern = regexp.MustCompile(runtimev1.Patterns["GitCommit"])

func (c AdmitCommand) validate() error {
	if err := c.Scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.WorkItemID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.DefinitionID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.ContentRevision.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if !commitPattern.MatchString(c.SourceCommit) {
		return invalidf("sourceCommit must be a full lowercase object name")
	}
	return nil
}

// Prepare authorizes the caller and resolves everything admission pins, outside
// any transaction, into an AdmissionPlan. Only work runs are admitted here: a
// public request can never create a cleanup run. Definition, capability,
// type, input and source conflicts are rejected here, before any state exists,
// so invalid graphs and inputs fail before dispatch (PX-A01).
func (s *Service) Prepare(ctx context.Context, caller Caller, cmd AdmitCommand) (*AdmissionPlan, error) {
	if err := cmd.validate(); err != nil {
		return nil, err
	}
	actor, err := s.authorize(ctx, caller, cmd.Scope, PermRunManage)
	if err != nil {
		return nil, err
	}
	def, doc, err := s.loadDefinition(ctx, cmd.Scope, cmd.DefinitionID)
	if err != nil {
		return nil, err
	}
	if err := s.checkKinds(ctx, doc); err != nil {
		return nil, err
	}
	binding, err := s.bind(doc)
	if err != nil {
		return nil, err
	}
	work, err := s.d.Work.Snapshot(ctx, cmd.Scope, cmd.WorkItemID)
	if err != nil {
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
	src, err := s.d.Sources.Verify(ctx, cmd.Scope, cmd.WorkItemID, cmd.SourceCommit)
	if err != nil {
		return nil, mapLookup(err, "source")
	}
	if src.WorkspaceID == "" {
		return nil, Unavailablef(nil, "the workspace could not be resolved")
	}
	snap, err := s.resolveConfiguration(ctx, cmd.Scope, doc, cmd.Inputs)
	if err != nil {
		return nil, err
	}
	snap, bindings, err := s.resolveEnvironmentSet(ctx, cmd.Scope, actor, doc, snap)
	if err != nil {
		return nil, err
	}
	snap.Runtime = binding
	run := &Run{
		Scope: cmd.Scope, ID: domain.RunID(s.d.NewID("run")), Purpose: PurposeWork,
		WorkItemID: cmd.WorkItemID, WorkspaceID: src.WorkspaceID,
		DefinitionID: def.ID, DefinitionDigest: string(def.Digest),
		ContentRevision: work.ContentRevision, Specification: &Specification{Spec: work.Spec, AcceptanceCriteria: work.Criteria},
		SourceCommit: cmd.SourceCommit, Snapshot: snap, SourceRetention: RetentionPending,
		RequestedBy: actor, Status: RunQueued, Reason: ReasonCreated, Version: 1,
	}
	plan, err := s.plan(run, doc, src, actor, caller.RequestID, work.ContentRevision)
	if err != nil {
		return nil, err
	}
	plan.Environments = bindings
	return plan, nil
}

// PrepareCleanup builds the plan of an internal cleanup run. It requires the
// deployment-bound authorization port (absent in M1: ErrNotConfigured), uses no
// workspace and no work item, and so consumes no business WIP. A definition that
// declares an environment additionally requires the retained-configuration port:
// the run pins the deployment's own retained snapshot and exact credential
// versions, and the admission transaction rechecks both.
func (s *Service) PrepareCleanup(ctx context.Context, cmd CleanupCommand) (*AdmissionPlan, error) {
	if err := cmd.Scope.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := cmd.DefinitionID.Validate(); err != nil {
		return nil, invalidf("%v", err)
	}
	if err := domain.WorkItemID(cmd.DeploymentID).Validate(); err != nil {
		return nil, invalidf("a recorded deployment is required")
	}
	if s.d.Cleanup == nil {
		return nil, fmt.Errorf("%w: cleanup admission is not available", ErrNotConfigured)
	}
	if err := s.d.Cleanup.AuthorizeCleanup(ctx, cmd.Scope, cmd.DeploymentID); err != nil {
		return nil, classifyAuthority(err)
	}
	def, doc, err := s.loadDefinition(ctx, cmd.Scope, cmd.DefinitionID)
	if err != nil {
		return nil, err
	}
	if err := s.checkKinds(ctx, doc); err != nil {
		return nil, err
	}
	binding, err := s.bind(doc)
	if err != nil {
		return nil, err
	}
	for _, st := range doc.Spec.Steps {
		if st.Environment != "" && (s.d.CleanupEnvironments == nil || !storeAdmitsEnvironments(s.d.Store)) {
			// Both the retained-configuration port and a store that revalidates
			// environments and registers a cleanup run's exact credential pins in
			// the admission transaction are required; either one missing refuses.
			return nil, fmt.Errorf("%w: retained deployment environment admission requires the cleanup binding and a transactional store", ErrNotConfigured)
		}
		if st.Executable() && st.Workspace != nil && st.Workspace.Access != pipeline.AccessNone && st.Workspace.Access != "" {
			return nil, Refusedf("a cleanup run uses no workspace")
		}
	}
	inputs, err := cleanupInputs(doc, cmd)
	if err != nil {
		return nil, err
	}
	snap, err := s.resolveConfiguration(ctx, cmd.Scope, doc, inputs)
	if err != nil {
		return nil, err
	}
	snap, bindings, err := s.resolveCleanupEnvironments(ctx, cmd.Scope, cmd.DeploymentID, doc, snap)
	if err != nil {
		return nil, err
	}
	snap.Runtime = binding
	run := &Run{
		Scope: cmd.Scope, ID: domain.RunID(s.d.NewID("run")), Purpose: PurposeCleanup, DeploymentID: cmd.DeploymentID,
		DefinitionID: def.ID, DefinitionDigest: string(def.Digest), Snapshot: snap, SourceRetention: RetentionNotRequired,
		RequestedBy: System, Status: RunQueued, Reason: ReasonCreated, Version: 1,
	}
	plan, err := s.plan(run, doc, SourceInfo{}, System, "", 0)
	if err != nil {
		return nil, err
	}
	plan.Environments = bindings
	return plan, nil
}

// CleanupDeploymentInput is the run input through which a cleanup run receives
// the recorded deployment it was admitted for (T-092). A definition that declares
// a string input of this name gets the deployment as its run-input value, so one
// project cleanup definition serves every due deployment of the project; a
// definition that does not declare it is unaffected. The value is always the
// command's DeploymentID, which authorization has just bound to the scope.
const CleanupDeploymentInput = "deployment_id"

// cleanupInputs returns the explicit run inputs of a cleanup run: the caller's
// own inputs plus the admitted deployment, never a caller's claim about it. The
// command's map is not modified. A caller-supplied deployment_id that is not the
// admitted deployment is refused, so a value can neither be supplied nor
// overridden from outside the admission.
func cleanupInputs(doc pipeline.Document, cmd CleanupCommand) (map[string]runtimev1.Value, error) {
	if v, ok := cmd.Inputs[CleanupDeploymentInput]; ok && (!v.IsString() || v.String() != cmd.DeploymentID) {
		return nil, Refusedf("the %s input of a cleanup run is the recorded deployment and cannot be supplied", CleanupDeploymentInput)
	}
	decl, declared := doc.Spec.Inputs[CleanupDeploymentInput]
	if declared && decl.Type != pipeline.TypeString {
		return nil, Refusedf("the %s input of a cleanup definition must be a string", CleanupDeploymentInput)
	}
	out := make(map[string]runtimev1.Value, len(cmd.Inputs)+1)
	for n, v := range cmd.Inputs {
		out[n] = v
	}
	delete(out, CleanupDeploymentInput)
	if declared {
		out[CleanupDeploymentInput] = runtimev1.StringValue(cmd.DeploymentID)
	}
	return out, nil
}

func (s *Service) loadDefinition(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, pipeline.Document, error) {
	def, err := s.d.Definitions.Get(ctx, scope, id)
	if err != nil {
		return pipeline.Definition{}, pipeline.Document{}, mapLookup(err, "definition")
	}
	doc, err := def.Content.Document()
	if err != nil {
		return pipeline.Definition{}, pipeline.Document{}, Unavailablef(err, "the stored definition is unreadable")
	}
	if err := checkDefinitionVersion(doc.APIVersion); err != nil {
		return pipeline.Definition{}, pipeline.Document{}, err
	}
	return def, doc, nil
}

// checkDefinitionVersion is the fail-closed execution guard on the pipeline
// document version. This executor implements exactly the current format, so
// anything else (empty, unknown or a future version a shared parser may later
// learn to read) is refused before admission creates any state: readability and
// registration do not grant execution. The message carries no document content.
func checkDefinitionVersion(version string) error {
	if version == pipelinev1.APIVersion {
		return nil
	}
	return Refusedf("the pipeline definition version is not supported for execution")
}

// checkKinds rejects steps whose kind this installation does not execute.
func (s *Service) checkKinds(ctx context.Context, doc pipeline.Document) error {
	seen := map[pipeline.StepKind]bool{}
	for _, st := range doc.Spec.Steps {
		if seen[st.Kind] {
			continue
		}
		seen[st.Kind] = true
		ok, err := s.d.Capabilities.StepKindEnabled(ctx, st.Kind)
		if err != nil {
			return Unavailablef(err, "step kind capabilities are unavailable")
		}
		if !ok {
			return Refusedf("the %s step kind is not enabled", st.Kind)
		}
	}
	return nil
}

func mapLookup(err error, what string) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrHidden):
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	case errors.Is(err, ErrAdmissionRefused), errors.Is(err, ErrConflict), errors.Is(err, ErrInvalid), errors.Is(err, ErrUnavailable),
		errors.Is(err, ErrWorkCancelled), errors.Is(err, ErrNotActionable):
		return err
	}
	return Unavailablef(err, "%s lookup failed", what)
}

// resolveConfiguration converts the definition's declarations into a
// configuration request and returns the pinned snapshot.
func (s *Service) resolveConfiguration(ctx context.Context, scope domain.Scope, doc pipeline.Document, explicit map[string]runtimev1.Value) (Snapshot, error) {
	names := make([]string, 0, len(doc.Spec.Inputs))
	for n := range doc.Spec.Inputs {
		names = append(names, n)
	}
	sort.Strings(names)
	req := ConfigRequest{Scope: scope, Explicit: explicit}
	for _, n := range names {
		in := doc.Spec.Inputs[n]
		d := InputDecl{Name: n, Type: string(in.Type), Required: in.Required}
		if in.Default != nil {
			var v runtimev1.Value
			switch in.Default.Type {
			case pipeline.TypeBoolean:
				v = runtimev1.BoolValue(in.Default.Bool)
			case pipeline.TypeInteger:
				v = runtimev1.IntValue(in.Default.Int)
			default:
				v = runtimev1.StringValue(in.Default.Str)
			}
			d.Default = &v
		}
		req.Inputs = append(req.Inputs, d)
	}
	for _, st := range doc.Spec.Steps {
		if st.Kind != pipeline.KindContainer {
			continue
		}
		envNames := make([]string, 0, len(st.Env))
		for n, e := range st.Env {
			if e.SecretRef != "" {
				envNames = append(envNames, n)
			}
		}
		sort.Strings(envNames)
		for _, n := range envNames {
			req.Secrets = append(req.Secrets, SecretUse{Step: st.ID, Name: n, Ref: st.Env[n].SecretRef, Delivery: runtimev1.DeliveryEnv})
		}
		fileNames := make([]string, 0, len(st.SecretFiles))
		for n := range st.SecretFiles {
			fileNames = append(fileNames, n)
		}
		sort.Strings(fileNames)
		for _, n := range fileNames {
			req.Secrets = append(req.Secrets, SecretUse{Step: st.ID, Name: n, Ref: st.SecretFiles[n], Delivery: runtimev1.DeliveryFile})
		}
	}
	res, err := s.d.Configuration.Resolve(ctx, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrAdmissionRefused), errors.Is(err, ErrUnavailable), errors.Is(err, ErrInvalid):
			return Snapshot{}, err
		}
		return Snapshot{}, Unavailablef(err, "configuration resolution failed")
	}
	return res.Snapshot, nil
}

// plan builds the pinned aggregate: steps in definition order, skip
// propagation for false conditions and the input snapshot digest.
func (s *Service) plan(run *Run, doc pipeline.Document, src SourceInfo, actor Actor, requestID string, revision domain.ContentRevision) (*AdmissionPlan, error) {
	now := s.now().UTC()
	run.CreatedAt, run.UpdatedAt = now, now
	digest, err := Digest(run.DefinitionID, run.DefinitionDigest, run.WorkItemID, run.ContentRevision, run.SourceCommit, run.Snapshot)
	if err != nil {
		return nil, err
	}
	run.SnapshotDigest = digest
	agg := &Aggregate{Run: run}
	for i, st := range doc.Spec.Steps {
		agg.Steps = append(agg.Steps, NewStepRun(run.ID, i, st, now))
	}
	if len(agg.Steps) == 0 {
		return nil, Refusedf("a definition has no steps")
	}
	agg.Propagate(now)
	var secrets []runtimev1.SecretBinding
	seen := map[string]bool{}
	for _, sec := range run.Snapshot.Secrets {
		k := fmt.Sprintf("%s@%d", sec.Binding.Ref, sec.Binding.Version)
		if !seen[k] {
			seen[k] = true
			secrets = append(secrets, sec.Binding)
		}
	}
	return &AdmissionPlan{Aggregate: agg, Source: src, Actor: actor, RequestID: requestID, WorkRevision: revision, Secrets: secrets}, nil
}

// Admit prepares and commits a work run in its own transaction, then confirms
// its retained source reference. Transports that need keyed creation call
// Prepare, join the plan to the idempotent transaction and then ConfirmSource.
func (s *Service) Admit(ctx context.Context, caller Caller, cmd AdmitCommand) (*Run, error) {
	plan, err := s.Prepare(ctx, caller, cmd)
	if err != nil {
		return nil, err
	}
	run, _, err := s.d.Store.Admit(ctx, plan)
	if err != nil {
		return nil, err
	}
	return s.ConfirmSource(ctx, run.Scope, run.ID)
}

// AdmitCleanup commits an internal cleanup run. M1 wiring has no deployment
// authorization, so this returns ErrNotConfigured there.
func (s *Service) AdmitCleanup(ctx context.Context, cmd CleanupCommand) (*Run, error) {
	plan, err := s.PrepareCleanup(ctx, cmd)
	if err != nil {
		return nil, err
	}
	run, _, err := s.d.Store.Admit(ctx, plan)
	return run, err
}

// ConfirmSource creates and confirms the run-bound retained source reference of
// a pending work run and records the confirmation. Dispatch is refused until it
// succeeds; a crash anywhere before leaves the same run pending, and
// reconciliation repeats this idempotently instead of creating a new run.
func (s *Service) ConfirmSource(ctx context.Context, scope domain.Scope, id domain.RunID) (*Run, error) {
	view, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	run := view.Run
	if run.SourceRetention != RetentionPending {
		return run, nil
	}
	ref, err := s.d.Sources.Retain(ctx, scope, run.WorkspaceID, run.ID, run.SourceCommit)
	if err != nil {
		return run, mapLookup(err, "source retention")
	}
	agg, _, err := s.d.Store.Update(ctx, scope, id, UpdateOptions{}, func(ctx context.Context, env Env, agg *Aggregate) (Effects, error) {
		return Effects{}, agg.ConfirmRetention(ref, env.Now())
	})
	if err != nil {
		return run, err
	}
	return agg.Run, nil
}
