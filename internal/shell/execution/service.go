package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// The declared registry names used by this capability. They are looked up
// rather than duplicated so grants, checks and receipt policy always come from
// the single operation matrix.
const (
	operationPipelineStart authorize.OperationName = "pipeline.start"
	operationInspect       authorize.OperationName = "execution.inspect"
	operationList          authorize.OperationName = "execution.list"
	operationCancel        authorize.OperationName = "execution.cancel"
	operationOutputs       authorize.OperationName = "execution.action.outputs"
)

// NewService validates the injected boundaries and returns the execution
// operation service.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires an identity source")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires an authority reader")
	case config.Reader == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires a reader")
	case config.State == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires a state reader")
	case config.Transitions == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires a transition port")
	case config.Starts == nil:
		return nil, fault.New(fault.InternalFailure, "execution service requires a start port")
	}
	return &Service{
		clock:       config.Clock,
		ids:         config.IDs,
		authorizer:  config.Authorizer,
		authority:   config.Authority,
		reader:      config.Reader,
		state:       config.State,
		transitions: config.Transitions,
		starts:      config.Starts,
	}, nil
}

// checkActor verifies that the presented principal is structurally usable. A
// missing principal is unauthenticated; any other malformed principal is an
// invalid input.
func checkActor(actor authorize.Principal) error {
	if actor.ID == "" {
		return fault.New(fault.Unauthenticated, "execution operation requires a verified principal")
	}
	if err := actor.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, "execution operation principal is invalid", err)
	}
	return nil
}

// requireProjectScope validates a project-scoped operation target.
func requireProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "execution operations are project-scoped")
	}
	return nil
}

// scopeContains reports whether the actor scope covers the target scope.
func scopeContains(actor, target tenancy.Scope) bool {
	if actor.TenantID == "" || target.TenantID == "" {
		return false
	}
	if actor.TenantID != target.TenantID {
		return false
	}
	if actor.ProjectID == "" {
		return true
	}
	return actor.ProjectID == target.ProjectID
}

// authorizeOperation asks the shared authorizer for the operation grant and
// refuses a deny. A previously calculated allow is never durable permission;
// the commit unit still carries the registry authority reads.
func (s *Service) authorizeOperation(ctx context.Context, actor authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Operation, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, fmt.Sprintf("execution operation %q is not registered", operation))
	}
	decision, err := s.authorizer.Authorize(ctx, actor, operation, scope)
	if err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return authorize.Operation{}, err
		}
		return authorize.Operation{}, fault.Wrap(fault.InternalFailure, "execution operation authorization failed", err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, fmt.Sprintf("principal %q may not perform %q", actor.ID, operation))
	}
	return registered, nil
}

// authorizeOutputs applies the execution-only own-outputs grant. A running
// execution authenticates as an execution principal with its admitted
// permission; humans and services cannot satisfy an execution-only operation.
func (s *Service) authorizeOutputs(
	ctx context.Context,
	actor authorize.Principal,
	operation authorize.OperationName,
	scope tenancy.Scope,
	executionID id.ExecutionID,
	actionID id.ActionID,
	deadline time.Time,
) (authorize.Operation, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, fmt.Sprintf("execution operation %q is not registered", operation))
	}
	if actor.Kind != identity.KindExecution {
		return s.authorizeOperation(ctx, actor, operation, scope)
	}
	if actor.ExecutionID != executionID {
		return authorize.Operation{}, fault.New(fault.Forbidden, "execution principal does not own this execution")
	}
	admitted := authorize.AdmittedExecution{
		Principal:   actor,
		ExecutionID: executionID,
		ActionID:    actionID,
		Permission:  authorize.ExecOwnOutputs,
		Deadline:    deadline,
	}
	decision, err := authorize.AuthorizeExecution(admitted, operation, scope)
	if err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return authorize.Operation{}, err
		}
		return authorize.Operation{}, fault.Wrap(fault.InternalFailure, "execution output authorization failed", err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, fmt.Sprintf("execution principal %q may not perform %q", actor.ID, operation))
	}
	return registered, nil
}

