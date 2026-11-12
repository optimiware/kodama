package work

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	executionstore "github.com/optimiware/kodama/internal/shell/store/units/execution"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// defaultHistoryLimit is the history page size when the caller asks for zero.
const defaultHistoryLimit = 50

// executionPlan is one queued execution plus its ordered admitted actions.
type executionPlan struct {
	executionID     id.ExecutionID
	pipelineID      pipeline.PipelineID
	pipelineVersion uint64
	taskID          id.TaskID
	principal       id.PrincipalID
	snapshot        pipeline.Snapshot
	actions         []pipeline.AdmittedAction
	at              time.Time
}

// writes renders the queued execution row and every admitted action row through
// the shared snapshot document codec, so a task or trigger start round-trips
// through the Kernel engine exactly like a task-free execution start.
func (plan executionPlan) writes() ([]store.Write, error) {
	if err := requireNonEmpty("execution id", string(plan.executionID)); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("execution principal", string(plan.principal)); err != nil {
		return nil, err
	}
	if plan.at.IsZero() {
		return nil, fault.New(fault.InvalidInput, "execution commit time is zero")
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      plan.pipelineID,
		PipelineVersion: plan.pipelineVersion,
		Snapshot:        plan.snapshot,
		Actions:         plan.actions,
	})
	if err != nil {
		return nil, err
	}
	actions, err := snapshotdoc.ActionRows(plan.executionID, plan.actions)
	if err != nil {
		return nil, err
	}
	digest := plan.snapshot.YAMLSHA256
	writes := []store.Write{{
		Table:  "executions",
		ID:     string(plan.executionID),
		Insert: true,
		Values: map[string]any{
			"pipeline_id":      nullableText(string(plan.pipelineID)),
			"task_id":          nullableText(string(plan.taskID)),
			"principal_id":     string(plan.principal),
			"state":            string(kernel.ExecutionQueued),
			"version":          int64(1),
			"yaml_sha256":      digest[:],
			"snapshot":         document,
			"token_nonce":      nil,
			"token_expires_at": nil,
			"workspace_id":     string(snapshotdoc.DeriveWorkspaceID(plan.executionID)),
			"created_at":       plan.at,
		},
	}}
	for _, action := range actions {
		writes = append(writes, store.Write{
			Table:  "actions",
			ID:     action.ID,
			Insert: true,
			Values: action.Values,
		})
	}
	return writes, nil
}

// taskWrite renders one guarded tasks update. The tasks table has no single
// version column, so every update carries an explicit Expect guard.
func taskWrite(identity id.TaskID, expect map[string]any, values map[string]any) store.Write {
	return store.Write{Table: "tasks", ID: string(identity), Expect: expect, Values: values}
}

// taskLock renders one guarded tasks lock.
func taskLock(identity id.TaskID, expect map[string]any) store.Lock {
	return store.Lock{Table: "tasks", IDs: []string{string(identity)}, Expect: expect}
}

// pipelineLock renders one guarded pipelines lock.
func pipelineLock(identity pipeline.PipelineID, version uint64) store.Lock {
	return store.Lock{Table: "pipelines", IDs: []string{string(identity)}, ExpectVersion: version}
}

// executionLock renders one guarded executions lock.
func executionLock(identity id.ExecutionID, version uint64) store.Lock {
	return store.Lock{Table: "executions", IDs: []string{string(identity)}, ExpectVersion: version}
}

// taskColumns is the decoded durable task conversation.
type taskColumns struct {
	contentVersion   uint64
	lifecycleVersion uint64
	state            task.State
	title            string
	description      string
	metadata         map[string]string
	inputs           map[id.InputKey]string
	history          []id.ExecutionID
	pipelineID       pipeline.PipelineID
	currentExecution id.ExecutionID
	initiator        id.PrincipalID
	question         *task.Question
}

// readTaskRow reads and decodes one scoped tasks row.
func (adapter *Task) readTaskRow(ctx context.Context, scope tenancy.Scope, identity id.TaskID) (store.Row, error) {
	if err := validProjectScope(scope); err != nil {
		return store.Row{}, err
	}
	if err := requireNonEmpty("task id", string(identity)); err != nil {
		return store.Row{}, err
	}
	return readRow(ctx, adapter.executor, scope, "tasks", string(identity))
}

// decodeTaskColumns decodes the durable columns of one tasks row.
func decodeTaskColumns(row store.Row) (taskColumns, error) {
	columns := taskColumns{
		contentVersion:   versionValue(row, "content_version"),
		lifecycleVersion: versionValue(row, "lifecycle_version"),
		state:            task.State(textValue(row, "state")),
		title:            textValue(row, "title"),
		description:      textValue(row, "description"),
		pipelineID:       pipeline.PipelineID(textValue(row, "pipeline_id")),
		currentExecution: id.ExecutionID(textValue(row, "current_execution_id")),
		initiator:        id.PrincipalID(textValue(row, "initiator_id")),
	}
	metadata, err := decodeJSON[map[string]string](bytesValue(row, "metadata"))
	if err != nil {
		return taskColumns{}, err
	}
	columns.metadata = metadata
	inputs, err := decodeJSON[map[id.InputKey]string](bytesValue(row, "inputs"))
	if err != nil {
		return taskColumns{}, err
	}
	columns.inputs = inputs
	history, err := decodeJSON[[]id.ExecutionID](bytesValue(row, "execution_history"))
	if err != nil {
		return taskColumns{}, err
	}
	columns.history = history
	if questionID := textValue(row, "question_id"); questionID != "" {
		assignment, err := decodeJSON[request.Assignment](bytesValue(row, "question_assignment"))
		if err != nil {
			return taskColumns{}, err
		}
		references, err := decodeJSON[[]string](bytesValue(row, "question_references"))
		if err != nil {
			return taskColumns{}, err
		}
		columns.question = &task.Question{
			ID:         task.QuestionID(questionID),
			Body:       textValue(row, "question_body"),
			Assignment: assignment,
			References: references,
			Version:    versionValue(row, "question_version"),
			AskedBy:    id.PrincipalID(textValue(row, "question_asked_by")),
			At:         timeValue(row, "question_at"),
		}
	}
	return columns, nil
}

