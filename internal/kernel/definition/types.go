// Package definition defines the typed, immutable pipeline definition values
// that admission hands to the Kernel.
//
// The Shell decodes YAML and validates structure; these declarations carry the
// result without importing a YAML library or interpreting task, role or Git
// semantics. Declared values are bounded and copied so an admitted snapshot
// cannot be changed after admission.
package definition

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// ActionKind is the single kind of an admitted action.
type ActionKind string

const (
	// ActionKindContainer runs a pinned container workload.
	ActionKindContainer ActionKind = "container"
	// ActionKindRequest asks an assignee for a decision about a fixed subject.
	ActionKindRequest ActionKind = "request"
)

// ActionKinds returns every supported action kind in declaration order.
func ActionKinds() []ActionKind {
	return []ActionKind{ActionKindContainer, ActionKindRequest}
}

// Valid reports whether the kind is one of the declared kinds.
func (kind ActionKind) Valid() bool {
	for _, candidate := range ActionKinds() {
		if kind == candidate {
			return true
		}
	}
	return false
}

// EnvVar is one literal environment entry. Values are never interpolated.
type EnvVar struct {
	Name  id.EnvName `json:"name"`
	Value string     `json:"value"`
}

// Container is a validated container action declaration. Images are pinned to
// immutable digests before admission; secrets are names only, never values.
type Container struct {
	Image    string          `json:"image"`
	Commands []string        `json:"commands"`
	Env      []EnvVar        `json:"env"`
	Secrets  []id.SecretName `json:"secrets"`
	Outputs  []id.OutputKey  `json:"outputs"`
}

// Request is a validated request action declaration. Assignment is an opaque
// validated value; the Kernel never interprets roles or access rules.
type Request struct {
	Assignment string         `json:"assignment"`
	Subject    Subject        `json:"subject"`
	Outputs    []id.OutputKey `json:"outputs"`
}

// Action is one ordered action declaration. Exactly one of Container or
// Request is present.
type Action struct {
	Name      id.ActionName `json:"name"`
	Timeout   time.Duration `json:"timeout"`
	Container *Container    `json:"container,omitempty"`
	Request   *Request      `json:"request,omitempty"`
}

// Pipeline is the validated, ordered action list with admitted inputs and
// literal pipeline defaults.
type Pipeline struct {
	Inputs  []id.InputKey `json:"inputs"`
	Env     []EnvVar      `json:"env"`
	Actions []Action      `json:"actions"`
}

// clone returns a deep copy so callers cannot mutate admitted values.
func (p Pipeline) clone() Pipeline {
	out := Pipeline{
		Inputs:  append([]id.InputKey(nil), p.Inputs...),
		Env:     append([]EnvVar(nil), p.Env...),
		Actions: make([]Action, len(p.Actions)),
	}
	for index, action := range p.Actions {
		out.Actions[index] = action.clone()
	}
	return out
}

func (a Action) clone() Action {
	out := a
	if a.Container != nil {
		container := *a.Container
		container.Commands = append([]string(nil), a.Container.Commands...)
		container.Env = append([]EnvVar(nil), a.Container.Env...)
		container.Secrets = append([]id.SecretName(nil), a.Container.Secrets...)
		container.Outputs = append([]id.OutputKey(nil), a.Container.Outputs...)
		out.Container = &container
	}
	if a.Request != nil {
		request := *a.Request
		request.Subject = a.Request.Subject.clone()
		request.Outputs = append([]id.OutputKey(nil), a.Request.Outputs...)
		out.Request = &request
	}
	return out
}

// SubjectKind distinguishes a literal from a direct lookup.
type SubjectKind string

const (
	// SubjectLiteral is an ordinary string value.
	SubjectLiteral SubjectKind = "literal"
	// SubjectReference is a direct lookup of one declared input or one earlier
	// succeeded action output. There is no expression, template or path
	// evaluation.
	SubjectReference SubjectKind = "reference"
)

// SubjectKinds returns every supported subject value kind.
func SubjectKinds() []SubjectKind {
	return []SubjectKind{SubjectLiteral, SubjectReference}
}

// Valid reports whether the kind is one of the declared kinds.
func (kind SubjectKind) Valid() bool {
	for _, candidate := range SubjectKinds() {
		if kind == candidate {
			return true
		}
	}
	return false
}

