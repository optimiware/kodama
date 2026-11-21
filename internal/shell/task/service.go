package task

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/flow"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/receipt"
	"github.com/optimiware/kodema/internal/shell/request"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/token"
)

// maxRestartReasonBytes bounds the optional restart reason text, matching
// api.MaxSubjectValueBytes so the recorded failure detail always passes public
// inspection validation.
const maxRestartReasonBytes = 4096

// Service orchestrates authorized task operations. Each mutation builds one
// complete commit unit: current membership and the initiator's current start
// authority are confirmed before the port, while versioned expectations travel
// in the unit so the port re-checks them at its atomic boundary and a
// lost-response retry reaches the receipt-first replay instead of failing
// early.
type Service struct {
	clock          Clock
	ids            IDSource
	authorizer     authorize.Authorizer
	authority      authorize.AuthorityReader
	principals     identity.PrincipalReader
	pipelines      PipelineReader
	reader         Reader
	executions     ExecutionReader
	aggregates     ExecutionAggregateReader
	admission      Admission
	creates        CreatePort
	edits          EditPort
	starts         StartPort
	asks           AskPort
	replies        ReplyPort
	cancels        CancelPort
	completes      CompletePort
	progress       ProgressPort
	defaultTimeout time.Duration
}

// NewService validates the task boundary dependencies.
func NewService(config ServiceConfig) (*Service, error) {
	if config.Clock == nil || config.IDs == nil || config.Authorizer == nil ||
		config.Authority == nil || config.Principals == nil || config.Pipelines == nil ||
		config.Reader == nil || config.Executions == nil || config.Aggregates == nil ||
		config.Admission == nil ||
		config.Creates == nil || config.Edits == nil || config.Starts == nil ||
		config.Asks == nil || config.Replies == nil || config.Cancels == nil ||
		config.Completes == nil || config.Progress == nil || config.DefaultTimeout <= 0 {
		return nil, fault.New(fault.InternalFailure, "task service requires clock, ids, authorizer, authority, principals, pipelines, reader, executions, aggregates, admission, every commit port and a positive default timeout")
	}
	return &Service{
		clock:          config.Clock,
		ids:            config.IDs,
		authorizer:     config.Authorizer,
		authority:      config.Authority,
		principals:     config.Principals,
		pipelines:      config.Pipelines,
		reader:         config.Reader,
		executions:     config.Executions,
		aggregates:     config.Aggregates,
		admission:      config.Admission,
		creates:        config.Creates,
		edits:          config.Edits,
		starts:         config.Starts,
		asks:           config.Asks,
		replies:        config.Replies,
		cancels:        config.Cancels,
		completes:      config.Completes,
		progress:       config.Progress,
		defaultTimeout: config.DefaultTimeout,
	}, nil
}

// Create authorizes and commits one task creation. A plain create starts
// nothing; Start asks for creation plus admission in the same commit unit.
func (service *Service) Create(ctx context.Context, command CreateCommand) (CreateResult, error) {
	if err := validateProjectScope(command.Scope); err != nil {
		return CreateResult{}, err
	}
	operation := authorize.OperationName("task.create")
	if command.Start {
		operation = "task.create_and_start"
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return CreateResult{}, err
	}
	if _, err := service.confirmActor(ctx, command.Actor, operation); err != nil {
		return CreateResult{}, err
	}
	if command.ParentID != "" {
		if _, err := id.ParseTaskID(string(command.ParentID)); err != nil {
			return CreateResult{}, fault.Wrap(fault.InvalidInput, "parent_id is invalid", err)
		}
	}
	if err := validateTitle(command.Title); err != nil {
		return CreateResult{}, err
	}
	metadata, err := parseMetadata(command.Metadata)
	if err != nil {
		return CreateResult{}, err
	}
	if command.Start && command.PipelineID == "" {
		return CreateResult{}, fault.New(fault.InvalidInput, "create_and_start requires a selected pipeline")
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return CreateResult{}, err
	}
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	if err := service.confirm(ctx, []authorize.AuthorityRead{membership}); err != nil {
		return CreateResult{}, err
	}
	if command.ParentID != "" {
		parent, err := service.reader.Get(ctx, command.Scope, command.ParentID)
		if err != nil {
			return CreateResult{}, err
		}
		if parent.Scope != command.Scope {
			return CreateResult{}, fault.New(fault.NotFound, "parent task is not present in this scope")
		}
	}
	// A plain create admits nothing, so it never reads the pipeline through
	// admission. No foreign key backs tasks.pipeline_id, so the scoped read is
	// what refuses a selection that is unknown or belongs to another project.
	var missingPipeline error
	if command.PipelineID != "" && !command.Start {
		if _, err := service.pipelines.Get(ctx, command.Scope, command.PipelineID); err != nil {
			if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
				return CreateResult{}, err
			}
			// The pipeline may be gone because it was deleted after this exact
			// create committed and its response was lost. The receipt-first
			// create port still has to observe the unit so the retry converges
			// on the original result; the port pins the pipeline row, so a
			// fresh create cannot commit against the missing selection.
			missingPipeline = err
		}
	}
	taskID, err := service.ids.NewTaskID()
	if err != nil {
		return CreateResult{}, err
	}
	if command.ParentID != "" && command.ParentID == taskID {
		return CreateResult{}, fault.New(fault.InvalidInput, "task cannot be its own parent")
	}
	now := service.clock.Now()
	created, err := service.newTaskEvent(event.TypeTaskCreated, command.Actor.ID, now, command.Scope, string(taskID), 1, command.ParentID, "")
	if err != nil {
		return CreateResult{}, err
	}
	unit := CreateUnit{
		Owner:            command.Scope,
		TaskID:           taskID,
		ParentID:         command.ParentID,
		Title:            command.Title,
		Description:      command.Description,
		Metadata:         metadata,
		SelectedPipeline: command.PipelineID,
		Inputs:           cloneInputs(command.Inputs),
		Initiator:        command.Actor.ID,
		At:               now,
		Events:           []event.Envelope{created},
		Authority:        []authorize.AuthorityRead{membership},
		PostCommit:       []event.PostCommitEffect{{Kind: event.PostPublishEvent}},
	}
	var executionID id.ExecutionID
	var snapshotHash *[32]byte
	if command.Start {
		executionPrincipal, err := service.ids.NewPrincipalID()
		if err != nil {
			return CreateResult{}, err
		}
		newExecutionID, err := service.ids.NewExecutionID()
		if err != nil {
			return CreateResult{}, err
		}
		admitted, err := service.admission.Admit(ctx, pipeline.AdmitRequest{
			Scope:      command.Scope,
			Definition: pipeline.Definition{ID: command.PipelineID},
			Inputs:     cloneInputs(command.Inputs),
			Task: pipeline.TaskContext{
				TaskID:      taskID,
				ParentID:    command.ParentID,
				Title:       command.Title,
				Description: command.Description,
				// A brand-new task has zero messages, so its admitted
				// conversation is the present JSON empty array (never nil): a
				// task-bound execution must still materialize /kodema/task.json.
				Conversation: []byte(emptyConversationJSON),
				Metadata:     cloneBytes(command.Metadata),
			},
			CompletesTask:  cloneOptionalBool(command.CompletesTask),
			DefaultTimeout: service.defaultTimeout,
		})
		if err != nil {
			return CreateResult{}, err
		}
		unit.Start = &StartUnit{
			Owner:                    command.Scope,
			TaskID:                   taskID,
			PipelineID:               admitted.PipelineID,
			ExpectedLifecycleVersion: 0,
			ExpectedPipelineVersion:  admitted.PipelineVersion,
			Snapshot:                 admitted.Snapshot,
			Actions:                  cloneActions(admitted.Actions),
			Initiator:                command.Actor.ID,
			ExecutionPrincipal:       executionPrincipal,
			NewExecutionID:           newExecutionID,
			At:                       now,
		}
		unit.Authority = append(unit.Authority, newAuthorityRead(operation, requires, authorize.CheckPipelineVersion, command.Scope, command.Actor.ID, string(admitted.PipelineID), admitted.PipelineVersion))
		executionID = newExecutionID
		hash := admitted.Snapshot.YAMLSHA256
		snapshotHash = &hash
		queued, err := service.newEvent(event.TypeExecutionQueued, command.Actor.ID, now, command.Scope, string(newExecutionID), 1)
		if err != nil {
			return CreateResult{}, err
		}
		unit.Events = append(unit.Events, queued)
		unit.PostCommit = append(unit.PostCommit, event.PostCommitEffect{Kind: event.PostDispatch, Target: string(newExecutionID)})
	}
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, executionID, snapshotHash)
	if err != nil {
		return CreateResult{}, err
	}
	unit.Receipt = record
	unit.Audit = []event.Audit{audit}
	result, err := service.creates.Create(ctx, unit)
	if err != nil && missingPipeline != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			return CreateResult{}, missingPipeline
		}
	}
	return result, err
}

