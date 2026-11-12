package nats

import (
	"context"
	"encoding/json"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Publish is the event.Publisher seam. It publishes the same already-committed
// envelope as PublishEvent.
func (broker *Broker) Publish(ctx context.Context, envelope event.Envelope) error {
	return broker.PublishEvent(ctx, envelope)
}

// PublishEvent publishes one already-committed event envelope. The envelope is
// validated before any dial, the subject derives from envelope.Scope only, the
// payload is the canonical public event JSON and the stable event id is the
// JetStream publication identity so a retry is deduplicated.
func (broker *Broker) PublishEvent(ctx context.Context, envelope event.Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	subject, err := EventSubject(envelope.Scope)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "event payload could not be encoded", err)
	}
	return broker.publishJetStream(ctx, subject, envelope.ID, payload)
}

// publishJetStream publishes with the stable identity and bounded retries. A
// failed attempt discards the suspect client, so the next attempt dials again
// through Connect and revalidates every live subscription. Retries keep the
// same id and payload; the broker deduplicates.
func (broker *Broker) publishJetStream(ctx context.Context, subject Subject, eventID id.EventID, payload []byte) error {
	return broker.publishIdentified(ctx, subject, eventID, payload, "event publication")
}

// publishIdentified is the shared identity-preserving publication loop used by
// committed events and credential invalidations. A failed attempt discards the
// suspect client, so the next attempt dials again through Connect and
// revalidates every live subscription. Retries keep the same identity and
// payload; the broker deduplicates.
func (broker *Broker) publishIdentified(ctx context.Context, subject Subject, messageID id.EventID, payload []byte, what string) error {
	var lastErr error
	for attempt := 1; attempt <= broker.maxPublishAttempts; attempt++ {
		if broker.isClosed() {
			return fault.New(fault.DependencyUnavailable, "broker adapter is closed")
		}
		if err := ctx.Err(); err != nil {
			return fault.Wrap(fault.DeadlineExpired, what+" was canceled", err)
		}
		client, err := broker.ensureClient(ctx)
		if err != nil {
			lastErr = err
			if broker.isClosed() {
				return fault.New(fault.DependencyUnavailable, "broker adapter is closed")
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fault.Wrap(fault.DeadlineExpired, what+" was canceled", ctxErr)
			}
		} else {
			err = client.PublishJetStream(ctx, subject, messageID, payload)
			if err == nil {
				return nil
			}
			lastErr = err
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fault.Wrap(fault.DeadlineExpired, what+" was canceled", ctxErr)
			}
			broker.discardClient(ctx, client)
		}
		if attempt == broker.maxPublishAttempts {
			break
		}
		if err := broker.clock.Sleep(ctx, broker.backoff(attempt)); err != nil {
			return fault.Wrap(fault.DeadlineExpired, what+" was canceled", err)
		}
	}
	return fault.Wrap(fault.DependencyUnavailable, what+" failed", lastErr)
}

// PublishLive publishes one live log chunk on Core NATS. The subject derives
// only from the explicit scope and the chunk execution id; a failure is
// returned to the caller and never rolled into domain progress.
func (broker *Broker) PublishLive(ctx context.Context, scope tenancy.Scope, chunk log.Chunk) error {
	subject, err := LiveLogSubject(scope, chunk)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "live log payload could not be encoded", err)
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.DeadlineExpired, "live publication was canceled", err)
	}
	client, err := broker.ensureClient(ctx)
	if err != nil {
		return dependencyError("live publication failed", err)
	}
	if err := client.PublishCore(ctx, subject, payload); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "live publication failed", err)
	}
	return nil
}
