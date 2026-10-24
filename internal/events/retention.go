package events

import (
	"fmt"
	"time"
)

// RetentionPolicy configures the independent lifetimes of publication history,
// observation history, consumer inboxes, quarantine and replay records. T-005
// provides the mechanism; production values are operational policy (decision
// O-11) and are never defaulted here: every field must be set explicitly.
type RetentionPolicy struct {
	// PublishedOutbox is how long published outbox rows and their attempt
	// history are kept. Unpublished rows are never pruned. A published row is
	// the persisted record of when the event was published, which bounds when
	// deduplication evidence may be forgotten, so rows are kept at least
	// BrokerRedelivery regardless of this value. Copies that arrive later
	// anyway (client buffers) are refused by the consumer-side evidence-horizon
	// check.
	PublishedOutbox time.Duration
	// Observation is how long the API observation history (cursors) is kept.
	Observation time.Duration
	// Inbox is how long completed inbox entries (deduplication evidence) are
	// kept after completion. Evidence is additionally kept while the event is
	// unpublished, within BrokerRedelivery of its publication, or inside an
	// active replay request's range.
	Inbox time.Duration
	// ResolvedQuarantine and CompletedReplay bound resolved operator records.
	// Unresolved quarantine and active replay requests are never pruned.
	ResolvedQuarantine time.Duration
	CompletedReplay    time.Duration
	// BrokerRedelivery is the longest the broker may still redeliver a message
	// (stream max age). Inbox must cover it, or a late redelivery would repeat
	// an effect.
	BrokerRedelivery time.Duration
	// ReplayWindow is the supported ordinary replay boundary: replay inside it
	// is protected by retained inbox evidence and observation history.
	ReplayWindow time.Duration
	// MaxClockSkew is the largest amount by which a producer's clock may run
	// ahead of the appending process's clock. Append refuses an event that
	// occurred further in the future (ErrFutureEvent). It bounds how long an
	// event can extend its own deduplication evidence: a consumer stamps the
	// completion of an event no earlier than the event's occurredAt (see
	// inbox completion), which is what makes the stale-append proof hold under
	// any producer clock skew, and this bound keeps that from being abused to
	// keep evidence forever.
	MaxClockSkew time.Duration
	// BatchSize bounds one cleanup transaction.
	BatchSize int
}

// Validate enforces the relationships that keep deduplication and replay safe.
func (p RetentionPolicy) Validate() error {
	positive := []struct {
		name string
		d    time.Duration
	}{
		{"published outbox", p.PublishedOutbox}, {"observation", p.Observation}, {"inbox", p.Inbox},
		{"resolved quarantine", p.ResolvedQuarantine}, {"completed replay", p.CompletedReplay},
		{"broker redelivery", p.BrokerRedelivery}, {"replay window", p.ReplayWindow}, {"maximum clock skew", p.MaxClockSkew},
	}
	for _, f := range positive {
		if f.d <= 0 {
			return fmt.Errorf("%w: retention %s must be positive (no production default is assumed)", ErrInvalidConfiguration, f.name)
		}
	}
	if p.BatchSize < 1 {
		return fmt.Errorf("%w: retention batch size must be at least 1", ErrInvalidConfiguration)
	}
	if p.Inbox < p.BrokerRedelivery {
		return fmt.Errorf("%w: inbox retention %s is shorter than broker redelivery %s; late redelivery would repeat effects", ErrInvalidConfiguration, p.Inbox, p.BrokerRedelivery)
	}
	if p.Inbox < p.ReplayWindow {
		return fmt.Errorf("%w: inbox retention %s is shorter than the replay window %s", ErrInvalidConfiguration, p.Inbox, p.ReplayWindow)
	}
	if p.Observation < p.ReplayWindow {
		return fmt.Errorf("%w: observation retention %s is shorter than the replay window %s", ErrInvalidConfiguration, p.Observation, p.ReplayWindow)
	}
	return nil
}

// BacklogLimits cap one tenant's unpublished outbox. Admission beyond a limit
// is rejected atomically; publishers, consumers and repair keep draining.
type BacklogLimits struct {
	MaxEvents int
	MaxBytes  int64
}

// Validate requires explicit positive limits.
func (l BacklogLimits) Validate() error {
	if l.MaxEvents < 1 || l.MaxBytes < 1 {
		return fmt.Errorf("%w: backlog limits must be positive", ErrInvalidConfiguration)
	}
	return nil
}