// taskValue shapes one decoded task row as the capability value.
func taskValue(identity id.TaskID, scope tenancy.Scope, columns taskColumns, messages []task.Message) task.Task {
	return task.Task{
		ID:               identity,
		Scope:            scope,
		Title:            columns.title,
		Description:      columns.description,
		Metadata:         columns.metadata,
		Messages:         messages,
		ContentVersion:   columns.contentVersion,
		LifecycleVersion: columns.lifecycleVersion,
		State:            columns.state,
		SelectedPipeline: columns.pipelineID,
		Inputs:           columns.inputs,
		Initiator:        columns.initiator,
		ExecutionHistory: columns.history,
		CurrentExecution: columns.currentExecution,
		Question:         columns.question,
	}
}

// messages reads every task_messages row of one task in (at, id) order.
func (adapter *Task) messages(ctx context.Context, scope tenancy.Scope, identity id.TaskID) ([]task.Message, error) {
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "task_messages",
		Equals:  map[string]any{"task_id": string(identity)},
		OrderBy: []store.Order{{Column: "at"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	messages := make([]task.Message, 0, len(rows))
	for _, row := range rows {
		references, err := decodeJSON[[]string](bytesValue(row, "refs"))
		if err != nil {
			return nil, err
		}
		messages = append(messages, task.Message{
			Author:     id.PrincipalID(textValue(row, "author_id")),
			At:         timeValue(row, "at"),
			Body:       textValue(row, "body"),
			References: references,
		})
	}
	return messages, nil
}

// appendedHistory reads the current execution history and appends one identity.
// The caller's update guard on lifecycle_version makes the read-modify-write
// safe: a concurrent lifecycle change makes the guarded write fail instead of
// losing the other history entry.
func (adapter *Task) appendedHistory(ctx context.Context, scope tenancy.Scope, identity id.TaskID, executionID id.ExecutionID) ([]id.ExecutionID, error) {
	row, err := adapter.readTaskRow(ctx, scope, identity)
	if err != nil {
		return nil, err
	}
	columns, err := decodeTaskColumns(row)
	if err != nil {
		return nil, err
	}
	history := make([]id.ExecutionID, 0, len(columns.history)+1)
	history = append(history, columns.history...)
	return append(history, executionID), nil
}

// discardStagedOutputsExcluding clears the staged output of every action row
// the Kernel commit does not already clear. The Kernel plan owns the primary
// action and every pending cancellation, so their staged-output columns are
// cleared on that same guarded write; issuing a second update for the same row
// would conflict with the plan's own version guard. Every other action is
// discarded with one guarded update.
func (adapter *Task) discardStagedOutputsExcluding(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID, planned []store.Write) ([]store.Write, error) {
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "actions",
		Equals:  map[string]any{"execution_id": string(executionID)},
		OrderBy: []store.Order{{Column: "index"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	plannedActions := make(map[string]int, len(planned))
	for position, write := range planned {
		if write.Table == "actions" {
			plannedActions[write.ID] = position
		}
	}
	var writes []store.Write
	for _, row := range rows {
		if !boolValue(row, "staged_outputs_set") && len(bytesValue(row, "staged_outputs")) == 0 {
			continue
		}
		identity := textValue(row, "id")
		if position, ok := plannedActions[identity]; ok {
			planned[position].Values["staged_outputs"] = nil
			planned[position].Values["staged_outputs_set"] = false
			continue
		}
		writes = append(writes, store.Write{
			Table:         "actions",
			ID:            identity,
			ExpectVersion: versionValue(row, "version"),
			Values: map[string]any{
				"staged_outputs":     nil,
				"staged_outputs_set": false,
			},
		})
	}
	return writes, nil
}

// cancellationPlan reads the task's current execution in scope, applies the
// cancellation rule and maps the Kernel cancel commit onto the same atomic
// store unit. It NEVER writes executions.state directly:
//
//   - a live predecessor must carry the Kernel cancel commit, which is mapped
//     by the execution adapter's PlanScopedCommit (execution state, credential,
//     primary action, pending cancellations and cleanup progress);
//   - a terminal predecessor is preserved: the unit pins only its version so a
//     concurrent change refuses the task operation, and writes nothing on the
//     execution, action or credential rows;
//   - a unit that declares an expected execution version but carries no commit
//     for a live predecessor is invalid_input.
func (adapter *Task) cancellationPlan(
	ctx context.Context,
	scope tenancy.Scope,
	identity id.TaskID,
	expectedExecutionVersion uint64,
	commit *flow.Commit,
	credential *token.Credential,
) ([]store.Lock, []store.Write, error) {
	pointer, err := adapter.currentExecutionPointer(ctx, scope, identity)
	if err != nil {
		return nil, nil, err
	}
	if pointer == "" {
		if err := task.ValidateCancellationCommit(expectedExecutionVersion, "", kernel.Execution{}, commit); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	pre, err := executionstore.New(adapter.executor, adapter.scanner).Load(ctx, scope, pointer)
	if err != nil {
		// A lost-response retry whose predecessor is no longer readable is
		// accepted only as "no aggregate" when the unit declares no version and
		// carries no commit; anything else fails closed with the reader's class.
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound && commit == nil && expectedExecutionVersion == 0 {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if err := task.ValidateCancellationCommit(expectedExecutionVersion, pointer, pre, commit); err != nil {
		return nil, nil, err
	}
	if commit == nil {
		return []store.Lock{executionLock(pointer, expectedExecutionVersion)}, nil, nil
	}
	read := func(ctx context.Context, table, identity string) (store.Row, error) {
		return adapter.executor.Read(ctx, scope, table, identity)
	}
	plan, err := executionstore.PlanScopedCommit(ctx, read, *commit, credential, nil)
	if err != nil {
		return nil, nil, err
	}
	discards, err := adapter.discardStagedOutputsExcluding(ctx, scope, pointer, plan.Writes)
	if err != nil {
		return nil, nil, err
	}
	return plan.Locks, append(plan.Writes, discards...), nil
}

// Create applies one complete task-creation commit unit, optionally with the
// nested admission/start rows in the same store.Unit.
func (adapter *Task) Create(ctx context.Context, unit task.CreateUnit) (task.CreateResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.CreateResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.CreateResult{}, err
	}
	if err := requireNonEmpty("task initiator", string(unit.Initiator)); err != nil {
		return task.CreateResult{}, err
	}
	if unit.At.IsZero() {
		return task.CreateResult{}, fault.New(fault.InvalidInput, "task creation time is zero")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.CreateResult{}, err
	}
	lifecycle := uint64(1)
	if unit.Start != nil {
		start := *unit.Start
		switch {
		case start.Owner != unit.Owner:
			return task.CreateResult{}, fault.New(fault.InvalidInput, "nested start owner does not match the creation scope")
		case start.TaskID != unit.TaskID:
			return task.CreateResult{}, fault.New(fault.InvalidInput, "nested start task does not match the created task")
		case start.ExpectedLifecycleVersion > 1:
			// The task is inserted at lifecycle version 1, so a create-and-start
			// unit carries no prior lifecycle version (0) and the nested update
			// is guarded on the row this same unit inserts.
			return task.CreateResult{}, fault.New(fault.InvalidInput, "nested start cannot expect an existing lifecycle version")
		case start.PipelineID == "":
			return task.CreateResult{}, fault.New(fault.InvalidInput, "nested start requires a pipeline id")
		case start.ExpectedPipelineVersion == 0:
			return task.CreateResult{}, fault.New(fault.InvalidInput, "nested start requires an expected pipeline version")
		}
		lifecycle = 2
	}
	result := task.CreateResult{
		TaskID:           unit.TaskID,
		ContentVersion:   1,
		LifecycleVersion: lifecycle,
	}
	if unit.Start != nil {
		result.ExecutionID = unit.Start.NewExecutionID
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.CreateResult{}, err
	}
	writes, locks, err := creationWrites(unit)
	if err != nil {
		return task.CreateResult{}, err
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{TaskID: unit.TaskID})
	if err != nil {
		return task.CreateResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.CreateResult{}, err
	}
	replayed, isReplay, err := replay[task.CreateResult](committed)
	if err != nil {
		return task.CreateResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// creationWrites renders the tasks insert plus the optional nested start rows.
func creationWrites(unit task.CreateUnit) ([]store.Write, []store.Lock, error) {
	values := map[string]any{
		"content_version":   int64(1),
		"lifecycle_version": int64(1),
		"state":             string(task.StateOpen),
		"initiator_id":      string(unit.Initiator),
		"title":             unit.Title,
		"created_at":        unit.At,
	}
	if unit.Description != "" {
		values["description"] = unit.Description
	}
	if len(unit.Metadata) > 0 {
		encoded, err := jsonColumn(unit.Metadata)
		if err != nil {
			return nil, nil, err
		}
		values["metadata"] = encoded
	}
	if unit.SelectedPipeline != "" {
		values["pipeline_id"] = string(unit.SelectedPipeline)
	}
	if len(unit.Inputs) > 0 {
		encoded, err := jsonColumn(unit.Inputs)
		if err != nil {
			return nil, nil, err
		}
		values["inputs"] = encoded
	}
	writes := []store.Write{{Table: "tasks", ID: string(unit.TaskID), Insert: true, Values: values}}
	var locks []store.Lock
	if unit.Start == nil {
		if unit.SelectedPipeline != "" {
			// No foreign key backs tasks.pipeline_id, so the unit pins the
			// selected pipeline itself. The unguarded lock fails when the row is
			// absent or outside the unit scope. The executor reads the receipt
			// first, so a lost-response retry still replays after the pipeline
			// was deleted.
			locks = append(locks, store.Lock{Table: "pipelines", IDs: []string{string(unit.SelectedPipeline)}})
		}
		return writes, locks, nil
	}
	start := *unit.Start
	plan := executionPlan{
		executionID:     start.NewExecutionID,
		pipelineID:      start.PipelineID,
		pipelineVersion: start.ExpectedPipelineVersion,
		taskID:          unit.TaskID,
		principal:       start.ExecutionPrincipal,
		snapshot:        start.Snapshot,
		actions:         start.Actions,
		at:              start.At,
	}
	executionWrites, err := plan.writes()
	if err != nil {
		return nil, nil, err
	}
	history, err := jsonColumn([]id.ExecutionID{start.NewExecutionID})
	if err != nil {
		return nil, nil, err
	}
	writes = append(writes, executionWrites...)
	writes = append(writes, taskWrite(unit.TaskID, map[string]any{"lifecycle_version": int64(1)}, map[string]any{
		"state":                string(task.StateOpen),
		"lifecycle_version":    int64(2),
		"pipeline_id":          string(start.PipelineID),
		"current_execution_id": string(start.NewExecutionID),
		"execution_history":    history,
	}))
	if start.PipelineID != "" {
		locks = append(locks, pipelineLock(start.PipelineID, start.ExpectedPipelineVersion))
	}
	return writes, locks, nil
}

// committedReceipt reports whether a durable receipt with the unit's
// idempotency identity already exists in the unit's scope. It is a pre-flight
// read only: the executor still decides replay versus changed content
// atomically, and a fresh start still carries every authority guard.
func (adapter *Task) committedReceipt(ctx context.Context, scope tenancy.Scope, key receipt.Key) (bool, error) {
	page, err := adapter.scanner.Query(ctx, scope, store.ScanQuery{
		Table: "receipts",
		Equals: map[string]any{
			"principal_id": string(key.PrincipalID),
			"operation":    key.Operation,
			"caller_key":   key.CallerKey,
		},
		Limit: 1,
	})
	if err != nil {
		return false, err
	}
	return len(page.Rows) > 0, nil
}

// Start applies one complete start/admission commit unit.
func (adapter *Task) Start(ctx context.Context, unit task.StartUnit) (task.StartResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.StartResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.StartResult{}, err
	}
	if err := requireNonEmpty("pipeline id", string(unit.PipelineID)); err != nil {
		return task.StartResult{}, err
	}
	if unit.ExpectedLifecycleVersion == 0 {
		return task.StartResult{}, fault.New(fault.InvalidInput, "start requires an expected lifecycle version")
	}
	if unit.ExpectedPipelineVersion == 0 {
		return task.StartResult{}, fault.New(fault.InvalidInput, "start requires an expected pipeline version")
	}
	if unit.At.IsZero() {
		return task.StartResult{}, fault.New(fault.InvalidInput, "start commit time is zero")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.StartResult{}, err
	}
	history, err := adapter.appendedHistory(ctx, unit.Owner, unit.TaskID, unit.NewExecutionID)
	if err != nil {
		return task.StartResult{}, err
	}
	result := task.StartResult{
		ExecutionID:      unit.NewExecutionID,
		LifecycleVersion: unit.ExpectedLifecycleVersion + 1,
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.StartResult{}, err
	}
	plan := executionPlan{
		executionID:     unit.NewExecutionID,
		pipelineID:      unit.PipelineID,
		pipelineVersion: unit.ExpectedPipelineVersion,
		taskID:          unit.TaskID,
		principal:       unit.ExecutionPrincipal,
		snapshot:        unit.Snapshot,
		actions:         unit.Actions,
		at:              unit.At,
	}
	writes, err := plan.writes()
	if err != nil {
		return task.StartResult{}, err
	}
	historyValue, err := jsonColumn(history)
	if err != nil {
		return task.StartResult{}, err
	}
	writes = append(writes, taskWrite(unit.TaskID, map[string]any{
		"state":             string(task.StateOpen),
		"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
	}, map[string]any{
		"state":                string(task.StateOpen),
		"lifecycle_version":    int64(unit.ExpectedLifecycleVersion + 1),
		"pipeline_id":          string(unit.PipelineID),
		"current_execution_id": string(unit.NewExecutionID),
		"execution_history":    historyValue,
	}))
	locks := []store.Lock{
		pipelineLock(unit.PipelineID, unit.ExpectedPipelineVersion),
		taskLock(unit.TaskID, map[string]any{
			"state":             string(task.StateOpen),
			"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
		}),
	}
	// A durable receipt with this idempotency identity means the invocation
	// already committed (or reused its key with changed content). The
	// conditional failed-task operator grant is a fresh-start admission gate:
	// once the first operator retry commits, the task's current execution is the
	// newly queued one, so re-evaluating the gate would refuse a lost-response
	// retry before the executor can replay its receipt. Take the replay path
	// while still carrying the start membership read, which the commit boundary
	// re-evaluates inside the transaction (a revoked operator is refused).
	authorityReads := unit.Authority
	replaying, err := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
	if err != nil {
		return task.StartResult{}, err
	}
	if replaying {
		// The ordinary membership read (when the unit carries one) stays and is
		// re-evaluated by authorityLocks below; only the fresh-start failed-task
		// admission read is dropped.
		authorityReads = withoutFailedTaskOperatorStart(authorityReads)
	} else if err := requireOperatorStartCondition(ctx, adapter.executor, adapter.scanner, unit.Owner, authorityReads); err != nil {
		return task.StartResult{}, err
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, authorityReads, authorityContext{TaskID: unit.TaskID})
	if err != nil && !replaying {
		// A concurrent identical retry may have committed between the pre-flight
		// read and the failed-task re-read, so the fresh-start condition no
		// longer holds. If the receipt now exists, re-evaluate only the
		// remaining authority reads and let the executor replay; a genuinely
		// fresh start with no receipt keeps the refusal.
		committed, lookupErr := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
		if lookupErr != nil {
			return task.StartResult{}, lookupErr
		}
		if committed {
			authorityReads = withoutFailedTaskOperatorStart(authorityReads)
			authority, err = authorityLocks(ctx, adapter.executor, adapter.scanner, authorityReads, authorityContext{TaskID: unit.TaskID})
		}
	}
	if err != nil {
		return task.StartResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.StartResult{}, err
	}
	replayed, isReplay, err := replay[task.StartResult](committed)
	if err != nil {
		return task.StartResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// Ask applies one complete conversation-handoff commit unit: question, waiting
// state, cancelled execution, cleared credential and discarded staged outputs.
func (adapter *Task) Ask(ctx context.Context, unit task.AskUnit) (task.AskResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.AskResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.AskResult{}, err
	}
	if err := requireNonEmpty("question id", string(unit.QuestionID)); err != nil {
		return task.AskResult{}, err
	}
	if unit.Question == "" {
		return task.AskResult{}, fault.New(fault.InvalidInput, "question body is empty")
	}
	if !unit.Assignment.Valid() {
		return task.AskResult{}, fault.New(fault.InvalidInput, "question assignment is invalid")
	}
	if unit.ExpectedLifecycleVersion == 0 {
		return task.AskResult{}, fault.New(fault.InvalidInput, "ask requires an expected lifecycle version")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.AskResult{}, err
	}
	assignment, err := jsonColumn(unit.Assignment)
	if err != nil {
		return task.AskResult{}, err
	}
	references, err := jsonColumn(unit.References)
	if err != nil {
		return task.AskResult{}, err
	}
	result := task.AskResult{
		LifecycleVersion: unit.ExpectedLifecycleVersion + 1,
		QuestionVersion:  1,
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.AskResult{}, err
	}
	values := map[string]any{
		"state":               string(task.StateWaiting),
		"lifecycle_version":   int64(unit.ExpectedLifecycleVersion + 1),
		"question_id":         string(unit.QuestionID),
		"question_body":       unit.Question,
		"question_assignment": assignment,
		"question_references": references,
		"question_version":    int64(1),
		"question_asked_by":   string(record.Key.PrincipalID),
		"question_at":         record.At,
	}
	writes := []store.Write{taskWrite(unit.TaskID, map[string]any{
		"state":             string(task.StateOpen),
		"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
	}, values)}
	locks := []store.Lock{taskLock(unit.TaskID, map[string]any{
		"state":             string(task.StateOpen),
		"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
	})}
	replaying, err := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
	if err != nil {
		return task.AskResult{}, err
	}
	if !replaying {
		cancelLocks, cancelWrites, cancelErr := adapter.cancellationPlan(
			ctx, unit.Owner, unit.TaskID, unit.ExpectedExecutionVersion, unit.Commit, unit.CredentialUpdate,
		)
		if cancelErr != nil {
			committed, lookupErr := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
			if lookupErr != nil {
				return task.AskResult{}, lookupErr
			}
			if !committed {
				return task.AskResult{}, cancelErr
			}
		} else {
			locks = append(locks, cancelLocks...)
			writes = append(writes, cancelWrites...)
		}
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{TaskID: unit.TaskID})
	if err != nil {
		return task.AskResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.AskResult{}, err
	}
	replayed, isReplay, err := replay[task.AskResult](committed)
	if err != nil {
		return task.AskResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// currentExecutionPointer reads the task's current execution identity in the
// task's own scope. An empty pointer means the task has no predecessor.
func (adapter *Task) currentExecutionPointer(ctx context.Context, scope tenancy.Scope, identity id.TaskID) (id.ExecutionID, error) {
	row, err := adapter.readTaskRow(ctx, scope, identity)
	if err != nil {
		return "", err
	}
	columns, err := decodeTaskColumns(row)
	if err != nil {
		return "", err
	}
	return columns.currentExecution, nil
}

// Reply applies one complete reply commit unit: answer, reopen, cleared
// question and exactly one fresh queued execution.
func (adapter *Task) Reply(ctx context.Context, unit task.ReplyUnit) (task.ReplyResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.ReplyResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.ReplyResult{}, err
	}
	if err := requireNonEmpty("pipeline id", string(unit.PipelineID)); err != nil {
		return task.ReplyResult{}, err
	}
	if err := requireNonEmpty("question id", string(unit.QuestionID)); err != nil {
		return task.ReplyResult{}, err
	}
	if unit.Answer == "" {
		return task.ReplyResult{}, fault.New(fault.InvalidInput, "reply answer is empty")
	}
	if unit.ExpectedLifecycleVersion == 0 || unit.ExpectedQuestionVersion == 0 {
		return task.ReplyResult{}, fault.New(fault.InvalidInput, "reply requires expected lifecycle and question versions")
	}
	if unit.ExpectedPipelineVersion == 0 {
		return task.ReplyResult{}, fault.New(fault.InvalidInput, "reply requires an expected pipeline version")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.ReplyResult{}, err
	}
	history, err := adapter.appendedHistory(ctx, unit.Owner, unit.TaskID, unit.NewExecutionID)
	if err != nil {
		return task.ReplyResult{}, err
	}
	historyValue, err := jsonColumn(history)
	if err != nil {
		return task.ReplyResult{}, err
	}
	result := task.ReplyResult{
		ExecutionID:      unit.NewExecutionID,
		LifecycleVersion: unit.ExpectedLifecycleVersion + 1,
		QuestionVersion:  unit.ExpectedQuestionVersion + 1,
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.ReplyResult{}, err
	}
	plan := executionPlan{
		executionID:     unit.NewExecutionID,
		pipelineID:      unit.PipelineID,
		pipelineVersion: unit.ExpectedPipelineVersion,
		taskID:          unit.TaskID,
		principal:       unit.ExecutionPrincipal,
		snapshot:        unit.Snapshot,
		actions:         unit.Actions,
		at:              unit.At,
	}
	writes, err := plan.writes()
	if err != nil {
		return task.ReplyResult{}, err
	}
	writes = append(writes, taskWrite(unit.TaskID, map[string]any{
		"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
		"question_version":  int64(unit.ExpectedQuestionVersion),
		"question_id":       string(unit.QuestionID),
	}, map[string]any{
		"state":                string(task.StateOpen),
		"lifecycle_version":    int64(unit.ExpectedLifecycleVersion + 1),
		"pipeline_id":          string(unit.PipelineID),
		"current_execution_id": string(unit.NewExecutionID),
		"execution_history":    historyValue,
		"question_id":          nil,
		"question_body":        nil,
		"question_assignment":  nil,
		"question_references":  nil,
		"question_version":     nil,
		"question_asked_by":    nil,
		"question_at":          nil,
	}))
	// The answer is a durable conversation message committed with the reply:
	// the reader and history expose it, and the next reply's admitted
	// conversation snapshot includes it.
	writes = append(writes, store.Write{
		Table:  "task_messages",
		ID:     string(unit.Receipt.ID),
		Insert: true,
		Values: map[string]any{
			"task_id":   string(unit.TaskID),
			"author_id": string(record.Key.PrincipalID),
			"at":        unit.At,
			"body":      unit.Answer,
			"refs":      nil,
		},
	})
	locks := []store.Lock{
		pipelineLock(unit.PipelineID, unit.ExpectedPipelineVersion),
		taskLock(unit.TaskID, map[string]any{
			"lifecycle_version": int64(unit.ExpectedLifecycleVersion),
			"question_version":  int64(unit.ExpectedQuestionVersion),
			"question_id":       string(unit.QuestionID),
		}),
	}
	if err := requireOperatorStartCondition(ctx, adapter.executor, adapter.scanner, unit.Owner, unit.Authority); err != nil {
		return task.ReplyResult{}, err
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{TaskID: unit.TaskID})
	if err != nil {
		return task.ReplyResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.ReplyResult{}, err
	}
	replayed, isReplay, err := replay[task.ReplyResult](committed)
	if err != nil {
		return task.ReplyResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// Cancel applies one complete task-cancellation commit unit.
func (adapter *Task) Cancel(ctx context.Context, unit task.CancelUnit) (task.CancelResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.CancelResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.CancelResult{}, err
	}
	if unit.ExpectedLifecycleVersion == 0 {
		return task.CancelResult{}, fault.New(fault.InvalidInput, "cancel requires an expected lifecycle version")
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.CancelResult{}, err
	}
	result := task.CancelResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.CancelResult{}, err
	}
	replaying, err := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
	if err != nil {
		return task.CancelResult{}, err
	}
	lockExpect := map[string]any{"lifecycle_version": int64(unit.ExpectedLifecycleVersion)}
	writeExpect := map[string]any{"lifecycle_version": int64(unit.ExpectedLifecycleVersion)}
	if !replaying {
		row, err := adapter.readTaskRow(ctx, unit.Owner, unit.TaskID)
		if err != nil {
			return task.CancelResult{}, err
		}
		columns, err := decodeTaskColumns(row)
		if err != nil {
			return task.CancelResult{}, err
		}
		if columns.state != task.StateOpen && columns.state != task.StateWaiting {
			replaying, err = adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
			if err != nil {
				return task.CancelResult{}, err
			}
			if !replaying {
				return task.CancelResult{}, fault.New(fault.VersionConflict, fmt.Sprintf("task state %q does not permit cancel", columns.state))
			}
		} else {
			lockExpect["state"] = string(columns.state)
			writeExpect["state"] = string(columns.state)
		}
	}
	writes := []store.Write{taskWrite(unit.TaskID, writeExpect, map[string]any{
		"state":               string(task.StateCancelled),
		"lifecycle_version":   int64(unit.ExpectedLifecycleVersion + 1),
		"question_id":         nil,
		"question_body":       nil,
		"question_assignment": nil,
		"question_references": nil,
		"question_version":    nil,
		"question_asked_by":   nil,
		"question_at":         nil,
	})}
	locks := []store.Lock{taskLock(unit.TaskID, lockExpect)}
	if !replaying {
		cancelLocks, cancelWrites, cancelErr := adapter.cancellationPlan(
			ctx, unit.Owner, unit.TaskID, unit.ExpectedExecutionVersion, unit.Commit, unit.CredentialUpdate,
		)
		if cancelErr != nil {
			committed, lookupErr := adapter.committedReceipt(ctx, unit.Owner, unit.Receipt.Key)
			if lookupErr != nil {
				return task.CancelResult{}, lookupErr
			}
			if !committed {
				return task.CancelResult{}, cancelErr
			}
		} else {
			locks = append(locks, cancelLocks...)
			writes = append(writes, cancelWrites...)
		}
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{TaskID: unit.TaskID})
	if err != nil {
		return task.CancelResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.CancelResult{}, err
	}
	replayed, isReplay, err := replay[task.CancelResult](committed)
	if err != nil {
		return task.CancelResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// Edit applies one complete task-edit commit unit.
func (adapter *Task) Edit(ctx context.Context, unit task.EditUnit) (task.EditResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.EditResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.EditResult{}, err
	}
	if err := validateReceipt(unit.Receipt); err != nil {
		return task.EditResult{}, err
	}
	var (
		result task.EditResult
		writes []store.Write
		locks  []store.Lock
		err    error
	)
	switch unit.Kind {
	case task.EditContent:
		if unit.ExpectedContentVersion == 0 {
			return task.EditResult{}, fault.New(fault.InvalidInput, "content edit requires an expected content version")
		}
		result = task.EditResult{ContentVersion: unit.ExpectedContentVersion + 1}
		values := map[string]any{
			"content_version": int64(unit.ExpectedContentVersion + 1),
			"title":           unit.Title,
			"description":     nullableText(unit.Description),
		}
		if len(unit.Metadata) > 0 {
			encoded, err := jsonColumn(unit.Metadata)
			if err != nil {
				return task.EditResult{}, err
			}
			values["metadata"] = encoded
		} else {
			values["metadata"] = nil
		}
		writes = []store.Write{taskWrite(unit.TaskID, map[string]any{"content_version": int64(unit.ExpectedContentVersion)}, values)}
		locks = []store.Lock{taskLock(unit.TaskID, map[string]any{"content_version": int64(unit.ExpectedContentVersion)})}
	case task.EditComment:
		if unit.ExpectedContentVersion == 0 {
			return task.EditResult{}, fault.New(fault.InvalidInput, "comment edit requires an expected content version")
		}
		if unit.Message == nil || unit.Message.Body == "" {
			return task.EditResult{}, fault.New(fault.InvalidInput, "comment edit requires a message body")
		}
		references, err := jsonColumn(unit.Message.References)
		if err != nil {
			return task.EditResult{}, err
		}
		result = task.EditResult{ContentVersion: unit.ExpectedContentVersion + 1}
		writes = []store.Write{
			taskWrite(unit.TaskID, map[string]any{"content_version": int64(unit.ExpectedContentVersion)}, map[string]any{
				"content_version": int64(unit.ExpectedContentVersion + 1),
			}),
			{
				Table:  "task_messages",
				ID:     string(unit.Receipt.ID),
				Insert: true,
				Values: map[string]any{
					"task_id":   string(unit.TaskID),
					"author_id": string(unit.Message.Author),
					"at":        unit.Message.At,
					"body":      unit.Message.Body,
					"refs":      references,
				},
			},
		}
		locks = []store.Lock{taskLock(unit.TaskID, map[string]any{"content_version": int64(unit.ExpectedContentVersion)})}
	case task.EditSelectPipeline:
		if unit.ExpectedLifecycleVersion == 0 {
			return task.EditResult{}, fault.New(fault.InvalidInput, "pipeline selection requires an expected lifecycle version")
		}
		if unit.PipelineID == "" {
			return task.EditResult{}, fault.New(fault.InvalidInput, "pipeline selection requires a pipeline id")
		}
		result = task.EditResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1}
		values := map[string]any{
			"lifecycle_version": int64(unit.ExpectedLifecycleVersion + 1),
			"pipeline_id":       string(unit.PipelineID),
		}
		if unit.StartAuthority != "" {
			// A selection records the selecting principal as the start
			// authority Start and Reply read as tasks.initiator_id.
			values["initiator_id"] = string(unit.StartAuthority)
		}
		if len(unit.Inputs) > 0 {
			encoded, err := jsonColumn(unit.Inputs)
			if err != nil {
				return task.EditResult{}, err
			}
			values["inputs"] = encoded
		}
		writes = []store.Write{taskWrite(unit.TaskID, map[string]any{"lifecycle_version": int64(unit.ExpectedLifecycleVersion)}, values)}
		locks = []store.Lock{taskLock(unit.TaskID, map[string]any{"lifecycle_version": int64(unit.ExpectedLifecycleVersion)})}
		if unit.ExpectedPipelineVersion != 0 {
			locks = append(locks, pipelineLock(unit.PipelineID, unit.ExpectedPipelineVersion))
		}
	default:
		return task.EditResult{}, fault.New(fault.InvalidInput, "unknown task edit kind")
	}
	record, err := withResult(unit.Receipt, result)
	if err != nil {
		return task.EditResult{}, err
	}
	authority, err := authorityLocks(ctx, adapter.executor, adapter.scanner, unit.Authority, authorityContext{TaskID: unit.TaskID})
	if err != nil {
		return task.EditResult{}, err
	}
	locks = append(locks, authority...)
	committed, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: &record,
		Audit:   unit.Audit,
		Events:  unit.Events,
	})
	if err != nil {
		return task.EditResult{}, err
	}
	replayed, isReplay, err := replay[task.EditResult](committed)
	if err != nil {
		return task.EditResult{}, err
	}
	if isReplay {
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// Apply commits one current-only completion. ProgressUnit carries no receipt,
// so the commit is guarded by the exact lifecycle version and current execution
// instead of an idempotency record; a stale retry is a version_conflict.
func (adapter *Task) Apply(ctx context.Context, unit task.ProgressUnit) (task.ProgressResult, error) {
	if err := validProjectScope(unit.Owner); err != nil {
		return task.ProgressResult{}, err
	}
	if err := requireNonEmpty("task id", string(unit.TaskID)); err != nil {
		return task.ProgressResult{}, err
	}
	if err := requireNonEmpty("execution id", string(unit.ExecutionID)); err != nil {
		return task.ProgressResult{}, err
	}
	if unit.ExpectedLifecycleVersion == 0 {
		return task.ProgressResult{}, fault.New(fault.InvalidInput, "progress requires an expected lifecycle version")
	}
	lockExpect := map[string]any{
		"state":                string(task.StateOpen),
		"lifecycle_version":    int64(unit.ExpectedLifecycleVersion),
		"current_execution_id": string(unit.ExecutionID),
	}
	writeExpect := map[string]any{
		"state":                string(task.StateOpen),
		"lifecycle_version":    int64(unit.ExpectedLifecycleVersion),
		"current_execution_id": string(unit.ExecutionID),
	}
	result := task.ProgressResult{LifecycleVersion: unit.ExpectedLifecycleVersion + 1}
	if _, err := execute(ctx, adapter.executor, unit.Owner, store.Unit{
		Locks: []store.Lock{taskLock(unit.TaskID, lockExpect)},
		Writes: []store.Write{taskWrite(unit.TaskID, writeExpect, map[string]any{
			"state":             string(task.StateDone),
			"lifecycle_version": int64(unit.ExpectedLifecycleVersion + 1),
		})},
	}); err != nil {
		return task.ProgressResult{}, err
	}
	return result, nil
}

// Get reads one scoped task and its conversation.
func (adapter *Task) Get(ctx context.Context, scope tenancy.Scope, identity id.TaskID) (task.Task, error) {
	row, err := adapter.readTaskRow(ctx, scope, identity)
	if err != nil {
		return task.Task{}, err
	}
	columns, err := decodeTaskColumns(row)
	if err != nil {
		return task.Task{}, err
	}
	messages, err := adapter.messages(ctx, scope, identity)
	if err != nil {
		return task.Task{}, err
	}
	return taskValue(identity, scope, columns, messages), nil
}

// History reads one stable history page merged from the task's executions,
// conversation messages and its outstanding question.
func (adapter *Task) History(ctx context.Context, scope tenancy.Scope, identity id.TaskID, query task.HistoryQuery) (task.HistoryPage, error) {
	row, err := adapter.readTaskRow(ctx, scope, identity)
	if err != nil {
		return task.HistoryPage{}, err
	}
	columns, err := decodeTaskColumns(row)
	if err != nil {
		return task.HistoryPage{}, err
	}
	limit := query.Limit
	if limit < 0 || limit > store.MaxQueryLimit {
		return task.HistoryPage{}, fault.New(fault.InvalidInput, "history limit is outside the allowed range")
	}
	if limit == 0 {
		limit = defaultHistoryLimit
	}
	var cursor *historyCursor
	if query.Cursor != "" {
		decoded, err := decodeHistoryCursor(query.Cursor, scope, identity)
		if err != nil {
			return task.HistoryPage{}, err
		}
		cursor = &decoded
	}
	records, err := adapter.historyEntries(ctx, scope, identity, columns)
	if err != nil {
		return task.HistoryPage{}, err
	}
	sort.SliceStable(records, func(left, right int) bool {
		return historyLess(records[left], records[right])
	})
	filtered := records
	if cursor != nil {
		filtered = make([]historyRecord, 0, len(records))
		for _, record := range records {
			if historyAfter(record, *cursor) {
				filtered = append(filtered, record)
			}
		}
	}
	page := task.HistoryPage{}
	if len(filtered) > limit {
		filtered = filtered[:limit]
		last := filtered[len(filtered)-1]
		next, err := encodeHistoryCursor(scope, identity, last.entry.At, string(last.entry.Kind), last.identity)
		if err != nil {
			return task.HistoryPage{}, err
		}
		page.NextCursor = next
	}
	page.Entries = make([]task.HistoryEntry, 0, len(filtered))
	for _, record := range filtered {
		page.Entries = append(page.Entries, record.entry)
	}
	return page, nil
}

// historyRecord is one merged history entry with its durable row identity. The
// identity is the keyset tie-breaker, so two messages with identical bodies at
// the same instant still page without duplication.
type historyRecord struct {
	entry    task.HistoryEntry
	identity string
}

// historyEntries merges the three durable history sources.
func (adapter *Task) historyEntries(ctx context.Context, scope tenancy.Scope, identity id.TaskID, columns taskColumns) ([]historyRecord, error) {
	var records []historyRecord
	executions, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "executions",
		Equals:  map[string]any{"task_id": string(identity)},
		OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range executions {
		executionID := id.ExecutionID(textValue(row, "id"))
		records = append(records, historyRecord{
			entry: task.HistoryEntry{
				Kind:        task.HistoryExecution,
				ExecutionID: executionID,
				State:       kernel.ExecutionState(textValue(row, "state")),
				At:          timeValue(row, "created_at"),
			},
			identity: string(executionID),
		})
	}
	messages, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "task_messages",
		Equals:  map[string]any{"task_id": string(identity)},
		OrderBy: []store.Order{{Column: "at"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range messages {
		references, err := decodeJSON[[]string](bytesValue(row, "refs"))
		if err != nil {
			return nil, err
		}
		message := task.Message{
			Author:     id.PrincipalID(textValue(row, "author_id")),
			At:         timeValue(row, "at"),
			Body:       textValue(row, "body"),
			References: references,
		}
		records = append(records, historyRecord{
			entry: task.HistoryEntry{
				Kind:    task.HistoryMessage,
				Message: &message,
				At:      message.At,
			},
			identity: textValue(row, "id"),
		})
	}
	if columns.question != nil {
		question := *columns.question
		records = append(records, historyRecord{
			entry: task.HistoryEntry{
				Kind:     task.HistoryQuestion,
				Question: &question,
				At:       question.At,
			},
			identity: string(question.ID),
		})
	}
	return records, nil
}

// historyLess orders records by (at, kind, identity).
func historyLess(left, right historyRecord) bool {
	if !left.entry.At.Equal(right.entry.At) {
		return left.entry.At.Before(right.entry.At)
	}
	leftRank, rightRank := historyKindRank(string(left.entry.Kind)), historyKindRank(string(right.entry.Kind))
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	return left.identity < right.identity
}

// historyAfter reports whether one record sorts strictly after the cursor key.
func historyAfter(record historyRecord, cursor historyCursor) bool {
	switch {
	case record.entry.At.After(cursor.At):
		return true
	case record.entry.At.Before(cursor.At):
		return false
	}
	rank, cursorRank := historyKindRank(string(record.entry.Kind)), historyKindRank(cursor.Kind)
	if rank != cursorRank {
		return rank > cursorRank
	}
	return record.identity > cursor.ID
}
