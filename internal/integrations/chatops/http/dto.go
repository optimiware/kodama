package chatopshttp

import (
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// The wire types are generated from api/chatops.yaml. This file maps them to
// and from the use cases' values. No entity is serialized directly: a field
// reaches the wire only by being listed here, so adding one to an entity cannot
// leak it. Deliveries never expose the endpoint, the credential reference, the
// payload or any receiver response.

func destinationDTO(d chatops.Destination) Destination {
	events := make([]EventType, 0, len(d.Events))
	for _, e := range d.Events {
		events = append(events, EventType(e))
	}
	out := Destination{
		Id: httpapi.ID(d.ID), Name: d.Name, Endpoint: d.Endpoint, Events: events, Enabled: d.Enabled, BindingRevision: d.BindingRevision,
		Version: httpapi.Version(d.Version), CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
	}
	if d.Credential != "" {
		c := d.Credential
		out.Credential = &c
	}
	return out
}

func eventStrings(in []EventType) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, string(e))
	}
	return out
}

func destinationInput(in DestinationCreate) chatops.DestinationInput {
	out := chatops.DestinationInput{Name: in.Name, Endpoint: in.Endpoint, Events: eventStrings(in.Events), Enabled: true}
	if in.Credential != nil {
		out.Credential = *in.Credential
	}
	if in.Enabled != nil {
		out.Enabled = *in.Enabled
	}
	return out
}

func destinationEdit(in DestinationEdit) chatops.DestinationEdit {
	out := chatops.DestinationEdit{Name: in.Name, Endpoint: in.Endpoint, Credential: in.Credential, Enabled: in.Enabled}
	if in.Events != nil {
		out.Events = eventStrings(*in.Events)
	}
	return out
}

func deliveryDTO(d chatops.Delivery) Delivery {
	out := Delivery{
		Id: httpapi.ID(d.ID), DestinationId: httpapi.ID(d.DestinationID), EventId: d.EventID, EventType: EventType(d.EventType),
		State: DeliveryState(d.State), Attempts: d.Attempts, Replays: d.Replays, BindingRevision: d.BindingRevision,
		Version: httpapi.Version(d.Version), CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
	}
	if d.State == chatops.StatePending {
		t := d.NextAttemptAt.UTC()
		out.NextAttemptAt = &t
	}
	if d.LastClass != "" {
		f := Failure{Class: FailureClass(d.LastClass), Uncertain: d.LastUncertain}
		if d.LastStatus != 0 {
			s := d.LastStatus
			f.HttpStatus = &s
		}
		out.LastFailure = &f
	}
	if d.FailedReason != "" {
		r := FailedReason(d.FailedReason)
		out.FailedReason = &r
	}
	if d.Receipt != "" {
		r := d.Receipt
		out.Receipt = &r
	}
	if !d.DeliveredAt.IsZero() {
		t := d.DeliveredAt.UTC()
		out.DeliveredAt = &t
	}
	return out
}

func attemptDTO(a chatops.Attempt) Attempt {
	out := Attempt{Number: a.Number, Result: AttemptResult(a.Result), Uncertain: a.Uncertain, Replay: a.Replay, StartedAt: a.StartedAt.UTC()}
	if a.Result == "" {
		out.Result = "in_flight"
	}
	if a.Class != "" {
		c := FailureClass(a.Class)
		out.Class = &c
	}
	if a.HTTPStatus != 0 {
		s := a.HTTPStatus
		out.HttpStatus = &s
	}
	if a.Receipt != "" {
		r := a.Receipt
		out.Receipt = &r
	}
	if !a.FinishedAt.IsZero() {
		t := a.FinishedAt.UTC()
		out.FinishedAt = &t
	}
	return out
}

func detailDTO(v chatops.DeliveryView) DeliveryDetail {
	d := deliveryDTO(v.Delivery)
	hist := make([]Attempt, 0, len(v.Attempts))
	for _, a := range v.Attempts {
		hist = append(hist, attemptDTO(a))
	}
	out := DeliveryDetail{
		Attempts: d.Attempts, BindingRevision: d.BindingRevision, CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt, DestinationId: d.DestinationId,
		EventId: d.EventId, EventType: d.EventType, FailedReason: d.FailedReason, Id: d.Id, LastFailure: d.LastFailure, NextAttemptAt: d.NextAttemptAt,
		Receipt: d.Receipt, Replays: d.Replays, State: d.State, UpdatedAt: d.UpdatedAt, Version: d.Version, AttemptHistory: hist,
	}
	if v.DestinationName != "" {
		n := v.DestinationName
		out.DestinationName = &n
	}
	return out
}

func version(v int64) domain.Version { return domain.Version(v) }
