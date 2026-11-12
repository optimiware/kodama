package executionstore

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// CommitReader reads one row before a commit plan is applied. Both the scoped
// executor (store.Executor.Read with its scope bound) and the cross-scope
// controller satisfy it. The read is optimistic: the unit's expected versions
// still guard every write, so a row that changed between the read and the
// commit is a version_conflict.
//
// A scoped caller binds its own tenancy scope into the callback, exactly as the
// work store unit does with its store.Executor, so the mapper never widens a
// read past the caller's scope.
type CommitReader func(ctx context.Context, table, id string) (store.Row, error)

// commitSource reads every row a commit plan needs: one row by identity and the
// ordered action rows of the execution. Both the scoped source and the
// cross-scope controller source satisfy it, so the engine commit builds its
// lifecycle pre-image through the same seam and inside the same plan as its
// guarded writes. The reads are optimistic; the unit's expected versions still
// guard every write and every projected event.
type commitSource interface {
	read(ctx context.Context, table, id string) (store.Row, error)
	scanActions(ctx context.Context, executionID id.ExecutionID) (store.QueryPage, error)
}

// Plan is the non-committing row mapping of one flow.Commit: the locks that
// serialize the affected rows in the declared order and the writes that persist
// the commit. A caller embeds them in its own store.Unit beside its own rows,
// receipt, audit and event intent.
//
// Plan never executes anything and never carries events: the caller decides the
// atomic unit and assembles any lifecycle events from
// event.ProjectLifecycle/event.ExcludeOwned, exactly as the engine, request and
// transition paths do.
type Plan struct {
	Locks  []store.Lock
	Writes []store.Write
}

// commitPlan is the complete row mapping of one flow.Commit: the execution
// update, the primary action update, every pending cancellation, the cleanup
// progress row, the locks that serialize them in the declared order and, for a
// projected engine commit, the lifecycle event publication intent that commits
// atomically with those rows.
type commitPlan struct {
	locks  []store.Lock
	writes []store.Write
	events []event.Envelope
}

