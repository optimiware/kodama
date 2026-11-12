package testkit

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// Store is the in-memory durable state behind the fake State port. It owns the
// persisted executions, the recorded intents and the commit counter. A State
// handle is only a view over the Store, so dropping a controller instance and
// asking for a new handle simulates a restart without losing committed state.
//
// The Store is safe for concurrent use. Every commit is validated completely
// before any field or side effect is written, so a version conflict or an
// injected storage failure leaves the whole transition rolled back.
type Store struct {
	mu sync.Mutex

	executions  map[id.ExecutionID]kernel.Execution
	commits     int
	failCommit  error
	invalidated map[id.ExecutionID]bool
	stops       map[id.ExecutionID][]flow.StopIntent
	cleanups    map[id.ExecutionID][]flow.CleanupIntent
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		executions:  make(map[id.ExecutionID]kernel.Execution),
		invalidated: make(map[id.ExecutionID]bool),
		stops:       make(map[id.ExecutionID][]flow.StopIntent),
		cleanups:    make(map[id.ExecutionID][]flow.CleanupIntent),
	}
}

// Handle returns a fresh State handle over the same persisted data.
func (s *Store) Handle() *State {
	return &State{store: s}
}

// Seed admits executions directly, bypassing a transition commit. It is the
// test's admission shortcut; the values are deep-copied so later caller
// mutation cannot reach the store.
func (s *Store) Seed(executions ...kernel.Execution) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, execution := range executions {
		s.executions[execution.ID] = cloneExecution(execution)
		delete(s.invalidated, execution.ID)
		delete(s.stops, execution.ID)
		delete(s.cleanups, execution.ID)
	}
}

// Execution returns a defensive copy of one seeded or committed execution.
func (s *Store) Execution(executionID id.ExecutionID) (kernel.Execution, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	execution, ok := s.executions[executionID]
	if !ok {
		return kernel.Execution{}, false
	}
	return cloneExecution(execution), true
}

// CommitCount returns the number of successful commits across every handle.
func (s *Store) CommitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

// FailNextCommit injects one storage failure into the next commit attempt. The
// failed commit is not applied and is not counted.
func (s *Store) FailNextCommit(err error) {
	if err == nil {
		err = fmt.Errorf("testkit: injected commit failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCommit = err
}

// CredentialInvalidated reports whether a commit recorded the credential
// invalidation intent for the execution.
func (s *Store) CredentialInvalidated(executionID id.ExecutionID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidated[executionID]
}

// PendingStops returns the recorded stop intents in deterministic execution
// order. A confirmed stop barrier or completed cleanup satisfies the intent and
// removes it.
func (s *Store) PendingStops() []flow.StopIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []flow.StopIntent
	for _, executionID := range sortedExecutionIDs(s.stops) {
		pending = append(pending, s.stops[executionID]...)
	}
	return pending
}

// PendingCleanups returns the recorded cleanup intents in deterministic
// execution order.
func (s *Store) PendingCleanups() []flow.CleanupIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending []flow.CleanupIntent
	for _, executionID := range sortedExecutionIDs(s.cleanups) {
		pending = append(pending, s.cleanups[executionID]...)
	}
	return pending
}

func sortedExecutionIDs[T any](groups map[id.ExecutionID][]T) []id.ExecutionID {
	ids := make([]id.ExecutionID, 0, len(groups))
	for executionID := range groups {
		ids = append(ids, executionID)
	}
	slices.Sort(ids)
	return ids
}

// State is one controller's view over a Store. It implements engine.State.
type State struct {
	store *Store
}

// Load returns the persisted execution or a not-found fault. The result is a
// defensive copy.
func (s *State) Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Execution{}, err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	execution, ok := s.store.executions[executionID]
	if !ok {
		return kernel.Execution{}, fault.New(fault.NotFound, fmt.Sprintf("execution %q was not admitted", executionID))
	}
	return cloneExecution(execution), nil
}

