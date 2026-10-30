// Package bus carries the kernel's committed events from the audit_events
// outbox to consumers: a port with an in-process implementation for
// development and a NATS JetStream adapter, and the Publisher that drains the
// outbox into it.
//
// Delivery is at least once. An event's identity is its audit_events ID and
// never changes, so a consumer deduplicates on a resource it owns (for example
// runs.trigger_key), not on the broker.
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// SubjectRoot starts every subject of the kernel's events. A subject is
// kodama.v1.<tenant-key>.<project-key>.<event-type>, with "_" for the project of
// a tenant-wide event; an event type's dots are further subject tokens.
const SubjectRoot = "kodama.v1"

// TenantWide is the project segment of an event that has no project.
const TenantWide = "_"

var (
	keyToken  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	typeToken = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}(\.[a-z][a-z0-9_]{0,62}){0,3}$`)
)

// Subject returns the subject of an event. The keys and the event type must be in
// the syntax of their columns, so no token can contain a dot, a wildcard or a
// space and an event can never be published under another tenant's subject.
func Subject(tenantKey, projectKey, eventType string) (string, error) {
	if !keyToken.MatchString(tenantKey) {
		return "", fmt.Errorf("bus: tenant key %q is not a subject token", truncate(tenantKey))
	}
	if projectKey == "" {
		projectKey = TenantWide
	} else if !keyToken.MatchString(projectKey) {
		return "", fmt.Errorf("bus: project key %q is not a subject token", truncate(projectKey))
	}
	if !typeToken.MatchString(eventType) {
		return "", fmt.Errorf("bus: event type %q is not a subject", truncate(eventType))
	}
	return SubjectRoot + "." + tenantKey + "." + projectKey + "." + eventType, nil
}

func truncate(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// Actor is who caused an event.
type Actor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Message is one committed event as consumers receive it. ID is the stable
// identity of the event (its audit_events ID).
type Message struct {
	ID           string           `json:"id"`
	TenantID     domain.TenantID  `json:"tenantId"`
	ProjectID    domain.ProjectID `json:"projectId,omitempty"`
	Type         string           `json:"type"`
	ResourceType string           `json:"resourceType"`
	ResourceID   string           `json:"resourceId"`
	Actor        Actor            `json:"actor"`
	OccurredAt   time.Time        `json:"occurredAt"`
	Data         json.RawMessage  `json:"data"`
}

// Encode is the wire form of a message.
func (m Message) Encode() ([]byte, error) { return json.Marshal(m) }

// DecodeMessage reads the wire form and checks what a consumer relies on.
func DecodeMessage(data []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, fmt.Errorf("bus: malformed message: %w", err)
	}
	if m.ID == "" || m.Type == "" || m.TenantID.Validate() != nil {
		return Message{}, errors.New("bus: a message needs an id, a type and a tenant")
	}
	return m, nil
}

// Sink accepts a message durably or fails. A nil error means the message will be
// delivered to every consumer at least once.
type Sink interface {
	Publish(ctx context.Context, subject string, m Message) error
}

// Handler processes one delivery. A nil error settles it; an error leaves it to
// be delivered again.
type Handler func(ctx context.Context, m Message) error

// Subscriber attaches a consumer. Name identifies a durable consumer: consumers
// sharing a name share the deliveries. Filter is a subject filter in NATS syntax
// ("*" is one token, ">" the rest). The returned function stops the consumer.
type Subscriber interface {
	Subscribe(ctx context.Context, name, filter string, h Handler) (stop func(), err error)
}

// Bus is both sides of a broker.
type Bus interface {
	Sink
	Subscriber
}

// MatchSubject reports whether a subject matches a NATS-syntax filter.
func MatchSubject(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, tok := range f {
		switch {
		case tok == ">":
			return i < len(s)
		case i >= len(s):
			return false
		case tok != "*" && tok != s[i]:
			return false
		}
	}
	return len(f) == len(s)
}
