package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	pipelinev1 "github.com/optimiware/kodama/contracts/pipeline/v1"
	pipelinev1alpha2 "github.com/optimiware/kodama/contracts/pipeline/v1alpha2"
	pipelinev1alpha3 "github.com/optimiware/kodama/contracts/pipeline/v1alpha3"
)

// StepKind is the executor family of a step.
type StepKind string

const (
	KindContainer StepKind = "container"
	KindAgent     StepKind = "agent"
	KindGate      StepKind = "gate"
)

// Valid reports whether k is one of the contract's step kinds. Whether an
// installation enables it is a capability question, not a shape question.
func (k StepKind) Valid() bool { return k == KindContainer || k == KindAgent || k == KindGate }

// ValueType is the declared type of an input or output.
type ValueType string

const (
	TypeString      ValueType = "string"
	TypeBoolean     ValueType = "boolean"
	TypeInteger     ValueType = "integer"
	TypeArtifact    ValueType = "artifact"
	TypeOCIArtifact ValueType = "ociArtifact"
)

// IsScalar reports whether the type is a string, boolean or integer.
func (t ValueType) IsScalar() bool {
	return t == TypeString || t == TypeBoolean || t == TypeInteger
}

// Access is the workspace access of a step.
type Access string

const (
	AccessNone  Access = "none"
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// RunSource is the workspace source naming the original run commit.
const RunSource = "run"

// ImplicitCommitOutput is the output every successful writer step provides.
const ImplicitCommitOutput = "commit"

// RetryClass is a failure class a step may be retried on.
type RetryClass string

const (
	RetryInfrastructure RetryClass = "infrastructure"
	RetryTransient      RetryClass = "transient"
)

// OutputsRoot is the only directory below which artifact outputs may live.
const OutputsRoot = "/kodama/outputs"

// Document is a typed pipeline document. A Document produced by ParseYAML or
// ParseJSON is structurally valid and not yet semantically validated; a Document
// is never evidence of registrability, only a Validated value is.
type Document struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata names the pipeline.
type Metadata struct {
	Name string `json:"name"`
}

// Spec is the pipeline body. TimeoutSeconds, RunWorkspace, Services and Network
// exist only in kodama/v1alpha2 documents; the v1alpha1 branches ignore them
// and the validator rejects a v1alpha1 document that sets any of them.
type Spec struct {
	TimeoutSeconds int64                 `json:"timeoutSeconds,omitempty"`
	RunWorkspace   *RunWorkspace         `json:"runWorkspace,omitempty"`
	Inputs         map[string]Input      `json:"inputs,omitempty"`
	Services       map[string]RunService `json:"services,omitempty"`
	Network        *Network              `json:"network,omitempty"`
	Steps          []Step                `json:"steps"`
}

// Input declares a run input.
type Input struct {
	Type     ValueType `json:"type"`
	Default  *Scalar   `json:"default,omitempty"`
	Required bool      `json:"required,omitempty"`
}

// Scalar is a string, boolean or integer literal. Integers keep full int64
// precision.
type Scalar struct {
	Type ValueType
	Str  string
	Bool bool
	Int  int64
}

// StringScalar, BoolScalar and IntScalar build literals.
func StringScalar(s string) Scalar { return Scalar{Type: TypeString, Str: s} }
func BoolScalar(b bool) Scalar     { return Scalar{Type: TypeBoolean, Bool: b} }
func IntScalar(i int64) Scalar     { return Scalar{Type: TypeInteger, Int: i} }

func (s Scalar) value() any {
	switch s.Type {
	case TypeBoolean:
		return s.Bool
	case TypeInteger:
		return s.Int
	default:
		return s.Str
	}
}

// UnmarshalJSON accepts exactly a JSON string, boolean or integer.
func (s *Scalar) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("empty scalar")
	}
	switch data[0] {
	case '"':
		var v string
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*s = StringScalar(v)
	case 't', 'f':
		var v bool
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*s = BoolScalar(v)
	default:
		v, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil {
			return errors.New("scalar is not a string, boolean or integer")
		}
		*s = IntScalar(v)
	}
	return nil
}

// MarshalJSON writes the literal.
func (s Scalar) MarshalJSON() ([]byte, error) { return json.Marshal(s.value()) }

// Condition is a step's `when`: run only if the named input equals the value.
type Condition struct {
	Input  string `json:"input"`
	Equals Scalar `json:"equals"`
}

// Workspace is a step's source access.
type Workspace struct {
	Access Access `json:"access"`
	From   string `json:"from,omitempty"`
}

// EnvValue is an environment entry: a nonsecret scalar (possibly a whole-scalar
// expression) or a secret reference.
type EnvValue struct {
	Scalar    *Scalar
	SecretRef string
}

