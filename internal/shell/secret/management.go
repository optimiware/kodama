// Package secret: management and activation-resolution declarations.
//
// These declarations are the consumer-owned seams on top of the declared
// envelope vocabulary. A stored secret is one encrypted current value in one
// tenant/project scope; management reads expose Metadata only, and the
// encrypted Envelope never leaves the management/resolution boundary.
//
// The persistence seam is the neutral store.Executor commit unit: a write,
// replacement or delete commits together with its receipt, audit row and
// config.changed publication intent, so no durable result can commit apart from
// the business change it protects.
package secret

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Stored is one current stored secret: readable metadata, its creation time and
// the encrypted envelope. It never carries plaintext.
type Stored struct {
	Metadata  Metadata
	CreatedAt time.Time
	Envelope  Envelope
}

// EnvelopeCipher is the consumer-owned envelope seam mirrored from the key
// adapter. It seals and opens one value under scoped associated data and never
// logs plaintext.
type EnvelopeCipher interface {
	Seal(context.Context, AssociatedData, []byte) (Envelope, error)
	Open(context.Context, AssociatedData, Envelope) ([]byte, error)
}

// NameLookup retrieves the current stored value for one exact scoped name. An
// empty ProjectID reads the tenant-wide row. A missing row is fault.NotFound;
// an unreadable row is a dependency failure. Lookup never widens or rewrites
// the supplied scope.
type NameLookup interface {
	Lookup(context.Context, tenancy.Scope, id.SecretName) (Stored, error)
}

// NamePolicy authorizes one declared secret name for one scope at activation.
// Check returns nil only for an explicit allow.
type NamePolicy interface {
	Check(context.Context, tenancy.Scope, id.SecretName) error
}

// Clock is the injected authoritative time source for management commits.
type Clock interface {
	Now() time.Time
}

// IDSource allocates the durable identities one management commit needs.
type IDSource interface {
	NewSecretID() (SecretID, error)
	NewReceiptID() (id.ReceiptID, error)
	NewEventID() (id.EventID, error)
}

// ManagerConfig is the complete management boundary set. InstallationID and a
// positive KeyVersion are required; every port is required.
type ManagerConfig struct {
	InstallationID string
	KeyVersion     KeyVersion
	Clock          Clock
	IDs            IDSource
	Executor       store.Executor
	// Scanner is the scoped multi-row read seam the commit-boundary membership
	// guard uses. A membership row's id is allocated by the identity adapter and
	// is never the principal id, so a human authority read scans memberships by
	// principal_id inside the caller's scope instead of reading by principal id.
	Scanner store.Scanner
	Cipher  EnvelopeCipher
	Lookup  NameLookup
}

// PutCommand inserts a new current value when ExpectedVersion is zero and
// replaces the current value when it is positive. The value is sealed before
// the guarded commit; it never appears in the receipt, audit, event or error.
//
// ActorKind is the authorized principal kind of Actor. A human actor is guarded
// by its current membership row and a service actor by its current service
// account row; the commit boundary re-reads that exact row, so it can never
// substitute the other kind's grant.
type PutCommand struct {
	Scope           tenancy.Scope
	Name            id.SecretName
	Value           string
	ExpectedVersion uint64 // 0 inserts; a positive value replaces
	Actor           id.PrincipalID
	ActorKind       identity.PrincipalKind
	CallerKey       string
	Content         receipt.Content
}

// DeleteCommand removes the current value at the expected version.
type DeleteCommand struct {
	Scope           tenancy.Scope
	Name            id.SecretName
	ExpectedVersion uint64
	Actor           id.PrincipalID
	ActorKind       identity.PrincipalKind
	CallerKey       string
	Content         receipt.Content
}

// ResolverConfig is the complete activation-resolution boundary set.
type ResolverConfig struct {
	InstallationID string
	Policy         NamePolicy
	Lookup         NameLookup
	Cipher         EnvelopeCipher
}
