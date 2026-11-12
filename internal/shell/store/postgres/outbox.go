// Event outbox over the events table with the kodama_publisher role.
//
// The publisher role has events-only SELECT and a publish_state-column UPDATE.
// It cannot read a domain table and cannot write any other event column.
// Pending never carries a publication high-water mark: the pending state is the
// truth, so a row that commits late with a lower sequence than an acknowledged
// row is still returned, and a restart scans from zero.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Outbox implements event.Outbox over the publisher-role connection pool.
type Outbox struct {
	db DB
}

// NewOutbox builds the durable event outbox over the kodama_publisher pool.
func NewOutbox(db DB) *Outbox {
	return &Outbox{db: db}
}

var _ event.Outbox = (*Outbox)(nil)

// outboxColumns is the event row projection the publisher reads. It deliberately
// omits publish_state: the row is pending by construction and the publisher has
// no reason to select a column it may only update to a fixed value.
var outboxColumns = []string{
	"id", "tenant_id", "project_id", "type", "report_type", "subject_id",
	"subject_version", "source_kind", "source_principal", "correlation_id",
	"causation_id", "at", "data", "publish_sequence",
}

const outboxPendingSQL = `SELECT id, tenant_id, project_id, type, report_type, subject_id, subject_version, source_kind, source_principal, correlation_id, causation_id, at, data, publish_sequence FROM events WHERE publish_state = 'pending' AND publish_sequence > $1 ORDER BY publish_sequence ASC LIMIT $2`

// Pending returns at most limit committed unacknowledged event rows in durable
// sequence order. after is an exclusive lower bound for bounded pagination only;
// the dispatcher always starts from zero so a late lower-sequence row is never
// skipped.
func (outbox *Outbox) Pending(ctx context.Context, after event.Sequence, limit int) ([]event.PendingEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, faultInvalid(fmt.Sprintf("outbox limit %d is not positive", limit))
	}
	if limit > store.MaxQueryLimit {
		limit = store.MaxQueryLimit
	}
	tx, err := beginUnscoped(ctx, outbox.db, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, store.Query{SQL: outboxPendingSQL, Args: []any{int64(after), limit}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	meta, ok := lookupTable("events")
	if !ok {
		return nil, faultInternal("the frozen schema has no events table")
	}
	pending := make([]event.PendingEvent, 0, limit)
	for rows.Next() {
		values, err := scanColumns(meta, outboxColumns, rows)
		if err != nil {
			return nil, err
		}
		row, err := pendingEventFromValues(values)
		if err != nil {
			return nil, err
		}
		pending = append(pending, row)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pending, nil
}

// MarkPublished acknowledges exactly one event id and sequence. An already
// published row is treated as acknowledged so a retry after a lost
// acknowledgement is idempotent; any other zero-row outcome is not_found.
func (outbox *Outbox) MarkPublished(ctx context.Context, eventID id.EventID, sequence event.Sequence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := id.ParseEventID(string(eventID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "event id is invalid", err)
	}
	if sequence == 0 {
		return fault.New(fault.InvalidInput, "event sequence is zero")
	}
	tx, err := beginUnscoped(ctx, outbox.db, false)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	result, err := tx.Exec(ctx, store.Statement{
		SQL:  `UPDATE events SET publish_state = 'published' WHERE id = $1 AND publish_sequence = $2 AND publish_state = 'pending'`,
		Args: []any{string(eventID), int64(sequence)},
	})
	if err != nil {
		return err
	}
	if result.RowsAffected == 0 {
		acknowledged, err := eventAlreadyPublished(ctx, tx, eventID, sequence)
		if err != nil {
			return err
		}
		if !acknowledged {
			return faultNotFound(fmt.Sprintf("event %q at sequence %d is not pending", eventID, sequence))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
}

func eventAlreadyPublished(ctx context.Context, tx store.Transaction, eventID id.EventID, sequence event.Sequence) (bool, error) {
	rows, err := tx.Query(ctx, store.Query{
		SQL:  `SELECT publish_state FROM events WHERE id = $1 AND publish_sequence = $2`,
		Args: []any{string(eventID), int64(sequence)},
	})
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Close(); err != nil {
			return false, err
		}
		return false, rows.Err()
	}
	var state string
	if err := rows.Scan(&state); err != nil {
		return false, fault.Wrap(fault.InternalFailure, "read event publication state", err)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	return state == "published", rows.Err()
}

// pendingEventFromValues rebuilds one committed event envelope and its durable
// sequence from the event row.
func pendingEventFromValues(values map[string]any) (event.PendingEvent, error) {
	sequence, ok := values["publish_sequence"].(int64)
	if !ok || sequence <= 0 {
		return event.PendingEvent{}, faultInternal("event row has no durable publication sequence")
	}
	identity, _ := values["id"].(string)
	tenant, _ := values["tenant_id"].(string)
	project, _ := values["project_id"].(string)
	eventType, _ := values["type"].(string)
	sourceKind, _ := values["source_kind"].(string)
	sourcePrincipal, _ := values["source_principal"].(string)
	subject, _ := values["subject_id"].(string)
	at, _ := values["at"].(time.Time)
	envelope := event.Envelope{
		ID:        id.EventID(identity),
		Type:      event.Type(eventType),
		Source:    event.Source{Kind: event.SourceKind(sourceKind), PrincipalID: id.PrincipalID(sourcePrincipal)},
		At:        at,
		Scope:     tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		SubjectID: subject,
	}
	if reportType, ok := values["report_type"].(string); ok {
		envelope.ReportType = event.ReportType(reportType)
	}
	if version, ok := values["subject_version"].(int64); ok {
		envelope.SubjectVersion = uint64(version)
	}
	if correlation, ok := values["correlation_id"].(string); ok {
		envelope.CorrelationID = correlation
	}
	if causation, ok := values["causation_id"].(string); ok {
		envelope.CausationID = causation
	}
	if data, ok := values["data"].([]byte); ok {
		envelope.Data = data
	}
	return event.PendingEvent{Sequence: event.Sequence(sequence), Event: envelope}, nil
}
