package work

import (
	"context"
	"encoding/json"
	"fmt"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Cancellation integration. Business cancellation commits immediately, with its
// reason, history, audit row and work_item.transitioned event, even while a
// writer is unsettled and even while the broker is down. That committed
// cancelled state is the durable cancellation intent: execution (T-020)
//
//   - consumes work_item.transitioned events whose "to" is cancelled and records,
//     idempotently, a request to cancel the item's current work runs, and
//   - periodically rescans cancelled items (CancellationSource) so a lost or
//     late delivery cannot leave a run unrequested.
//
// A cancelled item has no outgoing edge, so the event never contradicts current
// state and a duplicate delivery repeats an idempotent request. New dispatch is
// refused through the transaction-local admission seam (see the postgres
// package's LockActionable), not through this notification.

// CancelRequest asks execution to cancel the current work runs of one item.
type CancelRequest struct {
	Scope      domain.Scope
	WorkItemID domain.WorkItemID
	// Version is the item version at which it was cancelled.
	Version domain.Version
}

// CancelledItem is a cancelled work item read for reconciliation.
type CancelledItem struct {
	CancelRequest
}

// CancelledCursor continues a cancelled-item scan; the zero value starts it.
type CancelledCursor struct {
	ProjectID domain.ProjectID
	ID        domain.WorkItemID
}

// CancellationSource is the bounded, tenant-scoped scan of cancelled items.
type CancellationSource interface {
	CancelledItems(ctx context.Context, tenant domain.TenantID, after CancelledCursor, limit int) ([]CancelledItem, error)
}

// CancellationRequester is implemented by the execution module. Requests are
// idempotent: repeating one for an item that already has a cancellation request
// is a no-op.
type CancellationRequester interface {
	RequestCancellation(ctx context.Context, req CancelRequest) error
}

// ReconcileCancellations scans every cancelled item of tenant and re-requests
// cancellation of its runs. It is safe to run at any time and any number of
// times. It stops at the first requester failure so the caller retries.
func ReconcileCancellations(ctx context.Context, src CancellationSource, requester CancellationRequester, tenant domain.TenantID, batch int) (int, error) {
	if src == nil || requester == nil || batch < 1 {
		return 0, fmt.Errorf("%w: reconciliation needs a source, a requester and a positive batch", ErrInvalid)
	}
	var cursor CancelledCursor
	total := 0
	for {
		items, err := src.CancelledItems(ctx, tenant, cursor, batch)
		if err != nil {
			return total, err
		}
		for _, it := range items {
			if err := requester.RequestCancellation(ctx, it.CancelRequest); err != nil {
				return total, Unavailablef(err, "cancellation request for %s", it.WorkItemID)
			}
			total++
		}
		if len(items) < batch {
			return total, nil
		}
		last := items[len(items)-1]
		cursor = CancelledCursor{ProjectID: last.Scope.ProjectID, ID: last.WorkItemID}
	}
}

// CancellationFromEvent extracts the cancellation request carried by a
// work_item.transitioned event. ok is false for any other event or transition.
func CancellationFromEvent(env eventsv1.Envelope) (req CancelRequest, ok bool, err error) {
	if env.Type != eventsv1.TypeWorkItemTransitioned {
		return CancelRequest{}, false, nil
	}
	var data eventsv1.WorkItemTransitioned
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return CancelRequest{}, false, fmt.Errorf("%w: malformed work_item.transitioned payload", ErrInvalid)
	}
	if State(data.To) != StateCancelled {
		return CancelRequest{}, false, nil
	}
	req = CancelRequest{
		Scope:      domain.Scope{TenantID: env.TenantID, ProjectID: env.ProjectID},
		WorkItemID: domain.WorkItemID(env.Resource.ID), Version: domain.Version(env.Resource.Version),
	}
	if err := req.Scope.Validate(); err != nil {
		return CancelRequest{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := req.WorkItemID.Validate(); err != nil {
		return CancelRequest{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := req.Version.Validate(); err != nil {
		return CancelRequest{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return req, true, nil
}