// Reference is one direct subject lookup. Input selects inputs.<Key>; otherwise
// Action selects <Action>.outputs.<Key>. Declared-set membership and ordering
// are validated by the definition package; Key stays a plain string because the
// declared input and output key spaces are distinct.
type Reference struct {
	Input  bool   `json:"input,omitempty"`
	Action string `json:"action,omitempty"`
	Key    string `json:"key"`
}

func (r Reference) validate() error {
	if r.Key == "" {
		return fault.New(fault.InvalidInput, "subject reference key is empty")
	}
	if r.Input == (r.Action != "") {
		return fault.New(fault.InvalidInput, "subject reference must name exactly one source")
	}
	return nil
}

// SubjectValue is one subject field: either a literal or a reference.
type SubjectValue struct {
	Kind      SubjectKind `json:"kind"`
	Literal   string      `json:"literal,omitempty"`
	Reference Reference   `json:"reference,omitempty"`
}

// Validate checks the structural shape of one subject value.
func (v SubjectValue) Validate() error {
	switch v.Kind {
	case SubjectLiteral:
		if v.Reference != (Reference{}) {
			return fault.New(fault.InvalidInput, "subject literal also carries a reference")
		}
	case SubjectReference:
		if v.Literal != "" {
			return fault.New(fault.InvalidInput, "subject reference also carries a literal")
		}
		return v.Reference.validate()
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown subject value kind %q", v.Kind))
	}
	return nil
}

// MarshalJSON emits the exact subject value shape without leaking the
// unused half of the union.
func (v SubjectValue) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case SubjectLiteral:
		if v.Reference != (Reference{}) {
			return nil, fmt.Errorf("definition: malformed subject literal")
		}
		return json.Marshal(struct {
			Kind    SubjectKind `json:"kind"`
			Literal string      `json:"literal"`
		}{v.Kind, v.Literal})
	case SubjectReference:
		if v.Literal != "" {
			return nil, fmt.Errorf("definition: malformed subject reference")
		}
		if err := v.Reference.validate(); err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Kind      SubjectKind `json:"kind"`
			Reference Reference   `json:"reference"`
		}{v.Kind, v.Reference})
	default:
		return nil, fmt.Errorf("definition: unknown subject value kind %q", v.Kind)
	}
}