// PlanScopedCommit maps one Kernel reduction to the locks and writes a scoped
// caller must embed in its own store.Unit. It is the one row mapping every
// flow.Commit-carrying Shell path shares: the execution update, the primary
// action update, every pending cancellation, the cleanup progress row and the
// locks that serialize them in the declared order. It persists every fact the
// commit carries and never invents a state: the next states, output change,
// credential change, cleanup progress, launch intent, observation, answer,
// subject, deadline and typed failure all come from the commit. A non-nil
// deadline guard refuses a late request settlement at the transaction clock.
//
// read is the caller's own scoped row read (for example a store.Executor with
// its tenancy scope bound), so the mapper never reads outside the caller's
// scope. The read is optimistic: the expected versions in Plan.Locks and
// Plan.Writes remain the authority, so a row that changed between planning and
// the caller's commit is a version_conflict, not a stale write.
//
// PlanScopedCommit performs no I/O beyond those reads, executes no unit and
// produces no events. A sibling unit (the work store unit) adds its own
// task/authority rows to the returned Plan and commits the union once, with its
// own receipt, audit and appended lifecycle projection.
func PlanScopedCommit(ctx context.Context, read CommitReader, commit flow.Commit, credential *token.Credential, deadline *store.DeadlineGuard) (Plan, error) {
	if err := validateCommitFields(commit); err != nil {
		return Plan{}, err
	}
	plan := Plan{
		Locks: []store.Lock{executionVersionLock(commit.ExecutionID, commit.ExpectedExecutionVersion)},
	}
	var primary store.Row
	if commit.ActionID != "" {
		row, err := read(ctx, tableActions, string(commit.ActionID))
		if err != nil {
			// A missing or out-of-scope primary action is the same stale-target
			// condition the write guard reports, not a new read failure.
			if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
				return Plan{}, fault.New(fault.VersionConflict, fmt.Sprintf("primary action %q is missing or out of scope", commit.ActionID))
			}
			return Plan{}, err
		}
		primary = row
		if err := validateCommitAgainstPrimary(commit, primary); err != nil {
			return Plan{}, err
		}
	}
	executionValues := map[string]any{"state": string(commit.NextExecutionState)}
	if commit.InvalidateCredential || (credential != nil && !credential.Present()) {
		executionValues["token_nonce"] = nil
		executionValues["token_expires_at"] = nil
	}
	if credential != nil && credential.Present() {
		executionValues["token_nonce"] = append([]byte(nil), credential.Nonce...)
		executionValues["token_expires_at"] = credential.Expiry
	}
	if commit.Failure != nil {
		encoded, err := encodeFailure(commit.Failure)
		if err != nil {
			return Plan{}, err
		}
		executionValues["failure"] = encoded
	}
	plan.Writes = append(plan.Writes, store.Write{
		Table:         tableExecutions,
		ID:            string(commit.ExecutionID),
		ExpectVersion: commit.ExpectedExecutionVersion,
		Values:        executionValues,
	})

	if commit.ActionID != "" {
		values, err := actionCommitValues(ctx, read, commit, primary)
		if err != nil {
			return Plan{}, err
		}
		actionLock := store.Lock{
			Table:         tableActions,
			IDs:           []string{string(commit.ActionID)},
			ExpectVersion: commit.ExpectedActionVersion,
			Deadline:      deadline,
		}
		plan.Locks = append(plan.Locks, actionLock)
		plan.Writes = append(plan.Writes, store.Write{
			Table:         tableActions,
			ID:            string(commit.ActionID),
			ExpectVersion: commit.ExpectedActionVersion,
			Values:        values,
			Deadline:      deadline,
		})
	} else if deadline != nil {
		return Plan{}, fault.New(fault.InvalidInput, "a deadline guard requires a primary action")
	}
	for _, update := range commit.PendingCancellations {
		if _, err := id.ParseActionID(string(update.ActionID)); err != nil {
			return Plan{}, fault.Wrap(fault.InvalidInput, "pending action id is invalid", err)
		}
		plan.Locks = append(plan.Locks, store.Lock{
			Table:         tableActions,
			IDs:           []string{string(update.ActionID)},
			ExpectVersion: update.ExpectedVersion,
		})
		plan.Writes = append(plan.Writes, store.Write{
			Table:         tableActions,
			ID:            string(update.ActionID),
			ExpectVersion: update.ExpectedVersion,
			Values:        map[string]any{"state": string(update.NextState)},
		})
	}
	cleanupWrite, cleanupLock, err := planCleanup(ctx, read, commit)
	if err != nil {
		return Plan{}, err
	}
	if cleanupWrite != nil {
		plan.Writes = append(plan.Writes, *cleanupWrite)
	}
	if cleanupLock != nil {
		plan.Locks = append(plan.Locks, *cleanupLock)
	}
	return plan, nil
}

// planCommit maps one Kernel reduction to exactly one store.Unit's locks and
// writes. It is implemented on top of PlanScopedCommit so every path shares the
// one row mapping; only the engine path additionally projects lifecycle events.
//
// When project is true the same plan also carries the lifecycle event
// publication intent of the commit (the cross-scope engine adapter's path). The
// projection is derived from the pre-image read through this plan's own source
// and is only attached when the commit's expected versions equal that
// pre-image, so a stale or foreign commit is still the unit's version/scope
// guard (version_conflict/forbidden) rather than a projection refusal, and it
// can never publish an event for a state change that did not commit.
func planCommit(ctx context.Context, source commitSource, commit flow.Commit, credential *token.Credential, deadline *store.DeadlineGuard, project bool) (commitPlan, error) {
	plan, err := PlanScopedCommit(ctx, source.read, commit, credential, deadline)
	if err != nil {
		return commitPlan{}, err
	}
	result := commitPlan{locks: plan.Locks, writes: plan.Writes}
	if project {
		events, err := projectedEvents(ctx, source, commit)
		if err != nil {
			return commitPlan{}, err
		}
		result.events = events
	}
	return result, nil
}

