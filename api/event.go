package api

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// EventTypes returns every canonical public lifecycle event type in the Shell
// declaration order. The Shell retains `request.activated` as an internal
// alias; the public name is `action.requested`, which CanonicalEventType maps
// it to.
func EventTypes() []string {
	return []string{
		"task.created",
		"task.question",
		"task.reply",
		"task.cancelled",
		"execution.queued",
		"execution.started",
		"execution.succeeded",
		"execution.failed",
		"execution.cancelled",
		"action.transition",
		"action.requested",
		"request.resolved",
		"config.changed",
		"service.changed",
		"resource.verified",
		"observation.custom",
		"execution.cleanup",
		"pipeline.start_refused",
	}
}

// lifecycleAliases maps Shell-internal lifecycle names to their canonical
// public names. The alias is never emitted or accepted as a public event type,
// but it remains a lifecycle name for report-type separation.
var lifecycleAliases = map[string]string{
	"request.activated": "action.requested",
}

// CanonicalEventType maps one Shell lifecycle name to its canonical public
// name.
// It reports false for an unknown name.
func CanonicalEventType(value string) (string, bool) {
	if isCanonicalLifecycleType(value) {
		return value, true
	}
	if canonical, ok := lifecycleAliases[value]; ok {
		return canonical, true
	}
	return "", false
}

// EventSources returns every event source kind in the Shell declaration order.
func EventSources() []string {
	return []string{"shell", "service", "execution"}
}

// lifecycleType is the trusted verified-report envelope type.
const lifecycleType = "resource.verified"

// isCanonicalLifecycleType reports whether value is one of the canonical public
// lifecycle types.
func isCanonicalLifecycleType(value string) bool {
	for _, candidate := range EventTypes() {
		if candidate == value {
			return true
		}
	}
	return false
}

// isLifecycleType reports whether value is a canonical lifecycle type or a
// Shell-internal lifecycle alias. It is used for report-type separation so an
// alias can never be declared as a report type.
func isLifecycleType(value string) bool {
	if isCanonicalLifecycleType(value) {
		return true
	}
	_, ok := lifecycleAliases[value]
	return ok
}

// isEventSource reports whether value is one of the declared source kinds.
func isEventSource(value string) bool {
	for _, candidate := range EventSources() {
		if candidate == value {
			return true
		}
	}
	return false
}

