// Package event declares the durable event envelope, audit record and
// post-commit effect vocabulary.
//
// A versioned envelope carries a stable event ID, type, authenticated or
// verified source, occurrence time, tenant/project scope, applicable subject
// identity/version and bounded data. Scope and source derive from
// authentication or verified ingress, never from the request body. Domain
// mutations commit their event rows as publication intent together with state
// and audit; broker publication is a separate retrying reader that retains the
// same IDs.
package event

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Type is one stable event type.
type Type string

const (
	// TypeTaskCreated reports a newly created task.
	TypeTaskCreated Type = "task.created"
	// TypeTaskQuestion reports a conversation question and waiting handoff.
	TypeTaskQuestion Type = "task.question"
	// TypeTaskReply reports an answer and the fresh queued execution.
	TypeTaskReply Type = "task.reply"
	// TypeTaskCancelled reports an explicit task cancellation.
	TypeTaskCancelled Type = "task.cancelled"
	// TypeExecutionQueued reports an admitted queued execution.
	TypeExecutionQueued Type = "execution.queued"
	// TypeExecutionStarted reports an execution that left queued.
	TypeExecutionStarted Type = "execution.started"
	// TypeExecutionSucceeded reports a terminal success.
	TypeExecutionSucceeded Type = "execution.succeeded"
	// TypeExecutionFailed reports a terminal failure.
	TypeExecutionFailed Type = "execution.failed"
	// TypeExecutionCancelled reports a terminal cancellation.
	TypeExecutionCancelled Type = "execution.cancelled"
	// TypeActionTransition reports one committed action transition.
	TypeActionTransition Type = "action.transition"
	// TypeRequestActivated reports an activated request action.
	TypeRequestActivated Type = "request.activated"
	// TypeActionRequested reports the durable request action and its immutable
	// assignment/deadline publication intent (S04/S06 activation sequence).
	TypeActionRequested Type = "action.requested"
	// TypeRequestResolved reports a settled request action.
	TypeRequestResolved Type = "request.resolved"
	// TypeConfigChanged reports a configuration mutation.
	TypeConfigChanged Type = "config.changed"
	// TypeServiceChanged reports a service-account or credential change.
	TypeServiceChanged Type = "service.changed"
	// TypeVerifiedReport reports an accepted verified resource effect.
	TypeVerifiedReport Type = "resource.verified"
	// TypeCustomObservation reports an authenticated custom observation. It
	// can never impersonate a lifecycle or decision fact.
	TypeCustomObservation Type = "observation.custom"
	// TypeCleanup reports execution cleanup progress.
	TypeCleanup Type = "execution.cleanup"
	// TypePipelineStartRefused reports a permanent refusal of an automatic
	// push start. It is a Shell decision fact, never an execution observation.
	TypePipelineStartRefused Type = "pipeline.start_refused"
)

var types = [...]Type{
	TypeTaskCreated,
	TypeTaskQuestion,
	TypeTaskReply,
	TypeTaskCancelled,
	TypeExecutionQueued,
	TypeExecutionStarted,
	TypeExecutionSucceeded,
	TypeExecutionFailed,
	TypeExecutionCancelled,
	TypeActionTransition,
	TypeRequestActivated,
	TypeActionRequested,
	TypeRequestResolved,
	TypeConfigChanged,
	TypeServiceChanged,
	TypeVerifiedReport,
	TypeCustomObservation,
	TypeCleanup,
	TypePipelineStartRefused,
}

// Types returns every event type in declaration order.
func Types() []Type {
	return append([]Type(nil), types[:]...)
}

// Valid reports whether the type is one of the declared types.
func (eventType Type) Valid() bool {
	for _, candidate := range types {
		if eventType == candidate {
			return true
		}
	}
	return false
}

// MaxReportTypeLength bounds one binding-declared reportable event type.
const MaxReportTypeLength = 128

// ReportType is one binding-declared reportable event type, for example
// "git.ref.updated". It is deliberately not part of the closed lifecycle Type
// vocabulary: a verified resource report records the declared type in
// Envelope.ReportType alongside the trusted lifecycle type resource.verified,
// so custom observations can neither claim it as a lifecycle type nor forge the
// dedicated field.
type ReportType string