// Commit applies exactly one complete compare-and-commit transition. Expected
// versions are checked before anything is written, so a stale execution, a
// stale primary action or a stale pending action rolls back the whole payload.
func (s *State) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Execution{}, err
	}

	store := s.store
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.failCommit != nil {
		err := store.failCommit
		store.failCommit = nil
		return kernel.Execution{}, err
	}

	current, ok := store.executions[commit.ExecutionID]
	if !ok {
		return kernel.Execution{}, fault.New(fault.NotFound, fmt.Sprintf("execution %q was not admitted", commit.ExecutionID))
	}
	if current.Version != commit.ExpectedExecutionVersion {
		return kernel.Execution{}, versionConflict(
			"execution %q is at version %d, commit expected %d",
			commit.ExecutionID, current.Version, commit.ExpectedExecutionVersion,
		)
	}
	// An execution-only commit has no primary action. It still checks the
	// execution version and every pending-action version, and any conflict
	// rolls back the whole payload before anything is written.
	if commit.ActionID != "" {
		primaryIndex := actionIndex(current, commit.ActionID)
		if primaryIndex < 0 {
			return kernel.Execution{}, versionConflict("action %q is not part of execution %q", commit.ActionID, commit.ExecutionID)
		}
		if current.Actions[primaryIndex].Version != commit.ExpectedActionVersion {
			return kernel.Execution{}, versionConflict(
				"action %q is at version %d, commit expected %d",
				commit.ActionID, current.Actions[primaryIndex].Version, commit.ExpectedActionVersion,
			)
		}
	}
	for _, update := range commit.PendingCancellations {
		index := actionIndex(current, update.ActionID)
		if index < 0 {
			return kernel.Execution{}, versionConflict("action %q is not part of execution %q", update.ActionID, commit.ExecutionID)
		}
		if current.Actions[index].Version != update.ExpectedVersion {
			return kernel.Execution{}, versionConflict(
				"pending action %q is at version %d, commit expected %d",
				update.ActionID, current.Actions[index].Version, update.ExpectedVersion,
			)
		}
	}
	if err := validateCommit(current, commit); err != nil {
		return kernel.Execution{}, err
	}

	updated := applyCommit(current, commit)
	store.executions[commit.ExecutionID] = updated
	store.commits++

	if commit.InvalidateCredential {
		store.invalidated[commit.ExecutionID] = true
	}
	if commit.Stop != nil {
		store.stops[commit.ExecutionID] = append(store.stops[commit.ExecutionID], *commit.Stop)
	}
	if commit.Cleanup != nil {
		store.cleanups[commit.ExecutionID] = append(store.cleanups[commit.ExecutionID], *commit.Cleanup)
	}
	if commit.StopEvidence != nil && commit.StopEvidence.IsConfirmed() {
		delete(store.stops, commit.ExecutionID)
	}
	if commit.CleanupUpdate != nil && commit.CleanupUpdate.State == kernel.CleanupComplete {
		delete(store.stops, commit.ExecutionID)
		delete(store.cleanups, commit.ExecutionID)
	}

	return cloneExecution(updated), nil
}

func validateCommit(current kernel.Execution, commit flow.Commit) error {
	if !commit.NextExecutionState.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown next execution state %q", commit.NextExecutionState))
	}
	if !commit.Output.Action.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown output action %q", commit.Output.Action))
	}
	if commit.NextCurrentAction != nil {
		next := *commit.NextCurrentAction
		if next < -1 || next >= len(current.Actions) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("next current action %d is outside the action list", next))
		}
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
	} else {
		if !commit.NextActionState.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown next action state %q", commit.NextActionState))
		}
		primary := current.Actions[actionIndex(current, commit.ActionID)]
		if commit.Launch != nil {
			if primary.Container == nil {
				return fault.New(fault.InvalidInput, "launch intent was committed for a non-container action")
			}
			if !commit.Launch.State.Valid() {
				return fault.New(fault.InvalidInput, fmt.Sprintf("unknown launch state %q", commit.Launch.State))
			}
		}
		if commit.Observation != nil {
			if primary.Container == nil {
				return fault.New(fault.InvalidInput, "observation was committed for a non-container action")
			}
			if !commit.Observation.Valid() {
				return fault.New(fault.InvalidInput, fmt.Sprintf("inconsistent observation of kind %q", commit.Observation.Kind))
			}
		}
		if primary.Container != nil {
			if err := validateRetireCommit(primary, commit); err != nil {
				return err
			}
		}
		if commit.StopEvidence != nil && !commit.StopEvidence.State.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown stop state %q", commit.StopEvidence.State))
		}
		if commit.Subject != nil && primary.Request == nil {
			return fault.New(fault.InvalidInput, "subject was committed for a non-request action")
		}
		if commit.Answer != nil {
			if primary.Request == nil {
				return fault.New(fault.InvalidInput, "answer was committed for a non-request action")
			}
			if !commit.Answer.Decision.Valid() {
				return fault.New(fault.InvalidInput, fmt.Sprintf("unknown answer decision %q", commit.Answer.Decision))
			}
		}
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