// UnmarshalJSON decodes exactly one literal or reference form.
func (v *SubjectValue) UnmarshalJSON(data []byte) error {
	var raw struct {
		Kind      SubjectKind `json:"kind"`
		Literal   *string     `json:"literal"`
		Reference *Reference  `json:"reference"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("definition: decode subject value: %w", err)
	}
	switch raw.Kind {
	case SubjectLiteral:
		if raw.Literal == nil || raw.Reference != nil {
			return fault.New(fault.InvalidInput, "subject literal must not carry a reference")
		}
		*v = SubjectValue{Kind: SubjectLiteral, Literal: *raw.Literal}
	case SubjectReference:
		if raw.Reference == nil || raw.Literal != nil {
			return fault.New(fault.InvalidInput, "subject reference must not carry a literal")
		}
		if err := raw.Reference.validate(); err != nil {
			return err
		}
		*v = SubjectValue{Kind: SubjectReference, Reference: *raw.Reference}
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown subject value kind %q", raw.Kind))
	}
	return nil
}

// Subject is a bounded, immutable object of literals and direct lookups.
// Resolution persists one instance and never rebinds it after a pipeline edit.
type Subject struct {
	fields map[string]SubjectValue
}

// NewSubject copies fields into an immutable subject.
func NewSubject(fields map[string]SubjectValue) Subject {
	return Subject{fields: cloneSubjectFields(fields)}
}

func cloneSubjectFields(fields map[string]SubjectValue) map[string]SubjectValue {
	if fields == nil {
		return nil
	}
	out := make(map[string]SubjectValue, len(fields))
	for name, value := range fields {
		out[name] = value
	}
	return out
}

// Len returns the number of subject fields.
func (s Subject) Len() int {
	return len(s.fields)
}

// Fields returns a copy of the subject fields.
func (s Subject) Fields() map[string]SubjectValue {
	return cloneSubjectFields(s.fields)
}

func (s Subject) clone() Subject {
	return Subject{fields: cloneSubjectFields(s.fields)}
}

// MarshalJSON encodes the subject as an object of subject values.
func (s Subject) MarshalJSON() ([]byte, error) {
	if s.fields == nil {
		return []byte("null"), nil
	}
	return json.Marshal(s.fields)
}

// UnmarshalJSON decodes a subject object.
func (s *Subject) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		s.fields = nil
		return nil
	}
	var fields map[string]SubjectValue
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("definition: decode subject: %w", err)
	}
	s.fields = fields
	return nil
}

// RuntimeRequirements are the admitted adapter capabilities the execution
// requires. Unsupported requirements are rejected before dispatch.
type RuntimeRequirements struct {
	Capabilities []string `json:"capabilities"`
}

func (r RuntimeRequirements) clone() RuntimeRequirements {
	return RuntimeRequirements{Capabilities: append([]string(nil), r.Capabilities...)}
}

// SnapshotInput is the complete admission data for one execution snapshot.
// Task is nil for a task-free execution; a present task may still carry zero
// bytes, and that distinction is preserved. TaskID is empty when task-free.
// Secrets carry names only.
type SnapshotInput struct {
	Pipeline   Pipeline
	YAML       []byte
	YAMLDigest [32]byte
	Inputs     map[string]string
	Runtime    RuntimeRequirements
	Task       []byte
	TaskID     id.TaskID
}

// Snapshot is the immutable admitted definition/input data of one execution.
// Every accessor returns a copy so a caller cannot mutate admitted state.
type Snapshot struct {
	pipeline   Pipeline
	yaml       []byte
	yamlDigest [32]byte
	inputs     map[string]string
	runtime    RuntimeRequirements
	task       []byte
	taskID     id.TaskID
}

// NewSnapshot copies input into an immutable snapshot.
func NewSnapshot(input SnapshotInput) *Snapshot {
	return &Snapshot{
		pipeline:   input.Pipeline.clone(),
		yaml:       append([]byte(nil), input.YAML...),
		yamlDigest: input.YAMLDigest,
		inputs:     cloneStringMap(input.Inputs),
		runtime:    input.Runtime.clone(),
		task:       cloneOptionalBytes(input.Task),
		taskID:     input.TaskID,
	}
}

// cloneOptionalBytes copies value while preserving the nil-vs-empty
// distinction: nil stays nil, and a present zero-length value stays non-nil.
// Admission uses it for opaque task bytes so a present-empty task is never
// collapsed into a task-free execution.
func cloneOptionalBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte{}, value...)
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// Pipeline returns the admitted ordered definition.
func (s *Snapshot) Pipeline() Pipeline {
	if s == nil {
		return Pipeline{}
	}
	return s.pipeline.clone()
}

// YAML returns the exact admitted YAML bytes.
func (s *Snapshot) YAML() []byte {
	if s == nil {
		return nil
	}
	return append([]byte(nil), s.yaml...)
}

// YAMLDigest returns the SHA-256 of the exact admitted YAML bytes.
func (s *Snapshot) YAMLDigest() [32]byte {
	if s == nil {
		return [32]byte{}
	}
	return s.yamlDigest
}

// Inputs returns the admitted input values.
func (s *Snapshot) Inputs() map[string]string {
	if s == nil {
		return nil
	}
	return cloneStringMap(s.inputs)
}

// Runtime returns the admitted runtime requirements.
func (s *Snapshot) Runtime() RuntimeRequirements {
	if s == nil {
		return RuntimeRequirements{}
	}
	return s.runtime.clone()
}

// Task returns the opaque admitted task snapshot bytes and whether a task was
// present. The Kernel never interprets these bytes. A present task may be
// empty; that is distinct from a task-free execution, which returns nil.
func (s *Snapshot) Task() ([]byte, bool) {
	if s == nil || s.task == nil {
		return nil, false
	}
	return cloneOptionalBytes(s.task), true
}

// TaskID returns the admitted task identity for KODAMA_TASK context. It is
// empty when the execution is task-free.
func (s *Snapshot) TaskID() id.TaskID {
	if s == nil {
		return ""
	}
	return s.taskID
}
