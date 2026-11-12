// Package pipeline declares pipeline definitions and immutable admission
// snapshots.
//
// A pipeline is the API-posted definition: a stable id, a project-unique name,
// exact YAML bytes and an optimistic version. Admission freezes one snapshot
// whose YAML hash is the SHA-256 of the exact admitted bytes, never a
// canonicalized YAML hash; later definition edits cannot mutate it. This
// package declares the values and the pure bounds that registry digest pinning
// and YAML decoding share.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// PipelineID identifies one API-posted pipeline definition.
type PipelineID string

// ParsePipelineID validates and converts a pipeline identity.
func ParsePipelineID(value string) (PipelineID, error) {
	if !validPipelineID(value) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("invalid pipeline id %q", value))
	}
	return PipelineID(value), nil
}

func validPipelineID(value string) bool {
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

// InputKind is the declared type of one admission input.
type InputKind string

const (
	// InputString accepts any UTF-8 string value.
	InputString InputKind = "string"
	// InputNumber accepts a finite decimal number literal.
	InputNumber InputKind = "number"
	// InputBoolean accepts a true/false literal.
	InputBoolean InputKind = "boolean"
)

var inputKinds = [...]InputKind{InputString, InputNumber, InputBoolean}

// Valid reports whether the kind is one of the declared kinds.
func (kind InputKind) Valid() bool {
	for _, candidate := range inputKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

// InputSpec is one declared pipeline input. Key must be a valid input key;
// Required inputs must be supplied at admission.
type InputSpec struct {
	Key      id.InputKey `json:"key"`
	Kind     InputKind   `json:"kind"`
	Required bool        `json:"required"`
}

// ValidateInputs enforces the declared input set, required values and typed
// literals. Extra undeclared values, missing required values and values that
// do not parse as their declared kind are refused with invalid_input.
func ValidateInputs(specs []InputSpec, values map[id.InputKey]string) error {
	declared := make(map[id.InputKey]InputSpec, len(specs))
	for _, spec := range specs {
		if _, err := id.ParseInputKey(string(spec.Key)); err != nil {
			return fault.Wrap(fault.InvalidInput, "declared input key is invalid", err)
		}
		if !spec.Kind.Valid() {
			return fault.New(fault.InvalidInput, fmt.Sprintf("declared input %q has unknown kind %q", spec.Key, spec.Kind))
		}
		if _, duplicate := declared[spec.Key]; duplicate {
			return fault.New(fault.InvalidInput, fmt.Sprintf("declared input %q is duplicated", spec.Key))
		}
		declared[spec.Key] = spec
	}
	for key, value := range values {
		spec, ok := declared[key]
		if !ok {
			return fault.New(fault.InvalidInput, fmt.Sprintf("input %q is not declared", key))
		}
		if err := validateInputValue(spec, value); err != nil {
			return err
		}
	}
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		if _, present := values[spec.Key]; !present {
			return fault.New(fault.InvalidInput, fmt.Sprintf("required input %q is missing", spec.Key))
		}
	}
	return nil
}

func validateInputValue(spec InputSpec, value string) error {
	switch spec.Kind {
	case InputString:
		return nil
	case InputNumber:
		number, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("input %q is not a finite number", spec.Key))
		}
		return nil
	case InputBoolean:
		if _, err := strconv.ParseBool(value); err != nil {
			return fault.New(fault.InvalidInput, fmt.Sprintf("input %q is not a boolean", spec.Key))
		}
		return nil
	default:
		return fault.New(fault.InvalidInput, fmt.Sprintf("input %q has unknown kind %q", spec.Key, spec.Kind))
	}
}

// Limits bounds one admitted YAML document. Alias expansion is not supported,
// so MaxAliases is zero: any alias is refused.
type Limits struct {
	MaxYAMLBytes int `json:"max_yaml_bytes"`
	MaxDepth     int `json:"max_depth"`
	MaxAliases   int `json:"max_aliases"`
}

// DefaultLimits is the declared admission bound set: 1 MiB of YAML, 64 levels
// of nesting and no alias expansion.
var DefaultLimits = Limits{
	MaxYAMLBytes: 1 << 20,
	MaxDepth:     64,
	MaxAliases:   0,
}

// CheckYAMLBytes refuses a document larger than the bound.
func (limits Limits) CheckYAMLBytes(size int) error {
	if size > limits.MaxYAMLBytes {
		return fault.New(fault.InvalidInput, fmt.Sprintf("yaml is %d bytes; at most %d are allowed", size, limits.MaxYAMLBytes))
	}
	return nil
}

// CheckDepth refuses a document nested deeper than the bound.
func (limits Limits) CheckDepth(depth int) error {
	if depth > limits.MaxDepth {
		return fault.New(fault.InvalidInput, fmt.Sprintf("yaml depth is %d; at most %d is allowed", depth, limits.MaxDepth))
	}
	return nil
}

// CheckAliases refuses more aliases than the bound. The default bound is zero.
func (limits Limits) CheckAliases(count int) error {
	if count > limits.MaxAliases {
		return fault.New(fault.InvalidInput, fmt.Sprintf("yaml uses %d aliases; at most %d are allowed", count, limits.MaxAliases))
	}
	return nil
}

// PinnedImage is one image reference resolved to an immutable digest before
// admission. A mutable tag never reaches the snapshot.
type PinnedImage struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

// PushTrigger is the optional exact-ref push definition of an API-posted
// pipeline. Refs match exactly; there are no patterns or input mappings.
type PushTrigger struct {
	Repository resource.ResourceID `json:"repository"`
	Service    service.ServiceID   `json:"service"`
	Refs       []string            `json:"refs"`
}