// projectedEvents builds the lifecycle event publication intent of one engine
// commit. It reads the execution pre-image (state, version, owning scope and the
// ordered action states/versions) through the plan's source, so the pre-image
// read is part of the same optimistic plan and is validated by the unit's
// expected-version locks. It calls event.ProjectLifecycle, the one shared
// projection, and maps each spec to the durable envelope the controller seam
// commits.
//
// The engine commit path is the only emitter of these classes: it carries no
// caller events of its own, so the full projection is appended with no
// ExcludeOwned filter. A no-op or cleanup-only commit projects nothing. A stale
// or foreign commit (expected version, or a named action missing from the
// pre-image) projects nothing here and is decided by the unit's guards, so it
// cannot publish an event for a change that did not commit. Engine commits carry
// no audit row: there is no caller or human actor, and audit is required only
// where a capability records one.
func projectedEvents(ctx context.Context, source commitSource, commit flow.Commit) ([]event.Envelope, error) {
	if commit.ExpectedExecutionVersion == 0 {
		return nil, fault.New(fault.InvalidInput, "commit expected execution version is zero")
	}
	row, err := source.read(ctx, tableExecutions, string(commit.ExecutionID))
	if err != nil {
		// A missing execution is the commit unit's version_conflict, not a new
		// projection failure; there is no state change to describe.
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return nil, nil
		}
		return nil, err
	}
	pre, err := executionPreImage(ctx, source, commit.ExecutionID, row)
	if err != nil {
		return nil, err
	}
	if !projectionMatchesPreImage(pre, commit) {
		return nil, nil
	}
	specs, err := event.ProjectLifecycle(pre, commit)
	if err != nil {
		return nil, err
	}
	envelopes := make([]event.Envelope, 0, len(specs))
	for _, spec := range specs {
		envelope, err := event.NewEnvelope(spec.ID, spec.Source, spec.Scope, spec.At, event.EnvelopeInput{
			Type:           spec.Type,
			SubjectID:      spec.SubjectID,
			SubjectVersion: spec.SubjectVersion,
			Data:           spec.Data,
		})
		if err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, nil
}

// executionPreImage reads the version-guarded pre-image of one execution: its
// owning scope, state and version plus every action row in index order. The
// scope is the execution row's own tenant/project, never a caller value.
func executionPreImage(ctx context.Context, source commitSource, executionID id.ExecutionID, row store.Row) (event.ExecutionPreImage, error) {
	tenantID, err := rowString(row.Values, "tenant_id")
	if err != nil {
		return event.ExecutionPreImage{}, err
	}
	projectID, err := rowString(row.Values, "project_id")
	if err != nil {
		return event.ExecutionPreImage{}, err
	}
	version, err := rowUint64(row.Values, "version")
	if err != nil {
		return event.ExecutionPreImage{}, err
	}
	state, err := rowString(row.Values, "state")
	if err != nil {
		return event.ExecutionPreImage{}, err
	}
	page, err := source.scanActions(ctx, executionID)
	if err != nil {
		return event.ExecutionPreImage{}, err
	}
	if page.HasMore {
		return event.ExecutionPreImage{}, fault.New(fault.InternalFailure, "execution has more actions than one bounded page")
	}
	actions := make([]event.ActionPreImage, 0, len(page.Rows))
	for _, actionRow := range page.Rows {
		actionID, err := rowString(actionRow.Values, "id")
		if err != nil {
			return event.ExecutionPreImage{}, err
		}
		index, err := rowInt(actionRow.Values, "index")
		if err != nil {
			return event.ExecutionPreImage{}, err
		}
		kind, err := rowString(actionRow.Values, "kind")
		if err != nil {
			return event.ExecutionPreImage{}, err
		}
		actionState, err := rowString(actionRow.Values, "state")
		if err != nil {
			return event.ExecutionPreImage{}, err
		}
		actionVersion, err := rowUint64(actionRow.Values, "version")
		if err != nil {
			return event.ExecutionPreImage{}, err
		}
		actions = append(actions, event.ActionPreImage{
			ActionID: id.ActionID(actionID),
			Index:    index,
			Kind:     definition.ActionKind(kind),
			State:    kernel.ActionState(actionState),
			Version:  actionVersion,
		})
	}
	return event.ExecutionPreImage{
		Scope:       tenancy.Scope{TenantID: id.TenantID(tenantID), ProjectID: id.ProjectID(projectID)},
		ExecutionID: executionID,
		Version:     version,
		State:       kernel.ExecutionState(state),
		Actions:     actions,
	}, nil
}