// authorizeInspect applies the execution-only own-execution inspect grant. A
// running execution authenticates as an execution principal with its admitted
// permission and may inspect only its own execution; humans and services follow
// their registry rows.
func (s *Service) authorizeInspect(
	ctx context.Context,
	actor authorize.Principal,
	operation authorize.OperationName,
	scope tenancy.Scope,
	executionID id.ExecutionID,
) (authorize.Operation, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, fmt.Sprintf("execution operation %q is not registered", operation))
	}
	if actor.Kind != identity.KindExecution {
		return s.authorizeOperation(ctx, actor, operation, scope)
	}
	if actor.ExecutionID != executionID {
		return authorize.Operation{}, fault.New(fault.Forbidden, "execution principal does not own this execution")
	}
	admitted := authorize.AdmittedExecution{
		Principal:   actor,
		ExecutionID: executionID,
		Permission:  registered.Requires.Execution,
	}
	decision, err := authorize.AuthorizeExecution(admitted, operation, scope)
	if err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return authorize.Operation{}, err
		}
		return authorize.Operation{}, fault.Wrap(fault.InternalFailure, "execution inspect authorization failed", err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, fmt.Sprintf("execution principal %q may not perform %q", actor.ID, operation))
	}
	return registered, nil
}

// authorityReads builds the current-authority reads for one operation. Every
// read names the operation and its required grant; versioned checks carry the
// target and expected version.
func authorityReads(
	op authorize.Operation,
	scope tenancy.Scope,
	principal id.PrincipalID,
	executionTarget string,
	actionTarget string,
	expectedExecutionVersion uint64,
) []authorize.AuthorityRead {
	reads := make([]authorize.AuthorityRead, 0, len(op.Authority))
	for _, check := range op.Authority {
		read := authorize.AuthorityRead{
			Operation:   op.Name,
			Required:    op.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principal,
		}
		switch check {
		case authorize.CheckExecutionVersion, authorize.CheckPipelineVersion:
			read.TargetID = executionTarget
			read.ExpectedVersion = expectedExecutionVersion
		case authorize.CheckExecutionRunning:
			read.TargetID = executionTarget
		case authorize.CheckActionDeadline:
			read.TargetID = actionTarget
		}
		reads = append(reads, read)
	}
	return reads
}

// withoutAuthorityCheck drops one check from an operation's authority reads.
// It is used only where the operation's target shape has nothing to re-read;
// every other read, grant, principal and scope stays intact.
func withoutAuthorityCheck(reads []authorize.AuthorityRead, check authorize.AuthorityCheck) []authorize.AuthorityRead {
	filtered := make([]authorize.AuthorityRead, 0, len(reads))
	for _, read := range reads {
		if read.Check == check {
			continue
		}
		filtered = append(filtered, read)
	}
	return filtered
}

// confirmAuthority re-reads the current authority facts of a pure read
// operation before touching the projection reader, so stale session claims
// cannot authorize an inspection or listing after membership revocation.
func (s *Service) confirmAuthority(ctx context.Context, op authorize.Operation, scope tenancy.Scope, principal id.PrincipalID) error {
	reads := authorityReads(op, scope, principal, "", "", 0)
	facts, err := s.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

// buildReceipt builds the scoped idempotency receipt for one operation.
func (s *Service) buildReceipt(
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation, callerKey string,
	content receipt.Content,
	at time.Time,
	executionID id.ExecutionID,
	snapshotHash *[32]byte,
) (receipt.Record, error) {
	key := receipt.Key{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		PrincipalID: principal,
		Operation:   operation,
		CallerKey:   callerKey,
	}
	if err := key.Validate(); err != nil {
		return receipt.Record{}, err
	}
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		return receipt.Record{}, err
	}
	receiptID, err := s.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, err
	}
	return receipt.Record{
		ID:           receiptID,
		Key:          key,
		ContentHash:  fingerprint,
		ExecutionID:  executionID,
		SnapshotHash: snapshotHash,
		At:           at,
	}, nil
}

