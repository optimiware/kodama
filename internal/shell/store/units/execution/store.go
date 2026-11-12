package executionstore

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Store is the execution and request store adapter. It maps complete commit
// units to exactly one store.Unit and reads only through the scoped seams.
type Store struct {
	executor store.Executor
	scanner  store.Scanner
}

var (
	_ execution.StateReader    = (*Store)(nil)
	_ execution.Reader         = (*Store)(nil)
	_ execution.StartPort      = (*Store)(nil)
	_ execution.TransitionPort = (*Store)(nil)
	_ request.StateReader      = (*Store)(nil)
	_ request.ActivationPort   = (*Store)(nil)
	_ request.ResolvePort      = (*Store)(nil)
)

// New returns one adapter over the neutral executor and scanner seams.
func New(executor store.Executor, scanner store.Scanner) *Store {
	return &Store{executor: executor, scanner: scanner}
}

// aggregateSource reads the rows of one execution aggregate. The scoped store
// reads under a verified tenancy scope; the Kernel controller reads cross-scope
// and derives the owning scope only for writes.
type aggregateSource interface {
	read(ctx context.Context, table, id string) (store.Row, error)
	scanActions(ctx context.Context, executionID id.ExecutionID) (store.QueryPage, error)
}

// scopedSource reads one aggregate inside a verified tenancy scope.
type scopedSource struct {
	executor store.Executor
	scanner  store.Scanner
	scope    tenancy.Scope
}

func (source scopedSource) read(ctx context.Context, table, id string) (store.Row, error) {
	return source.executor.Read(ctx, source.scope, table, id)
}

func (source scopedSource) scanActions(ctx context.Context, executionID id.ExecutionID) (store.QueryPage, error) {
	return source.scanner.Query(ctx, source.scope, actionsQuery(executionID))
}

// controllerSource reads one aggregate through the cross-scope Kernel seam.
type controllerSource struct {
	controller store.Controller
}

func (source controllerSource) read(ctx context.Context, table, id string) (store.Row, error) {
	return source.controller.Read(ctx, table, id)
}

func (source controllerSource) scanActions(ctx context.Context, executionID id.ExecutionID) (store.QueryPage, error) {
	return source.controller.Scan(ctx, store.ControllerScan{ScanQuery: actionsQuery(executionID)})
}

