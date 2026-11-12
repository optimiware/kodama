package executionstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Engine implements the scope-free Kernel state port over the cross-scope
// store.Controller seam. The Kernel never names a tenancy scope: reads use the
// controller read authority and a commit derives its owning scope from the
// execution row inside the same transaction.
type Engine struct {
	controller store.Controller
}

var _ engine.State = (*Engine)(nil)

// NewEngine returns the Kernel state adapter over the controller seam.
func NewEngine(controller store.Controller) *Engine {
	return &Engine{controller: controller}
}

// Load returns one persisted execution aggregate without a caller scope.
func (e *Engine) Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Execution{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return kernel.Execution{}, fault.Wrap(fault.InvalidInput, "execution id is invalid", err)
	}
	return loadAggregate(ctx, controllerSource{controller: e.controller}, executionID)
}

// Commit applies exactly one complete compare-and-commit transition and returns
// the persisted post-commit aggregate. Every fact the commit carries is written
// in one controller unit; the owning scope is derived by the seam.
//
// The unit also carries the lifecycle event publication intent of the commit.
// Engine-driven commits are the only emitter of those classes (emitter
// ownership), so the full event.ProjectLifecycle projection is appended with no
// exclusion: the state writes, cleanup progress, action transitions and public
// action.requested commit atomically with their events, and a refused, stale or
// duplicate commit stages neither. The events' scope is the execution row's own
// scope, read by the same plan and re-verified by the controller seam.
func (e *Engine) Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Execution{}, err
	}
	if _, err := id.ParseExecutionID(string(commit.ExecutionID)); err != nil {
		return kernel.Execution{}, fault.Wrap(fault.InvalidInput, "commit execution id is invalid", err)
	}
	plan, err := planCommit(ctx, controllerSource{controller: e.controller}, commit, nil, nil, true)
	if err != nil {
		return kernel.Execution{}, err
	}
	if activationCommit(commit) {
		// The Shell predecessor barrier is enforced again at the authoritative
		// commit boundary, not only in discovery: a notification or a direct
		// Reconcile must not activate a replacement while a prior execution of
		// its task still has an unconfirmed stop. The predecessor rows the
		// barrier relied on are locked at their observed versions, so a
		// concurrent stop/cleanup commit is a version conflict rather than a
		// stale activation.
		row, err := e.controller.Read(ctx, tableExecutions, string(commit.ExecutionID))
		if err != nil {
			return kernel.Execution{}, err
		}
		confirmed, locks, err := e.taskBarrier(ctx, commit.ExecutionID, row)
		if err != nil {
			return kernel.Execution{}, err
		}
		if !confirmed {
			return kernel.Execution{}, fault.New(fault.VersionConflict,
				fmt.Sprintf("task predecessor stop is not confirmed for execution %q", commit.ExecutionID))
		}
		plan.locks = append(plan.locks, locks...)
	}
	if _, err := e.controller.Execute(ctx, string(commit.ExecutionID), store.Unit{
		Locks:  plan.locks,
		Writes: plan.writes,
		Events: plan.events,
	}); err != nil {
		return kernel.Execution{}, err
	}
	return e.Load(ctx, commit.ExecutionID)
}

// Scan reports the executions one recovery pass must reconcile. It reads
// cross-scope through the controller seam and returns a deterministic, bounded
// list: due deadlines for the due scan, and outstanding cleanup/effect/progress
// work for the unfinished scan.
func (e *Engine) Scan(ctx context.Context, request engine.ScanRequest) ([]id.ExecutionID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !request.Kind.Valid() {
		return nil, fault.New(fault.InvalidInput, "unknown execution scan kind")
	}
	var collected map[id.ExecutionID]struct{}
	var err error
	switch request.Kind {
	case engine.ScanDue:
		collected, err = e.scanDue(ctx, request.Now)
	case engine.ScanUnfinished:
		collected, err = e.scanUnfinished(ctx, request.Now)
	}
	if err != nil {
		return nil, err
	}
	identities := make([]id.ExecutionID, 0, len(collected))
	for executionID := range collected {
		identities = append(identities, executionID)
	}
	slices.Sort(identities)
	if request.Limit > 0 && len(identities) > request.Limit {
		identities = identities[:request.Limit]
	}
	return identities, nil
}

// scanDue collects executions whose single running action has reached its
// persisted deadline. The deadline column is nullable, so the adapter filters it
// after the bounded controller page instead of using an invalid NULL range scan.
func (e *Engine) scanDue(ctx context.Context, now time.Time) (map[id.ExecutionID]struct{}, error) {
	collected := make(map[id.ExecutionID]struct{})
	err := e.eachControllerRow(ctx, store.ScanQuery{
		Table:  tableActions,
		Equals: map[string]any{"state": string(kernel.ActionRunning)},
		Limit:  store.MaxQueryLimit,
	}, func(row store.Row) error {
		deadline, err := rowTime(row.Values, "deadline")
		if err != nil {
			return err
		}
		if deadline.IsZero() || now.Before(deadline) {
			return nil
		}
		executionID, err := rowString(row.Values, "execution_id")
		if err != nil {
			return err
		}
		collected[id.ExecutionID(executionID)] = struct{}{}
		return nil
	})
	return collected, err
}

