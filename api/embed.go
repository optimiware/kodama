// Package api exposes the committed public OpenAPI document to programs that
// need the exact published API surface without reading the source tree.
package api

import _ "embed"

// OpenAPI is the aggregate public API document generated from api/*.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