// buildAudit builds the single immutable audit row committed with the change.
func (s *Service) buildAudit(scope tenancy.Scope, actor id.PrincipalID, operation string, at time.Time) (event.Audit, error) {
	eventID, err := s.ids.NewEventID()
	if err != nil {
		return event.Audit{}, err
	}
	return event.Audit{ID: eventID, Scope: scope, Actor: actor, Operation: operation, At: at}, nil
}

// buildEvent builds one event publication intent.
func (s *Service) buildEvent(
	eventType event.Type,
	scope tenancy.Scope,
	subject string,
	subjectVersion uint64,
	actor id.PrincipalID,
	at time.Time,
) (event.Envelope, error) {
	eventID, err := s.ids.NewEventID()
	if err != nil {
		return event.Envelope{}, err
	}
	envelope := event.Envelope{
		ID:             eventID,
		Type:           eventType,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: actor},
		At:             at,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: subjectVersion,
	}
	if err := envelope.Validate(); err != nil {
		return event.Envelope{}, err
	}
	return envelope, nil
}

func publishEffect() event.PostCommitEffect {
	return event.PostCommitEffect{Kind: event.PostPublishEvent}
}

// startPipelineVersion resolves the pipeline revision one start commits. A
// caller-supplied envelope expectation is authoritative for a stored selection:
// it must equal the version the admission selected, otherwise
// a fresh start is a version conflict instead of an execution of a newer
// definition. Inline admissions and internal callers that omit the expectation
// use the admitted revision.
func startPipelineVersion(command StartCommand) (uint64, error) {
	expected := command.ExpectedPipelineVersion
	if expected == nil {
		return command.Admission.PipelineVersion, nil
	}
	if command.Admission.PipelineID == "" {
		return 0, fault.New(fault.InvalidInput, "an inline admission does not accept an expected pipeline version")
	}
	if *expected == 0 {
		return 0, fault.New(fault.InvalidInput, "expected pipeline version must be positive")
	}
	if *expected != command.Admission.PipelineVersion {
		return 0, fault.New(fault.VersionConflict,
			fmt.Sprintf("pipeline version %d does not match the admitted version %d", *expected, command.Admission.PipelineVersion))
	}
	return *expected, nil
}