// scanUnfinished collects every execution that still owes recovery work:
// pending cleanup whose durable retry deadline is due, a running container
// workload whose launch may have happened, or a non-terminal execution with a
// pending action and no active action.
//
// A pending cleanup is due when next_retry_at is NULL or not after now. The
// retry deadline is the persisted backoff schedule, so a failed attempt is not
// reattempted on every scan pass and its finite attempt budget lasts the
// recovery window. Queued/running progress work is unaffected.
func (e *Engine) scanUnfinished(ctx context.Context, now time.Time) (map[id.ExecutionID]struct{}, error) {
	collected := make(map[id.ExecutionID]struct{})
	if err := e.eachControllerRow(ctx, store.ScanQuery{
		Table:  tableCleanup,
		Equals: map[string]any{"state": string(kernel.CleanupPending)},
		Limit:  store.MaxQueryLimit,
	}, func(row store.Row) error {
		retryAt, err := rowTime(row.Values, "next_retry_at")
		if err != nil {
			return err
		}
		if !retryAt.IsZero() && now.Before(retryAt) {
			return nil
		}
		return collectExecutionID(row, collected)
	}); err != nil {
		return nil, err
	}
	if err := e.eachControllerRow(ctx, store.ScanQuery{
		Table:  tableActions,
		Equals: map[string]any{"state": string(kernel.ActionRunning), "kind": string(definition.ActionKindContainer)},
		Limit:  store.MaxQueryLimit,
	}, func(row store.Row) error {
		return collectExecutionID(row, collected)
	}); err != nil {
		return nil, err
	}

	pending := make(map[id.ExecutionID]struct{})
	if err := e.eachControllerRow(ctx, store.ScanQuery{
		Table:  tableActions,
		Equals: map[string]any{"state": string(kernel.ActionPending)},
		Limit:  store.MaxQueryLimit,
	}, func(row store.Row) error {
		return collectExecutionID(row, pending)
	}); err != nil {
		return nil, err
	}
	active := make(map[id.ExecutionID]struct{})
	if err := e.eachControllerRow(ctx, store.ScanQuery{
		Table:  tableActions,
		Equals: map[string]any{"state": string(kernel.ActionRunning)},
		Limit:  store.MaxQueryLimit,
	}, func(row store.Row) error {
		return collectExecutionID(row, active)
	}); err != nil {
		return nil, err
	}
	for _, state := range []kernel.ExecutionState{kernel.ExecutionQueued, kernel.ExecutionRunning} {
		if err := e.eachControllerRow(ctx, store.ScanQuery{
			Table:  tableExecutions,
			Equals: map[string]any{"state": string(state)},
			Limit:  store.MaxQueryLimit,
		}, func(row store.Row) error {
			executionID, err := rowString(row.Values, "id")
			if err != nil {
				return err
			}
			identity := id.ExecutionID(executionID)
			if _, running := active[identity]; running {
				return nil
			}
			if _, waiting := pending[identity]; !waiting {
				return nil
			}
			// A queued or dormant task execution may not be discovered for
			// activation until every prior execution of its task has confirmed
			// stop. Blocked work is omitted, not an error, so the same scan
			// still reports the predecessor's overdue cleanup and every
			// unrelated execution. Unreadable evidence fails the scan closed.
			confirmed, _, err := e.taskBarrier(ctx, identity, row)
			if err != nil {
				return err
			}
			if !confirmed {
				return nil
			}
			collected[identity] = struct{}{}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return collected, nil
}

// maxControllerPages bounds one recovery scan so a corrupt or adversarial cursor
// cannot loop forever. Each page holds up to store.MaxQueryLimit rows.
const maxControllerPages = 64

// eachControllerRow streams every row of one bounded controller query.
func (e *Engine) eachControllerRow(ctx context.Context, query store.ScanQuery, visit func(store.Row) error) error {
	cursor := ""
	for range maxControllerPages {
		if err := ctx.Err(); err != nil {
			return err
		}
		query.After = cursor
		result, err := e.controller.Scan(ctx, store.ControllerScan{ScanQuery: query})
		if err != nil {
			return err
		}
		for _, row := range result.Rows {
			if err := visit(row); err != nil {
				return err
			}
		}
		if !result.HasMore {
			return nil
		}
		cursor = result.Next
	}
	return fault.New(fault.InternalFailure, "controller recovery scan exceeded its bounded page count")
}

// collectExecutionID adds one row's execution identity to a set.
func collectExecutionID(row store.Row, into map[id.ExecutionID]struct{}) error {
	executionID, err := rowString(row.Values, "execution_id")
	if err != nil {
		return err
	}
	if executionID == "" {
		return fault.New(fault.InternalFailure, "controller row has no execution identity")
	}
	into[id.ExecutionID(executionID)] = struct{}{}
	return nil
}
