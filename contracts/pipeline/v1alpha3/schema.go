// Package pipelinev1alpha3 holds the structural contract of the
// container-plugin pipeline document (apiVersion kodama/v1alpha3). It extends
// run_shared execution with declared state bindings and verified artifact file
// inputs, while carrying no application Environment or Deployment reference.
package pipelinev1alpha3

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	APIVersion = "kodama/v1alpha3"
	Kind       = "Pipeline"
	SchemaID   = "https://kodama.invalid/contracts/pipeline/v1alpha3/schema.json"
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

// Schema returns the compiled schema.
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
