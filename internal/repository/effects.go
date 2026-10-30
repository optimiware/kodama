package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
)

// The adapters share the translation of a mutation's Effects into the rows they
// write, so PostgreSQL and memory produce identical audit records and
// identical event envelopes (contracts/events/v1).

// ResourceType is the audit and event resource type of a repository.
const ResourceType = eventsv1.ResourceTypeRepository

// AuditActorType maps the actor to the audit table's vocabulary (the event
// vocabulary's "service" is the audit table's "system").
func (a Actor) AuditActorType() string {
	if a.Type == ActorService {
		return "system"
	}
	return string(a.Type)
}

// Bounds of the audit columns.
const (
	maxAuditReason    = 2048
	maxAuditRequestID = 256
)

// AuditFields returns the reason and request ID of an audit record trimmed to
// the columns' bounds.
func (a Audit) AuditFields(ctx context.Context) (reason, requestID string) {
	reason, requestID = a.Reason, RequestID(ctx)
	if len(reason) > maxAuditReason {
		reason = reason[:maxAuditReason]
	}
	if len(requestID) > maxAuditRequestID {
		requestID = requestID[:maxAuditRequestID]
	}
	return reason, requestID
}

// EventID is deterministic per (repository, version, position): a retried
// transaction can never append a second event for the same committed change.
func EventID(r *Repository, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "repository/%s/%s/%s/%d/%d", r.Scope.TenantID, r.Scope.ProjectID, r.ID, r.Version, index))
	return "evt_repo_" + hex.EncodeToString(sum[:20])
}

// Envelope builds the catalog envelope of the index-th event of the change that
// made r current. correlationID is the request's; when it is empty newID
// supplies one. The adapter appends it to the outbox in the change's
// transaction.
func (e Event) Envelope(r *Repository, index int, at time.Time, correlationID string, newID func(prefix string) string) (eventsv1.Envelope, error) {
	var (
		typ  string
		data any
	)
	switch e.Kind {
	case EventCreated:
		typ = eventsv1.TypeRepositoryCreated
		data = eventsv1.RepositoryCreated{Name: r.Name, DefaultBranch: r.DefaultBranch, Hosting: string(r.Hosting)}
	case EventStateChanged:
		typ = eventsv1.TypeRepositoryStateChanged
		data = eventsv1.RepositoryStateChanged{From: e.From, To: e.To, Reason: e.Reason}
	default:
		return eventsv1.Envelope{}, fmt.Errorf("repository: unknown event kind %q", e.Kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return eventsv1.Envelope{}, err
	}
	if correlationID == "" {
		correlationID = newID("corr")
	}
	return eventsv1.Envelope{
		ID: EventID(r, index), SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: at.UTC(),
		TenantID: r.Scope.TenantID, ProjectID: r.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: ResourceType, ID: string(r.ID), Version: int64(r.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(e.Actor.Type), ID: e.Actor.ID},
		CorrelationID: correlationID, Data: raw,
	}, nil
}
