package event

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// EnvelopeInput is the untrusted part of one durable event. It deliberately
// carries no source, scope or time: the authenticated or verified producer, the
// ownership scope and the occurrence time are trusted constructor parameters,
// so a request body can never choose who produced an event or where it lives.
type EnvelopeInput struct {
	Type           Type       `json:"type"`
	ReportType     ReportType `json:"report_type,omitempty"`
	SubjectID      string     `json:"subject_id,omitempty"`
	SubjectVersion uint64     `json:"subject_version,omitempty"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
	CausationID    string     `json:"causation_id,omitempty"`
	Data           []byte     `json:"data,omitempty"`
}

// NewEnvelope builds one durable event envelope from trusted context and an
// untrusted input.
//
// The supplied event ID is retained unchanged so a publication retry
// republishes the same event. The trusted source, scope and time are applied
// as given. An input lifecycle alias is canonicalized through
// api.CanonicalEventType (request.activated becomes action.requested); an
// unknown name is refused. Report-type separation is enforced here: only
// resource.verified may carry a report type, it must pass api.ReportTypeValid,
// and it must not be a lifecycle type or alias. Every other type must leave
// ReportType empty. The payload is bounded by DefaultBounds.MaxDataBytes and
// the finished envelope must satisfy Envelope.Validate.
func NewEnvelope(eventID id.EventID, source Source, scope tenancy.Scope, at time.Time, input EnvelopeInput) (Envelope, error) {
	canonical, ok := api.CanonicalEventType(string(input.Type))
	if !ok {
		return Envelope{}, fault.New(fault.InvalidInput, fmt.Sprintf("unknown event type %q", input.Type))
	}

	reportType := input.ReportType
	if canonical == string(TypeVerifiedReport) {
		if !api.ReportTypeValid(string(reportType)) {
			return Envelope{}, fault.New(fault.InvalidInput, fmt.Sprintf("verified report event type %q is invalid", reportType))
		}
		if _, lifecycle := api.CanonicalEventType(string(reportType)); lifecycle {
			return Envelope{}, fault.New(fault.InvalidInput, fmt.Sprintf("report type %q is a lifecycle type", reportType))
		}
	} else if reportType != "" {
		return Envelope{}, fault.New(fault.InvalidInput, fmt.Sprintf("report type %q is only valid on a verified resource report", reportType))
	}

	envelope := Envelope{
		ID:             eventID,
		Type:           Type(canonical),
		ReportType:     reportType,
		Source:         source,
		At:             at,
		Scope:          scope,
		SubjectID:      input.SubjectID,
		SubjectVersion: input.SubjectVersion,
		CorrelationID:  input.CorrelationID,
		CausationID:    input.CausationID,
		Data:           append([]byte(nil), input.Data...),
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}
