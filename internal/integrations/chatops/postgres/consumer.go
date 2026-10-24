package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

// NewBinding contributes the ChatOps consumer to the event bus
// (eventspg.NewStore(..., binding)). The handler runs inside T-005's own inbox
// transaction, in a savepoint, and joins it: it selects the project's enabled
// destinations, applies their filters through the pure planner and inserts every
// matching delivery intent, so the intents and the inbox completion commit
// together or not at all. It never opens a transaction of its own and never
// performs DNS, credential access or HTTP: those happen only in the worker, after
// the intents are durable.
//
// Tenant-wide events and events nobody selected produce no rows. Delivery
// identity is (tenant, project, destination, event): redelivery of an event, a
// repair pass or a second process inserts nothing new, and an intent that
// already exists (delivered, failed or pending) is never reopened or changed;
// reopening a failed notification is the separate, authorized replay.
func NewBinding(planner chatops.Planner) (eventspg.Binding, error) {
	if !planner.Ready() {
		return eventspg.Binding{}, errors.New("chatops/postgres: a planner with a canonical API origin is required")
	}
	return eventspg.Binding{
		Spec: chatops.ConsumerSpec(),
		Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
			return handle(ctx, tx, planner, env)
		},
	}, nil
}

func handle(ctx context.Context, tx pgx.Tx, planner chatops.Planner, env eventsv1.Envelope) error {
	if env.ProjectID == "" || !chatops.IsSupportedEvent(env.Type) {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT `+destinationColumns+` FROM chatops_destinations
WHERE tenant_id = $1 AND project_id = $2 AND enabled AND $3 = ANY(events) ORDER BY id`,
		string(env.TenantID), string(env.ProjectID), env.Type)
	if err != nil {
		return events.Transient(fmt.Errorf("chatops: select destinations: %w", err))
	}
	var dests []chatops.Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			rows.Close()
			return events.Transient(fmt.Errorf("chatops: read destination: %w", err))
		}
		dests = append(dests, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return events.Transient(fmt.Errorf("chatops: read destinations: %w", err))
	}
	intents, err := planner.Plan(env, dests)
	if err != nil {
		// The same event can never project differently: do not retry it forever.
		return events.Permanent(fmt.Errorf("chatops: project event: %w", err))
	}
	for _, d := range intents {
		if err := insertIntent(ctx, tx, d); err != nil {
			if errors.Is(err, chatops.ErrConflict) {
				return events.Permanent(err)
			}
			return events.Transient(err)
		}
	}
	return nil
}

// insertIntent inserts one intent, or recognizes the existing one. Content is
// immutable: a conflicting digest under the same identity is an error, never an
// update.
func insertIntent(ctx context.Context, tx pgx.Tx, d chatops.Delivery) error {
	tag, err := tx.Exec(ctx, `INSERT INTO chatops_deliveries (tenant_id, project_id, id, destination_id, event_id, event_type, binding_revision,
payload, payload_digest, state, next_attempt_at, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10, 1, $11, $11)
ON CONFLICT (tenant_id, project_id, destination_id, event_id) DO NOTHING`,
		string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.ID), string(d.DestinationID), d.EventID, d.EventType, d.BindingRevision,
		d.Payload, d.PayloadDigest, d.NextAttemptAt.UTC(), d.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("chatops: insert intent: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var digest string
	if err := tx.QueryRow(ctx, `SELECT payload_digest FROM chatops_deliveries
WHERE tenant_id = $1 AND project_id = $2 AND destination_id = $3 AND event_id = $4`,
		string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.DestinationID), d.EventID).Scan(&digest); err != nil {
		return fmt.Errorf("chatops: read existing intent: %w", err)
	}
	if digest != d.PayloadDigest {
		return chatops.Fail(chatops.ErrConflict, nil, "the delivery exists with different content")
	}
	return nil
}
