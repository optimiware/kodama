package request

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

const (
	operationActivate authorize.OperationName = "execution.request.activate"
	operationResolve  authorize.OperationName = "execution.request.resolve"
)

// NewService validates the injected boundaries and returns the request
// operation service.
func NewService(config Config) (*Service, error) {
	switch {
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires a clock")
	case config.IDs == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires an identity source")
	case config.Authorizer == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires an authority reader")
	case config.State == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires a state reader")
	case config.Activations == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires an activation port")
	case config.Resolutions == nil:
		return nil, fault.New(fault.InternalFailure, "request service requires a resolution port")
	}
	return &Service{
		clock:       config.Clock,
		ids:         config.IDs,
		authorizer:  config.Authorizer,
		authority:   config.Authority,
		state:       config.State,
		activations: config.Activations,
		resolutions: config.Resolutions,
	}, nil
}

// checkActor verifies that the presented principal is structurally usable. A
// missing principal is unauthenticated; any other malformed principal is an
// invalid input.
func checkActor(actor authorize.Principal) error {
	if actor.ID == "" {
		return fault.New(fault.Unauthenticated, "request operation requires a verified principal")
	}
	if err := actor.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, "request operation principal is invalid", err)
	}
	return nil
}

// requireProjectScope validates a project-scoped request target.
func requireProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "request operations are project-scoped")
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
// refuses a deny.
func (s *Service) authorizeOperation(ctx context.Context, actor authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) (authorize.Operation, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, fmt.Sprintf("request operation %q is not registered", operation))
	}
	decision, err := s.authorizer.Authorize(ctx, actor, operation, scope)
	if err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return authorize.Operation{}, err
		}
		return authorize.Operation{}, fault.Wrap(fault.InternalFailure, "request operation authorization failed", err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, fmt.Sprintf("principal %q may not perform %q", actor.ID, operation))
	}
	return registered, nil
}

// actionIndex returns the position of one action id, or -1.
func actionIndex(execution kernel.Execution, actionID id.ActionID) int {
	for index, action := range execution.Actions {
		if action.ID == actionID {
			return index
		}
	}
	return -1
}

