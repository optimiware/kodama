package secrets

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// ConsumerKindRun is the only consumer kind of local pins: a pipeline run.
const ConsumerKindRun = "run"

// ConsumerRef identifies a consumer that retains a secret version.
type ConsumerRef struct {
	Kind string
	ID   string
}

// Validate checks the reference.
func (c ConsumerRef) Validate() error {
	if c.Kind != ConsumerKindRun {
		return invalidf("unsupported consumer kind")
	}
	_, err := validID(c.ID)
	return err
}

// HandlingEffect is an external procedure that revocation obliges for a
// consumer. The module records and tracks it; it does not perform it.
type HandlingEffect string

const (
	// EffectTerminateConsumer: stop the consumer that already received the
	// value (execution owns cancellation).
	EffectTerminateConsumer HandlingEffect = "terminate_consumer"
	// EffectRotateIssuedCredential: revoke or rotate the credential at its
	// issuer, because delivered bytes cannot be recalled.
	EffectRotateIssuedCredential HandlingEffect = "rotate_issued_credential"
)

// Effects lists every effect created for each known consumer on revocation.
func Effects() []HandlingEffect {
	return []HandlingEffect{EffectTerminateConsumer, EffectRotateIssuedCredential}
}

// HandlingState is the explicit progress of one effect.
type HandlingState string

const (
	// HandlingPending: obliged and not yet acknowledged.
	HandlingPending HandlingState = "pending"
	// HandlingCompleted: acknowledged as performed by the owning procedure.
	HandlingCompleted HandlingState = "completed"
	// HandlingNotApplicable: acknowledged as unnecessary (for example the
	// value was never delivered).
	HandlingNotApplicable HandlingState = "not_applicable"
)

// ConsumerHandlingRecord is the plain persisted state of a ConsumerHandling.
type ConsumerHandlingRecord struct {
	Scope         domain.Scope
	ID            string
	SecretID      string
	SecretVersion int64
	Consumer      ConsumerRef
	Effect        HandlingEffect
	State         HandlingState
	// Delivered records whether the consumer had already resolved the value.
	Delivered  bool
	CreatedAt  time.Time
	ResolvedAt *time.Time
	ResolvedBy string
}

// ConsumerHandling tracks one external effect owed because a version was
// revoked. It is an acknowledged-effect record: pending until the owning
// procedure reports an outcome, never implicitly complete. Its existence does
// not claim that delivered bytes were recalled.
type ConsumerHandling struct{ rec ConsumerHandlingRecord }

// NewConsumerHandling creates a pending effect.
func NewConsumerHandling(scope domain.Scope, id, secretID string, version int64, consumer ConsumerRef, effect HandlingEffect, delivered bool, now time.Time) (ConsumerHandling, error) {
	return RestoreConsumerHandling(ConsumerHandlingRecord{
		Scope: scope, ID: id, SecretID: secretID, SecretVersion: version, Consumer: consumer,
		Effect: effect, State: HandlingPending, Delivered: delivered, CreatedAt: now.UTC(),
	})
}

// RestoreConsumerHandling validates persisted state.
func RestoreConsumerHandling(r ConsumerHandlingRecord) (ConsumerHandling, error) {
	if err := r.Scope.Validate(); err != nil {
		return ConsumerHandling{}, err
	}
	for _, id := range []string{r.ID, r.SecretID} {
		if _, err := validID(id); err != nil {
			return ConsumerHandling{}, err
		}
	}
	if err := r.Consumer.Validate(); err != nil {
		return ConsumerHandling{}, err
	}
	switch r.Effect {
	case EffectTerminateConsumer, EffectRotateIssuedCredential:
	default:
		return ConsumerHandling{}, invalidf("unsupported handling effect")
	}
	switch r.State {
	case HandlingPending:
		if r.ResolvedAt != nil {
			return ConsumerHandling{}, invalidf("a pending effect has no resolution")
		}
	case HandlingCompleted, HandlingNotApplicable:
		if r.ResolvedAt == nil || r.ResolvedBy == "" {
			return ConsumerHandling{}, invalidf("a resolved effect needs a time and an actor")
		}
	default:
		return ConsumerHandling{}, invalidf("unsupported handling state")
	}
	return ConsumerHandling{rec: r}, nil
}

func (h ConsumerHandling) Record() ConsumerHandlingRecord { return h.rec }
func (h ConsumerHandling) ID() string                     { return h.rec.ID }
func (h ConsumerHandling) State() HandlingState           { return h.rec.State }
func (h ConsumerHandling) Pending() bool                  { return h.rec.State == HandlingPending }
func (h ConsumerHandling) String() string {
	return fmt.Sprintf("handling %s %s/%d", h.rec.Effect, h.rec.SecretID, h.rec.SecretVersion)
}

// Acknowledge records the outcome of the external procedure. pending may become
// completed or not_applicable exactly once; repeating the same outcome is a
// no-op and a different outcome is rejected, so an acknowledgement is never
// silently rewritten.
func (h *ConsumerHandling) Acknowledge(outcome HandlingState, by string, now time.Time) (changed bool, err error) {
	if outcome != HandlingCompleted && outcome != HandlingNotApplicable {
		return false, invalidf("an acknowledgement is completed or not_applicable")
	}
	if by == "" {
		return false, invalidf("an acknowledging actor is required")
	}
	if h.rec.State == outcome {
		return false, nil
	}
	if h.rec.State != HandlingPending {
		return false, fmt.Errorf("%w: effect already %s", ErrInvalidTransition, h.rec.State)
	}
	t := now.UTC()
	h.rec.State, h.rec.ResolvedAt, h.rec.ResolvedBy = outcome, &t, by
	return true, nil
}

// BackupRetention states what is known about backups holding the deleted
// secret's ciphertext. This module cannot observe backups, so it never reports
// "none": unknown stays unresolved until the production retention decision
// (O-11) and its operator procedure provide evidence.
type BackupRetention string

const BackupRetentionUnresolved BackupRetention = "unresolved"

// RetentionReport accounts for what outlives a deletion (VS-12).
type RetentionReport struct {
	// PinnedVersions counts run pins that retain a version of the secret.
	PinnedVersions int
	// ActivePins counts pins whose consumer has not released them.
	ActivePins int
	// PendingHandling counts unacknowledged external effects.
	PendingHandling int
	// BackupRetention is the known backup status.
	BackupRetention BackupRetention
}

// RevocationResult describes a revocation.
type RevocationResult struct {
	Secret  Secret
	Version SecretVersion
	// Changed is false when the version was already revoked.
	Changed bool
	// Handling lists the effects owed to known consumers (all pending for a new
	// revocation; the existing ones for a repeated call).
	Handling []ConsumerHandling
}

// PendingHandlingCount counts effects not yet acknowledged.
func (r RevocationResult) PendingHandlingCount() int {
	n := 0
	for _, h := range r.Handling {
		if h.Pending() {
			n++
		}
	}
	return n
}
