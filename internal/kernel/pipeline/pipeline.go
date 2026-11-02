// Package pipeline owns the simplified .kodama/<processname>.yaml authoring
// format of ADR 0013. It parses one document strictly, validates its semantic
// rules and derives a deterministic execution order. It does not schedule runs
// or depend on a runtime, store or transport.
package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/domain"
)

// Diagnostic codes. They are stable identifiers in lower_snake_case; messages
// are fixed text and never echo commands, environment values or secret values.
const (
	CodeInvalidYAML       = "invalid_yaml"
	CodeUnknownField      = "unknown_field"
	CodeInvalidName       = "invalid_name"
	CodeInvalidValue      = "invalid_value"
	CodeDuplicateStep     = "duplicate_step"
	CodeUnknownDependency = "unknown_dependency"
	CodeDependencyCycle   = "dependency_cycle"
	CodeInvalidScope      = "invalid_scope"
	CodeInvalidSecret     = "invalid_secret"
	CodeInvalidEnv        = "invalid_env"
)

// Diagnostic is one bounded, client-safe finding at a document path.
type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e *Diagnostic) Error() string {
	return fmt.Sprintf("%s at %s: %s", e.Code, e.Path, e.Message)
}

// Unwrap lets callers classify every rejected document with
// errors.Is(err, domain.ErrInvalidValue).
func (e *Diagnostic) Unwrap() error { return domain.ErrInvalidValue }

// Document is one pipeline definition. The parsed document is persisted whole;
// runs snapshot it.
type Document struct {
	Name     string             `json:"name" yaml:"name"`
	Triggers Triggers           `json:"triggers,omitempty" yaml:"triggers,omitempty"`
	Services map[string]Service `json:"services,omitempty" yaml:"services,omitempty"`
	Steps    []Step             `json:"steps" yaml:"steps"`
}

// Triggers names the Git events that start the pipeline.
type Triggers struct {
	Git GitTrigger `json:"git,omitempty" yaml:"git,omitempty"`
}

// GitTrigger filters the Git events that start the pipeline.
type GitTrigger struct {
	Branches []string `json:"branches,omitempty" yaml:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Events   []string `json:"events,omitempty" yaml:"events,omitempty"` // push, tag, merge
}

// Service is a support container shared by the pipeline's steps.
type Service struct {
	Scope    string            `json:"scope,omitempty" yaml:"scope,omitempty"` // pipeline (default), project, tenant
	Image    string            `json:"image" yaml:"image"`
	Commands []string          `json:"commands,omitempty" yaml:"commands,omitempty"`
	Env      map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets  []string          `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Ports    []int             `json:"ports,omitempty" yaml:"ports,omitempty"`
}

