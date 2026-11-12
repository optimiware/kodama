package mcp

import (
	"encoding/json"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// Tool is the MCP advertisement of one operation. The public API schema
// declares the one tool shape; this package only derives the catalog from the
// declared route table.
type Tool = api.MCPTool

// toolCatalog is the deterministic declaration built once from the declared
// route inventory: exactly the 51 registry operations in registry order, each
// with the field-level api.OperationInputSchema of its operation. A
// construction failure is retained so the edge refuses instead of advertising a
// placeholder.
var toolCatalog, toolCatalogErr = buildToolCatalog()

// buildToolCatalog derives the catalog from api.Routes so the MCP tool inventory
// can never drift from the REST/registry inventory.
func buildToolCatalog() ([]Tool, error) {
	return catalogForRoutes(api.Routes(), api.OperationInputSchema)
}

// catalogForRoutes builds one tool per route, in route order, from the
// field-level schema producer. A producer failure is a construction error: the
// catalog is never partially advertised with an empty or title-only schema.
// Every schema is copied so the catalog owns its bytes.
func catalogForRoutes(routes []api.Route, schemaFor func(string) (json.RawMessage, error)) ([]Tool, error) {
	catalog := make([]Tool, len(routes))
	for index, route := range routes {
		schema, err := schemaFor(route.Operation)
		if err != nil {
			return nil, fault.Wrap(fault.InternalFailure,
				"MCP tool "+route.Operation+" has no field-level input schema", err)
		}
		catalog[index] = Tool{
			Name:        route.Operation,
			InputSchema: append(json.RawMessage(nil), schema...),
		}
	}
	return catalog, nil
}

// ToolCatalog returns the complete MCP tool catalog in declaration order. Each
// call returns a copy of every schema's bytes, so a consumer cannot mutate the
// shared declaration. A construction failure yields no catalog; NewEdge and
// tools/list refuse rather than advertise an incomplete inventory.
func ToolCatalog() []Tool {
	if toolCatalogErr != nil {
		return nil
	}
	catalog := make([]Tool, len(toolCatalog))
	for index, tool := range toolCatalog {
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		catalog[index] = tool
	}
	return catalog
}
