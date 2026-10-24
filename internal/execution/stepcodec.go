package execution

import (
	"encoding/json"

	"github.com/optimiware/kodama/internal/pipeline"
)

// The pipeline module's EnvValue decodes a scalar or {"secretRef": ...} but has
// no encoder, so a stored step needs this explicit wire form: the frozen step of
// a run must round-trip exactly, secret references included.

type envWire pipeline.EnvValue

func (e envWire) MarshalJSON() ([]byte, error) {
	if e.Scalar != nil {
		return e.Scalar.MarshalJSON()
	}
	return json.Marshal(map[string]string{"secretRef": e.SecretRef})
}

func (e *envWire) UnmarshalJSON(b []byte) error {
	var v pipeline.EnvValue
	if err := v.UnmarshalJSON(b); err != nil {
		return err
	}
	*e = envWire(v)
	return nil
}

type stepWire struct {
	ID             string                     `json:"id"`
	Kind           pipeline.StepKind          `json:"kind"`
	Needs          []string                   `json:"needs,omitempty"`
	When           *pipeline.Condition        `json:"when,omitempty"`
	Workspace      *pipeline.Workspace        `json:"workspace,omitempty"`
	Environment    string                     `json:"environment,omitempty"`
	Image          string                     `json:"image,omitempty"`
	Command        []string                   `json:"command,omitempty"`
	Args           []string                   `json:"args,omitempty"`
	Env            map[string]envWire         `json:"env,omitempty"`
	SecretFiles    map[string]string          `json:"secretFiles,omitempty"`
	TimeoutSeconds int64                      `json:"timeoutSeconds,omitempty"`
	Retry          *pipeline.Retry            `json:"retry,omitempty"`
	Outputs        map[string]pipeline.Output `json:"outputs,omitempty"`
	Agent          *pipeline.AgentSpec        `json:"agent,omitempty"`
	Gate           *pipeline.GateSpec         `json:"gate,omitempty"`
}

// EncodeStep serializes a frozen step.
func EncodeStep(s pipeline.Step) ([]byte, error) {
	w := stepWire{ID: s.ID, Kind: s.Kind, Needs: s.Needs, When: s.When, Workspace: s.Workspace, Environment: s.Environment, Image: s.Image,
		Command: s.Command, Args: s.Args, SecretFiles: s.SecretFiles, TimeoutSeconds: s.TimeoutSeconds, Retry: s.Retry, Outputs: s.Outputs,
		Agent: s.Agent, Gate: s.Gate}
	if len(s.Env) > 0 {
		w.Env = make(map[string]envWire, len(s.Env))
		for k, v := range s.Env {
			w.Env[k] = envWire(v)
		}
	}
	return json.Marshal(w)
}

// DecodeStep restores a frozen step.
func DecodeStep(b []byte) (pipeline.Step, error) {
	var w stepWire
	if err := json.Unmarshal(b, &w); err != nil {
		return pipeline.Step{}, err
	}
	s := pipeline.Step{ID: w.ID, Kind: w.Kind, Needs: w.Needs, When: w.When, Workspace: w.Workspace, Environment: w.Environment, Image: w.Image,
		Command: w.Command, Args: w.Args, SecretFiles: w.SecretFiles, TimeoutSeconds: w.TimeoutSeconds, Retry: w.Retry, Outputs: w.Outputs,
		Agent: w.Agent, Gate: w.Gate}
	if len(w.Env) > 0 {
		s.Env = make(map[string]pipeline.EnvValue, len(w.Env))
		for k, v := range w.Env {
			s.Env[k] = pipeline.EnvValue(v)
		}
	}
	return s, nil
}
