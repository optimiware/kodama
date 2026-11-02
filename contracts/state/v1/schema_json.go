package statev1

import (
	"bytes"
	_ "embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const SchemaID = "https://kodama.invalid/contracts/state/v1/schema.json"

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
			compileErr = fmt.Errorf("statev1: read schema: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(SchemaID, doc); err != nil {
			compileErr = fmt.Errorf("statev1: add schema: %w", err)
			return
		}
		compiled, compileErr = c.Compile(SchemaID)
		if compileErr != nil {
			compileErr = fmt.Errorf("statev1: compile schema: %w", compileErr)
		}
	})
	return compiled, compileErr
}