// Edit authorizes and commits one content, comment or explicit selection edit.
func (service *Service) Edit(ctx context.Context, command EditCommand) (EditResult, error) {
	operation, err := editOperation(command.Kind)
	if err != nil {
		return EditResult{}, err
	}
	if err := validateProjectScope(command.Scope); err != nil {
		return EditResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return EditResult{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return EditResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return EditResult{}, err
	}
	if current.Scope != command.Scope {
		return EditResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	if err := confirmExecutionBinding(actor, current); err != nil {
		return EditResult{}, err
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return EditResult{}, err
	}
	now := service.clock.Now()
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	unit := EditUnit{
		Owner:     command.Scope,
		TaskID:    command.TaskID,
		Kind:      command.Kind,
		Authority: []authorize.AuthorityRead{membership},
	}
	switch command.Kind {
	case EditContent:
		if err := validateTitle(command.Title); err != nil {
			return EditResult{}, err
		}
		metadata, err := parseMetadata(command.Metadata)
		if err != nil {
			return EditResult{}, err
		}
		unit.ExpectedContentVersion = command.ExpectedContentVersion
		unit.Title = command.Title
		unit.Description = command.Description
		unit.Metadata = metadata
		unit.Authority = append(unit.Authority, newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", 0))
	case EditComment:
		if command.Body == "" {
			return EditResult{}, fault.New(fault.InvalidInput, "comment body is empty")
		}
		unit.ExpectedContentVersion = command.ExpectedContentVersion
		unit.Message = &Message{
			Author:     command.Actor.ID,
			At:         now,
			Body:       command.Body,
			References: cloneStrings(command.References),
		}
		unit.Authority = append(unit.Authority, newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", 0))
	case EditSelectPipeline:
		if command.PipelineID == "" {
			return EditResult{}, fault.New(fault.InvalidInput, "select_pipeline requires a pipeline id")
		}
		definition, err := service.pipelines.Get(ctx, command.Scope, command.PipelineID)
		if err != nil {
			return EditResult{}, err
		}
		unit.ExpectedLifecycleVersion = command.ExpectedLifecycleVersion
		unit.PipelineID = command.PipelineID
		unit.Inputs = cloneInputs(command.Inputs)
		unit.StartAuthority = command.Actor.ID
		unit.ExpectedPipelineVersion = definition.Version
		unit.Authority = append(unit.Authority,
			newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
			newAuthorityRead(operation, requires, authorize.CheckPipelineVersion, command.Scope, command.Actor.ID, string(command.PipelineID), definition.Version),
		)
	default:
		return EditResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown edit kind %q", command.Kind))
	}
	if err := service.confirm(ctx, nonVersionedReads(unit.Authority)); err != nil {
		return EditResult{}, err
	}
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, "", nil)
	if err != nil {
		return EditResult{}, err
	}
	unit.Receipt = record
	unit.Audit = []event.Audit{audit}
	return service.edits.Edit(ctx, unit)
}

// Start authorizes and commits one task start, optionally switching the task's
// selected pipeline and/or inputs atomically in the same transaction.
func (service *Service) Start(ctx context.Context, command StartCommand) (StartResult, error) {
	const operation authorize.OperationName = "task.start"
	const selectOperation authorize.OperationName = "task.select_pipeline"
	if err := validateProjectScope(command.Scope); err != nil {
		return StartResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return StartResult{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return StartResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return StartResult{}, err
	}
	if current.Scope != command.Scope {
		return StartResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	pipelineChanged := command.PipelineID != "" && command.PipelineID != current.SelectedPipeline
	inputsProvided := command.Inputs != nil
	changingSelection := pipelineChanged || inputsProvided
	if changingSelection {
		if err := service.authorize(ctx, command.Actor, selectOperation, command.Scope); err != nil {
			return StartResult{}, err
		}
		if _, err := service.confirmActor(ctx, command.Actor, selectOperation); err != nil {
			return StartResult{}, err
		}
	}
	switch current.State {
	case StateOpen:
	case StateDone:
		// A completed stage may transition atomically to a different pipeline
		// stage when authorized with pipeline-selection authority; an ordinary
		// start without a pipeline switch keeps done terminal.
		if !pipelineChanged {
			return StartResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit start", current.State))
		}
	default:
		return StartResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit start", current.State))
	}
	targetPipeline := current.SelectedPipeline
	if command.PipelineID != "" {
		targetPipeline = command.PipelineID
	}
	if targetPipeline == "" {
		return StartResult{}, fault.New(fault.InvalidInput, "task has no selected pipeline")
	}
	targetInputs := cloneInputs(current.Inputs)
	if inputsProvided {
		targetInputs = cloneInputs(command.Inputs)
	}
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return StartResult{}, fault.New(fault.InternalFailure, fmt.Sprintf("operation %q is not registered", operation))
	}
	requires := registered.Requires
	conditional := authorize.ConditionalGrantChecks(registered, actor)
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	preConfirm := []authorize.AuthorityRead{membership}
	var selectMembership *authorize.AuthorityRead
	if changingSelection {
		selectRequires, err := registryRequires(selectOperation)
		if err != nil {
			return StartResult{}, err
		}
		read := newAuthorityRead(selectOperation, selectRequires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
		selectMembership = &read
		preConfirm = append(preConfirm, read)
	}
	if err := service.confirm(ctx, preConfirm); err != nil {
		return StartResult{}, err
	}
	now := service.clock.Now()
	conversation, err := conversationBytes(current.Messages)
	if err != nil {
		return StartResult{}, err
	}
	executionPrincipal, err := service.ids.NewPrincipalID()
	if err != nil {
		return StartResult{}, err
	}
	newExecutionID, err := service.ids.NewExecutionID()
	if err != nil {
		return StartResult{}, err
	}
	admitted, err := service.admission.Admit(ctx, pipeline.AdmitRequest{
		Scope:      command.Scope,
		Definition: pipeline.Definition{ID: targetPipeline},
		Inputs:     cloneInputs(targetInputs),
		Task: pipeline.TaskContext{
			TaskID:       current.ID,
			ParentID:     current.ParentID,
			Title:        current.Title,
			Description:  current.Description,
			Conversation: conversation,
			Metadata:     marshalMetadata(current.Metadata),
		},
		CompletesTask:  cloneOptionalBool(command.CompletesTask),
		DefaultTimeout: service.defaultTimeout,
	})
	if err != nil {
		return StartResult{}, err
	}
	predecessor, known, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
	if err != nil {
		return StartResult{}, err
	}
	dispatch := current.CurrentExecution == "" || (known && predecessor.StopEvidence.IsConfirmed())
	// A conditional grant is completed only by the commit-boundary read. The
	// operator retry names the observed current execution (empty when the task
	// has none) and its observed version, so the store re-reads the task state
	// and that execution atomically with the start commit and fails closed on a
	// missing or moved row.
	conditionalTarget := ""
	var conditionalVersion uint64
	if current.CurrentExecution != "" {
		conditionalTarget = string(current.CurrentExecution)
		if known {
			conditionalVersion = predecessor.Version
		}
	}
	authority := []authorize.AuthorityRead{
		membership,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
		newAuthorityRead(operation, requires, authorize.CheckPipelineVersion, command.Scope, command.Actor.ID, string(admitted.PipelineID), admitted.PipelineVersion),
	}
	if selectMembership != nil {
		authority = append(authority, *selectMembership)
	}
	for _, check := range conditional {
		authority = append(authority, newAuthorityRead(operation, requires, check, command.Scope, actor.ID, conditionalTarget, conditionalVersion))
	}
	hash := admitted.Snapshot.YAMLSHA256
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, newExecutionID, &hash)
	if err != nil {
		return StartResult{}, err
	}
	queued, err := service.newEvent(event.TypeExecutionQueued, command.Actor.ID, now, command.Scope, string(newExecutionID), 1)
	if err != nil {
		return StartResult{}, err
	}
	effects := []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	if dispatch {
		effects = append(effects, event.PostCommitEffect{Kind: event.PostDispatch, Target: string(newExecutionID)})
	}
	initiator := current.Initiator
	if changingSelection {
		initiator = command.Actor.ID
	}
	unit := StartUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		PipelineID:               admitted.PipelineID,
		ExpectedState:            current.State,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		ExpectedPipelineVersion:  admitted.PipelineVersion,
		Inputs:                   cloneInputs(command.Inputs),
		UpdateInputs:             inputsProvided,
		UpdateInitiator:          changingSelection,
		Snapshot:                 admitted.Snapshot,
		Actions:                  cloneActions(admitted.Actions),
		Initiator:                initiator,
		ExecutionPrincipal:       executionPrincipal,
		NewExecutionID:           newExecutionID,
		At:                       now,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   []event.Envelope{queued},
		Authority:                authority,
		PostCommit:               effects,
	}
	return service.starts.Start(ctx, unit)
}

// Restart atomically cancels any active nonterminal execution on an open task
// as superseded (without faking a review rejection), optionally updates the
// selected pipeline and/or inputs, and queues a fresh replacement execution
// that honors Kodema's confirmed-stop predecessor dispatch barrier.
func (service *Service) Restart(ctx context.Context, command RestartCommand) (RestartResult, error) {
	const operation authorize.OperationName = "task.restart"
	const startOperation authorize.OperationName = "task.start"
	const cancelOperation authorize.OperationName = "task.cancel"
	const selectOperation authorize.OperationName = "task.select_pipeline"
	if err := validateProjectScope(command.Scope); err != nil {
		return RestartResult{}, err
	}
	if !utf8.ValidString(command.Reason) {
		return RestartResult{}, fault.New(fault.InvalidInput, "restart reason is not valid UTF-8")
	}
	if len(command.Reason) > maxRestartReasonBytes {
		return RestartResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("restart reason is %d bytes; at most %d are allowed", len(command.Reason), maxRestartReasonBytes))
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return RestartResult{}, err
	}
	if _, err := service.confirmActor(ctx, command.Actor, operation); err != nil {
		return RestartResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, startOperation, command.Scope); err != nil {
		return RestartResult{}, err
	}
	if _, err := service.confirmActor(ctx, command.Actor, startOperation); err != nil {
		return RestartResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return RestartResult{}, err
	}
	if current.Scope != command.Scope {
		return RestartResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	if current.State != StateOpen {
		return RestartResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit restart", current.State))
	}
	pipelineChanged := command.PipelineID != "" && command.PipelineID != current.SelectedPipeline
	inputsProvided := command.Inputs != nil
	changingSelection := pipelineChanged || inputsProvided
	if changingSelection {
		if err := service.authorize(ctx, command.Actor, selectOperation, command.Scope); err != nil {
			return RestartResult{}, err
		}
		if _, err := service.confirmActor(ctx, command.Actor, selectOperation); err != nil {
			return RestartResult{}, err
		}
	}
	targetPipeline := current.SelectedPipeline
	if command.PipelineID != "" {
		targetPipeline = command.PipelineID
	}
	if targetPipeline == "" {
		return RestartResult{}, fault.New(fault.InvalidInput, "task has no selected pipeline")
	}
	targetInputs := cloneInputs(current.Inputs)
	if inputsProvided {
		targetInputs = cloneInputs(command.Inputs)
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return RestartResult{}, err
	}
	startRequires, err := registryRequires(startOperation)
	if err != nil {
		return RestartResult{}, err
	}
	now := service.clock.Now()
	predecessor, known, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
	if err != nil {
		return RestartResult{}, err
	}
	var (
		aggregate    kernel.Execution
		cancelCommit *flow.Commit
	)
	if known {
		aggregate, cancelCommit, err = service.cancelReductionWithReason(ctx, command.Scope, current.CurrentExecution, flow.CancelSuperseded, command.Reason, now)
		if err != nil {
			return RestartResult{}, err
		}
		if err := ValidateCancellationCommit(aggregate.Version, current.CurrentExecution, aggregate, cancelCommit); err != nil {
			return RestartResult{}, err
		}
		if cancelCommit != nil {
			if err := service.authorize(ctx, command.Actor, cancelOperation, command.Scope); err != nil {
				return RestartResult{}, err
			}
			if _, err := service.confirmActor(ctx, command.Actor, cancelOperation); err != nil {
				return RestartResult{}, err
			}
		}
	}
	membershipReads := []authorize.AuthorityRead{
		newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
		newAuthorityRead(startOperation, unconditionalStartRequires(startRequires), authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
	}
	if cancelCommit != nil {
		cancelRequires, err := registryRequires(cancelOperation)
		if err != nil {
			return RestartResult{}, err
		}
		membershipReads = append(membershipReads, newAuthorityRead(cancelOperation, cancelRequires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0))
	}
	if changingSelection {
		selectRequires, err := registryRequires(selectOperation)
		if err != nil {
			return RestartResult{}, err
		}
		membershipReads = append(membershipReads, newAuthorityRead(selectOperation, selectRequires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0))
	}
	if err := service.confirm(ctx, membershipReads); err != nil {
		return RestartResult{}, err
	}
	conversation, err := conversationBytes(current.Messages)
	if err != nil {
		return RestartResult{}, err
	}
	executionPrincipal, err := service.ids.NewPrincipalID()
	if err != nil {
		return RestartResult{}, err
	}
	newExecutionID, err := service.ids.NewExecutionID()
	if err != nil {
		return RestartResult{}, err
	}
	admitted, err := service.admission.Admit(ctx, pipeline.AdmitRequest{
		Scope:      command.Scope,
		Definition: pipeline.Definition{ID: targetPipeline},
		Inputs:     cloneInputs(targetInputs),
		Task: pipeline.TaskContext{
			TaskID:       current.ID,
			ParentID:     current.ParentID,
			Title:        current.Title,
			Description:  current.Description,
			Conversation: conversation,
			Metadata:     marshalMetadata(current.Metadata),
		},
		CompletesTask:  cloneOptionalBool(command.CompletesTask),
		DefaultTimeout: service.defaultTimeout,
	})
	if err != nil {
		return RestartResult{}, err
	}
	authority := append([]authorize.AuthorityRead(nil), membershipReads...)
	authority = append(authority,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
		newAuthorityRead(operation, requires, authorize.CheckPipelineVersion, command.Scope, command.Actor.ID, string(admitted.PipelineID), admitted.PipelineVersion),
	)
	if known {
		authority = append(authority, newAuthorityRead(operation, requires, authorize.CheckExecutionVersion, command.Scope, command.Actor.ID, string(current.CurrentExecution), aggregate.Version))
	}
	hash := admitted.Snapshot.YAMLSHA256
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, newExecutionID, &hash)
	if err != nil {
		return RestartResult{}, err
	}
	audit.Detail = command.Reason
	queued, err := service.newEvent(event.TypeExecutionQueued, command.Actor.ID, now, command.Scope, string(newExecutionID), 1)
	if err != nil {
		return RestartResult{}, err
	}
	events := []event.Envelope{queued}
	if cancelCommit != nil {
		events, err = appendCancellationProjection(events, command.Scope, aggregate, *cancelCommit)
		if err != nil {
			return RestartResult{}, err
		}
	}
	dispatch := current.CurrentExecution == "" || (cancelCommit == nil && known && predecessor.StopEvidence.IsConfirmed())
	effects := []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	if dispatch {
		effects = append(effects, event.PostCommitEffect{Kind: event.PostDispatch, Target: string(newExecutionID)})
	}
	initiator := current.Initiator
	if changingSelection {
		initiator = command.Actor.ID
	}
	unit := StartUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		PipelineID:               admitted.PipelineID,
		ExpectedState:            StateOpen,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		ExpectedPipelineVersion:  admitted.PipelineVersion,
		Inputs:                   cloneInputs(command.Inputs),
		UpdateInputs:             inputsProvided,
		UpdateInitiator:          changingSelection,
		Snapshot:                 admitted.Snapshot,
		Actions:                  cloneActions(admitted.Actions),
		Initiator:                initiator,
		ExecutionPrincipal:       executionPrincipal,
		NewExecutionID:           newExecutionID,
		At:                       now,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   events,
		Authority:                authority,
		PostCommit:               effects,
	}
	if known {
		unit.ExpectedExecutionVersion = aggregate.Version
		unit.CancelCommit = cancelCommit
		if cancelCommit != nil {
			unit.CredentialUpdate = &token.Credential{}
		}
	}
	committed, err := service.starts.Start(ctx, unit)
	if err != nil {
		return RestartResult{}, err
	}
	return RestartResult{
		ExecutionID:         committed.ExecutionID,
		SupersededExecution: committed.SupersededExecution,
		LifecycleVersion:    committed.LifecycleVersion,
		Replayed:            committed.Replayed,
	}, nil
}

// Ask authorizes and commits one indivisible conversation handoff.
func (service *Service) Ask(ctx context.Context, command AskCommand) (AskResult, error) {
	const operation authorize.OperationName = "task.ask"
	if err := validateProjectScope(command.Scope); err != nil {
		return AskResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return AskResult{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return AskResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return AskResult{}, err
	}
	if current.Scope != command.Scope {
		return AskResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	if err := confirmExecutionBinding(actor, current); err != nil {
		return AskResult{}, err
	}
	switch current.State {
	case StateDone, StateCancelled:
		return AskResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit ask", current.State))
	case StateOpen, StateWaiting:
		// A waiting task is a possible lost-response retry: the receipt-first
		// port decides whether this exact ask already committed.
	default:
		return AskResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit ask", current.State))
	}
	if command.Question == "" {
		return AskResult{}, fault.New(fault.InvalidInput, "question is empty")
	}
	if !command.Assignment.Valid() {
		return AskResult{}, fault.New(fault.InvalidInput, "question assignment is invalid")
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return AskResult{}, err
	}
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	if err := service.confirm(ctx, []authorize.AuthorityRead{membership}); err != nil {
		return AskResult{}, err
	}
	questionID, err := service.ids.NewQuestionID()
	if err != nil {
		return AskResult{}, err
	}
	now := service.clock.Now()
	_, known, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
	if err != nil {
		return AskResult{}, err
	}
	authority := []authorize.AuthorityRead{
		membership,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
	}
	unit := AskUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		QuestionID:               questionID,
		Question:                 command.Question,
		Assignment:               command.Assignment,
		References:               cloneStrings(command.References),
		CredentialUpdate:         &token.Credential{},
	}
	var aggregate kernel.Execution
	if known {
		aggregate, unit.Commit, err = service.cancelReduction(ctx, command.Scope, current.CurrentExecution, now)
		if err != nil {
			return AskResult{}, err
		}
		unit.ExpectedExecutionVersion = aggregate.Version
		authority = append(authority, newAuthorityRead(operation, requires, authorize.CheckExecutionVersion, command.Scope, command.Actor.ID, string(current.CurrentExecution), aggregate.Version))
		if err := ValidateCancellationCommit(unit.ExpectedExecutionVersion, current.CurrentExecution, aggregate, unit.Commit); err != nil {
			return AskResult{}, err
		}
	}
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, "", nil)
	if err != nil {
		return AskResult{}, err
	}
	questionEvent, err := service.newTaskEvent(event.TypeTaskQuestion, command.Actor.ID, now, command.Scope, string(command.TaskID), command.ExpectedLifecycleVersion+1, current.ParentID, "")
	if err != nil {
		return AskResult{}, err
	}
	events := []event.Envelope{questionEvent}
	if unit.Commit != nil {
		events, err = appendCancellationProjection(events, command.Scope, aggregate, *unit.Commit)
		if err != nil {
			return AskResult{}, err
		}
	}
	unit.Receipt = record
	unit.Audit = []event.Audit{audit}
	unit.Events = events
	unit.Authority = authority
	unit.PostCommit = []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	return service.asks.Ask(ctx, unit)
}

// Reply authorizes and commits one indivisible assigned reply with its fresh
// execution admission.
func (service *Service) Reply(ctx context.Context, command ReplyCommand) (ReplyResult, error) {
	const operation authorize.OperationName = "task.reply"
	if err := validateProjectScope(command.Scope); err != nil {
		return ReplyResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return ReplyResult{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return ReplyResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return ReplyResult{}, err
	}
	if current.Scope != command.Scope {
		return ReplyResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	var assignment request.Assignment
	switch current.State {
	case StateDone, StateCancelled:
		return ReplyResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit reply", current.State))
	case StateWaiting:
		if current.Question == nil || current.Question.ID != command.QuestionID {
			return ReplyResult{}, fault.New(fault.VersionConflict, "task has no matching outstanding question")
		}
		if current.Question.Version != command.ExpectedQuestionVersion {
			return ReplyResult{}, fault.New(fault.VersionConflict, "stale question version")
		}
		if err := enforceAssignment(actor, current.Question.Assignment); err != nil {
			return ReplyResult{}, err
		}
		assignment = current.Question.Assignment
	case StateOpen:
		// An open task is a possible lost-response retry: the receipt-first
		// port decides whether this exact reply already committed.
	default:
		return ReplyResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit reply", current.State))
	}
	if command.Answer == "" {
		return ReplyResult{}, fault.New(fault.InvalidInput, "reply answer is empty")
	}
	if current.SelectedPipeline == "" {
		return ReplyResult{}, fault.New(fault.InvalidInput, "task has no selected pipeline")
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return ReplyResult{}, err
	}
	startRead, err := service.initiatorStartRead(ctx, command.Scope, current.Initiator)
	if err != nil {
		return ReplyResult{}, err
	}
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	if err := service.confirm(ctx, []authorize.AuthorityRead{membership, startRead}); err != nil {
		return ReplyResult{}, err
	}
	now := service.clock.Now()
	messages := current.Messages
	if current.Question != nil && !messagesContainQuestion(messages, current.Question) {
		messages = insertMessageChronologically(messages, questionMessage(current.Question))
	}
	answer := &Message{Author: command.Actor.ID, At: now, Body: command.Answer}
	conversation, err := conversationBytes(messages, answer)
	if err != nil {
		return ReplyResult{}, err
	}
	executionPrincipal, err := service.ids.NewPrincipalID()
	if err != nil {
		return ReplyResult{}, err
	}
	newExecutionID, err := service.ids.NewExecutionID()
	if err != nil {
		return ReplyResult{}, err
	}
	admitted, err := service.admission.Admit(ctx, pipeline.AdmitRequest{
		Scope:      command.Scope,
		Definition: pipeline.Definition{ID: current.SelectedPipeline},
		Inputs:     cloneInputs(current.Inputs),
		Task: pipeline.TaskContext{
			TaskID:       current.ID,
			ParentID:     current.ParentID,
			Title:        current.Title,
			Description:  current.Description,
			Conversation: conversation,
			Metadata:     marshalMetadata(current.Metadata),
		},
		DefaultTimeout: service.defaultTimeout,
	})
	if err != nil {
		return ReplyResult{}, err
	}
	authority := []authorize.AuthorityRead{
		membership,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
		newAuthorityRead(operation, requires, authorize.CheckPipelineVersion, command.Scope, command.Actor.ID, string(admitted.PipelineID), admitted.PipelineVersion),
		startRead,
	}
	dispatch := current.CurrentExecution == ""
	if current.CurrentExecution != "" {
		predecessor, found, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
		if err != nil {
			return ReplyResult{}, err
		}
		dispatch = found && predecessor.StopEvidence.IsConfirmed()
		if found {
			authority = append(authority, newAuthorityRead(operation, requires, authorize.CheckExecutionVersion, command.Scope, command.Actor.ID, string(predecessor.ID), predecessor.Version))
		}
	}
	hash := admitted.Snapshot.YAMLSHA256
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, newExecutionID, &hash)
	if err != nil {
		return ReplyResult{}, err
	}
	replied, err := service.newTaskEvent(event.TypeTaskReply, command.Actor.ID, now, command.Scope, string(command.TaskID), command.ExpectedLifecycleVersion+1, current.ParentID, "")
	if err != nil {
		return ReplyResult{}, err
	}
	queued, err := service.newEvent(event.TypeExecutionQueued, command.Actor.ID, now, command.Scope, string(newExecutionID), 1)
	if err != nil {
		return ReplyResult{}, err
	}
	effects := []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	if dispatch {
		effects = append(effects, event.PostCommitEffect{Kind: event.PostDispatch, Target: string(newExecutionID)})
	}
	unit := ReplyUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		PipelineID:               admitted.PipelineID,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		QuestionID:               command.QuestionID,
		ExpectedQuestionVersion:  command.ExpectedQuestionVersion,
		Assignment:               assignment,
		Answer:                   command.Answer,
		Initiator:                current.Initiator,
		ExpectedPipelineVersion:  admitted.PipelineVersion,
		Snapshot:                 admitted.Snapshot,
		Actions:                  cloneActions(admitted.Actions),
		ExecutionPrincipal:       executionPrincipal,
		NewExecutionID:           newExecutionID,
		At:                       now,
		Receipt:                  record,
		Audit:                    []event.Audit{audit},
		Events:                   []event.Envelope{replied, queued},
		Authority:                authority,
		PostCommit:               effects,
	}
	return service.replies.Reply(ctx, unit)
}

// Cancel authorizes and commits one task cancellation.
func (service *Service) Cancel(ctx context.Context, command CancelCommand) (CancelResult, error) {
	const operation authorize.OperationName = "task.cancel"
	if err := validateProjectScope(command.Scope); err != nil {
		return CancelResult{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return CancelResult{}, err
	}
	if _, err := service.confirmActor(ctx, command.Actor, operation); err != nil {
		return CancelResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return CancelResult{}, err
	}
	if current.Scope != command.Scope {
		return CancelResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return CancelResult{}, err
	}
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	if err := service.confirm(ctx, []authorize.AuthorityRead{membership}); err != nil {
		return CancelResult{}, err
	}
	now := service.clock.Now()
	_, known, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
	if err != nil {
		return CancelResult{}, err
	}
	authority := []authorize.AuthorityRead{
		membership,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
	}
	unit := CancelUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		CredentialUpdate:         &token.Credential{},
	}
	var aggregate kernel.Execution
	if known {
		aggregate, unit.Commit, err = service.cancelReduction(ctx, command.Scope, current.CurrentExecution, now)
		if err != nil {
			return CancelResult{}, err
		}
		unit.ExpectedExecutionVersion = aggregate.Version
		authority = append(authority, newAuthorityRead(operation, requires, authorize.CheckExecutionVersion, command.Scope, command.Actor.ID, string(current.CurrentExecution), aggregate.Version))
		if err := ValidateCancellationCommit(unit.ExpectedExecutionVersion, current.CurrentExecution, aggregate, unit.Commit); err != nil {
			return CancelResult{}, err
		}
	}
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, "", nil)
	if err != nil {
		return CancelResult{}, err
	}
	cancelled, err := service.newTaskEvent(event.TypeTaskCancelled, command.Actor.ID, now, command.Scope, string(command.TaskID), command.ExpectedLifecycleVersion+1, current.ParentID, "")
	if err != nil {
		return CancelResult{}, err
	}
	events := []event.Envelope{cancelled}
	if unit.Commit != nil {
		events, err = appendCancellationProjection(events, command.Scope, aggregate, *unit.Commit)
		if err != nil {
			return CancelResult{}, err
		}
	}
	unit.Receipt = record
	unit.Audit = []event.Audit{audit}
	unit.Events = events
	unit.Authority = authority
	unit.PostCommit = []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	return service.cancels.Cancel(ctx, unit)
}

// Complete authorizes and commits an explicit transition of an open task to
// done. Waiting and cancelled tasks are refused; a task with an active
// nonterminal (queued or running) current execution is also refused so active
// workloads are never orphaned. An already-done task still reaches the
// receipt-first port so an idempotent retry converges cleanly while a fresh
// call against a done task is refused with version_conflict.
func (service *Service) Complete(ctx context.Context, command CompleteCommand) (CompleteResult, error) {
	const operation authorize.OperationName = "task.complete"
	if err := validateProjectScope(command.Scope); err != nil {
		return CompleteResult{}, err
	}
	if !utf8.ValidString(command.Reason) {
		return CompleteResult{}, fault.New(fault.InvalidInput, "complete reason is not valid UTF-8")
	}
	if len(command.Reason) > maxRestartReasonBytes {
		return CompleteResult{}, fault.New(fault.InvalidInput, fmt.Sprintf("complete reason is %d bytes; at most %d are allowed", len(command.Reason), maxRestartReasonBytes))
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return CompleteResult{}, err
	}
	if _, err := service.confirmActor(ctx, command.Actor, operation); err != nil {
		return CompleteResult{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return CompleteResult{}, err
	}
	if current.Scope != command.Scope {
		return CompleteResult{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	switch current.State {
	case StateOpen, StateDone:
		// A done task may be a lost-response retry of this exact complete call:
		// let the receipt-first port replay an equal retry or refuse a fresh
		// call with version_conflict.
	default:
		return CompleteResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit complete", current.State))
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return CompleteResult{}, err
	}
	membership := newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0)
	if err := service.confirm(ctx, []authorize.AuthorityRead{membership}); err != nil {
		return CompleteResult{}, err
	}
	now := service.clock.Now()
	predecessor, known, err := service.predecessor(ctx, command.Scope, current.CurrentExecution)
	if err != nil {
		return CompleteResult{}, err
	}
	if current.State == StateOpen && known && !predecessor.State.Terminal() {
		return CompleteResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("cannot complete task while current execution %q is %q", predecessor.ID, predecessor.State))
	}
	authority := []authorize.AuthorityRead{
		membership,
		newAuthorityRead(operation, requires, authorize.CheckTaskVersion, command.Scope, command.Actor.ID, "", command.ExpectedLifecycleVersion),
	}
	unit := CompleteUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		ExpectedLifecycleVersion: command.ExpectedLifecycleVersion,
		Reason:                   command.Reason,
	}
	if known {
		unit.ExpectedExecutionVersion = predecessor.Version
		authority = append(authority, newAuthorityRead(operation, requires, authorize.CheckExecutionVersion, command.Scope, command.Actor.ID, string(current.CurrentExecution), predecessor.Version))
	}
	record, audit, err := service.operationRecord(command.Scope, command.Actor.ID, string(operation), command.CallerKey, command.Content, now, "", nil)
	if err != nil {
		return CompleteResult{}, err
	}
	audit.Detail = command.Reason
	completed, err := service.newTaskEvent(event.TypeTaskCompleted, command.Actor.ID, now, command.Scope, string(command.TaskID), command.ExpectedLifecycleVersion+1, current.ParentID, command.Reason)
	if err != nil {
		return CompleteResult{}, err
	}
	unit.Receipt = record
	unit.Audit = []event.Audit{audit}
	unit.Events = []event.Envelope{completed}
	unit.Authority = authority
	unit.PostCommit = []event.PostCommitEffect{{Kind: event.PostPublishEvent}}
	return service.completes.Complete(ctx, unit)
}

// Progress applies one current-only execution observation. Only a success of
// the task's current execution while the task is open completes it (when the
// execution's admitted snapshot has completes_task enabled, the default); every
// other observation is reported without a write.
func (service *Service) Progress(ctx context.Context, command ProgressCommand) (ProgressStatus, error) {
	if err := validateProjectScope(command.Scope); err != nil {
		return ProgressStatus{}, err
	}
	if command.TaskID == "" {
		return ProgressStatus{}, fault.New(fault.InvalidInput, "progress requires a task id")
	}
	if command.ExecutionID == "" {
		return ProgressStatus{}, fault.New(fault.InvalidInput, "progress requires an execution id")
	}
	if command.Outcome != ProgressSucceeded && command.Outcome != ProgressFailed {
		return ProgressStatus{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown progress outcome %q", command.Outcome))
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return ProgressStatus{}, err
	}
	if current.Scope != command.Scope {
		return ProgressStatus{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	currentOnly := command.Outcome == ProgressSucceeded &&
		current.State == StateOpen &&
		current.CurrentExecution != "" &&
		current.CurrentExecution == command.ExecutionID
	if !currentOnly {
		return ProgressStatus{State: current.State}, nil
	}
	if view, known, err := service.predecessor(ctx, command.Scope, command.ExecutionID); err != nil {
		return ProgressStatus{}, err
	} else if known && !view.CompletesTaskOnSuccess() {
		return ProgressStatus{State: current.State}, nil
	}
	result, err := service.progress.Apply(ctx, ProgressUnit{
		Owner:                    command.Scope,
		TaskID:                   command.TaskID,
		ExecutionID:              command.ExecutionID,
		ExpectedLifecycleVersion: current.LifecycleVersion,
		At:                       service.clock.Now(),
	})
	if err != nil {
		return ProgressStatus{}, err
	}
	return ProgressStatus{State: StateDone, Applied: true, Replayed: result.Replayed}, nil
}

// History authorizes one scoped history read and passes the stable cursor and
// limit through unchanged.
func (service *Service) History(ctx context.Context, command HistoryCommand) (HistoryPage, error) {
	const operation authorize.OperationName = "task.history"
	if err := validateProjectScope(command.Scope); err != nil {
		return HistoryPage{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return HistoryPage{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return HistoryPage{}, err
	}
	// An execution principal may read only the history of the task it is
	// currently running, so the target task must be read before the reader is
	// asked for its history.
	if actor.Kind == identity.KindExecution {
		current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
		if err != nil {
			return HistoryPage{}, err
		}
		if current.Scope != command.Scope {
			return HistoryPage{}, fault.New(fault.NotFound, "task is not present in this scope")
		}
		if err := confirmExecutionBinding(actor, current); err != nil {
			return HistoryPage{}, err
		}
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return HistoryPage{}, err
	}
	reads := []authorize.AuthorityRead{
		newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
	}
	if err := service.confirm(ctx, reads); err != nil {
		return HistoryPage{}, err
	}
	return service.reader.History(ctx, command.Scope, command.TaskID, HistoryQuery{Limit: command.Limit, Cursor: command.Cursor})
}

// ReadCommand is one authorized task read. A read carries no receipt, commit
// unit or event, so the command has no caller key or content.
type ReadCommand struct {
	Actor  authorize.Principal
	Scope  tenancy.Scope
	TaskID id.TaskID
}

// Read authorizes one scoped task read and returns the authoritative task
// unchanged. The declared registry grants task.read to human and service
// engineers and viewers and binds an execution principal to its own task's
// current execution; like History it is a pure read with no receipt, unit or
// event.
func (service *Service) Read(ctx context.Context, command ReadCommand) (Task, error) {
	const operation authorize.OperationName = "task.read"
	if err := validateProjectScope(command.Scope); err != nil {
		return Task{}, err
	}
	if err := service.authorize(ctx, command.Actor, operation, command.Scope); err != nil {
		return Task{}, err
	}
	actor, err := service.confirmActor(ctx, command.Actor, operation)
	if err != nil {
		return Task{}, err
	}
	// An execution principal may read only the task it is currently running.
	// The binding read is the only read permitted before a foreign or stale
	// execution is refused.
	if actor.Kind == identity.KindExecution {
		current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
		if err != nil {
			return Task{}, err
		}
		if current.Scope != command.Scope {
			return Task{}, fault.New(fault.NotFound, "task is not present in this scope")
		}
		if err := confirmExecutionBinding(actor, current); err != nil {
			return Task{}, err
		}
	}
	requires, err := registryRequires(operation)
	if err != nil {
		return Task{}, err
	}
	reads := []authorize.AuthorityRead{
		newAuthorityRead(operation, requires, authorize.CheckMembership, command.Scope, command.Actor.ID, "", 0),
	}
	if err := service.confirm(ctx, reads); err != nil {
		return Task{}, err
	}
	current, err := service.reader.Get(ctx, command.Scope, command.TaskID)
	if err != nil {
		return Task{}, err
	}
	if current.Scope != command.Scope {
		return Task{}, fault.New(fault.NotFound, "task is not present in this scope")
	}
	return current, nil
}

// predecessor reads the current execution when the task has one. A missing row
// is treated as no known predecessor, so a lost-response retry can still reach
// the receipt-first port and no dispatch is requested without stop evidence.
func (service *Service) predecessor(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (ExecutionView, bool, error) {
	if executionID == "" {
		return ExecutionView{}, false, nil
	}
	view, err := service.executions.Get(ctx, scope, executionID)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return ExecutionView{}, false, nil
		}
		return ExecutionView{}, false, err
	}
	return view, true, nil
}

func (service *Service) authorize(ctx context.Context, actor authorize.Principal, operation authorize.OperationName, scope tenancy.Scope) error {
	if actor.Scope != scope {
		return fault.New(fault.Forbidden, "actor scope does not match command scope")
	}
	var (
		decision authorize.Decision
		err      error
	)
	if actor.Kind == identity.KindExecution && executionOwnTaskOperation(operation) {
		// An execution principal carries the registry's execution permission,
		// never a human or service membership grant. Only the own-task
		// content/ask operations use that admitted permission here; every
		// other operation keeps the shared authorizer, whose declared matrix
		// refuses an execution principal.
		requires, requiresErr := registryRequires(operation)
		if requiresErr != nil {
			return requiresErr
		}
		admitted := authorize.AdmittedExecution{
			Principal:   actor,
			ExecutionID: actor.ExecutionID,
			Permission:  requires.Execution,
		}
		decision, err = authorize.AuthorizeExecution(admitted, operation, scope)
	} else {
		decision, err = service.authorizer.Authorize(ctx, actor, operation, scope)
	}
	if err != nil {
		return err
	}
	if decision != authorize.DecisionAllow {
		return fault.New(fault.Forbidden, fmt.Sprintf("principal is not authorized for %q", operation))
	}
	return nil
}

// executionOwnTaskOperation reports whether the registry grants an execution
// principal this own-task operation. The closed set is task content/metadata
// reads and edits (read, history, content edit), which the commit boundary
// binds to the exact own task and current execution, and the own-task ask.
// Conversation comments, task creation, lifecycle transitions and replies stay
// outside it even where the registry row carries an execution permission, so
// they remain refused for an execution principal.
func executionOwnTaskOperation(operation authorize.OperationName) bool {
	switch operation {
	case "task.read", "task.history", "task.edit", "task.ask":
		return true
	default:
		return false
	}
}

// confirmActor re-reads the actor's current principal so a previously
// calculated allow never substitutes for current enablement and membership.
func (service *Service) confirmActor(ctx context.Context, actor authorize.Principal, operation authorize.OperationName) (identity.Principal, error) {
	requires, err := registryRequires(operation)
	if err != nil {
		return identity.Principal{}, err
	}
	principal, err := service.principals.ReadPrincipal(ctx, identity.PrincipalRequest{Scope: actor.Scope, PrincipalID: actor.ID})
	if err != nil {
		return identity.Principal{}, err
	}
	if principal.Scope != actor.Scope {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is not present in the actor scope")
	}
	if !principal.Enabled {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is disabled")
	}
	if principal.Kind == identity.KindExecution {
		// Execution authority is the registry's execution permission, never a
		// human or service membership grant. An operation that declares no
		// execution permission therefore refuses every execution principal.
		if requires.Execution == authorize.ExecNone {
			return identity.Principal{}, fault.New(fault.Forbidden, fmt.Sprintf("execution principal has no permission for %q", operation))
		}
		if principal.ExecutionID == "" || principal.ExecutionID != actor.ExecutionID {
			return identity.Principal{}, fault.New(fault.Forbidden, "execution principal identity does not match the authenticated actor")
		}
		return principal, nil
	}
	if !requires.Allows(principal) {
		return identity.Principal{}, fault.New(fault.Forbidden, fmt.Sprintf("principal does not currently hold the grant for %q", operation))
	}
	return principal, nil
}

// confirmExecutionBinding refuses an execution actor unless the re-read
// principal is the target task's current execution in the same scope. An
// execution may act only on the task it is running, so a foreign or stale
// execution can neither ask on, edit, comment on nor read another task.
func confirmExecutionBinding(principal identity.Principal, current Task) error {
	if principal.Kind != identity.KindExecution {
		return nil
	}
	if principal.Scope != current.Scope || principal.ExecutionID == "" || principal.ExecutionID != current.CurrentExecution {
		return fault.New(fault.Forbidden, "execution principal is not the task's current execution")
	}
	return nil
}

// initiatorStartRead confirms the task initiator's current start authority
// before admission and returns the authority read carried in the unit.
//
// The operator role is a conditional task.start grant. This pre-admission
// membership read cannot evaluate the failed-task condition, so an initiator
// whose only start grant is the conditional operator role is refused: an
// operator-only initiator never starts a fresh execution through a reply, and
// an engineer grant is unaffected.
//
// The read carried into the unit drops the conditional operator role from its
// required human roles. A reply creates a fresh execution, so the failed-task
// condition can never apply to the initiator; keeping the operator role in the
// read would let an initiator demoted from engineer to operator-only between
// this check and the commit pass the in-transaction re-evaluation. Service roles
// and the execution permission are unchanged.
func (service *Service) initiatorStartRead(ctx context.Context, scope tenancy.Scope, initiator id.PrincipalID) (authorize.AuthorityRead, error) {
	registered, ok := authorize.LookupOperation("task.start")
	if !ok {
		return authorize.AuthorityRead{}, fault.New(fault.InternalFailure, "operation \"task.start\" is not registered")
	}
	requires := registered.Requires
	read := newAuthorityRead("task.start", unconditionalStartRequires(requires), authorize.CheckMembership, scope, initiator, "", 0)
	principal, err := service.principals.ReadPrincipal(ctx, identity.PrincipalRequest{Scope: scope, PrincipalID: initiator})
	if err != nil {
		return authorize.AuthorityRead{}, err
	}
	if principal.Scope != scope || !principal.Enabled || !requires.Allows(principal) {
		return authorize.AuthorityRead{}, fault.New(fault.Forbidden, "task initiator no longer holds start authority")
	}
	if len(authorize.ConditionalGrantChecks(registered, principal)) > 0 {
		return authorize.AuthorityRead{}, fault.New(fault.Forbidden, "task initiator holds only a conditional start grant")
	}
	return read, nil
}

// unconditionalStartRequires returns the task.start grant set without the
// conditional operator human role. The reply's initiator read is a fresh-start
// membership check that cannot evaluate the failed-task condition, so it must
// never accept the operator role as sufficient. Service roles and the execution
// permission are unchanged.
func unconditionalStartRequires(requires authorize.Requires) authorize.Requires {
	humanRoles := make([]tenancy.Role, 0, len(requires.HumanRoles))
	for _, role := range requires.HumanRoles {
		if role == tenancy.RoleOperator {
			continue
		}
		humanRoles = append(humanRoles, role)
	}
	requires.HumanRoles = humanRoles
	return requires
}

func (service *Service) confirm(ctx context.Context, reads []authorize.AuthorityRead) error {
	if len(reads) == 0 {
		return nil
	}
	facts, err := service.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

func (service *Service) operationRecord(scope tenancy.Scope, principal id.PrincipalID, operation, callerKey string, content receipt.Content, at time.Time, executionID id.ExecutionID, snapshotHash *[32]byte) (receipt.Record, event.Audit, error) {
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		return receipt.Record{}, event.Audit{}, fault.Wrap(fault.InvalidInput, "receipt content is invalid", err)
	}
	receiptID, err := service.ids.NewReceiptID()
	if err != nil {
		return receipt.Record{}, event.Audit{}, err
	}
	auditID, err := service.ids.NewEventID()
	if err != nil {
		return receipt.Record{}, event.Audit{}, err
	}
	record := receipt.Record{
		ID: receiptID,
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principal,
			Operation:   operation,
			CallerKey:   callerKey,
		},
		ContentHash:  fingerprint,
		ExecutionID:  executionID,
		SnapshotHash: snapshotHash,
		At:           at,
	}
	if err := record.Key.Validate(); err != nil {
		return receipt.Record{}, event.Audit{}, err
	}
	return record, event.Audit{ID: auditID, Scope: scope, Actor: principal, Operation: operation, At: at}, nil
}

type taskEventData struct {
	ParentID id.TaskID `json:"parent_id,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

func (service *Service) newTaskEvent(eventType event.Type, actor id.PrincipalID, at time.Time, scope tenancy.Scope, subjectID string, subjectVersion uint64, parentID id.TaskID, reason string) (event.Envelope, error) {
	envelope, err := service.newEvent(eventType, actor, at, scope, subjectID, subjectVersion)
	if err != nil {
		return event.Envelope{}, err
	}
	if parentID != "" || reason != "" {
		encoded, err := json.Marshal(taskEventData{
			ParentID: parentID,
			Reason:   reason,
		})
		if err != nil {
			return event.Envelope{}, fault.Wrap(fault.InternalFailure, "task event payload could not be encoded", err)
		}
		envelope.Data = encoded
	}
	return envelope, nil
}

func cloneOptionalBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func (service *Service) newEvent(eventType event.Type, actor id.PrincipalID, at time.Time, scope tenancy.Scope, subjectID string, subjectVersion uint64) (event.Envelope, error) {
	eventID, err := service.ids.NewEventID()
	if err != nil {
		return event.Envelope{}, err
	}
	return event.Envelope{
		ID:             eventID,
		Type:           eventType,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: actor},
		At:             at,
		Scope:          scope,
		SubjectID:      subjectID,
		SubjectVersion: subjectVersion,
	}, nil
}

func registryRequires(operation authorize.OperationName) (authorize.Requires, error) {
	registered, ok := authorize.LookupOperation(operation)
	if !ok {
		return authorize.Requires{}, fault.New(fault.InternalFailure, fmt.Sprintf("operation %q is not registered", operation))
	}
	return registered.Requires, nil
}

func newAuthorityRead(operation authorize.OperationName, requires authorize.Requires, check authorize.AuthorityCheck, scope tenancy.Scope, principal id.PrincipalID, target string, expected uint64) authorize.AuthorityRead {
	return authorize.AuthorityRead{
		Operation:       operation,
		Required:        requires,
		Check:           check,
		Scope:           scope,
		PrincipalID:     principal,
		TargetID:        target,
		ExpectedVersion: expected,
	}
}

// nonVersionedReads drops the versioned reads a commit port must observe
// inside its atomic unit. Confirming them here would reject a lost-response
// retry whose committed write already advanced the version, so the replay
// would never be reached.
func nonVersionedReads(reads []authorize.AuthorityRead) []authorize.AuthorityRead {
	filtered := make([]authorize.AuthorityRead, 0, len(reads))
	for _, read := range reads {
		switch read.Check {
		case authorize.CheckTaskVersion, authorize.CheckExecutionVersion, authorize.CheckPipelineVersion:
			continue
		default:
			filtered = append(filtered, read)
		}
	}
	return filtered
}

func enforceAssignment(actor identity.Principal, assignment request.Assignment) error {
	if !assignment.Valid() {
		return fault.New(fault.InternalFailure, "task question carries an invalid assignment")
	}
	switch assignment.Kind {
	case request.AssignmentRole:
		if actor.Kind != identity.KindHuman || !actor.HasRole(assignment.Role) {
			return fault.New(fault.Forbidden, "reply is assigned to a human role the caller does not currently hold")
		}
	case request.AssignmentService:
		if actor.Kind != identity.KindService || actor.ID != id.PrincipalID(assignment.ServiceID) {
			return fault.New(fault.Forbidden, "reply is assigned to a different service principal")
		}
	}
	return nil
}

func editOperation(kind EditKind) (authorize.OperationName, error) {
	switch kind {
	case EditContent:
		return "task.edit", nil
	case EditComment:
		return "task.comment", nil
	case EditSelectPipeline:
		return "task.select_pipeline", nil
	default:
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("unknown edit kind %q", kind))
	}
}

func validateProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "task operations require a project scope")
	}
	return nil
}

func validateTitle(title string) error {
	if title == "" {
		return fault.New(fault.InvalidInput, "task title is empty")
	}
	return nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneInputs(values map[id.InputKey]string) map[id.InputKey]string {
	if values == nil {
		return nil
	}
	cloned := make(map[id.InputKey]string, len(values))
	maps.Copy(cloned, values)
	return cloned
}

func cloneActions(actions []pipeline.AdmittedAction) []AdmittedAction {
	if actions == nil {
		return nil
	}
	return append([]AdmittedAction(nil), actions...)
}