// projectionMatchesPreImage reports whether the commit names exactly the
// version-guarded rows the pre-image read. A mismatch is left to the commit
// unit's own expected-version/scope guards (version_conflict or forbidden), so
// the adapter never turns a stale or foreign commit into a projection error and
// never publishes an event for a change that did not commit.
func projectionMatchesPreImage(pre event.ExecutionPreImage, commit flow.Commit) bool {
	if commit.ExpectedExecutionVersion != pre.Version {
		return false
	}
	actions := make(map[id.ActionID]event.ActionPreImage, len(pre.Actions))
	for _, action := range pre.Actions {
		actions[action.ActionID] = action
	}
	if commit.ActionID != "" {
		action, ok := actions[commit.ActionID]
		if !ok || commit.ExpectedActionVersion != action.Version {
			return false
		}
	}
	for _, update := range commit.PendingCancellations {
		action, ok := actions[update.ActionID]
		if !ok || update.ExpectedVersion != action.Version {
			return false
		}
	}
	return true
}

// validateCommitFields checks the commit shape that needs no row read and
// refuses every action-scoped field on an execution-only commit. A field that
// would be applied nowhere is invalid_input, never a silent drop: a stop
// barrier, answer, launch intent, observation or output change must not be lost
// because the commit named no primary action.
func validateCommitFields(commit flow.Commit) error {
	if !commit.NextExecutionState.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown next execution state %q", commit.NextExecutionState))
	}
	if !commit.Output.Action.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown output action %q", commit.Output.Action))
	}
	if commit.ActionID == "" {
		// Execution-only commits settle the execution and its pending actions
		// without a primary action. Any action-scoped field would be applied
		// nowhere, so it is refused rather than silently dropped.
		if commit.NextActionState != "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf("next action state %q requires a primary action", commit.NextActionState))
		}
		if commit.ExpectedActionVersion != 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("expected action version %d requires a primary action", commit.ExpectedActionVersion))
		}
		switch commit.Output.Action {
		case flow.OutputNone, flow.OutputDiscard:
		default:
			return fault.New(fault.InvalidInput, fmt.Sprintf("output action %q requires a primary action", commit.Output.Action))
		}
		if commit.Launch != nil {
			return fault.New(fault.InvalidInput, "launch requires a primary action")
		}
		if commit.Observation != nil {
			return fault.New(fault.InvalidInput, "observation requires a primary action")
		}
		if commit.Subject != nil {
			return fault.New(fault.InvalidInput, "subject requires a primary action")
		}
		if commit.Answer != nil {
			return fault.New(fault.InvalidInput, "answer requires a primary action")
		}
		if commit.StopEvidence != nil {
			return fault.New(fault.InvalidInput, "stop evidence requires a primary action")
		}
		// With no primary action the only action state change is the pending
		// cancellations, which are always cancellations, so the post-commit
		// current action Load derives is -1.
		if commit.NextCurrentAction != nil && *commit.NextCurrentAction != -1 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("next current action %d diverges from the derived value -1", *commit.NextCurrentAction))
		}
	} else if !commit.NextActionState.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown next action state %q", commit.NextActionState))
	}
	if commit.Failure != nil && !commit.Failure.Kind.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown failure kind %q", commit.Failure.Kind))
	}
	if commit.CleanupUpdate != nil && !commit.CleanupUpdate.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown cleanup state %q", commit.CleanupUpdate.State))
	}
	if commit.Stop != nil && commit.Stop.WorkloadID == "" {
		return fault.New(fault.InvalidInput, "stop intent requires a workload identity")
	}
	if commit.Cleanup != nil && commit.Cleanup.WorkloadID == "" && commit.Cleanup.WorkspaceID == "" {
		return fault.New(fault.InvalidInput, "cleanup intent requires an owned resource identity")
	}
	if commit.ActionID != "" && commit.StopEvidence != nil && !commit.StopEvidence.State.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown stop state %q", commit.StopEvidence.State))
	}
	seen := make(map[id.ActionID]bool, len(commit.PendingCancellations))
	for _, update := range commit.PendingCancellations {
		if commit.ActionID != "" && update.ActionID == commit.ActionID {
			return fault.New(fault.InvalidInput, "pending cancellation repeats the primary action")
		}
		if seen[update.ActionID] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("pending cancellation repeats action %q", update.ActionID))
		}
		seen[update.ActionID] = true
		if update.NextState != kernel.ActionCancelled {
			return fault.New(fault.InvalidInput, fmt.Sprintf("pending action %q must be cancelled, not %q", update.ActionID, update.NextState))
		}
	}
	return nil
}

