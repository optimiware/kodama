// Package client is the generated Go HTTP client of Kodama's public API (core
// assembly subset). It is generated from api/openapi.yaml, which is itself
// generated from the module fragments in api/; it has a configurable server
// URL (the URL must include the /api/v1 prefix), HTTP client and request
// editors for authentication, and no import of Kodama's internal implementation.
//
// Regenerate and check drift with `make api-generate` / `make api-check`.
package client

// The bundle step is the repository's own deterministic assembler; the client
// generator is pinned by version and invoked without being added to go.mod, as
// for the module fragments (internal/httpapi/generate.go).
//
//go:generate go run ../../internal/app/apispec/cmd/bundle -api .. -out ../openapi.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml ../openapi.yaml
