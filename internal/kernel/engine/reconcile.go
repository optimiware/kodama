package engine

// The reconciler is the only Kernel component that performs I/O. It loads one
// execution, observes any outstanding external effect, reduces exactly one
// explicit fact with the injected authoritative clock, compare-and-commits the
// complete transition payload and only then performs the committed effects.
//
// Every transition is durable before its effect. A crash between commit and
// effect is recovered by a later Reconcile or Scan from the persisted intent,
// never by replaying a stale transition. Launch uncertainty, stop evidence and
// cleanup progress are separate durable values, so an ambiguous response is
// never collapsed into proven-not-started, confirmed-stopped or clean.
//
// A proven-never-started identity is retired before it is replaced: the
// `retiring` launch state and the proof are committed first, Remove follows
// idempotently, and only a confirmed removal commits the replacement identity.
// A retiring identity is launch-terminal and never prepared, started, observed
// or re-issued; cancellation, expiry and workspace loss settle it through
// terminal cleanup without a Stop.

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// maxReconcileSteps bounds one Reconcile call. The loop stops earlier as soon
// as it reaches a non-settling observation or an ambiguous launch outcome, so
// this is only a defensive guard against an adapter that keeps making progress
// without reaching quiescence.
const maxReconcileSteps = 64

// Engine drives durable Kernel reconciliation through the declared ports. It
// owns no persistence, identity or transport and holds no per-execution memory:
// every decision is derived from persisted state, so a restart reconstructs the
// same work.
type Engine struct {
	state     State
	runtime   Runtime
	workspace Workspace
	clock     Clock

	// policyMu guards policy, which is installation configuration rather than
	// per-execution memory. SetCleanupPolicy may run while Reconcile/Scan is
	// deciding the next cleanup step.
	policyMu sync.RWMutex
	policy   CleanupPolicy
}

// New returns a reconciler over the supplied consumer-owned ports. The cleanup
// retry bound starts at the safe default; installations may replace it through
// SetCleanupPolicy without changing this signature.
func New(state State, runtime Runtime, workspace Workspace, clock Clock) *Engine {
	return &Engine{
		state:     state,
		runtime:   runtime,
		workspace: workspace,
		clock:     clock,
		policy:    CleanupPolicy{MaxAttempts: DefaultCleanupMaxAttempts},
	}
}

// Reconcile loads one execution and drives committed work until it reaches a
// point that needs new external evidence. It may apply several transitions in
// one call; callers that only need one step still make progress.
func (e *Engine) Reconcile(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	var execution kernel.Execution
	for range maxReconcileSteps {
		if err := ctx.Err(); err != nil {
			return execution, err
		}
		loaded, err := e.state.Load(ctx, executionID)
		if err != nil {
			return execution, err
		}
		execution = loaded

		progress, stop, err := e.advance(ctx, execution)
		if err != nil {
			if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
				// A concurrent transition won the same commit boundary. Re-read
				// and re-evaluate instead of replaying the stale payload.
				continue
			}
			return execution, err
		}
		if stop || !progress {
			return e.latest(ctx, executionID, execution)
		}
	}
	return e.latest(ctx, executionID, execution)
}

// latest reloads the persisted execution so callers observe committed state
// rather than the pre-commit snapshot used for the last decision.
func (e *Engine) latest(ctx context.Context, executionID id.ExecutionID, fallback kernel.Execution) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return fallback, err
	}
	execution, err := e.state.Load(ctx, executionID)
	if err != nil {
		return fallback, err
	}
	return execution, nil
}

func (e *Engine) now() time.Time {
	return e.clock.Now()
}

// advance applies at most one decision to the loaded execution. It reports
// whether anything was committed and whether the caller should stop driving
// this execution (for example because only new external evidence can help).
func (e *Engine) advance(ctx context.Context, execution kernel.Execution) (progress bool, stop bool, err error) {
	switch {
	case execution.State.Terminal():
		return e.advanceTerminal(ctx, execution)
	case execution.State == kernel.ExecutionQueued:
		return e.advanceQueued(ctx, execution)
	case execution.State == kernel.ExecutionRunning:
		return e.advanceRunning(ctx, execution)
	default:
		return false, true, nil
	}
}

