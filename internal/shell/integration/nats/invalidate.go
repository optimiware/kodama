package nats

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/token"
)

// Invalidate is the token.InvalidationPort seam. It delivers one committed
// credential invalidation to live consumers. Delivery is a post-commit
// retrying effect: it never rolls back the committed revocation, a failure is
// returned to the caller for retry, and a retried effect keeps the same
// message identity so the broker deduplicates it.
func (broker *Broker) Invalidate(ctx context.Context, invalidation token.Invalidation) error {
	if err := invalidation.Validate(); err != nil {
		return err
	}
	subject, err := InvalidationSubject(invalidation.Scope)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(invalidation)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "invalidation payload could not be encoded", err)
	}
	// Invalidation carries no credential material, so the canonical payload is
	// the whole message. The stable identity is derived from that payload: the
	// same committed invalidation retried by the post-commit effect keeps the
	// same broker message id.
	return broker.publishIdentified(ctx, subject, invalidationMessageID(payload), payload, "invalidation publication")
}

// invalidationMessageID derives the stable broker publication identity of one
// committed invalidation from its canonical payload. The identity is not a
// credential and reveals nothing beyond the payload the consumer already
// receives.
func invalidationMessageID(payload []byte) id.EventID {
	sum := sha256.Sum256(payload)
	return id.EventID("inv-" + hex.EncodeToString(sum[:]))
}