// validateCommitAgainstPrimary checks the action-scoped shape against the
// primary action row: a launch or observation belongs to a container action, a
// subject or answer belongs to a request action, and a non-nil NextCurrentAction
// must equal the index Load will derive from the single running row after this
// commit (-1 when the commit leaves no running action). A nil NextCurrentAction
// is accepted because the adapter persists no current-action column; Load
// derives the value deterministically from the action rows.
func validateCommitAgainstPrimary(commit flow.Commit, primary store.Row) error {
	index, err := rowInt(primary.Values, "index")
	if err != nil {
		return err
	}
	kind, err := rowString(primary.Values, "kind")
	if err != nil {
		return err
	}
	isContainer := kind == string(definition.ActionKindContainer)
	isRequest := kind == string(definition.ActionKindRequest)
	if commit.Launch != nil {
		if !isContainer {
			return fault.New(fault.InvalidInput, "launch intent was committed for a non-container action")
		}
		if !commit.Launch.State.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown launch state %q", commit.Launch.State))
		}
	}
	if commit.Observation != nil {
		if !isContainer {
			return fault.New(fault.InvalidInput, "observation was committed for a non-container action")
		}
		if !commit.Observation.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("inconsistent observation of kind %q", commit.Observation.Kind))
		}
	}
	if commit.Subject != nil && !isRequest {
		return fault.New(fault.InvalidInput, "subject was committed for a non-request action")
	}
	if commit.Answer != nil {
		if !isRequest {
			return fault.New(fault.InvalidInput, "answer was committed for a non-request action")
		}
		if !commit.Answer.Decision.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown answer decision %q", commit.Answer.Decision))
		}
	}
	derived := -1
	if commit.NextActionState == kernel.ActionRunning {
		derived = index
	}
	if commit.NextCurrentAction != nil && *commit.NextCurrentAction != derived {
		return fault.New(fault.InvalidInput, fmt.Sprintf("next current action %d diverges from the derived value %d", *commit.NextCurrentAction, derived))
	}
	return nil
}

