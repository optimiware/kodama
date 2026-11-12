package executionstore

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Transition applies one complete execution-transition commit unit: the
// expected execution/action versions, the Kernel commit's next states, output
// staging/promotion/discard, credential clearing, persisted launch/observation/
// failure/answer evidence, cleanup progress, the receipt, audit rows and event
// publication intent commit together.
func (s *Store) Transition(ctx context.Context, unit execution.TransitionUnit) (execution.TransitionResult, error) {
	if err := ctx.Err(); err != nil {
		return execution.TransitionResult{}, err
	}
	scope, err := scopeFromOwner(unit.Owner)
	if err != nil {
		return execution.TransitionResult{}, err
	}
	if _, err := id.ParseExecutionID(string(unit.ExecutionID)); err != nil {
		return execution.TransitionResult{}, fault.Wrap(fault.InvalidInput, "transition execution id is invalid", err)
	}
	result := execution.TransitionResult{
		ExecutionVersion: unit.ExpectedExecutionVersion + 1,
		NextExecution:    unit.Commit.NextExecutionState,
		NextAction:       unit.Commit.NextActionState,
	}
	if unit.Commit.ActionID != "" {
		result.ActionVersion = unit.ExpectedActionVersion + 1
	}
	record, err := resultReceipt(unit.Receipt, result)
	if err != nil {
		return execution.TransitionResult{}, err
	}
	plan, err := planCommit(ctx, scopedSource{executor: s.executor, scanner: s.scanner, scope: scope}, unit.Commit, unit.CredentialUpdate, nil, false)
	if err != nil {
		return execution.TransitionResult{}, err
	}
	authority, err := s.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return execution.TransitionResult{}, err
	}
	plan.locks = append(plan.locks, authority...)

	committed, err := s.executor.Execute(ctx, scope, store.Unit{
		Locks:   plan.locks,
		Writes:  plan.writes,
		Receipt: record,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return execution.TransitionResult{}, err
	}
	if committed.Replayed {
		replayed, err := decodeResult[execution.TransitionResult](committed.Receipt, result)
		if err != nil {
			return execution.TransitionResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}