// advanceQueued activates the first pending action. Activation persists the
// absolute deadline and stable effect identity before any runtime effect, and
// retains the execution workspace before the action becomes usable.
func (e *Engine) advanceQueued(ctx context.Context, execution kernel.Execution) (bool, bool, error) {
	index := firstPending(execution)
	if index < 0 {
		return false, true, nil
	}
	return e.activate(ctx, execution, index)
}

// advanceRunning reconciles the single active action, or activates the next
// pending successor after a settlement.
func (e *Engine) advanceRunning(ctx context.Context, execution kernel.Execution) (bool, bool, error) {
	if execution.CurrentAction < 0 {
		available, err := e.workspaceAvailable(ctx, execution)
		if err != nil {
			return false, false, err
		}
		if !available {
			return e.failWorkspaceLost(ctx, execution)
		}
		index := firstPending(execution)
		if index < 0 {
			return false, true, nil
		}
		return e.activate(ctx, execution, index)
	}

	index := execution.CurrentAction
	if index >= len(execution.Actions) {
		return false, true, nil
	}
	action := execution.Actions[index]
	if action.State != kernel.ActionRunning {
		return false, true, nil
	}

	// A missing retained workspace fails visibly before any dispatch. Scratch
	// state is never reconstructed.
	available, err := e.workspaceAvailable(ctx, execution)
	if err != nil {
		return false, false, err
	}
	if !available {
		return e.failWorkspaceLost(ctx, execution)
	}

	now := e.now()
	if !action.Deadline.IsZero() && !now.Before(action.Deadline) {
		return e.applyFact(ctx, execution, flow.ExpireFact{
			ExecutionID: execution.ID,
			ActionID:    action.ID,
			At:          now,
		})
	}

	switch action.Kind {
	case definition.ActionKindContainer:
		if action.Container == nil {
			return false, true, nil
		}
		switch action.Container.Launch.State {
		case kernel.LaunchNotIssued:
			return e.performStart(ctx, execution, index)
		case kernel.LaunchRetiring:
			// A retiring identity is launch-terminal: only the retirement
			// completion may act on it. It is never observed, prepared, started
			// or re-issued.
			return e.retire(ctx, execution, index)
		default:
			return e.observe(ctx, execution, index)
		}
	case definition.ActionKindRequest:
		// A request waits for an authorized Shell settlement. It owns no
		// workload and receives no credential delivery.
		return false, true, nil
	default:
		return false, true, nil
	}
}

// activate retains the execution workspace, commits one activation transition
// and then materializes the committed work.
//
// Retention comes before the commit, so a committed activation always has a
// retained workspace: a request is never answerable and a workload never starts
// without one. Retaining is idempotent and keyed by the persisted workspace
// identity, so a controller that stops in between leaves nothing to recover: the
// action is still pending and the next pass retains again. Workspace adapters
// rely on this order: when Retain is called, the activating action has not left
// pending yet.
//
// A retryable workspace failure commits nothing and is reported for a later
// pass. Any other failure is evidence that the workspace is unusable and fails
// the execution visibly before the action becomes usable.
func (e *Engine) activate(ctx context.Context, execution kernel.Execution, index int) (bool, bool, error) {
	action := execution.Actions[index]
	_, commit, err := flow.Reduce(execution, flow.ActivateFact{
		ExecutionID: execution.ID,
		ActionID:    action.ID,
		At:          e.now(),
	})
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.InvalidInput {
			return e.failPreparation(ctx, execution, action, "activation_refused", err)
		}
		return false, true, nil
	}
	if commit == nil {
		return false, true, nil
	}
	// An activation that already settles the execution (an unresolvable
	// request subject) makes nothing usable and needs no workspace. Otherwise
	// workspace retention is separate from workload ownership and survives
	// request waits and controller restarts.
	if !commit.NextExecutionState.Terminal() {
		if err := e.workspace.Retain(ctx, execution.WorkspaceID); err != nil {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			if retryableWorkspaceError(err) {
				return false, false, err
			}
			return e.failWorkspaceLost(ctx, execution)
		}
	}

	updated, err := e.state.Commit(ctx, *commit)
	if err != nil {
		return false, false, err
	}
	if updated.State.Terminal() {
		return true, false, nil
	}

	if action.Kind == definition.ActionKindContainer {
		return e.performStart(ctx, updated, index)
	}
	return true, false, nil
}

