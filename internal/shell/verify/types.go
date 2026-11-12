// Package verify declares actor verification, typed subject matching and the
// verified resource report commit unit.
//
// Verification authenticates the calling service and, independently, the actor
// credential. Service permission never substitutes for actor authority. A
// request's immutable subject is compared field by field with typed equality:
// every required field must match by name, kind and value; recorded fields the
// enforcing service did not supply as required are display context and are
// ignored; a required field is never ignorable. A verification record captures
// the service, actor, execution/action, resource, operation/subject, requested
// approval, result and time, and never the forwarded credential.
package verify

import (
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// VerificationID identifies one verification record.
type VerificationID string

// ParseVerificationID validates and converts a verification identity.
func ParseVerificationID(value string) (VerificationID, error) {
	if !validVerificationID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid verification id %q", value))
	}
	return VerificationID(value), nil
}

func validVerificationID(value string) bool {
	if value == "" || len(value) > id.MaxIDLength {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// ValueKind is the JSON value kind of one subject field.
type ValueKind string

const (
	// KindString is a JSON string.
	KindString ValueKind = "string"
	// KindNumber is a JSON number.
	KindNumber ValueKind = "number"
	// KindBoolean is a JSON boolean.
	KindBoolean ValueKind = "boolean"
	// KindNull is JSON null.
	KindNull ValueKind = "null"
)

var valueKinds = [...]ValueKind{KindString, KindNumber, KindBoolean, KindNull}

// Valid reports whether the kind is one of the declared kinds.
func (kind ValueKind) Valid() bool {
	for _, candidate := range valueKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// SubjectField is one immutable subject field. Display context is not a flag:
// recorded fields the enforcing service did not supply as required are display
// context, and matching ignores them.
type SubjectField struct {
	Name  string    `json:"name"`
	Kind  ValueKind `json:"kind"`
	Value string    `json:"value"`
}

// Subject is the immutable ordered field set of one request.
type Subject struct {
	Fields []SubjectField `json:"fields"`
}

// Field returns the first field with the supplied name.
func (subject Subject) Field(name string) (SubjectField, bool) {
	for _, field := range subject.Fields {
		if field.Name == name {
			return field, true
		}
	}
	return SubjectField{}, false
}

// SubjectMatches reports whether the recorded subject satisfies every required
// field with typed equality. Every required field must exist in the recorded
// subject with the same name, value kind and value; a required field is never
// ignorable. Recorded fields the enforcing service did not supply as required
// are immutable display context and are ignored. A missing required field, a
// kind or value mismatch, an ambiguous duplicate name or an unknown kind
// refuses. An empty required subject matches only an empty recorded subject; it
// is never a wildcard.
func SubjectMatches(required, recorded Subject) bool {
	requiredFields, ok := fieldMap(required)
	if !ok {
		return false
	}
	if len(requiredFields) == 0 {
		return len(recorded.Fields) == 0
	}
	recordedFields, ok := fieldMap(recorded)
	if !ok {
		return false
	}
	for name, want := range requiredFields {
		got, present := recordedFields[name]
		if !present {
			return false
		}
		if got.Kind != want.Kind || got.Value != want.Value {
			return false
		}
	}
	return true
}

// fieldMap builds the validated field map and refuses empty names, unknown kinds
// and ambiguous duplicate names.
func fieldMap(subject Subject) (map[string]SubjectField, bool) {
	fields := make(map[string]SubjectField, len(subject.Fields))
	for _, field := range subject.Fields {
		if field.Name == "" || !field.Kind.Valid() {
			return nil, false
		}
		if _, duplicate := fields[field.Name]; duplicate {
			return nil, false
		}
		fields[field.Name] = field
	}
	return fields, true
}

// Result is the outcome of one verification attempt.
type Result string

const (
	// ResultApproved means the required approval evidence matched.
	ResultApproved Result = "approved"
	// ResultRefused means the required evidence did not match.
	ResultRefused Result = "refused"
)

var results = [...]Result{ResultApproved, ResultRefused}

// Valid reports whether the result is one of the declared results.
func (result Result) Valid() bool {
	for _, candidate := range results {
		if result == candidate {
			return true
		}
	}
	return false
}

// Record is the durable verification record. It never carries the forwarded
// actor credential or a token.
type Record struct {
	ID                VerificationID      `json:"id"`
	Scope             tenancy.Scope       `json:"scope"`
	CallingService    service.ServiceID   `json:"calling_service"`
	BindingID         resource.BindingID  `json:"binding_id"`
	Actor             identity.Principal  `json:"actor"`
	ExecutionID       id.ExecutionID      `json:"execution_id,omitempty"`
	ActionID          id.ActionID         `json:"action_id,omitempty"`
	ResourceID        resource.ResourceID `json:"resource_id"`
	Operation         string              `json:"operation"`
	Subject           Subject             `json:"subject"`
	RequestedApproval request.Assignment  `json:"requested_approval,omitempty"`
	Result            Result              `json:"result"`
	Approved          bool                `json:"approved"`
	At                time.Time           `json:"at"`
}

// ReportUnit is the complete atomic verified-report commit unit.
//
// Actor and execution causation come from the verification record, never from
// the report body. EventType is the binding-declared reportable type (for
// example "git.ref.updated"), validated against the binding before acceptance;
// the accepted envelope carries it in Envelope.ReportType under the trusted
// lifecycle type resource.verified. (ReportingService, SourceEventID) is the
// deduplication identity; a changed replay is refused.
type ReportUnit struct {
	Owner            tenancy.Scope             `json:"owner"`
	ReportingService service.ServiceID         `json:"reporting_service"`
	BindingID        resource.BindingID        `json:"binding_id"`
	SourceEventID    string                    `json:"source_event_id"`
	EventType        event.ReportType          `json:"event_type"`
	VerificationID   VerificationID            `json:"verification_id"`
	ResourceID       resource.ResourceID       `json:"resource_id"`
	Operation        string                    `json:"operation"`
	Subject          Subject                   `json:"subject"`
	Receipt          receipt.Record            `json:"receipt"`
	Audit            []event.Audit             `json:"audit"`
	Events           []event.Envelope          `json:"events"`
	Authority        []authorize.AuthorityRead `json:"authority"`
	PostCommit       []event.PostCommitEffect  `json:"post_commit"`
}

// ValidateEventType checks the report's binding-declared event type. The type
// must be syntactically usable, the binding must list it in ReportableEvents,
// and the unit must carry exactly one accepted resource.verified envelope whose
// ReportType is the same declared type; an undeclared, missing or mismatched
// accepted type is refused with invalid_input.
func (unit ReportUnit) ValidateEventType(binding resource.Binding) error {
	if !unit.EventType.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("invalid reportable event type %q", unit.EventType))
	}
	if !binding.DeclaresReport(string(unit.EventType)) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("binding does not declare reportable event type %q", unit.EventType))
	}
	accepted := 0
	for _, envelope := range unit.Events {
		if envelope.Type != event.TypeVerifiedReport {
			continue
		}
		accepted++
		if envelope.ReportType != unit.EventType {
			return fault.New(fault.InvalidInput, fmt.Sprintf("accepted event report type %q does not match declared type %q", envelope.ReportType, unit.EventType))
		}
	}
	if accepted != 1 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("verified report unit carries %d resource.verified events; want exactly 1", accepted))
	}
	return nil
}