// validateRetireCommit enforces the retire-then-replace protocol at
// the fake state boundary. A retirement must persist the affirmative
// nonexecution proof for the action's current identity in the same payload, and
// a retiring identity is launch-terminal: it accepts only its identical proof
// or exactly one legal replacement identity (not_issued, a new workload id, a
// non-empty idempotency key and at most an unknown observation). Stop evidence,
// cleanup progress and settlement commits carry neither field and stay legal.
func validateRetireCommit(primary kernel.Action, commit flow.Commit) error {
	container := primary.Container

	if commit.Launch != nil && commit.Launch.State == kernel.LaunchRetiring {
		if commit.Launch.WorkloadID != container.WorkloadID {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"retiring workload %q is not the current identity %q of action %q",
				commit.Launch.WorkloadID, container.WorkloadID, primary.ID,
			))
		}
		if commit.Observation == nil || !commit.Observation.ProofOfNonexecution() {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"retiring action %q requires a persisted never-started proof",
				primary.ID,
			))
		}
		if container.Launch.State == kernel.LaunchRetiring {
			return fault.New(fault.InvalidInput, fmt.Sprintf("action %q is already retiring", primary.ID))
		}
		return nil
	}

	if container.Launch.State != kernel.LaunchRetiring {
		return nil
	}

	if commit.Launch != nil {
		// The only legal launch write on a retiring action is the single
		// replacement identity: a fresh not_issued workload with its own key.
		if commit.Launch.State != kernel.LaunchNotIssued ||
			commit.Launch.WorkloadID == container.WorkloadID ||
			commit.Launch.WorkloadID == "" ||
			commit.Launch.IdempotencyKey == "" {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"action %q is retiring; only a not_issued replacement identity with a new workload and key is allowed",
				primary.ID,
			))
		}
		if commit.Observation != nil && commit.Observation.Kind != kernel.ObservationUnknown {
			return fault.New(fault.InvalidInput, fmt.Sprintf(
				"replacement of action %q cannot carry observation kind %q",
				primary.ID, commit.Observation.Kind,
			))
		}
		return nil
	}

	// Without a replacement launch, the only observation a retiring identity
	// may accept is the identical persisted proof.
	if commit.Observation != nil && !observationsEqual(container.Observation, *commit.Observation) {
		return fault.New(fault.InvalidInput, fmt.Sprintf(
			"observation of kind %q conflicts with the persisted proof of retiring action %q",
			commit.Observation.Kind, primary.ID,
		))
	}
	return nil
}

// observationsEqual compares recorded and candidate evidence the same way the
// reducer and engine do: kind and proof must match exactly, and any exit status
// or stop barrier must match too.
func observationsEqual(recorded, candidate kernel.Observation) bool {
	if recorded.Kind != candidate.Kind || recorded.Proof != candidate.Proof {
		return false
	}
	if (recorded.ExitCode == nil) != (candidate.ExitCode == nil) {
		return false
	}
	if recorded.ExitCode != nil && *recorded.ExitCode != *candidate.ExitCode {
		return false
	}
	if (recorded.Stop == nil) != (candidate.Stop == nil) {
		return false
	}
	if recorded.Stop != nil && *recorded.Stop != *candidate.Stop {
		return false
	}
	return true
}

