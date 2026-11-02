# internal/mcpserver

The external MCP server (`kodama mcp serve`). It is the opposite direction from
`internal/mcpproxy`: external MCP clients call the Kodama public API through
this server, while the proxy lets Kodama agents call approved external MCP
servers.

The server is a stateless OAuth resource server and exchange client. It verifies
an MCP-bound bearer with the selected OIDC issuer, exchanges it for a separate
API-bound delegated token, and sends only that token to Kodama. It has no
database, broker, object store, KMS or Kubernetes client; its only credential is
the dedicated token-exchange client secret.

`operations.go` parses the embedded public OpenAPI document and builds one tool
per operation. `execute.go` performs the fixed-base HTTP call, preserving path
and query parameters, `If-Match`, `Idempotency-Key`, `Last-Event-ID`, request
bodies and API error envelopes. `exchange.go` implements RFC 8693 and verifies
the returned API token. `auth.go` serves RFC 9728 metadata, the 401 challenge and
the verified caller context. `server.go` registers the tools on the MCP SDK and
serves Streamable HTTP.

Specification: [external MCP server](../../docs/specs/mcp-server.md).
Implementation notes: [mcp-server](../../docs/implementation/mcp-server.md).