// ReportResult is the committed report result.
type ReportResult struct {
	VerificationID VerificationID `json:"verification_id"`
	EventID        id.EventID     `json:"event_id"`
	Replayed       bool           `json:"replayed"`
}

// BindingQuery asks for the current binding that authorizes one calling
// service on one registered resource. The adapter reads the binding and its
// current version inside the verification commit.
type BindingQuery struct {
	Scope      tenancy.Scope       `json:"scope"`
	ResourceID resource.ResourceID `json:"resource_id"`
	ServiceID  service.ServiceID   `json:"service_id"`
}

// Validate checks the scope, resource and calling service.
func (query BindingQuery) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if _, err := resource.ParseResourceID(string(query.ResourceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "binding query resource id is invalid", err)
	}
	if _, err := service.ParseServiceID(string(query.ServiceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "binding query service id is invalid", err)
	}
	return nil
}

// EvidenceQuery asks for prior succeeded request evidence in one execution.
// The adapter returns only durable facts; the verifier performs the exact typed
// match.
type EvidenceQuery struct {
	Scope       tenancy.Scope       `json:"scope"`
	ExecutionID id.ExecutionID      `json:"execution_id"`
	ActionID    id.ActionID         `json:"action_id,omitempty"`
	Assignment  request.Assignment  `json:"assignment"`
	Operation   string              `json:"operation"`
	ResourceID  resource.ResourceID `json:"resource_id"`
	Subject     Subject             `json:"subject"`
}

// Validate checks the scope, execution, assignment, operation and resource.
func (query EvidenceQuery) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseExecutionID(string(query.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "evidence query execution id is invalid", err)
	}
	if !query.Assignment.Valid() {
		return fault.New(fault.InvalidInput, "evidence query assignment is invalid")
	}
	if query.Operation == "" {
		return fault.New(fault.InvalidInput, "evidence query operation is empty")
	}
	if _, err := resource.ParseResourceID(string(query.ResourceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "evidence query resource id is invalid", err)
	}
	return nil
}

// Evidence is one durable prior-request fact used for approval matching. It
// carries the immutable subject and the authorized decision, never a
// credential or a durable allow.
type Evidence struct {
	ExecutionID id.ExecutionID      `json:"execution_id"`
	ActionID    id.ActionID         `json:"action_id"`
	Assignment  request.Assignment  `json:"assignment"`
	Operation   string              `json:"operation"`
	ResourceID  resource.ResourceID `json:"resource_id"`
	Subject     Subject             `json:"subject"`
	Decision    request.Decision    `json:"decision"`
	ResolvedAt  time.Time           `json:"resolved_at"`
}

// RecordUnit is the complete atomic verification-record creation commit unit.
// The verifier creates a durable verification fact through this unit instead of
// a private substitute. The forwarded actor credential is never part of it.
type RecordUnit struct {
	Owner      tenancy.Scope             `json:"owner"`
	Record     Record                    `json:"record"`
	Receipt    receipt.Record            `json:"receipt"`
	Audit      []event.Audit             `json:"audit"`
	Events     []event.Envelope          `json:"events"`
	Authority  []authorize.AuthorityRead `json:"authority"`
	PostCommit []event.PostCommitEffect  `json:"post_commit"`
}

// RecordResult is the committed verification-record result.
type RecordResult struct {
	ID       VerificationID `json:"id"`
	Replayed bool           `json:"replayed"`
}
