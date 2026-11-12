package task

// Both task cancel paths build the Kernel cancel reduction.
//
// `Ask` and `Cancel` never write `executions.state = cancelled` directly. When
// the task has a known current execution the service loads its
// version-guarded aggregate through the consumer-owned
// `ExecutionAggregateReader`, reduces `flow.CancelFact` exactly as the
// execution service does, carries the resulting `flow.Commit` and aggregate
// version, and validates the unit with `ValidateCancellationCommit` before the
// port. A terminal predecessor is preserved with no commit.
//
// The unit's own task event stays first; the appended projection is
// `event.ExcludeOwned(event.ProjectLifecycle(pre, commit))`. Neither
// `task.question` nor `task.cancelled` is a projected lifecycle class, so
// nothing is excluded and every transition is published exactly once.

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// cancelReduction loads the task's current execution aggregate and returns the
// Kernel cancel commit for a non-terminal predecessor. A terminal predecessor
// returns the aggregate with a nil commit so its row is preserved.
//
// The aggregate read is scoped and never retried in another scope. A missing
// aggregate for a known task pointer is an inconsistency and is reported as
// the reader's uniform not_found; a dependency failure keeps its class. Either
// way the operation fails closed before the port and no unit is built.
func (service *Service) cancelReduction(
	ctx context.Context,
	scope tenancy.Scope,
	pointer id.ExecutionID,
	at time.Time,
) (kernel.Execution, *flow.Commit, error) {
	aggregate, err := service.aggregates.Load(ctx, scope, pointer)
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	if aggregate.ID != pointer {
		return kernel.Execution{}, nil, fault.New(fault.InvalidInput,
			fmt.Sprintf("execution aggregate %q does not match the task's current execution %q",
				aggregate.ID, pointer))
	}
	if aggregate.State.Terminal() {
		return aggregate, nil, nil
	}
	_, commit, err := flow.Reduce(aggregate, flow.CancelFact{
		ExecutionID: aggregate.ID,
		Reason:      flow.CancelRequested,
		At:          at,
	})
	if err != nil {
		return kernel.Execution{}, nil, err
	}
	if commit == nil {
		return kernel.Execution{}, nil, fault.New(fault.InternalFailure,
			fmt.Sprintf("cancel reduction for execution %q returned no commit", aggregate.ID))
	}
	return aggregate, commit, nil
}

// cancellationPreImage builds the version-guarded pre-image of the aggregate
// the reduction was derived from. Actions stay in declaration order.
func cancellationPreImage(scope tenancy.Scope, aggregate kernel.Execution) event.ExecutionPreImage {
	actions := make([]event.ActionPreImage, len(aggregate.Actions))
	for position, action := range aggregate.Actions {
		actions[position] = event.ActionPreImage{
			ActionID: action.ID,
			Index:    action.Index,
			Kind:     action.Kind,
			State:    action.State,
			Version:  action.Version,
		}
	}
	return event.ExecutionPreImage{
		Scope:       scope,
		ExecutionID: aggregate.ID,
		Version:     aggregate.Version,
		State:       aggregate.State,
		Actions:     actions,
	}
}

// appendCancellationProjection returns the unit's own events followed by the
// cancel commit's lifecycle projection. Neither task event is a projected
// class, so no event is excluded. A projection refusal is returned unchanged,
// so the caller fails the operation closed before the port.
func appendCancellationProjection(
	own []event.Envelope,
	scope tenancy.Scope,
	aggregate kernel.Execution,
	commit flow.Commit,
) ([]event.Envelope, error) {
	projected, err := event.ProjectLifecycle(cancellationPreImage(scope, aggregate), commit)
	if err != nil {
		return nil, err
	}
	remainder := event.ExcludeOwned(projected)
	events := append([]event.Envelope(nil), own...)
	for _, spec := range remainder {
		envelope, err := event.NewEnvelope(
			spec.ID, spec.Source, spec.Scope, spec.At,
			event.EnvelopeInput{
				Type:           spec.Type,
				SubjectID:      spec.SubjectID,
				SubjectVersion: spec.SubjectVersion,
				Data:           spec.Data,
			},
		)
		if err != nil {
			return nil, err
		}
		events = append(events, envelope)
	}
	return events, nil
}
