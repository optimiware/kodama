package platform

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// EventReader is the store-backed event.ReadPort.
//
// Read maps the scope-qualified cursor query to a bounded store.Scanner page
// over the committed event table in durable publication-sequence order. The
// scanner applies the transaction scope, so a read can never observe another
// tenant's or project's events. An optional type filter is applied after the
// scoped page; the reader keeps scanning bounded pages until it fills the
// requested page or exhausts the matching rows, so a filtered read never
// reports a false end of stream.
type EventReader struct {
	scanner store.Scanner
}

// NewEventReader builds the event read adapter. A missing scanner is a
// construction failure rather than an unbounded read.
func NewEventReader(scanner store.Scanner) (*EventReader, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "event reader requires a scanner")
	}
	return &EventReader{scanner: scanner}, nil
}

var _ event.ReadPort = (*EventReader)(nil)

// Read returns one committed-only, sequence-ordered page.
func (reader *EventReader) Read(ctx context.Context, query event.ReadQuery) (event.ReadPage, error) {
	if err := query.Validate(); err != nil {
		return event.ReadPage{}, err
	}
	wanted := make(map[event.Type]bool, len(query.Types))
	for _, eventType := range query.Types {
		wanted[eventType] = true
	}

	page := event.ReadPage{Events: make([]event.Envelope, 0, query.Limit)}
	cursor := query.AfterSequence
	for len(page.Events) < query.Limit {
		result, err := reader.scanner.Query(ctx, query.Scope, store.ScanQuery{
			Table:   eventsTable,
			Range:   []store.RangeFilter{{Column: publishSequenceColumn, After: int64(cursor)}},
			OrderBy: []store.Order{{Column: publishSequenceColumn}},
			Limit:   query.Limit,
		})
		if err != nil {
			return event.ReadPage{}, err
		}
		if len(result.Rows) == 0 {
			break
		}
		progressed := false
		for index, row := range result.Rows {
			envelope, sequence, err := decodeEventRow(row)
			if err != nil {
				return event.ReadPage{}, err
			}
			cursor = sequence
			progressed = true
			if len(wanted) > 0 && !wanted[envelope.Type] {
				continue
			}
			if len(page.Events) == query.Limit {
				break
			}
			page.Events = append(page.Events, envelope)
			page.Next = sequence
			if len(page.Events) == query.Limit {
				page.HasMore = result.HasMore || index < len(result.Rows)-1
				return page, nil
			}
		}
		if !progressed || !result.HasMore {
			break
		}
	}
	return page, nil
}

// decodeEventRow maps one events row to an envelope and its durable
// publication sequence. The row is validated against the envelope rules so
// durable corruption is reported rather than returned as an event.
func decodeEventRow(row store.Row) (event.Envelope, uint64, error) {
	eventID, err := textValue(row, idColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	eventType, err := textValue(row, typeColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	reportType, err := nullableTextValue(row, reportTypeColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	subjectID, err := textValue(row, subjectIDColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	subjectVersion, err := nullableUintValue(row, subjectVersionColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	sourceKind, err := textValue(row, sourceKindColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	sourcePrincipal, err := textValue(row, sourcePrincipalColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	correlationID, err := nullableTextValue(row, correlationIDColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	causationID, err := nullableTextValue(row, causationIDColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	occurredAt, err := timeValue(row, atColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	data, err := bytesValue(row, dataColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	tenant, err := textValue(row, tenantIDColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	project, err := nullableTextValue(row, projectIDColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	sequence, err := intValue(row, publishSequenceColumn)
	if err != nil {
		return event.Envelope{}, 0, err
	}
	if sequence <= 0 {
		return event.Envelope{}, 0, fault.New(fault.InternalFailure, "event row has no positive publication sequence")
	}
	envelope := event.Envelope{
		ID:             id.EventID(eventID),
		Type:           event.Type(eventType),
		ReportType:     event.ReportType(reportType),
		Source:         event.Source{Kind: event.SourceKind(sourceKind), PrincipalID: id.PrincipalID(sourcePrincipal)},
		At:             occurredAt,
		Scope:          tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		SubjectID:      subjectID,
		SubjectVersion: subjectVersion,
		CorrelationID:  correlationID,
		CausationID:    causationID,
		Data:           data,
	}
	if err := envelope.Validate(); err != nil {
		return event.Envelope{}, 0, fault.Wrap(fault.InternalFailure, "committed event row is invalid", err)
	}
	return envelope, uint64(sequence), nil
}
