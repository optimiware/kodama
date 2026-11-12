package services

// The merge-coordinator adapter: assignment-keyed merge execution on the
// consumer-owned MergeRunner, strict subject/output mapping and settlement
// convergence. It calls public operations only (task.inbox, execution.inspect,
// execution.request.resolve) and never approves, reviews or pushes.

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// considerMerge performs one assignment-keyed merge and answers the request with
// exactly the declared observations. The durable assignment is released only
// after the resolution converged.
func (adapter *Adapter) considerMerge(ctx context.Context, scanned api.InboxItem, current api.ExecutionInspectResult, action api.ExecutionAction) error {
	if adapter.merges == nil {
		return fmt.Errorf("merge coordinator dependency is required")
	}
	subject, err := ParseMergeSubject(scanned.Subject)
	if err != nil {
		return err
	}
	if err := ValidateMergeOutputs(scanned.Outputs); err != nil {
		return err
	}
	request := MergeRequest{
		Key: subject.Key,
		Assignment: MergeAssignment{
			ExecutionID: id.ExecutionID(scanned.ExecutionID),
			ActionID:    id.ActionID(scanned.ActionID),
		},
		Base:      subject.Base,
		Candidate: subject.Candidate,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	result, err := adapter.merges.Merge(ctx, request)
	if err != nil {
		return err
	}
	outputs, err := MergeOutputValues(result, scanned.Outputs)
	if err != nil {
		return err
	}
	if err := adapter.resolve(ctx, scanned, current, action, outputs); err != nil {
		return err
	}
	return adapter.merges.Settle(ctx, request, result.State)
}

// reconcilePending settles merge assignments whose request resolution is already
// committed but whose resolve response was lost. It never executes an effect and
// never settles an assignment whose current action is still running.
func (adapter *Adapter) reconcilePending(ctx context.Context) error {
	if adapter.merges == nil {
		return nil
	}
	pending, err := adapter.merges.Pending(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, assignment := range pending {
		if err := adapter.settleResolved(ctx, assignment); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// mergeReleaseObserver is the coordinator's optional observe-only release
// capability. It is optional: a merge runner that does not provide it simply
// keeps an uncertain assignment held.
type mergeReleaseObserver interface {
	ReleaseObserved(context.Context, MergeRequest) (MergeResult, error)
}

// settleResolved inspects one held assignment's exact action and settles it when
// the request is no longer answerable. A cancelled or expired uncertain
// assignment is observed once and released only when that observation is
// conclusive.
func (adapter *Adapter) settleResolved(ctx context.Context, assignment PendingMerge) error {
	if err := assignment.Request.Validate(); err != nil {
		return err
	}
	inspected, err := adapter.inspect(ctx, api.InboxItem{
		ExecutionID: string(assignment.Request.Assignment.ExecutionID),
		ActionID:    string(assignment.Request.Assignment.ActionID),
	})
	if err != nil {
		var publicErr *api.Error
		if errors.As(err, &publicErr) && publicErr.Code == api.CodeNotFound {
			return nil
		}
		return err
	}
	if inspected.ExecutionID != string(assignment.Request.Assignment.ExecutionID) {
		return nil
	}
	action, ok := currentAction(inspected, string(assignment.Request.Assignment.ActionID))
	if !ok {
		return nil
	}
	if action.Decision == "" && action.State == "running" {
		expired := action.Deadline != nil && !adapter.clock.Now().Before(*action.Deadline)
		if !expired {
			// The request is still pending; the inbox scan owns it.
			return nil
		}
	}
	if assignment.State == EffectUncertain {
		if action.Decision != "" {
			// An answered request never releases an unknown effect. Only a
			// cancelled or expired request makes observation safe.
			return nil
		}
		reconciler, ok := adapter.merges.(mergeReleaseObserver)
		if !ok {
			// An unknown effect cannot be released without observation. The
			// assignment stays held and a later reconciliation decides it.
			return nil
		}
		if _, err := reconciler.ReleaseObserved(ctx, assignment.Request); err != nil {
			if errors.Is(err, ErrUncertain) {
				// The observation was inconclusive; keep it held for a later
				// pass instead of failing the whole scan.
				return nil
			}
			return err
		}
		return nil
	}
	return adapter.merges.Settle(ctx, assignment.Request, assignment.State)
}