// Valid reports whether the declared report type is syntactically usable.
func (reportType ReportType) Valid() bool {
	value := string(reportType)
	if value == "" || len(value) > MaxReportTypeLength {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '.', character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// SourceKind names who produced an event.
type SourceKind string

const (
	// SourceShell is a Shell domain change.
	SourceShell SourceKind = "shell"
	// SourceService is a verified service ingress.
	SourceService SourceKind = "service"
	// SourceExecution is an authenticated execution observation.
	SourceExecution SourceKind = "execution"
)

var sourceKinds = [...]SourceKind{SourceShell, SourceService, SourceExecution}

// SourceKinds returns every source kind in declaration order.
func SourceKinds() []SourceKind {
	return append([]SourceKind(nil), sourceKinds[:]...)
}

// Valid reports whether the source kind is one of the declared kinds.
func (kind SourceKind) Valid() bool {
	for _, candidate := range sourceKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Source is the authenticated or verified producer of an event.
type Source struct {
	Kind        SourceKind     `json:"kind"`
	PrincipalID id.PrincipalID `json:"principal_id"`
}

// Bounds is the event payload bound set.
type Bounds struct {
	MaxDataBytes int `json:"max_data_bytes"`
}

// DefaultBounds is the declared event bound set: 64 KiB of data per event.
var DefaultBounds = Bounds{MaxDataBytes: 64 << 10}

// Envelope is one durable event row and its publication intent.
type Envelope struct {
	ID             id.EventID    `json:"id"`
	Type           Type          `json:"type"`
	ReportType     ReportType    `json:"report_type,omitempty"`
	Source         Source        `json:"source"`
	At             time.Time     `json:"at"`
	Scope          tenancy.Scope `json:"scope"`
	SubjectID      string        `json:"subject_id,omitempty"`
	SubjectVersion uint64        `json:"subject_version,omitempty"`
	CorrelationID  string        `json:"correlation_id,omitempty"`
	CausationID    string        `json:"causation_id,omitempty"`
	Data           []byte        `json:"data,omitempty"`
}

// Validate checks the envelope identity, type, report type, source, time, scope
// and payload bound. Unknown lifecycle types, a missing/invalid report type on a
// verified report, a report type on any other event and oversized data use the
// declared invalid_input class.
func (envelope Envelope) Validate() error {
	if _, err := id.ParseEventID(string(envelope.ID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "event id is invalid", err)
	}
	if !envelope.Type.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event type %q", envelope.Type))
	}
	if envelope.Type == TypeVerifiedReport {
		if !envelope.ReportType.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("verified report event type %q is invalid", envelope.ReportType))
		}
	} else if envelope.ReportType != "" {
		return fault.New(fault.InvalidInput, fmt.Sprintf("report type %q is only valid on a verified resource report", envelope.ReportType))
	}
	if !envelope.Source.Kind.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event source kind %q", envelope.Source.Kind))
	}
	if envelope.Source.PrincipalID == "" {
		return fault.New(fault.InvalidInput, "event source principal is empty")
	}
	if envelope.At.IsZero() {
		return fault.New(fault.InvalidInput, "event time is zero")
	}
	if err := envelope.Scope.Validate(); err != nil {
		return err
	}
	if envelope.SubjectID == "" {
		return fault.New(fault.InvalidInput, "event subject id is empty")
	}
	if len(envelope.Data) > DefaultBounds.MaxDataBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("event data is %d bytes; at most %d are allowed", len(envelope.Data), DefaultBounds.MaxDataBytes))
	}
	return nil
}

// Audit is one immutable audit record committed with a domain change. It never
// carries credential material or secret plaintext.
type Audit struct {
	ID        id.EventID     `json:"id"`
	Scope     tenancy.Scope  `json:"scope"`
	Actor     id.PrincipalID `json:"actor"`
	Operation string         `json:"operation"`
	At        time.Time      `json:"at"`
	Detail    string         `json:"detail,omitempty"`
}

// PostCommitKind names one effect performed after the transaction commits.
type PostCommitKind string

const (
	// PostPublishEvent publishes already-committed event rows.
	PostPublishEvent PostCommitKind = "publish_event"
	// PostDeliverCredential delivers a freshly issued token to its mount.
	PostDeliverCredential PostCommitKind = "deliver_credential"
	// PostRefreshCredential replaces a mounted token after rotation.
	PostRefreshCredential PostCommitKind = "refresh_credential"
	// PostDispatch asks the runtime to dispatch a queued execution.
	PostDispatch PostCommitKind = "dispatch"
)

var postCommitKinds = [...]PostCommitKind{
	PostPublishEvent,
	PostDeliverCredential,
	PostRefreshCredential,
	PostDispatch,
}

// Valid reports whether the kind is one of the declared kinds.
func (kind PostCommitKind) Valid() bool {
	for _, candidate := range postCommitKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// PostCommitEffect is one retryable effect requested by a committed unit. It
// is intent, not evidence: an unacknowledged effect is never treated as
// executed or as not executed.
type PostCommitEffect struct {
	Kind   PostCommitKind `json:"kind"`
	Target string         `json:"target,omitempty"`
}

// Read bounds for authorized event reads.
const (
	// MaxReadLimit is the largest accepted event-read page size.
	MaxReadLimit = 200
	// MaxSubscribeSubjects bounds one subscription grant's subject list.
	MaxSubscribeSubjects = 16
)

// ReadQuery is one scoped, cursor-based event read. AfterSequence is the
// exclusive durable sequence cursor; zero reads from the beginning. Types
// optionally narrows the result to the listed lifecycle types and an empty
// filter means every type. Limit must be within 1..MaxReadLimit.
type ReadQuery struct {
	Scope         tenancy.Scope `json:"scope"`
	AfterSequence uint64        `json:"after_sequence,omitempty"`
	Types         []Type        `json:"types,omitempty"`
	Limit         int           `json:"limit"`
}

// Validate checks the scope, the bounded limit and every requested type. A
// read is scope-qualified and never a durable permission.
func (query ReadQuery) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if query.Limit <= 0 || query.Limit > MaxReadLimit {
		return fault.New(fault.InvalidInput, fmt.Sprintf("event read limit is outside 1..%d", MaxReadLimit))
	}
	for _, eventType := range query.Types {
		if !eventType.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event read type filter %q", eventType))
		}
	}
	return nil
}

