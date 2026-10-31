package outbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/bus"
)

// MaxDedupKeyLen bounds a deduplication key.
const MaxDedupKeyLen = 512

// EventID is the stable identity of an event: a digest of the tenant, the event
// type and the deduplication key the event catalog defines for that type
// (EC-01), for example the wait id of wait.resolved or the run id and run
// version of run.state_changed. The tenant is part of the digest so equal keys
// of two tenants never share an identity. The same inputs always yield the same
// id, so republishing a row yields the same event.
func EventID(tenant domain.TenantID, eventType, dedupKey string) (string, error) {
	if err := tenant.Validate(); err != nil {
		return "", err
	}
	if eventType == "" || strings.ContainsRune(eventType, 0) {
		return "", errors.New("outbox: an event needs a type")
	}
	if dedupKey == "" || len(dedupKey) > MaxDedupKeyLen || strings.ContainsRune(dedupKey, 0) {
		return "", fmt.Errorf("outbox: the deduplication key of %s must be 1..%d bytes", eventType, MaxDedupKeyLen)
	}
	h := sha256.New()
	for _, part := range []string{string(tenant), eventType, dedupKey} {
		fmt.Fprintf(h, "%d:%s\x00", len(part), part)
	}
	return "evt_" + hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// Intent is an event the caller decided to publish as part of a state change.
type Intent struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID // empty for a tenant-wide event
	// TenantKey and ProjectKey name the subject; ProjectKey is empty for a
	// tenant-wide event.
	TenantKey, ProjectKey string
	Type                  string
	// DedupKey is the deduplication key of the event type (event catalog).
	DedupKey                 string
	ResourceType, ResourceID string
	Actor                    bus.Actor
	OccurredAt               time.Time
	// Data carries references and bounded metadata only; nil becomes {}.
	Data json.RawMessage
}

// Row is a committed outbox entry.
type Row struct {
	bus.Message
	TenantKey, ProjectKey string
	// Cursor is the stable position of the row in the store's listing of
	// unpublished rows: opaque, assigned when the row is enqueued, never changed
	// by a retry, and strictly increasing in the listing order, so comparing two
	// cursors as strings orders the rows. Reconciliation pages by it.
	Cursor      string
	Attempts    int
	NextAttempt time.Time
	CreatedAt   time.Time
}

// Row builds the outbox row of the intent: the identity, the validated subject
// and the wire message. It fails before any mutation happens when the event
// could not be published.
func (i Intent) Row(now time.Time) (Row, error) {
	id, err := EventID(i.TenantID, i.Type, i.DedupKey)
	if err != nil {
		return Row{}, err
	}
	if i.ProjectID != "" {
		if err := i.ProjectID.Validate(); err != nil {
			return Row{}, err
		}
		if i.ProjectKey == "" {
			return Row{}, errors.New("outbox: a project event needs its project key")
		}
	}
	if _, err := bus.Subject(i.TenantKey, i.ProjectKey, i.Type); err != nil {
		return Row{}, err
	}
	data := i.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	} else if !json.Valid(data) {
		return Row{}, fmt.Errorf("outbox: the data of %s is not JSON", i.Type)
	}
	at := i.OccurredAt
	if at.IsZero() {
		at = now
	}
	return Row{
		Message: bus.Message{ID: id, TenantID: i.TenantID, ProjectID: i.ProjectID, Type: i.Type, ResourceType: i.ResourceType,
			ResourceID: i.ResourceID, Actor: i.Actor, OccurredAt: at.UTC(), Data: data},
		TenantKey: i.TenantKey, ProjectKey: i.ProjectKey, CreatedAt: now.UTC(), NextAttempt: now.UTC(),
	}, nil
}

// Subject is the bus subject of the row.
func (r Row) Subject() (string, error) { return bus.Subject(r.TenantKey, r.ProjectKey, r.Type) }