// actionCommitValues renders the primary action update of one commit. primary is
// the row read before the plan was built; it supplies the activation timestamp
// the reducer would leave unchanged.
func actionCommitValues(ctx context.Context, read CommitReader, commit flow.Commit, primary store.Row) (map[string]any, error) {
	values := map[string]any{"state": string(commit.NextActionState)}
	if !commit.Deadline.IsZero() {
		values["deadline"] = commit.Deadline
	}
	switch commit.Output.Action {
	case flow.OutputNone:
	case flow.OutputStage:
		encoded, err := encodeJSON(commit.Output.Values)
		if err != nil {
			return nil, err
		}
		values["staged_outputs"] = encoded
		values["staged_outputs_set"] = true
	case flow.OutputPromote:
		encoded, err := encodeJSON(commit.Output.Values)
		if err != nil {
			return nil, err
		}
		values["outputs"] = encoded
		values["staged_outputs"] = nil
		values["staged_outputs_set"] = false
	case flow.OutputDiscard:
		values["staged_outputs"] = nil
		values["staged_outputs_set"] = false
	default:
		return nil, fault.New(fault.InvalidInput, "transition commit carries an unknown output action")
	}
	if commit.Subject != nil {
		encoded, err := encodeJSON(commit.Subject)
		if err != nil {
			return nil, err
		}
		values["request_subject"] = encoded
		// The admitted assignment is immutable, so it never changes after
		// admission. Rewriting the one shared encoding beside the resolved
		// subject keeps an engine-activated request row decodable even when the
		// row was inserted by a writer that did not carry the column.
		assignment, err := admittedRequestAssignment(ctx, read, commit.ExecutionID, primary)
		if err != nil {
			return nil, err
		}
		values["request_assignment"] = assignment
	}
	if commit.StopEvidence != nil {
		encoded, err := encodeStop(*commit.StopEvidence)
		if err != nil {
			return nil, err
		}
		values["stop"] = encoded
	}
	if commit.Launch != nil {
		if !commit.Launch.State.Valid() {
			return nil, fault.New(fault.InvalidInput, "commit launch intent carries an unknown launch state")
		}
		values["workload_id"] = nullableText(string(commit.Launch.WorkloadID))
		values["launch_state"] = string(commit.Launch.State)
		values["launch_key"] = nullableText(commit.Launch.IdempotencyKey)
	}
	if commit.Observation != nil {
		encoded, err := encodeObservation(*commit.Observation)
		if err != nil {
			return nil, err
		}
		values["observation"] = encoded
	}
	if commit.NextActionState == kernel.ActionRunning && !commit.At.IsZero() && primary.Values["activated_at"] == nil {
		values["activated_at"] = commit.At
	}
	if commit.Answer != nil {
		if !commit.Answer.Decision.Valid() {
			return nil, fault.New(fault.InvalidInput, "commit answer carries an unknown decision")
		}
		values["request_decision"] = string(commit.Answer.Decision)
		values["request_actor"] = string(commit.Answer.Actor)
		values["request_resolved_at"] = commit.Answer.At
		if len(commit.Answer.Provenance) > 0 {
			values["answer_provenance"] = append([]byte(nil), commit.Answer.Provenance...)
		} else {
			values["answer_provenance"] = nil
		}
		// The schema shares actions.outputs between container outputs and
		// request responses, so a failed answer's retained response is stored
		// here and Load keeps it off Action.Outputs for a failing decision.
		if commit.Answer.Outputs == nil {
			values["outputs"] = nil
		} else {
			encoded, err := encodeJSON(commit.Answer.Outputs)
			if err != nil {
				return nil, err
			}
			values["outputs"] = encoded
		}
	}
	return values, nil
}

// admittedRequestAssignment returns the exact actions.request_assignment bytes
// of the request action a commit activates. It reads the immutable admitted
// snapshot stored on the execution row and encodes the same assignment the
// start insert wrote, so every writer of the column uses one encoder. The
// primary row only names the action index; the definition lives in the
// snapshot document (the schema has no definition column).
func admittedRequestAssignment(ctx context.Context, read CommitReader, executionID id.ExecutionID, primary store.Row) ([]byte, error) {
	index, err := rowInt(primary.Values, "index")
	if err != nil {
		return nil, err
	}
	row, err := read(ctx, tableExecutions, string(executionID))
	if err != nil {
		return nil, err
	}
	data, err := rowBytes(row.Values, "snapshot")
	if err != nil {
		return nil, err
	}
	admitted, err := snapshotdoc.Decode(data)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(admitted.Actions) {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("admitted action index %d is outside the execution snapshot", index))
	}
	action := admitted.Actions[index]
	if action.Index != index {
		return nil, fault.New(fault.InternalFailure, "admitted action index does not match its position")
	}
	if action.Kind != definition.ActionKindRequest {
		return nil, fault.New(fault.InternalFailure, "request subject was committed for a non-request admitted action")
	}
	name, err := rowString(primary.Values, "name")
	if err != nil {
		return nil, err
	}
	if name != string(action.Name) {
		return nil, fault.New(fault.InternalFailure, "admitted action name does not match its row")
	}
	assignment, err := snapshotdoc.RequestAssignment(action)
	if err != nil {
		return nil, err
	}
	if assignment == nil {
		return nil, fault.New(fault.InternalFailure, "admitted request action has no assignment")
	}
	return assignment, nil
}