// requirePendingRequest locates one pending request action.
func requirePendingRequest(execution kernel.Execution, actionID id.ActionID) (int, kernel.Action, error) {
	index := actionIndex(execution, actionID)
	if index < 0 {
		return -1, kernel.Action{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not part of the execution", actionID))
	}
	action := execution.Actions[index]
	if action.Kind != definition.ActionKindRequest || action.Request == nil {
		return -1, kernel.Action{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not a request action", actionID))
	}
	if action.State != kernel.ActionPending {
		return -1, kernel.Action{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is %q, not pending", actionID, action.State))
	}
	return index, action, nil
}

// declaredAssignment parses the admitted assignment declaration of one action.
func declaredAssignment(action kernel.Action) (Assignment, error) {
	assignment, err := ParseAssignment(action.Request.Definition.Assignment)
	if err != nil {
		return Assignment{}, err
	}
	if !assignment.Valid() {
		return Assignment{}, fault.New(fault.InvalidInput, "admitted request assignment is invalid")
	}
	return assignment, nil
}

// buildReceipt builds the scoped idempotency receipt for one request commit.
func (s *Service) buildReceipt(
	scope tenancy.Scope,
	principal id.PrincipalID,
	operation, callerKey string,
	content receipt.Content,
	at time.Time,
	executionID id.ExecutionID,
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
	return receipt.Record{ID: receiptID, Key: key, ContentHash: fingerprint, ExecutionID: executionID, At: at}, nil
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

// confirmedStopEvidence returns the stop evidence of the latest earlier
// container action. It refuses an activation while any earlier container
// workload is not affirmatively stopped.
func confirmedStopEvidence(execution kernel.Execution, target int) (kernel.StopEvidence, error) {
	var evidence kernel.StopEvidence
	for index := range target {
		action := execution.Actions[index]
		if action.State != kernel.ActionSucceeded {
			return kernel.StopEvidence{}, fault.New(
				fault.InvalidInput,
				fmt.Sprintf("action %q cannot activate before action %q succeeds", execution.Actions[target].ID, action.ID),
			)
		}
		if action.Kind != definition.ActionKindContainer {
			continue
		}
		if !action.Stop.IsConfirmed() {
			return kernel.StopEvidence{}, fault.New(
				fault.InvalidInput,
				fmt.Sprintf("action %q cannot activate before the workload of %q is confirmed stopped", execution.Actions[target].ID, action.ID),
			)
		}
		evidence = action.Stop
	}
	return evidence, nil
}

// activationAuthority lists the current facts the adapter re-reads inside the
// activation commit: the execution is running and still at the expected
// version. Activation is an internal engine step with no public registry row,
// so the reads carry no operation/grant context. The confirmed stop barrier
// travels separately in StopEvidence.
func activationAuthority(command ActivateCommand) []authorize.AuthorityRead {
	actor := command.Caller.principal()
	return []authorize.AuthorityRead{
		{
			Check:       authorize.CheckExecutionRunning,
			Scope:       command.Scope,
			PrincipalID: actor,
			TargetID:    string(command.ExecutionID),
		},
		{
			Check:           authorize.CheckExecutionVersion,
			Scope:           command.Scope,
			PrincipalID:     actor,
			TargetID:        string(command.ExecutionID),
			ExpectedVersion: command.ExpectedExecutionVersion,
		},
	}
}

// Activate builds one request-activation commit unit after every earlier
// container workload is confirmed stopped. It never activates an unanswerable
// request. Only the internal engine caller is accepted; no public principal can
// invoke it. The immutable subject and deadline come from the admitted action
// through flow.Reduce, never from caller-supplied fields.
func (s *Service) Activate(ctx context.Context, command ActivateCommand) (ActivationResult, error) {
	if !command.Caller.valid() {
		return ActivationResult{}, fault.New(fault.Forbidden, "request activation requires the internal engine caller")
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return ActivationResult{}, err
	}
	current, err := s.state.Load(ctx, command.Scope, command.ExecutionID)
	if err != nil {
		return ActivationResult{}, err
	}
	if current.Version != command.ExpectedExecutionVersion {
		return ActivationResult{}, fault.New(
			fault.VersionConflict,
			fmt.Sprintf("execution %q is at version %d, expected %d", command.ExecutionID, current.Version, command.ExpectedExecutionVersion),
		)
	}
	index, action, err := requirePendingRequest(current, command.ActionID)
	if err != nil {
		return ActivationResult{}, err
	}
	if current.State.Terminal() {
		return ActivationResult{}, fault.New(fault.InvalidInput, "a terminal execution cannot activate request work")
	}
	for _, candidate := range current.Actions {
		if candidate.State == kernel.ActionRunning {
			return ActivationResult{}, fault.New(
				fault.InvalidInput,
				fmt.Sprintf("action %q is already running", candidate.ID),
			)
		}
	}
	if action.Version != command.ExpectedActionVersion {
		return ActivationResult{}, fault.New(
			fault.VersionConflict,
			fmt.Sprintf("action %q is at version %d, expected %d", command.ActionID, action.Version, command.ExpectedActionVersion),
		)
	}
	if !command.Assignment.Valid() {
		return ActivationResult{}, fault.New(fault.InvalidInput, "request activation assignment is invalid")
	}
	admitted, err := declaredAssignment(action)
	if err != nil {
		return ActivationResult{}, err
	}
	if admitted != command.Assignment {
		return ActivationResult{}, fault.New(
			fault.InvalidInput,
			fmt.Sprintf("request activation assignment %q does not match the admitted %q", command.Assignment, admitted),
		)
	}
	if action.Timeout <= 0 {
		return ActivationResult{}, fault.New(fault.InvalidInput, "admitted request timeout must be positive")
	}
	stop, err := confirmedStopEvidence(current, index)
	if err != nil {
		return ActivationResult{}, err
	}
	at := s.clock.Now()
	_, activation, err := flow.Reduce(current, flow.ActivateFact{
		ExecutionID: command.ExecutionID,
		ActionID:    command.ActionID,
		At:          at,
	})
	if err != nil {
		return ActivationResult{}, err
	}
	if activation == nil || activation.Subject == nil || activation.NextExecutionState != kernel.ExecutionRunning {
		return ActivationResult{}, fault.New(fault.InvalidInput, "request activation cannot resolve an immutable subject")
	}
	subject := *activation.Subject
	deadline := activation.Deadline
	if subject.Len() == 0 {
		return ActivationResult{}, fault.New(fault.InvalidInput, "request activation requires a resolved subject")
	}
	// Caller-supplied subject/timeout are assertions only. A mismatch is
	// refused rather than trusted; the unit always carries the reducer values.
	if command.Subject == nil || !reflect.DeepEqual(command.Subject.Fields(), subject.Fields()) {
		return ActivationResult{}, fault.New(fault.InvalidInput, "request activation subject does not match the admitted action")
	}
	if command.Timeout != action.Timeout {
		return ActivationResult{}, fault.New(
			fault.InvalidInput,
			fmt.Sprintf("request activation timeout %v does not match the admitted %v", command.Timeout, action.Timeout),
		)
	}
	actor := command.Caller.principal()
	record, err := s.buildReceipt(
		command.Scope, actor, string(operationActivate), command.CallerKey,
		command.Content, at, command.ExecutionID,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	audit, err := s.buildAudit(command.Scope, actor, string(operationActivate), at)
	if err != nil {
		return ActivationResult{}, err
	}
	envelope, err := s.buildEvent(
		event.TypeActionRequested, command.Scope, string(command.ExecutionID),
		current.Version+1, actor, at,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	// The owner emits action.requested; the unit appends the projected
	// pending->running action.transition and carries the Kernel reduction it
	// projects.
	events, err := appendLifecycleProjection(
		[]event.Envelope{envelope},
		lifecyclePreImage(command.Scope, current),
		*activation,
		event.OwnedEvent{Type: event.TypeActionRequested},
	)
	if err != nil {
		return ActivationResult{}, err
	}
	credential := token.Credential{}
	unit := ActivationUnit{
		Owner:                    command.Scope,
		ExecutionID:              command.ExecutionID,
		ActionID:                 command.ActionID,
		ExpectedExecutionVersion: current.Version,
		ExpectedActionVersion:    action.Version,
		Commit:                   *activation,
		Subject:                  subject,
		Assignment:               admitted,
		Timeout:                  action.Timeout,
		Deadline:                 deadline,
		At:                       at,
		StopEvidence:             stop,
		CredentialUpdate:         &credential,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   events,
		Authority:                activationAuthority(command),
		PostCommit:               []event.PostCommitEffect{publishEffect()},
	}
	return s.activations.Activate(ctx, unit)
}

// resolveReceiptContent is the retry-identity content of one request answer.
// It deliberately excludes expected versions and actor/time metadata so an
// identical authorized retry reproduces the same fingerprint after settlement.
type resolveReceiptContent struct {
	Decision string         `json:"decision"`
	Response kernel.Outputs `json:"response"`
}

// assignmentAuthority builds the in-commit respondent-kind/assignment read.
// It is the only authority that remains applicable to an already-answered
// action: the receipt is the durable decision, and re-reading the historical
// execution version or deadline would make an authorized replay impossible.
func assignmentAuthority(
	op authorize.Operation,
	command ResolveCommand,
	assignment Assignment,
) authorize.AuthorityRead {
	if assignment.Kind == AssignmentRole {
		return authorize.AuthorityRead{
			Operation:   op.Name,
			Required:    authorize.Requires{HumanRoles: []tenancy.Role{assignment.Role}},
			Check:       authorize.CheckMembership,
			Scope:       command.Scope,
			PrincipalID: command.Actor.ID,
			TargetID:    string(command.ActionID),
		}
	}
	return authorize.AuthorityRead{
		Operation:   op.Name,
		Required:    op.Requires,
		Check:       authorize.CheckServiceEnabled,
		Scope:       command.Scope,
		PrincipalID: command.Actor.ID,
		TargetID:    string(assignment.ServiceID),
	}
}

// resolveAuthority builds the in-commit assignment authority plus, for a fresh
// decision, the registry version/deadline reads. A replay carries only the
// assignment authority because those historical expectations no longer apply.
func resolveAuthority(
	op authorize.Operation,
	command ResolveCommand,
	assignment Assignment,
	replay bool,
) []authorize.AuthorityRead {
	reads := []authorize.AuthorityRead{assignmentAuthority(op, command, assignment)}
	if replay {
		return reads
	}
	for _, check := range op.Authority {
		switch check {
		case authorize.CheckMembership:
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   op.Name,
			Required:    op.Requires,
			Check:       check,
			Scope:       command.Scope,
			PrincipalID: command.Actor.ID,
		}
		switch check {
		case authorize.CheckExecutionVersion:
			read.TargetID = string(command.ExecutionID)
			read.ExpectedVersion = command.ExpectedExecutionVersion
		case authorize.CheckActionDeadline:
			read.TargetID = string(command.ActionID)
		}
		reads = append(reads, read)
	}
	return reads
}

// requireCurrentAssignmentAuthority verifies the respondent kind and re-reads
// the exact current assignment fact at the commit boundary.
func (s *Service) requireCurrentAssignmentAuthority(
	ctx context.Context,
	command ResolveCommand,
	assignment Assignment,
	op authorize.Operation,
	replay bool,
) error {
	switch assignment.Kind {
	case AssignmentRole:
		if command.Actor.Kind != identity.KindHuman {
			return fault.New(fault.Forbidden, "a role assignment requires a human respondent")
		}
	case AssignmentService:
		if command.Actor.Kind != identity.KindService {
			return fault.New(fault.Forbidden, "a service assignment requires a service respondent")
		}
		if string(command.Actor.ID) != string(assignment.ServiceID) {
			return fault.New(fault.Forbidden, "the respondent is not the assigned service")
		}
	default:
		return fault.New(fault.InvalidInput, "request assignment is invalid")
	}
	reads := resolveAuthority(op, command, assignment, replay)
	facts, err := s.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

// Resolve builds one authorized request-resolution commit unit. Decision,
// actor and assignment never come from the workload-controlled response.
func (s *Service) Resolve(ctx context.Context, command ResolveCommand) (ResolveResult, error) {
	if err := checkActor(command.Actor); err != nil {
		return ResolveResult{}, err
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return ResolveResult{}, err
	}
	if !scopeContains(command.Actor.Scope, command.Scope) {
		return ResolveResult{}, fault.New(fault.Forbidden, "request resolution scope is outside the actor scope")
	}
	if !command.Decision.Valid() {
		return ResolveResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("request decision %q is not supported", command.Decision))
	}
	op, err := s.authorizeOperation(ctx, command.Actor, operationResolve, command.Scope)
	if err != nil {
		return ResolveResult{}, err
	}
	current, err := s.state.Load(ctx, command.Scope, command.ExecutionID)
	if err != nil {
		return ResolveResult{}, err
	}
	index := actionIndex(current, command.ActionID)
	if index < 0 {
		return ResolveResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not part of the execution", command.ActionID))
	}
	action := current.Actions[index]
	if action.Kind != definition.ActionKindRequest || action.Request == nil {
		return ResolveResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not a request action", command.ActionID))
	}
	// An answered action is a retry, not a new decision. Settlement advances
	// both versions, so a client resending its original command still carries
	// the pre-settlement expectations. Skip the up-front comparisons only for
	// that answered case and pass the original expectations to the
	// receipt-first port, which replays equal content or refuses changed
	// content. The current assignment authority and schema are still checked
	// below; an unanswered action keeps the strict version gate.
	answered := action.Request.Answer != nil
	if !answered {
		// The body is action-only; the shared envelope may still carry the
		// expected execution version. When it does, it is the execution guard:
		// a mismatch refuses before any commit. When it is omitted, the exact
		// versions read here become the internal commit expectations, so the
		// store's compare-and-set still refuses a concurrent advance.
		if command.ExpectedExecutionVersion != 0 && current.Version != command.ExpectedExecutionVersion {
			return ResolveResult{}, fault.New(
				fault.VersionConflict,
				fmt.Sprintf("execution %q is at version %d, expected %d", command.ExecutionID, current.Version, command.ExpectedExecutionVersion),
			)
		}
		if action.Version != command.ExpectedActionVersion {
			return ResolveResult{}, fault.New(
				fault.VersionConflict,
				fmt.Sprintf("action %q is at version %d, expected %d", command.ActionID, action.Version, command.ExpectedActionVersion),
			)
		}
		if current.State != kernel.ExecutionRunning || current.CurrentAction != index || action.State != kernel.ActionRunning {
			return ResolveResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("action %q is not the running request action", command.ActionID))
		}
	}
	// Derive the internal commit versions from the observed state. A fresh
	// decision already matched the explicit action expectation and, when
	// supplied, the envelope execution guard; an answered replay uses the
	// settled versions only for its receipt-first validation.
	command.ExpectedExecutionVersion = current.Version
	command.ExpectedActionVersion = action.Version
	if action.Request.Subject == nil || action.Request.Subject.Len() == 0 {
		return ResolveResult{}, fault.New(fault.InvalidInput, "request action has no resolved subject")
	}
	assignment, err := declaredAssignment(action)
	if err != nil {
		return ResolveResult{}, err
	}
	schema := ResponseSchema{Keys: append([]id.OutputKey(nil), action.Request.Definition.Outputs...)}
	if err := schema.Validate(command.Response); err != nil {
		return ResolveResult{}, err
	}
	at := s.clock.Now()
	if !answered && !at.Before(action.Deadline) {
		return ResolveResult{}, fault.New(fault.DeadlineExpired, fmt.Sprintf("action %q is at or past its deadline", command.ActionID))
	}
	if err := s.requireCurrentAssignmentAuthority(ctx, command, assignment, op, answered); err != nil {
		return ResolveResult{}, err
	}
	kernelDecision, ok := command.Decision.KernelDecision()
	if !ok {
		return ResolveResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("request decision %q is not supported", command.Decision))
	}
	// The Kernel reduction is the only source of the settled action/execution
	// state. On a first settlement it is attached to the unit so the adapter
	// persists exactly what the reducer produced; an equal-content retry of an
	// already-answered action yields no reduction, so the replay unit carries a
	// zero Commit and the adapter settles it receipt-first.
	_, reduction, err := flow.Reduce(current, flow.AnswerFact{
		ExecutionID: command.ExecutionID,
		ActionID:    command.ActionID,
		Answer: kernel.Answer{
			Decision: kernelDecision,
			Outputs:  command.Response,
			Actor:    command.Actor.ID,
			At:       at,
		},
		At: at,
	})
	if err != nil {
		return ResolveResult{}, err
	}
	var commit flow.Commit
	if reduction != nil {
		commit = *reduction
	}
	encoded, err := json.Marshal(resolveReceiptContent{Decision: string(command.Decision), Response: command.Response})
	if err != nil {
		return ResolveResult{}, fault.Wrap(fault.InternalFailure, "request receipt content could not be encoded", err)
	}
	idempotencyKey := fmt.Sprintf("%s/%s", command.ExecutionID, command.ActionID)
	record, err := s.buildReceipt(
		command.Scope, command.Actor.ID, string(op.Name), idempotencyKey,
		receipt.Content{MediaType: "application/json", Bytes: encoded}, at, command.ExecutionID,
	)
	if err != nil {
		return ResolveResult{}, err
	}
	audit, err := s.buildAudit(command.Scope, command.Actor.ID, string(op.Name), at)
	if err != nil {
		return ResolveResult{}, err
	}
	envelope, err := s.buildEvent(
		event.TypeRequestResolved, command.Scope, string(command.ExecutionID),
		action.Version+1, command.Actor.ID, at,
	)
	if err != nil {
		return ResolveResult{}, err
	}
	// The owner emits request.resolved; a first settlement appends the projected
	// execution/action transitions of the answer reduction. A receipt-first
	// replay carries a zero commit and appends nothing.
	events := []event.Envelope{envelope}
	if reduction != nil {
		events, err = appendLifecycleProjection(
			events,
			lifecyclePreImage(command.Scope, current),
			*reduction,
			event.OwnedEvent{Type: event.TypeRequestResolved, SubjectID: string(command.ExecutionID)},
		)
		if err != nil {
			return ResolveResult{}, err
		}
	}
	unit := ResolveUnit{
		Owner:                    command.Scope,
		ExecutionID:              command.ExecutionID,
		ActionID:                 command.ActionID,
		ExpectedExecutionVersion: command.ExpectedExecutionVersion,
		ExpectedActionVersion:    command.ExpectedActionVersion,
		Decision:                 command.Decision,
		Response:                 command.Response,
		Actor:                    command.Actor.ID,
		Assignment:               assignment,
		At:                       at,
		IdempotencyKey:           idempotencyKey,
		Commit:                   commit,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   events,
		Authority:                resolveAuthority(op, command, assignment, answered),
		PostCommit: []event.PostCommitEffect{
			publishEffect(),
			{Kind: event.PostRefreshCredential},
		},
	}
	if answered {
		if err := unit.ValidateReplay(); err != nil {
			return ResolveResult{}, err
		}
	} else if err := unit.Validate(); err != nil {
		return ResolveResult{}, err
	}
	return s.resolutions.Resolve(ctx, unit)
}
