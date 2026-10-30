# External MCP server

**Scope:** `kodama mcp serve`, the OAuth-protected MCP endpoint over the public
Kodama HTTP API. This is the opposite direction from the [MCP proxy](ai-gateway-and-mcp-proxy.md):
the proxy lets Kodama agents call approved external MCP servers; this server
lets external MCP clients call Kodama.

## Purpose

Give MCP clients the same capabilities as the public API without adding a second
authorization system, a service credential or a second source of business
state. The server is a thin, stateless gateway:

```text
MCP client --OAuth bearer--> kodama mcp --same bearer--> Kodama /api/v1
```

Kodama remains the authority for membership, roles, gates, tenants and every
business transition. The MCP server only validates the token, selects a fixed
public operation, forwards the request and maps the response.

## Model

- The public OpenAPI document (`api/openapi.yaml`) is embedded in the binary and
  parsed at startup. Each operation becomes one tool named
  `kodama_<operationId>`.
- Path and query parameters become snake_case tool arguments; a JSON request
  body is the `body` object. `If-Match`, `Idempotency-Key` and `Last-Event-ID`
  become `if_match`, `idempotency_key` and `last_event_id`.
- List tools return the API page and `nextCursor` unchanged. The server does not
  invent cursors.
- Observation tools request the JSON snapshot representation and return a
  bounded batch. `follow=true` is refused for logs; MCP tools do not hold a
  stream open.
- `createSecret` and `createSecretVersion` are disabled unless the installation
  explicitly enables secret-value writes. Secret values must not be placed in
  logs, errors or retained diagnostics.
- `--read-only` exposes only GET tools. `--toolsets` narrows the exposed groups
  (`identity`, `work`, `workspace`, `pipeline`, `runs`, `gates`, `delivery`,
  `configuration`, `artifacts`, `model`, `repo`, `chatops`). The default exposes
  every non-sensitive operation.

## OAuth resource server

The server is an OAuth 2.0 protected resource (RFC 9728), not an authorization
server and not an OAuth client.

- It serves protected-resource metadata at the path-specific and root
  `/.well-known/oauth-protected-resource` locations.
- It advertises the configured issuer as the authorization server and
  `openid` as the supported scope. Kodama authorizes by membership and role;
  OAuth scopes do not grant Kodama permissions.
- It validates signature, exact issuer, exact audience, expiry and not-before
  with the issuer's JWKS. ID tokens (`nonce` or `at_hash`) are rejected.
- It returns `401` with a `WWW-Authenticate: Bearer resource_metadata=...`
  challenge when the token is absent or invalid.
- It forwards the caller's raw bearer token to Kodama. It stores no token, has
  no refresh token, mints no token and holds no service credential.

The metadata `resource` is the exact public MCP URL. The token `audience` is the
audience Kodama accepts (`KODAMA_OIDC_AUDIENCE`). They may be the same value, or
the deployment may use the gateway form where the audience is Kodama's API
audience and the resource is the MCP URL. The deployment must document which
form it uses.

Standard MCP OAuth also needs an authorization server that accepts the MCP
client's `resource` parameter and either supports dynamic client registration or
has a pre-registered client for the client's redirect URI. The current
`oauthsonas` developer fixture does not yet do both; that is an upstream
prerequisite, not something this server can fake.

## Requirements

| ID | Requirement |
| --- | --- |
| MS-01 | Every public OpenAPI operation MUST have exactly one MCP tool, and no tool may name an operation outside the published document. |
| MS-02 | The server MUST be an OAuth resource server: exact issuer, audience, signature and expiry validation, a 401 challenge and protected-resource metadata. |
| MS-03 | The server MUST forward the caller's verified bearer to Kodama and MUST NOT hold a service credential, mint a token or store a token. |
| MS-04 | Kodama MUST re-authorize every call; an MCP tool MUST NOT add tenant, project or role authority. |
| MS-05 | Tool arguments, request bodies, tokens and secret values MUST NOT appear in logs or error text. Secret-value tools MUST be opt-in. |
| MS-06 | The API base URL MUST be fixed configuration. The server MUST NOT fetch arbitrary URLs, follow redirects or proxy arbitrary paths. |
| MS-07 | Requests and responses MUST be bounded by time and size. Observation and log tools MUST return a bounded snapshot, never an unbounded stream. |
| MS-08 | Idempotency keys, `If-Match` versions and API error envelopes MUST be preserved so retries and conflicts behave as they do on the HTTP API. |
| MS-09 | The MCP process MUST NOT require PostgreSQL, NATS, S3, KMS or Kubernetes access, and MUST NOT read the Kodama secrets directory. |
| MS-10 | `--read-only` and `--toolsets` MUST hide tools; hiding a tool MUST NOT be used as an authorization boundary. |
| MS-11 | The server MUST advertise only the `tools` MCP capability and a pinned protocol revision. |
| MS-12 | A deployment MUST document the OAuth resource/audience form and the authorization-server client-registration prerequisite. |
| MS-13 | Operations that return Git credentials MUST be hidden unless the installation explicitly enables credential tools; the MCP server MUST NOT make a credential value the default result of a model-facing tool. |
| MS-14 | The Helm chart MUST install the MCP Deployment and Service when enabled, MUST expose it only through its own Ingress host, and MUST NOT mount the server runtime Secret or a Kubernetes API token. |

## Acceptance

| ID | Scenario | Requirements |
| --- | --- | --- |
| MS-A01 | The embedded document's public operations each have exactly one tool, and the registry has no extra tool. | MS-01 |
| MS-A02 | A standard MCP client discovers metadata, receives the 401 challenge, authenticates, lists tools and calls a tool; the API sees the same bearer. | MS-02, MS-03, MS-11 |
| MS-A03 | Wrong issuer, wrong audience, expired token, ID token and forged signature are refused without echoing the token. | MS-02, MS-05 |
| MS-A04 | A token for tenant A cannot read tenant B; Kodama's 403/404 is preserved. | MS-04 |
| MS-A05 | `--read-only` hides every mutating tool; an unknown toolset is refused; a hidden tool is not an authorization grant. | MS-10 |
| MS-A06 | Secret-value tools are absent unless enabled; enabling them still never logs the value and never returns it. | MS-05 |
| MS-A07 | API error envelopes keep `code`, `requestId` and status; oversized responses and `follow=true` are refused. | MS-07, MS-08 |
| MS-A08 | The process starts without a database, broker, object store, KMS or Kubernetes credential. | MS-09 |
| MS-A09 | The deployment guide states the resource/audience and client-registration requirements. | MS-12 |
| MS-A10 | A Git-credential-returning operation is absent by default and appears only with `--enable-credential-tools`; enabling it still does not log the value. | MS-13 |
| MS-A11 | The chart renders an MCP Deployment, Service and optional dedicated-host Ingress; the MCP Pod has no runtime Secret, no database/KMS env and no service-account token; a shared MCP/server host fails rendering. | MS-14 |

## Unresolved decisions

- Whether the target MCP clients require dynamic client registration; if so,
  the authorization server or a separate broker must provide it.
- Whether secret-value tools should ever be enabled for model-driven clients.
- Whether observation should later use MCP resource subscriptions instead of a
  bounded snapshot tool.
- Whether the MCP server ships in the Kodama chart or as a separate deployment.
  The chart now ships it as a separate Deployment; the raw manifest remains a
  fallback.
- **Token passthrough.** The current gateway forwards the caller's Kodama API
  token. The MCP authorization specification forbids passing a client token to a
  downstream API; production should use an MCP-specific audience and a separate
  upstream credential (token exchange or a scoped service credential). This is a
  product change tracked separately from the chart wiring.