// actionsQuery names the ordered action rows of one execution.
func actionsQuery(executionID id.ExecutionID) store.ScanQuery {
	return store.ScanQuery{
		Table:   tableActions,
		Equals:  map[string]any{"execution_id": string(executionID)},
		OrderBy: []store.Order{{Column: "index"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	}
}

// Load returns one scoped execution aggregate: the whole version-guarded
// execution (state, version, current action and every action row), not a lossy
// view, so the task service can build the Kernel cancel fact from it.
//
// The read is scoped to the caller's tenancy scope. An execution owned by
// another tenant or project, an unknown execution and a missing execution are
// uniformly not_found, never forbidden, so the read exposes no existence
// oracle. It satisfies the consumer-owned task.ExecutionAggregateReader port.
func (s *Store) Load(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Execution{}, err
	}
	if err := scope.Validate(); err != nil {
		return kernel.Execution{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return kernel.Execution{}, fault.Wrap(fault.InvalidInput, "execution id is invalid", err)
	}
	source := scopedSource{executor: s.executor, scanner: s.scanner, scope: scope}
	return loadAggregate(ctx, source, executionID)
}

// loadAggregate rebuilds one kernel execution from its relational rows and the
// immutable admitted snapshot document.
func loadAggregate(ctx context.Context, source aggregateSource, executionID id.ExecutionID) (kernel.Execution, error) {
	row, err := source.read(ctx, tableExecutions, string(executionID))
	if err != nil {
		return kernel.Execution{}, err
	}
	return aggregateFromRow(ctx, source, row)
}

// aggregateFromRow rebuilds one kernel execution from one executions row.
func aggregateFromRow(ctx context.Context, source aggregateSource, row store.Row) (kernel.Execution, error) {
	aggregate, _, err := aggregateFromRowWithAssignments(ctx, source, row)
	return aggregate, err
}

// aggregateFromRowWithAssignments rebuilds one kernel execution from one
// executions row and also returns the exact persisted request assignment of
// every request action, keyed by action identity. The assignment is the durable
// inspect fact; it is never inferred from the admitted declaration.
func aggregateFromRowWithAssignments(ctx context.Context, source aggregateSource, row store.Row) (kernel.Execution, map[id.ActionID]request.Assignment, error) {
	executionID, err := rowString(row.Values, "id")
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	version, err := rowUint64(row.Values, "version")
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	state, err := rowString(row.Values, "state")
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	workspace, err := rowString(row.Values, "workspace_id")
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	snapshotData, err := rowBytes(row.Values, "snapshot")
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	snapshot, err := decodeSnapshot(snapshotData)
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	failure, err := decodeFailure(row.Values["failure"])
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	actions, current, assignments, err := actionsOf(ctx, source, id.ExecutionID(executionID), snapshot.Pipeline().Actions, id.WorkspaceID(workspace))
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	cleanup, err := cleanupOf(ctx, source, id.ExecutionID(executionID))
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	return kernel.Execution{
		ID:            id.ExecutionID(executionID),
		Version:       version,
		State:         kernel.ExecutionState(state),
		WorkspaceID:   id.WorkspaceID(workspace),
		Snapshot:      snapshot,
		Actions:       actions,
		CurrentAction: current,
		Failure:       failure,
		Cleanup:       cleanup,
	}, assignments, nil
}

// actionsOf reads the ordered action rows and pairs each with its admitted
// definition. CurrentAction is derived from the single running action row. The
// persisted request assignment of each request action is returned beside the
// aggregate.
func actionsOf(ctx context.Context, source aggregateSource, executionID id.ExecutionID, definitions []definition.Action, workspace id.WorkspaceID) ([]kernel.Action, int, map[id.ActionID]request.Assignment, error) {
	page, err := source.scanActions(ctx, executionID)
	if err != nil {
		return nil, 0, nil, err
	}
	actions := make([]kernel.Action, 0, len(page.Rows))
	assignments := make(map[id.ActionID]request.Assignment, len(page.Rows))
	current := -1
	for _, row := range page.Rows {
		action, err := decodeAction(row.Values, definitions, workspace)
		if err != nil {
			return nil, 0, nil, err
		}
		if action.State == kernel.ActionRunning {
			if current >= 0 {
				return nil, 0, nil, fault.New(fault.InternalFailure, "execution has more than one running action")
			}
			current = action.Index
		}
		if action.Kind == definition.ActionKindRequest {
			var assignment request.Assignment
			if err := decodeJSON(row.Values["request_assignment"], &assignment); err != nil {
				return nil, 0, nil, err
			}
			if !assignment.Valid() {
				return nil, 0, nil, fault.New(fault.InternalFailure, "request action has no readable assignment")
			}
			assignments[action.ID] = assignment
		}
		actions = append(actions, action)
	}
	if page.HasMore {
		return nil, 0, nil, fault.New(fault.InternalFailure, "execution has more actions than one bounded page")
	}
	return actions, current, assignments, nil
}

// decodeAction rebuilds one kernel action from its row and admitted definition.
func decodeAction(values map[string]any, definitions []definition.Action, workspace id.WorkspaceID) (kernel.Action, error) {
	actionIDText, err := rowString(values, "id")
	if err != nil {
		return kernel.Action{}, err
	}
	index, err := rowInt(values, "index")
	if err != nil {
		return kernel.Action{}, err
	}
	name, err := rowString(values, "name")
	if err != nil {
		return kernel.Action{}, err
	}
	kind, err := rowString(values, "kind")
	if err != nil {
		return kernel.Action{}, err
	}
	state, err := rowString(values, "state")
	if err != nil {
		return kernel.Action{}, err
	}
	version, err := rowUint64(values, "version")
	if err != nil {
		return kernel.Action{}, err
	}
	timeoutMS, err := rowUint64(values, "timeout_ms")
	if err != nil {
		return kernel.Action{}, err
	}
	deadline, err := rowTime(values, "deadline")
	if err != nil {
		return kernel.Action{}, err
	}
	activatedAt, err := rowTime(values, "activated_at")
	if err != nil {
		return kernel.Action{}, err
	}
	stagedSet, err := rowBool(values, "staged_outputs_set")
	if err != nil {
		return kernel.Action{}, err
	}
	stop, err := decodeStop(values["stop"])
	if err != nil {
		return kernel.Action{}, err
	}
	observation, hasObservation, err := decodeObservation(values["observation"])
	if err != nil {
		return kernel.Action{}, err
	}
	workloadID, err := rowString(values, "workload_id")
	if err != nil {
		return kernel.Action{}, err
	}
	launchState, err := rowString(values, "launch_state")
	if err != nil {
		return kernel.Action{}, err
	}
	launchKey, err := rowString(values, "launch_key")
	if err != nil {
		return kernel.Action{}, err
	}
	var outputs kernel.Outputs
	if err := decodeJSON(values["outputs"], &outputs); err != nil {
		return kernel.Action{}, err
	}
	var staged kernel.Outputs
	if err := decodeJSON(values["staged_outputs"], &staged); err != nil {
		return kernel.Action{}, err
	}
	action := kernel.Action{
		ID:               id.ActionID(actionIDText),
		Index:            index,
		Name:             id.ActionName(name),
		Kind:             definition.ActionKind(kind),
		State:            kernel.ActionState(state),
		Version:          version,
		Timeout:          time.Duration(timeoutMS) * time.Millisecond,
		ActivatedAt:      activatedAt,
		Deadline:         deadline,
		Outputs:          outputs,
		StagedOutputs:    staged,
		StagedOutputsSet: stagedSet,
		Stop:             stop,
	}
	if index < 0 || index >= len(definitions) {
		return kernel.Action{}, fault.New(fault.InternalFailure, fmt.Sprintf("action %q has no admitted definition", actionIDText))
	}
	admitted := definitions[index]
	admittedKind := definition.ActionKindRequest
	if admitted.Container != nil {
		admittedKind = definition.ActionKindContainer
	}
	if admitted.Name != action.Name || admittedKind != action.Kind {
		return kernel.Action{}, fault.New(fault.InternalFailure, fmt.Sprintf("action %q does not match its admitted definition", actionIDText))
	}
	switch action.Kind {
	case definition.ActionKindContainer:
		container := &kernel.ContainerAction{
			Definition:  cloneContainer(admitted.Container),
			WorkloadID:  id.WorkloadID(workloadID),
			WorkspaceID: workspace,
		}
		// A launch intent exists only after activation persisted one. The
		// launch_state column is NOT NULL, so the presence of the identity or
		// key (not the default enum) is the marker.
		if workloadID != "" || launchKey != "" {
			container.Launch = kernel.LaunchIntent{
				WorkloadID:     id.WorkloadID(workloadID),
				State:          kernel.LaunchState(launchState),
				IdempotencyKey: launchKey,
			}
		}
		if hasObservation {
			container.Observation = observation
		}
		action.Container = container
	case definition.ActionKindRequest:
		requestAction := &kernel.RequestAction{Definition: cloneRequest(admitted.Request)}
		var subject definition.Subject
		if err := decodeJSON(values["request_subject"], &subject); err != nil {
			return kernel.Action{}, err
		}
		if subject.Len() > 0 {
			resolved := subject
			requestAction.Subject = &resolved
		}
		decision, err := rowString(values, "request_decision")
		if err != nil {
			return kernel.Action{}, err
		}
		if decision != "" {
			actor, err := rowString(values, "request_actor")
			if err != nil {
				return kernel.Action{}, err
			}
			at, err := rowTime(values, "request_resolved_at")
			if err != nil {
				return kernel.Action{}, err
			}
			provenance, err := rowBytes(values, "answer_provenance")
			if err != nil {
				return kernel.Action{}, err
			}
			requestAction.Answer = &kernel.Answer{
				Decision:   kernel.Decision(decision),
				Outputs:    outputs,
				Actor:      id.PrincipalID(actor),
				At:         at,
				Provenance: provenance,
			}
			// A failed answer keeps its response inspectable but never
			// promotes it to settled downstream outputs.
			if kernel.Decision(decision) == kernel.DecisionFail {
				action.Outputs = nil
			}
		}
		action.Request = requestAction
	default:
		return kernel.Action{}, fault.New(fault.InternalFailure, fmt.Sprintf("action %q has unknown kind %q", actionIDText, kind))
	}
	return action, nil
}

func cloneContainer(value *definition.Container) definition.Container {
	if value == nil {
		return definition.Container{}
	}
	clone := *value
	clone.Commands = append([]string(nil), value.Commands...)
	clone.Env = append([]definition.EnvVar(nil), value.Env...)
	clone.Secrets = append([]id.SecretName(nil), value.Secrets...)
	clone.Outputs = append([]id.OutputKey(nil), value.Outputs...)
	return clone
}

func cloneRequest(value *definition.Request) definition.Request {
	if value == nil {
		return definition.Request{}
	}
	clone := *value
	clone.Outputs = append([]id.OutputKey(nil), value.Outputs...)
	return clone
}

// cleanupOf reads the durable cleanup row. A missing row means no cleanup is owed.
func cleanupOf(ctx context.Context, source aggregateSource, executionID id.ExecutionID) (kernel.Cleanup, error) {
	row, err := source.read(ctx, tableCleanup, cleanupID(executionID))
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return kernel.Cleanup{State: kernel.CleanupNone}, nil
		}
		return kernel.Cleanup{}, err
	}
	state, err := rowString(row.Values, "state")
	if err != nil {
		return kernel.Cleanup{}, err
	}
	workload, err := rowString(row.Values, "workload_id")
	if err != nil {
		return kernel.Cleanup{}, err
	}
	workspace, err := rowString(row.Values, "workspace_id")
	if err != nil {
		return kernel.Cleanup{}, err
	}
	attempts, err := rowInt(row.Values, "attempts")
	if err != nil {
		return kernel.Cleanup{}, err
	}
	lastError, err := rowString(row.Values, "last_error")
	if err != nil {
		return kernel.Cleanup{}, err
	}
	return kernel.Cleanup{
		State:       kernel.CleanupState(state),
		WorkloadID:  id.WorkloadID(workload),
		WorkspaceID: id.WorkspaceID(workspace),
		Attempts:    attempts,
		LastError:   lastError,
	}, nil
}