// ReportTypeValid reports whether a binding-declared reportable event type is
// syntactically usable. It is a grammar check only: a lifecycle type still
// passes the grammar and is refused by the report schemas.
func ReportTypeValid(value string) bool {
	if value == "" || len(value) > maxReportTypeLength {
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

// validReportType reports whether value is a usable, non-lifecycle report type.
func validReportType(value string) bool {
	return ReportTypeValid(value) && !isLifecycleType(value)
}

// EventSource is the authenticated or verified producer of an event.
type EventSource struct {
	Kind        string `json:"kind"`
	PrincipalID string `json:"principal_id"`
}

// EventScope is the tenant/project scope of an event.
type EventScope struct {
	TenantID  string `json:"tenant_id"`
	ProjectID string `json:"project_id,omitempty"`
}

// Validate checks the tenant and, when present, the project identity.
func (scope EventScope) Validate() error {
	if !validID(scope.TenantID) {
		return invalid("event scope tenant id is invalid")
	}
	if scope.ProjectID != "" && !validID(scope.ProjectID) {
		return invalid("event scope project id is invalid")
	}
	return nil
}

// MaxSubscribeSubjects bounds one event.subscribe grant's subject list.
const MaxSubscribeSubjects = 16

// observationType is the only event type a custom observation may carry.
const observationType = "observation.custom"

// EventPublishRequest is the event.publish input: one custom observation. The
// body carries only the facts the client may choose; the event identity,
// occurrence time, source, scope and receipt identity are server-derived. The
// type must be the observation.custom name, so a custom observation can
// never impersonate a lifecycle or verified-report fact, and no report_type is
// accepted.
type EventPublishRequest struct {
	ProjectID      string `json:"project_id,omitempty" schema:"id"`
	Type           string `json:"type"`
	SubjectID      string `json:"subject_id"`
	SubjectVersion uint64 `json:"subject_version,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
	CausationID    string `json:"causation_id,omitempty"`
	Data           []byte `json:"data,omitempty"`
}

// Validate checks the optional project, the observation type, the opaque
// subject/correlation bounds and the payload bound.
func (request EventPublishRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	canonical, ok := CanonicalEventType(request.Type)
	if !ok {
		return invalid("unknown event type %q", request.Type)
	}
	if canonical != observationType {
		return invalid("event type %q is not a custom observation", request.Type)
	}
	if request.SubjectID == "" {
		return invalid("event subject id is empty")
	}
	if len(request.SubjectID) > MaxSubjectValueBytes {
		return invalid("event subject id is %d bytes; at most %d are allowed", len(request.SubjectID), MaxSubjectValueBytes)
	}
	for name, value := range map[string]string{
		"correlation_id": request.CorrelationID,
		"causation_id":   request.CausationID,
	} {
		if len(value) > MaxSubjectValueBytes {
			return invalid("event %s is %d bytes; at most %d are allowed", name, len(value), MaxSubjectValueBytes)
		}
	}
	if len(request.Data) > MaxEventDataBytes {
		return invalid("event data is %d bytes; at most %d are allowed", len(request.Data), MaxEventDataBytes)
	}
	return nil
}

// EventPublishResult is the committed event.publish result. It reports only the
// committed event and receipt identities and whether an equal retry replayed
// the committed observation; it never reports a decision or state transition.
type EventPublishResult struct {
	EventID   string `json:"event_id" schema:"id"`
	ReceiptID string `json:"receipt_id" schema:"id"`
	Replayed  bool   `json:"replayed"`
}

// Validate checks the committed event and receipt identities.
func (result EventPublishResult) Validate() error {
	if !validID(result.EventID) {
		return invalid("event_id is invalid")
	}
	if !validID(result.ReceiptID) {
		return invalid("receipt_id is invalid")
	}
	return nil
}

// EventReadRequest is the event.read input. Page carries the opaque public
// cursor (its decoded sort key is the canonical decimal service sequence) and
// the bounded page limit, while Types optionally narrows the read. The project
// is carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type EventReadRequest struct {
	ProjectID string   `json:"project_id,omitempty" schema:"id"`
	Page      *Page    `json:"page,omitempty"`
	Types     []string `json:"types,omitempty"`
}

// Validate checks the optional project, the bounded page, the opaque page
// cursor and every lifecycle type filter.
func (request EventReadRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Page != nil {
		if err := request.Page.Validate(); err != nil {
			return err
		}
		if request.Page.Cursor != "" {
			if _, err := eventSequenceCursor(request.Page.Cursor); err != nil {
				return err
			}
		}
	}
	return validateEventTypeFilter(request.Types)
}

// EventReadResult is one committed-only event page. Next is the opaque cursor
// of the last committed sequence (always present, even when HasMore is false)
// and HasMore reports whether another committed page exists.
type EventReadResult struct {
	Events  []Event `json:"events"`
	Next    string  `json:"next"`
	HasMore bool    `json:"has_more"`
}

// Validate checks every returned envelope and the continuation cursor.
func (result EventReadResult) Validate() error {
	for index, envelope := range result.Events {
		if err := envelope.Validate(); err != nil {
			return invalid("event read result event %d is invalid: %v", index, err)
		}
	}
	if _, err := eventSequenceCursor(result.Next); err != nil {
		return err
	}
	return nil
}

// EventSubscribeRequest is the event.subscribe input. Types optionally narrows
// the subscription and Cursor is the opaque resume point. The project is
// carried explicitly for MCP callers and is optional for REST callers whose
// route supplies it.
type EventSubscribeRequest struct {
	ProjectID string   `json:"project_id,omitempty" schema:"id"`
	Types     []string `json:"types,omitempty"`
	Cursor    string   `json:"cursor,omitempty"`
}

// Validate checks the optional project, every lifecycle type filter and the
// opaque resume cursor.
func (request EventSubscribeRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Cursor != "" {
		if _, err := eventSequenceCursor(request.Cursor); err != nil {
			return err
		}
	}
	return validateEventTypeFilter(request.Types)
}

// EventSubscribeResult is the validated event.subscribe setup result. It names
// only scope-qualified event subjects, the opaque resume cursor and the
// credential expiry; it carries no credential or token material.
type EventSubscribeResult struct {
	Subjects  []string  `json:"subjects"`
	Cursor    string    `json:"cursor"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Validate checks the bounded subject count, every scope-qualified subject,
// that all subjects name the same scope, the opaque cursor and the required
// expiry. A wildcard, malformed or mixed-scope subject is refused.
func (result EventSubscribeResult) Validate() error {
	if len(result.Subjects) == 0 {
		return invalid("event subscribe result has no subjects")
	}
	if len(result.Subjects) > MaxSubscribeSubjects {
		return invalid("event subscribe result has %d subjects; at most %d are allowed", len(result.Subjects), MaxSubscribeSubjects)
	}
	if result.ExpiresAt.IsZero() {
		return invalid("event subscribe result expiry is zero")
	}
	if _, err := eventSequenceCursor(result.Cursor); err != nil {
		return err
	}
	var grantedTenant, grantedProject string
	for index, subject := range result.Subjects {
		tenant, project, err := parseEventSubject(subject)
		if err != nil {
			return err
		}
		if index == 0 {
			grantedTenant, grantedProject = tenant, project
			continue
		}
		if tenant != grantedTenant || project != grantedProject {
			return invalid("event subscribe result mixes scopes")
		}
	}
	return nil
}

// eventSequenceCursor decodes one opaque public event cursor and returns the
// service sequence number. The cursor sort key is the canonical decimal
// encoding of the exclusive service uint64 cursor, so a raw decimal, a
// non-decimal or a non-canonical payload refuses.
func eventSequenceCursor(cursor string) (uint64, error) {
	if cursor == "" {
		return 0, invalid("event cursor is empty")
	}
	sortKey, err := DecodeCursor(cursor)
	if err != nil {
		return 0, err
	}
	sequence, err := strconv.ParseUint(sortKey, 10, 64)
	if err != nil {
		return 0, invalid("event cursor does not carry a decimal sequence")
	}
	if strconv.FormatUint(sequence, 10) != sortKey {
		return 0, invalid("event cursor is not a canonical decimal sequence")
	}
	return sequence, nil
}

// validateEventTypeFilter checks every requested lifecycle type filter. The
// Shell-internal request.activated alias is never accepted as a public filter.
func validateEventTypeFilter(types []string) error {
	for _, value := range types {
		if !isCanonicalLifecycleType(value) {
			return invalid("unknown event type filter %q", value)
		}
	}
	return nil
}

// parseEventSubject mirrors the Shell's scope-qualified subject shape
// kodama.v1.<tenant>.<project|_>.events and returns its scope tokens.
func parseEventSubject(subject string) (tenant, project string, err error) {
	if subject == "" {
		return "", "", invalid("event subscribe subject is empty")
	}
	if strings.ContainsAny(subject, "*>") {
		return "", "", invalid("event subscribe subject %q is a wildcard", subject)
	}
	tokens := strings.Split(subject, ".")
	if len(tokens) != 5 || tokens[0] != "kodama" || tokens[1] != "v1" || tokens[4] != "events" {
		return "", "", invalid("event subscribe subject %q is not scope-qualified", subject)
	}
	if !validID(tokens[2]) {
		return "", "", invalid("event subscribe subject tenant token is invalid")
	}
	if tokens[3] != "_" && !validID(tokens[3]) {
		return "", "", invalid("event subscribe subject project token is invalid")
	}
	return tokens[2], tokens[3], nil
}

// Event is the public event envelope. A verified resource report carries the
// trusted lifecycle type resource.verified and the binding-declared report type
// in ReportType; every other type must leave ReportType empty.
type Event struct {
	ID             string      `json:"id"`
	Type           string      `json:"type"`
	ReportType     string      `json:"report_type,omitempty"`
	Source         EventSource `json:"source"`
	At             time.Time   `json:"at"`
	Scope          EventScope  `json:"scope"`
	SubjectID      string      `json:"subject_id,omitempty"`
	SubjectVersion uint64      `json:"subject_version,omitempty"`
	CorrelationID  string      `json:"correlation_id,omitempty"`
	CausationID    string      `json:"causation_id,omitempty"`
	Data           []byte      `json:"data,omitempty"`
}

// Validate checks the envelope identity, lifecycle type, report-type
// separation, source, time, scope, subject and payload bound.
func (event Event) Validate() error {
	if !validID(event.ID) {
		return invalid("event id is invalid")
	}
	if !isCanonicalLifecycleType(event.Type) {
		return invalid("unknown public event type %q", event.Type)
	}
	if event.Type == lifecycleType {
		if !validReportType(event.ReportType) {
			return invalid("verified report event type %q is invalid", event.ReportType)
		}
	} else if event.ReportType != "" {
		return invalid("report type %q is only valid on a verified resource report", event.ReportType)
	}
	if !isEventSource(event.Source.Kind) {
		return invalid("unknown event source kind %q", event.Source.Kind)
	}
	if event.Source.PrincipalID == "" {
		return invalid("event source principal is empty")
	}
	if event.At.IsZero() {
		return invalid("event time is zero")
	}
	if err := event.Scope.Validate(); err != nil {
		return err
	}
	if event.SubjectID == "" {
		return invalid("event subject id is empty")
	}
	if len(event.SubjectID) > MaxSubjectValueBytes {
		return invalid("event subject id is %d bytes; at most %d are allowed", len(event.SubjectID), MaxSubjectValueBytes)
	}
	if len(event.Data) > MaxEventDataBytes {
		return invalid("event data is %d bytes; at most %d are allowed", len(event.Data), MaxEventDataBytes)
	}
	return nil
}

// SubjectField is one immutable typed verification subject field.
type SubjectField struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Subject is the immutable ordered field set of one request.
type Subject struct {
	Fields []SubjectField `json:"fields"`
}

// subjectValueKinds is the typed-value vocabulary.
var subjectValueKinds = []string{"string", "number", "boolean", "null"}

// validSubjectKind reports whether kind is one of the declared value kinds.
func validSubjectKind(kind string) bool {
	for _, candidate := range subjectValueKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// Validate checks field count, unique names, value kinds and typed values. The
// empty subject is valid.
func (subject Subject) Validate() error {
	if len(subject.Fields) > MaxSubjectFields {
		return invalid("subject has %d fields; at most %d are allowed", len(subject.Fields), MaxSubjectFields)
	}
	seen := make(map[string]bool, len(subject.Fields))
	for _, field := range subject.Fields {
		if field.Name == "" {
			return invalid("subject field name is empty")
		}
		if len(field.Name) > MaxSubjectValueBytes {
			return invalid("subject field name %q is too long", field.Name)
		}
		if seen[field.Name] {
			return invalid("subject field name %q is repeated", field.Name)
		}
		seen[field.Name] = true
		if !validSubjectKind(field.Kind) {
			return invalid("subject field %q has unknown kind %q", field.Name, field.Kind)
		}
		if len(field.Value) > MaxSubjectValueBytes {
			return invalid("subject field %q value is too long", field.Name)
		}
		switch field.Kind {
		case "number":
			var number json.Number
			if err := json.Unmarshal([]byte(field.Value), &number); err != nil {
				return invalid("subject field %q is not a JSON number", field.Name)
			}
		case "boolean":
			if field.Value != "true" && field.Value != "false" {
				return invalid("subject field %q is not a JSON boolean", field.Name)
			}
		case "null":
			if field.Value != "" {
				return invalid("subject null field %q must have an empty value", field.Name)
			}
		}
	}
	return nil
}

// ResourceOperation is one verifiable effect an enforcing service performs
// against its own resource. Resource operations are a separate vocabulary from
// the public router registry.
type ResourceOperation string

// ResourceOperationGitRefUpdate is the Git service's ref-update effect.
const ResourceOperationGitRefUpdate ResourceOperation = "git.ref.update"

// ResourceOperationGitRead is the Git service's repository-level read check:
// verification supplies the principal and scoped roles with an empty subject
// and no approval, and the repository's ordered rules decide the read.
const ResourceOperationGitRead ResourceOperation = "git.read"

// resourceOperations is the resource-operation declaration order.
var resourceOperations = []ResourceOperation{ResourceOperationGitRefUpdate, ResourceOperationGitRead}

// ResourceOperations returns every resource operation in declaration order.
func ResourceOperations() []ResourceOperation {
	return append([]ResourceOperation(nil), resourceOperations...)
}

// Valid reports whether the operation is a known resource operation.
func (operation ResourceOperation) Valid() bool {
	for _, candidate := range resourceOperations {
		if candidate == operation {
			return true
		}
	}
	return false
}

// humanRoles is the human role vocabulary used by approval strings.
var humanRoles = []string{"admin", "engineer", "reviewer", "operator", "viewer"}

// validHumanRole reports whether role is a declared human role.
func validHumanRole(role string) bool {
	for _, candidate := range humanRoles {
		if candidate == role {
			return true
		}
	}
	return false
}

// validApproval checks an optional `role/<role>` approval request.
func validApproval(approval string) bool {
	if approval == "" {
		return true
	}
	const prefix = "role/"
	if !strings.HasPrefix(approval, prefix) {
		return false
	}
	role := strings.TrimPrefix(approval, prefix)
	return validHumanRole(role) && !strings.Contains(role, "/")
}
