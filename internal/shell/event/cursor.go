package event

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// CursorPort reads the latest committed event-table sequence for one scope.
//
// It is the consumer-owned resume seam for event.subscribe setup. The event
// table is the durable stream, so the resume point is read from committed rows,
// never inferred from a broker position. A subscription never owns a second
// event store.
type CursorPort interface {
	Cursor(context.Context, tenancy.Scope) (uint64, error)
}

// CredentialValidity reads the authoritative expiry of the principal's current
// credential. A subscription grant may never outlive it; the caller still
// revalidates the current credential and grant on every delivery and reconnect.
type CredentialValidity interface {
	ValidUntil(context.Context, identity.Principal) (time.Time, error)
}
