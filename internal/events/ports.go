package events

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and consumer-owned. They use domain-oriented
// values only; PostgreSQL and JetStream adapters implement them and translate
// driver errors. There is deliberately no generic repository and no global
// handler registry.

// TenantSource enumerates tenants for background workers, which then use
// ordinary tenant-scoped transactions per tenant (storage.ListTenantIDs).
type TenantSource interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}

// Claim is one leased outbox event. Token is the fencing token: completion or
// failure is honored only while it is the row's current token.
//
// LeaseExpiresAt is the absolute end of the claim's lifetime. A holder must not
// hand the event to the broker at or after it and should bound any send so it
// ends by then. This reduces stale sends but is not fencing: a message already
// buffered by the client or handed to the network cannot be recalled, so late
// copies are made harmless by the consumer's evidence-horizon check, not by the
// publisher.
type Claim struct {
	TenantID     domain.TenantID
	EventID      string
	EventType    string
	SubjectToken string
	Payload      []byte
	Token        int64
	Attempt      int
	// LeaseExpiresAt is the absolute claim deadline (the store's persisted lease).
	LeaseExpiresAt time.Time
}

// PublicationStore is the publisher's view of the outbox. Implementations must
// not hold locks while the caller talks to the broker.
type PublicationStore interface {
	// ClaimDue leases up to limit due rows (new, retry-due, or with an expired
	// lease) of one tenant. Concurrent callers never receive the same row.
	ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int) ([]Claim, error)
	// Complete marks the row published iff c.Token is still current.
	Complete(ctx context.Context, c Claim, now time.Time) error
	// Fail records a classified failure and schedules the retry iff c.Token is current.
	Fail(ctx context.Context, c Claim, class FailureClass, message string, now time.Time) error
}

// PublicationStats reports one tenant's publication health without payloads.
type PublicationStats struct {
	Pending              int
	Claimed              int
	BacklogBytes         int64
	OldestUnpublishedAge time.Duration
	// Attempts is the total number of publication attempts recorded for
	// unpublished events.
	Attempts      int64
	NextRetryAt   time.Time
	LastClass     FailureClass
	LastError     string
	LastFailureAt time.Time
}

// StatsReader exposes PublicationStats.
type StatsReader interface {
	PublicationStats(ctx context.Context, tenant domain.TenantID, now time.Time) (PublicationStats, error)
}

// Message is what the publisher hands the broker. The adapter composes the
// subject from its namespace, the trusted subject token and the type, and uses
// ID as the broker's message-deduplication identity.
type Message struct {
	ID           string
	SubjectToken string
	EventType    string
	Data         []byte
}

// BrokerPublisher publishes one message. A nil return means durable broker
// acknowledgment; every other outcome, including timeouts after the request was
// sent, must be returned as an error so the row stays retryable.
type BrokerPublisher interface {
	Publish(ctx context.Context, m Message) error
}

// DeliveryIdentity is the bounded transport identity retained with
// quarantined deliveries so operators can correlate them with the broker.
type DeliveryIdentity struct {
	Stream      string
	StreamSeq   uint64
	Subject     string
	DeliveryNum int
}

// Delivery is one received message under a trusted scope. Scope comes from the
// subscription/subject mapping, never from the payload.
type Delivery struct {
	Tenant   domain.TenantID
	Consumer string
	Payload  []byte
	Identity DeliveryIdentity
}

// OutcomeKind is the transport-relevant result of processing a delivery.
type OutcomeKind string

const (
	// OutcomeCompleted: the local effect and inbox result committed; acknowledge.
	OutcomeCompleted OutcomeKind = "completed"
	// OutcomeDuplicate: already completed earlier; acknowledge without an effect.
	OutcomeDuplicate OutcomeKind = "duplicate"
	// OutcomeRetry: a transient failure was recorded durably; redeliver after RetryAfter.
	OutcomeRetry OutcomeKind = "retry"
	// OutcomeQuarantined: poison was durably quarantined; acknowledge.
	OutcomeQuarantined OutcomeKind = "quarantined"
)

// Outcome tells the transport adapter how to settle the message.
type Outcome struct {
	Kind         OutcomeKind
	RetryAfter   time.Duration
	QuarantineID string
	Reason       string
}

// Processor turns a received delivery into a durable local outcome. It returns
// an error only when no durable outcome could be recorded (for example the
// database is unavailable); the transport must then leave the message
// unacknowledged for redelivery.
type Processor interface {
	Process(ctx context.Context, d Delivery) (Outcome, error)
}

// SubjectResolver maps a server-assigned subject token to its tenant.
type SubjectResolver interface {
	ResolveSubjectToken(ctx context.Context, token string) (domain.TenantID, error)
}