// applyFact reduces one explicit fact and commits the complete transition.
// A refusal is not an error: it means the fact is stale or illegal for the
// loaded revision, so the reconciler simply stops and re-reads later.
func (e *Engine) applyFact(ctx context.Context, execution kernel.Execution, fact flow.Fact) (bool, bool, error) {
	_, commit, err := flow.Reduce(execution, fact)
	if err != nil {
		return false, true, nil
	}
	if commit == nil {
		return false, true, nil
	}
	if _, err := e.state.Commit(ctx, *commit); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// workspaceAvailable reports the retained workspace status. A missing
// workspace is a failure, not permission to recreate scratch state.
func (e *Engine) workspaceAvailable(ctx context.Context, execution kernel.Execution) (bool, error) {
	observation, err := e.workspace.Status(ctx, execution.WorkspaceID)
	if err != nil {
		return false, err
	}
	return observation.Available, nil
}

// failWorkspaceLost commits the visible workspace-loss failure together with
// the stop intent for any live workload, the pending cleanup record and every
// pending-action cancellation.
func (e *Engine) failWorkspaceLost(ctx context.Context, execution kernel.Execution) (bool, bool, error) {
	return e.applyFact(ctx, execution, flow.WorkspaceLostFact{
		ExecutionID: execution.ID,
		WorkspaceID: execution.WorkspaceID,
		At:          e.now(),
	})
}

// preparationNeverStartedProof is the persisted affirmative evidence that a
// preparation failure settled the action before its start request was issued.
const preparationNeverStartedProof = "preparation failed before the start request was issued"

// failPreparation settles the selected action as failed before it became
// usable. The failure, the pending-action cancellations, the credential
// invalidation and the pending cleanup progress travel in one commit.
//
// When the failure happens after the launch marker was issued but before Start
// was requested, the commit also records the failed identity's affirmative
// pre-Start evidence and retains that identity in the cleanup record. Without
// it, terminal cleanup has to observe an identity the runtime never
// materialized; an absent Kubernetes object and an unconfirmed Stop are not
// proof, so the workspace would stay owed forever.
func (e *Engine) failPreparation(ctx context.Context, execution kernel.Execution, action kernel.Action, reason string, cause error) (bool, bool, error) {
	nextCurrent := -1
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	cleanup := &flow.CleanupUpdate{
		State:       kernel.CleanupPending,
		WorkspaceID: execution.WorkspaceID,
	}
	commit := flow.Commit{
		ExecutionID:              execution.ID,
		ExpectedExecutionVersion: execution.Version,
		NextExecutionState:       kernel.ExecutionFailed,
		ActionID:                 action.ID,
		ExpectedActionVersion:    action.Version,
		NextActionState:          kernel.ActionFailed,
		PendingCancellations:     pendingCancellations(execution, action.ID),
		NextCurrentAction:        &nextCurrent,
		Output:                   flow.OutputChange{Action: flow.OutputDiscard},
		InvalidateCredential:     true,
		Failure: &kernel.Failure{
			Kind:   kernel.FailurePreparation,
			Reason: reason,
			Detail: detail,
		},
		CleanupUpdate: cleanup,
		At:            e.now(),
	}
	if action.Kind == definition.ActionKindContainer && action.Container != nil &&
		action.Container.WorkloadID != "" && action.Container.Launch.State == kernel.LaunchIssued {
		// The marker is durable and Start was never called, so the exact
		// identity is affirmatively never started. Persist that evidence beside
		// the failure so cleanup removes only this identity's partial resources
		// and never waits for an observation or a stop that will not arrive.
		proof := kernel.NeverStarted(preparationNeverStartedProof)
		cleanup.WorkloadID = action.Container.WorkloadID
		commit.Observation = &proof
	} else {
		// No start could have been requested for this action, but it owns no
		// effect identity itself. Keep any earlier issued workload attributable
		// instead of dropping the cleanup identity.
		cleanup.WorkloadID = lastEffectWorkload(execution)
	}
	if _, err := e.state.Commit(ctx, commit); err != nil {
		return false, false, err
	}
	return true, false, nil
}

// lastEffectWorkload returns the last issued workload identity, matching the
// terminal cleanup selection. It is used to keep a cleanup record attributable
// when the failed action itself never owned an identity.
func lastEffectWorkload(execution kernel.Execution) id.WorkloadID {
	var last id.WorkloadID
	for _, action := range execution.Actions {
		if action.Kind != definition.ActionKindContainer || action.Container == nil {
			continue
		}
		if action.Container.WorkloadID == "" || action.Container.Launch.State == kernel.LaunchNotIssued {
			continue
		}
		last = action.Container.WorkloadID
	}
	return last
}

// ---------------------------------------------------------------------------
// Public command boundary
// ---------------------------------------------------------------------------

// SubmitOutputs applies the output rules through the same reducer and
// compare-and-commit boundary. An identical retry converges without a commit; a
// replacement submission is refused.
func (e *Engine) SubmitOutputs(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, outputs kernel.Outputs) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	execution, err := e.state.Load(ctx, executionID)
	if err != nil {
		return err
	}
	_, commit, err := flow.Reduce(execution, flow.OutputFact{
		ExecutionID: executionID,
		ActionID:    actionID,
		Outputs:     outputs,
		At:          e.now(),
	})
	if err != nil {
		return err
	}
	if commit == nil {
		return nil
	}
	_, err = e.state.Commit(ctx, *commit)
	return err
}

