package api

import (
	"bytes"
	"encoding/json"
	"time"
)

// Inbox query and observation wire vocabulary.
//
// The inbox is a scoped read, not a mutable request entity. A query names the
// item kinds and an optional page; the result carries typed observations.

// TaskInboxRequest is the task.inbox input. The project is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it. An
// empty kinds list asks for every inbox kind.
type TaskInboxRequest struct {
	ProjectID string   `json:"project_id,omitempty" schema:"id"`
	Kinds     []string `json:"kinds,omitempty"`
	Page      *Page    `json:"page,omitempty"`
}

// Validate checks the optional project identity, inbox kind set and page
// bounds. An omitted or empty kinds list is every kind.
func (request TaskInboxRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	for _, kind := range request.Kinds {
		if !validInboxKind(kind) {
			return invalid("unknown inbox kind %q", kind)
		}
	}
	if request.Page != nil {
		return request.Page.Validate()
	}
	return nil
}

// InboxItem is one scoped inbox observation. The identities are optional per
// kind: a request item requires action_id and execution_id, while the two task
// kinds require task_id and carry no execution or action. Assignment is the
// `role/<role>` or `service/<id>` grammar.
//
// A request item is self-sufficient: it carries the immutable typed subject,
// the declared response output names and the action deadline (At), so the exact
// assignee can decide and answer without a second read. It refuses the
// waiting-task question projection. A waiting_task item carries the complete
// outstanding question projection so the exact assignee can answer without a
// second read: question_id, the question body, durable references and the task
// lifecycle version. Version stays the QUESTION version (the reply's expected
// question version) while LifecycleVersion is the task lifecycle version (the
// reply envelope's expected_version). A failed_task item carries neither
// projection; its Version is already its lifecycle version.
type InboxItem struct {
	Kind             string    `json:"kind"`
	TaskID           string    `json:"task_id,omitempty" schema:"id"`
	ExecutionID      string    `json:"execution_id,omitempty" schema:"id"`
	ActionID         string    `json:"action_id,omitempty" schema:"id"`
	Assignment       string    `json:"assignment,omitempty"`
	Subject          *Subject  `json:"subject,omitempty"`
	Outputs          []string  `json:"outputs,omitempty"`
	QuestionID       string    `json:"question_id,omitempty" schema:"id"`
	Question         string    `json:"question,omitempty"`
	References       []string  `json:"references,omitempty"`
	Version          uint64    `json:"version" schema:"version"`
	LifecycleVersion uint64    `json:"lifecycle_version,omitempty" schema:"version"`
	At               time.Time `json:"at"`
}

// UnmarshalJSON decodes one inbox item strictly. The request subject is
// decoded with member presence so a field missing its name, kind or value is
// refused rather than silently defaulting to an empty string; every nested
// level still refuses unknown members.
func (item *InboxItem) UnmarshalJSON(data []byte) error {
	type alias InboxItem
	var wire struct {
		*alias
		Subject json.RawMessage `json:"subject"`
	}
	wire.alias = (*alias)(item)
	if err := decodeStrict(data, &wire); err != nil {
		return err
	}
	item.Subject = nil
	raw := bytes.TrimSpace(wire.Subject)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var subjectWire struct {
		Fields []struct {
			Name  *string `json:"name"`
			Kind  *string `json:"kind"`
			Value *string `json:"value"`
		} `json:"fields"`
	}
	if err := decodeStrict(raw, &subjectWire); err != nil {
		return err
	}
	subject := Subject{Fields: make([]SubjectField, 0, len(subjectWire.Fields))}
	for _, field := range subjectWire.Fields {
		if field.Name == nil || field.Kind == nil || field.Value == nil {
			return invalid("subject field is missing a required member")
		}
		subject.Fields = append(subject.Fields, SubjectField{Name: *field.Name, Kind: *field.Kind, Value: *field.Value})
	}
	item.Subject = &subject
	return nil
}

