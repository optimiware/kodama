package chatops

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// The ports below are small and consumer-owned. PostgreSQL, the outbound HTTP
// client, the credential source and the identity authority implement or feed
// them from adapters; this package imports none of those technologies. A missing
// port fails closed: there is no allow-all or send-anyway default.

// Actor is an attributable principal. Destination configuration, delivery
// inspection and replay are human operations; any other actor is denied by the
// authorization adapter.
type Actor struct {
	Type string
	ID   string
}

// ActorUser is the human actor type.
const ActorUser = "user"

// Validate checks the actor can be attributed.
func (a Actor) Validate() error {
	if a.Type != ActorUser {
		return invalidf("an actor is a user")
	}
	if n := len(a.ID); n < 1 || n > 256 {
		return invalidf("an actor identifier is 1 to 256 bytes")
	}
	return nil
}

// Caller is the verified cause of a request. The permission an operation needs is
// named by the operation itself (see Permission), never by the caller.
type Caller struct {
	Actor     Actor
	RequestID string
}

// Permission names an authorization question. The adapter maps it to the
// identity module's permission of the same name: configuration and replay use
// project:settings, sanitized delivery inspection uses project:read. This is an
// adapter-local mapping to existing permissions, not a role of this module.
type Permission string

const (
	// PermSettings configures destinations and replays deliveries; it also reads
	// destination configuration, which stays restricted to settings
	// administrators.
	PermSettings Permission = "project:settings"
	// PermRead inspects the sanitized delivery status and attempts.
	PermRead Permission = "project:read"
)

// Store is the persistence port of the management use cases. Every method runs
// in one tenant-scoped transaction in which the implementation first takes the
// project lock, then repeats the permission check through transaction-bound
// authorization (so a revocation orders against the operation), and only then
// touches ChatOps rows. Authorization failures are ErrNotFound (hidden) or
// ErrDenied; an authority outage is ErrUnavailable and never access.
type Store interface {
	// CheckAccess answers whether the caller currently holds the permission in
	// the scope, in its own transaction. Use cases call it before any external
	// lookup so an unauthorized caller learns nothing from policy or credential
	// answers; the operation's own transaction still decides authoritatively.
	CheckAccess(ctx context.Context, c Caller, scope domain.Scope, perm Permission) error
	// CreateDestination inserts a destination (PermSettings), enforcing the
	// project's destination limit and name uniqueness.
	CreateDestination(ctx context.Context, c Caller, d Destination, limit int) (Destination, error)
	GetDestination(ctx context.Context, c Caller, scope domain.Scope, id DestinationID) (Destination, error)
	ListDestinations(ctx context.Context, c Caller, scope domain.Scope, after DestinationID, limit int) ([]Destination, error)
	// UpdateDestination locks the row, applies Destination.Edit against the
	// expected version and writes the result with its audit entry atomically.
	UpdateDestination(ctx context.Context, c Caller, scope domain.Scope, id DestinationID, expected int64, edit DestinationEdit, now time.Time) (Destination, error)

	ListDeliveries(ctx context.Context, c Caller, scope domain.Scope, f DeliveryFilter, after DeliveryPosition, limit int) ([]Delivery, error)
	GetDelivery(ctx context.Context, c Caller, scope domain.Scope, id DeliveryID) (DeliveryView, error)
	// ReplayDelivery applies Delivery.Replay against the expected version under
	// the destination's current binding, recording the audit entry in the same
	// transaction.
	ReplayDelivery(ctx context.Context, c Caller, scope domain.Scope, id DeliveryID, expected int64, reason string, now time.Time) (Delivery, error)
}

// DeliveryFilter narrows a delivery listing.
type DeliveryFilter struct {
	State       DeliveryState
	Destination DestinationID
}

// DeliveryPosition is the keyset position of a delivery listing, newest first.
type DeliveryPosition struct {
	CreatedAt time.Time
	ID        DeliveryID
}

// IsZero reports whether the position is the start of the listing.
func (p DeliveryPosition) IsZero() bool { return p.ID == "" }

// DeliveryView is a delivery with its complete attempt history, sanitized for
// inspection: no endpoint, credential reference, payload or response content.
type DeliveryView struct {
	Delivery Delivery
	// DestinationName is the label of the destination, when it still exists.
	DestinationName string
	Attempts        []Attempt
}

