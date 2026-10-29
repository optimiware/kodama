# internal/mcpserver

The external MCP server (`kodama mcp serve`). It is the opposite direction from
`internal/mcpproxy`: external MCP clients call the Kodama public API through
this server, while the proxy lets Kodama agents call approved external MCP
servers.

The server is a stateless OAuth resource server. It verifies the caller's bearer
with the selected OIDC issuer, then forwards the same bearer to the Kodama API.
It has no database, broker, object store, KMS or Kubernetes client and holds no
service credential.

`operations.go` parses the embedded public OpenAPI document and builds one tool
per operation. `execute.go` performs the fixed-base HTTP call, preserving path
and query parameters, `If-Match`, `Idempotency-Key`, `Last-Event-ID`, request
bodies and API error envelopes. `auth.go` serves RFC 9728 metadata and the 401
challenge. `server.go` registers the tools on the MCP SDK and serves Streamable
HTTP.

Specification: [external MCP server](../../docs/specs/mcp-server.md).
Implementation notes: [mcp-server](../../docs/implementation/mcp-server.md).
