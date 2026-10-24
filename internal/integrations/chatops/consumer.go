package chatops

import (
	"errors"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
)

// ConsumerName is the stable identity of the ChatOps durable consumer. The inbox
// is keyed by (tenant, name, event ID), so renaming it forfeits its
// deduplication evidence: the name never changes.
const ConsumerName = "chatops"

// ConsumerSpec is the consumer's contribution to the event bus. It is
// historical, because every unique event is a notification (older observations
// and several events at one resource version included), and an external-intent
// consumer: its handler only records durable delivery intents and never calls a
// receiver, so the bus refuses to replay beyond the deduplication horizon.
// IdempotentBeyondHorizon is deliberately false: repeating an intent whose
// evidence was pruned could notify a receiver twice.
func ConsumerSpec() events.ConsumerSpec {
	return events.ConsumerSpec{
		Name:                    ConsumerName,
		Mode:                    events.ModeHistorical,
		Effect:                  events.EffectExternalIntent,
		IdempotentBeyondHorizon: false,
		Types:                   append([]string(nil), SupportedEvents...),
	}
}

// Planner turns one event into delivery intents. It is pure: it is given the
// project's enabled destinations and performs no lookup, no DNS, no credential
// access and no HTTP, so it is safe to run inside the event bus's inbox
// transaction.
type Planner struct {
	links LinkBuilder
	now   func() time.Time
}

// Ready reports whether the planner was built by NewPlanner.
func (p Planner) Ready() bool { return p.links != (LinkBuilder{}) && p.now != nil }

// NewPlanner requires the link builder (the canonical API origin).
func NewPlanner(links LinkBuilder, clock func() time.Time) (Planner, error) {
	if links == (LinkBuilder{}) {
		return Planner{}, errors.New("chatops: a canonical API origin is required to build review links")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return Planner{links: links, now: clock}, nil
}

// Plan returns one pending delivery per destination that is enabled, belongs to
// the event's project and selects the event type. Tenant-wide events and event
// types outside the allow-list produce nothing. A projection failure is
// returned; the same event can never project differently, so the caller
// classifies it as permanent.
func (p Planner) Plan(env eventsv1.Envelope, destinations []Destination) ([]Delivery, error) {
	if env.ProjectID == "" || !IsSupportedEvent(env.Type) {
		return nil, nil
	}
	var out []Delivery
	now := p.now()
	for _, d := range destinations {
		if d.Scope.TenantID != env.TenantID || d.Scope.ProjectID != env.ProjectID || !d.Selects(env.Type) {
			continue
		}
		payload, err := Project(env, DeliveryIDFor(d.Scope, d.ID, env.ID), p.links)
		if err != nil {
			return nil, err
		}
		b, err := payload.Encode()
		if err != nil {
			return nil, err
		}
		del, err := NewDelivery(d, env.ID, env.Type, b, now)
		if err != nil {
			return nil, err
		}
		out = append(out, del)
	}
	return out, nil
}