func applyCommit(current kernel.Execution, commit flow.Commit) kernel.Execution {
	updated := cloneExecution(current)
	updated.Version = commit.ExpectedExecutionVersion + 1
	updated.State = commit.NextExecutionState
	if commit.NextCurrentAction != nil {
		updated.CurrentAction = *commit.NextCurrentAction
	}

	// Execution-only commits carry no primary action row update. Their
	// execution version/state, current index, failure, cleanup progress and
	// pending-action cancellations are still applied atomically below.
	if commit.ActionID != "" {
		action := &updated.Actions[actionIndex(updated, commit.ActionID)]
		previousState := action.State
		action.Version = commit.ExpectedActionVersion + 1
		action.State = commit.NextActionState
		if commit.NextActionState == kernel.ActionRunning && previousState != kernel.ActionRunning && !commit.At.IsZero() {
			action.ActivatedAt = commit.At
		}
		if !commit.Deadline.IsZero() {
			action.Deadline = commit.Deadline
		}

		switch commit.Output.Action {
		case flow.OutputStage:
			action.StagedOutputs = cloneOutputs(commit.Output.Values)
			action.StagedOutputsSet = true
		case flow.OutputPromote:
			action.Outputs = cloneOutputs(commit.Output.Values)
			action.StagedOutputs = nil
			action.StagedOutputsSet = false
		case flow.OutputDiscard:
			action.StagedOutputs = nil
			action.StagedOutputsSet = false
		}

		if commit.Launch != nil {
			action.Container.Launch = *commit.Launch
			action.Container.WorkloadID = commit.Launch.WorkloadID
		}
		if commit.Observation != nil {
			action.Container.Observation = cloneObservation(*commit.Observation)
		}
		if commit.StopEvidence != nil {
			action.Stop = *commit.StopEvidence
		}
		if commit.Subject != nil {
			subject := definition.NewSubject(commit.Subject.Fields())
			action.Request.Subject = &subject
		}
		if commit.Answer != nil {
			action.Request.Answer = cloneAnswer(commit.Answer)
		}
	}
	if commit.Failure != nil {
		updated.Failure = cloneFailure(commit.Failure)
	}
	if commit.CleanupUpdate != nil {
		updated.Cleanup = kernel.Cleanup{
			State:       commit.CleanupUpdate.State,
			WorkloadID:  commit.CleanupUpdate.WorkloadID,
			WorkspaceID: commit.CleanupUpdate.WorkspaceID,
			Attempts:    commit.CleanupUpdate.Attempts,
			LastError:   commit.CleanupUpdate.LastError,
		}
	}
	for _, update := range commit.PendingCancellations {
		pending := &updated.Actions[actionIndex(updated, update.ActionID)]
		pending.Version = update.ExpectedVersion + 1
		pending.State = update.NextState
	}
	return updated
}

// Scan returns due or unfinished executions in stable execution-ID order. The
// caller supplies the authoritative time; the fake never reads a wall clock.
func (s *State) Scan(ctx context.Context, request engine.ScanRequest) ([]id.ExecutionID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !request.Kind.Valid() {
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("unknown scan kind %q", request.Kind))
	}

	store := s.store
	store.mu.Lock()
	defer store.mu.Unlock()

	ids := make([]id.ExecutionID, 0, len(store.executions))
	for executionID, execution := range store.executions {
		switch request.Kind {
		case engine.ScanDue:
			if dueAt(execution, request.Now) {
				ids = append(ids, executionID)
			}
		case engine.ScanUnfinished:
			if unfinished(store, execution) {
				ids = append(ids, executionID)
			}
		}
	}
	slices.Sort(ids)
	if request.Limit > 0 && len(ids) > request.Limit {
		ids = ids[:request.Limit]
	}
	return ids, nil
}

func dueAt(execution kernel.Execution, now time.Time) bool {
	for _, action := range execution.Actions {
		if action.State != kernel.ActionRunning || action.Deadline.IsZero() {
			continue
		}
		if !now.Before(action.Deadline) {
			return true
		}
	}
	return false
}

func unfinished(store *Store, execution kernel.Execution) bool {
	switch execution.Cleanup.State {
	case kernel.CleanupPending:
		return true
	case kernel.CleanupComplete:
		// Completed cleanup means no external resource is still owed, even if
		// the seeded action record was left in a non-terminal shape.
		return false
	}
	if len(store.stops[execution.ID]) > 0 || len(store.cleanups[execution.ID]) > 0 {
		return true
	}
	// A non-terminal execution that owes forward progress has no active action:
	// either it was never activated (queued) or it settled its current action
	// and crashed before activating the successor (running with
	// CurrentAction == -1). The pending action carries no persisted deadline,
	// so only the unfinished scan can resume it.
	if !execution.State.Terminal() && execution.CurrentAction < 0 {
		for _, action := range execution.Actions {
			if action.State == kernel.ActionPending {
				return true
			}
		}
	}
	for _, action := range execution.Actions {
		// A running container action owns an external effect that may have
		// started before a crash. Its launch intent (not issued, issued or
		// unknown) must be observed or completed before the work is finished.
		if action.State == kernel.ActionRunning && action.Container != nil {
			return true
		}
	}
	return false
}

func actionIndex(execution kernel.Execution, actionID id.ActionID) int {
	for index, action := range execution.Actions {
		if action.ID == actionID {
			return index
		}
	}
	return -1
}

func versionConflict(format string, arguments ...any) error {
	return fault.New(fault.VersionConflict, fmt.Sprintf(format, arguments...))
}
