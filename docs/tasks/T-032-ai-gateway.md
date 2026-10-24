---
id: T-032
milestone: M2
status: done
depends_on: [T-010, T-012, T-020]
decision_blockers: []
owns: [internal/gateway/, api/gateway.yaml]
check: make check-go
resources: [go]
---
# T-032 — First model gateway protocol

## Goal
Mediate a verified OpenAI-compatible endpoint with simple bounded usage.

## Contract
Implement [gateway rules](../specs/ai-gateway-and-mcp-proxy.md). Freeze exact streaming request/response coverage before the harness smoke test; initially target chat completions, not universal provider compatibility.

O-19 keeps model provider credentials behind this gateway rather than in agent prompts or ambient environments. O-13 assigns subscription billing, commercial metering/access and entitlements to Platform. Preserve this module's operational usage/outcome evidence and atomic execution budgets; do not add a parallel pricing/payment/subscription authority or weaken bounds because billing is external.

## Deliverables
Project model settings, secret routing, stream/cancel handling, model allow-list, atomic request cap/one-in-flight per run, token estimates/uncertainty records, API fragment.

## Acceptance
`cross_replica_admission`, `bounded_token_overshoot`, `partial_stream`, `redirect_credential`, and `content_retention_off` verify GM-01–GM-06/GM-09/GM-11/GM-12.

## Out of scope
Billing settlement ledger, silent provider fallback, or unbounded parallel overshoot.