// validateAdmission checks that the caller supplied a complete immutable
// admission result. Exactly two selection shapes are valid: a stored admission
// carries a non-empty pipeline id and a positive pipeline version; an inline
// admission carries neither. Any mismatched pair or an incomplete
// snapshot/action is a malformed request, never an empty execution.
func validateAdmission(admission pipeline.AdmissionResult) error {
	switch {
	case admission.PipelineID != "" && admission.PipelineVersion == 0:
		return fault.New(fault.InvalidInput, "execution start requires a non-zero pipeline version for a stored pipeline")
	case admission.PipelineID == "" && admission.PipelineVersion != 0:
		return fault.New(fault.InvalidInput, "execution start requires an empty pipeline version for an inline admission")
	}
	if admission.Snapshot.YAMLSHA256 == ([32]byte{}) {
		return fault.New(fault.InvalidInput, "execution start requires an admitted snapshot")
	}
	if admission.Snapshot.TaskID != "" {
		return fault.New(fault.InvalidInput, "execution start requires a task-free admitted snapshot")
	}
	for _, action := range admission.Actions {
		if err := action.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Start commits one authorized task-free pipeline start. It never carries a
// task id or task version and never re-derives admission from YAML.
func (s *Service) Start(ctx context.Context, command StartCommand) (StartResult, error) {
	if err := checkActor(command.Actor); err != nil {
		return StartResult{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return StartResult{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return StartResult{}, fault.New(fault.Forbidden, "execution start scope is outside the actor scope")
	}
	op, err := s.authorizeOperation(ctx, command.Actor, operationPipelineStart, command.Scope)
	if err != nil {
		return StartResult{}, err
	}
	if err := validateAdmission(command.Admission); err != nil {
		return StartResult{}, err
	}
	pipelineVersion, err := startPipelineVersion(command)
	if err != nil {
		return StartResult{}, err
	}
	at := s.clock.Now()
	executionID, err := s.ids.NewExecutionID()
	if err != nil {
		return StartResult{}, err
	}
	snapshotHash := command.Admission.Snapshot.YAMLSHA256
	record, err := s.buildReceipt(
		command.Scope, command.Actor.ID, string(op.Name), command.CallerKey,
		command.Content, at, executionID, &snapshotHash,
	)
	if err != nil {
		return StartResult{}, err
	}
	audit, err := s.buildAudit(command.Scope, command.Actor.ID, string(op.Name), at)
	if err != nil {
		return StartResult{}, err
	}
	envelope, err := s.buildEvent(event.TypeExecutionQueued, command.Scope, string(executionID), 1, command.Actor.ID, at)
	if err != nil {
		return StartResult{}, err
	}
	authority := authorityReads(
		op, command.Scope, command.Actor.ID,
		string(command.Admission.PipelineID), "", pipelineVersion,
	)
	if command.Admission.PipelineID == "" {
		// An inline admission pins no stored pipeline revision, so there is no
		// revision to re-read. Only that read is omitted: the membership grant,
		// operation, principal and scope checks stay in force.
		authority = withoutAuthorityCheck(authority, authorize.CheckPipelineVersion)
	}
	unit := StartUnit{
		Owner:                   command.Scope,
		PipelineID:              command.Admission.PipelineID,
		ExpectedPipelineVersion: pipelineVersion,
		Snapshot:                command.Admission.Snapshot,
		Actions:                 append([]pipeline.AdmittedAction(nil), command.Admission.Actions...),
		Initiator:               command.Initiator,
		ExecutionPrincipal:      command.ExecutionPrincipal,
		NewExecutionID:          executionID,
		At:                      at,
		Receipt:                 record,
		Audit:                   []event.Audit{audit},
		Events:                  []event.Envelope{envelope},
		Authority:               authority,
		PostCommit: []event.PostCommitEffect{
			publishEffect(),
			{Kind: event.PostDispatch},
		},
	}
	return s.starts.Start(ctx, unit)
}

// Inspect reads one scoped execution view. It never returns an execution owned
// by another tenant/project.
func (s *Service) Inspect(ctx context.Context, command InspectCommand) (View, error) {
	if err := checkActor(command.Actor); err != nil {
		return View{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return View{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return View{}, fault.New(fault.Forbidden, "execution inspect scope is outside the actor scope")
	}
	op, err := s.authorizeInspect(ctx, command.Actor, operationInspect, command.Scope, command.ExecutionID)
	if err != nil {
		return View{}, err
	}
	if err := s.confirmAuthority(ctx, op, command.Scope, command.Actor.ID); err != nil {
		return View{}, err
	}
	view, err := s.reader.Inspect(ctx, command.Scope, command.ExecutionID)
	if err != nil {
		return View{}, err
	}
	if view.Owner != command.Scope {
		return View{}, fault.New(fault.NotFound, "execution is not visible in this scope")
	}
	if view.ExecutionID != command.ExecutionID {
		return View{}, fault.New(fault.InternalFailure, "execution read returned a different execution identity")
	}
	return view, nil
}

// List reads one scoped execution page and refuses any row that escapes the
// caller's verified scope.
func (s *Service) List(ctx context.Context, command ListCommand) (Page, error) {
	if err := checkActor(command.Actor); err != nil {
		return Page{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return Page{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return Page{}, fault.New(fault.Forbidden, "execution list scope is outside the actor scope")
	}
	op, err := s.authorizeOperation(ctx, command.Actor, operationList, command.Scope)
	if err != nil {
		return Page{}, err
	}
	if err := s.confirmAuthority(ctx, op, command.Scope, command.Actor.ID); err != nil {
		return Page{}, err
	}
	page, err := s.reader.List(ctx, command.Scope, command.Filter)
	if err != nil {
		return Page{}, err
	}
	for _, view := range page.Items {
		if view.Owner != command.Scope {
			return Page{}, fault.New(fault.NotFound, "execution list returned an execution outside the caller scope")
		}
	}
	return page, nil
}

// convergedTransition returns the recorded result of a duplicate operation
// whose reduction produced no new commit.
func convergedTransition(current kernel.Execution, actionID id.ActionID) TransitionResult {
	result := TransitionResult{
		ExecutionVersion: current.Version,
		NextExecution:    current.State,
		Replayed:         true,
	}
	for _, action := range current.Actions {
		if action.ID != actionID {
			continue
		}
		result.ActionVersion = action.Version
		result.NextAction = action.State
		break
	}
	return result
}

// Cancel commits one explicit cancellation. It never fabricates stop evidence:
// the reducer requests the stop and records cleanup progress.
func (s *Service) Cancel(ctx context.Context, command CancelCommand) (TransitionResult, error) {
	if err := checkActor(command.Actor); err != nil {
		return TransitionResult{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return TransitionResult{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return TransitionResult{}, fault.New(fault.Forbidden, "execution cancel scope is outside the actor scope")
	}
	op, err := s.authorizeOperation(ctx, command.Actor, operationCancel, command.Scope)
	if err != nil {
		return TransitionResult{}, err
	}
	current, err := s.state.Load(ctx, command.Scope, command.ExecutionID)
	if err != nil {
		return TransitionResult{}, err
	}
	at := s.clock.Now()
	_, commit, err := flow.Reduce(current, flow.CancelFact{
		ExecutionID: command.ExecutionID,
		Reason:      flow.CancelRequested,
		At:          at,
	})
	if err != nil {
		return TransitionResult{}, err
	}
	if commit == nil {
		// An identical cancellation retry converges on the already-recorded
		// terminal result even though the execution version advanced.
		return convergedTransition(current, ""), nil
	}
	if current.Version != command.ExpectedExecutionVersion {
		return TransitionResult{}, fault.New(
			fault.VersionConflict,
			fmt.Sprintf("execution %q is at version %d, expected %d", command.ExecutionID, current.Version, command.ExpectedExecutionVersion),
		)
	}
	actionVersion := uint64(0)
	if commit.ActionID != "" {
		for _, action := range current.Actions {
			if action.ID == commit.ActionID {
				actionVersion = action.Version
				break
			}
		}
	}
	record, err := s.buildReceipt(
		command.Scope, command.Actor.ID, string(op.Name), command.CallerKey,
		command.Content, at, command.ExecutionID, nil,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	audit, err := s.buildAudit(command.Scope, command.Actor.ID, string(op.Name), at)
	if err != nil {
		return TransitionResult{}, err
	}
	envelope, err := s.buildEvent(
		event.TypeExecutionCancelled, command.Scope, string(command.ExecutionID),
		current.Version+1, command.Actor.ID, at,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	// The owner emits execution.cancelled; the unit appends the projected
	// cleanup progress and every cancelled action.transition.
	events, err := appendLifecycleProjection(
		[]event.Envelope{envelope},
		lifecyclePreImage(command.Scope, current),
		*commit,
		event.OwnedEvent{Type: event.TypeExecutionCancelled},
	)
	if err != nil {
		return TransitionResult{}, err
	}
	unit := TransitionUnit{
		Owner:                    command.Scope,
		ExecutionID:              command.ExecutionID,
		ExpectedExecutionVersion: current.Version,
		ExpectedActionVersion:    actionVersion,
		Commit:                   *commit,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   events,
		Authority: authorityReads(
			op, command.Scope, command.Actor.ID, string(command.ExecutionID), "", current.Version,
		),
		PostCommit: []event.PostCommitEffect{publishEffect()},
	}
	return s.transitions.Transition(ctx, unit)
}

// StageOutputs stages container output values for the exact current action.
// Staging never promotes values; only a runtime-confirmed settlement does.
func (s *Service) StageOutputs(ctx context.Context, command OutputsCommand) (TransitionResult, error) {
	if err := checkActor(command.Actor); err != nil {
		return TransitionResult{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return TransitionResult{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return TransitionResult{}, fault.New(fault.Forbidden, "execution output scope is outside the actor scope")
	}
	current, err := s.state.Load(ctx, command.Scope, command.ExecutionID)
	if err != nil {
		return TransitionResult{}, err
	}
	index := -1
	for position, action := range current.Actions {
		if action.ID == command.ActionID {
			index = position
			break
		}
	}
	if index < 0 {
		return TransitionResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not part of the execution", command.ActionID))
	}
	action := current.Actions[index]
	at := s.clock.Now()
	_, commit, err := flow.Reduce(current, flow.OutputFact{
		ExecutionID: command.ExecutionID,
		ActionID:    command.ActionID,
		Outputs:     command.Outputs,
		At:          at,
	})
	if err != nil {
		return TransitionResult{}, err
	}
	op, err := s.authorizeOutputs(
		ctx, command.Actor, operationOutputs, command.Scope,
		command.ExecutionID, command.ActionID, action.Deadline,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	if commit == nil {
		// An identical output retry converges on the already-staged or already
		// promoted values even though the execution version advanced.
		return convergedTransition(current, command.ActionID), nil
	}
	if current.Version != command.ExpectedExecutionVersion {
		return TransitionResult{}, fault.New(
			fault.VersionConflict,
			fmt.Sprintf("execution %q is at version %d, expected %d", command.ExecutionID, current.Version, command.ExpectedExecutionVersion),
		)
	}
	actionVersion := action.Version
	content := command.Content
	if len(content.Bytes) == 0 && content.MediaType == "" {
		encoded, err := json.Marshal(command.Outputs)
		if err != nil {
			return TransitionResult{}, fault.Wrap(fault.InternalFailure, "output content could not be encoded", err)
		}
		content = receipt.Content{MediaType: "application/json", Bytes: encoded}
	}
	record, err := s.buildReceipt(
		command.Scope, command.Actor.ID, string(op.Name), command.CallerKey,
		content, at, command.ExecutionID, nil,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	audit, err := s.buildAudit(command.Scope, command.Actor.ID, string(op.Name), at)
	if err != nil {
		return TransitionResult{}, err
	}
	envelope, err := s.buildEvent(
		event.TypeActionTransition, command.Scope, string(command.ExecutionID),
		current.Version+1, command.Actor.ID, at,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	// Output staging changes no durable lifecycle state, so the owner's
	// action.transition is the complete publication for this commit. The
	// projection is still derived from the same pre-image and excluded by the
	// class the unit already emits.
	events, err := appendLifecycleProjection(
		[]event.Envelope{envelope},
		lifecyclePreImage(command.Scope, current),
		*commit,
		event.OwnedEvent{Type: event.TypeActionTransition, SubjectID: string(command.ActionID)},
	)
	if err != nil {
		return TransitionResult{}, err
	}
	unit := TransitionUnit{
		Owner:                    command.Scope,
		ExecutionID:              command.ExecutionID,
		ExpectedExecutionVersion: current.Version,
		ExpectedActionVersion:    actionVersion,
		Commit:                   *commit,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   events,
		Authority: authorityReads(
			op, command.Scope, command.Actor.ID,
			string(command.ExecutionID), string(command.ActionID), current.Version,
		),
		PostCommit: []event.PostCommitEffect{publishEffect()},
	}
	return s.transitions.Transition(ctx, unit)
}