// Step is one container execution step sharing /workspace with its siblings.
type Step struct {
	Name      string            `json:"name" yaml:"name"`
	Image     string            `json:"image" yaml:"image"`
	DependsOn []string          `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	Commands  []string          `json:"commands" yaml:"commands"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Secrets   []string          `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Timeout   string            `json:"timeout,omitempty" yaml:"timeout,omitempty"` // Go duration, e.g. "15m"
}

var unknownFieldRE = regexp.MustCompile(`field ([^ ]+) not found in type`)

// Parse decodes YAML strictly: unknown fields are rejected. It checks the
// document's shape only; callers use Validate for the semantic rules.
func Parse(data []byte) (Document, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return Document{}, &Diagnostic{Code: CodeInvalidYAML, Path: "$", Message: "the document is empty"}
		}
		return Document{}, parseError(err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return Document{}, parseError(err)
		}
		return Document{}, &Diagnostic{Code: CodeInvalidYAML, Path: "$", Message: "exactly one document is allowed"}
	}
	return doc, nil
}

func parseError(err error) error {
	if m := unknownFieldRE.FindStringSubmatch(err.Error()); m != nil {
		return &Diagnostic{Code: CodeUnknownField, Path: m[1], Message: "the field is not part of the pipeline format"}
	}
	return &Diagnostic{Code: CodeInvalidYAML, Path: "$", Message: "the document is not valid YAML"}
}

// Canonical returns encoding/json of the document with defaults filled, so
// equal documents share bytes. It never mutates the receiver.
func (d Document) Canonical() ([]byte, error) {
	c := d.clone()
	for name, service := range c.Services {
		if service.Scope == "" {
			service.Scope = "pipeline"
			c.Services[name] = service
		}
	}
	return json.Marshal(c)
}

// Order returns the step names in deterministic execution order: repeatedly
// take the not-yet-emitted step whose depends_on are all emitted and whose
// declaration index is smallest. It returns a Diagnostic on a cycle.
func (d Document) Order() ([]string, error) {
	index := make(map[string]int, len(d.Steps))
	for i, step := range d.Steps {
		if _, ok := index[step.Name]; ok {
			return nil, &Diagnostic{Code: CodeDuplicateStep, Path: pathf("steps[%d].name", i), Message: "the step name is already declared"}
		}
		index[step.Name] = i
	}
	for i, step := range d.Steps {
		seen := make(map[string]bool, len(step.DependsOn))
		for j, dep := range step.DependsOn {
			path := pathf("steps[%d].depends_on[%d]", i, j)
			if dep == step.Name {
				return nil, &Diagnostic{Code: CodeInvalidValue, Path: path, Message: "a step cannot depend on itself"}
			}
			if seen[dep] {
				return nil, &Diagnostic{Code: CodeInvalidValue, Path: path, Message: "the dependency is declared more than once"}
			}
			seen[dep] = true
			if _, ok := index[dep]; !ok {
				return nil, &Diagnostic{Code: CodeUnknownDependency, Path: path, Message: "the dependency is not a declared step"}
			}
		}
	}

	order := make([]string, 0, len(d.Steps))
	emitted := make(map[string]bool, len(d.Steps))
	for len(order) < len(d.Steps) {
		next := -1
		for i, step := range d.Steps {
			if emitted[step.Name] {
				continue
			}
			ready := true
			for _, dep := range step.DependsOn {
				if !emitted[dep] {
					ready = false
					break
				}
			}
			if ready {
				next = i
				break
			}
		}
		if next < 0 {
			return nil, &Diagnostic{Code: CodeDependencyCycle, Path: "steps", Message: "the step dependencies contain a cycle"}
		}
		order = append(order, d.Steps[next].Name)
		emitted[d.Steps[next].Name] = true
	}
	return order, nil
}

// clone copies every map and slice so filling defaults cannot be observed by
// the receiver.
func (d Document) clone() Document {
	c := d
	c.Triggers.Git.Branches = cloneStrings(d.Triggers.Git.Branches)
	c.Triggers.Git.Tags = cloneStrings(d.Triggers.Git.Tags)
	c.Triggers.Git.Events = cloneStrings(d.Triggers.Git.Events)
	if d.Services != nil {
		c.Services = make(map[string]Service, len(d.Services))
		for name, service := range d.Services {
			service.Commands = cloneStrings(service.Commands)
			service.Env = cloneMap(service.Env)
			service.Secrets = cloneStrings(service.Secrets)
			service.Ports = cloneInts(service.Ports)
			c.Services[name] = service
		}
	}
	if d.Steps != nil {
		c.Steps = make([]Step, len(d.Steps))
		for i, step := range d.Steps {
			step.DependsOn = cloneStrings(step.DependsOn)
			step.Commands = cloneStrings(step.Commands)
			step.Env = cloneMap(step.Env)
			step.Secrets = cloneStrings(step.Secrets)
			c.Steps[i] = step
		}
	}
	return c
}

func cloneStrings(in []string) []string { return slices.Clone(in) }

func cloneInts(in []int) []int { return slices.Clone(in) }

func cloneMap(in map[string]string) map[string]string { return maps.Clone(in) }