// SettleRequest applies one authorized answer. Timeliness is decided by the
// authoritative clock, never by the sender timestamp.
func (e *Engine) SettleRequest(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, answer kernel.Answer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	execution, err := e.state.Load(ctx, executionID)
	if err != nil {
		return err
	}
	_, commit, err := flow.Reduce(execution, flow.AnswerFact{
		ExecutionID: executionID,
		ActionID:    actionID,
		Answer:      answer,
		At:          e.now(),
	})
	if err != nil {
		return err
	}
	if commit == nil {
		return nil
	}
	_, err = e.state.Commit(ctx, *commit)
	return err
}

// Cancel commits the cancellation transition. Stop and cleanup effects remain
// recoverable through Reconcile and Scan from the committed intents.
func (e *Engine) Cancel(ctx context.Context, executionID id.ExecutionID, reason flow.CancelReason) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	execution, err := e.state.Load(ctx, executionID)
	if err != nil {
		return err
	}
	_, commit, err := flow.Reduce(execution, flow.CancelFact{
		ExecutionID: executionID,
		Reason:      reason,
		At:          e.now(),
	})
	if err != nil {
		return err
	}
	if commit == nil {
		return nil
	}
	_, err = e.state.Commit(ctx, *commit)
	return err
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func firstPending(execution kernel.Execution) int {
	for index, action := range execution.Actions {
		if action.State == kernel.ActionPending {
			return index
		}
	}
	return -1
}

func actionIndex(execution kernel.Execution, actionID id.ActionID) int {
	for index, action := range execution.Actions {
		if action.ID == actionID {
			return index
		}
	}
	return -1
}

// pendingCancellations cancels every pending action except the primary one, so
// a failure or cancellation never leaves a successor pending.
func pendingCancellations(execution kernel.Execution, primary id.ActionID) []flow.ActionUpdate {
	var updates []flow.ActionUpdate
	for _, action := range execution.Actions {
		if action.ID == primary || action.State != kernel.ActionPending {
			continue
		}
		updates = append(updates, flow.ActionUpdate{
			ActionID:        action.ID,
			ExpectedVersion: action.Version,
			NextState:       kernel.ActionCancelled,
		})
	}
	return updates
}

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

func cloneOutputs(values kernel.Outputs) kernel.Outputs {
	if values == nil {
		return nil
	}
	cloned := make(kernel.Outputs, len(values))
	maps.Copy(cloned, values)
	return cloned
}

func unsupportedCapability(name string) error {
	return fault.New(fault.UnsupportedCapability, fmt.Sprintf("runtime does not declare admitted capability %q", name))
}

func trimDetail(detail string) string {
	return strings.TrimSpace(detail)
}
