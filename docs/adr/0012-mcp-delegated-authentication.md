---
status: proposed
---

# ADR 0012: Delegated authentication for the external MCP server

**Date:** 2026-10-30  
**Direction:** owner-requested MCP server hardening. The MCP server remains a separate process and keeps automatic OpenAPI-derived tool coverage.  
**Disposition:** proposed until the target authorization server's RFC 8693 and delegation contract is qualified. The executable fallback is fail-closed, not token passthrough.  
**Specification:** [External MCP server](../specs/mcp-server.md) (MS). **Implementation:** [MCP server](../implementation/mcp-server.md).

## Context

The first external MCP server implementation validated a token whose audience was the Kodama API and forwarded that same token to the API. That made the MCP process a transparent credential relay rather than a separate OAuth resource server. It also allowed a token minted for the API to be used at MCP and left no verified intermediary for authorization or audit.

Kodama's API currently maps a verified `(issuer, subject)` to a stable user ID and then authorizes through current membership. It did not retain delegation information, and its human-only permission check treated any mapped user actor as a human.

## Decision

### 1. Separate the two resources

The MCP endpoint is an OAuth protected resource with its own resource identifier and audience. The Kodama API is a different resource. A token accepted at MCP is not accepted at the API merely because both use the same issuer.

### 2. Exchange, do not forward

The MCP process is a confidential OAuth client of an RFC 8693 token-exchange endpoint. It sends the verified MCP token only to that endpoint, requests the configured API resource and audience, and verifies the returned token's issuer, API audience, expiry and represented subject. Only the exchanged token reaches the API. There is no passthrough fallback.

The exchange is fixed-destination, bounded and fail-closed. A failed exchange fails the tool call without an API request.

### 3. Preserve representation and intermediary

The API carries the represented user and the verified intermediary in the authenticated principal. Current membership and role checks still apply to the represented user. A delegated invocation is not a direct human action: human-only gate and completion permissions are denied at the route authorization boundary.

Persistent audit enrichment is a separate API/schema change; this ADR does not claim it.

### 4. Keep the MCP process stateless

The MCP process stores no token and has no database, broker, object-store, KMS or Kubernetes API credential. Its only credential is a dedicated exchange client secret, mounted from an existing Secret. The server runtime Secret is never mounted.

### 5. Keep automatic OpenAPI coverage

Every public OpenAPI operation continues to become one MCP tool. Authentication and exchange happen at invocation time; they do not introduce a hand-maintained tool catalogue.

### 6. Chart routing

A dedicated MCP hostname is the recommended deployment. A shared hostname is permitted only with explicit `/mcp` and `/.well-known/oauth-protected-resource/mcp` routes; a catch-all MCP Ingress on the server host is refused.

## Consequences

- **Authorization server dependency.** The issuer must support the exchange and a suitable client-authentication method. The current `oauthsonas` development fixture does not, so the development chart profile disables MCP.
- **Credential custody.** The MCP process now holds an exchange secret. It remains outside the database/KMS/runtime-secret trust boundary, but the secret must be rotated and scoped to exchange only.
- **Human decisions.** MCP tools remain automatically generated, but delegated calls cannot decide gates or complete work. A future explicit human-confirmation mechanism may change that policy; it needs its own contract.
- **Audit.** The represented user and intermediary are available to the API authentication layer; persisting both in audit rows requires an owning schema change.
- **Compatibility.** Direct API and CLI clients keep their existing audience and token path. MCP clients must obtain MCP-bound tokens instead of API-bound tokens.

## Alternatives considered

- **Keep token passthrough as a trusted gateway.** Rejected as the production default; it collapses the two resource boundaries and violates the MCP authorization guidance.
- **Use one service credential for all MCP calls.** Rejected for human workflows: it loses per-user authorization and audit attribution. It remains suitable only for explicitly scoped machine flows.
- **Run an MCP-local authorization server or token vault.** Rejected for the first implementation because it makes the MCP process stateful and duplicates Platform's authority. A separately qualified broker remains possible.
- **Reimplement business operations inside MCP.** Rejected: it creates a second source of business state and defeats the API contract.