// Claim is one leased delivery handed to the worker together with the snapshot of
// the destination binding it was verified against.
type Claim struct {
	Delivery    Delivery
	Destination Destination
	// Attempt is the number of the attempt this claim admits.
	Attempt int
	Token   int64
	// LeaseExpiresAt is the absolute end of the claim.
	LeaseExpiresAt time.Time
}

// ClaimBatch is the result of one claim pass.
type ClaimBatch struct {
	Claims []Claim
	// Refused counts deliveries failed before any send because their
	// destination's binding changed; Exhausted counts expired claims that were
	// the cycle's last attempt.
	Refused, Exhausted int
}

// WorkStore is the persistence port of the worker. ClaimDue selects due work
// of one tenant, verifies the destination's current eligibility inside the claim
// transaction (a disabled destination is skipped, a changed binding is refused),
// records the attempt before returning, and commits before any network effect.
// Finish is conditioned on the current fencing token.
type WorkStore interface {
	ClaimDue(ctx context.Context, tenant domain.TenantID, now time.Time, lease time.Duration, limit int, p RetryPolicy) (ClaimBatch, error)
	Finish(ctx context.Context, c Claim, o Outcome, now time.Time, p RetryPolicy) (Delivery, error)
}

// FenceStore persists the restore fence of a tenant's deliveries.
type FenceStore interface {
	// SetDeliveryFence fences or clears a tenant's fence. While fenced, claims
	// return nothing.
	SetDeliveryFence(ctx context.Context, tenant domain.TenantID, fenced bool, operator Operator, reason string, now time.Time) error
}

// Operator is a platform operator (restore procedures are not tenant roles).
type Operator struct{ ID string }

// OperatorAuthorizer authorizes restore operations. A nil authorizer refuses
// every one of them.
type OperatorAuthorizer interface {
	AuthorizeOperator(ctx context.Context, op Operator, tenant domain.TenantID, action string) error
}

// SendRequest is everything one attempt may send. The adapter derives the headers
// from it; nothing arrives from a caller.
type SendRequest struct {
	Scope domain.Scope
	// Destination is the verified binding snapshot: endpoint, credential
	// reference and revision.
	Destination Destination
	DeliveryID  DeliveryID
	// IdempotencyKey is the stable receiver key, identical on every attempt and
	// every replay of the delivery.
	IdempotencyKey string
	EventID        string
	EventType      string
	Attempt        int
	// Payload is the exact stored bytes, identical on every attempt.
	Payload []byte
}

// Sender performs one outbound attempt. It resolves credentials, enforces the
// installation's outbound policy and classifies every failure into an Outcome; it
// has no error return so that no raw transport error can leak into records.
// The context carries the attempt deadline, which is below the claim lease.
type Sender interface {
	Send(ctx context.Context, r SendRequest) Outcome
}

// Credential is request authentication material resolved for one send. It is
// never stored, logged or returned.
type Credential struct {
	// Bearer is an opaque bearer token placed in the Authorization header.
	Bearer string
}

// CredentialSource resolves a credential reference for exactly one recipient. It
// must refuse (ErrCredentialUnavailable) a reference that is missing, revoked,
// foreign to the tenant/project, or not bound to the recipient, and report an
// outage as any other error. Merged run-pinned adapters (secrets.VersionSource,
// the gateway and MCP adapters) require a run that ChatOps must not fabricate,
// so a recipient-bound source is supplied by the installation (T-075/T-046
// select the binding; T-061 qualifies hosted custody). Without one, an
// authenticated destination cannot be activated.
type CredentialSource interface {
	Resolve(ctx context.Context, scope domain.Scope, ref string, r Recipient) (Credential, error)
}

// EndpointPolicy approves a destination endpoint before it is stored. The
// outbound adapter owns the policy (approved HTTPS destinations, address
// rules); the real address check still happens at dial time.
type EndpointPolicy interface {
	Approve(scope domain.Scope, endpoint string) error
}

// TenantSource enumerates tenants for the worker. It is the event bus's
// TenantSource seam; there is no unrestricted discovery.
type TenantSource interface {
	TenantIDs(ctx context.Context) ([]domain.TenantID, error)
}