// Validate checks the inbox kind, kind-specific identities, assignment, version
// and time. A request item requires the immutable typed subject, the declared
// response output names and the action deadline; it refuses the waiting-task
// question projection. A waiting_task item requires the question identity,
// body, durable references and lifecycle version and refuses the request
// projection; failed_task refuses both, so a stale or injected projection can
// never masquerade as another kind.
func (item InboxItem) Validate() error {
	if !validInboxKind(item.Kind) {
		return invalid("unknown inbox kind %q", item.Kind)
	}
	if item.Version == 0 {
		return invalid("inbox item version must be positive")
	}
	if item.At.IsZero() {
		return invalid("inbox item time is zero")
	}
	switch item.Kind {
	case "request":
		if !validID(item.ExecutionID) {
			return invalid("inbox request execution_id is invalid")
		}
		if !validID(item.ActionID) {
			return invalid("inbox request action_id is invalid")
		}
		if !validAssignment(item.Assignment) {
			return invalid("inbox request assignment %q must be role/<role> or service/<id>", item.Assignment)
		}
		if item.Subject == nil {
			return invalid("inbox request carries no subject")
		}
		if err := item.Subject.Validate(); err != nil {
			return err
		}
		if len(item.Subject.Fields) == 0 {
			return invalid("inbox request subject is empty")
		}
		if err := validateOutputNames(item.Outputs); err != nil {
			return err
		}
		if err := item.validateNoQuestionProjection(); err != nil {
			return err
		}
	case "waiting_task":
		if !validID(item.TaskID) {
			return invalid("inbox waiting_task task_id is invalid")
		}
		if item.ExecutionID != "" || item.ActionID != "" {
			return invalid("inbox waiting_task carries an execution or action identity")
		}
		if !validAssignment(item.Assignment) {
			return invalid("inbox waiting_task assignment %q must be role/<role> or service/<id>", item.Assignment)
		}
		if err := item.validateNoRequestProjection(); err != nil {
			return err
		}
		if !validID(item.QuestionID) {
			return invalid("inbox waiting_task question_id is invalid")
		}
		if item.Question == "" {
			return invalid("inbox waiting_task question is empty")
		}
		if err := validateReferences(item.References); err != nil {
			return err
		}
		if item.LifecycleVersion == 0 {
			return invalid("inbox waiting_task lifecycle_version must be positive")
		}
	case "failed_task":
		if !validID(item.TaskID) {
			return invalid("inbox failed_task task_id is invalid")
		}
		if item.ExecutionID != "" || item.ActionID != "" {
			return invalid("inbox failed_task carries an execution or action identity")
		}
		if item.Assignment != "" && !validAssignment(item.Assignment) {
			return invalid("inbox failed_task assignment %q must be role/<role> or service/<id>", item.Assignment)
		}
		if err := item.validateNoRequestProjection(); err != nil {
			return err
		}
		if err := item.validateNoQuestionProjection(); err != nil {
			return err
		}
	}
	if item.TaskID != "" && !validID(item.TaskID) {
		return invalid("inbox task_id is invalid")
	}
	if item.ExecutionID != "" && !validID(item.ExecutionID) {
		return invalid("inbox execution_id is invalid")
	}
	if item.ActionID != "" && !validID(item.ActionID) {
		return invalid("inbox action_id is invalid")
	}
	return nil
}

// validateNoRequestProjection refuses the request-only subject and declared
// output names on a kind that does not own them.
func (item InboxItem) validateNoRequestProjection() error {
	if item.Subject != nil {
		return invalid("inbox %s carries a request subject", item.Kind)
	}
	if len(item.Outputs) > 0 {
		return invalid("inbox %s carries request outputs", item.Kind)
	}
	return nil
}

// validateOutputNames checks the bounded declared response name list: every
// entry is a non-empty lowercase key and the list fit the subject bound.
func validateOutputNames(outputs []string) error {
	if len(outputs) > MaxSubjectFields {
		return invalid("inbox request has %d outputs; at most %d are allowed", len(outputs), MaxSubjectFields)
	}
	for _, output := range outputs {
		if !validOutputKey(output) {
			return invalid("inbox request output name %q is invalid", output)
		}
	}
	return nil
}

// validOutputKey reports whether name is a bounded lowercase declared key.
func validOutputKey(name string) bool {
	if name == "" || len(name) > maxIDLength {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case index > 0 && character >= '0' && character <= '9':
		case index > 0 && (character == '_' || character == '-'):
		default:
			return false
		}
	}
	return true
}

// validateNoQuestionProjection refuses the waiting-only question projection on
// a kind that does not own it.
func (item InboxItem) validateNoQuestionProjection() error {
	if item.QuestionID != "" {
		return invalid("inbox %s carries a question_id", item.Kind)
	}
	if item.Question != "" {
		return invalid("inbox %s carries a question", item.Kind)
	}
	if len(item.References) > 0 {
		return invalid("inbox %s carries references", item.Kind)
	}
	if item.LifecycleVersion != 0 {
		return invalid("inbox %s carries a lifecycle_version", item.Kind)
	}
	return nil
}

// TaskInboxResult is the task.inbox result.
type TaskInboxResult struct {
	Items      []InboxItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// Validate checks every inbox item and the optional cursor bound.
func (result TaskInboxResult) Validate() error {
	for _, item := range result.Items {
		if err := item.Validate(); err != nil {
			return err
		}
	}
	if len(result.NextCursor) > MaxCursorBytes {
		return invalid("next_cursor is %d bytes; at most %d are allowed", len(result.NextCursor), MaxCursorBytes)
	}
	return nil
}

// inboxKinds is the inbox kind vocabulary.
var inboxKinds = []string{"request", "waiting_task", "failed_task"}

// validInboxKind reports whether kind is a declared inbox kind.
func validInboxKind(kind string) bool {
	for _, candidate := range inboxKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}
