package nats

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
)

// Observe handles one message on the dedicated authenticated ingress subject.
// It resolves the presented credential, refuses a human principal, derives the
// event source and scope from the verified principal, requires event.publish in
// that scope and hands the observation to the sink. It never publishes to a
// committed-event subject.
func (broker *Broker) Observe(ctx context.Context, request ObservationRequest) error {
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "observation ingress was canceled", err)
	}

	principal, err := broker.resolveCurrent(ctx, request.Credential)
	if err != nil {
		return err
	}
	source, err := observationSource(principal)
	if err != nil {
		return err
	}
	if !request.Type.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event type %q", request.Type))
	}
	if request.Type != event.TypeCustomObservation {
		return fault.New(fault.Forbidden, "the observation ingress cannot publish a lifecycle or verified-report event")
	}
	if len(request.Data) > event.DefaultBounds.MaxDataBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("observation data is %d bytes; at most %d are allowed", len(request.Data), event.DefaultBounds.MaxDataBytes))
	}
	if request.DeclaredSource != nil && *request.DeclaredSource != source {
		return fault.New(fault.Forbidden, "declared observation source does not match authentication")
	}
	if request.DeclaredScope != nil && *request.DeclaredScope != principal.Scope {
		return fault.New(fault.Forbidden, "declared observation scope does not match authentication")
	}
	if err := broker.authorize(ctx, principal, OperationEventPublish, principal.Scope); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "observation ingress was canceled", err)
	}

	observation := Observation{
		Scope:  principal.Scope,
		Source: source,
		Type:   request.Type,
		Data:   request.Data,
	}
	if err := broker.sink.Observe(ctx, observation); err != nil {
		return dependencyError("observation sink failed", err)
	}
	return nil
}

// observationSource derives the event source from the authenticated principal
// kind. A human principal can never use the ingress.
func observationSource(principal identity.Principal) (event.Source, error) {
	switch principal.Kind {
	case identity.KindExecution:
		return event.Source{Kind: event.SourceExecution, PrincipalID: principal.ID}, nil
	case identity.KindService:
		return event.Source{Kind: event.SourceService, PrincipalID: principal.ID}, nil
	case identity.KindHuman:
		return event.Source{}, fault.New(fault.Forbidden, "the observation ingress is not open to human principals")
	default:
		return event.Source{}, fault.New(fault.Unauthenticated, "principal kind cannot use the observation ingress")
	}
}
