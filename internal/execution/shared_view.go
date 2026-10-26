package execution

import (
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ResourceAuditID is the audit and event resource ID of a run resource: the
// T-119 eventsv1.RunResourceID of (tenant, project, run, key), "rr-" plus 40
// hex characters of the 4-byte-framed SHA-256. It is the single derivation; the
// earlier underscore flattening aliased distinct resources and is gone.
func ResourceAuditID(scope domain.Scope, run domain.RunID, key string) (string, error) {
	return eventsv1.RunResourceID(scope.TenantID, scope.ProjectID, run, key)
}

// ResourceView is the safe projection of one run resource for run observation:
// kind, key, phase, safe reason and the evidence times. It carries no UID,
// fingerprint, spec, endpoint, log or credential. The transport that serves it
// applies current authorization (P6); `GET P/runs/{id}` additively exposes the
// list (docs/contracts/run-resources.md#observations-and-api-projection).
type ResourceView struct {
	Kind      ResourceKind
	Key       string
	Phase     ResourcePhase
	Reason    ResourceReason
	ReadyAt   *time.Time
	StoppedAt *time.Time
	RemovedAt *time.Time
}

// SharedView is the additive run_shared projection of a run.
type SharedView struct {
	RunModel       string
	DeadlineAt     *time.Time
	ExecutionOrder []string
	// Access is the effective workspace access of every executable step.
	Access    map[string]Access
	Resources []ResourceView
	// CleanupReason is the visible reason a draining run is held nonterminal by
	// its resources (resource_stop_unconfirmed or cleanup_pending); empty
	// otherwise. It is derived, never a business status.
	CleanupReason ResourceReason
}

// SharedView returns the projection, or nil for an attempt_private run.
func (a *Aggregate) SharedView() *SharedView {
	if a.Run.Shared == nil {
		return nil
	}
	v := &SharedView{
		RunModel: a.Run.Shared.RunModel, DeadlineAt: cloneTimePtr(a.Run.DeadlineAt), ExecutionOrder: append([]string{}, a.Run.Shared.Order...),
		Access: map[string]Access{}, Resources: []ResourceView{}, CleanupReason: a.CleanupReason(),
	}
	for k, x := range a.Run.Shared.Access {
		v.Access[k] = x
	}
	for _, r := range a.Resources {
		v.Resources = append(v.Resources, ResourceView{
			Kind: r.Kind, Key: r.Key, Phase: r.Phase, Reason: r.Reason, ReadyAt: cloneTimePtr(r.ReadyAt), StoppedAt: cloneTimePtr(r.StoppedAt), RemovedAt: cloneTimePtr(r.RemovedAt),
		})
	}
	return v
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
