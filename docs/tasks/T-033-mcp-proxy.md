---
id: T-033
milestone: M2
status: done
depends_on: [T-010, T-012, T-019, T-020]
decision_blockers: []
owns: [internal/mcpproxy/, api/mcp.yaml, test/support/mcp/]
check: make check-go
resources: [go]
review: human
---
# T-033 — Streamable HTTP MCP proxy

## Goal
Expose only approved tools under the active attempt's identity.

## Contract
Pin a supported MCP version/transport with `github.com/modelcontextprotocol/go-sdk` and enforce project registration plus profile tool allow-list. Scope remote session IDs to caller; use integration-owned remote credentials.

This generic credential profile does not establish delegated access to Optimi Platform's user-authorized MCP resource. Preserve the [platform tool boundary](../specs/ai-gateway-and-mcp-proxy.md#optimi-platform-tools) and expose a narrow invocation seam; O-18/T-061 owns the recipient-bound hosted integration.

## Deliverables
Registration handlers/storage, discovery/invocation filtering, caller/session mapping/expiry, endpoint checks, cancellation, audit, local protocol server fixture.

## Acceptance
`hidden_tool_direct_call`, `foreign_session`, `remote_token_audience`, `unknown_side_effect`, and `protocol_smoke` cover GM-01/GM-02/GM-07–GM-12.

## Out of scope
Argument-policy DSL, delegated user OAuth, or automatic retries of arbitrary tool effects.
