package mcp

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/optimiware/kodama/api"
)

// TestCatalogForRoutesRefusesMissingSchema pins that construction refuses an
// exposed operation whose field-level schema cannot be produced instead of
// advertising a placeholder or skipping the operation.
func TestCatalogForRoutesRefusesMissingSchema(t *testing.T) {
	routes := api.Routes()[:2]
	failure := errors.New("schema table entry missing")

	catalog, err := catalogForRoutes(routes, func(string) (json.RawMessage, error) {
		return nil, failure
	})
	if err == nil {
		t.Fatalf("catalogForRoutes accepted a failing schema producer: %+v", catalog)
	}
	if !errors.Is(err, failure) {
		t.Fatalf("construction error = %v; want it to keep the private cause", err)
	}
}

// TestCatalogForRoutesCopiesSchemaBytes pins that the catalog owns its schema
// bytes rather than aliasing the producer's slice.
func TestCatalogForRoutesCopiesSchemaBytes(t *testing.T) {
	routes := api.Routes()[:1]
	source := json.RawMessage(`{"type":"object","title":"fixture.input.v1"}`)

	catalog, err := catalogForRoutes(routes, func(string) (json.RawMessage, error) {
		return source, nil
	})
	if err != nil {
		t.Fatalf("catalogForRoutes: %v", err)
	}
	if len(catalog) != 1 {
		t.Fatalf("catalog tools = %d; want 1", len(catalog))
	}
	if catalog[0].Name != routes[0].MCPTool {
		t.Fatalf("tool = %q; want %q", catalog[0].Name, routes[0].MCPTool)
	}
	if string(catalog[0].InputSchema) != string(source) {
		t.Fatalf("tool schema = %s; want %s", catalog[0].InputSchema, source)
	}

	source[0] = 'x'
	if string(catalog[0].InputSchema) != `{"type":"object","title":"fixture.input.v1"}` {
		t.Fatalf("catalog aliases the producer's bytes: %s", catalog[0].InputSchema)
	}
}
