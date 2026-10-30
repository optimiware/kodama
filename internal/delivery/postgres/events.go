package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
)

// EventAppender appends an event to the outbox inside the caller's tenant
// transaction. EventsFrom adapts the event bus's Append; it must join tx and
// never commit it. A full backlog must return an error wrapping
// delivery.ErrBackpressure, which rolls the deployment change back.
type EventAppender func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error

// EventsFrom adapts the durable event bus's transaction-local append. Broker
// availability is never required.
func EventsFrom(store *eventspg.Store) EventAppender {
	return func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		_, err := store.Append(ctx, tx, env)
		if errors.Is(err, events.ErrBacklogFull) {
			return fmt.Errorf("%w: %v", delivery.ErrBackpressure, err)
		}
		return err
	}
}

// stateChanged builds the deployment.state_changed envelope: references only,
// never kubeconfig content, secret values or manifests. The event ID is derived
// from the deployment and its committed version, so a retried transition is
// the same event.
func stateChanged(m delivery.Mutation, d delivery.Deployment, fromName string) (eventsv1.Envelope, error) {
	rec := d.Record()
	commit, digest := rec.Source.Commit, string(rec.Image.Digest)
	// The event carries one commit only for a single source. A deployment of a
	// source set names every repository and commit in its record, which a
	// consumer reloads by the reference; no one commit stands for the set.
	var sourceCommit *string
	if commit != "" {
		sourceCommit = &commit
	}
	data, err := json.Marshal(eventsv1.DeploymentStateChanged{
		EnvironmentID: string(rec.Environment), From: fromName, To: string(rec.Status),
		SourceCommit: sourceCommit, ArtifactIDs: []string{string(rec.Image.ArtifactID)}, ImageDigest: &digest,
	})
	if err != nil {
		return eventsv1.Envelope{}, err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("deployment-state|%s|%d", rec.ID, rec.Version)))
	correlation := m.RequestID
	if correlation == "" {
		correlation = "delivery_" + string(rec.ID)
	}
	var actor eventsv1.ActorType
	switch m.Actor.Type {
	case delivery.ActorUser:
		actor = eventsv1.ActorUser
	case delivery.ActorWorkload:
		actor = eventsv1.ActorWorkload
	default:
		actor = eventsv1.ActorService
	}
	return eventsv1.Envelope{
		ID: "evt_dep_" + hex.EncodeToString(sum[:16]), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeDeploymentStateChanged,
		OccurredAt: m.Now.UTC(), TenantID: rec.Scope.TenantID, ProjectID: rec.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "deployment", ID: string(rec.ID), Version: int64(rec.Version)},
		Actor:    eventsv1.Actor{Type: actor, ID: m.Actor.ID}, CorrelationID: correlation, Data: data,
	}, nil
}
