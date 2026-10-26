// Package pipelinev1alpha2 holds the structural contract of the run_shared
// pipeline document (apiVersion kodama/v1alpha2): the JSON Schema, its fixtures
// and a compiled-schema accessor.
//
// It is a separate document version, never an extension flag of
// kodama/v1alpha1 (see ../v1). Schema representability is not installation
// support: whether an installation accepts the format is an explicit, default
// off capability of the shared validator in internal/pipeline, and the schema
// knows nothing about edge-to-service references, source defaults, the
// execution policy or capabilities.
package pipelinev1alpha2

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Document identity.
const (
	APIVersion = "kodama/v1alpha2"
	Kind       = "Pipeline"
	// SchemaID is the schema's stable identifier.
	SchemaID = "https://kodama.invalid/contracts/pipeline/v1alpha2/schema.json"
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
			compileErr = fmt.Errorf("pipelinev1alpha2: read schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(SchemaID, doc); err != nil {
			compileErr = fmt.Errorf("pipelinev1alpha2: add schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(SchemaID)
		if compileErr != nil {
			compileErr = fmt.Errorf("pipelinev1alpha2: compile schema: %w", compileErr)
		}
	})
	return compiled, compileErr
}
