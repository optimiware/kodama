package executionstore

// The Shell predecessor barrier shared by queued discovery and the activation
// commit. The Kernel state adapter is task-independent, but executions are
// written with the task identity that admitted them; the Shell therefore
// enforces its own handoff rule from those persisted rows. The rule is the one
// docs/specs/06-shell-lifecycles.md section 3 states: a replacement execution
// may dispatch only after every prior workload is confirmed stopped.
//
// Cleanup completion is sufficient evidence, but it is not the only sufficient
// evidence: a terminal execution whose every possibly started container action
// carries a confirmed stop barrier is stopped even while cleanup is still owed.
// The same derivation is used by the task service's scoped ExecutionReader, so
// both the reply decision and this authoritative barrier share one rule.

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/store"
)

// StopEvidence reports the predecessor's stop barrier from the rows the Kernel
// persisted.
//
// A non-terminal execution is never confirmed, whatever cleanup or per-action
// evidence it already carries: a queued or running execution can still start its
// next action. Only after the execution itself reaches a terminal state does the
// persisted stop evidence apply. Cleanup complete is affirmative: the Kernel
// only completes cleanup after every possibly started workload is confirmed
// stopped or affirmatively proven never started, and the owned resources are
// removed. While cleanup is still owed, a possibly started container action's
// own persisted stop barrier is used; the execution is confirmed only when
// every such action carries a confirmed barrier. A missing cleanup row, a
// pending cleanup, a pending barrier and an absent barrier all report
// unconfirmed evidence, so dispatch is never gated on an inferred stop.
func StopEvidence(execution kernel.Execution) kernel.StopEvidence {
	if !execution.State.Terminal() {
		return kernel.PendingStop("execution is not terminal and may still start another action")
	}
	if execution.Cleanup.State == kernel.CleanupComplete {
		return kernel.ConfirmedStop("execution cleanup is complete")
	}
	possiblyStarted := 0
	confirmed := 0
	for _, action := range execution.Actions {
		if action.Container == nil {
			continue
		}
		launched := action.Container.WorkloadID != "" && action.Container.Launch.State != kernel.LaunchNotIssued
		recorded := action.Stop.State != ""
		if !launched && !recorded {
			continue
		}
		possiblyStarted++
		if action.Stop.IsConfirmed() {
			confirmed++
		}
	}
	if possiblyStarted > 0 && confirmed == possiblyStarted {
		return kernel.ConfirmedStop("every possibly started workload has confirmed stop evidence")
	}
	return kernel.PendingStop("execution cleanup is not complete and a workload stop is not confirmed")
}

// activationCommit reports whether one Kernel commit makes new work usable: the
// execution and its primary action both become running.
func activationCommit(commit flow.Commit) bool {
	return commit.NextExecutionState == kernel.ExecutionRunning && commit.NextActionState == kernel.ActionRunning
}

// taskBarrier evaluates the predecessor barrier of one execution and returns
// the version locks a commit must carry for the rows it relied on.
//
// The execution's own task_id relationship names every sibling execution of the
// same task in the same scope. Every sibling created before this execution
// must be terminal and its persisted StopEvidence confirmed; an execution with
// a strictly later creation instant is a successor and does not hold this one.
//
// Siblings created at the same instant are ordered by what the rows already
// say, so they neither block each other forever nor run together. A sibling
// that has left the queue is always prior: it may have started a workload. A
// sibling that is still queued is a successor of an execution that has already
// been activated, and two queued siblings are ordered by identity. The queued
// execution that goes first locks the other at the version it read, so the
// other's concurrent activation is a version conflict. An unreadable row, a
// malformed stop value or a missing creation time fails closed with an error,
// never a silent confirmation.
//
// A task-free execution has no predecessor relationship and is confirmed.
// Discovery treats a blocked result as "not ready yet" and keeps the
// predecessor's own cleanup work in the same scan; the commit boundary turns a
// blocked result into a version_conflict before anything is written.
func (e *Engine) taskBarrier(ctx context.Context, executionID id.ExecutionID, row store.Row) (bool, []store.Lock, error) {
	taskID, err := rowString(row.Values, "task_id")
	if err != nil {
		return false, nil, err
	}
	if taskID == "" {
		return true, nil, nil
	}
	tenantID, err := rowString(row.Values, "tenant_id")
	if err != nil {
		return false, nil, err
	}
	projectID, err := rowString(row.Values, "project_id")
	if err != nil {
		return false, nil, err
	}
	createdAt, err := rowTime(row.Values, "created_at")
	if err != nil {
		return false, nil, err
	}
	if createdAt.IsZero() {
		return false, nil, fault.New(fault.InternalFailure,
			fmt.Sprintf("task execution %q has no creation time; predecessor order is unreadable", executionID))
	}
	state, err := rowString(row.Values, "state")
	if err != nil {
		return false, nil, err
	}
	queued := kernel.ExecutionState(state) == kernel.ExecutionQueued

	confirmed := true
	var locks []store.Lock
	err = e.eachControllerRow(ctx, store.ScanQuery{
		Table: tableExecutions,
		Equals: map[string]any{
			"tenant_id":  tenantID,
			"project_id": projectID,
			"task_id":    taskID,
		},
		OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
		Limit:   store.MaxQueryLimit,
	}, func(prior store.Row) error {
		priorIDText, err := rowString(prior.Values, "id")
		if err != nil {
			return err
		}
		priorID := id.ExecutionID(priorIDText)
		if priorIDText == "" {
			return fault.New(fault.InternalFailure, "task sibling execution row has no identity")
		}
		if priorID == executionID {
			return nil
		}
		priorCreated, err := rowTime(prior.Values, "created_at")
		if err != nil {
			return err
		}
		if priorCreated.IsZero() {
			return fault.New(fault.InternalFailure,
				fmt.Sprintf("task execution %q has no creation time; predecessor order is unreadable", priorID))
		}
		if priorCreated.After(createdAt) {
			return nil
		}
		priorVersion, err := rowUint64(prior.Values, "version")
		if err != nil {
			return err
		}
		if priorCreated.Equal(createdAt) {
			priorState, err := rowString(prior.Values, "state")
			if err != nil {
				return err
			}
			if kernel.ExecutionState(priorState) == kernel.ExecutionQueued {
				if !queued {
					return nil
				}
				if priorID > executionID {
					locks = append(locks, executionVersionLock(priorID, priorVersion))
					return nil
				}
			}
		}
		aggregate, err := loadAggregate(ctx, controllerSource{controller: e.controller}, priorID)
		if err != nil {
			return err
		}
		if !StopEvidence(aggregate).IsConfirmed() {
			// Every prior execution is checked. A blocked barrier is a
			// discovery/skip and a commit refusal, never an error.
			confirmed = false
			return nil
		}
		locks = append(locks, executionVersionLock(priorID, priorVersion))
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return confirmed, locks, nil
}