// ReadPage is one committed-only, sequence-ordered event page. Next is the
// cursor to pass as the next query's AfterSequence and HasMore reports whether
// another committed page exists.
type ReadPage struct {
	Events  []Envelope `json:"events"`
	Next    uint64     `json:"next"`
	HasMore bool       `json:"has_more"`
}

// SubscribeSetup is the authorized event.subscribe setup request. Subjects are
// derived from Scope; Types optionally narrows the subscription and Cursor is
// the event-table resume point.
type SubscribeSetup struct {
	Scope  tenancy.Scope `json:"scope"`
	Types  []Type        `json:"types,omitempty"`
	Cursor uint64        `json:"cursor,omitempty"`
}

// Validate checks the scope and every requested type filter.
func (setup SubscribeSetup) Validate() error {
	if err := setup.Scope.Validate(); err != nil {
		return err
	}
	for _, eventType := range setup.Types {
		if !eventType.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("unknown event subscribe type filter %q", eventType))
		}
	}
	return nil
}

// SubscribeGrant is the validated result of an event.subscribe setup. It names
// only scope-qualified event subjects (never a wildcard or a foreign/mixed
// scope), the event-table resume cursor and the credential expiry. It carries
// no credential or token material; live connections still revalidate the
// current principal and grant.
type SubscribeGrant struct {
	Subjects  []string  `json:"subjects"`
	Cursor    uint64    `json:"cursor"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate checks the bounded subject count, every scope-qualified subject,
// that all subjects name the same scope, and the required expiry. A wildcard,
// malformed or mixed-scope subject is refused.
func (grant SubscribeGrant) Validate() error {
	_, err := grant.scope()
	return err
}

// ValidateFor additionally refuses a grant whose subjects name any scope other
// than the setup scope, so a grant can never widen a subscription across
// tenants or projects.
func (grant SubscribeGrant) ValidateFor(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	granted, err := grant.scope()
	if err != nil {
		return err
	}
	if granted != scope {
		return fault.New(fault.InvalidInput, "event subscribe grant names a foreign scope")
	}
	return nil
}

func (grant SubscribeGrant) scope() (tenancy.Scope, error) {
	if len(grant.Subjects) == 0 {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, "event subscribe grant has no subjects")
	}
	if len(grant.Subjects) > MaxSubscribeSubjects {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, fmt.Sprintf("event subscribe grant has %d subjects; at most %d are allowed", len(grant.Subjects), MaxSubscribeSubjects))
	}
	if grant.ExpiresAt.IsZero() {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, "event subscribe grant expiry is zero")
	}
	var granted tenancy.Scope
	for index, subject := range grant.Subjects {
		scope, err := parseScopeQualifiedEventSubject(subject)
		if err != nil {
			return tenancy.Scope{}, err
		}
		if index == 0 {
			granted = scope
			continue
		}
		if scope != granted {
			return tenancy.Scope{}, fault.New(fault.InvalidInput, "event subscribe grant mixes scopes")
		}
	}
	return granted, nil
}

// parseScopeQualifiedEventSubject mirrors the declared event subject shape:
// kodama.v1.<tenant>.<project|_>.events. A wildcard or an empty/extra token is
// refused, and "_" is the tenant-wide project placeholder.
func parseScopeQualifiedEventSubject(subject string) (tenancy.Scope, error) {
	if subject == "" {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, "event subscribe subject is empty")
	}
	if strings.ContainsAny(subject, "*>") {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, fmt.Sprintf("event subscribe subject %q is a wildcard", subject))
	}
	tokens := strings.Split(subject, ".")
	if len(tokens) != 5 || tokens[0] != "kodama" || tokens[1] != "v1" || tokens[4] != "events" {
		return tenancy.Scope{}, fault.New(fault.InvalidInput, fmt.Sprintf("event subscribe subject %q is not scope-qualified", subject))
	}
	tenantID, err := id.ParseTenantID(tokens[2])
	if err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.InvalidInput, "event subscribe subject tenant token is invalid", err)
	}
	scope := tenancy.Scope{TenantID: tenantID}
	if tokens[3] != "_" {
		projectID, err := id.ParseProjectID(tokens[3])
		if err != nil {
			return tenancy.Scope{}, fault.Wrap(fault.InvalidInput, "event subscribe subject project token is invalid", err)
		}
		scope.ProjectID = projectID
	}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	return scope, nil
}
