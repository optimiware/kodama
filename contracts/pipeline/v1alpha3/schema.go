// Package pipelinev1alpha3 holds the structural contract of the run_shared
// pipeline document with a declared source set (apiVersion kodama/v1alpha3):
// the JSON Schema, its fixtures and a compiled-schema accessor. It is the
// kodama/v1alpha2 document plus spec.sources and nothing else.
//
// It is a separate, additive document version (ADR 0011), never an extension
// flag of kodama/v1alpha1 (../v1) or kodama/v1alpha2 (../v1alpha2), whose
// schemas, fixtures, canonical bytes and digests are unchanged. Schema
// representability is not installation support: whether an installation accepts
// the format is an explicit, default-off capability of the shared validator in
// internal/pipeline, and the schema knows nothing about whether a named
// repository exists in the project, path and writer consistency across the
// document, the execution policy or capabilities.
package pipelinev1alpha3

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Document identity.
const (
	APIVersion = "kodama/v1alpha3"
	Kind       = "Pipeline"
	// SchemaID is the schema's stable identifier.
	SchemaID = "https://kodama.invalid/contracts/pipeline/v1alpha3/schema.json"
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
			compileErr = fmt.Errorf("pipelinev1alpha3: read schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(SchemaID, doc); err != nil {
			compileErr = fmt.Errorf("pipelinev1alpha3: add schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(SchemaID)
		if compileErr != nil {
			compileErr = fmt.Errorf("pipelinev1alpha3: compile schema: %w", compileErr)
		}
	})
	return compiled, compileErr
}
