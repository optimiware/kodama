# External MCP server implementation

`kodama mcp serve` is a separate process command in the existing binary. It is a
client of the public API and the selected OIDC issuer; it is not part of the
Kodama server assembly and does not read `KODAMA_ENV`.

## Layout

| Path | Purpose |
| --- | --- |
| `api/embed.go` | Embeds the committed `api/openapi.yaml` for operation discovery |
| `internal/mcpserver/operations.go` | Parses paths, parameters, request bodies and annotations; builds one tool per operation |
| `internal/mcpserver/execute.go` | Fixed-base API executor, argument/body mapping, delegated credential, error envelope and size bounds |
| `internal/mcpserver/exchange.go` | RFC 8693 exchange, API-token verification and separate-credential enforcement |
| `internal/mcpserver/auth.go` | OAuth protected-resource metadata, 401 challenge, token verification and caller context |
| `internal/mcpserver/server.go` | MCP SDK server, tool registration, stateless Streamable HTTP and probes |
| `internal/mcpserver/config.go` | Flags, environment fallbacks and validation |
| `internal/mcpserver/command.go` | `kodama mcp serve` command and usage |

## Operation discovery

The server embeds the aggregate OpenAPI document. `BuildOperations`:

1. reads every path and HTTP method;
2. resolves local `$ref` parameters, request bodies and schemas;
3. converts parameter names to snake_case tool arguments;
4. builds a JSON Schema object with path/query/header arguments and a `body`
   object;
5. marks read-only, destructive, idempotent, sensitive and streaming
   operations;
6. names the tool `kodama_<snake_case(operationId)>`.

There is no runtime fetching of OpenAPI and no generated hand-maintained tool
list. A route added to `api/openapi.yaml` appears on the next build; the coverage
test fails if the count or one-to-one mapping changes.

## Authentication

`NewAuthenticator` constructs the same `internal/identity/oidc` verifier used by
the server, with the MCP issuer and MCP audience. `VerifyAccessToken` returns the
principal, expiry, scopes and optional delegation without exposing the raw token.
The HTTP middleware validates the bearer and stores a request-scoped `Caller`;
the raw token is available only to the exchange provider.

The executor asks `ExchangeProvider` for a separate API token. The provider sends
the MCP token only to the configured RFC 8693 endpoint, validates the returned
token against the API issuer and audience, and checks that the represented
subject did not change. The executor refuses an empty token, the incoming token,
or any exchange failure; there is no passthrough fallback. No token is stored or
refreshed by the MCP server.

## Tool execution

Every tool handler receives the JSON object arguments and calls the fixed
executor. The executor:

- obtains the delegated API token before constructing the request;
- substitutes URL-escaped path parameters;
- encodes query parameters;
- sets `If-Match`, `Idempotency-Key` and `Last-Event-ID`;
- marshals the `body` object as `application/json`;
- refuses redirects and arbitrary URLs;
- bounds the response by `--max-response-bytes`;
- parses `{error:{code,message,requestId,details}}` into a tool error with
  `_meta`;
- returns successful JSON as text content and, when it is a JSON value, as
  structured content.

Observation tools request `application/json` snapshots. Log tools request
`application/x-ndjson`; `follow=true` is rejected because an MCP tool call must
return a bounded result.

## Helm chart

The Kodama chart installs `mcp-deployment.yaml`, `mcp-service.yaml`,
`mcp-serviceaccount.yaml` and, when enabled, `mcp-ingress.yaml`. The Deployment
uses the same release image, `args: ["mcp", "serve"]`, a dedicated ServiceAccount
with token automount disabled, a read-only root filesystem and no runtime Secret
mount. It mounts only the exchange Secret and optional CA Secrets. The external
profile exposes `mcp.ingress.host` on its own origin. A shared host is accepted
only with `mcp.ingress.path: /mcp`, which routes `/mcp` and the path-specific
metadata to MCP while `/` remains with Kodama. See the
[chart README](../../deploy/helm/kodama/README.md#mcp-server).

## Command

```sh
kodama mcp serve \
  --api-url https://kodama.example/api/v1 \
  --resource https://mcp.kodama.example/mcp \
  --audience https://mcp.kodama.example/mcp \
  --issuer https://id.example \
  --api-audience https://kodama.example/api/v1 \
  --api-issuer https://id.example \
  --exchange-url https://id.example/token \
  --exchange-client-id kodama-mcp \
  --exchange-client-secret-file /run/secrets/mcp-client
```

Environment fallbacks are `KODAMA_MCP_API_URL`, `KODAMA_MCP_RESOURCE`,
`KODAMA_MCP_AUDIENCE`, `KODAMA_MCP_ISSUER`, `KODAMA_MCP_API_AUDIENCE`,
`KODAMA_MCP_API_ISSUER`, `KODAMA_MCP_API_RESOURCE`, `KODAMA_MCP_EXCHANGE_URL`,
`KODAMA_MCP_EXCHANGE_CLIENT_ID`, `KODAMA_MCP_EXCHANGE_CLIENT_AUTH`,
`KODAMA_MCP_EXCHANGE_CLIENT_SECRET_FILE`,
`KODAMA_MCP_EXCHANGE_SCOPES`, `KODAMA_MCP_EXCHANGE_CA_FILE`,
`KODAMA_MCP_SCOPES`, `KODAMA_MCP_LISTEN`, `KODAMA_MCP_READ_ONLY`,
`KODAMA_MCP_TOOLSETS`, `KODAMA_MCP_ENABLE_SECRET_WRITES`,
`KODAMA_MCP_API_CA_FILE`, `KODAMA_MCP_OIDC_CA_FILE`,
`KODAMA_MCP_ALLOW_INSECURE_HTTP`, `KODAMA_MCP_ALLOW_INSECURE_API_HTTP`,
`KODAMA_MCP_DEFAULT_TENANT`,
`KODAMA_MCP_DEFAULT_PROJECT`, `KODAMA_MCP_HTTP_TIMEOUT` and
`KODAMA_MCP_MAX_RESPONSE_BYTES`.

`--resource` must be the exact public MCP URL because the MCP SDK compares the
protected-resource metadata `resource` with the endpoint. `--audience` is the
inbound MCP audience and defaults to the resource. `--api-audience` is the
separate delegated API audience. They must not be silently conflated.

## Tests

- `TestBuildOperationsCoversPublicAPI`: document-derived operation count, one tool each.
- `TestOperationSchemaShape`: request-body and required-argument extraction.
- `TestExecutor*`: path/query/header/body mapping, defaults, API error envelopes
  and response bounds.
- `TestParseServeConfig*`: required configuration and safe URL rules.
- `TestServerOAuthMetadataAndChallenge`: RFC 9728 metadata and 401 challenge.
- `TestServerMCPClientListsAndCallsTools`: real SDK client, tool listing, tool
  call and separate delegated token at the API.
- `TestServerExchangeFailureDoesNotCallAPI`: exchange failure fails the tool
  without an API request.
- `TestExchangeProvider*`: RFC 8693 request shape, client authentication,
  separate API token and subject-continuity checks.
- `TestServerRejectsWrongAudienceAndIDToken`: wrong audience, expired token and
  ID-token claims.