// Inspect returns the credential-free inspection projection of one execution.
func (s *Store) Inspect(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (execution.View, error) {
	if err := ctx.Err(); err != nil {
		return execution.View{}, err
	}
	if err := scope.Validate(); err != nil {
		return execution.View{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return execution.View{}, fault.Wrap(fault.InvalidInput, "execution id is invalid", err)
	}
	source := scopedSource{executor: s.executor, scanner: s.scanner, scope: scope}
	row, err := source.read(ctx, tableExecutions, string(executionID))
	if err != nil {
		return execution.View{}, err
	}
	aggregate, assignments, err := aggregateFromRowWithAssignments(ctx, source, row)
	if err != nil {
		return execution.View{}, err
	}
	return view(scope, row, aggregate, assignments)
}

// List returns one stable scoped execution page. The scanner applies the scope
// and keyset pagination; the adapter only maps the filter to declared columns.
func (s *Store) List(ctx context.Context, scope tenancy.Scope, filter execution.ListFilter) (execution.Page, error) {
	if err := ctx.Err(); err != nil {
		return execution.Page{}, err
	}
	if err := scope.Validate(); err != nil {
		return execution.Page{}, err
	}
	limit := filter.Limit
	if limit == 0 {
		limit = store.MaxQueryLimit
	}
	if limit < 1 || limit > store.MaxQueryLimit {
		return execution.Page{}, fault.New(fault.InvalidInput, fmt.Sprintf("execution list limit %d is outside 1..%d", limit, store.MaxQueryLimit))
	}
	query := store.ScanQuery{
		Table:   tableExecutions,
		Equals:  make(map[string]any, 3),
		OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
		After:   filter.Cursor,
		Limit:   limit,
	}
	if filter.TaskID != "" {
		query.Equals["task_id"] = string(filter.TaskID)
	}
	if filter.PipelineID != "" {
		query.Equals["pipeline_id"] = string(filter.PipelineID)
	}
	if filter.State != "" {
		query.Equals["state"] = string(filter.State)
	}
	page, err := s.scanner.Query(ctx, scope, query)
	if err != nil {
		return execution.Page{}, err
	}
	source := scopedSource{executor: s.executor, scanner: s.scanner, scope: scope}
	result := execution.Page{Items: make([]execution.View, 0, len(page.Rows))}
	for _, row := range page.Rows {
		aggregate, assignments, err := aggregateFromRowWithAssignments(ctx, source, row)
		if err != nil {
			return execution.Page{}, err
		}
		projection, err := view(scope, row, aggregate, assignments)
		if err != nil {
			return execution.Page{}, err
		}
		result.Items = append(result.Items, projection)
	}
	if page.HasMore {
		result.Next = page.Next
	}
	return result, nil
}

// view projects one kernel aggregate into the credential-free inspection view.
// assignments carries the exact persisted request assignment of every request
// action, so a responder never needs a second read.
func view(scope tenancy.Scope, row store.Row, aggregate kernel.Execution, assignments map[id.ActionID]request.Assignment) (execution.View, error) {
	pipelineID, err := rowString(row.Values, "pipeline_id")
	if err != nil {
		return execution.View{}, err
	}
	taskID, err := rowString(row.Values, "task_id")
	if err != nil {
		return execution.View{}, err
	}
	projection := execution.View{
		Owner:         scope,
		ExecutionID:   aggregate.ID,
		TaskID:        id.TaskID(taskID),
		PipelineID:    pipeline.PipelineID(pipelineID),
		State:         aggregate.State,
		Version:       aggregate.Version,
		CurrentAction: aggregate.CurrentAction,
		WorkspaceID:   aggregate.WorkspaceID,
		Cleanup:       aggregate.Cleanup,
		Failure:       aggregate.Failure,
	}
	if aggregate.Snapshot != nil && projection.TaskID == "" {
		projection.TaskID = aggregate.Snapshot.TaskID()
	}
	projection.Actions = make([]execution.ActionView, 0, len(aggregate.Actions))
	for _, action := range aggregate.Actions {
		actionView := execution.ActionView{
			ID:               action.ID,
			Index:            action.Index,
			Name:             action.Name,
			Kind:             action.Kind,
			State:            action.State,
			Version:          action.Version,
			Timeout:          action.Timeout,
			ActivatedAt:      action.ActivatedAt,
			Deadline:         action.Deadline,
			Outputs:          action.Outputs,
			StagedOutputs:    action.StagedOutputs,
			StagedOutputsSet: action.StagedOutputsSet,
			Stop:             action.Stop,
		}
		if action.Request != nil {
			actionView.Subject = action.Request.Subject
			actionView.Assignment = assignments[action.ID]
			if action.Request.Answer != nil {
				actionView.Decision = string(action.Request.Answer.Decision)
				actionView.Actor = action.Request.Answer.Actor
				actionView.AnsweredAt = action.Request.Answer.At
			}
		}
		projection.Actions = append(projection.Actions, actionView)
	}
	sort.SliceStable(projection.Actions, func(left, right int) bool {
		return projection.Actions[left].Index < projection.Actions[right].Index
	})
	return projection, nil
}