// SnapshotInput is the admission input for one immutable snapshot. Env carries
// the admitted environment defaults from the definition's `env` mapping; they
// are copied into the snapshot so later definition edits cannot change them.
type SnapshotInput struct {
	YAML         []byte
	Inputs       map[id.InputKey]string
	Env          []definition.EnvVar
	Images       []PinnedImage
	SecretNames  []id.SecretName
	Requirements []string
	TaskID       id.TaskID
	Conversation []byte
	References   []string
	Metadata     map[string]string
}

// Snapshot is the immutable admitted definition. It owns copies of every input
// so a caller cannot mutate admitted data after admission, and its YAML hash is
// the SHA-256 of the exact admitted bytes. Env is the admitted environment
// default set. Conversation is the admitted task conversation bytes and is
// absent (nil) for a task-free execution; an admitted empty conversation is
// present and empty, which Conversation/HasConversation preserve and the JSON
// form keeps distinct (null versus an empty string).
type Snapshot struct {
	YAMLSHA256   [32]byte               `json:"yaml_sha256"`
	Inputs       map[id.InputKey]string `json:"inputs,omitempty"`
	Env          []definition.EnvVar    `json:"env,omitempty"`
	Images       []PinnedImage          `json:"images,omitempty"`
	SecretNames  []id.SecretName        `json:"secret_names,omitempty"`
	Requirements []string               `json:"requirements,omitempty"`
	TaskID       id.TaskID              `json:"task_id,omitempty"`
	Conversation []byte                 `json:"conversation"`
	References   []string               `json:"references,omitempty"`
	Metadata     map[string]string      `json:"metadata,omitempty"`

	yaml []byte
}

// NewSnapshot validates and copies one admission snapshot. Empty YAML is
// refused: a pipeline always has exact admitted bytes.
func NewSnapshot(input SnapshotInput) (Snapshot, error) {
	if len(input.YAML) == 0 {
		return Snapshot{}, fault.New(fault.InvalidInput, "admitted yaml is empty")
	}
	if err := DefaultLimits.CheckYAMLBytes(len(input.YAML)); err != nil {
		return Snapshot{}, err
	}
	for _, image := range input.Images {
		if image.Reference == "" || image.Digest == "" {
			return Snapshot{}, fault.New(fault.InvalidInput, "pinned image requires a reference and a digest")
		}
	}
	for _, name := range input.SecretNames {
		if _, err := id.ParseSecretName(string(name)); err != nil {
			return Snapshot{}, fault.Wrap(fault.InvalidInput, "snapshot secret name is invalid", err)
		}
	}
	for _, requirement := range input.Requirements {
		if requirement == "" {
			return Snapshot{}, fault.New(fault.InvalidInput, "snapshot runtime requirement is empty")
		}
	}
	yaml := append([]byte(nil), input.YAML...)
	return Snapshot{
		YAMLSHA256:   sha256.Sum256(yaml),
		Inputs:       cloneInputs(input.Inputs),
		Env:          cloneEnv(input.Env),
		Images:       append([]PinnedImage(nil), input.Images...),
		SecretNames:  append([]id.SecretName(nil), input.SecretNames...),
		Requirements: append([]string(nil), input.Requirements...),
		TaskID:       input.TaskID,
		Conversation: cloneBytes(input.Conversation),
		References:   append([]string(nil), input.References...),
		Metadata:     cloneMetadata(input.Metadata),
		yaml:         yaml,
	}, nil
}

// YAMLBytes returns a copy of the exact admitted YAML bytes.
func (snapshot Snapshot) YAMLBytes() []byte {
	return append([]byte(nil), snapshot.yaml...)
}

// ConversationBytes returns a copy of the admitted task conversation. It is nil
// when the execution is task-free and non-nil (possibly empty) when a
// conversation was admitted.
func (snapshot Snapshot) ConversationBytes() []byte {
	return cloneBytes(snapshot.Conversation)
}

// HasConversation reports whether a task conversation was admitted. An admitted
// empty conversation is present; a task-free execution is absent. The
// distinction is durable: the snapshot keeps it in memory and in its JSON form.
func (snapshot Snapshot) HasConversation() bool {
	return snapshot.Conversation != nil
}

// HashHex returns the lowercase hex SHA-256 of the exact admitted YAML bytes.
func (snapshot Snapshot) HashHex() string {
	return hex.EncodeToString(snapshot.YAMLSHA256[:])
}

// cloneBytes copies exact bytes and preserves nil versus empty: an empty but
// non-nil input stays empty and non-nil, because an admitted empty conversation
// is different from an absent one.
func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}

func cloneEnv(values []definition.EnvVar) []definition.EnvVar {
	if values == nil {
		return nil
	}
	out := make([]definition.EnvVar, len(values))
	copy(out, values)
	return out
}

func cloneInputs(values map[id.InputKey]string) map[id.InputKey]string {
	if values == nil {
		return nil
	}
	out := make(map[id.InputKey]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneMetadata(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

// Pipeline is one API-posted definition at its current version.
type Pipeline struct {
	ID         PipelineID    `json:"id"`
	Scope      tenancy.Scope `json:"scope"`
	Name       string        `json:"name"`
	YAML       []byte        `json:"-"`
	YAMLSHA256 [32]byte      `json:"yaml_sha256"`
	Version    uint64        `json:"version"`
	Push       *PushTrigger  `json:"push,omitempty"`
}