// UnmarshalJSON accepts a scalar or {"secretRef": "secret://project/name"}.
func (e *EnvValue) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var v struct {
			SecretRef string `json:"secretRef"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return errors.New("environment entry is not a scalar or a secretRef")
		}
		*e = EnvValue{SecretRef: v.SecretRef}
		return nil
	}
	var s Scalar
	if err := s.UnmarshalJSON(data); err != nil {
		return err
	}
	*e = EnvValue{Scalar: &s}
	return nil
}

func (e EnvValue) value() any {
	if e.Scalar != nil {
		return e.Scalar.value()
	}
	return map[string]any{"secretRef": e.SecretRef}
}

// Retry bounds automatic re-attempts. The zero value means one attempt.
type Retry struct {
	MaxAttempts    int64        `json:"maxAttempts,omitempty"`
	BackoffSeconds int64        `json:"backoffSeconds,omitempty"`
	On             []RetryClass `json:"on,omitempty"`
}

// Output declares a named step output.
type Output struct {
	Type ValueType `json:"type"`
	Path string    `json:"path,omitempty"`
}

// AgentSpec selects an immutable agent profile.
type AgentSpec struct {
	Profile string `json:"profile"`
}

// GateSpec carries a human gate's references.
type GateSpec struct {
	Evidence     []string `json:"evidence,omitempty"`
	DeploymentID string   `json:"deploymentId,omitempty"`
}

// Step is one node of the graph. Kind-specific fields are meaningful only for
// their kind; the schema rejects the others.
type Step struct {
	ID    string     `json:"id"`
	Kind  StepKind   `json:"kind"`
	Needs []string   `json:"needs,omitempty"`
	When  *Condition `json:"when,omitempty"`

	Workspace   *Workspace          `json:"workspace,omitempty"`
	Environment string              `json:"environment,omitempty"`
	Image       string              `json:"image,omitempty"`
	Command     []string            `json:"command,omitempty"`
	Args        []string            `json:"args,omitempty"`
	Env         map[string]EnvValue `json:"env,omitempty"`
	SecretFiles map[string]string   `json:"secretFiles,omitempty"`
	// State and ArtifactFiles are kodama/v1alpha3 executable-step bindings. They
	// are absent from the older formats and are never inferred.
	State         map[string]StateBinding `json:"state,omitempty"`
	ArtifactFiles map[string]string       `json:"artifactFiles,omitempty"`

	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	Retry          *Retry            `json:"retry,omitempty"`
	Outputs        map[string]Output `json:"outputs,omitempty"`

	Agent *AgentSpec `json:"agent,omitempty"`
	Gate  *GateSpec  `json:"gate,omitempty"`

	// Egress names authorized egress profiles (kodama/v1alpha2 container steps
	// only).
	Egress []string `json:"egress,omitempty"`
}

// Executable reports whether the step runs work (container or agent).
func (s Step) Executable() bool { return s.Kind == KindContainer || s.Kind == KindAgent }

// access returns the effective workspace access (none when omitted).
func (s Step) access() Access {
	if s.Workspace == nil || s.Workspace.Access == "" {
		return AccessNone
	}
	return s.Workspace.Access
}

// IsWriter reports whether the step publishes a new source commit.
func (s Step) IsWriter() bool { return s.Executable() && s.access() == AccessWrite }

// normalized returns a deep copy with every documented default applied:
// absent needs, workspace, retry, args, env, secret files, outputs and inputs
// become their empty forms; retry.maxAttempts defaults to one attempt. Nothing
// else is rewritten. The copy shares no map or slice with the receiver.
func normalize(d Document) Document {
	if d.IsShared() {
		return normalizeV2(d)
	}
	out := Document{APIVersion: d.APIVersion, Kind: d.Kind, Metadata: d.Metadata}
	out.Spec.Inputs = make(map[string]Input, len(d.Spec.Inputs))
	for name, in := range d.Spec.Inputs {
		c := Input{Type: in.Type, Required: in.Required}
		if in.Default != nil {
			v := *in.Default
			c.Default = &v
		}
		out.Spec.Inputs[name] = c
	}
	out.Spec.Steps = make([]Step, len(d.Spec.Steps))
	for i, s := range d.Spec.Steps {
		c := Step{ID: s.ID, Kind: s.Kind, Needs: append([]string{}, s.Needs...)}
		if s.When != nil {
			w := *s.When
			c.When = &w
		}
		switch s.Kind {
		case KindGate:
			g := GateSpec{Evidence: []string{}}
			if s.Gate != nil {
				g.Evidence = append(g.Evidence, s.Gate.Evidence...)
				g.DeploymentID = s.Gate.DeploymentID
			}
			c.Gate = &g
		default:
			ws := Workspace{Access: AccessNone}
			if s.Workspace != nil && s.Workspace.Access != "" {
				ws = *s.Workspace
			}
			c.Workspace = &ws
			r := Retry{MaxAttempts: 1, On: []RetryClass{}}
			if s.Retry != nil {
				r.BackoffSeconds = s.Retry.BackoffSeconds
				r.On = append(r.On, s.Retry.On...)
				if s.Retry.MaxAttempts != 0 {
					r.MaxAttempts = s.Retry.MaxAttempts
				}
			}
			c.Retry = &r
			c.TimeoutSeconds = s.TimeoutSeconds
			c.Outputs = make(map[string]Output, len(s.Outputs))
			for k, v := range s.Outputs {
				c.Outputs[k] = v
			}
			if s.Kind == KindContainer {
				c.Environment = s.Environment
				c.Image = s.Image
				c.Command = append([]string{}, s.Command...)
				c.Args = append([]string{}, s.Args...)
				c.Env = make(map[string]EnvValue, len(s.Env))
				for k, v := range s.Env {
					e := EnvValue{SecretRef: v.SecretRef}
					if v.Scalar != nil {
						sc := *v.Scalar
						e.Scalar = &sc
					}
					c.Env[k] = e
				}
				c.SecretFiles = make(map[string]string, len(s.SecretFiles))
				for k, v := range s.SecretFiles {
					c.SecretFiles[k] = v
				}
			}
			if s.Agent != nil {
				a := *s.Agent
				c.Agent = &a
			}
		}
		out.Spec.Steps[i] = c
	}
	return out
}

// tree renders a normalized document as a generic JSON tree whose maps
// serialize with sorted keys. It is the input of canonical serialization.
func (d Document) tree() map[string]any {
	if d.IsShared() {
		return d.treeV2()
	}
	inputs := map[string]any{}
	for name, in := range d.Spec.Inputs {
		m := map[string]any{"type": string(in.Type), "required": in.Required}
		if in.Default != nil {
			m["default"] = in.Default.value()
		}
		inputs[name] = m
	}
	steps := make([]any, 0, len(d.Spec.Steps))
	for _, s := range d.Spec.Steps {
		m := map[string]any{"id": s.ID, "kind": string(s.Kind), "needs": strs(s.Needs)}
		if s.When != nil {
			m["when"] = map[string]any{"input": s.When.Input, "equals": s.When.Equals.value()}
		}
		switch s.Kind {
		case KindGate:
			g := map[string]any{"evidence": strs(s.Gate.Evidence)}
			if s.Gate.DeploymentID != "" {
				g["deploymentId"] = s.Gate.DeploymentID
			}
			m["gate"] = g
		default:
			ws := map[string]any{"access": string(s.Workspace.Access)}
			if s.Workspace.From != "" {
				ws["from"] = s.Workspace.From
			}
			m["workspace"] = ws
			classes := make([]any, len(s.Retry.On))
			for i, c := range s.Retry.On {
				classes[i] = string(c)
			}
			m["retry"] = map[string]any{"maxAttempts": s.Retry.MaxAttempts, "backoffSeconds": s.Retry.BackoffSeconds, "on": classes}
			m["timeoutSeconds"] = s.TimeoutSeconds
			outs := map[string]any{}
			for k, o := range s.Outputs {
				om := map[string]any{"type": string(o.Type)}
				if o.Path != "" {
					om["path"] = o.Path
				}
				outs[k] = om
			}
			m["outputs"] = outs
			if s.Kind == KindContainer {
				if s.Environment != "" {
					m["environment"] = s.Environment
				}
				m["image"] = s.Image
				m["command"] = strs(s.Command)
				m["args"] = strs(s.Args)
				env := map[string]any{}
				for k, v := range s.Env {
					env[k] = v.value()
				}
				m["env"] = env
				files := map[string]any{}
				for k, v := range s.SecretFiles {
					files[k] = v
				}
				m["secretFiles"] = files
			}
			if s.Agent != nil {
				m["agent"] = map[string]any{"profile": s.Agent.Profile}
			}
		}
		steps = append(steps, m)
	}
	return map[string]any{
		"apiVersion": d.APIVersion,
		"kind":       d.Kind,
		"metadata":   map[string]any{"name": d.Metadata.Name},
		"spec":       map[string]any{"inputs": inputs, "steps": steps},
	}
}

func strs(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// sortedKeys returns map keys in byte order, for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func decodeDocument(raw []byte) (Document, error) {
	var d Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Document{}, fmt.Errorf("decode document: %w", err)
	}
	if (d.APIVersion != pipelinev1.APIVersion && d.APIVersion != pipelinev1alpha2.APIVersion && d.APIVersion != pipelinev1alpha3.APIVersion) || d.Kind != pipelinev1.Kind {
		return Document{}, errors.New("decode document: unexpected apiVersion or kind")
	}
	return d, nil
}
