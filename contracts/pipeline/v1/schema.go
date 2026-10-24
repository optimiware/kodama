// Package pipelinev1 holds the frozen structural contract of a pipeline
// definition document (apiVersion kodama/v1alpha1): the JSON Schema, its
// fixtures and a compiled-schema accessor.
//
// The schema is the single structural authority for the CLI and the API. It
// deliberately knows nothing about which step kinds an installation enables,
// graph shape, reference resolution or source ordering; internal/pipeline
// enforces those on top of it. The planned M2 agent and gate shapes are
// representable here without being advertised as executable.
package pipelinev1

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Document identity.
const (
	APIVersion = "kodama/v1alpha1"
	Kind       = "Pipeline"
	// SchemaID is the schema's stable identifier.
	SchemaID = "https://kodama.invalid/contracts/pipeline/v1/schema.json"
)

//go:embed schema.json
var schemaJSON []byte

// SchemaJSON returns a copy of the embedded JSON Schema document.
func SchemaJSON() []byte { return append([]byte(nil), schemaJSON...) }

var (
	compileOnce sync.Once
	compiled    *jsonschema.Schema
	compileErr  error
)

// Schema returns the compiled schema. Compilation happens once; the returned
// schema is safe for concurrent validation.
func Schema() (*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
		if err != nil {
			compileErr = fmt.Errorf("pipelinev1: read schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(SchemaID, doc); err != nil {
			compileErr = fmt.Errorf("pipelinev1: add schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(SchemaID)
		if compileErr != nil {
			compileErr = fmt.Errorf("pipelinev1: compile schema: %w", compileErr)
		}
	})
	return compiled, compileErr
}