// Cleanup retry scheduling bounds how quickly a failing cleanup is reattempted.
// A failed attempt is delayed for a deterministic exponential interval derived
// from its persisted attempt count, so a short scan cadence cannot spend the
// Kernel's finite attempt budget in one burst. There is no randomness and the
// only clock is the commit's own authoritative time.
const (
	// cleanupRetryBaseDelay is the delay after the first failed attempt.
	cleanupRetryBaseDelay = time.Second
	// cleanupRetryMaxDelay caps the delay after later failed attempts.
	cleanupRetryMaxDelay = 30 * time.Second
)

// cleanupRetryDelay returns the durable delay before the next cleanup attempt
// after attempts failed attempts: min(base * 2^(attempts-1), cap). A
// non-positive attempt count has no failed attempt to back off from.
func cleanupRetryDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	delay := cleanupRetryBaseDelay
	for step := 1; step < attempts; step++ {
		if delay >= cleanupRetryMaxDelay {
			break
		}
		delay *= 2
	}
	if delay > cleanupRetryMaxDelay {
		delay = cleanupRetryMaxDelay
	}
	return delay
}

// planCleanup maps the commit's cleanup intent/progress to one durable cleanup
// row write. The row is created on the first intent and updated in place.
//
// next_retry_at is the durable retry schedule the unfinished scan honors. A
// failed attempt (the new attempt count exceeds the persisted one) is delayed by
// cleanupRetryDelay(new attempts); the initial intent and progress that spends
// no attempt (an unconfirmed-stop acknowledgment, the exhaustion diagnostic)
// stay promptly eligible at commit.At so stop observation keeps progressing.
// A commit that carries no cleanup update writes nothing here, so an unrelated
// commit preserves the persisted schedule.
func planCleanup(ctx context.Context, read CommitReader, commit flow.Commit) (*store.Write, *store.Lock, error) {
	update := commit.CleanupUpdate
	intent := commit.Cleanup
	if update == nil && intent == nil {
		return nil, nil, nil
	}
	identity := cleanupID(commit.ExecutionID)
	missing := false
	previousAttempts := 0
	existing, err := read(ctx, tableCleanup, identity)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			missing = true
		} else {
			return nil, nil, err
		}
	} else {
		previousAttempts, err = rowInt(existing.Values, "attempts")
		if err != nil {
			return nil, nil, err
		}
	}
	values := map[string]any{}
	if update != nil {
		values["state"] = string(update.State)
		values["workload_id"] = nullableText(string(update.WorkloadID))
		values["workspace_id"] = nullableText(string(update.WorkspaceID))
		values["attempts"] = int64(update.Attempts)
		values["last_error"] = nullableText(update.LastError)
		if update.State == kernel.CleanupPending {
			if update.Attempts > previousAttempts {
				values["next_retry_at"] = commit.At.Add(cleanupRetryDelay(update.Attempts))
			} else {
				values["next_retry_at"] = commit.At
			}
		} else {
			values["next_retry_at"] = nil
		}
	} else {
		values["state"] = string(kernel.CleanupPending)
		values["workload_id"] = nullableText(string(intent.WorkloadID))
		values["workspace_id"] = nullableText(string(intent.WorkspaceID))
		values["attempts"] = int64(0)
		values["last_error"] = nil
		values["next_retry_at"] = commit.At
	}
	if missing {
		values["execution_id"] = string(commit.ExecutionID)
		return &store.Write{Table: tableCleanup, ID: identity, Insert: true, Values: values}, nil, nil
	}
	lock := store.Lock{Table: tableCleanup, IDs: []string{identity}}
	return &store.Write{
		Table:  tableCleanup,
		ID:     identity,
		Expect: map[string]any{"execution_id": string(commit.ExecutionID)},
		Values: values,
	}, &lock, nil
}
