package request

// A Shell unit that carries a Kernel flow.Commit publishes the lifecycle events
// of that commit. The unit appends
// event.ExcludeOwned(event.ProjectLifecycle(pre, commit), owned...) to the
// events it already emits itself, so every transition class is published
// exactly once. The pre-image is built from the state the service already read
// for its guarded decision; no extra read happens here.

import (
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// lifecyclePreImage builds the version-guarded pre-image of the execution the
// service already loaded. Actions stay in declaration order.
func lifecyclePreImage(scope tenancy.Scope, current kernel.Execution) event.ExecutionPreImage {
	actions := make([]event.ActionPreImage, len(current.Actions))
	for position, action := range current.Actions {
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
		ExecutionID: current.ID,
		Version:     current.Version,
		State:       current.State,
		Actions:     actions,
	}
}

// appendLifecycleProjection returns the unit's own events in their declared
// order followed by the retained lifecycle projection in its total order. A
// projection refusal is returned unchanged, so the caller fails the operation
// closed before the unit is committed.
func appendLifecycleProjection(
	own []event.Envelope,
	pre event.ExecutionPreImage,
	commit flow.Commit,
	owned ...event.OwnedEvent,
) ([]event.Envelope, error) {
	projected, err := event.ProjectLifecycle(pre, commit)
	if err != nil {
		return nil, err
	}
	remainder := event.ExcludeOwned(projected, owned...)
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
